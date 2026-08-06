package service

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/argon2"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Password hashing.
//
// # Argon2id, and no custom cryptography
//
// The construction is golang.org/x/crypto/argon2's IDKey -- the Password
// Hashing Competition winner in its hybrid mode, which is the current
// recommendation for interactive logins because it resists both GPU
// parallelism (memory hardness) and side-channel analysis (the "id" hybrid).
//
// Nothing in this file invents a primitive. It chooses parameters, generates a
// salt, calls IDKey, and compares in constant time. Every one of those four is
// a place a hand-rolled scheme goes wrong, and none of them is a place to be
// clever.
//
// # Parameters travel with the hash
//
// A credential stores the memory, iteration, and parallelism values it was
// produced with. Verification uses THOSE, not the current policy -- otherwise
// raising the cost would invalidate every existing password at once. A
// successful login whose parameters are below the current policy is
// transparently re-hashed, so an installation upgrades itself as people log in.
//
// # Nothing here is logged, returned, or formatted
//
// No function in this file takes a logger. No error it produces contains a
// password, a salt, or a hash -- the errors are sentinels with fixed text. The
// plaintext exists as a parameter and goes out of scope.

// Argon2 parameter bounds.
//
// The floor is what makes a misconfiguration refuse rather than silently
// weaken; the ceiling is what stops a configuration error turning every login
// into a denial of service against the process's own memory.
const (
	// MinArgonMemoryKiB is 16 MiB. Below this the memory hardness that
	// distinguishes Argon2 from a fast hash stops being meaningful.
	MinArgonMemoryKiB uint32 = 16 * 1024
	// MaxArgonMemoryKiB is 1 GiB. A login must not be able to allocate more
	// than a container's whole memory limit, and several concurrent logins each
	// allocate independently.
	MaxArgonMemoryKiB uint32 = 1024 * 1024

	MinArgonIterations uint32 = 1
	MaxArgonIterations uint32 = 16

	MinArgonParallelism uint8 = 1
	MaxArgonParallelism uint8 = 16

	// ArgonSaltBytes is 16, the reference implementation's recommendation. A
	// salt is for uniqueness rather than secrecy; 128 bits makes a collision
	// across any realistic number of credentials impossible.
	ArgonSaltBytes = 16
	// ArgonKeyBytes is 32, matching SHA-256's output width.
	ArgonKeyBytes uint32 = 32

	// argonAlgorithm is the only algorithm this build produces or accepts.
	argonAlgorithm = "argon2id"
)

// Default parameters.
//
// 64 MiB, 3 passes, 4 lanes. Comfortably above OWASP's current minimum
// (19 MiB / 2 passes) and chosen so a login costs roughly 100ms on the modest
// hardware HarborMaster is deployed on -- slow enough that offline guessing is
// expensive, fast enough that a person does not notice.
//
// An operator on constrained hardware can lower them; the floors above stop
// them being lowered into meaninglessness.
const (
	DefaultArgonMemoryKiB   uint32 = uint32(config.DefaultArgonMemoryKiB)
	DefaultArgonIterations  uint32 = uint32(config.DefaultArgonIterations)
	DefaultArgonParallelism uint8  = uint8(config.DefaultArgonParallelism)
)

// Password errors. Sentinels with fixed text: none carries a password, a hash,
// or anything derived from either.
var (
	// ErrPasswordMismatch reports a password that did not verify.
	ErrPasswordMismatch = errors.New("password does not match")
	// ErrCredentialUnusable reports a stored credential that cannot be verified
	// against -- an unknown algorithm, an unparseable salt, out-of-range
	// parameters.
	//
	// Treated as a failed login rather than an error the operator sees, and
	// never as a reason to accept the password: a credential HarborMaster
	// cannot evaluate has not been satisfied.
	ErrCredentialUnusable = errors.New("stored credential cannot be verified")
	// ErrArgonParameters reports a configured parameter outside its bounds.
	ErrArgonParameters = errors.New("argon2 parameters are out of range")
)

