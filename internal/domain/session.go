package domain

import (
	"crypto/rand"
	"encoding/base64"
	"time"
)

// Sessions.
//
// # Server-side, opaque, and digested at rest
//
// A session token is 32 bytes from the system entropy source, base64url
// encoded, and sent to the browser in a cookie. What HarborMaster STORES is a
// keyed digest of it, under the installation key -- so a stolen database
// yields no usable session, and a stolen database plus the key still yields
// only the ability to verify a token somebody already has.
//
// There is deliberately no JWT, no signed cookie, and no client-side claim.
// A self-contained token cannot be revoked without a server-side denylist,
// which is a session table with extra steps and worse failure modes: HarborMaster
// needs revocation on logout, password change, disablement, and role change,
// and a lookup that is already happening is the cheapest way to have it.
//
// # The CSRF token is derived, never stored
//
// See CSRFToken below. Storing a CSRF token in the clear would make it the one
// authentication secret a database thief could read directly.

// Session token and identifier sizes.
const (
	// SessionTokenBytes is the raw entropy in a session token. 256 bits, which
	// is not guessable within the lifetime of the universe and costs nothing.
	SessionTokenBytes = 32
	// SessionIDPrefix and SessionIDHexLength describe the PUBLIC identifier,
	// which is what an administrator sees in a session list and what a
	// revocation request names. It is NOT the token: knowing it grants nothing.
	SessionIDPrefix    = "ses_"
	SessionIDHexLength = 20
)

// SessionRevocation records why a session ended.
//
// A closed vocabulary, kept because "why am I logged out" is the question an
// operator asks, and because a session revoked by a role change is a different
// security event from one that simply expired.
type SessionRevocation string

// Revocation reasons.
const (
	SessionActiveReason SessionRevocation = ""
	// SessionLoggedOut is an operator ending their own session.
	SessionLoggedOut SessionRevocation = "loggedOut"
	// SessionPasswordChanged revokes every session on a credential change.
	SessionPasswordChanged SessionRevocation = "passwordChanged"
	// SessionRoleChanged revokes on a privilege change, so an in-flight
	// session cannot keep permissions it no longer has.
	SessionRoleChanged SessionRevocation = "roleChanged"
	// SessionUserDisabled revokes when an account is disabled.
	SessionUserDisabled SessionRevocation = "userDisabled"
	// SessionRevokedByAdmin is an administrator ending someone else's session.
	SessionRevokedByAdmin SessionRevocation = "revokedByAdmin"
	// SessionSuperseded is the old session ending because a new one replaced
	// it -- rotation on login, and the per-user session cap.
	SessionSuperseded SessionRevocation = "superseded"
	// SessionExpired is idle or absolute expiry.
	SessionExpired SessionRevocation = "expired"
)

// SessionRevocations lists every reason.
var SessionRevocations = []SessionRevocation{
	SessionLoggedOut, SessionPasswordChanged, SessionRoleChanged,
	SessionUserDisabled, SessionRevokedByAdmin, SessionSuperseded,
	SessionExpired,
}

// ValidSessionRevocation reports whether name is a known reason.
func ValidSessionRevocation(name string) bool {
	if name == "" {
		return true
	}
	for _, reason := range SessionRevocations {
		if string(reason) == name {
			return true
		}
	}
	return false
}

// Explain renders the reason in operator-facing words.
func (r SessionRevocation) Explain() string {
	switch r {
	case SessionLoggedOut:
		return "you signed out"
	case SessionPasswordChanged:
		return "the account's password was changed"
	case SessionRoleChanged:
		return "the account's role was changed"
	case SessionUserDisabled:
		return "the account was disabled"
	case SessionRevokedByAdmin:
		return "an administrator ended this session"
	case SessionSuperseded:
		return "a newer session replaced this one"
	case SessionExpired:
		return "the session expired"
	default:
		return "the session is no longer valid"
	}
}

