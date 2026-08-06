package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Authentication and sessions.
//
// # The shape of the thing
//
// A login verifies a password, issues an opaque 256-bit token, stores a KEYED
// DIGEST of it, and puts the raw token in a cookie. Every subsequent request
// digests the cookie and looks the row up through a unique index. Logout,
// password change, role change, and disablement all revoke.
//
// # What is never stored
//
// The session token. The CSRF token. The password. All three exist in memory
// for the duration of one request and nowhere else.
//
// # The CSRF token is derived, not stored
//
// csrf = HMAC(installation key, "csrf" purpose, raw session token)
//
// That single line is the whole design, and it has four properties worth
// stating:
//
//   - Nothing extra is persisted, so the requirement "do not store raw CSRF
//     tokens" is satisfied by there being nothing to store.
//   - It is deterministic, so the session endpoint can return it on every call
//     without a second round of state.
//   - It rotates automatically with the session token, so rotation-on-login and
//     rotation-on-privilege-change come free.
//   - An attacker who steals the DATABASE cannot compute it, because they have
//     the digest rather than the token. An attacker performing CSRF cannot read
//     it, because the same-origin policy stops them reading any response.
//
// # Enumeration resistance
//
// A login for an unknown username performs a full decoy Argon2id evaluation and
// returns the same error, with the same shape, as a wrong password. The audit
// log records both identically. See PasswordHasher.VerifyDecoy.

// Authentication errors.
//
// Deliberately few, and deliberately coarse. The handler maps every one of the
// first three onto ONE response, because distinguishing them for the client is
// exactly the enumeration oracle this design refuses to be.
var (
	// ErrInvalidCredentials covers an unknown username, a wrong password, and a
	// disabled account. One error for all three.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrTooManyAttempts reports that the account or the client address is in
	// backoff.
	ErrTooManyAttempts = errors.New("too many attempts")
	// ErrNoSession reports a request with no usable session.
	ErrNoSession = errors.New("no usable session")
	// ErrPasswordRejected reports a new password that failed the policy.
	ErrPasswordRejected = errors.New("password rejected")
	// ErrBootstrapClosed reports a bootstrap attempt on a claimed installation.
	ErrBootstrapClosed = errors.New("this installation already has an administrator")
	// ErrBootstrapToken reports a missing or wrong bootstrap token.
	ErrBootstrapToken = errors.New("the bootstrap token is not valid")
)

// AuthStore is the persistence the authentication service needs.
//
// A narrow interface rather than three repositories, so the surface is visible
// in one place and a test can substitute it wholesale.
type AuthStore interface {
	// Users.
	UserByUsername(ctx context.Context, username string) (domain.User, error)
	UserByID(ctx context.Context, userID string) (domain.User, error)
	CredentialFor(ctx context.Context, userID string) (store.Credential, error)
	RecordLogin(ctx context.Context, userID string, now time.Time) error
	RecordFailure(ctx context.Context, userID string, now time.Time,
		backoff func(attempts int) time.Duration) (time.Time, error)
	SetCredential(ctx context.Context, userID string, credential store.PreparedCredential,
		mustChange bool, now time.Time) error

	// Sessions.
	CreateSession(ctx context.Context, request store.NewSession, maxPerUser int,
		now time.Time) (domain.Session, error)
	SessionByTokenDigest(ctx context.Context, digest string, now time.Time) (domain.Session, error)
	TouchSession(ctx context.Context, sessionID string, lastSeen, idleExpiresAt time.Time) error
	RevokeSession(ctx context.Context, sessionID string, reason domain.SessionRevocation,
		now time.Time) error
	RevokeUserSessions(ctx context.Context, userID string, reason domain.SessionRevocation,
		except string, now time.Time) (int64, error)
	ListUserSessions(ctx context.Context, userID string, now time.Time, limit int) ([]domain.Session, error)
	ExpireStaleSessions(ctx context.Context, now time.Time, batch int) (int64, error)
	PruneSessions(ctx context.Context, cutoff time.Time, batch int) (int64, error)

	// Bootstrap.
	BootstrapState(ctx context.Context) (store.BootstrapState, error)
	SetBootstrapToken(ctx context.Context, digest string, expiresAt, now time.Time) error
	CompleteBootstrap(ctx context.Context, request store.NewUser, now time.Time) (domain.User, error)

	// Audit, for the failure counter that feeds the per-address throttle.
	RecentAuthFailures(ctx context.Context, clientAddr string, since time.Time) (int, error)
}

