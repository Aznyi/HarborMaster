package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The authentication tests run against a REAL migrated database and a real
// Argon2id hasher, at deliberately low cost parameters.
//
// A double would let a bug in the SQL -- the session-versus-password-change
// comparison, the last-administrator guard, the per-user cap -- pass every test
// here and fail in production. These are the properties most worth checking
// against the thing that actually enforces them.

// testArgon is the lowest cost the validator accepts.
//
// Correctness of the hashing is not what these tests measure; production cost
// is set in configuration and checked in TestArgonParameterBounds. Using 64 MiB
// here would make the suite take minutes.
func testArgon() service.ArgonParams {
	return service.ArgonParams{
		MemoryKiB:   service.MinArgonMemoryKiB,
		Iterations:  1,
		Parallelism: 1,
	}
}

// authHarness is a wired authentication stack over a real database.
type authHarness struct {
	db     *store.DB
	auth   *service.AuthService
	users  *service.UserService
	local  *service.LocalAdmin
	audit  *service.AuditRecorder
	hasher *service.PasswordHasher
	cfg    config.Auth

	mu  sync.Mutex
	now time.Time
}

// advance moves the harness clock forward.
func (h *authHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *authHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// newAuthHarness wires the stack.
func newAuthHarness(t *testing.T) *authHarness {
	t.Helper()

	db := openDB(t)

	hasher, err := service.NewPasswordHasher(testArgon())
	if err != nil {
		t.Fatalf("password hasher: %v", err)
	}

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		Value: strings.Repeat("ab", 32),
	})
	if err != nil {
		t.Fatalf("secret key: %v", err)
	}

	cfg := config.Auth{
		SessionIdleTTL:       time.Hour,
		SessionAbsoluteTTL:   24 * time.Hour,
		SessionTouchInterval: 5 * time.Minute,
		MaxSessionsPerUser:   3,
		SessionRetention:     24 * time.Hour,
		MaxLoginBackoff:      15 * time.Minute,
		MaxAddressFailures:   5,
		AddressFailureWindow: 10 * time.Minute,
		BootstrapTokenTTL:    time.Hour,
		AuditRetention:       180 * 24 * time.Hour,
		AuditSummaryWindow:   24 * time.Hour,
	}

	harness := &authHarness{
		db:     db,
		hasher: hasher,
		cfg:    cfg,
		now:    time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
	}
	clock := func() time.Time { return harness.clock() }

	harness.audit = service.NewAuditRecorder(db.Audit, cfg, nil, clock)
	harness.auth = service.NewAuthService(service.AuthOptions{
		Store:  service.NewAuthStore(db.Users, db.Sessions, db.Audit),
		Audit:  harness.audit,
		Key:    key,
		Hasher: hasher,
		Config: cfg,
		Now:    clock,
	})
	harness.users = service.NewUserService(
		service.NewUserAdminStore(db.Users, db.Sessions), harness.audit, hasher, nil, clock)
	harness.local = service.NewLocalAdmin(db.Users, db.Sessions, db.Audit, hasher, clock)

	return harness
}

