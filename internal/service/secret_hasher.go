package service

import (
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Hasher converts sensitive values into digests.
//
// The raw value exists only for the duration of a Digest call. Nothing here
// retains it, copies it, or logs it: the value arrives as an argument, is fed
// to the MAC, and goes out of scope.
type Hasher struct {
	key SecretKey
}

// NewHasher builds a Hasher over an installation key.
func NewHasher(key SecretKey) *Hasher { return &Hasher{key: key} }

// KeyID reports which key this Hasher digests under. Safe to store and log.
func (h *Hasher) KeyID() string {
	if h == nil {
		return ""
	}
	return h.key.KeyID
}

// Digest hashes a sensitive value.
//
// The variable's NAME is deliberately not part of the input. Two variables
// holding the same secret produce the same digest, which is what makes a
// credential copied across services visible as such -- and which means renaming
// a variable does not read as "the secret changed".
func (h *Hasher) Digest(value string) domain.SecretDigest {
	return domain.SecretDigest{
		Present:   true,
		Length:    len(value),
		Digest:    h.key.HMAC(value),
		Algorithm: domain.DigestHMACSHA256,
		KeyID:     h.key.KeyID,
	}
}

// Absent describes a variable that is not set.
//
// Distinct from a variable set to the empty string, which gets a real digest:
// "unset" and "set to nothing" are different configurations, and a future
// restore must not conflate them.
func (h *Hasher) Absent() domain.SecretDigest {
	return domain.SecretDigest{
		Present:   false,
		Algorithm: domain.DigestHMACSHA256,
		KeyID:     h.key.KeyID,
	}
}