// AuthOptions configures an AuthService.
type AuthOptions struct {
	Store  AuthStore
	Audit  *AuditRecorder
	Key    SecretKey
	Hasher *PasswordHasher

	Config config.Auth
	Logger *slog.Logger
	Now    func() time.Time
}

// AuthService owns authentication, sessions, and the bootstrap lifecycle.
type AuthService struct {
	store  AuthStore
	audit  *AuditRecorder
	key    SecretKey
	hasher *PasswordHasher

	cfg    config.Auth
	logger *slog.Logger
	now    func() time.Time
}

// NewAuthService builds an AuthService.
func NewAuthService(opts AuthOptions) *AuthService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &AuthService{
		store:  opts.Store,
		audit:  opts.Audit,
		key:    opts.Key,
		hasher: opts.Hasher,
		cfg:    opts.Config,
		logger: logger,
		now:    now,
	}
}

// Ready reports whether the service can authenticate at all.
func (s *AuthService) Ready() bool {
	return s.store != nil && s.hasher != nil && s.key.Valid()
}

// -------------------------------------------------------------- bootstrap --

// BootstrapStatus is what an unclaimed installation tells the browser.
//
// Deliberately minimal: whether the installation is claimed, and whether a
// token is required. It reveals nothing about users, because on an unclaimed
// installation there are none, and on a claimed one this endpoint says only
// "claimed".
type BootstrapStatus struct {
	// Completed reports that an administrator exists.
	Completed bool `json:"completed"`
	// TokenRequired reports that the bootstrap POST needs the one-time token
	// printed at startup. Always true for the web flow.
	TokenRequired bool `json:"tokenRequired"`
}

// BootstrapStatus reports whether this installation has been claimed.
func (s *AuthService) BootstrapStatus(ctx context.Context) (BootstrapStatus, error) {
	state, err := s.store.BootstrapState(ctx)
	if err != nil {
		return BootstrapStatus{}, err
	}
	return BootstrapStatus{Completed: state.Completed, TokenRequired: true}, nil
}

// IssueBootstrapToken mints the one-time token for an unclaimed installation.
//
// # Why a token at all
//
// Without one, claiming a brand-new installation is a race won by whoever
// reaches the port first -- which on an exposed port is not the operator. The
// token moves the requirement from "be first" to "can read the server's log or
// data directory", which is the same bar the rest of the deployment already
// assumes.
//
// Returns the RAW token for the caller to print. Only its digest is stored, so
// a database thief cannot claim an installation either.
//
// Idempotent in the way that matters: called at every startup, it mints a fresh
// token and invalidates the previous one. An operator who lost the token
// restarts; an attacker who captured an old log does not benefit.
func (s *AuthService) IssueBootstrapToken(ctx context.Context) (string, time.Time, error) {
	state, err := s.store.BootstrapState(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	if state.Completed {
		return "", time.Time{}, ErrBootstrapClosed
	}

	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, errors.New("system entropy source unavailable")
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])

	now := s.now().UTC()
	expiresAt := now.Add(s.cfg.BootstrapTokenTTL)

	if err := s.store.SetBootstrapToken(ctx,
		s.key.HMACFor(PurposeBootstrap, token), expiresAt, now); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// BootstrapRequest is the first-administrator creation.
type BootstrapRequest struct {
	Token    string
	Username string
	Password string

	RequestID  string
	ClientAddr string
	UserAgent  string
}

