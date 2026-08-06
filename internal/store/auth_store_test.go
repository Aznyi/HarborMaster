package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// authClock is a fixed instant the auth tests advance by hand.
var authClock = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// testCredential builds a syntactically valid verifier.
//
// Not a real Argon2id hash: the repository stores opaque strings and the
// hashing belongs to internal/service. Using a recognisable placeholder makes a
// leak assertion below meaningful.
func testCredential(hash string) store.PreparedCredential {
	return store.PreparedCredential{
		Algorithm:   "argon2id",
		MemoryKiB:   65536,
		Iterations:  3,
		Parallelism: 4,
		Salt:        "c2FsdHNhbHRzYWx0",
		Hash:        hash,
	}
}

// claimInstallation creates the first administrator and returns it.
func claimInstallation(t *testing.T, db *store.DB, username string) domain.User {
	t.Helper()

	user, err := db.Users.CompleteBootstrap(context.Background(), store.NewUser{
		UserID:     domain.NewUserID(),
		Username:   username,
		Role:       domain.RoleAdministrator,
		Credential: testCredential("FIRST-ADMIN-HASH"),
	}, authClock)
	if err != nil {
		t.Fatalf("complete bootstrap: %v", err)
	}
	return user
}

// makeUser creates an additional account.
func makeUser(t *testing.T, db *store.DB, username string, role domain.Role) domain.User {
	t.Helper()

	user, err := db.Users.Create(context.Background(), store.NewUser{
		UserID:     domain.NewUserID(),
		Username:   username,
		Role:       role,
		Credential: testCredential("HASH-" + username),
	}, authClock)
	if err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	return user
}

// ---------------------------------------------------------------- bootstrap --

func TestAFreshInstallationIsUnclaimed(t *testing.T) {
	db := openTestDB(t)

	state, err := db.Users.BootstrapState(context.Background())
	if err != nil {
		t.Fatalf("bootstrap state: %v", err)
	}
	if state.Completed {
		t.Error("a fresh installation reports itself claimed")
	}
	if state.TokenDigest != "" {
		t.Error("a fresh installation already carries a bootstrap token digest")
	}
	if state.TokenUsable(authClock) {
		t.Error("a bootstrap token is usable before one was issued")
	}
}

func TestABootstrapTokenExpires(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	expiry := authClock.Add(time.Hour)
	if err := db.Users.SetBootstrapToken(ctx, "DIGEST", expiry, authClock); err != nil {
		t.Fatalf("set bootstrap token: %v", err)
	}

	state, err := db.Users.BootstrapState(ctx)
	if err != nil {
		t.Fatalf("bootstrap state: %v", err)
	}
	if !state.TokenUsable(authClock) {
		t.Error("a freshly issued token is not usable")
	}
	if state.TokenUsable(expiry.Add(time.Second)) {
		t.Error("a token is still usable after its expiry")
	}
}

// TestBootstrapHappensExactlyOnceUnderConcurrency is the race the conditional
// UPDATE exists to decide.
//
// Two callers claiming simultaneously must produce ONE administrator. A losing
// transaction must roll back its user insert too, or the installation ends up
// with a second account that nobody created deliberately.
func TestBootstrapHappensExactlyOnceUnderConcurrency(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const attempts = 8
	var (
		wait      sync.WaitGroup
		mutex     sync.Mutex
		succeeded int
		errs      []error
	)

	wait.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(index int) {
			defer wait.Done()
			_, err := db.Users.CompleteBootstrap(ctx, store.NewUser{
				UserID:     domain.NewUserID(),
				Username:   "admin" + string(rune('a'+index)),
				Role:       domain.RoleAdministrator,
				Credential: testCredential("HASH"),
			}, authClock)

			mutex.Lock()
			defer mutex.Unlock()
			if err == nil {
				succeeded++
				return
			}
			errs = append(errs, err)
		}(i)
	}
	wait.Wait()

	if succeeded != 1 {
		t.Errorf("%d of %d concurrent bootstraps succeeded, want exactly 1 (errors: %v)",
			succeeded, attempts, errs)
	}

	users, total, err := db.Users.List(ctx, store.UserFilter{})
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if total != 1 || len(users) != 1 {
		t.Errorf("the installation holds %d accounts after a contended bootstrap, want 1", total)
	}
}

