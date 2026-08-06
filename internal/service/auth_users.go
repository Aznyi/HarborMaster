package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// User administration.
//
// # Every method here is behind PermUserManage
//
// Enforced by the route policy, not by a check in these functions. That
// separation is deliberate: a service method that also decided authorization
// would be a second place the policy lives, and the two would eventually
// disagree. What these methods DO enforce is the invariants authorization
// cannot express -- that an installation always retains a way in, and that a
// privilege change revokes the sessions holding the old privilege.
//
// # No user deletion
//
// Disabling is reversible and preserves history. A deleted user would leave
// audit rows naming an account nobody can look up, which defeats the purpose of
// having recorded them.

// UserAdminStore is the persistence user administration needs.
type UserAdminStore interface {
	Create(ctx context.Context, request store.NewUser, now time.Time) (domain.User, error)
	Get(ctx context.Context, userID string) (domain.User, error)
	List(ctx context.Context, filter store.UserFilter) ([]domain.User, int, error)
	SetRole(ctx context.Context, userID string, role domain.Role, now time.Time) error
	SetStatus(ctx context.Context, userID string, status domain.UserStatus, now time.Time) error
	SetCredential(ctx context.Context, userID string, credential store.PreparedCredential,
		mustChange bool, now time.Time) error
	CountActiveAdministrators(ctx context.Context) (int, error)
	RevokeUserSessions(ctx context.Context, userID string, reason domain.SessionRevocation,
		except string, now time.Time) (int64, error)
}

// UserService owns account administration.
type UserService struct {
	store  UserAdminStore
	audit  *AuditRecorder
	hasher *PasswordHasher

	logger *slog.Logger
	now    func() time.Time
}

// NewUserService builds a UserService.
func NewUserService(
	adminStore UserAdminStore,
	audit *AuditRecorder,
	hasher *PasswordHasher,
	logger *slog.Logger,
	now func() time.Time,
) *UserService {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &UserService{store: adminStore, audit: audit, hasher: hasher, logger: logger, now: now}
}

// CreateUserRequest is an administrator creating an account.
type CreateUserRequest struct {
	Username string
	Role     domain.Role
	// Password is optional. When empty a temporary one is GENERATED and
	// returned once, which is the safer default: it means an administrator
	// never has to invent a password for somebody else, and never has one they
	// chose sitting in their clipboard.
	Password string
}

// CreatedUser is a new account and, when one was generated, its temporary
// password.
//
// The password is returned EXACTLY ONCE, in the response to the request that
// created it. It is never stored in plaintext, never logged, and never
// retrievable again.
type CreatedUser struct {
	User domain.User
	// TemporaryPassword is set only when the service generated one.
	TemporaryPassword string
}

// Create adds an account.
//
// The new account always carries MustChangePassword: the credential was chosen
// by somebody other than its holder, whether generated here or typed by an
// administrator, and a password the account holder did not choose is a shared
// secret until they do.
func (s *UserService) Create(
	ctx context.Context,
	actor Actor,
	request CreateUserRequest,
) (CreatedUser, error) {
	username := domain.NormaliseUsername(request.Username)
	if !domain.ValidUsername(username) {
		return CreatedUser{}, ErrInvalidUsername
	}
	if !domain.ValidRole(string(request.Role)) {
		return CreatedUser{}, ErrInvalidRole
	}

	password := request.Password
	generated := false
	if password == "" {
		var err error
		if password, err = GeneratePassword(); err != nil {
			return CreatedUser{}, err
		}
		generated = true
	}

	if problem := domain.CheckPassword(password, username); problem != domain.PasswordOK {
		return CreatedUser{}, passwordProblem(problem)
	}

	credential, err := s.hasher.Hash(password)
	if err != nil {
		return CreatedUser{}, err
	}

	user, err := s.store.Create(ctx, store.NewUser{
		UserID:             domain.NewUserID(),
		Username:           username,
		Role:               request.Role,
		MustChangePassword: true,
		CreatedByUserID:    actor.UserID,
		Credential:         credential,
	}, s.now().UTC())
	if err != nil {
		return CreatedUser{}, err
	}

	s.audit.RecordAction(ctx, actor, domain.AuditUserCreated, domain.AuditSucceeded,
		domain.AuditTargetUser, user.UserID, user.Username,
		"created with the "+string(user.Role)+" role")

	created := CreatedUser{User: user}
	if generated {
		created.TemporaryPassword = password
	}
	return created, nil
}

// Get returns one account.
func (s *UserService) Get(ctx context.Context, userID string) (domain.User, error) {
	return s.store.Get(ctx, userID)
}

// List returns a page of accounts.
func (s *UserService) List(ctx context.Context, filter store.UserFilter) ([]domain.User, int, error) {
	return s.store.List(ctx, filter)
}