// claim bootstraps the installation through the token flow and returns the
// administrator and its session.
func (h *authHarness) claim(t *testing.T, username, password string) (domain.User, service.IssuedSession) {
	t.Helper()

	token, _, err := h.auth.IssueBootstrapToken(context.Background())
	if err != nil {
		t.Fatalf("issue bootstrap token: %v", err)
	}

	user, issued, err := h.auth.Bootstrap(context.Background(), service.BootstrapRequest{
		Token:      token,
		Username:   username,
		Password:   password,
		ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return user, issued
}

// auditActions returns every recorded action, newest first.
func (h *authHarness) auditActions(t *testing.T) []domain.AuditAction {
	t.Helper()

	events, _, err := h.db.Audit.List(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	actions := make([]domain.AuditAction, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action)
	}
	return actions
}

// hasAction reports whether an action was recorded.
func hasAction(actions []domain.AuditAction, want domain.AuditAction) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- bootstrap --

func TestBootstrapRequiresTheOneTimeToken(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	// No token issued yet.
	if _, _, err := harness.auth.Bootstrap(ctx, service.BootstrapRequest{
		Token: "anything", Username: "admin", Password: "a decent passphrase",
	}); !errors.Is(err, service.ErrBootstrapToken) {
		t.Errorf("bootstrap without an issued token = %v, want ErrBootstrapToken", err)
	}

	token, _, err := harness.auth.IssueBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// A wrong token, and the right one with a character changed.
	for _, wrong := range []string{"", "wrong", token[:len(token)-1] + "X"} {
		if _, _, err := harness.auth.Bootstrap(ctx, service.BootstrapRequest{
			Token: wrong, Username: "admin", Password: "a decent passphrase",
		}); !errors.Is(err, service.ErrBootstrapToken) {
			t.Errorf("bootstrap with token %q = %v, want ErrBootstrapToken", wrong, err)
		}
	}

	if _, _, err := harness.auth.Bootstrap(ctx, service.BootstrapRequest{
		Token: token, Username: "admin", Password: "a decent passphrase",
	}); err != nil {
		t.Fatalf("bootstrap with the correct token: %v", err)
	}

	actions := harness.auditActions(t)
	if !hasAction(actions, domain.AuditBootstrapRejected) {
		t.Error("the rejected bootstrap attempts were not audited")
	}
	if !hasAction(actions, domain.AuditBootstrapCompleted) {
		t.Error("the successful bootstrap was not audited")
	}
}

func TestAReissuedBootstrapTokenInvalidatesThePrevious(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	first, _, err := harness.auth.IssueBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("issue first token: %v", err)
	}
	second, _, err := harness.auth.IssueBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("issue second token: %v", err)
	}
	if first == second {
		t.Fatal("two issuances produced the same token")
	}

	if _, _, err := harness.auth.Bootstrap(ctx, service.BootstrapRequest{
		Token: first, Username: "admin", Password: "a decent passphrase",
	}); !errors.Is(err, service.ErrBootstrapToken) {
		t.Errorf("the superseded token was accepted (%v)\n"+
			"\tan operator who lost a token restarts; a captured old log must not help", err)
	}
}

func TestAnExpiredBootstrapTokenIsRefused(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	token, expiresAt, err := harness.auth.IssueBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	harness.advance(expiresAt.Sub(harness.clock()) + time.Second)

	if _, _, err := harness.auth.Bootstrap(ctx, service.BootstrapRequest{
		Token: token, Username: "admin", Password: "a decent passphrase",
	}); !errors.Is(err, service.ErrBootstrapToken) {
		t.Errorf("an expired token was accepted (%v)", err)
	}
}

func TestBootstrapClosesPermanently(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	if _, _, err := harness.auth.IssueBootstrapToken(ctx); !errors.Is(err, service.ErrBootstrapClosed) {
		t.Errorf("a token was issued for a claimed installation (%v)", err)
	}

	status, err := harness.auth.BootstrapStatus(ctx)
	if err != nil {
		t.Fatalf("bootstrap status: %v", err)
	}
	if !status.Completed {
		t.Error("a claimed installation does not report itself claimed")
	}

	// Even disabling every administrator must not re-open it.
	_, err = harness.users.SetStatus(ctx, service.Actor{}, "no-such-user", domain.UserDisabled)
	if err == nil {
		t.Error("disabling an unknown account succeeded")
	}
	status, err = harness.auth.BootstrapStatus(ctx)
	if err != nil {
		t.Fatalf("bootstrap status: %v", err)
	}
	if !status.Completed {
		t.Error("the installation stopped reporting itself claimed")
	}
}

// -------------------------------------------------------------------- login --