func TestBootstrapIsRefusedOnceClaimed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	claimInstallation(t, db, "admin")

	_, err := db.Users.CompleteBootstrap(ctx, store.NewUser{
		UserID:     domain.NewUserID(),
		Username:   "second",
		Role:       domain.RoleAdministrator,
		Credential: testCredential("HASH"),
	}, authClock)
	if !errors.Is(err, store.ErrAlreadyBootstrapped) {
		t.Errorf("second bootstrap error = %v, want ErrAlreadyBootstrapped", err)
	}

	if err := db.Users.SetBootstrapToken(ctx, "NEW", authClock.Add(time.Hour), authClock); !errors.Is(err, store.ErrAlreadyBootstrapped) {
		t.Errorf("minting a token on a claimed installation = %v, want ErrAlreadyBootstrapped\n"+
			"\tre-opening the bootstrap flow would be a permanent backdoor", err)
	}
}

// -------------------------------------------------------------------- users --

func TestUsernamesAreUniqueAndValidated(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	claimInstallation(t, db, "admin")

	makeUser(t, db, "operator", domain.RoleOperator)

	_, err := db.Users.Create(ctx, store.NewUser{
		UserID:     domain.NewUserID(),
		Username:   "operator",
		Role:       domain.RoleViewer,
		Credential: testCredential("HASH"),
	}, authClock)
	if !errors.Is(err, store.ErrUsernameTaken) {
		t.Errorf("duplicate username error = %v, want ErrUsernameTaken", err)
	}

	for _, bad := range []store.NewUser{
		{UserID: domain.NewUserID(), Username: "Bad Name", Role: domain.RoleViewer,
			Credential: testCredential("HASH")},
		{UserID: "", Username: "goodname", Role: domain.RoleViewer,
			Credential: testCredential("HASH")},
		{UserID: domain.NewUserID(), Username: "goodname", Role: "root",
			Credential: testCredential("HASH")},
		// No credential at all: an account that cannot be logged into but
		// occupies a username, and whose verifier lookup would have to invent
		// something.
		{UserID: domain.NewUserID(), Username: "goodname", Role: domain.RoleViewer},
	} {
		if _, err := db.Users.Create(ctx, bad, authClock); err == nil {
			t.Errorf("Create accepted an invalid account: %+v", bad.Username)
		}
	}
}

// TestTheLastAdministratorCannotBeRemoved is the lockout guard.
//
// Both paths -- demotion and disablement -- have to be blocked, and the check
// has to happen inside the transaction that performs the change, or two
// concurrent demotions each see the other still present.
func TestTheLastAdministratorCannotBeRemoved(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")

	if err := db.Users.SetRole(ctx, admin.UserID, domain.RoleViewer, authClock); !errors.Is(err, store.ErrLastAdministrator) {
		t.Errorf("demoting the last administrator = %v, want ErrLastAdministrator", err)
	}
	if err := db.Users.SetStatus(ctx, admin.UserID, domain.UserDisabled, authClock); !errors.Is(err, store.ErrLastAdministrator) {
		t.Errorf("disabling the last administrator = %v, want ErrLastAdministrator", err)
	}

	// With a second administrator the first may be demoted, and then the
	// second becomes the one that cannot be.
	second := makeUser(t, db, "admin2", domain.RoleAdministrator)
	if err := db.Users.SetRole(ctx, admin.UserID, domain.RoleViewer, authClock); err != nil {
		t.Fatalf("demoting an administrator with a peer: %v", err)
	}
	if err := db.Users.SetRole(ctx, second.UserID, domain.RoleOperator, authClock); !errors.Is(err, store.ErrLastAdministrator) {
		t.Errorf("demoting the now-only administrator = %v, want ErrLastAdministrator", err)
	}
}

func TestTheLastAdministratorGuardIsAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	first := claimInstallation(t, db, "admin")
	second := makeUser(t, db, "admin2", domain.RoleAdministrator)

	var wait sync.WaitGroup
	wait.Add(2)
	for _, id := range []string{first.UserID, second.UserID} {
		go func(userID string) {
			defer wait.Done()
			_ = db.Users.SetRole(ctx, userID, domain.RoleViewer, authClock)
		}(id)
	}
	wait.Wait()

	remaining, err := db.Users.CountActiveAdministrators(ctx)
	if err != nil {
		t.Fatalf("count administrators: %v", err)
	}
	if remaining < 1 {
		t.Error("two concurrent demotions removed every administrator; the guard " +
			"must run inside the transaction that performs the change")
	}
}

func TestCredentialLookupFailsClosedWithoutOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	claimInstallation(t, db, "admin")

	_, err := db.Users.CredentialFor(ctx, "user-that-does-not-exist")
	if !errors.Is(err, store.ErrNoCredential) {
		t.Errorf("CredentialFor(unknown) = %v, want ErrNoCredential\n"+
			"\ta zero verifier would be a hash that something could match", err)
	}
}

func TestFailureBackoffAccumulatesAndClearsOnLogin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")

	backoff := func(attempts int) time.Duration {
		return time.Duration(attempts) * time.Minute
	}

	for attempt := 1; attempt <= 3; attempt++ {
		unlockAt, err := db.Users.RecordFailure(ctx, admin.UserID, authClock, backoff)
		if err != nil {
			t.Fatalf("record failure %d: %v", attempt, err)
		}
		want := authClock.Add(time.Duration(attempt) * time.Minute)
		if !unlockAt.Equal(want) {
			t.Errorf("failure %d unlocks at %s, want %s", attempt, unlockAt, want)
		}
	}

	credential, err := db.Users.CredentialFor(ctx, admin.UserID)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if credential.FailedAttempts != 3 {
		t.Errorf("failed attempts = %d, want 3", credential.FailedAttempts)
	}
	if credential.LockedUntil == nil {
		t.Error("a failed attempt did not set a lock time")
	}

	if err := db.Users.RecordLogin(ctx, admin.UserID, authClock.Add(time.Hour)); err != nil {
		t.Fatalf("record login: %v", err)
	}
	cleared, err := db.Users.CredentialFor(ctx, admin.UserID)
	if err != nil {
		t.Fatalf("credential after login: %v", err)
	}
	if cleared.FailedAttempts != 0 || cleared.LockedUntil != nil {
		t.Errorf("a successful login left failures=%d lockedUntil=%v; both must clear",
			cleared.FailedAttempts, cleared.LockedUntil)
	}
}

// ----------------------------------------------------------------- sessions --

// newSession issues a session for a user.
func newSession(t *testing.T, db *store.DB, user domain.User, digest string, now time.Time) domain.Session {
	t.Helper()

	session, err := db.Sessions.Create(context.Background(), store.NewSession{
		SessionID:         domain.NewSessionID(),
		UserID:            user.UserID,
		Username:          user.Username,
		Role:              user.Role,
		TokenDigest:       digest,
		IdleExpiresAt:     now.Add(time.Hour),
		AbsoluteExpiresAt: now.Add(24 * time.Hour),
		UserAgent:         "test-agent",
		ClientAddr:        "192.0.2.10",
	}, 10, now)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func TestASessionIsFoundOnlyByItsDigest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")
	session := newSession(t, db, admin, "DIGEST-A", authClock)

	found, err := db.Sessions.ByTokenDigest(ctx, "DIGEST-A", authClock)
	if err != nil {
		t.Fatalf("lookup by digest: %v", err)
	}
	if found.SessionID != session.SessionID || found.UserID != admin.UserID {
		t.Errorf("lookup returned session %q/user %q, want %q/%q",
			found.SessionID, found.UserID, session.SessionID, admin.UserID)
	}

	if _, err := db.Sessions.ByTokenDigest(ctx, "DIGEST-B", authClock); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("lookup of an unknown digest = %v, want ErrSessionNotFound", err)
	}
}

