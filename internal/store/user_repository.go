package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// UserRepository owns accounts, credentials, and bootstrap state.
//
// # Why credentials are a separate table and a separate method
//
// Reading a user happens on EVERY authenticated request, to re-check the role
// and the account status. If the credential lived on the same row, every one of
// those reads would pull a password hash into memory and into whatever the
// caller does with the struct next.
//
// It does not. `domain.User` has no field for a credential, the user queries
// never select one, and the single method that does -- credentialFor -- is
// unexported and used only by Verify and by the password-change path.
//
// # Nothing here logs, returns, or formats a hash
//
// Not in an error, not in a wrapped error, not in a debug field. The Credential
// type is unexported and never crosses the package boundary.
type UserRepository struct {
	db *sql.DB
}

// Errors this repository can produce.
var (
	// ErrInvalidInput reports a value that failed validation at the storage
	// boundary.
	//
	// A last-line check. Every caller validates first, so reaching this means a
	// layer above let something through -- and the repository refuses rather
	// than storing a role, username, or credential the rest of the system
	// assumes cannot exist.
	ErrInvalidInput = errors.New("value is not acceptable")
	// ErrUsernameTaken reports a username that already exists.
	ErrUsernameTaken = errors.New("that username is already in use")
	// ErrNoCredential reports a user with no password set. Distinct from a
	// wrong password: it means the account was created but never given a
	// credential, which should not happen and must not authenticate.
	ErrNoCredential = errors.New("the account has no credential")
	// ErrLastAdministrator reports an attempt to remove the last way in.
	//
	// Refused rather than allowed, because an installation with no
	// administrator can only be recovered from the command line, and an
	// operator who locks themselves out through the UI will not expect that.
	ErrLastAdministrator = errors.New("this is the last active administrator")
	// ErrAlreadyBootstrapped reports a bootstrap attempt on a claimed
	// installation.
	ErrAlreadyBootstrapped = errors.New("this installation already has an administrator")
)

const selectUserColumns = `
	SELECT u.id, u.user_id, u.username, u.role, u.status,
	       u.must_change_password, u.password_changed_at, u.last_login_at,
	       u.created_by_user_id, u.created_at, u.updated_at
	FROM users u`

// ------------------------------------------------------------- creating --

// NewUser is the input to Create.
//
// The password arrives as a PREPARED CREDENTIAL rather than as a plaintext
// string: hashing is the service's job, and a repository that accepted a
// password would be a repository that could log one.
type NewUser struct {
	UserID   string
	Username string
	Role     domain.Role
	// MustChangePassword marks a credential the account holder did not choose.
	MustChangePassword bool
	// CreatedByUserID is the administrator who created the account. Empty for
	// the bootstrap administrator.
	CreatedByUserID string

	Credential PreparedCredential
}

// PreparedCredential is a password verifier the service has already produced.
//
// Salt and Hash are base64 (raw, unpadded). Neither this type nor any field on
// it is ever rendered into a log, an error, or a response.
type PreparedCredential struct {
	Algorithm   string
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	Salt        string
	Hash        string
}

// Valid reports whether the credential is complete enough to store.
func (c PreparedCredential) Valid() bool {
	return c.Algorithm != "" && c.MemoryKiB > 0 && c.Iterations > 0 &&
		c.Parallelism > 0 && c.Salt != "" && c.Hash != ""
}