// Bootstrap creates the first administrator and issues a session.
//
// # Every failure is audited, including the rejected ones
//
// An installation being claimed is the single most security-relevant moment in
// its life, and a rejected attempt at it is worth more to an administrator than
// a successful one.
func (s *AuthService) Bootstrap(
	ctx context.Context,
	request BootstrapRequest,
) (domain.User, IssuedSession, error) {
	now := s.now().UTC()

	state, err := s.store.BootstrapState(ctx)
	if err != nil {
		return domain.User{}, IssuedSession{}, err
	}
	if state.Completed {
		s.recordAudit(ctx, domain.AuditEvent{
			Action: domain.AuditBootstrapRejected, Outcome: domain.AuditDenied,
			RequestID: request.RequestID, ClientAddr: request.ClientAddr,
			Reason: "the installation already has an administrator",
		})
		return domain.User{}, IssuedSession{}, ErrBootstrapClosed
	}

	// The token, compared in constant time. A byte-by-byte comparison would
	// leak the prefix, and a token is guessable one character at a time if the
	// comparison says how far it got.
	if !state.TokenUsable(now) {
		s.recordAudit(ctx, domain.AuditEvent{
			Action: domain.AuditBootstrapRejected, Outcome: domain.AuditDenied,
			RequestID: request.RequestID, ClientAddr: request.ClientAddr,
			Reason: "no bootstrap token is currently valid",
		})
		return domain.User{}, IssuedSession{}, ErrBootstrapToken
	}
	presented := s.key.HMACFor(PurposeBootstrap, request.Token)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(state.TokenDigest)) != 1 {
		s.recordAudit(ctx, domain.AuditEvent{
			Action: domain.AuditBootstrapRejected, Outcome: domain.AuditDenied,
			RequestID: request.RequestID, ClientAddr: request.ClientAddr,
			Reason: "the bootstrap token did not match",
		})
		return domain.User{}, IssuedSession{}, ErrBootstrapToken
	}

	username := domain.NormaliseUsername(request.Username)
	if !domain.ValidUsername(username) {
		return domain.User{}, IssuedSession{}, ErrPasswordRejected
	}
	if problem := domain.CheckPassword(request.Password, username); problem != domain.PasswordOK {
		return domain.User{}, IssuedSession{}, passwordProblem(problem)
	}

	credential, err := s.hasher.Hash(request.Password)
	if err != nil {
		return domain.User{}, IssuedSession{}, err
	}

	user, err := s.store.CompleteBootstrap(ctx, store.NewUser{
		UserID:     domain.NewUserID(),
		Username:   username,
		Role:       domain.RoleAdministrator,
		Credential: credential,
	}, now)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyBootstrapped) {
			return domain.User{}, IssuedSession{}, ErrBootstrapClosed
		}
		return domain.User{}, IssuedSession{}, err
	}

	// Logged at WARN. An installation gaining its first administrator is not
	// routine, and an operator who did not expect it needs to see it.
	s.logger.WarnContext(ctx, "installation claimed: first administrator created",
		slog.String("userId", user.UserID),
		slog.String("username", user.Username))

	s.recordAudit(ctx, domain.AuditEvent{
		Action: domain.AuditBootstrapCompleted, Outcome: domain.AuditSucceeded,
		ActorUserID: user.UserID, ActorUsername: user.Username, ActorRole: user.Role,
		TargetType: domain.AuditTargetUser, TargetID: user.UserID,
		TargetName: user.Username,
		RequestID:  request.RequestID, ClientAddr: request.ClientAddr,
	})

	issued, err := s.issueSession(ctx, user, request.ClientAddr, request.UserAgent, now)
	if err != nil {
		return user, IssuedSession{}, err
	}
	return user, issued, nil
}

// ------------------------------------------------------------------ login --

// LoginRequest is a credential presentation.
type LoginRequest struct {
	Username string
	Password string

	RequestID  string
	ClientAddr string
	UserAgent  string
}

// IssuedSession is a freshly created session and the secrets that go with it.
//
// Returned ONCE, to the handler that will put the token in a cookie and the
// CSRF token in a response body. Never stored, never logged.
type IssuedSession struct {
	Session domain.Session
	// Token is the raw session token for the cookie.
	Token string
	// CSRFToken is derived from Token and returned so the SPA can hold it in
	// memory for the lifetime of the page.
	CSRFToken string
	// ExpiresAt is the cookie's Max-Age anchor.
	ExpiresAt time.Time
}