func TestExpiryAndRevocationAreIndistinguishableToACaller(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")

	newSession(t, db, admin, "IDLE", authClock)
	newSession(t, db, admin, "ABSOLUTE", authClock)
	revoked := newSession(t, db, admin, "REVOKED", authClock)

	if err := db.Sessions.Revoke(ctx, revoked.SessionID, domain.SessionLoggedOut, authClock); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	cases := map[string]time.Time{
		"IDLE":     authClock.Add(2 * time.Hour),  // past the idle expiry
		"ABSOLUTE": authClock.Add(48 * time.Hour), // past the absolute expiry
		"REVOKED":  authClock,
	}
	for digest, when := range cases {
		if _, err := db.Sessions.ByTokenDigest(ctx, digest, when); !errors.Is(err, store.ErrSessionNotFound) {
			t.Errorf("lookup of %s = %v, want ErrSessionNotFound\n"+
				"\tthe three reasons must be indistinguishable to a caller", digest, err)
		}
	}
}

// TestAPasswordChangeInvalidatesOlderSessions is the belt to the revocation's
// braces.
//
// SetCredential stamps password_changed_at, and the lookup requires a session
// to be NEWER than that stamp. So even if the explicit revocation write were
// lost to a crash, an older session stops working.
func TestAPasswordChangeInvalidatesOlderSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")
	newSession(t, db, admin, "OLD", authClock)

	changedAt := authClock.Add(time.Minute)
	if err := db.Users.SetCredential(ctx, admin.UserID,
		testCredential("NEW-HASH"), false, changedAt); err != nil {
		t.Fatalf("set credential: %v", err)
	}

	// Note: no explicit revocation. The timestamp alone must be sufficient.
	if _, err := db.Sessions.ByTokenDigest(ctx, "OLD", changedAt.Add(time.Second)); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("a session issued before a password change is still usable (%v)", err)
	}

	newSession(t, db, admin, "NEW", changedAt.Add(time.Minute))
	if _, err := db.Sessions.ByTokenDigest(ctx, "NEW", changedAt.Add(2*time.Minute)); err != nil {
		t.Errorf("a session issued after the change is not usable: %v", err)
	}
}

func TestThePerUserSessionCapEvictsTheOldest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")

	const cap = 3
	for i := 0; i < 6; i++ {
		_, err := db.Sessions.Create(ctx, store.NewSession{
			SessionID:         domain.NewSessionID(),
			UserID:            admin.UserID,
			Username:          admin.Username,
			Role:              admin.Role,
			TokenDigest:       "DIGEST-" + string(rune('a'+i)),
			IdleExpiresAt:     authClock.Add(time.Hour),
			AbsoluteExpiresAt: authClock.Add(24 * time.Hour),
		}, cap, authClock)
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	live, err := db.Sessions.ListForUser(ctx, admin.UserID, authClock, 50)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(live) != cap {
		t.Errorf("%d live sessions, want the cap of %d", len(live), cap)
	}

	// The three newest survive; the three oldest are superseded.
	for i := 0; i < 3; i++ {
		digest := "DIGEST-" + string(rune('a'+i))
		if _, err := db.Sessions.ByTokenDigest(ctx, digest, authClock); !errors.Is(err, store.ErrSessionNotFound) {
			t.Errorf("session %s survived the cap (%v)", digest, err)
		}
	}
	for i := 3; i < 6; i++ {
		digest := "DIGEST-" + string(rune('a'+i))
		if _, err := db.Sessions.ByTokenDigest(ctx, digest, authClock); err != nil {
			t.Errorf("session %s was evicted but is among the newest: %v", digest, err)
		}
	}
}

func TestRevokingAllForAUserCanSpareOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")

	keep := newSession(t, db, admin, "KEEP", authClock)
	newSession(t, db, admin, "DROP-1", authClock)
	newSession(t, db, admin, "DROP-2", authClock)

	revoked, err := db.Sessions.RevokeAllForUser(ctx, admin.UserID,
		domain.SessionPasswordChanged, keep.SessionID, authClock)
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if revoked != 2 {
		t.Errorf("revoked %d sessions, want 2", revoked)
	}
	if _, err := db.Sessions.ByTokenDigest(ctx, "KEEP", authClock); err != nil {
		t.Errorf("the spared session was revoked anyway: %v", err)
	}
}