// ArgonParams is one Argon2id cost setting.
type ArgonParams struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultArgonParams returns the built-in cost.
func DefaultArgonParams() ArgonParams {
	return ArgonParams{
		MemoryKiB:   DefaultArgonMemoryKiB,
		Iterations:  DefaultArgonIterations,
		Parallelism: DefaultArgonParallelism,
	}
}

// Validate reports whether the parameters are within bounds.
func (p ArgonParams) Validate() error {
	if p.MemoryKiB < MinArgonMemoryKiB || p.MemoryKiB > MaxArgonMemoryKiB {
		return fmt.Errorf("%w: memory", ErrArgonParameters)
	}
	if p.Iterations < MinArgonIterations || p.Iterations > MaxArgonIterations {
		return fmt.Errorf("%w: iterations", ErrArgonParameters)
	}
	if p.Parallelism < MinArgonParallelism || p.Parallelism > MaxArgonParallelism {
		return fmt.Errorf("%w: parallelism", ErrArgonParameters)
	}
	return nil
}

// AtLeast reports whether these parameters meet or exceed the policy.
//
// Used to decide whether a credential should be re-hashed after a successful
// login. Every dimension has to meet the policy: a credential with more memory
// but fewer iterations than the current setting is not "at least as strong",
// because the two are not interchangeable.
func (p ArgonParams) AtLeast(policy ArgonParams) bool {
	return p.MemoryKiB >= policy.MemoryKiB &&
		p.Iterations >= policy.Iterations &&
		p.Parallelism >= policy.Parallelism
}

// PasswordHasher produces and verifies Argon2id credentials.
//
// Holds the CURRENT policy. Verification does not use it -- a stored credential
// carries its own parameters -- but hashing and the re-hash decision do.
type PasswordHasher struct {
	params ArgonParams

	// decoy is a real credential over a value nobody knows, built lazily so
	// constructing a hasher stays cheap. See decoyCredential.
	decoyOnce sync.Once
	decoy     store.Credential
}

// NewPasswordHasher builds a hasher, refusing parameters outside the bounds.
//
// Refusing rather than clamping: an operator who set 1 MiB meant something, and
// silently running at 16 MiB would leave them believing a setting took effect
// that did not.
func NewPasswordHasher(params ArgonParams) (*PasswordHasher, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	return &PasswordHasher{params: params}, nil
}

// Params returns the current policy.
func (h *PasswordHasher) Params() ArgonParams { return h.params }

// Hash produces a credential for a password.
//
// A fresh 128-bit salt per credential, from the system entropy source, so two
// accounts choosing the same password produce different verifiers and no
// precomputed table applies.
func (h *PasswordHasher) Hash(password string) (store.PreparedCredential, error) {
	salt := make([]byte, ArgonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		// The one failure mode worth propagating: without entropy there is no
		// safe salt, and a fixed one would be worse than refusing.
		return store.PreparedCredential{}, errors.New("system entropy source unavailable")
	}

	key := argon2.IDKey([]byte(password), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, ArgonKeyBytes)

	return store.PreparedCredential{
		Algorithm:   argonAlgorithm,
		MemoryKiB:   h.params.MemoryKiB,
		Iterations:  h.params.Iterations,
		Parallelism: h.params.Parallelism,
		Salt:        base64.RawStdEncoding.EncodeToString(salt),
		Hash:        base64.RawStdEncoding.EncodeToString(key),
	}, nil
}