// Login verifies a credential and issues a session.
//
// # The order of the checks is deliberate
//
//  1. Per-address throttle, BEFORE any database read keyed on the username, so
//     a flood cannot be turned into a user-enumeration side channel through
//     query timing.
//  2. User lookup. An unknown user runs the decoy hash and returns.
//  3. Account lockout, from the persisted backoff.
//  4. Password verification.
//
// Every failure path returns ErrInvalidCredentials or ErrTooManyAttempts and
// nothing more specific.
func (s *AuthService) Login(ctx context.Context, request LoginRequest) (domain.User, IssuedSession, error) {
	now := s.now().UTC()
	username := domain.NormaliseUsername(request.Username)

	// ---- per-address throttle ---------------------------------------------

	if throttled, err := s.addressThrottled(ctx, request.ClientAddr, now); err != nil {
		return domain.User{}, IssuedSession{}, err
	} else if throttled {
		s.recordAudit(ctx, domain.AuditEvent{
			Action: domain.AuditLoginRateLimited, Outcome: domain.AuditDenied,
			ActorUsername: username,
			RequestID:     request.RequestID, ClientAddr: request.ClientAddr,
			Reason: "too many recent failures from this address",
		})
		return domain.User{}, IssuedSession{}, ErrTooManyAttempts
	}

	// ---- the user ----------------------------------------------------------

	user, err := s.store.UserByUsername(ctx, username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return domain.User{}, IssuedSession{}, err
		}
		// UNKNOWN USERNAME. The decoy hash costs the same as a real
		// verification, so the response time does not distinguish the two.
		s.hasher.VerifyDecoy(request.Password)
		s.auditLoginFailure(ctx, request, username, "", "unknown username or wrong password")
		return domain.User{}, IssuedSession{}, ErrInvalidCredentials
	}

	credential, err := s.store.CredentialFor(ctx, user.UserID)
	if err != nil {
		// An account with no credential cannot authenticate. Same decoy, same
		// answer: the client learns nothing about which case it hit.
		s.hasher.VerifyDecoy(request.Password)
		s.auditLoginFailure(ctx, request, username, user.UserID, "the account has no usable credential")
		return domain.User{}, IssuedSession{}, ErrInvalidCredentials
	}

	// ---- account backoff ---------------------------------------------------

	if credential.LockedUntil != nil && now.Before(*credential.LockedUntil) {
		// Still hashed, so a locked account and an unlocked one with a wrong
		// password take the same time. Without this an attacker could probe
		// which accounts they had already triggered a lockout on.
		s.hasher.VerifyDecoy(request.Password)
		s.recordAudit(ctx, domain.AuditEvent{
			Action: domain.AuditLoginRateLimited, Outcome: domain.AuditDenied,
			ActorUserID: user.UserID, ActorUsername: user.Username,
			RequestID: request.RequestID, ClientAddr: request.ClientAddr,
			Reason: "the account is in authentication backoff",
		})
		return domain.User{}, IssuedSession{}, ErrTooManyAttempts
	}

	// ---- the password ------------------------------------------------------

	if err := s.hasher.Verify(credential, request.Password); err != nil {
		if _, failErr := s.store.RecordFailure(ctx, user.UserID, now, s.backoff); failErr != nil {
			s.logger.WarnContext(ctx, "could not record an authentication failure",
				slog.String("error", failErr.Error()))
		}
		s.auditLoginFailure(ctx, request, username, user.UserID, "unknown username or wrong password")
		return domain.User{}, IssuedSession{}, ErrInvalidCredentials
	}

	// A DISABLED account is refused HERE, after the password check, so the
	// response time and the error are identical to a wrong password. Refusing
	// earlier would tell an attacker which usernames exist and are disabled.
	if !user.Active() {
		s.auditLoginFailure(ctx, request, username, user.UserID, "the account is disabled")
		return domain.User{}, IssuedSession{}, ErrInvalidCredentials
	}

	// ---- upgrade the hash if the policy has moved on -----------------------
	//
	// The one moment the plaintext is available to re-hash with.
	if s.hasher.NeedsRehash(credential) {
		if upgraded, hashErr := s.hasher.Hash(request.Password); hashErr == nil {
			if err := s.store.SetCredential(ctx, user.UserID, upgraded,
				user.MustChangePassword, now); err != nil {
				// Not fatal: the operator authenticated correctly, and failing
				// their login over a background upgrade would be perverse.
				s.logger.WarnContext(ctx, "could not upgrade a password hash",
					slog.String("userId", user.UserID))
			}
		}
	}

	if err := s.store.RecordLogin(ctx, user.UserID, now); err != nil {
		s.logger.WarnContext(ctx, "could not record a login",
			slog.String("userId", user.UserID), slog.String("error", err.Error()))
	}

	issued, err := s.issueSession(ctx, user, request.ClientAddr, request.UserAgent, now)
	if err != nil {
		return domain.User{}, IssuedSession{}, err
	}

	s.recordAudit(ctx, domain.AuditEvent{
		Action: domain.AuditLoginSucceeded, Outcome: domain.AuditSucceeded,
		ActorUserID: user.UserID, ActorUsername: user.Username, ActorRole: user.Role,
		ActorSessionID: issued.Session.SessionID,
		RequestID:      request.RequestID, ClientAddr: request.ClientAddr,
	})

	return user, issued, nil
}