func TestLoginSucceedsAndIssuesADistinctSession(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	user, issued, err := harness.auth.Login(ctx, service.LoginRequest{
		Username:   "ADMIN", // normalisation is part of the contract
		Password:   "a decent passphrase",
		ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if user.Username != "admin" {
		t.Errorf("logged in as %q, want %q", user.Username, "admin")
	}
	if !domain.ValidSessionToken(issued.Token) {
		t.Errorf("the issued token %q is not a well-formed session token", issued.Token)
	}
	if issued.CSRFToken == "" {
		t.Error("no CSRF token was issued")
	}
	if issued.CSRFToken == issued.Token {
		t.Error("the CSRF token equals the session token; a header-readable value " +
			"must never be the cookie's value")
	}

	// A second login produces a different session and a different token.
	_, again, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if again.Token == issued.Token || again.Session.SessionID == issued.Session.SessionID {
		t.Error("two logins produced the same session")
	}
}

// TestEveryCredentialFailureLooksTheSame is the enumeration-resistance
// property.
//
// An unknown username, a wrong password, and a disabled account must all return
// the same error. An attacker with a username list must learn nothing from the
// responses about which entries are real.
func TestEveryCredentialFailureLooksTheSame(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	disabled, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "disabled", Role: domain.RoleViewer, Password: "another fine phrase",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := harness.users.SetStatus(ctx, service.Actor{},
		disabled.User.UserID, domain.UserDisabled); err != nil {
		t.Fatalf("disable account: %v", err)
	}

	cases := map[string]service.LoginRequest{
		"unknown username": {Username: "nobody", Password: "any password at all"},
		"wrong password":   {Username: "admin", Password: "not the passphrase"},
		"disabled account": {Username: "disabled", Password: "another fine phrase"},
	}
	for name, request := range cases {
		request.ClientAddr = "198.51.100." + string(rune('1'+len(name)%9))
		_, _, err := harness.auth.Login(ctx, request)
		if !errors.Is(err, service.ErrInvalidCredentials) {
			t.Errorf("%s = %v, want ErrInvalidCredentials\n"+
				"\tevery credential failure must be indistinguishable to the client",
				name, err)
		}
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "disabled") {
			t.Errorf("%s: the error text discloses the account state: %v", name, err)
		}
	}
}

func TestRepeatedFailuresBackOffAndSucceedAfterTheWait(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	// Enough failures to build a backoff, from a single address that stays
	// under the address throttle.
	for i := 0; i < 3; i++ {
		if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
			Username: "admin", Password: "wrong password here", ClientAddr: "192.0.2.10",
		}); !errors.Is(err, service.ErrInvalidCredentials) {
			t.Fatalf("failure %d = %v, want ErrInvalidCredentials", i, err)
		}
	}

	// The correct password now hits the backoff rather than succeeding.
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	}); !errors.Is(err, service.ErrTooManyAttempts) {
		t.Errorf("login during backoff = %v, want ErrTooManyAttempts", err)
	}

	// It is a BACKOFF, not a lockout: waiting it out restores service.
	harness.advance(harness.cfg.MaxLoginBackoff + time.Minute)
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	}); err != nil {
		t.Errorf("login after the backoff elapsed: %v\n"+
			"\ta hard lockout would let anyone who knows a username deny it service", err)
	}
}

func TestTheAddressThrottleIsPerAddress(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	// Exceed the address failure budget from one address, spread across
	// usernames so no single account backoff is what stops it.
	for i := 0; i < harness.cfg.MaxAddressFailures+1; i++ {
		_, _, _ = harness.auth.Login(ctx, service.LoginRequest{
			Username:   "nobody" + string(rune('a'+i)),
			Password:   "wrong password here",
			ClientAddr: "203.0.113.5",
		})
	}

	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "203.0.113.5",
	}); !errors.Is(err, service.ErrTooManyAttempts) {
		t.Errorf("login from a throttled address = %v, want ErrTooManyAttempts", err)
	}

	// Another address is unaffected: the throttle must not become a way to
	// lock out the whole installation.
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.77",
	}); err != nil {
		t.Errorf("login from an unrelated address: %v", err)
	}
}

// ----------------------------------------------------------------- sessions --

func TestAuthenticateRefusesEverythingButALiveSession(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	_, issued := harness.claim(t, "admin", "a decent passphrase")

	if _, err := harness.auth.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("authenticate a live session: %v", err)
	}

	for name, token := range map[string]string{
		"empty":          "",
		"wrong shape":    "short",
		"oversized":      strings.Repeat("a", 100_000),
		"right shape":    strings.Repeat("a", 43),
		"one char off":   issued.Token[:len(issued.Token)-1] + "X",
		"the CSRF token": issued.CSRFToken,
	} {
		if _, err := harness.auth.Authenticate(ctx, token); !errors.Is(err, service.ErrNoSession) {
			t.Errorf("authenticate(%s) = %v, want ErrNoSession", name, err)
		}
	}
}

func TestASessionExpiresIdleAndAbsolutely(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	_, issued := harness.claim(t, "admin", "a decent passphrase")

	harness.advance(harness.cfg.SessionIdleTTL + time.Minute)
	if _, err := harness.auth.Authenticate(ctx, issued.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("an idle-expired session still authenticates (%v)", err)
	}

	// A session kept warm still dies at the absolute ceiling.
	harness2 := newAuthHarness(t)
	_, warm := harness2.claim(t, "admin", "a decent passphrase")
	for elapsed := time.Duration(0); elapsed < harness2.cfg.SessionAbsoluteTTL; elapsed += 30 * time.Minute {
		harness2.advance(30 * time.Minute)
		if _, err := harness2.auth.Authenticate(ctx, warm.Token); err != nil {
			break
		}
	}
	harness2.advance(harness2.cfg.SessionAbsoluteTTL)
	if _, err := harness2.auth.Authenticate(ctx, warm.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("a session kept warm survived past the absolute ceiling (%v)\n"+
			"\tthe absolute TTL is what bounds a STOLEN session", err)
	}
}

