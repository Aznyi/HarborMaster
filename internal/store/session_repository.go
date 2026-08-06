package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// SessionRepository owns server-side sessions.
//
// # The token is never here
//
// Every method takes or returns a DIGEST. The raw session token exists in the
// browser's cookie and, for the duration of one request, in memory; it is never
// a parameter to a query, never a column, and never in an error.
//
// # Lookup is one indexed probe
//
// A request presents a token, the service digests it, and ByTokenDigest finds
// the row through the unique index. There is no scan, no per-row comparison,
// and therefore no timing signal that depends on how many sessions exist.
type SessionRepository struct {
	db *sql.DB
}

// ErrSessionNotFound reports a token that matches no live session.
//
// Deliberately indistinguishable from an expired or revoked one at this layer:
// the caller gets "no usable session" and cannot accidentally tell a client
// which of the three it was.
var ErrSessionNotFound = errors.New("no usable session")

const selectSessionColumns = `
	SELECT s.id, s.session_id, s.user_id, s.username, s.role,
	       s.created_at, s.last_seen_at, s.idle_expires_at, s.absolute_expires_at,
	       s.revoked_at, s.revocation, s.user_agent, s.client_addr
	FROM sessions s`

// NewSession is the input to Create.
type NewSession struct {
	SessionID string
	UserID    string
	Username  string
	Role      domain.Role

	// TokenDigest is the keyed digest of the token. The token itself is the
	// caller's business and never reaches this package.
	TokenDigest string

	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time

	UserAgent  string
	ClientAddr string
}

