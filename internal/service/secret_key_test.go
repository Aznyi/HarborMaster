package service

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// validKeyHex is 32 bytes, the required length, hex-encoded.
var validKeyHex = strings.Repeat("ab", secretKeyBytes)

func writeKeyFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestLoadKeyFromFile(t *testing.T) {
	path := writeKeyFile(t, t.TempDir(), "key", validKeyHex)

	got, err := LoadSecretKey(SecretKeyOptions{File: path})
	if err != nil {
		t.Fatalf("LoadSecretKey: %v", err)
	}
	if len(got.key) != secretKeyBytes {
		t.Errorf("key length = %d, want %d", len(got.key), secretKeyBytes)
	}
	if got.KeyID == "" {
		t.Error("KeyID must be set")
	}
	if got.Source != KeySourceFile {
		t.Errorf("Source = %q, want %q", got.Source, KeySourceFile)
	}
}

func TestLoadKeyAcceptsBase64(t *testing.T) {
	raw := make([]byte, secretKeyBytes)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	path := writeKeyFile(t, t.TempDir(), "key", base64.StdEncoding.EncodeToString(raw))

	got, err := LoadSecretKey(SecretKeyOptions{File: path})
	if err != nil {
		t.Fatalf("LoadSecretKey: %v", err)
	}
	if hex.EncodeToString(got.key) != hex.EncodeToString(raw) {
		t.Error("base64 key did not decode to the expected bytes")
	}
}

func TestLoadKeyFromValueUsesEnvSource(t *testing.T) {
	got, err := LoadSecretKey(SecretKeyOptions{Value: validKeyHex})
	if err != nil {
		t.Fatalf("LoadSecretKey: %v", err)
	}
	if got.Source != KeySourceEnv {
		t.Errorf("Source = %q, want %q", got.Source, KeySourceEnv)
	}
}

// A file beats a directly supplied value: it is the recommended form, so a
// deployment that sets both should get the safer one.
func TestFileTakesPrecedenceOverValue(t *testing.T) {
	other := strings.Repeat("cd", secretKeyBytes)
	path := writeKeyFile(t, t.TempDir(), "key", other)

	got, err := LoadSecretKey(SecretKeyOptions{Value: validKeyHex, File: path})
	if err != nil {
		t.Fatalf("LoadSecretKey: %v", err)
	}
	if got.Source != KeySourceFile {
		t.Errorf("Source = %q, want the file to win", got.Source)
	}
}

func TestLoadKeyRejectsWrongLength(t *testing.T) {
	for name, content := range map[string]string{
		"short": "deadbeef",
		"long":  strings.Repeat("ab", secretKeyBytes+8),
		"empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeKeyFile(t, t.TempDir(), "key", content)
			if _, err := LoadSecretKey(SecretKeyOptions{File: path}); err == nil {
				t.Fatal("expected a wrong-length key to be rejected; a short key silently weakens every digest")
			}
		})
	}
}

func TestLoadKeyRejectsMalformedEncoding(t *testing.T) {
	path := writeKeyFile(t, t.TempDir(), "key", "not-hex-or-base64!!!@@@###$$$%%%^^^&&&***")
	if _, err := LoadSecretKey(SecretKeyOptions{File: path}); err == nil {
		t.Fatal("expected a malformed key to be rejected")
	}
}

func TestLoadKeyRejectsAllZero(t *testing.T) {
	path := writeKeyFile(t, t.TempDir(), "key", strings.Repeat("00", secretKeyBytes))
	if _, err := LoadSecretKey(SecretKeyOptions{File: path}); err == nil {
		t.Fatal("expected an all-zero key to be rejected; it is almost certainly a truncated or placeholder file")
	}
}

func TestLoadKeyRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs privilege on Windows")
	}
	dir := t.TempDir()
	real := writeKeyFile(t, dir, "real", validKeyHex)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := LoadSecretKey(SecretKeyOptions{File: link}); err == nil {
		t.Fatal("expected a symlinked key file to be refused; a symlink is a redirection primitive, not a configuration style")
	}
}

func TestLoadKeyRejectsDirectory(t *testing.T) {
	if _, err := LoadSecretKey(SecretKeyOptions{File: t.TempDir()}); err == nil {
		t.Fatal("expected a directory to be refused")
	}
}

// A configured key that is missing must fail startup. Creating one implicitly
// would produce digests that compare unequal against every historical
// snapshot, which reads to an operator as "every secret changed at once" --
// a false security alarm indistinguishable from a real breach.
func TestLoadKeyRefusesToRegenerateWhenConfiguredFileIsMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	if _, err := LoadSecretKey(SecretKeyOptions{File: missing}); err == nil {
		t.Fatal("a configured but missing key file must fail startup, not regenerate")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("a configured key file must never be created implicitly")
	}
}

func TestGenerateThenReloadIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-hmac.key")

	first, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path})
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.Source != KeySourceGenerated {
		t.Errorf("Source = %q, want %q", first.Source, KeySourceGenerated)
	}

	second, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path})
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first.KeyID != second.KeyID {
		t.Errorf("key changed across reload: %q then %q", first.KeyID, second.KeyID)
	}
	if second.Source != KeySourceFile {
		t.Errorf("second Source = %q, want %q", second.Source, KeySourceFile)
	}
}

func TestGeneratedKeysDifferBetweenInstallations(t *testing.T) {
	a, err := LoadSecretKey(SecretKeyOptions{GeneratePath: filepath.Join(t.TempDir(), "k")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadSecretKey(SecretKeyOptions{GeneratePath: filepath.Join(t.TempDir(), "k")})
	if err != nil {
		t.Fatal(err)
	}
	if a.KeyID == b.KeyID {
		t.Error("two generated keys are identical; crypto/rand is not being used")
	}
}

func TestGeneratedKeyFileIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "snapshot-hmac.key")
	if _, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %#o, want 0600", perm)
	}
}

func TestGenerateCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "snapshot-hmac.key")
	if _, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path}); err != nil {
		t.Fatalf("LoadSecretKey: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("key file not created: %v", err)
	}
}

// The auto-generated key had been in use -- evidenced by snapshots carrying a
// digest key ID -- and the file has since disappeared. Regenerating would
// invalidate every historical digest, so startup fails instead.
func TestRefusesToRegenerateWhenPriorKeyWasUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-hmac.key")

	_, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path, PriorKeyIDSet: true})
	if err == nil {
		t.Fatal("expected startup to fail rather than regenerate a lost key")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("a replacement key must not be written")
	}
}

// A prior key ID with the file still present is the normal restart path.
func TestPriorKeyIDWithExistingFileLoadsNormally(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot-hmac.key")
	if _, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path}); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSecretKey(SecretKeyOptions{GeneratePath: path, PriorKeyIDSet: true}); err != nil {
		t.Fatalf("restart with an intact key should succeed: %v", err)
	}
}

func TestWidePermissionsWarnButLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "key", validKeyHex)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSecretKey(SecretKeyOptions{File: path})
	if err != nil {
		t.Fatalf("wide permissions should warn, not fail: %v", err)
	}
	if !got.PermissionsTooWide {
		t.Error("PermissionsTooWide not reported for a 0644 key file")
	}
}

// Nothing in an error may echo the file's contents. An error string reaches a
// log record, and a log record reaches wherever logs are shipped.
func TestErrorsNeverContainKeyMaterial(t *testing.T) {
	secret := strings.Repeat("cd", secretKeyBytes)
	path := writeKeyFile(t, t.TempDir(), "key", secret+"trailing-garbage")

	_, err := LoadSecretKey(SecretKeyOptions{File: path})
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked key material: %v", err)
	}
	if strings.Contains(err.Error(), "trailing-garbage") {
		t.Errorf("error echoed file contents: %v", err)
	}
}

// The KeyID identifies the key without revealing it.
func TestKeyIDIsDerivedButNotReversible(t *testing.T) {
	got, err := LoadSecretKey(SecretKeyOptions{Value: validKeyHex})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(validKeyHex, got.KeyID) {
		t.Error("KeyID is a substring of the key itself")
	}
	if len(got.KeyID) != keyIDHexLength {
		t.Errorf("KeyID length = %d, want %d", len(got.KeyID), keyIDHexLength)
	}
}

func TestHMACIsKeyedNotPlainHash(t *testing.T) {
	a, err := LoadSecretKey(SecretKeyOptions{Value: validKeyHex})
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadSecretKey(SecretKeyOptions{Value: strings.Repeat("cd", secretKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if a.HMAC("hunter2") == b.HMAC("hunter2") {
		t.Error("two different keys produced the same digest; the digest is not keyed")
	}
	if a.HMAC("hunter2") != a.HMAC("hunter2") {
		t.Error("the same key and value produced different digests")
	}
}

// A key file large enough to exhaust memory must not be read whole.
func TestLoadKeyBoundsFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecretKey(SecretKeyOptions{File: path}); err == nil {
		t.Fatal("expected an oversized key file to be rejected")
	}
}
