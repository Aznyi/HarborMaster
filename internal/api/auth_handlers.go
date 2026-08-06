package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The authentication endpoints.
//
// # One response for every credential failure
//
// Login answers 401 with the same code and the same prose whether the username
// was unknown, the password was wrong, or the account was disabled. The service
// makes the TIMING match too, by hashing against a decoy. Together those are
// what stop the login endpoint being an account directory.
//
// # No token ever reaches a response body
//
// The session token goes in a Set-Cookie header and nowhere else. The CSRF
// token does go in a body -- it has to, because JavaScript must send it back in
// a header -- and it is useless without the cookie a script cannot read.

// AuthService is the authentication capability the API depends on.
//
// A narrow interface rather than *service.AuthService, so the handlers stay
// testable and the surface the API can reach is visible in one place.
type AuthService interface {
	BootstrapStatus(ctx context.Context) (service.BootstrapStatus, error)
	Bootstrap(ctx context.Context, request service.BootstrapRequest) (domain.User, service.IssuedSession, error)
	Login(ctx context.Context, request service.LoginRequest) (domain.User, service.IssuedSession, error)
	Logout(ctx context.Context, identity service.Authenticated, requestID, clientAddr string) error
	Authenticate(ctx context.Context, token string) (service.Authenticated, error)
	ChangePassword(ctx context.Context, identity service.Authenticated,
		request service.ChangePasswordRequest) (service.IssuedSession, error)
	ListSessions(ctx context.Context, userID, currentSessionID string) ([]domain.Session, error)
	RevokeSession(ctx context.Context, sessionID string, reason domain.SessionRevocation,
		actor service.Authenticated, requestID, clientAddr string) error
	CSRFToken(sessionToken string) string
	ValidCSRF(sessionToken, presented string) bool
}

// UserService is the account administration capability.
type UserService interface {
	Create(ctx context.Context, actor service.Actor, request service.CreateUserRequest) (service.CreatedUser, error)
	Get(ctx context.Context, userID string) (domain.User, error)
	List(ctx context.Context, filter store.UserFilter) ([]domain.User, int, error)
	SetRole(ctx context.Context, actor service.Actor, userID string, role domain.Role) (domain.User, error)
	SetStatus(ctx context.Context, actor service.Actor, userID string, status domain.UserStatus) (domain.User, error)
	ResetPassword(ctx context.Context, actor service.Actor, userID string) (string, error)
}

// AuditService is the security audit capability.
type AuditService interface {
	Record(ctx context.Context, event domain.AuditEvent)
	RecordAction(ctx context.Context, actor service.Actor, action domain.AuditAction,
		outcome domain.AuditOutcome, targetType domain.AuditTargetType,
		targetID, targetName, reason string)
	List(ctx context.Context, filter store.AuditFilter) ([]domain.AuditEvent, int, error)
	Summary(ctx context.Context) (domain.AuditSummary, error)
}

// -------------------------------------------------------------- bootstrap --

// handleBootstrapStatus reports whether this installation has been claimed.
//
// Public, and deliberately says almost nothing: one boolean, plus the fact that
// claiming needs a token. The SPA needs it to choose between the login page and
// the bootstrap page before it holds any credential.
func (s *Server) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"authentication is not configured on this server")
		return
	}

	status, err := s.auth.BootstrapStatus(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "bootstrap status failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, status)
}

// bootstrapRequestBody claims an installation.
type bootstrapRequestBody struct {
	// Token is the one-time value printed at startup.
	Token string `json:"token"`
	// Username and Password are the first administrator's.
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleBootstrap creates the first administrator.
//
// Reachable only while the installation is unclaimed -- the route policy makes
// it answer 404 afterwards -- and only with the one-time token.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var body bootstrapRequestBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	user, issued, err := s.auth.Bootstrap(r.Context(), service.BootstrapRequest{
		Token:      strings.TrimSpace(body.Token),
		Username:   body.Username,
		Password:   body.Password,
		RequestID:  RequestIDFrom(r.Context()),
		ClientAddr: s.clientAddr(r),
		UserAgent:  r.UserAgent(),
	})
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	// The cache flips here rather than waiting for the next request to
	// rediscover it, so the bootstrap route stops answering immediately.
	s.claimed.Store(true)

	s.setSessionCookie(w, r, issued)
	writeJSON(w, r, s.logger, http.StatusCreated, sessionResponse{
		User:      publicUser(user),
		CSRFToken: issued.CSRFToken,
		ExpiresAt: issued.ExpiresAt.UTC().Format(timeFormat),
	})
}

// ------------------------------------------------------------------ login --