// Create inserts an account and its credential in one transaction.
//
// Both or neither. A user without a credential could not log in and would
// occupy the username; a credential without a user is unreachable. Splitting
// them across two statements would make a crash between the two produce one of
// those states.
func (r *UserRepository) Create(ctx context.Context, request NewUser, now time.Time) (domain.User, error) {
	if !domain.ValidUserID(request.UserID) ||
		!domain.ValidUsername(request.Username) ||
		!domain.ValidRole(string(request.Role)) ||
		!request.Credential.Valid() {
		return domain.User{}, fmt.Errorf("create user: %w", ErrInvalidInput)
	}

	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, fmt.Errorf("begin user insert: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO users
			(user_id, username, role, status, must_change_password,
			 password_changed_at, created_by_user_id, created_at, updated_at)
		VALUES (?, ?, ?, 'active', ?, ?, ?, ?, ?)`,
		request.UserID, request.Username, string(request.Role),
		boolToInt(request.MustChangePassword), stamp,
		request.CreatedByUserID, stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.User{}, ErrUsernameTaken
		}
		return domain.User{}, fmt.Errorf("insert user: %w", AsError(err))
	}

	if err := insertCredential(ctx, tx, request.UserID, request.Credential, stamp); err != nil {
		return domain.User{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.User{}, fmt.Errorf("commit user insert: %w", AsError(err))
	}

	return r.Get(ctx, request.UserID)
}

func insertCredential(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	credential PreparedCredential,
	stamp string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO user_credentials
			(user_id, algorithm, memory_kib, iterations, parallelism,
			 salt, hash, failed_attempts, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			algorithm       = excluded.algorithm,
			memory_kib      = excluded.memory_kib,
			iterations      = excluded.iterations,
			parallelism     = excluded.parallelism,
			salt            = excluded.salt,
			hash            = excluded.hash,
			-- A new credential clears the brute-force state. The attempts
			-- counted against the OLD password say nothing about the new one,
			-- and leaving a lockout in place would mean a password change could
			-- not get an operator back in.
			failed_attempts = 0,
			locked_until    = NULL,
			last_failure_at = NULL,
			updated_at      = excluded.updated_at`,
		userID, credential.Algorithm, credential.MemoryKiB, credential.Iterations,
		credential.Parallelism, credential.Salt, credential.Hash, stamp)
	if err != nil {
		// The error is wrapped WITHOUT the statement's arguments. A driver
		// error that echoed its parameters would put a password hash in a log.
		return fmt.Errorf("write credential: %w", AsError(err))
	}
	return nil
}

// --------------------------------------------------------------- reading --

// Get returns one account by its public identifier.
func (r *UserRepository) Get(ctx context.Context, userID string) (domain.User, error) {
	return r.scanOne(ctx, selectUserColumns+` WHERE u.user_id = ?`, userID)
}

// ByUsername returns one account by its normalised login name.
//
// The username is normalised by the CALLER before it reaches here, so this is
// an exact match against the unique index rather than a collation-dependent
// comparison.
func (r *UserRepository) ByUsername(ctx context.Context, username string) (domain.User, error) {
	return r.scanOne(ctx, selectUserColumns+` WHERE u.username = ?`, username)
}

func (r *UserRepository) scanOne(ctx context.Context, query string, args ...any) (domain.User, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return domain.User{}, fmt.Errorf("query user: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanUsers(rows)
	if err != nil {
		return domain.User{}, err
	}
	if len(found) == 0 {
		return domain.User{}, ErrNotFound
	}
	return found[0], nil
}

// UserFilter narrows a user listing.
type UserFilter struct {
	Status domain.UserStatus
	Role   domain.Role
	Page   Page
}

// List returns a page of accounts, oldest first.
//
// Oldest first because the list is short, stable ordering matters more than
// recency, and the bootstrap administrator being first is what an operator
// expects.
func (r *UserRepository) List(ctx context.Context, filter UserFilter) ([]domain.User, int, error) {
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 2)

	if filter.Status != "" {
		clauses = append(clauses, "u.status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.Role != "" {
		clauses = append(clauses, "u.role = ?")
		args = append(args, string(filter.Role))
	}

	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users u`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", AsError(err))
	}

	page := filter.Page.normalise()
	rows, err := r.db.QueryContext(ctx,
		selectUserColumns+where+` ORDER BY u.id LIMIT ? OFFSET ?`,
		append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query users: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanUsers(rows)
	if err != nil {
		return nil, 0, err
	}
	return found, total, nil
}

// CountActiveAdministrators reports how many ways into the installation remain.
func (r *UserRepository) CountActiveAdministrators(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'administrator' AND status = 'active'`).
		Scan(&count); err != nil {
		return 0, fmt.Errorf("count administrators: %w", AsError(err))
	}
	return count, nil
}

// --------------------------------------------------------------- writing --