func TestDisablingAnAccountEndsItsSessionsImmediately(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	created, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "operator", Role: domain.RoleOperator, Password: "a fine phrase for work",
	})
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	_, issued, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "operator", Password: "a fine phrase for work", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("operator login: %v", err)
	}
	if _, err := harness.auth.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("authenticate before disablement: %v", err)
	}

	if _, err := harness.users.SetStatus(ctx, service.Actor{},
		created.User.UserID, domain.UserDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, err := harness.auth.Authenticate(ctx, issued.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("a disabled account's session still authenticates (%v)\n"+
			"\tthe window between disablement and expiry is exactly the one that matters", err)
	}
}

// TestARoleChangeTakesEffectWithoutWaiting is the reason the middleware re-reads
// the user on every request.
func TestARoleChangeTakesEffectWithoutWaiting(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	created, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "operator", Role: domain.RoleOperator, Password: "a fine phrase for work",
	})
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	_, issued, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "operator", Password: "a fine phrase for work", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := harness.users.SetRole(ctx, service.Actor{},
		created.User.UserID, domain.RoleViewer); err != nil {
		t.Fatalf("demote: %v", err)
	}

	// The session was revoked by the role change. If a future design keeps it
	// alive, the identity it resolves to must carry the NEW role -- never the
	// one the session was issued under.
	identity, err := harness.auth.Authenticate(ctx, issued.Token)
	if err == nil && identity.User.Role != domain.RoleViewer {
		t.Errorf("after a demotion the session resolves to role %q, want %q",
			identity.User.Role, domain.RoleViewer)
	}
}

func TestTheSessionCapSupersedesTheOldest(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	tokens := make([]string, 0, harness.cfg.MaxSessionsPerUser+2)
	for i := 0; i < harness.cfg.MaxSessionsPerUser+2; i++ {
		_, issued, err := harness.auth.Login(ctx, service.LoginRequest{
			Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
		})
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		tokens = append(tokens, issued.Token)
	}

	live := 0
	for _, token := range tokens {
		if _, err := harness.auth.Authenticate(ctx, token); err == nil {
			live++
		}
	}
	if live != harness.cfg.MaxSessionsPerUser {
		t.Errorf("%d sessions are live, want the cap of %d", live, harness.cfg.MaxSessionsPerUser)
	}
	// The OLDEST are the ones that went.
	if _, err := harness.auth.Authenticate(ctx, tokens[0]); err == nil {
		t.Error("the oldest session survived the cap")
	}
}

