package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Local administration: the recovery path that does not go through HTTP.
//
// # Why this is a separate type
//
// Two operations here have no legitimate remote form: claiming an installation
// without the one-time token, and setting an account's password without knowing
// the old one. Both are correct for someone standing on the host with the
// database in their hands, and both are catastrophic if reachable over a
// network.
//
// Keeping them off AuthService and off UserService is what makes "never exposed
// over HTTP" a STRUCTURAL fact rather than a promise. The api package depends on
// the AuthService and UserService interfaces, and neither declares these
// methods; a handler cannot call what its dependency does not have.
// TestLocalAdminIsNotReachableFromTheAPI fails the build if internal/api so much
// as names this type.
//
// # The authorization is the filesystem
//
// There is no credential check here, because the caller has already proved
// something stronger than any credential: they can read and write the database
// file. Anyone who can do that can already rewrite the users table by hand. The
// CLI's job is to make the supported recovery less dangerous than the
// unsupported one, not to pretend it is gated.
//
// Every operation is still AUDITED, with an actor recorded as the local console
// rather than a user, because "an administrator's password was replaced" is
// exactly the event a compromised host would produce.

// LocalAdminActor is the recorded actor for a console operation.
//
// Not a username: no account performed this, and attributing it to the account
// that was modified would be a lie the audit log then repeats forever.
const LocalAdminActor = "local console"

var (
	// ErrUserNotFound is returned when no account has the given username.
	ErrUserNotFound = errors.New("no account with that username")
	// ErrBootstrapRequired is returned when a recovery operation is attempted
	// on an installation that has no administrator yet.
	ErrBootstrapRequired = errors.New("this installation has not been claimed yet")
)

// LocalAdmin performs console-only account recovery.
type LocalAdmin struct {
	users    *store.UserRepository
	sessions *store.SessionRepository
	audit    *store.AuditRepository
	hasher   *PasswordHasher
	now      func() time.Time
}

// NewLocalAdmin builds the console recovery service.
func NewLocalAdmin(
	users *store.UserRepository,
	sessions *store.SessionRepository,
	audit *store.AuditRepository,
	hasher *PasswordHasher,
	now func() time.Time,
) *LocalAdmin {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &LocalAdmin{users: users, sessions: sessions, audit: audit, hasher: hasher, now: now}
}

// Claimed reports whether this installation already has an administrator.
func (a *LocalAdmin) Claimed(ctx context.Context) (bool, error) {
	state, err := a.users.BootstrapState(ctx)
	if err != nil {
		return false, err
	}
	return state.Completed, nil
}

// Claim creates the first administrator from the console.
//
// No token, for the reason in the file header. It is still refused once the
// installation has an administrator: recovery for a forgotten password is
// ResetPassword below, and allowing a second "first" administrator would turn
// the bootstrap path into a permanent backdoor.
//
// mustChange marks the credential temporary. The caller sets it when the
// password was GENERATED and printed rather than typed, because a password that
// has been on a screen and in a scrollback is a shared secret until its holder
// replaces it.
func (a *LocalAdmin) Claim(
	ctx context.Context,
	username, password string,
	mustChange bool,
) (domain.User, error) {
	normalised := domain.NormaliseUsername(username)
	if !domain.ValidUsername(normalised) {
		return domain.User{}, fmt.Errorf("%w: %s", ErrPasswordRejected, domain.UsernameRule())
	}
	if problem := domain.CheckPassword(password, normalised); problem != domain.PasswordOK {
		return domain.User{}, passwordProblem(problem)
	}

	claimed, err := a.Claimed(ctx)
	if err != nil {
		return domain.User{}, err
	}
	if claimed {
		return domain.User{}, ErrBootstrapClosed
	}

	credential, err := a.hasher.Hash(password)
	if err != nil {
		return domain.User{}, err
	}

	now := a.now().UTC()
	user, err := a.users.CompleteBootstrap(ctx, store.NewUser{
		UserID:             domain.NewUserID(),
		Username:           normalised,
		Role:               domain.RoleAdministrator,
		MustChangePassword: mustChange,
		Credential:         credential,
	}, now)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyBootstrapped) {
			return domain.User{}, ErrBootstrapClosed
		}
		return domain.User{}, err
	}

	a.record(ctx, domain.AuditEvent{
		Action: domain.AuditBootstrapCompleted, Outcome: domain.AuditSucceeded,
		ActorUsername: LocalAdminActor,
		TargetType:    domain.AuditTargetUser, TargetID: user.UserID, TargetName: user.Username,
		Reason: "claimed from the local console",
	}, now)
	return user, nil
}