// SetRole changes an account's role and revokes its sessions.
//
// # Why the sessions go
//
// A session carries the privileges of the role it was issued under only in the
// sense that the middleware re-reads the user -- so a demotion takes effect
// immediately without any revocation at all. Revoking anyway is belt and
// braces for the PROMOTION case, where the operator should re-authenticate
// before exercising a privilege they did not have a moment ago, and it makes
// "a privilege change ends the sessions that predate it" a rule with no
// exceptions to reason about.
func (s *UserService) SetRole(
	ctx context.Context,
	actor Actor,
	userID string,
	role domain.Role,
) (domain.User, error) {
	if !domain.ValidRole(string(role)) {
		return domain.User{}, ErrInvalidRole
	}

	before, err := s.store.Get(ctx, userID)
	if err != nil {
		return domain.User{}, err
	}
	if before.Role == role {
		// Nothing to do, and revoking sessions for a no-op change would be a
		// surprising way to sign somebody out.
		return before, nil
	}

	now := s.now().UTC()
	if err := s.store.SetRole(ctx, userID, role, now); err != nil {
		if errors.Is(err, store.ErrLastAdministrator) {
			s.audit.RecordAction(ctx, actor, domain.AuditUserRoleChanged, domain.AuditDenied,
				domain.AuditTargetUser, userID, before.Username,
				"refused: this is the last active administrator")
		}
		return domain.User{}, err
	}

	revoked, err := s.store.RevokeUserSessions(ctx, userID, domain.SessionRoleChanged, "", now)
	if err != nil {
		s.logger.WarnContext(ctx, "could not revoke sessions after a role change",
			slog.String("userId", userID), slog.String("error", err.Error()))
	}

	s.audit.RecordAction(ctx, actor, domain.AuditUserRoleChanged, domain.AuditSucceeded,
		domain.AuditTargetUser, userID, before.Username,
		"role changed from "+string(before.Role)+" to "+string(role)+"; "+
			sessionsRevokedReason(revoked))

	return s.store.Get(ctx, userID)
}

// SetStatus enables or disables an account.
//
// Disabling revokes every session immediately. Without that, a disabled
// operator would keep working until their session expired -- which for an
// account being disabled because it was compromised is exactly the window that
// matters.
func (s *UserService) SetStatus(
	ctx context.Context,
	actor Actor,
	userID string,
	status domain.UserStatus,
) (domain.User, error) {
	if !domain.ValidUserStatus(string(status)) {
		return domain.User{}, ErrInvalidStatus
	}

	before, err := s.store.Get(ctx, userID)
	if err != nil {
		return domain.User{}, err
	}
	if before.Status == status {
		return before, nil
	}

	now := s.now().UTC()
	if err := s.store.SetStatus(ctx, userID, status, now); err != nil {
		if errors.Is(err, store.ErrLastAdministrator) {
			s.audit.RecordAction(ctx, actor, domain.AuditUserDisabled, domain.AuditDenied,
				domain.AuditTargetUser, userID, before.Username,
				"refused: this is the last active administrator")
		}
		return domain.User{}, err
	}

	action := domain.AuditUserEnabled
	reason := "account enabled"
	if status == domain.UserDisabled {
		action = domain.AuditUserDisabled

		revoked, revokeErr := s.store.RevokeUserSessions(ctx, userID,
			domain.SessionUserDisabled, "", now)
		if revokeErr != nil {
			s.logger.WarnContext(ctx, "could not revoke sessions after disabling an account",
				slog.String("userId", userID), slog.String("error", revokeErr.Error()))
		}
		reason = "account disabled; " + sessionsRevokedReason(revoked)
	}

	s.audit.RecordAction(ctx, actor, action, domain.AuditSucceeded,
		domain.AuditTargetUser, userID, before.Username, reason)

	return s.store.Get(ctx, userID)
}

// ResetPassword sets a temporary credential for another account.
//
// The new password is GENERATED rather than chosen by the administrator, and
// returned once. An administrator who picks a password for somebody else picks
// one they know, and "the admin knows your password" is the state this avoids.
//
// Every session on the account is revoked, because a reset is what an
// administrator does when they believe an account is compromised.
func (s *UserService) ResetPassword(
	ctx context.Context,
	actor Actor,
	userID string,
) (string, error) {
	user, err := s.store.Get(ctx, userID)
	if err != nil {
		return "", err
	}

	password, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	credential, err := s.hasher.Hash(password)
	if err != nil {
		return "", err
	}

	now := s.now().UTC()
	if err := s.store.SetCredential(ctx, userID, credential, true, now); err != nil {
		return "", err
	}

	revoked, err := s.store.RevokeUserSessions(ctx, userID, domain.SessionPasswordChanged, "", now)
	if err != nil {
		s.logger.WarnContext(ctx, "could not revoke sessions after a password reset",
			slog.String("userId", userID), slog.String("error", err.Error()))
	}

	s.audit.RecordAction(ctx, actor, domain.AuditPasswordReset, domain.AuditSucceeded,
		domain.AuditTargetUser, userID, user.Username,
		"password reset by an administrator; "+sessionsRevokedReason(revoked))

	return password, nil
}

// User administration errors.
var (
	// ErrInvalidUsername reports a username outside the allowlist.
	ErrInvalidUsername = errors.New("the username is not acceptable")
	// ErrInvalidRole reports an unknown role.
	ErrInvalidRole = errors.New("the role is not one of viewer, operator, or administrator")
	// ErrInvalidStatus reports an unknown status.
	ErrInvalidStatus = errors.New("the status is not one of active or disabled")
)

// GeneratePassword produces a temporary credential.
//
// # Why base64 of 18 random bytes
//
// 144 bits, which is far beyond guessable, and base64url so it is typeable and
// unambiguous when read aloud or pasted. 24 characters comfortably clears the
// twelve-character policy floor.
//
// Deliberately NOT a word list. A memorable password is a benefit only when a
// person chooses to keep it, and this one exists to be used once and replaced.
func GeneratePassword() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("system entropy source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