func TestLogoutRevokesOnlyTheCallersSession(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	_, first, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	_, second, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	identity, err := harness.auth.Authenticate(ctx, first.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := harness.auth.Logout(ctx, identity, "req-1", "192.0.2.10"); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if _, err := harness.auth.Authenticate(ctx, first.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("the logged-out session still authenticates (%v)", err)
	}
	if _, err := harness.auth.Authenticate(ctx, second.Token); err != nil {
		t.Errorf("logout ended a session it was not asked to end: %v", err)
	}
}

// --------------------------------------------------------------------- CSRF --

// TestTheCSRFTokenIsDerivedFromTheSessionToken is the whole design.
//
// Nothing is stored, so nothing can be stolen from the database; it is
// deterministic, so the server needs no state to check it; and it changes with
// the session, so it rotates on every login without a rotation mechanism.
func TestTheCSRFTokenIsDerivedFromTheSessionToken(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	_, issued := harness.claim(t, "admin", "a decent passphrase")

	if got := harness.auth.CSRFToken(issued.Token); got != issued.CSRFToken {
		t.Error("deriving the CSRF token twice gave two answers; the derivation " +
			"must be deterministic because nothing about it is stored")
	}
	if !harness.auth.ValidCSRF(issued.Token, issued.CSRFToken) {
		t.Error("the issued CSRF token does not validate against its own session")
	}

	// A token from another session must not validate.
	_, other, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if harness.auth.ValidCSRF(issued.Token, other.CSRFToken) {
		t.Error("another session's CSRF token validated")
	}

	for name, presented := range map[string]string{
		"empty":             "",
		"the session token": issued.Token,
		"one character off": issued.CSRFToken[:len(issued.CSRFToken)-1] + "0",
	} {
		if harness.auth.ValidCSRF(issued.Token, presented) {
			t.Errorf("ValidCSRF accepted %s", name)
		}
	}
	if harness.auth.ValidCSRF("", issued.CSRFToken) {
		t.Error("ValidCSRF accepted a token with no session")
	}
}

// ---------------------------------------------------------------- passwords --

func TestChangingAPasswordRequiresTheCurrentOne(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	_, issued := harness.claim(t, "admin", "a decent passphrase")

	identity, err := harness.auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	if _, err := harness.auth.ChangePassword(ctx, identity, service.ChangePasswordRequest{
		CurrentPassword: "not the current one",
		NewPassword:     "a brand new passphrase",
	}); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("change with a wrong current password = %v, want ErrInvalidCredentials\n"+
			"\ta stolen session must not become permanent account control", err)
	}

	if _, err := harness.auth.ChangePassword(ctx, identity, service.ChangePasswordRequest{
		CurrentPassword: "a decent passphrase",
		NewPassword:     "short",
	}); !errors.Is(err, service.ErrPasswordRejected) {
		t.Errorf("change to a weak password = %v, want ErrPasswordRejected", err)
	}

	if _, err := harness.auth.ChangePassword(ctx, identity, service.ChangePasswordRequest{
		CurrentPassword: "a decent passphrase",
		NewPassword:     "a brand new passphrase",
	}); err != nil {
		t.Fatalf("change password: %v", err)
	}

	// The old password no longer works and the new one does.
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.99",
	}); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("the old password still authenticates (%v)", err)
	}
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a brand new passphrase", ClientAddr: "192.0.2.99",
	}); err != nil {
		t.Errorf("the new password does not authenticate: %v", err)
	}
}

func TestAPasswordChangeEndsEveryOtherSession(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	_, mine, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_, thiefs, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a decent passphrase", ClientAddr: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	identity, err := harness.auth.Authenticate(ctx, mine.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	harness.advance(time.Minute)
	rotated, err := harness.auth.ChangePassword(ctx, identity, service.ChangePasswordRequest{
		CurrentPassword: "a decent passphrase",
		NewPassword:     "a brand new passphrase",
	})
	if err != nil {
		t.Fatalf("change password: %v", err)
	}

	if _, err := harness.auth.Authenticate(ctx, thiefs.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("another session survived a password change (%v)\n"+
			"\tincluding one a thief holds, which is the point", err)
	}
	if _, err := harness.auth.Authenticate(ctx, mine.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("the session the change was made from is still usable (%v)\n"+
			"\tit ROTATES: every session goes and a new one replaces it", err)
	}

	// The operator is not signed out -- the replacement session works.
	if rotated.Token == mine.Token {
		t.Error("the rotated session reuses the old token")
	}
	if _, err := harness.auth.Authenticate(ctx, rotated.Token); err != nil {
		t.Errorf("the replacement session does not authenticate: %v", err)
	}
	if rotated.CSRFToken == rotated.Token {
		t.Error("the rotated CSRF token equals the session token")
	}
}

func TestArgonParameterBoundsAndRehash(t *testing.T) {
	// Out-of-range parameters are refused rather than clamped: a login that
	// allocated a gigabyte would be a denial of service triggered from an
	// unauthenticated endpoint.
	for name, params := range map[string]service.ArgonParams{
		"no memory":       {MemoryKiB: 0, Iterations: 3, Parallelism: 4},
		"absurd memory":   {MemoryKiB: 1 << 30, Iterations: 3, Parallelism: 4},
		"no iterations":   {MemoryKiB: 65536, Iterations: 0, Parallelism: 4},
		"many iterations": {MemoryKiB: 65536, Iterations: 1000, Parallelism: 4},
		"no parallelism":  {MemoryKiB: 65536, Iterations: 3, Parallelism: 0},
	} {
		if _, err := service.NewPasswordHasher(params); err == nil {
			t.Errorf("NewPasswordHasher accepted %s", name)
		}
	}

	weak, err := service.NewPasswordHasher(testArgon())
	if err != nil {
		t.Fatalf("weak hasher: %v", err)
	}
	prepared, err := weak.Hash("a decent passphrase")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	stored := store.Credential{
		Algorithm: prepared.Algorithm, MemoryKiB: prepared.MemoryKiB,
		Iterations: prepared.Iterations, Parallelism: prepared.Parallelism,
		Salt: prepared.Salt, Hash: prepared.Hash,
	}
	if err := weak.Verify(stored, "a decent passphrase"); err != nil {
		t.Errorf("a credential does not verify against its own hasher: %v", err)
	}
	if err := weak.Verify(stored, "a different passphrase"); err == nil {
		t.Error("a wrong password verified")
	}
	if weak.NeedsRehash(stored) {
		t.Error("a credential produced at current policy was marked for re-hash")
	}

	stronger, err := service.NewPasswordHasher(service.ArgonParams{
		MemoryKiB: service.MinArgonMemoryKiB * 2, Iterations: 2, Parallelism: 1,
	})
	if err != nil {
		t.Fatalf("stronger hasher: %v", err)
	}
	if !stronger.NeedsRehash(stored) {
		t.Error("a credential below the current policy was not marked for re-hash")
	}
	// It still VERIFIES, using its own stored parameters. Raising the cost
	// must not invalidate every existing password.
	if err := stronger.Verify(stored, "a decent passphrase"); err != nil {
		t.Errorf("raising the cost invalidated an existing credential: %v", err)
	}
}