func TestStaleSessionsAreExpiredAndPruned(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")
	newSession(t, db, admin, "STALE", authClock)

	later := authClock.Add(48 * time.Hour)
	expired, err := db.Sessions.ExpireStale(ctx, later, 100)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if expired != 1 {
		t.Errorf("expired %d sessions, want 1", expired)
	}

	pruned, err := db.Sessions.Prune(ctx, later.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d sessions, want 1", pruned)
	}

	count, err := db.Sessions.CountActive(ctx, later)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if count != 0 {
		t.Errorf("%d sessions remain active after expiry and pruning", count)
	}
}

func TestASessionRefusesAnInvalidRoleOrIdentifier(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := claimInstallation(t, db, "admin")

	for name, request := range map[string]store.NewSession{
		"no session id": {UserID: admin.UserID, Role: admin.Role, TokenDigest: "D"},
		"no digest": {SessionID: domain.NewSessionID(), UserID: admin.UserID,
			Role: admin.Role},
		"unknown role": {SessionID: domain.NewSessionID(), UserID: admin.UserID,
			Role: "root", TokenDigest: "D"},
	} {
		if _, err := db.Sessions.Create(ctx, request, 10, authClock); !errors.Is(err, store.ErrInvalidInput) {
			t.Errorf("Create(%s) = %v, want ErrInvalidInput", name, err)
		}
	}
}

// -------------------------------------------------------------------- audit --

func TestAuditEventsAreBoundedAndDefaulted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Every string field oversized, and no outcome, no id, no timestamp.
	huge := strings.Repeat("A", 100_000)
	err := db.Audit.Record(ctx, domain.AuditEvent{
		Action:        domain.AuditLoginFailed,
		ActorUsername: huge,
		TargetID:      huge,
		TargetName:    huge,
		ClientAddr:    huge,
		Reason:        huge,
		RequestID:     huge,
	}, authClock)
	if err != nil {
		t.Fatalf("record oversized event: %v", err)
	}

	events, total, err := db.Audit.List(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("%d audit events recorded, want 1", total)
	}

	event := events[0]
	if event.EventID == "" {
		t.Error("an event was stored without an identifier")
	}
	if event.Outcome != domain.AuditFailed {
		t.Errorf("an event with no outcome was stored as %q; an unset outcome must "+
			"fail closed to %q", event.Outcome, domain.AuditFailed)
	}
	if event.OccurredAt.IsZero() {
		t.Error("an event was stored without a timestamp")
	}
	for name, value := range map[string]string{
		"actorUsername": event.ActorUsername,
		"targetId":      event.TargetID,
		"targetName":    event.TargetName,
		"clientAddr":    event.ClientAddr,
		"reason":        event.Reason,
		"requestId":     event.RequestID,
	} {
		if len(value) >= len(huge) {
			t.Errorf("%s was stored at %d bytes; every audit field must be bounded",
				name, len(value))
		}
	}
}