// Create issues a session and enforces the per-user cap.
//
// # Why the cap matters
//
// Without one, an attacker with a valid credential -- or an operator with a
// misbehaving script -- can create unbounded rows. The cap makes the oldest
// session fall off, which is also the behaviour an operator expects from
// "you have been signed out on your other device".
//
// Both the insert and the eviction happen in one transaction, so the cap is
// never exceeded even momentarily.
func (r *SessionRepository) Create(
	ctx context.Context,
	request NewSession,
	maxPerUser int,
	now time.Time,
) (domain.Session, error) {
	if !domain.ValidSessionID(request.SessionID) ||
		request.TokenDigest == "" ||
		!domain.ValidRole(string(request.Role)) {
		return domain.Session{}, fmt.Errorf("create session: %w", ErrInvalidInput)
	}
	if maxPerUser < 1 {
		maxPerUser = 1
	}

	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Session{}, fmt.Errorf("begin session insert: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions
			(session_id, user_id, username, role, token_digest,
			 created_at, last_seen_at, idle_expires_at, absolute_expires_at,
			 user_agent, client_addr)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		request.SessionID, request.UserID, request.Username, string(request.Role),
		request.TokenDigest, stamp, stamp,
		formatTime(request.IdleExpiresAt.UTC()),
		formatTime(request.AbsoluteExpiresAt.UTC()),
		domain.SanitiseDisplayText(request.UserAgent, domain.MaxUserAgentBytes),
		request.ClientAddr)
	if err != nil {
		return domain.Session{}, fmt.Errorf("insert session: %w", AsError(err))
	}

	// Evict the oldest live sessions past the cap. Ordered by id DESC and
	// skipped past the cap, so the NEWEST maxPerUser survive -- including the
	// one just inserted.
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revocation = 'superseded'
		WHERE revoked_at IS NULL
		  AND user_id = ?
		  AND id NOT IN (
		      SELECT id FROM sessions
		      WHERE revoked_at IS NULL AND user_id = ?
		      ORDER BY id DESC
		      LIMIT ?
		  )`, stamp, request.UserID, request.UserID, maxPerUser); err != nil {
		return domain.Session{}, fmt.Errorf("enforce session cap: %w", AsError(err))
	}

	if err := tx.Commit(); err != nil {
		return domain.Session{}, fmt.Errorf("commit session insert: %w", AsError(err))
	}

	return r.byID(ctx, request.SessionID)
}

// ByTokenDigest returns the live session a digest names.
//
// # What "live" means here
//
// Not revoked, not past either expiry, AND issued after the account's password
// last changed. The last condition is the belt to the revocation braces: a
// password change explicitly revokes sessions, and this makes the revocation
// hold even if that write was lost to a crash between the two.
//
// The user's status is deliberately NOT checked here. The authorization
// middleware re-reads the user and refuses a disabled account, and doing it
// there means one place decides rather than two that could disagree.
func (r *SessionRepository) ByTokenDigest(
	ctx context.Context,
	digest string,
	now time.Time,
) (domain.Session, error) {
	if digest == "" {
		return domain.Session{}, ErrSessionNotFound
	}
	stamp := formatTime(now.UTC())

	rows, err := r.db.QueryContext(ctx, selectSessionColumns+`
		JOIN users u ON u.user_id = s.user_id
		WHERE s.token_digest = ?
		  AND s.revoked_at IS NULL
		  AND s.idle_expires_at > ?
		  AND s.absolute_expires_at > ?
		  AND s.created_at >= u.password_changed_at`,
		digest, stamp, stamp)
	if err != nil {
		return domain.Session{}, fmt.Errorf("query session: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanSessions(rows)
	if err != nil {
		return domain.Session{}, err
	}
	if len(found) == 0 {
		return domain.Session{}, ErrSessionNotFound
	}
	return found[0], nil
}

// byID reads one session by its public identifier, live or not.
func (r *SessionRepository) byID(ctx context.Context, sessionID string) (domain.Session, error) {
	rows, err := r.db.QueryContext(ctx,
		selectSessionColumns+` WHERE s.session_id = ?`, sessionID)
	if err != nil {
		return domain.Session{}, fmt.Errorf("query session by id: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanSessions(rows)
	if err != nil {
		return domain.Session{}, err
	}
	if len(found) == 0 {
		return domain.Session{}, ErrNotFound
	}
	return found[0], nil
}

// Touch extends a session's idle expiry.
//
// # Why this is conditional rather than unconditional
//
// Idle expiry has to move forward as a session is used, or an operator working
// steadily would be logged out mid-task. Writing on EVERY request would make
// every read a write, which on SQLite's single writer is the difference between
// a page load and a queue.
//
// So the caller only calls this past a threshold, and the statement is
// conditional too: a concurrent request that already moved the expiry forward
// makes this one affect no rows, which is correct and costs nothing.
func (r *SessionRepository) Touch(
	ctx context.Context,
	sessionID string,
	lastSeen time.Time,
	idleExpiresAt time.Time,
) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ?, idle_expires_at = ?
		WHERE session_id = ?
		  AND revoked_at IS NULL
		  AND idle_expires_at < ?`,
		formatTime(lastSeen.UTC()), formatTime(idleExpiresAt.UTC()),
		sessionID, formatTime(idleExpiresAt.UTC()))
	if err != nil {
		return fmt.Errorf("touch session: %w", AsError(err))
	}
	return nil
}

// Revoke ends one session.
//
// Idempotent: revoking an already-revoked session affects no rows and is not an
// error. The caller's intent was that it not be usable, and it is not.
func (r *SessionRepository) Revoke(
	ctx context.Context,
	sessionID string,
	reason domain.SessionRevocation,
	now time.Time,
) error {
	if !domain.ValidSessionRevocation(string(reason)) || reason == "" {
		return fmt.Errorf("revoke session: %w", ErrInvalidInput)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revocation = ?
		WHERE session_id = ? AND revoked_at IS NULL`,
		formatTime(now.UTC()), string(reason), sessionID)
	if err != nil {
		return fmt.Errorf("revoke session: %w", AsError(err))
	}
	return nil
}

// RevokeAllForUser ends every live session an account holds.
//
// The write behind logout-everywhere, password change, role change, and
// disablement. `except` keeps one session alive, which is what a password
// change performed by the account holder needs: revoking their own session
// mid-request would log them out of the page that just succeeded.
func (r *SessionRepository) RevokeAllForUser(
	ctx context.Context,
	userID string,
	reason domain.SessionRevocation,
	except string,
	now time.Time,
) (int64, error) {
	if !domain.ValidSessionRevocation(string(reason)) || reason == "" {
		return 0, fmt.Errorf("revoke sessions: %w", ErrInvalidInput)
	}

	result, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revocation = ?
		WHERE user_id = ? AND revoked_at IS NULL AND session_id <> ?`,
		formatTime(now.UTC()), string(reason), userID, except)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", AsError(err))
	}

	revoked, _ := result.RowsAffected()
	return revoked, nil
}