// ResetOutcome is what a console password reset did.
type ResetOutcome struct {
	// User is the account as it now stands.
	User domain.User
	// SessionsRevoked is how many live sessions were ended.
	SessionsRevoked int64
	// Reactivated reports that a disabled or locked account was made active
	// again, which the operator needs told: it is a change they did not ask for
	// in so many words.
	Reactivated bool
}

// ResetPassword replaces an account's password from the console.
//
// # What it deliberately does
//
//   - Revokes every live session for the account. A password reset whose old
//     sessions survive has not recovered anything from an attacker who holds
//     one.
//   - Reactivates a disabled or locked account. The reset exists because
//     somebody is locked out; leaving them locked out would make it useless.
//     Reported in the outcome so it is never silent.
//   - Requires the password to change on next login when mustChange is set, so
//     a password an operator typed into a terminal (and into their shell
//     history, if they were careless) is temporary.
//
// # What it deliberately does not do
//
// Change the role. A password reset is not a privilege grant, and a CLI that
// could quietly make an account an administrator would be the most attractive
// thing on the host.
func (a *LocalAdmin) ResetPassword(
	ctx context.Context,
	username, password string,
	mustChange bool,
) (ResetOutcome, error) {
	normalised := domain.NormaliseUsername(username)
	if !domain.ValidUsername(normalised) {
		return ResetOutcome{}, ErrUserNotFound
	}
	if problem := domain.CheckPassword(password, normalised); problem != domain.PasswordOK {
		return ResetOutcome{}, passwordProblem(problem)
	}

	claimed, err := a.Claimed(ctx)
	if err != nil {
		return ResetOutcome{}, err
	}
	if !claimed {
		return ResetOutcome{}, ErrBootstrapRequired
	}

	user, err := a.users.ByUsername(ctx, normalised)
	if errors.Is(err, store.ErrNotFound) {
		return ResetOutcome{}, ErrUserNotFound
	}
	if err != nil {
		return ResetOutcome{}, err
	}

	credential, err := a.hasher.Hash(password)
	if err != nil {
		return ResetOutcome{}, err
	}

	now := a.now().UTC()
	if err := a.users.SetCredential(ctx, user.UserID, credential, mustChange, now); err != nil {
		return ResetOutcome{}, err
	}

	outcome := ResetOutcome{}
	if user.Status != domain.UserActive {
		if err := a.users.SetStatus(ctx, user.UserID, domain.UserActive, now); err != nil {
			// The password is already changed. Reporting the partial result is
			// more useful than an error that suggests nothing happened.
			return ResetOutcome{}, fmt.Errorf(
				"the password was changed but the account could not be reactivated: %w", err)
		}
		outcome.Reactivated = true
	}

	// Empty `except`: no session survives a console reset, including one the
	// operator might be holding in a browser.
	revoked, err := a.sessions.RevokeAllForUser(ctx, user.UserID,
		domain.SessionPasswordChanged, "", now)
	if err != nil {
		return ResetOutcome{}, fmt.Errorf(
			"the password was changed but sessions could not be revoked: %w", err)
	}
	outcome.SessionsRevoked = revoked

	refreshed, err := a.users.Get(ctx, user.UserID)
	if err != nil {
		return ResetOutcome{}, err
	}
	outcome.User = refreshed

	a.record(ctx, domain.AuditEvent{
		Action: domain.AuditPasswordReset, Outcome: domain.AuditSucceeded,
		ActorUsername: LocalAdminActor,
		TargetType:    domain.AuditTargetUser, TargetID: refreshed.UserID,
		TargetName: refreshed.Username,
		Reason: fmt.Sprintf("reset from the local console; %s revoked%s",
			plural(revoked, "session"), reactivatedSuffix(outcome.Reactivated)),
	}, now)

	return outcome, nil
}

// record appends an audit row, tolerating failure.
//
// A console operation must not fail because the audit table could not be
// written; the operator is recovering from something already, and the write
// they came for has happened.
func (a *LocalAdmin) record(ctx context.Context, event domain.AuditEvent, now time.Time) {
	if a.audit == nil {
		return
	}
	_ = a.audit.Record(ctx, event, now)
}

// plural renders a count with its noun.
func plural(count int64, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// reactivatedSuffix notes a reactivation in an audit reason.
func reactivatedSuffix(reactivated bool) string {
	if reactivated {
		return "; the account was reactivated"
	}
	return ""
}