// auditLoginFailure records a failed authentication.
//
// One shape for every cause. The REASON differs -- an administrator reading the
// log deserves to know whether an account is disabled -- but the client sees
// one error, and the audit action is the same, so the log's row count does not
// distinguish a real account from a guess either.
func (s *AuthService) auditLoginFailure(
	ctx context.Context,
	request LoginRequest,
	username, userID, reason string,
) {
	s.recordAudit(ctx, domain.AuditEvent{
		Action: domain.AuditLoginFailed, Outcome: domain.AuditFailed,
		ActorUserID: userID, ActorUsername: username,
		RequestID: request.RequestID, ClientAddr: request.ClientAddr,
		Reason: reason,
	})
}

// addressThrottled reports whether a client address has failed too often.
//
// Read from the AUDIT LOG rather than a separate counter: the data is already
// there, a second table would be a second thing to keep consistent, and a
// throttle that reads what an administrator reads cannot silently disagree
// with it.
//
// An unknown address is never throttled. It would otherwise be a way to lock
// everyone out at once by presenting a header that fails to parse.
func (s *AuthService) addressThrottled(ctx context.Context, clientAddr string, now time.Time) (bool, error) {
	if clientAddr == "" || clientAddr == "unknown" || s.cfg.MaxAddressFailures <= 0 {
		return false, nil
	}

	failures, err := s.store.RecentAuthFailures(ctx, clientAddr, now.Add(-s.cfg.AddressFailureWindow))
	if err != nil {
		// A throttle that cannot be evaluated must not block logins: that would
		// turn a database hiccup into a total lockout. Logged and allowed; the
		// per-account backoff still applies.
		s.logger.WarnContext(ctx, "could not evaluate the address throttle",
			slog.String("error", err.Error()))
		return false, nil
	}
	return failures >= s.cfg.MaxAddressFailures, nil
}

// backoff returns how long an account waits after n consecutive failures.
//
// Exponential, capped. A hard lockout would let anyone who knows a username
// deny that account service by guessing at it; a bounded backoff makes online
// guessing impractical while a legitimate operator waits at most the ceiling.
//
// The first two failures cost nothing, because a mistyped password is the
// common case and punishing it makes the tool unpleasant for no security gain.
func (s *AuthService) backoff(attempts int) time.Duration {
	if attempts <= 2 {
		return 0
	}

	// 1s, 2s, 4s, ... doubling per failure past the second.
	delay := time.Second
	for i := 3; i < attempts && delay < s.cfg.MaxLoginBackoff; i++ {
		delay *= 2
	}
	if delay > s.cfg.MaxLoginBackoff {
		delay = s.cfg.MaxLoginBackoff
	}
	return delay
}

// ---------------------------------------------------------------- sessions --

// issueSession creates a session and derives its CSRF token.
func (s *AuthService) issueSession(
	ctx context.Context,
	user domain.User,
	clientAddr, userAgent string,
	now time.Time,
) (IssuedSession, error) {
	token, err := domain.NewSessionToken()
	if err != nil {
		return IssuedSession{}, errors.New("system entropy source unavailable")
	}

	absolute := now.Add(s.cfg.SessionAbsoluteTTL)
	idle := now.Add(s.cfg.SessionIdleTTL)
	// The idle expiry can never exceed the absolute one; otherwise a long idle
	// window would silently extend the hard ceiling.
	if idle.After(absolute) {
		idle = absolute
	}

	session, err := s.store.CreateSession(ctx, store.NewSession{
		SessionID:         domain.NewSessionID(),
		UserID:            user.UserID,
		Username:          user.Username,
		Role:              user.Role,
		TokenDigest:       s.key.HMACFor(PurposeSession, token),
		IdleExpiresAt:     idle,
		AbsoluteExpiresAt: absolute,
		UserAgent:         userAgent,
		ClientAddr:        clientAddr,
	}, s.cfg.MaxSessionsPerUser, now)
	if err != nil {
		return IssuedSession{}, err
	}

	return IssuedSession{
		Session:   session,
		Token:     token,
		CSRFToken: s.CSRFToken(token),
		ExpiresAt: absolute,
	}, nil
}

