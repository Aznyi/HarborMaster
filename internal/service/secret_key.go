package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// secretKeyBytes is the required key length. Exactly 32 bytes: HMAC-SHA-256's
// block-optimal size, and a length a short or truncated file cannot satisfy by
// accident.
const secretKeyBytes = 32

// keyIDHexLength is the hex length of a KeyID: 4 bytes of a SHA-256 over the
// key. Enough to tell two keys apart, far too little to attack the key.
const keyIDHexLength = 8

// maxKeyFileBytes bounds how much of a key file is read.
//
// A key file is tiny. Reading an unbounded amount from an operator-supplied
// path would let a mistyped path -- /dev/zero, a log file, a disk image --
// exhaust memory during startup.
const maxKeyFileBytes = 256

// KeySource records where the installation key came from. Reported at startup
// so an operator can confirm the deployment is using the form they intended.
type KeySource string

// Key sources.
const (
	// KeySourceFile is a key read from a path, which is how Docker Secrets and
	// Kubernetes secret volumes present material. The preferred form.
	KeySourceFile KeySource = "file"
	// KeySourceEnv is a key supplied directly in the environment. Works, but an
	// environment variable is readable through /proc/<pid>/environ and is
	// routinely captured by process inspectors and crash reporters.
	KeySourceEnv KeySource = "env"
	// KeySourceGenerated is a key HarborMaster created on first start. A
	// standalone convenience, not the recommended production path.
	KeySourceGenerated KeySource = "generated"
)

// SecretKey is the installation key that derives secret digests.
//
// The key bytes are unexported and there is no accessor: everything outside
// this file interacts with the key through HMAC, so there is no code path that
// can copy the material into a log record, an API response, or the database.
type SecretKey struct {
	key []byte

	// KeyID identifies the key without revealing it. Derived by hashing, so it
	// is safe to store beside a digest and to print at startup, and it is what
	// tells digests produced under different keys apart.
	KeyID string
	// Source records how the key was obtained.
	Source KeySource
	// PermissionsTooWide reports that the key file was readable beyond its
	// owner. Loading proceeds -- refusing would strand an operator whose umask
	// differs -- but startup warns.
	PermissionsTooWide bool
	// ObservedMode is the permission bits found, for that warning. Zero when
	// the key did not come from a file.
	ObservedMode os.FileMode
}

// SecretKeyOptions selects where the key comes from.
//
// Resolution order is File, then Value, then GeneratePath. File wins over Value
// because it is the safer form, so a deployment that sets both gets the better
// one rather than the one that happens to be checked first.
type SecretKeyOptions struct {
	// File is a path to read the key from.
	File string
	// Value is a hex or base64 key supplied directly.
	Value string
	// GeneratePath is where a key may be created when neither of the above is
	// configured. Empty disables generation entirely.
	GeneratePath string
	// PriorKeyIDSet reports that snapshots already exist carrying a digest key
	// ID, meaning a key was in use. With it set, a missing key file is a fatal
	// error rather than a reason to generate a new one.
	PriorKeyIDSet bool
}

// Key-loading errors. Callers distinguish these to decide whether the problem
// is configuration or a lost key.
var (
	// ErrKeyMissing reports a configured key that could not be read.
	ErrKeyMissing = errors.New("snapshot HMAC key is missing")
	// ErrKeyInvalid reports a key that was read but is unusable.
	ErrKeyInvalid = errors.New("snapshot HMAC key is invalid")
	// ErrKeyLost reports that a key was previously in use and has disappeared.
	ErrKeyLost = errors.New("snapshot HMAC key was in use and is now missing")
)

// LoadSecretKey resolves the installation key.
//
// Every failure mode is fatal rather than degrading. The alternative --
// generating a replacement -- would produce digests that compare unequal
// against every historical snapshot, which an operator reads as "every secret
// in every container changed at once": a false security alarm indistinguishable
// from a real breach. Refusing to start is loud, recoverable, and honest.
//
// No error returned from here contains key material or file contents.
func LoadSecretKey(opts SecretKeyOptions) (SecretKey, error) {
	switch {
	case opts.File != "":
		return loadKeyFile(opts.File, KeySourceFile)

	case opts.Value != "":
		key, err := decodeKey(strings.TrimSpace(opts.Value))
		if err != nil {
			return SecretKey{}, fmt.Errorf("%w: from the environment: %w", ErrKeyInvalid, err)
		}
		return newSecretKey(key, KeySourceEnv), nil

	case opts.GeneratePath != "":
		return loadOrGenerate(opts.GeneratePath, opts.PriorKeyIDSet)

	default:
		return SecretKey{}, fmt.Errorf("%w: no key file, key value, or generation path configured", ErrKeyMissing)
	}
}