// Session is one authenticated browser or API session.
//
// Note what is absent: the token. Only its digest is persisted, and the digest
// is `json:"-"` for the same reason SecretDigest's is -- it cannot reach an API
// response by accident.
type Session struct {
	ID int64 `json:"-"`
	// SessionID is the public identifier. Safe to display and to name in a
	// revocation request; grants nothing.
	SessionID string `json:"sessionId"`

	UserID string `json:"userId"`
	// Username and Role are SNAPSHOTS taken when the session was issued.
	//
	// Recorded for display in a session list. Authorization never reads them:
	// the middleware re-reads the user on every request, so a role change takes
	// effect immediately rather than at the next login.
	Username string `json:"username"`
	Role     Role   `json:"role"`

	// TokenDigest is the keyed digest of the session token. NEVER serialised.
	TokenDigest string `json:"-"`

	CreatedAt time.Time `json:"createdAt"`
	// LastSeenAt drives idle expiry. Written at most once per
	// SessionTouchInterval so a busy session does not write on every request.
	LastSeenAt time.Time `json:"lastSeenAt"`
	// IdleExpiresAt and AbsoluteExpiresAt are both enforced. The first bounds
	// an abandoned session; the second bounds a stolen one that is being kept
	// warm.
	IdleExpiresAt     time.Time `json:"idleExpiresAt"`
	AbsoluteExpiresAt time.Time `json:"absoluteExpiresAt"`

	RevokedAt  *time.Time        `json:"revokedAt,omitempty"`
	Revocation SessionRevocation `json:"revocation,omitempty"`

	// UserAgent is a bounded, sanitised note for the session list, so an
	// operator can tell one of their own sessions from another. Truncated hard:
	// it is attacker-controlled text that reaches a browser.
	UserAgent string `json:"userAgent,omitempty"`
	// ClientAddr is the normalised source address. See NormaliseClientAddr for
	// what "normalised" means and why the port is dropped.
	ClientAddr string `json:"clientAddr,omitempty"`

	// Current marks the session making the request, in a session list. Derived
	// at read time; never stored.
	Current bool `json:"current,omitempty"`
}

// Active reports whether the session may still authenticate a request.
//
// Every condition is checked here, in one place, so a caller cannot satisfy
// itself with two of the three. `now` is passed rather than read so the check
// is deterministic in tests and uses the same clock as the caller's other
// decisions.
func (s Session) Active(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	if !now.Before(s.IdleExpiresAt) {
		return false
	}
	return now.Before(s.AbsoluteExpiresAt)
}

// ExpiryReason reports why an inactive session is inactive.
func (s Session) ExpiryReason(now time.Time) SessionRevocation {
	switch {
	case s.RevokedAt != nil:
		return s.Revocation
	case !now.Before(s.IdleExpiresAt), !now.Before(s.AbsoluteExpiresAt):
		return SessionExpired
	default:
		return SessionActiveReason
	}
}

// MaxUserAgentBytes bounds the stored user-agent note.
const MaxUserAgentBytes = 120

// NewSessionToken generates a session token.
//
// base64url without padding: URL- and cookie-safe, and shorter than hex for
// the same entropy. Returned to the caller ONCE, put in a cookie, and never
// stored -- only its keyed digest is.
func NewSessionToken() (string, error) {
	raw := make([]byte, SessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ValidSessionToken reports whether a value has the SHAPE of a session token.
//
// A cheap rejection before the value reaches a digest and a database lookup:
// a cookie is attacker-controlled, and there is no reason to hash a megabyte
// of it. It says nothing about whether the token is real.
func ValidSessionToken(token string) bool {
	// 32 bytes base64url without padding is exactly 43 characters.
	const encodedLength = 43
	if len(token) != encodedLength {
		return false
	}
	for i := 0; i < len(token); i++ {
		char := token[i]
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '-', char == '_':
		default:
			return false
		}
	}
	return true
}

// NewSessionID generates a public session identifier.
func NewSessionID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return SessionIDPrefix + encodeHex(raw[:])
}

// ValidSessionID reports whether id has the shape of a generated identifier.
func ValidSessionID(id string) bool {
	if len(id) != len(SessionIDPrefix)+SessionIDHexLength {
		return false
	}
	if id[:len(SessionIDPrefix)] != SessionIDPrefix {
		return false
	}
	for _, r := range id[len(SessionIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// encodeHex renders bytes as lowercase hex without importing encoding/hex into
// every file that needs an identifier.
func encodeHex(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(raw)*2)
	for i, b := range raw {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return string(out)
}