// Verify checks a password against a stored credential.
//
// # Constant time
//
// The comparison is subtle.ConstantTimeCompare. A byte-by-byte comparison that
// returned early would leak how much of the hash matched, which over enough
// attempts recovers it -- and a recovered hash is an offline attack that no
// longer needs the server.
//
// # An unusable credential never verifies
//
// An unknown algorithm, a corrupt salt, or out-of-range parameters produce
// ErrCredentialUnusable rather than any comparison at all. The alternative --
// falling back to some default -- would mean a corrupted row could be made to
// accept a chosen password.
func (h *PasswordHasher) Verify(credential store.Credential, password string) error {
	if credential.Algorithm != argonAlgorithm {
		return ErrCredentialUnusable
	}

	stored := ArgonParams{
		MemoryKiB:   credential.MemoryKiB,
		Iterations:  credential.Iterations,
		Parallelism: credential.Parallelism,
	}
	// Bounds-checked before use, because these values drive an allocation. A
	// row claiming 64 GiB of memory would otherwise be an out-of-memory kill
	// triggered by a login attempt.
	if err := stored.Validate(); err != nil {
		return ErrCredentialUnusable
	}

	salt, err := base64.RawStdEncoding.DecodeString(credential.Salt)
	if err != nil || len(salt) == 0 {
		return ErrCredentialUnusable
	}
	expected, err := base64.RawStdEncoding.DecodeString(credential.Hash)
	if err != nil || len(expected) == 0 {
		return ErrCredentialUnusable
	}

	// The output length is this package's own constant, not a value derived
	// from the row. A stored hash of any other length is an unusable credential
	// rather than an allocation request sized by whatever the column happened to
	// contain -- which is the difference between a corrupt row being refused and
	// a corrupt row deciding how much memory a login costs.
	if len(expected) != int(ArgonKeyBytes) {
		return ErrCredentialUnusable
	}
	computed := argon2.IDKey([]byte(password), salt,
		stored.Iterations, stored.MemoryKiB, stored.Parallelism, ArgonKeyBytes)

	if subtle.ConstantTimeCompare(computed, expected) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// NeedsRehash reports whether a credential was produced below current policy.
//
// Checked after a SUCCESSFUL verification, which is the only moment the
// plaintext is available to re-hash with. An installation therefore upgrades
// itself as people log in, without a migration that cannot exist -- a password
// hash cannot be strengthened without the password.
func (h *PasswordHasher) NeedsRehash(credential store.Credential) bool {
	if credential.Algorithm != argonAlgorithm {
		return true
	}
	stored := ArgonParams{
		MemoryKiB:   credential.MemoryKiB,
		Iterations:  credential.Iterations,
		Parallelism: credential.Parallelism,
	}
	return !stored.AtLeast(h.params)
}

// decoyCredential is a real, valid credential over a value nobody knows.
//
// # What it is for
//
// User enumeration through timing. A login for an unknown username has nothing
// to hash against, so the naive implementation returns immediately -- while a
// known username pays for a full Argon2id evaluation. That difference is tens
// of milliseconds and trivially measurable over a network, which turns the
// login endpoint into an account directory.
//
// So an unknown username is verified against THIS instead. The work is
// identical, the answer is always "no", and the timings match.
//
// Built once at first use, under the CURRENT parameters, so it stays matched to
// what a real credential costs.
func (h *PasswordHasher) decoyCredential() store.Credential {
	h.decoyOnce.Do(func() {
		// A random password nobody holds. Generated rather than fixed so the
		// decoy hash is not a known constant that could be recognised in a
		// memory dump and used to identify the code path.
		var filler [32]byte
		_, _ = rand.Read(filler[:])

		prepared, err := h.Hash(base64.RawStdEncoding.EncodeToString(filler[:]))
		if err != nil {
			return
		}
		h.decoy = store.Credential{
			Algorithm:   prepared.Algorithm,
			MemoryKiB:   prepared.MemoryKiB,
			Iterations:  prepared.Iterations,
			Parallelism: prepared.Parallelism,
			Salt:        prepared.Salt,
			Hash:        prepared.Hash,
		}
	})
	return h.decoy
}

// VerifyDecoy performs the same work as Verify and always fails.
//
// Called on the unknown-username path so its cost matches the known-username
// path. The return value is discarded by the caller; what matters is the time
// it took.
func (h *PasswordHasher) VerifyDecoy(password string) {
	decoy := h.decoyCredential()
	if decoy.Hash == "" {
		// The decoy could not be built, which means entropy was unavailable at
		// first use. Nothing useful to do: the caller still reports the same
		// failure, and the timing difference is the lesser problem.
		return
	}
	_ = h.Verify(decoy, password)
}