// CSRFToken derives the CSRF token for a session token.
//
// See the file header. Deterministic, unstored, and rotating with the session.
func (s *AuthService) CSRFToken(sessionToken string) string {
	if sessionToken == "" {
		return ""
	}
	return s.key.HMACFor(PurposeCSRF, sessionToken)
}

// ValidCSRF reports whether a presented CSRF token matches the session.
//
// Constant-time, for the same reason the bootstrap token comparison is: a
// comparison that returns early tells an attacker how much of the token they
// have guessed.
func (s *AuthService) ValidCSRF(sessionToken, presented string) bool {
	if sessionToken == "" || presented == "" {
		return false
	}
	expected := s.CSRFToken(sessionToken)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

// Authenticated is a resolved identity for one request.
type Authenticated struct {
	User    domain.User
	Session domain.Session
	// Token is the raw session token, carried so the CSRF check can derive
	// against it. Never logged and never serialised.
	Token string
}

// Authenticate resolves a session token to a live identity.
//
// # Two reads, deliberately
//
// The session row, then the USER row. The user is re-read on every request
// rather than trusted from the session's snapshot, which is what makes a role
// change, a disablement, and a password change take effect immediately rather
// than at the next login.
//
// The cost is one indexed lookup per request against a table with as many rows
// as there are operators.
func (s *AuthService) Authenticate(ctx context.Context, token string) (Authenticated, error) {
	if !domain.ValidSessionToken(token) {
		// Rejected on SHAPE before it reaches a digest or a query. A cookie is
		// attacker-controlled and there is no reason to hash a megabyte of it.
		return Authenticated{}, ErrNoSession
	}

	now := s.now().UTC()

	session, err := s.store.SessionByTokenDigest(ctx, s.key.HMACFor(PurposeSession, token), now)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			return Authenticated{}, ErrNoSession
		}
		return Authenticated{}, err
	}

	user, err := s.store.UserByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Authenticated{}, ErrNoSession
		}
		return Authenticated{}, err
	}
	if !user.Active() {
		// The session outlived a disablement. Revoked now so the next request
		// does not have to notice again, and refused immediately.
		if err := s.store.RevokeSession(ctx, session.SessionID,
			domain.SessionUserDisabled, now); err != nil {
			s.logger.WarnContext(ctx, "could not revoke a disabled user's session",
				slog.String("error", err.Error()))
		}
		return Authenticated{}, ErrNoSession
	}

	// Idle expiry moves forward, but not on every request: see
	// SessionRepository.Touch for why a read must not become a write.
	if now.Sub(session.LastSeenAt) >= s.cfg.SessionTouchInterval {
		idle := now.Add(s.cfg.SessionIdleTTL)
		if idle.After(session.AbsoluteExpiresAt) {
			idle = session.AbsoluteExpiresAt
		}
		if err := s.store.TouchSession(ctx, session.SessionID, now, idle); err != nil {
			s.logger.WarnContext(ctx, "could not extend a session",
				slog.String("error", err.Error()))
		}
	}

	return Authenticated{User: user, Session: session, Token: token}, nil
}

// Logout ends one session.
func (s *AuthService) Logout(ctx context.Context, identity Authenticated, requestID, clientAddr string) error {
	now := s.now().UTC()

	if err := s.store.RevokeSession(ctx, identity.Session.SessionID,
		domain.SessionLoggedOut, now); err != nil {
		return err
	}

	s.recordAudit(ctx, domain.AuditEvent{
		Action: domain.AuditLogout, Outcome: domain.AuditSucceeded,
		ActorUserID: identity.User.UserID, ActorUsername: identity.User.Username,
		ActorRole: identity.User.Role, ActorSessionID: identity.Session.SessionID,
		TargetType: domain.AuditTargetSession, TargetID: identity.Session.SessionID,
		RequestID: requestID, ClientAddr: clientAddr,
	})
	return nil
}

// ListSessions returns an account's live sessions.
func (s *AuthService) ListSessions(ctx context.Context, userID, currentSessionID string) ([]domain.Session, error) {
	sessions, err := s.store.ListUserSessions(ctx, userID, s.now().UTC(), s.cfg.MaxSessionsPerUser*2)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].Current = sessions[i].SessionID == currentSessionID
	}
	return sessions, nil
}

