package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Test support for the authenticated API.
//
// # Why every server in the tests is authenticated by default
//
// After Phase 9.5 there is no anonymous API. A test that built a bare Server
// would exercise the 401 path and nothing else, so newAuthedServer installs a
// stub identity by default and `authed` stamps the request with the session
// cookie and CSRF header the middleware requires.
//
// The stub is deliberately PERMISSIVE -- it grants whatever role it was given
// and accepts the fixed test token -- because these tests are about handler
// behaviour. The authorization behaviour itself is checked separately, in
// auth_api_test.go, against the same middleware with the stub configured to
// refuse.

const (
	// testSessionToken has the right SHAPE for a session token so the code
	// paths that validate it before use behave as they do in production.
	testSessionToken = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	testCSRFToken    = "test-csrf-token"
)

// stubAuth is an AuthService double.
type stubAuth struct {
	mu sync.Mutex

	// user is the identity every accepted token resolves to.
	user domain.User
	// claimed reports whether the installation has an administrator.
	claimed bool
	// rejectToken makes Authenticate refuse everything, for the 401 paths.
	rejectToken bool
	// acceptAnyCSRF relaxes the CSRF check, for tests that are not about it.
	acceptAnyCSRF bool

	// denials counts recorded authorization failures, so a test can assert
	// that a refusal was audited rather than merely returned.
	loginCalls int
}

// newStubAuth builds a stub granting the given role.
func newStubAuth(role domain.Role) *stubAuth {
	return &stubAuth{
		user: domain.User{
			UserID:   "usr_test0000000000000000",
			Username: "tester",
			Role:     role,
			Status:   domain.UserActive,
		},
		claimed:       true,
		acceptAnyCSRF: true,
	}
}

func (s *stubAuth) identity() service.Authenticated {
	return service.Authenticated{
		User: s.user,
		Session: domain.Session{
			SessionID: "ses_test0000000000000000",
			UserID:    s.user.UserID,
			Username:  s.user.Username,
			Role:      s.user.Role,
		},
		Token: testSessionToken,
	}
}

func (s *stubAuth) BootstrapStatus(context.Context) (service.BootstrapStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return service.BootstrapStatus{Completed: s.claimed, TokenRequired: true}, nil
}

func (s *stubAuth) Bootstrap(
	_ context.Context,
	request service.BootstrapRequest,
) (domain.User, service.IssuedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		return domain.User{}, service.IssuedSession{}, service.ErrBootstrapClosed
	}
	if request.Token != "the-bootstrap-token" {
		return domain.User{}, service.IssuedSession{}, service.ErrBootstrapToken
	}
	if problem := domain.CheckPassword(request.Password, request.Username); problem != domain.PasswordOK {
		return domain.User{}, service.IssuedSession{}, service.PasswordRejectedError{Problem: problem}
	}
	s.claimed = true
	s.user.Username = domain.NormaliseUsername(request.Username)
	s.user.Role = domain.RoleAdministrator
	return s.user, s.issued(), nil
}