func TestAnUnusableCredentialNeverVerifies(t *testing.T) {
	hasher, err := service.NewPasswordHasher(testArgon())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	good, err := hasher.Hash("a decent passphrase")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	for name, credential := range map[string]store.Credential{
		"empty": {},
		"unknown algorithm": {Algorithm: "bcrypt", MemoryKiB: good.MemoryKiB,
			Iterations: good.Iterations, Parallelism: good.Parallelism,
			Salt: good.Salt, Hash: good.Hash},
		"absurd memory": {Algorithm: good.Algorithm, MemoryKiB: 1 << 30,
			Iterations: good.Iterations, Parallelism: good.Parallelism,
			Salt: good.Salt, Hash: good.Hash},
		"corrupt salt": {Algorithm: good.Algorithm, MemoryKiB: good.MemoryKiB,
			Iterations: good.Iterations, Parallelism: good.Parallelism,
			Salt: "not base64!!", Hash: good.Hash},
		"empty hash": {Algorithm: good.Algorithm, MemoryKiB: good.MemoryKiB,
			Iterations: good.Iterations, Parallelism: good.Parallelism,
			Salt: good.Salt, Hash: ""},
	} {
		if err := hasher.Verify(credential, "a decent passphrase"); err == nil {
			t.Errorf("Verify accepted %s\n"+
				"\ta corrupt row must never be made to accept a chosen password", name)
		}
	}
}

func TestTwoAccountsWithTheSamePasswordGetDifferentVerifiers(t *testing.T) {
	hasher, err := service.NewPasswordHasher(testArgon())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}

	first, err := hasher.Hash("the very same passphrase")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := hasher.Hash("the very same passphrase")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if first.Salt == second.Salt {
		t.Error("two credentials share a salt; a per-credential salt is what stops " +
			"one precomputation applying to every account")
	}
	if first.Hash == second.Hash {
		t.Error("two credentials over the same password produced the same hash")
	}
}

// ------------------------------------------------------- user administration --

func TestCreatingAnAccountReturnsItsTemporaryPasswordExactlyOnce(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	created, err := harness.users.Create(ctx, service.Actor{Username: "admin"},
		service.CreateUserRequest{Username: "viewer", Role: domain.RoleViewer})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.TemporaryPassword == "" {
		t.Fatal("no temporary password was generated for an account created without one")
	}
	if !created.User.MustChangePassword {
		t.Error("an account whose password was chosen by somebody else does not " +
			"require a change at next login")
	}

	// It logs in, and is not retrievable afterwards.
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "viewer", Password: created.TemporaryPassword, ClientAddr: "192.0.2.10",
	}); err != nil {
		t.Fatalf("login with the temporary password: %v", err)
	}

	fetched, err := harness.users.Get(ctx, created.User.UserID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	encoded, err := json.Marshal(fetched)
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	if strings.Contains(string(encoded), created.TemporaryPassword) {
		t.Error("the account's JSON contains its temporary password")
	}

	// The audit row names the account and its role, never the password.
	events, _, err := harness.db.Audit.List(ctx, store.AuditFilter{
		Actions: []domain.AuditAction{domain.AuditUserCreated},
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d user-created events, want 1", len(events))
	}
	if strings.Contains(events[0].Reason, created.TemporaryPassword) {
		t.Error("the audit reason contains the temporary password")
	}
}

func TestResettingAPasswordEndsEverySession(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	created, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "operator", Role: domain.RoleOperator, Password: "a fine phrase for work",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, issued, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "operator", Password: "a fine phrase for work", ClientAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	harness.advance(time.Minute)
	temporary, err := harness.users.ResetPassword(ctx,
		service.Actor{Username: "admin"}, created.User.UserID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if temporary == "" {
		t.Fatal("the reset produced no password")
	}

	if _, err := harness.auth.Authenticate(ctx, issued.Token); !errors.Is(err, service.ErrNoSession) {
		t.Errorf("a session survived a password reset (%v)\n"+
			"\ta reset is what an administrator does when they think an account is "+
			"compromised", err)
	}
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "operator", Password: temporary, ClientAddr: "192.0.2.10",
	}); err != nil {
		t.Errorf("the reset password does not authenticate: %v", err)
	}
}