// RevokeSession ends one session an operator named.
//
// The caller has already established that the session belongs to them, or that
// they hold user:manage. Checking ownership here as well would duplicate a
// decision the authorization layer already made in one place.
func (s *AuthService) RevokeSession(
	ctx context.Context,
	sessionID string,
	reason domain.SessionRevocation,
	actor Authenticated,
	requestID, clientAddr string,
) error {
	now := s.now().UTC()
	if err := s.store.RevokeSession(ctx, sessionID, reason, now); err != nil {
		return err
	}

	s.recordAudit(ctx, domain.AuditEvent{
		Action: domain.AuditSessionRevoked, Outcome: domain.AuditSucceeded,
		ActorUserID: actor.User.UserID, ActorUsername: actor.User.Username,
		ActorRole: actor.User.Role, ActorSessionID: actor.Session.SessionID,
		TargetType: domain.AuditTargetSession, TargetID: sessionID,
		RequestID: requestID, ClientAddr: clientAddr,
		Reason: string(reason),
	})
	return nil
}

// --------------------------------------------------------------- passwords --

// ChangePasswordRequest is an operator changing their own password.
type ChangePasswordRequest struct {
	CurrentPassword string
	NewPassword     string

	RequestID  string
	ClientAddr string
}

// ChangePassword replaces an operator's own credential.
//
// # The current password is required
//
// Even though the request is already authenticated. A session is a
// possession factor; the current password is a knowledge factor, and requiring
// it stops a stolen session from being upgraded into permanent account control.
//
// # EVERY session is revoked, and the caller gets a new one
//
// Including any a thief holds. The caller is not logged out: a fresh session is
// issued and returned, so the page that just succeeded keeps working under a
// new token.
//
// Rotation rather than sparing the caller's row, and the reason is a real
// conflict between two controls. SetCredential stamps password_changed_at, and
// SessionByTokenDigest requires a session to be NEWER than that stamp -- the
// belt that survives a crash between the revocation and the write. A session
// spared from revocation would still fail that check, so "your own session
// survives" was a promise the storage layer could not keep. Issuing a new one
// keeps the operator signed in AND keeps the timestamp guard absolute.
func (s *AuthService) ChangePassword(
	ctx context.Context,
	identity Authenticated,
	request ChangePasswordRequest,
) (IssuedSession, error) {
	now := s.now().UTC()

	credential, err := s.store.CredentialFor(ctx, identity.User.UserID)
	if err != nil {
		return IssuedSession{}, ErrInvalidCredentials
	}
	if err := s.hasher.Verify(credential, request.CurrentPassword); err != nil {
		s.recordAudit(ctx, domain.AuditEvent{
			Action: domain.AuditPasswordChanged, Outcome: domain.AuditDenied,
			ActorUserID: identity.User.UserID, ActorUsername: identity.User.Username,
			ActorRole: identity.User.Role, ActorSessionID: identity.Session.SessionID,
			RequestID: request.RequestID, ClientAddr: request.ClientAddr,
			Reason: "the current password did not match",
		})
		return IssuedSession{}, ErrInvalidCredentials
	}

	if problem := domain.CheckPassword(request.NewPassword, identity.User.Username); problem != domain.PasswordOK {
		return IssuedSession{}, passwordProblem(problem)
	}

	prepared, err := s.hasher.Hash(request.NewPassword)
	if err != nil {
		return IssuedSession{}, err
	}
	// mustChange is cleared: the account holder has now chosen their own
	// password, which is the whole condition the flag exists to enforce.
	if err := s.store.SetCredential(ctx, identity.User.UserID, prepared, false, now); err != nil {
		return IssuedSession{}, err
	}

	// Empty `except`: every session goes, including the caller's. See the
	// doc comment for why sparing one would not have worked anyway.
	revoked, err := s.store.RevokeUserSessions(ctx, identity.User.UserID,
		domain.SessionPasswordChanged, "", now)
	if err != nil {
		s.logger.WarnContext(ctx, "could not revoke sessions after a password change",
			slog.String("userId", identity.User.UserID), slog.String("error", err.Error()))
	}

	// The user record is re-read so the new session carries the CLEARED
	// must-change flag rather than the stale one the request arrived with.
	user, err := s.store.UserByID(ctx, identity.User.UserID)
	if err != nil {
		return IssuedSession{}, err
	}

	issued, err := s.issueSession(ctx, user,
		request.ClientAddr, identity.Session.UserAgent, now)
	if err != nil {
		// The password IS changed and every session is gone. Reporting the
		// error is right -- the caller must sign in again -- but it must not
		// read as "nothing happened".
		return IssuedSession{}, fmt.Errorf(
			"the password was changed; sign in again: %w", err)
	}

	s.recordAudit(ctx, domain.AuditEvent{
		Action: domain.AuditPasswordChanged, Outcome: domain.AuditSucceeded,
		ActorUserID: user.UserID, ActorUsername: user.Username,
		ActorRole: user.Role, ActorSessionID: identity.Session.SessionID,
		TargetType: domain.AuditTargetUser, TargetID: user.UserID,
		TargetName: user.Username,
		RequestID:  request.RequestID, ClientAddr: request.ClientAddr,
		Reason: sessionsRevokedReason(revoked) + "; a new session was issued",
	})
	return issued, nil
}

