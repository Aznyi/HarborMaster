package service

import (
	"context"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Adapters from the repositories to the narrow interfaces the auth services
// depend on.
//
// # Why the indirection exists
//
// AuthService needs pieces of three repositories -- users, sessions, and audit.
// Taking all three would make its dependency list a description of the storage
// layout rather than of what it needs, and would make a test construct three
// doubles to exercise one login.
//
// One adapter, one interface, one double.

// authStore adapts the repositories to AuthStore.
type authStore struct {
	users    *store.UserRepository
	sessions *store.SessionRepository
	audit    *store.AuditRepository
}

// NewAuthStore builds the persistence adapter the authentication service needs.
func NewAuthStore(
	users *store.UserRepository,
	sessions *store.SessionRepository,
	audit *store.AuditRepository,
) AuthStore {
	return &authStore{users: users, sessions: sessions, audit: audit}
}

func (s *authStore) UserByUsername(ctx context.Context, username string) (domain.User, error) {
	return s.users.ByUsername(ctx, username)
}

func (s *authStore) UserByID(ctx context.Context, userID string) (domain.User, error) {
	return s.users.Get(ctx, userID)
}

func (s *authStore) CredentialFor(ctx context.Context, userID string) (store.Credential, error) {
	return s.users.CredentialFor(ctx, userID)
}

func (s *authStore) RecordLogin(ctx context.Context, userID string, now time.Time) error {
	return s.users.RecordLogin(ctx, userID, now)
}

func (s *authStore) RecordFailure(
	ctx context.Context,
	userID string,
	now time.Time,
	backoff func(attempts int) time.Duration,
) (time.Time, error) {
	return s.users.RecordFailure(ctx, userID, now, backoff)
}

func (s *authStore) SetCredential(
	ctx context.Context,
	userID string,
	credential store.PreparedCredential,
	mustChange bool,
	now time.Time,
) error {
	return s.users.SetCredential(ctx, userID, credential, mustChange, now)
}

func (s *authStore) CreateSession(
	ctx context.Context,
	request store.NewSession,
	maxPerUser int,
	now time.Time,
) (domain.Session, error) {
	return s.sessions.Create(ctx, request, maxPerUser, now)
}

func (s *authStore) SessionByTokenDigest(
	ctx context.Context,
	digest string,
	now time.Time,
) (domain.Session, error) {
	return s.sessions.ByTokenDigest(ctx, digest, now)
}

func (s *authStore) TouchSession(
	ctx context.Context,
	sessionID string,
	lastSeen, idleExpiresAt time.Time,
) error {
	return s.sessions.Touch(ctx, sessionID, lastSeen, idleExpiresAt)
}

func (s *authStore) RevokeSession(
	ctx context.Context,
	sessionID string,
	reason domain.SessionRevocation,
	now time.Time,
) error {
	return s.sessions.Revoke(ctx, sessionID, reason, now)
}

func (s *authStore) RevokeUserSessions(
	ctx context.Context,
	userID string,
	reason domain.SessionRevocation,
	except string,
	now time.Time,
) (int64, error) {
	return s.sessions.RevokeAllForUser(ctx, userID, reason, except, now)
}

func (s *authStore) ListUserSessions(
	ctx context.Context,
	userID string,
	now time.Time,
	limit int,
) ([]domain.Session, error) {
	return s.sessions.ListForUser(ctx, userID, now, limit)
}

func (s *authStore) ExpireStaleSessions(ctx context.Context, now time.Time, batch int) (int64, error) {
	return s.sessions.ExpireStale(ctx, now, batch)
}

func (s *authStore) PruneSessions(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	return s.sessions.Prune(ctx, cutoff, batch)
}

func (s *authStore) BootstrapState(ctx context.Context) (store.BootstrapState, error) {
	return s.users.BootstrapState(ctx)
}

func (s *authStore) SetBootstrapToken(
	ctx context.Context,
	digest string,
	expiresAt, now time.Time,
) error {
	return s.users.SetBootstrapToken(ctx, digest, expiresAt, now)
}

func (s *authStore) CompleteBootstrap(
	ctx context.Context,
	request store.NewUser,
	now time.Time,
) (domain.User, error) {
	return s.users.CompleteBootstrap(ctx, request, now)
}

func (s *authStore) RecentAuthFailures(
	ctx context.Context,
	clientAddr string,
	since time.Time,
) (int, error) {
	return s.audit.RecentFailuresFor(ctx, clientAddr, since)
}

// userAdminStore adapts the repositories to UserAdminStore.
type userAdminStore struct {
	users    *store.UserRepository
	sessions *store.SessionRepository
}

// NewUserAdminStore builds the persistence adapter user administration needs.
func NewUserAdminStore(
	users *store.UserRepository,
	sessions *store.SessionRepository,
) UserAdminStore {
	return &userAdminStore{users: users, sessions: sessions}
}

func (s *userAdminStore) Create(
	ctx context.Context,
	request store.NewUser,
	now time.Time,
) (domain.User, error) {
	return s.users.Create(ctx, request, now)
}

func (s *userAdminStore) Get(ctx context.Context, userID string) (domain.User, error) {
	return s.users.Get(ctx, userID)
}

func (s *userAdminStore) List(
	ctx context.Context,
	filter store.UserFilter,
) ([]domain.User, int, error) {
	return s.users.List(ctx, filter)
}

func (s *userAdminStore) SetRole(
	ctx context.Context,
	userID string,
	role domain.Role,
	now time.Time,
) error {
	return s.users.SetRole(ctx, userID, role, now)
}

func (s *userAdminStore) SetStatus(
	ctx context.Context,
	userID string,
	status domain.UserStatus,
	now time.Time,
) error {
	return s.users.SetStatus(ctx, userID, status, now)
}

func (s *userAdminStore) SetCredential(
	ctx context.Context,
	userID string,
	credential store.PreparedCredential,
	mustChange bool,
	now time.Time,
) error {
	return s.users.SetCredential(ctx, userID, credential, mustChange, now)
}

func (s *userAdminStore) CountActiveAdministrators(ctx context.Context) (int, error) {
	return s.users.CountActiveAdministrators(ctx)
}

func (s *userAdminStore) RevokeUserSessions(
	ctx context.Context,
	userID string,
	reason domain.SessionRevocation,
	except string,
	now time.Time,
) (int64, error) {
	return s.sessions.RevokeAllForUser(ctx, userID, reason, except, now)
}

// ArgonParamsFrom reads the Argon2id cost out of configuration.
//
// The configuration layer owns the DEFAULTS -- it is where an operator sets
// them -- and this package owns the BOUNDS, beside the code that allocates
// against them. Converting here keeps the int-typed configuration away from the
// uint32/uint8 the hashing API takes, and does it once.
func ArgonParamsFrom(cfg config.Auth) ArgonParams {
	// Every conversion is range-checked against this package's own maximum
	// before it happens, so a configured value that would not fit -- or that
	// would wrap on a 64-bit int -- keeps the default rather than becoming a
	// small number. A silently wrapped cost is a silently weakened one.
	params := DefaultArgonParams()
	if cfg.ArgonMemoryKiB > 0 && cfg.ArgonMemoryKiB <= int(MaxArgonMemoryKiB) {
		params.MemoryKiB = uint32(cfg.ArgonMemoryKiB)
	}
	if cfg.ArgonIterations > 0 && cfg.ArgonIterations <= int(MaxArgonIterations) {
		params.Iterations = uint32(cfg.ArgonIterations)
	}
	if cfg.ArgonParallelism > 0 && cfg.ArgonParallelism <= int(MaxArgonParallelism) {
		params.Parallelism = uint8(cfg.ArgonParallelism)
	}
	return params
}