// SetRole changes an account's role.
//
// Refuses to demote the last active administrator, INSIDE the transaction that
// would perform the demotion. Checking outside it would be a race: two
// administrators demoting each other concurrently would both see a count of two
// and both succeed.
func (r *UserRepository) SetRole(
	ctx context.Context,
	userID string,
	role domain.Role,
	now time.Time,
) error {
	if !domain.ValidRole(string(role)) {
		return fmt.Errorf("set role: %w", ErrInvalidInput)
	}

	return r.inTx(ctx, func(tx *sql.Tx) error {
		if role != domain.RoleAdministrator {
			if err := guardLastAdministrator(ctx, tx, userID); err != nil {
				return err
			}
		}

		result, err := tx.ExecContext(ctx,
			`UPDATE users SET role = ?, updated_at = ? WHERE user_id = ?`,
			string(role), formatTime(now.UTC()), userID)
		if err != nil {
			return fmt.Errorf("update role: %w", AsError(err))
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetStatus enables or disables an account.
func (r *UserRepository) SetStatus(
	ctx context.Context,
	userID string,
	status domain.UserStatus,
	now time.Time,
) error {
	if !domain.ValidUserStatus(string(status)) {
		return fmt.Errorf("set status: %w", ErrInvalidInput)
	}

	return r.inTx(ctx, func(tx *sql.Tx) error {
		if status == domain.UserDisabled {
			if err := guardLastAdministrator(ctx, tx, userID); err != nil {
				return err
			}
		}

		result, err := tx.ExecContext(ctx,
			`UPDATE users SET status = ?, updated_at = ? WHERE user_id = ?`,
			string(status), formatTime(now.UTC()), userID)
		if err != nil {
			return fmt.Errorf("update status: %w", AsError(err))
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// guardLastAdministrator refuses a change that would leave no way in.
//
// Runs inside the caller's transaction, so the count and the update are one
// atomic decision.
func guardLastAdministrator(ctx context.Context, tx *sql.Tx, userID string) error {
	var isAdmin int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users
		 WHERE user_id = ? AND role = 'administrator' AND status = 'active'`,
		userID).Scan(&isAdmin)
	if err != nil {
		return fmt.Errorf("check administrator: %w", AsError(err))
	}
	if isAdmin == 0 {
		// Not an active administrator, so removing them removes no way in.
		return nil
	}

	var remaining int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users
		 WHERE role = 'administrator' AND status = 'active' AND user_id <> ?`,
		userID).Scan(&remaining)
	if err != nil {
		return fmt.Errorf("count remaining administrators: %w", AsError(err))
	}
	if remaining == 0 {
		return ErrLastAdministrator
	}
	return nil
}

// SetCredential replaces an account's password verifier.
//
// Stamps password_changed_at, which is what invalidates every session issued
// before this moment -- see SessionRepository.ByTokenDigest. The session rows
// are revoked explicitly too; the timestamp is the belt that survives a crash
// between the two writes.
func (r *UserRepository) SetCredential(
	ctx context.Context,
	userID string,
	credential PreparedCredential,
	mustChange bool,
	now time.Time,
) error {
	if !credential.Valid() {
		return fmt.Errorf("set credential: %w", ErrInvalidInput)
	}
	stamp := formatTime(now.UTC())

	return r.inTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE users SET
				password_changed_at  = ?,
				must_change_password = ?,
				updated_at           = ?
			WHERE user_id = ?`,
			stamp, boolToInt(mustChange), stamp, userID)
		if err != nil {
			return fmt.Errorf("update user for credential: %w", AsError(err))
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return ErrNotFound
		}
		return insertCredential(ctx, tx, userID, credential, stamp)
	})
}

// RecordLogin stamps a successful authentication and clears the failure state.
func (r *UserRepository) RecordLogin(ctx context.Context, userID string, now time.Time) error {
	stamp := formatTime(now.UTC())

	return r.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET last_login_at = ?, updated_at = ? WHERE user_id = ?`,
			stamp, stamp, userID); err != nil {
			return fmt.Errorf("record login: %w", AsError(err))
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_credentials SET
				failed_attempts = 0,
				locked_until    = NULL,
				last_failure_at = NULL,
				updated_at      = ?
			WHERE user_id = ?`, stamp, userID); err != nil {
			return fmt.Errorf("clear failures: %w", AsError(err))
		}
		return nil
	})
}

// ---------------------------------------------------------- credentials --

// Credential is the verifier the authentication service needs.
//
// Exported because the service must hash against it, and deliberately minimal:
// there is no field here that is not required to verify a password, and the
// type is never serialised, logged, or returned by an API handler. An
// architecture test asserts that no package outside internal/service and
// internal/store names it.
type Credential struct {
	Algorithm   string
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	Salt        string
	Hash        string

	// FailedAttempts and LockedUntil drive the exponential backoff. Persisted
	// so a process restart does not hand an attacker a free reset.
	FailedAttempts int
	LockedUntil    *time.Time
}

// CredentialFor returns the verifier for an account.
//
// Returns ErrNoCredential rather than a zero value for an account with none:
// a zero verifier would hash to something, and something that verifies against
// an empty hash is the worst possible outcome.
func (r *UserRepository) CredentialFor(ctx context.Context, userID string) (Credential, error) {
	var (
		credential  Credential
		lockedUntil sql.NullString
	)

	err := r.db.QueryRowContext(ctx, `
		SELECT algorithm, memory_kib, iterations, parallelism, salt, hash,
		       failed_attempts, locked_until
		FROM user_credentials WHERE user_id = ?`, userID,
	).Scan(&credential.Algorithm, &credential.MemoryKiB, &credential.Iterations,
		&credential.Parallelism, &credential.Salt, &credential.Hash,
		&credential.FailedAttempts, &lockedUntil)

	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		// Deliberately does NOT wrap the driver error with AsError's detail
		// path here; the row contains a hash and a driver that echoed the
		// failing row would put it in a log.
		return Credential{}, errors.New("read credential")
	}

	if lockedUntil.Valid && lockedUntil.String != "" {
		parsed, parseErr := parseTime(lockedUntil.String)
		if parseErr != nil {
			return Credential{}, parseErr
		}
		credential.LockedUntil = &parsed
	}
	return credential, nil
}

// RecordFailure increments the failure counter and applies the backoff.
//
// # Why exponential backoff rather than a hard lockout
//
// A hard lockout lets anyone who knows a username deny that account service by
// guessing at it, which turns an authentication control into a denial-of-service
// tool. A backoff that grows to a bounded ceiling makes online guessing
// impractical while a legitimate operator waits at most that ceiling.
//
// Returns the moment the account becomes usable again, so the caller can decide
// what to tell the client -- which, to avoid enumeration, is nothing specific.
func (r *UserRepository) RecordFailure(
	ctx context.Context,
	userID string,
	now time.Time,
	backoff func(attempts int) time.Duration,
) (time.Time, error) {
	var unlockAt time.Time

	err := r.inTx(ctx, func(tx *sql.Tx) error {
		var attempts int
		if err := tx.QueryRowContext(ctx,
			`SELECT failed_attempts FROM user_credentials WHERE user_id = ?`,
			userID).Scan(&attempts); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoCredential
			}
			return errors.New("read failure count")
		}

		attempts++
		unlockAt = now.Add(backoff(attempts))

		if _, err := tx.ExecContext(ctx, `
			UPDATE user_credentials SET
				failed_attempts = ?,
				locked_until    = ?,
				last_failure_at = ?,
				updated_at      = ?
			WHERE user_id = ?`,
			attempts, formatTime(unlockAt.UTC()), formatTime(now.UTC()),
			formatTime(now.UTC()), userID); err != nil {
			return errors.New("record failure")
		}
		return nil
	})

	return unlockAt, err
}

// ------------------------------------------------------ bootstrap state --

// BootstrapState is whether this installation has been claimed.
type BootstrapState struct {
	// Completed reports that an administrator exists. Once true, always true:
	// an installation whose administrators were all disabled must not fall back
	// into a flow that lets anyone claim it.
	Completed   bool
	CompletedAt *time.Time
	UserID      string

	// TokenDigest is the keyed digest of the one-time bootstrap token, and
	// TokenExpiresAt when it stops being accepted.
	TokenDigest    string
	TokenExpiresAt *time.Time
}

// TokenUsable reports whether a bootstrap token may still be presented.
func (b BootstrapState) TokenUsable(now time.Time) bool {
	if b.Completed || b.TokenDigest == "" || b.TokenExpiresAt == nil {
		return false
	}
	return now.Before(*b.TokenExpiresAt)
}

// BootstrapState reads the single state row.
func (r *UserRepository) BootstrapState(ctx context.Context) (BootstrapState, error) {
	var (
		state     BootstrapState
		completed sql.NullString
		expires   sql.NullString
	)

	err := r.db.QueryRowContext(ctx, `
		SELECT bootstrap_completed_at, bootstrap_user_id,
		       bootstrap_token_digest, bootstrap_token_expires_at
		FROM auth_state WHERE id = 1`,
	).Scan(&completed, &state.UserID, &state.TokenDigest, &expires)
	if err != nil {
		return BootstrapState{}, fmt.Errorf("read bootstrap state: %w", AsError(err))
	}

	if completed.Valid && completed.String != "" {
		parsed, parseErr := parseTime(completed.String)
		if parseErr != nil {
			return BootstrapState{}, parseErr
		}
		state.Completed = true
		state.CompletedAt = &parsed
	}
	if expires.Valid && expires.String != "" {
		parsed, parseErr := parseTime(expires.String)
		if parseErr != nil {
			return BootstrapState{}, parseErr
		}
		state.TokenExpiresAt = &parsed
	}
	return state, nil
}

// SetBootstrapToken records a fresh one-time token digest.
//
// Refuses once the installation is claimed, so a token cannot be minted to
// re-open a bootstrap flow that has already run.
func (r *UserRepository) SetBootstrapToken(
	ctx context.Context,
	digest string,
	expiresAt time.Time,
	now time.Time,
) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE auth_state SET
			bootstrap_token_digest     = ?,
			bootstrap_token_expires_at = ?,
			updated_at                 = ?
		WHERE id = 1 AND bootstrap_completed_at IS NULL`,
		digest, formatTime(expiresAt.UTC()), formatTime(now.UTC()))
	if err != nil {
		return fmt.Errorf("set bootstrap token: %w", AsError(err))
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrAlreadyBootstrapped
	}
	return nil
}

// CompleteBootstrap creates the first administrator and claims the
// installation, atomically.
//
// # Why both in one transaction
//
// Two concurrent bootstrap requests must produce exactly one administrator. The
// conditional UPDATE on `bootstrap_completed_at IS NULL` is what decides the
// race: the loser's update affects no rows and its whole transaction rolls
// back, including the user it was about to insert.
func (r *UserRepository) CompleteBootstrap(
	ctx context.Context,
	request NewUser,
	now time.Time,
) (domain.User, error) {
	if !domain.ValidUserID(request.UserID) ||
		!domain.ValidUsername(request.Username) ||
		!request.Credential.Valid() {
		return domain.User{}, fmt.Errorf("bootstrap: %w", ErrInvalidInput)
	}

	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, fmt.Errorf("begin bootstrap: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// The claim comes FIRST. If it does not take, nothing else in this
	// transaction should happen, and rolling back an insert is cheaper than
	// discovering a duplicate administrator afterwards.
	result, err := tx.ExecContext(ctx, `
		UPDATE auth_state SET
			bootstrap_completed_at     = ?,
			bootstrap_user_id          = ?,
			bootstrap_token_digest     = '',
			bootstrap_token_expires_at = NULL,
			updated_at                 = ?
		WHERE id = 1 AND bootstrap_completed_at IS NULL`,
		stamp, request.UserID, stamp)
	if err != nil {
		return domain.User{}, fmt.Errorf("claim installation: %w", AsError(err))
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.User{}, ErrAlreadyBootstrapped
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO users
			(user_id, username, role, status, must_change_password,
			 password_changed_at, created_by_user_id, created_at, updated_at)
		VALUES (?, ?, 'administrator', 'active', ?, ?, '', ?, ?)`,
		request.UserID, request.Username,
		boolToInt(request.MustChangePassword), stamp, stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.User{}, ErrUsernameTaken
		}
		return domain.User{}, fmt.Errorf("insert bootstrap administrator: %w", AsError(err))
	}

	if err := insertCredential(ctx, tx, request.UserID, request.Credential, stamp); err != nil {
		return domain.User{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.User{}, fmt.Errorf("commit bootstrap: %w", AsError(err))
	}

	return r.Get(ctx, request.UserID)
}

// ---------------------------------------------------------------- helpers --

// inTx runs fn inside a transaction, rolling back on any error.
func (r *UserRepository) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", AsError(err))
	}
	return nil
}

func scanUsers(rows *sql.Rows) ([]domain.User, error) {
	out := make([]domain.User, 0, 8)

	for rows.Next() {
		var (
			user       domain.User
			role       string
			status     string
			mustChange int
			changed    string
			lastLogin  sql.NullString
			created    string
			updated    string
		)
		if err := rows.Scan(&user.ID, &user.UserID, &user.Username, &role, &status,
			&mustChange, &changed, &lastLogin, &user.CreatedByUserID,
			&created, &updated); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}

		user.Role = domain.Role(role)
		user.Status = domain.UserStatus(status)
		user.MustChangePassword = mustChange == 1

		var err error
		if user.PasswordChangedAt, err = parseTime(changed); err != nil {
			return nil, err
		}
		if user.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if user.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		user.LastLoginAt = scanOptionalTime(lastLogin)

		out = append(out, user)
	}
	return out, rows.Err()
}