// ---------------------------------------------------------- console recovery --

func TestConsoleClaimNeedsNoTokenButRunsOnlyOnce(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	user, err := harness.local.Claim(ctx, "admin", "a decent passphrase", false)
	if err != nil {
		t.Fatalf("console claim: %v", err)
	}
	if user.Role != domain.RoleAdministrator {
		t.Errorf("the console claim produced role %q, want administrator", user.Role)
	}

	if _, err := harness.local.Claim(ctx, "second", "another fine phrase", false); !errors.Is(err, service.ErrBootstrapClosed) {
		t.Errorf("a second console claim = %v, want ErrBootstrapClosed\n"+
			"\tallowing it would make the bootstrap path a permanent backdoor", err)
	}

	// A GENERATED password is temporary. It has been printed to a terminal and
	// is a shared secret until its holder replaces it.
	fresh := newAuthHarness(t)
	generated, err := fresh.local.Claim(ctx, "admin", "a printed passphrase", true)
	if err != nil {
		t.Fatalf("console claim with a generated password: %v", err)
	}
	if !generated.MustChangePassword {
		t.Error("a generated bootstrap password was not marked temporary")
	}
	if user.MustChangePassword {
		t.Error("an operator-chosen bootstrap password was marked temporary")
	}

	// The claim is audited as coming from the console, not from a user.
	events, _, err := harness.db.Audit.List(ctx, store.AuditFilter{
		Actions: []domain.AuditAction{domain.AuditBootstrapCompleted},
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d bootstrap events, want 1", len(events))
	}
	if events[0].ActorUsername != service.LocalAdminActor {
		t.Errorf("the console claim was attributed to %q, want %q",
			events[0].ActorUsername, service.LocalAdminActor)
	}
}

func TestConsoleResetReactivatesRevokesAndForcesAChange(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	created, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "operator", Role: domain.RoleOperator, Password: "a fine phrase for work",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "operator", Password: "a fine phrase for work", ClientAddr: "192.0.2.10",
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := harness.users.SetStatus(ctx, service.Actor{},
		created.User.UserID, domain.UserDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}

	harness.advance(time.Minute)
	outcome, err := harness.local.ResetPassword(ctx, "operator", "a recovered passphrase", true)
	if err != nil {
		t.Fatalf("console reset: %v", err)
	}
	if !outcome.Reactivated {
		t.Error("a disabled account was not reactivated; a reset that leaves the " +
			"operator locked out has recovered nothing")
	}
	if outcome.User.Role != domain.RoleOperator {
		t.Errorf("the console reset changed the role to %q; a password reset is not "+
			"a privilege grant", outcome.User.Role)
	}
	if !outcome.User.MustChangePassword {
		t.Error("a console-set password is not marked temporary")
	}

	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "operator", Password: "a recovered passphrase", ClientAddr: "192.0.2.10",
	}); err != nil {
		t.Errorf("the recovered password does not authenticate: %v", err)
	}
}

func TestConsoleResetRefusesAnUnknownAccountAndAWeakPassword(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	harness.claim(t, "admin", "a decent passphrase")

	if _, err := harness.local.ResetPassword(ctx, "nobody", "a decent passphrase", true); !errors.Is(err, service.ErrUserNotFound) {
		t.Errorf("reset of an unknown account = %v, want ErrUserNotFound", err)
	}
	if _, err := harness.local.ResetPassword(ctx, "admin", "short", true); !errors.Is(err, service.ErrPasswordRejected) {
		t.Errorf("reset to a weak password = %v, want ErrPasswordRejected", err)
	}
}

// -------------------------------------------------------------------- audit --