func (s *stubAuth) issued() service.IssuedSession {
	identity := s.identity()
	return service.IssuedSession{
		Session:   identity.Session,
		Token:     testSessionToken,
		CSRFToken: testCSRFToken,
		ExpiresAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (s *stubAuth) Login(
	_ context.Context,
	request service.LoginRequest,
) (domain.User, service.IssuedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginCalls++
	if request.Password != "the correct passphrase" {
		return domain.User{}, service.IssuedSession{}, service.ErrInvalidCredentials
	}
	return s.user, s.issued(), nil
}

func (s *stubAuth) Logout(context.Context, service.Authenticated, string, string) error {
	return nil
}

func (s *stubAuth) Authenticate(_ context.Context, token string) (service.Authenticated, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectToken || token != testSessionToken {
		return service.Authenticated{}, service.ErrNoSession
	}
	return s.identity(), nil
}

func (s *stubAuth) ChangePassword(
	_ context.Context,
	_ service.Authenticated,
	request service.ChangePasswordRequest,
) (service.IssuedSession, error) {
	if request.CurrentPassword != "the correct passphrase" {
		return service.IssuedSession{}, service.ErrInvalidCredentials
	}
	if problem := domain.CheckPassword(request.NewPassword, "tester"); problem != domain.PasswordOK {
		return service.IssuedSession{}, service.PasswordRejectedError{Problem: problem}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued(), nil
}

func (s *stubAuth) ListSessions(context.Context, string, string) ([]domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []domain.Session{s.identity().Session}, nil
}

func (s *stubAuth) RevokeSession(
	_ context.Context,
	sessionID string,
	_ domain.SessionRevocation,
	_ service.Authenticated,
	_, _ string,
) error {
	if sessionID != "ses_test0000000000000000" {
		return store.ErrNotFound
	}
	return nil
}

func (s *stubAuth) CSRFToken(string) string { return testCSRFToken }

func (s *stubAuth) ValidCSRF(_, presented string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptAnyCSRF {
		return presented != ""
	}
	return presented == testCSRFToken
}

// stubUsers is a UserService double.
type stubUsers struct {
	mu      sync.Mutex
	created []service.CreateUserRequest
	roles   map[string]domain.Role
	users   []domain.User
	err     error
}

func newStubUsers() *stubUsers {
	return &stubUsers{
		roles: map[string]domain.Role{},
		users: []domain.User{{
			UserID: "usr_test0000000000000000", Username: "tester",
			Role: domain.RoleAdministrator, Status: domain.UserActive,
		}},
	}
}

func (s *stubUsers) Create(
	_ context.Context,
	_ service.Actor,
	request service.CreateUserRequest,
) (service.CreatedUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return service.CreatedUser{}, s.err
	}
	if !domain.ValidUsername(domain.NormaliseUsername(request.Username)) {
		return service.CreatedUser{}, service.ErrInvalidUsername
	}
	if !domain.ValidRole(string(request.Role)) {
		return service.CreatedUser{}, service.ErrInvalidRole
	}
	s.created = append(s.created, request)
	return service.CreatedUser{
		User: domain.User{
			UserID:   "usr_created00000000000000",
			Username: domain.NormaliseUsername(request.Username),
			Role:     request.Role,
			Status:   domain.UserActive,
		},
		TemporaryPassword: "a-generated-temporary-password",
	}, nil
}

func (s *stubUsers) Get(_ context.Context, userID string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range s.users {
		if user.UserID == userID {
			return user, nil
		}
	}
	return domain.User{}, store.ErrNotFound
}

func (s *stubUsers) List(_ context.Context, _ store.UserFilter) ([]domain.User, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users, len(s.users), nil
}

func (s *stubUsers) SetRole(
	_ context.Context,
	_ service.Actor,
	userID string,
	role domain.Role,
) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !domain.ValidRole(string(role)) {
		return domain.User{}, service.ErrInvalidRole
	}
	s.roles[userID] = role
	return domain.User{UserID: userID, Username: "other", Role: role, Status: domain.UserActive}, nil
}

func (s *stubUsers) SetStatus(
	_ context.Context,
	_ service.Actor,
	userID string,
	status domain.UserStatus,
) (domain.User, error) {
	if !domain.ValidUserStatus(string(status)) {
		return domain.User{}, service.ErrInvalidStatus
	}
	return domain.User{
		UserID: userID, Username: "other", Role: domain.RoleViewer, Status: status,
	}, nil
}

func (s *stubUsers) ResetPassword(_ context.Context, _ service.Actor, userID string) (string, error) {
	if userID == "usr_missing000000000000" {
		return "", store.ErrNotFound
	}
	return "a-generated-temporary-password", nil
}

// stubAudit is an AuditService double that keeps what it was told.
type stubAudit struct {
	mu     sync.Mutex
	events []domain.AuditEvent
}

func (s *stubAudit) Record(_ context.Context, event domain.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *stubAudit) RecordAction(
	ctx context.Context,
	actor service.Actor,
	action domain.AuditAction,
	outcome domain.AuditOutcome,
	targetType domain.AuditTargetType,
	targetID, targetName, reason string,
) {
	s.Record(ctx, domain.AuditEvent{
		Action: action, Outcome: outcome,
		ActorUserID: actor.UserID, ActorUsername: actor.Username, ActorRole: actor.Role,
		ActorSessionID: actor.SessionID,
		TargetType:     targetType, TargetID: targetID, TargetName: targetName,
		RequestID: actor.RequestID, ClientAddr: actor.ClientAddr, Reason: reason,
	})
}

func (s *stubAudit) List(context.Context, store.AuditFilter) ([]domain.AuditEvent, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events, len(s.events), nil
}

func (s *stubAudit) Summary(context.Context) (domain.AuditSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return domain.AuditSummary{Total: len(s.events)}, nil
}

// recorded returns the events whose action matches.
func (s *stubAudit) recorded(action domain.AuditAction) []domain.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	var found []domain.AuditEvent
	for _, event := range s.events {
		if event.Action == action {
			found = append(found, event)
		}
	}
	return found
}

