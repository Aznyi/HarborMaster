package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Users and their credentials.
//
// # What is not here
//
// No password field, in any form. A User is identity and authorization; the
// credential lives in its own table, is written only by the authentication
// service, and is never loaded into this struct. That separation is what makes
// it safe to return a User from an API endpoint at all.
//
// No email, no personal name, no avatar. HarborMaster authenticates operators
// of one host; every field it does not hold is a field it cannot leak.

// UserStatus is whether an account may authenticate.
type UserStatus string

// Account statuses.
const (
	// UserActive means the account may log in.
	UserActive UserStatus = "active"
	// UserDisabled means it may not.
	//
	// Disabling is reversible and preserves history, which is why there is no
	// user deletion: an audit record naming a user who no longer exists is a
	// record nobody can interpret.
	UserDisabled UserStatus = "disabled"
)

// UserStatuses lists every status.
var UserStatuses = []UserStatus{UserActive, UserDisabled}

// ValidUserStatus reports whether name is a known status.
func ValidUserStatus(name string) bool {
	for _, status := range UserStatuses {
		if string(status) == name {
			return true
		}
	}
	return false
}

// User is one account.
//
// Every field here is safe to serialise. There is deliberately no field that
// could carry a credential, a session token, or a hash.
type User struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// UserID is the IMMUTABLE public identifier, generated server-side.
	//
	// Random rather than sequential, and used in URLs in place of the row id,
	// so an administration endpoint cannot be walked and the number of accounts
	// is not disclosed by an identifier.
	UserID string `json:"userId"`

	// Username is the login name, stored lowercase.
	Username string     `json:"username"`
	Role     Role       `json:"role"`
	Status   UserStatus `json:"status"`

	// MustChangePassword marks a credential set by an administrator or by the
	// bootstrap flow. The session is created but every request except the
	// password change is refused until it is cleared.
	MustChangePassword bool `json:"mustChangePassword"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// LastLoginAt is when a session was last issued. Nil for an account that
	// has never logged in.
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
	// PasswordChangedAt is when the credential was last set. Sessions issued
	// before it are invalid, which is what makes a password change revoke them
	// even if the revocation write is lost.
	PasswordChangedAt time.Time `json:"passwordChangedAt"`

	// CreatedByUserID is the administrator who created the account, or empty
	// for the bootstrap administrator.
	CreatedByUserID string `json:"createdByUserId,omitempty"`
}

// Active reports whether the account may authenticate.
func (u User) Active() bool { return u.Status == UserActive }

// Can reports whether the user holds a permission.
//
// A DISABLED user holds none, whatever their role. Checked here rather than
// only at login, so a session that outlives a disablement by a moment cannot
// be used: the authorization middleware re-reads the user on every request.
func (u User) Can(permission Permission) bool {
	if !u.Active() {
		return false
	}
	return u.Role.Can(permission)
}

// UserIDPrefix and UserIDHexLength describe a generated identifier.
const (
	UserIDPrefix    = "usr_"
	UserIDHexLength = 20
)

// NewUserID generates an immutable public identifier.
//
// Panics only if the system entropy source fails, which on every supported
// platform means the process cannot safely continue anyway.
func NewUserID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return UserIDPrefix + hex.EncodeToString(raw[:])
}

// ValidUserID reports whether id has the shape of a generated identifier.
func ValidUserID(id string) bool {
	if len(id) != len(UserIDPrefix)+UserIDHexLength {
		return false
	}
	if id[:len(UserIDPrefix)] != UserIDPrefix {
		return false
	}
	for _, r := range id[len(UserIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------ usernames --

// Username bounds.
const (
	// MinUsernameBytes stops a single-character account being created by
	// accident.
	MinUsernameBytes = 3
	// MaxUsernameBytes bounds a value that reaches a unique index, a log
	// record, an audit row, and a browser.
	MaxUsernameBytes = 64
)

// NormaliseUsername folds a username to its stored form.
//
// Lowercased and trimmed, so "Admin", "admin", and " admin " are ONE account
// rather than three. Case-insensitive matching has to happen somewhere, and
// doing it by normalising on the way in means the unique index enforces it --
// rather than a query using a collation that a future migration could change.
//
// ASCII-only lowering, deliberately: unicode.ToLower would fold the Turkish
// dotless ı and the Kelvin sign K onto ASCII letters, which is exactly the
// homograph confusion the allowlist below exists to prevent.
func NormaliseUsername(raw string) string {
	trimmed := strings.TrimSpace(raw)

	var builder strings.Builder
	builder.Grow(len(trimmed))
	for i := 0; i < len(trimmed); i++ {
		char := trimmed[i]
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		builder.WriteByte(char)
	}
	return builder.String()
}

// ValidUsername reports whether a normalised username is acceptable.
//
// An ALLOWLIST: lowercase ASCII letters, digits, and the three separators, with
// an alphanumeric first character. Everything outside it is refused rather than
// escaped.
//
// The reason is homographs. A username reaches an audit record, a log line, and
// a page an administrator reads to decide who did what; two accounts that
// render identically -- "admin" and "аdmin" with a Cyrillic а -- would make
// that page actively misleading. Refusing non-ASCII costs an operator nothing
// HarborMaster's audience needs and removes the whole class.
func ValidUsername(username string) bool {
	if len(username) < MinUsernameBytes || len(username) > MaxUsernameBytes {
		return false
	}
	if username != NormaliseUsername(username) {
		return false
	}

	for i := 0; i < len(username); i++ {
		char := username[i]
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			continue
		case i == 0:
			// The first character must be alphanumeric: a name beginning with
			// a dot or a dash is the shape of a command-line flag, and one
			// beginning with an underscore reads as reserved.
			return false
		case char == '.', char == '-', char == '_':
			continue
		default:
			return false
		}
	}
	// A trailing separator is the other half of the same rule: "admin." and
	// "admin" are too easily confused in a list.
	last := username[len(username)-1]
	return last != '.' && last != '-' && last != '_'
}

// UsernameRule renders the allowlist in operator-facing words.
//
// One function rather than the same sentence written out at each rejection
// site, so the message an operator reads cannot drift away from what
// ValidUsername actually enforces.
func UsernameRule() string {
	return fmt.Sprintf(
		"a username is %d-%d characters of lowercase letters, digits, dot, dash, "+
			"or underscore, starting with a letter or digit and not ending with a separator",
		MinUsernameBytes, MaxUsernameBytes)
}

// ------------------------------------------------------------ passwords --

// Password bounds.
//
// # Why there is a maximum
//
// Argon2id's cost is linear in input length only up to the hash's block size,
// so a long password is not itself expensive -- but an UNBOUNDED one is a
// memory amplifier on an unauthenticated endpoint, and every byte of it is
// copied through the request pipeline before hashing. 1024 is far past any
// real passphrase and far short of anything that costs the process something.
//
// # Why the minimum is 12 rather than 8
//
// HarborMaster fronts a socket that can stop containers. Eight characters is
// the historical default from an era of very different offline-cracking
// economics, and Argon2id's cost buys back some of that gap but not all of it.
// Twelve is the shortest length at which a passphrase is more natural to choose
// than a mangled word, which is the actual security benefit.
const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 1024
)

// PasswordProblem classifies why a password was refused.
//
// A closed vocabulary so the UI can explain the specific problem without the
// server assembling prose from the password itself -- which would echo it into
// a response.
type PasswordProblem string

// Password problems.
const (
	PasswordOK PasswordProblem = ""
	// PasswordTooShort is the common case.
	PasswordTooShort PasswordProblem = "tooShort"
	// PasswordTooLong is the bound above.
	PasswordTooLong PasswordProblem = "tooLong"
	// PasswordNotPrintable means it contains control characters. A password
	// with an embedded newline is almost always a paste accident, and it would
	// be untypeable afterwards.
	PasswordNotPrintable PasswordProblem = "notPrintable"
	// PasswordTooCommon means it is on the refusal list below.
	PasswordTooCommon PasswordProblem = "tooCommon"
	// PasswordContainsUsername means it contains, or is contained by, the
	// account name. The single most guessable password for a known account.
	PasswordContainsUsername PasswordProblem = "containsUsername"
	// PasswordTooUniform means it is one repeated character or a simple
	// ascending run, which reaches the length bound without adding anything.
	PasswordTooUniform PasswordProblem = "tooUniform"
)

// Explain renders the problem in operator-facing words.
//
// Never includes the password or any part of it.
func (p PasswordProblem) Explain() string {
	switch p {
	case PasswordTooShort:
		return "the password must be at least 12 characters"
	case PasswordTooLong:
		return "the password is too long"
	case PasswordNotPrintable:
		return "the password must not contain control characters"
	case PasswordTooCommon:
		return "that password is one of the most commonly used ones and is refused"
	case PasswordContainsUsername:
		return "the password must not contain the username"
	case PasswordTooUniform:
		return "the password must not be a single repeated character or a simple sequence"
	default:
		return ""
	}
}

// refusedPasswords are values that must never protect a Docker socket.
//
// Deliberately SHORT rather than a bundled corpus. A million-entry list would
// add a megabyte to the binary and a lookup to every password change to catch
// what an attacker's first thousand guesses would catch anyway; what this list
// is for is the specific failure of a default or placeholder credential
// surviving into production. Every entry is at least twelve characters, because
// anything shorter is already refused by length.
var refusedPasswords = map[string]struct{}{
	"password1234":     {},
	"password12345":    {},
	"passw0rd1234":     {},
	"administrator":    {},
	"harbormaster":     {},
	"harbormaster1":    {},
	"harbormaster123":  {},
	"changeme1234":     {},
	"changemeplease":   {},
	"letmein12345":     {},
	"qwertyuiop123":    {},
	"123456789012":     {},
	"1234567890123":    {},
	"welcome12345":     {},
	"dockerdocker":     {},
	"adminadmin123":    {},
	"defaultpassword":  {},
	"temporarypasswd":  {},
	"correcthorse":     {},
	"iloveyou1234":     {},
	"trustno1trustno1": {},
}

// CheckPassword validates a candidate password against the account it protects.
//
// # What it does not do
//
// It does not require a character-class mixture. Composition rules push people
// toward "Password1!" -- predictable, and no stronger than a longer passphrase.
// Length plus a refusal list plus the username check catches the passwords that
// actually get chosen, without teaching anyone to append an exclamation mark.
//
// # It never echoes the password
//
// The return value is a classification. Nothing derived from the password
// reaches a caller, a log, or a response.
func CheckPassword(password, username string) PasswordProblem {
	if len(password) < MinPasswordBytes {
		return PasswordTooShort
	}
	if len(password) > MaxPasswordBytes {
		return PasswordTooLong
	}
	if !utf8.ValidString(password) {
		// Invalid UTF-8 would be mangled on its way through a JSON round trip
		// and become untypeable. Reported as unprintable, which is what it is
		// from the operator's point of view.
		return PasswordNotPrintable
	}
	for _, r := range password {
		if unicode.IsControl(r) {
			return PasswordNotPrintable
		}
	}

	folded := strings.ToLower(password)
	if _, refused := refusedPasswords[folded]; refused {
		return PasswordTooCommon
	}

	if username != "" {
		name := NormaliseUsername(username)
		if len(name) >= 3 && strings.Contains(folded, name) {
			return PasswordContainsUsername
		}
	}

	if uniformPassword(password) {
		return PasswordTooUniform
	}
	return PasswordOK
}

// uniformPassword reports whether a password is one repeated character or a
// simple ascending or descending run.
//
// Both reach any length bound without adding entropy, so a length-only rule
// would accept "aaaaaaaaaaaa" and "abcdefghijkl".
func uniformPassword(password string) bool {
	runes := []rune(password)
	if len(runes) < 2 {
		return true
	}

	same := true
	ascending := true
	descending := true
	for i := 1; i < len(runes); i++ {
		if runes[i] != runes[0] {
			same = false
		}
		if runes[i] != runes[i-1]+1 {
			ascending = false
		}
		if runes[i] != runes[i-1]-1 {
			descending = false
		}
	}
	return same || ascending || descending
}