// ListForUser returns an account's live sessions, newest first.
func (r *SessionRepository) ListForUser(
	ctx context.Context,
	userID string,
	now time.Time,
	limit int,
) ([]domain.Session, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	stamp := formatTime(now.UTC())

	rows, err := r.db.QueryContext(ctx, selectSessionColumns+`
		WHERE s.user_id = ?
		  AND s.revoked_at IS NULL
		  AND s.idle_expires_at > ?
		  AND s.absolute_expires_at > ?
		ORDER BY s.id DESC
		LIMIT ?`, userID, stamp, stamp, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

// CountActive reports how many live sessions exist across the installation.
func (r *SessionRepository) CountActive(ctx context.Context, now time.Time) (int, error) {
	stamp := formatTime(now.UTC())

	var count int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`,
		stamp, stamp).Scan(&count); err != nil {
		return 0, fmt.Errorf("count sessions: %w", AsError(err))
	}
	return count, nil
}

// ExpireStale marks sessions that have passed either expiry.
//
// Bookkeeping rather than enforcement: ByTokenDigest already refuses an expired
// session, so a row this pass has not reached yet is not usable. Running it
// keeps the session list honest and gives the retention pass something to
// remove.
func (r *SessionRepository) ExpireStale(ctx context.Context, now time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	stamp := formatTime(now.UTC())

	result, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revocation = 'expired'
		WHERE id IN (
			SELECT id FROM sessions
			WHERE revoked_at IS NULL
			  AND (idle_expires_at <= ? OR absolute_expires_at <= ?)
			LIMIT ?
		)`, stamp, stamp, stamp, batch)
	if err != nil {
		return 0, fmt.Errorf("expire sessions: %w", AsError(err))
	}

	expired, _ := result.RowsAffected()
	return expired, nil
}

// Prune removes revoked sessions past the retention cutoff.
//
// Only REVOKED rows, and only old ones. A live session is never removed by
// retention, and a recently revoked one is kept so "why was I signed out" has
// an answer for a while.
func (r *SessionRepository) Prune(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}

	result, err := r.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE id IN (
			SELECT id FROM sessions
			WHERE revoked_at IS NOT NULL AND revoked_at < ?
			LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune sessions: %w", AsError(err))
	}

	pruned, _ := result.RowsAffected()
	return pruned, nil
}

func scanSessions(rows *sql.Rows) ([]domain.Session, error) {
	out := make([]domain.Session, 0, 8)

	for rows.Next() {
		var (
			session    domain.Session
			role       string
			created    string
			lastSeen   string
			idle       string
			absolute   string
			revoked    sql.NullString
			revocation string
		)
		if err := rows.Scan(&session.ID, &session.SessionID, &session.UserID,
			&session.Username, &role, &created, &lastSeen, &idle, &absolute,
			&revoked, &revocation, &session.UserAgent, &session.ClientAddr); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}

		session.Role = domain.Role(role)
		session.Revocation = domain.SessionRevocation(revocation)

		var err error
		if session.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if session.LastSeenAt, err = parseTime(lastSeen); err != nil {
			return nil, err
		}
		if session.IdleExpiresAt, err = parseTime(idle); err != nil {
			return nil, err
		}
		if session.AbsoluteExpiresAt, err = parseTime(absolute); err != nil {
			return nil, err
		}
		session.RevokedAt = scanOptionalTime(revoked)

		out = append(out, session)
	}
	return out, rows.Err()
}