// HMAC returns the hex-encoded keyed digest of value.
//
// Keyed rather than a bare SHA-256: a stolen database then contains digests an
// attacker cannot attack without also holding the key, which is what makes a
// weak secret non-recoverable from the database alone.
func (k SecretKey) HMAC(value string) string {
	mac := hmac.New(sha256.New, k.key)
	// hash.Hash never returns an error from Write.
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// HMACFor derives a digest under a NAMED PURPOSE.
//
// # Why domain separation matters here
//
// One installation key now protects three unrelated things: snapshot secret
// digests, session tokens, and CSRF tokens. Without separation they share a
// keyspace, and a value that is a legitimate digest in one context is a
// legitimate digest in the others -- so anywhere an attacker can get a digest
// computed over chosen input becomes a way to mint a value another subsystem
// will accept.
//
// Prefixing with a length-delimited purpose makes the three keyspaces disjoint:
// no input to one can produce the same MAC as any input to another, because the
// purpose is unambiguously recoverable from the prefix.
//
// HMAC (the unprefixed method above) is left exactly as it was. Snapshot
// digests are compared against values recorded by earlier releases, and adding
// a prefix would make every historical digest compare unequal -- which an
// operator would read as "every secret in every container changed at once".
func (k SecretKey) HMACFor(purpose, value string) string {
	mac := hmac.New(sha256.New, k.key)
	// The length prefix is what makes the encoding injective. Without it,
	// purpose "ab" + value "c" and purpose "a" + value "bc" would hash
	// identically.
	_, _ = mac.Write([]byte(strconv.Itoa(len(purpose))))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// Purposes for HMACFor. Named constants rather than string literals at the call
// sites, so a typo cannot silently create a fourth keyspace that nothing else
// can verify against.
const (
	// PurposeSession digests a session token for storage.
	PurposeSession = "harbormaster.session.v1"
	// PurposeCSRF derives a CSRF token from a session token.
	PurposeCSRF = "harbormaster.csrf.v1"
	// PurposeBootstrap digests the one-time bootstrap token.
	PurposeBootstrap = "harbormaster.bootstrap.v1"
)

// Valid reports whether the key is usable. A zero SecretKey is not.
func (k SecretKey) Valid() bool { return len(k.key) == secretKeyBytes }

// newSecretKey derives the identity fields from the key material.
func newSecretKey(key []byte, source KeySource) SecretKey {
	sum := sha256.Sum256(key)
	return SecretKey{
		key:    key,
		KeyID:  hex.EncodeToString(sum[:keyIDHexLength/2]),
		Source: source,
	}
}

// loadKeyFile reads and validates a key file.
//
// The file is opened first and then inspected through the resulting descriptor,
// never through the path a second time: checking the path and then opening it
// leaves a window in which the two can refer to different files.
func loadKeyFile(path string, source KeySource) (SecretKey, error) {
	file, err := openKeyFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Named without contents: the path is configuration the operator
			// supplied, so echoing it helps them fix it.
			return SecretKey{}, fmt.Errorf("%w: %s", ErrKeyMissing, path)
		}
		return SecretKey{}, fmt.Errorf("%w: cannot open %s: %w", ErrKeyMissing, path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return SecretKey{}, fmt.Errorf("%w: cannot inspect %s: %w", ErrKeyInvalid, path, err)
	}
	if !info.Mode().IsRegular() {
		// Directories, devices, FIFOs, and sockets are all rejected. A FIFO in
		// particular would block startup indefinitely.
		return SecretKey{}, fmt.Errorf("%w: %s is not a regular file", ErrKeyInvalid, path)
	}
	if info.Size() > maxKeyFileBytes {
		return SecretKey{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrKeyInvalid, path, maxKeyFileBytes)
	}

	// Limited even though the size was checked: the file could grow between the
	// stat and the read, and the limit costs nothing.
	raw, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return SecretKey{}, fmt.Errorf("%w: cannot read %s: %w", ErrKeyInvalid, path, err)
	}
	if len(raw) > maxKeyFileBytes {
		return SecretKey{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrKeyInvalid, path, maxKeyFileBytes)
	}

	key, err := decodeKey(strings.TrimSpace(string(raw)))
	if err != nil {
		// The decode error describes the shape of the problem and never the
		// contents, so this is safe to log.
		return SecretKey{}, fmt.Errorf("%w: %s: %w", ErrKeyInvalid, path, err)
	}

	resolved := newSecretKey(key, source)
	resolved.ObservedMode = info.Mode().Perm()
	// Warn rather than refuse: a operator whose umask or volume driver widened
	// the mode should be told, not stranded.
	resolved.PermissionsTooWide = info.Mode().Perm()&^os.FileMode(0o600) != 0
	return resolved, nil
}