// ------------------------------------------------------------- maintenance --

// SweepSessions expires and prunes sessions.
//
// Called on a timer. Neither operation is load-bearing for correctness --
// SessionByTokenDigest already refuses an expired session -- but the session
// list stays honest and the table stays bounded.
func (s *AuthService) SweepSessions(ctx context.Context) {
	now := s.now().UTC()

	if expired, err := s.store.ExpireStaleSessions(ctx, now, sessionSweepBatch); err != nil {
		s.logger.WarnContext(ctx, "could not expire stale sessions",
			slog.String("error", err.Error()))
	} else if expired > 0 {
		s.logger.DebugContext(ctx, "expired stale sessions", slog.Int64("count", expired))
	}

	if s.cfg.SessionRetention <= 0 {
		return
	}
	cutoff := now.Add(-s.cfg.SessionRetention)
	if pruned, err := s.store.PruneSessions(ctx, cutoff, sessionSweepBatch); err != nil {
		s.logger.WarnContext(ctx, "could not prune revoked sessions",
			slog.String("error", err.Error()))
	} else if pruned > 0 {
		s.logger.DebugContext(ctx, "pruned revoked sessions", slog.Int64("count", pruned))
	}
}

// sessionSweepBatch bounds one maintenance transaction.
const sessionSweepBatch = 500

// Run sweeps sessions on an interval until ctx is cancelled.
//
// Off the request path deliberately: a sweep holds the single SQLite writer for
// as long as it takes to delete a batch, and no login should ever wait on that.
//
// A sweep does not run at startup. The session table is bounded by the
// per-user cap and by expiry being checked on every lookup, so there is nothing
// urgent to do in the first seconds of a process -- which are the seconds an
// operator is most likely to be trying to log in.
func (s *AuthService) Run(ctx context.Context) {
	if !s.Ready() || s.cfg.SessionSweepInterval <= 0 {
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(s.cfg.SessionSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepSessions(ctx)
		}
	}
}

// ---------------------------------------------------------------- helpers --

// recordAudit writes an audit row, tolerating failure.
//
// A failed audit write must not fail the action: see AuditRepository.Record for
// why. Nil-safe so a partially wired service in a test does not panic.
func (s *AuthService) recordAudit(ctx context.Context, event domain.AuditEvent) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, event)
}

// passwordProblem converts a policy verdict into an error the API can render.
//
// The verdict travels in the error so the handler can name the specific
// problem, and the error text never contains the password.
func passwordProblem(problem domain.PasswordProblem) error {
	return PasswordRejectedError{Problem: problem}
}

// PasswordRejectedError carries which policy rule refused a password.
type PasswordRejectedError struct {
	Problem domain.PasswordProblem
}

func (e PasswordRejectedError) Error() string { return e.Problem.Explain() }

// Unwrap lets callers match ErrPasswordRejected without knowing the shape.
func (e PasswordRejectedError) Unwrap() error { return ErrPasswordRejected }

// sessionsRevokedReason renders the count for an audit row.
func sessionsRevokedReason(revoked int64) string {
	if revoked <= 0 {
		return "no other sessions were active"
	}
	if revoked == 1 {
		return "1 other session was revoked"
	}
	return itoa(revoked) + " other sessions were revoked"
}

// itoa avoids importing strconv into this file for one call.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