// TestNoAuditRowEverContainsASecret is the leak sweep over the whole log.
//
// Every path above ran; this walks what they recorded and looks for any of the
// secrets that were in scope. An audit log that captured a password would be a
// worse liability than no log at all.
func TestNoAuditRowEverContainsASecret(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	const (
		adminPassword = "a stout secret phrase"
		newPassword   = "a fresh secret phrase"
	)

	bootstrapToken, _, err := harness.auth.IssueBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	_, issued, err := harness.auth.Bootstrap(ctx, service.BootstrapRequest{
		Token: bootstrapToken, Username: "admin", Password: adminPassword,
		ClientAddr: "192.0.2.10", UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, _, _ = harness.auth.Login(ctx, service.LoginRequest{
		Username: "admin", Password: "a wrong password entirely", ClientAddr: "192.0.2.10",
	})
	created, err := harness.users.Create(ctx, service.Actor{Username: "admin"},
		service.CreateUserRequest{Username: "operator", Role: domain.RoleOperator})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	identity, err := harness.auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	harness.advance(time.Minute)
	if _, err := harness.auth.ChangePassword(ctx, identity, service.ChangePasswordRequest{
		CurrentPassword: adminPassword, NewPassword: newPassword,
	}); err != nil {
		t.Fatalf("change password: %v", err)
	}

	secrets := map[string]string{
		"the administrator password": adminPassword,
		"the replacement password":   newPassword,
		"the wrong password":         "a wrong password entirely",
		"the bootstrap token":        bootstrapToken,
		"the session token":          issued.Token,
		"the CSRF token":             issued.CSRFToken,
		"the temporary password":     created.TemporaryPassword,
	}

	events, _, err := harness.db.Audit.List(ctx, store.AuditFilter{Page: store.Page{Limit: 200}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no audit events were recorded; the sweep would pass vacuously")
	}

	for _, event := range events {
		haystack := strings.Join([]string{
			string(event.Action), string(event.Outcome), event.ActorUserID,
			event.ActorUsername, string(event.ActorRole), event.ActorSessionID,
			string(event.TargetType), event.TargetID, event.TargetName,
			event.RequestID, event.ClientAddr, event.Reason,
		}, "\x00")

		for name, secret := range secrets {
			if secret == "" {
				continue
			}
			if strings.Contains(haystack, secret) {
				t.Errorf("audit event %q contains %s", event.Action, name)
			}
		}
	}
}

// TestTheAuditLogAttributesEveryActionToItsActor is the point of the phase.
func TestTheAuditLogAttributesEveryActionToItsActor(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()
	admin, issued := harness.claim(t, "admin", "a decent passphrase")

	identity, err := harness.auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	actor := service.Actor{
		UserID: admin.UserID, Username: admin.Username, Role: admin.Role,
		SessionID: identity.Session.SessionID,
		RequestID: "req-42", ClientAddr: "192.0.2.10",
	}

	if _, err := harness.users.Create(ctx, actor, service.CreateUserRequest{
		Username: "operator", Role: domain.RoleOperator,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	events, _, err := harness.db.Audit.List(ctx, store.AuditFilter{
		Actions: []domain.AuditAction{domain.AuditUserCreated},
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d user-created events, want 1", len(events))
	}

	event := events[0]
	if event.ActorUserID != admin.UserID || event.ActorUsername != "admin" {
		t.Errorf("the event is attributed to %q/%q, want %q/%q",
			event.ActorUserID, event.ActorUsername, admin.UserID, "admin")
	}
	if event.ActorRole != domain.RoleAdministrator {
		t.Errorf("actor role = %q, want administrator", event.ActorRole)
	}
	if event.ActorSessionID != identity.Session.SessionID {
		t.Error("the event does not name the session the action was taken from")
	}
	if event.RequestID != "req-42" {
		t.Errorf("request id = %q, want req-42", event.RequestID)
	}
	if event.ClientAddr != "192.0.2.10" {
		t.Errorf("client address = %q, want 192.0.2.10", event.ClientAddr)
	}
	if event.TargetName != "operator" {
		t.Errorf("target name = %q, want operator", event.TargetName)
	}
}

func TestSweepingSessionsIsBoundedAndSurvivesShutdown(t *testing.T) {
	harness := newAuthHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	harness.claim(t, "admin", "a decent passphrase")

	// A cancelled context must make Run return promptly rather than block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.auth.Run(ctx)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session sweeper did not stop when its context was cancelled")
	}
}