func TestAuditFilteringNarrowsWithoutBuildingSQL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	record := func(action domain.AuditAction, outcome domain.AuditOutcome, actor string) {
		t.Helper()
		if err := db.Audit.Record(ctx, domain.AuditEvent{
			Action: action, Outcome: outcome,
			ActorUserID: actor, ActorUsername: actor,
			TargetType: domain.AuditTargetUser, TargetID: "t-1",
		}, authClock); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	record(domain.AuditLoginSucceeded, domain.AuditSucceeded, "u-1")
	record(domain.AuditLoginFailed, domain.AuditFailed, "u-1")
	record(domain.AuditSnapshotCreated, domain.AuditSucceeded, "u-2")

	byActor, total, err := db.Audit.List(ctx, store.AuditFilter{ActorUserID: "u-1"})
	if err != nil {
		t.Fatalf("filter by actor: %v", err)
	}
	if total != 2 || len(byActor) != 2 {
		t.Errorf("actor filter returned %d events, want 2", total)
	}

	byOutcome, total, err := db.Audit.List(ctx,
		store.AuditFilter{Outcomes: []domain.AuditOutcome{domain.AuditFailed}})
	if err != nil {
		t.Fatalf("filter by outcome: %v", err)
	}
	if total != 1 || len(byOutcome) != 1 {
		t.Errorf("outcome filter returned %d events, want 1", total)
	}

	// A filter value that is not in the vocabulary must match nothing rather
	// than being interpolated into the statement.
	injected, total, err := db.Audit.List(ctx, store.AuditFilter{
		ActorUserID: "u-1' OR '1'='1",
	})
	if err != nil {
		t.Fatalf("filter with an injection attempt: %v", err)
	}
	if total != 0 || len(injected) != 0 {
		t.Errorf("an injection-shaped actor id matched %d events; every filter value "+
			"must be a bound parameter", total)
	}
}

func TestRecentFailuresCountOnlyRecentOnesForThatAddress(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := db.Audit.Record(ctx, domain.AuditEvent{
			Action: domain.AuditLoginFailed, Outcome: domain.AuditFailed,
			ClientAddr: "192.0.2.10",
		}, authClock); err != nil {
			t.Fatalf("record failure: %v", err)
		}
	}
	if err := db.Audit.Record(ctx, domain.AuditEvent{
		Action: domain.AuditLoginFailed, Outcome: domain.AuditFailed,
		ClientAddr: "198.51.100.4",
	}, authClock); err != nil {
		t.Fatalf("record other address: %v", err)
	}

	count, err := db.Audit.RecentFailuresFor(ctx, "192.0.2.10", authClock.Add(-time.Hour))
	if err != nil {
		t.Fatalf("recent failures: %v", err)
	}
	if count != 3 {
		t.Errorf("recent failures for 192.0.2.10 = %d, want 3", count)
	}

	// A window that starts after the events counts none, so the throttle
	// forgets rather than accumulating forever.
	count, err = db.Audit.RecentFailuresFor(ctx, "192.0.2.10", authClock.Add(time.Hour))
	if err != nil {
		t.Fatalf("recent failures in a later window: %v", err)
	}
	if count != 0 {
		t.Errorf("recent failures in a later window = %d, want 0", count)
	}
}

// TestSecurityAuditIsKeptLongerThanOperational is the two-cutoff retention.
//
// An inventory refresh from six months ago is noise. A failed login from six
// months ago is the first entry in a story, and pruning both on one schedule
// would either keep the noise or lose the story.
func TestSecurityAuditIsKeptLongerThanOperational(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	old := authClock.Add(-365 * 24 * time.Hour)
	if err := db.Audit.Record(ctx, domain.AuditEvent{
		Action: domain.AuditInventoryRefreshed, Outcome: domain.AuditSucceeded,
		OccurredAt: old,
	}, old); err != nil {
		t.Fatalf("record operational: %v", err)
	}
	if err := db.Audit.Record(ctx, domain.AuditEvent{
		Action: domain.AuditLoginFailed, Outcome: domain.AuditFailed,
		OccurredAt: old,
	}, old); err != nil {
		t.Fatalf("record security: %v", err)
	}

	// Operational retention of 180 days, security retention of two years.
	operationalCutoff := authClock.Add(-180 * 24 * time.Hour)
	securityCutoff := authClock.Add(-2 * 365 * 24 * time.Hour)

	pruned, err := db.Audit.Prune(ctx, operationalCutoff, securityCutoff, 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d events, want 1 (the operational one only)", pruned)
	}

	remaining, total, err := db.Audit.List(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list after prune: %v", err)
	}
	if total != 1 {
		t.Fatalf("%d events remain, want 1", total)
	}
	if remaining[0].Action != domain.AuditLoginFailed {
		t.Errorf("the surviving event is %q; the SECURITY event must be the one kept",
			remaining[0].Action)
	}
}