// loginRequestBody is a credential presentation.
type loginRequestBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// sessionResponse is what the SPA needs to operate.
//
// The user, the CSRF token, and when the session ends. NOT the session token:
// that is in an HttpOnly cookie, and putting it here would hand it to any
// script on the page.
type sessionResponse struct {
	User      publicUserView `json:"user"`
	CSRFToken string         `json:"csrfToken"`
	ExpiresAt string         `json:"expiresAt"`
}

// handleLogin authenticates and issues a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"authentication is not configured on this server")
		return
	}
	// The write guard runs here rather than in the middleware, because the
	// login route is public and the middleware's write path is only reached for
	// authenticated routes. Same checks: fetch metadata, origin, rate limit.
	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	var body loginRequestBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	// Bounded before the service sees them. An unbounded password would be
	// hashed, and hashing is the expensive operation this endpoint offers to
	// anonymous callers.
	if len(body.Username) > domain.MaxUsernameBytes || len(body.Password) > domain.MaxPasswordBytes {
		// The SAME response as a wrong password. A distinct "too long" would
		// tell an attacker their input reached validation, which is a small but
		// free piece of information to withhold.
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"the username or password is incorrect")
		return
	}

	user, issued, err := s.auth.Login(r.Context(), service.LoginRequest{
		Username:   body.Username,
		Password:   body.Password,
		RequestID:  RequestIDFrom(r.Context()),
		ClientAddr: s.clientAddr(r),
		UserAgent:  r.UserAgent(),
	})
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	// SESSION FIXATION: the token is brand new, generated inside Login. There
	// is no path that adopts a token the client supplied, so a pre-set cookie
	// cannot survive authentication.
	s.setSessionCookie(w, r, issued)

	writeJSON(w, r, s.logger, http.StatusOK, sessionResponse{
		User:      publicUser(user),
		CSRFToken: issued.CSRFToken,
		ExpiresAt: issued.ExpiresAt.UTC().Format(timeFormat),
	})
}

// handleLogout ends the current session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		// Unreachable: the route policy requires a session. Handled anyway so
		// the handler is correct read on its own.
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")
		return
	}

	if err := s.auth.Logout(r.Context(), identity,
		RequestIDFrom(r.Context()), s.clientAddr(r)); err != nil {
		s.logger.ErrorContext(r.Context(), "logout failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleSession returns the current identity.
//
// The SPA's first call. It is how the app learns who it is talking to, what
// that identity may do, and the CSRF token every later write needs.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, sessionResponse{
		User: publicUser(identity.User),
		// Derived from the token in the cookie, so it is stable for the life of
		// the session and needs no storage.
		CSRFToken: s.auth.CSRFToken(identity.Token),
		ExpiresAt: identity.Session.AbsoluteExpiresAt.UTC().Format(timeFormat),
	})
}

// --------------------------------------------------------------- password --

// changePasswordBody is an operator changing their own password.
type changePasswordBody struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handleChangePassword replaces the caller's own credential.
//
// Reachable by an account that must change its password -- that is the whole
// point of the exemption -- and requires the CURRENT password even though the
// request is already authenticated. A session is possession; the current
// password is knowledge, and requiring both stops a stolen session becoming
// permanent account control.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")
		return
	}

	var body changePasswordBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	if len(body.CurrentPassword) > domain.MaxPasswordBytes ||
		len(body.NewPassword) > domain.MaxPasswordBytes {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the password is too long")
		return
	}

	issued, err := s.auth.ChangePassword(r.Context(), identity, service.ChangePasswordRequest{
		CurrentPassword: body.CurrentPassword,
		NewPassword:     body.NewPassword,
		RequestID:       RequestIDFrom(r.Context()),
		ClientAddr:      s.clientAddr(r),
	})
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	// The session ROTATES. Every session on the account was revoked, including
	// the one this request arrived on, and a fresh token replaces the cookie --
	// so a thief holding a session is out, and the operator is not.
	//
	// The CSRF token in the body changes with it, because it is derived from
	// the session token. A client that keeps using the old one will be refused,
	// which is why it is returned here rather than left to the next page load.
	s.setSessionCookie(w, r, issued)

	refreshed := identity.User
	refreshed.MustChangePassword = false

	writeJSON(w, r, s.logger, http.StatusOK, sessionResponse{
		User:      publicUser(refreshed),
		CSRFToken: issued.CSRFToken,
		ExpiresAt: issued.ExpiresAt.UTC().Format(timeFormat),
	})
}

// --------------------------------------------------------------- sessions --

// sessionListResponse is an operator's live sessions.
type sessionListResponse struct {
	Items []domain.Session `json:"items"`
}