// loadOrGenerate returns the key at path, creating one if it does not exist.
//
// Generation happens on exactly one transition: no key file AND no evidence
// that a key was ever in use.
func loadOrGenerate(path string, priorKeyIDSet bool) (SecretKey, error) {
	key, err := loadKeyFile(path, KeySourceFile)
	switch {
	case err == nil:
		return key, nil
	case !errors.Is(err, ErrKeyMissing):
		// The file exists but is unusable. Never overwrite it: it may be
		// recoverable, and clobbering it would destroy the only thing that can
		// verify historical digests.
		return SecretKey{}, err
	}

	if priorKeyIDSet {
		return SecretKey{}, fmt.Errorf(
			"%w: snapshots already exist that were digested with a key, but %s is gone; "+
				"restore it from backup, or remove the existing snapshots to start fresh",
			ErrKeyLost, path)
	}

	return generateKeyFile(path)
}

// generateKeyFile creates a new key.
//
// Written to a temporary file and renamed, so a crash mid-write can never leave
// a truncated key that would silently produce wrong digests. The temporary file
// is opened O_EXCL so two processes racing at first startup cannot both believe
// they created it.
func generateKeyFile(path string) (SecretKey, error) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return SecretKey{}, fmt.Errorf("create key directory: %w", err)
		}
	}

	key := make([]byte, secretKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return SecretKey{}, fmt.Errorf("generate key: %w", err)
	}
	encoded := []byte(hex.EncodeToString(key))

	temp := path + ".tmp"
	// G304: the path is operator configuration, not request input. It reaches
	// here only from SecretKeyOptions, which is populated from the environment
	// at startup. O_EXCL is what makes the write safe, not the path.
	//nolint:gosec // G304: configured path, opened O_EXCL.
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Another process is generating right now. Let it finish and read
			// what it wrote rather than fighting over the file.
			return loadKeyFile(path, KeySourceFile)
		}
		return SecretKey{}, fmt.Errorf("create key file: %w", err)
	}

	if err := writeAndSync(file, encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return SecretKey{}, fmt.Errorf("write key file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return SecretKey{}, fmt.Errorf("close key file: %w", err)
	}

	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return SecretKey{}, fmt.Errorf("install key file: %w", err)
	}
	// Durability of the rename itself, not just the contents: without this a
	// crash could leave the directory entry unwritten and the key lost.
	syncDir(dir)

	resolved := newSecretKey(key, KeySourceGenerated)
	resolved.ObservedMode = 0o600
	return resolved, nil
}

func writeAndSync(file *os.File, content []byte) error {
	if _, err := file.Write(content); err != nil {
		return err
	}
	return file.Sync()
}

// syncDir flushes a directory entry. Best effort: not every platform or
// filesystem supports it, and failing to sync a directory is not a reason to
// refuse a key that was otherwise written successfully.
func syncDir(dir string) {
	if dir == "" {
		return
	}
	// G304: the directory is derived from the configured key path. Opened
	// read-only, solely to fsync the directory entry.
	//nolint:gosec // G304: configured path, opened read-only for fsync.
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = handle.Close() }()
	_ = handle.Sync()
}

// decodeKey parses and validates key material.
//
// Errors describe the shape of the problem and never echo the input: this error
// reaches a log record, and a log record reaches wherever logs are shipped.
func decodeKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("key is empty")
	}

	var (
		key []byte
		err error
	)
	switch {
	case isHex(encoded):
		key, err = hex.DecodeString(encoded)
	default:
		key, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			key, err = base64.RawStdEncoding.DecodeString(encoded)
		}
		if err != nil {
			key, err = base64.URLEncoding.DecodeString(encoded)
		}
	}
	if err != nil {
		return nil, errors.New("key is not valid hex or base64")
	}

	if len(key) != secretKeyBytes {
		// The length is a property of the key, not the key, so reporting it
		// helps an operator without disclosing anything.
		return nil, fmt.Errorf("key decoded to %d bytes, want exactly %d", len(key), secretKeyBytes)
	}

	// An all-zero key is almost certainly a truncated file, a placeholder, or a
	// zero-filled volume, and it would produce digests an attacker can
	// reproduce without the key at all.
	var zero [secretKeyBytes]byte
	if subtle.ConstantTimeCompare(key, zero[:]) == 1 {
		return nil, errors.New("key is all zero bytes")
	}

	return key, nil
}

func isHex(s string) bool {
	if len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