// testAuthConfig is the auth configuration the tests run under.
func testAuthConfig() config.Auth {
	return config.Auth{
		SessionIdleTTL:       time.Hour,
		SessionAbsoluteTTL:   24 * time.Hour,
		SessionTouchInterval: 5 * time.Minute,
		MaxSessionsPerUser:   5,
		AuditSummaryWindow:   24 * time.Hour,
	}
}

// newAuthedServer builds a Server with an authenticated administrator unless
// the caller supplied their own auth stack.
//
// Every NewServer call in the API tests goes through here, so adding a route
// with a permission does not silently make an existing test exercise a 403
// instead of the handler it was written for.
func newAuthedServer(opts Options) *Server {
	if opts.Auth == nil {
		opts.Auth = newStubAuth(domain.RoleAdministrator)
	}
	if opts.Users == nil {
		opts.Users = newStubUsers()
	}
	if opts.Audit == nil {
		opts.Audit = &stubAudit{}
	}
	if opts.AuthConfig.SessionIdleTTL == 0 {
		opts.AuthConfig = testAuthConfig()
	}
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	// The write limiters now run in the MIDDLEWARE, so a server built with a
	// zero rate configuration refuses every write with 429 before any handler
	// sees it. Production always supplies these from config.Load, which fills
	// its own defaults; a test that has not opted into rate-limit behaviour
	// gets a permissive budget so it exercises the path it was written for.
	if opts.SnapshotConfig.WriteRateLimit == 0 {
		opts.SnapshotConfig.WriteRateLimit = 10_000
		opts.SnapshotConfig.WriteRateBurst = 10_000
	}
	if opts.PolicyConfig.WriteRateLimit == 0 {
		opts.PolicyConfig.WriteRateLimit = 10_000
		opts.PolicyConfig.WriteRateBurst = 10_000
	}
	return NewServer(opts)
}

// authed stamps a request with the test session cookie and CSRF header.
//
// The cookie always; the CSRF header only on a state-changing method, matching
// what a browser client does. A request that already carries either is left
// alone, so a test about the missing-token paths can build one by hand.
func authed(r *http.Request) *http.Request {
	if _, err := r.Cookie(SessionCookieName); err != nil {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})
	}
	if stateChanging(r.Method) && r.Header.Get(CSRFHeader) == "" {
		r.Header.Set(CSRFHeader, testCSRFToken)
	}
	return r
}

// anonymous strips the session cookie and CSRF header from a request.
//
// Used by the tests that assert the unauthenticated behaviour, so they read as
// deliberately anonymous rather than as having forgotten `authed`.
func anonymous(r *http.Request) *http.Request {
	r.Header.Del("Cookie")
	r.Header.Del(CSRFHeader)
	return r
}

// asRole builds an authenticated server whose identity holds one role.
func asRole(opts Options, role domain.Role) (*Server, *stubAuth, *stubAudit) {
	auth := newStubAuth(role)
	audit := &stubAudit{}
	opts.Auth = auth
	opts.Audit = audit
	return newAuthedServer(opts), auth, audit
}