// handleListSessions returns the caller's own sessions.
//
// Their OWN. There is no parameter naming another account: an administrator
// investigating somebody else's sessions does it through /users/{id}, where the
// permission check is explicit.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")
		return
	}

	sessions, err := s.auth.ListSessions(r.Context(),
		identity.User.UserID, identity.Session.SessionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "session list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, sessionListResponse{Items: sessions})
}

// handleRevokeSession ends one of the caller's own sessions.
//
// # The ownership check is the interesting part
//
// The session id is a caller-supplied identifier naming a row, which is the
// textbook shape of an insecure direct object reference. So the caller's own
// session list is read and the id matched against it: a session that is not
// theirs answers 404, not 403, because "that session exists but is not yours"
// is more than they are entitled to know.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")
		return
	}

	sessionID := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidSessionID(sessionID) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the session id is not well formed")
		return
	}

	sessions, err := s.auth.ListSessions(r.Context(),
		identity.User.UserID, identity.Session.SessionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "session lookup failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	owned := false
	for _, session := range sessions {
		if session.SessionID == sessionID {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "session not found")
		return
	}

	if err := s.auth.RevokeSession(r.Context(), sessionID, domain.SessionLoggedOut,
		identity, RequestIDFrom(r.Context()), s.clientAddr(r)); err != nil {
		s.logger.ErrorContext(r.Context(), "session revoke failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	// Revoking the CURRENT session is a logout, so the cookie goes too.
	if sessionID == identity.Session.SessionID {
		s.clearSessionCookie(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------- shared --

// publicUserView is the account as the API returns it.
//
// A projection rather than domain.User directly, so the permission list can be
// computed for the UI without putting a derived field on the domain type -- and
// so a future field added to domain.User does not silently join the API
// contract.
type publicUserView struct {
	UserID   string            `json:"userId"`
	Username string            `json:"username"`
	Role     domain.Role       `json:"role"`
	Status   domain.UserStatus `json:"status"`
	// Permissions is what this role may do, so the UI can hide controls it
	// would be refused anyway. HIDING IS NOT AUTHORIZATION -- the backend
	// refuses regardless -- it is only there to avoid offering an action that
	// cannot work.
	Permissions        []domain.Permission `json:"permissions"`
	MustChangePassword bool                `json:"mustChangePassword"`
	CreatedAt          string              `json:"createdAt"`
	LastLoginAt        string              `json:"lastLoginAt,omitempty"`
}

// timeFormat is the wire format for timestamps in this file.
const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

// publicUser projects an account for the API.
func publicUser(user domain.User) publicUserView {
	view := publicUserView{
		UserID:             user.UserID,
		Username:           user.Username,
		Role:               user.Role,
		Status:             user.Status,
		Permissions:        user.Role.Permissions(),
		MustChangePassword: user.MustChangePassword,
		CreatedAt:          user.CreatedAt.UTC().Format(timeFormat),
	}
	if user.LastLoginAt != nil {
		view.LastLoginAt = user.LastLoginAt.UTC().Format(timeFormat)
	}
	return view
}

// writeAuthError maps an authentication failure onto a response.
//
// # The three credential failures collapse into one
//
// ErrInvalidCredentials covers an unknown username, a wrong password, and a
// disabled account, and this function does not distinguish them either. That is
// the whole enumeration defence: a difference anywhere in the chain -- status
// code, error code, message, or timing -- undoes it.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidCredentials):
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"the username or password is incorrect")

	case errors.Is(err, service.ErrTooManyAttempts):
		// 429 with no Retry-After. The exact backoff is derived from how many
		// times THIS account has failed, and returning it would let an attacker
		// measure their own progress against a specific account.
		writeError(w, r, s.logger, http.StatusTooManyRequests, CodeConflict,
			"too many attempts; wait a little and try again")

	case errors.Is(err, service.ErrPasswordRejected):
		// The specific policy problem IS returned. It is about the password the
		// caller just chose, which they already know, and withholding it would
		// leave them guessing at a rule.
		var rejected service.PasswordRejectedError
		message := "that password is not acceptable"
		if errors.As(err, &rejected) {
			message = rejected.Problem.Explain()
		}
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, message)

	case errors.Is(err, service.ErrBootstrapClosed):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"this installation already has an administrator")

	case errors.Is(err, service.ErrBootstrapToken):
		// Deliberately vague and deliberately 401. Saying whether the token was
		// missing, expired, or wrong would let it be probed.
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"the bootstrap token is missing or not valid")

	case errors.Is(err, service.ErrInvalidUsername):
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the username must be 3 to 64 characters of lowercase letters, digits, dot, dash, or underscore")

	case errors.Is(err, store.ErrUsernameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"that username is already in use")

	case errors.Is(err, service.ErrNoSession):
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")

	default:
		s.logger.ErrorContext(r.Context(), "authentication request failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}
