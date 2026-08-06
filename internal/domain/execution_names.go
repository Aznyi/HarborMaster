package domain

import "strings"

// Container names during a recreation.
//
// # Why names are derived rather than chosen
//
// A recreation needs three names on the host: the one the replacement takes
// over (the original's), one to park the original under, and one to quarantine
// a failed replacement under. Two of those are names HarborMaster CREATES, and
// they are created against a privileged socket.
//
// So they are DERIVED, deterministically, from two values HarborMaster already
// controls: a container name it read from the daemon, and an execution id it
// generated itself from the system entropy source. There is no code path in
// which caller text becomes a container name, and the derivation is pure so a
// test can pin every character of the result.
//
// # Why the execution id is in the name
//
// Two recreations of the same container -- one that failed and was left parked,
// one attempted later -- must not collide. Including the execution id makes the
// parked name unique per attempt, and makes the container on the host traceable
// to the audit record that created it without consulting anything else.
//
// # Why the names are validated anyway
//
// Derivation makes the output safe by construction. Validation makes it safe by
// evidence, and the two are worth keeping separate: a future edit to the
// derivation that produced something illegal would be caught here rather than
// by the daemon, which is the difference between a refusal before the mutation
// point and a failure after it.

// Name suffixes. Fixed literals; nothing about them varies with input.
const (
	// ParkedNameSuffix marks the original, stopped and renamed aside so the
	// replacement can take its name. It is NOT removed until the replacement is
	// fully proved.
	ParkedNameSuffix = ".hm-old-"
	// QuarantineNameSuffix marks a replacement that failed verification. It is
	// stopped and renamed rather than removed, because it is the evidence an
	// operator needs to work out why the new image did not work.
	QuarantineNameSuffix = ".hm-failed-"
)

// MaxContainerNameBytes bounds a name HarborMaster will create.
//
// Docker itself is more permissive, but a container name becomes a DNS label on
// user-defined networks, and a name long enough to be truncated somewhere in
// that chain is a name that resolves to something unexpected. Bounded well
// below anything that could happen.
const MaxContainerNameBytes = 200

// maxDerivedSuffixBytes is the longest suffix a derivation can add: the longer
// of the two markers plus a full execution id.
const maxDerivedSuffixBytes = len(QuarantineNameSuffix) + len(ExecutionIDPrefix) + ExecutionIDHexLength

// MaxRecreatableNameBytes is the longest ORIGINAL name a recreation can handle.
//
// A longer one would produce a parked or quarantine name past the bound, and
// silently truncating it would break the uniqueness the execution id provides.
// Refusing is the honest answer.
const MaxRecreatableNameBytes = MaxContainerNameBytes - maxDerivedSuffixBytes

// NormaliseContainerName strips the leading slash the Engine reports.
//
// The daemon returns names as "/web". Everything in HarborMaster stores and
// displays them without it, and a stray slash in a name sent BACK to the daemon
// is how a create request ends up meaning something other than intended.
func NormaliseContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

// ValidContainerName reports whether a name is one HarborMaster may use.
//
// An ALLOWLIST of Docker's own documented character set, and deliberately
// stricter at the first character. This value is sent to a privileged socket
// and appears in a URL, a log line, and a browser; anything outside the set is
// refused rather than escaped.
func ValidContainerName(name string) bool {
	if name == "" || len(name) > MaxContainerNameBytes {
		return false
	}

	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		case i == 0:
			// Docker requires an alphanumeric first character. A name beginning
			// with a dot or a dash is also the shape of a command-line flag,
			// which is a second reason not to allow it anywhere near an API
			// that some tooling shells out to.
			return false
		case r == '_', r == '.', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}

// RecreatableContainerName reports whether a name can survive a recreation.
//
// Checked in the PREFLIGHT, before anything is stopped. A name that cannot be
// parked is a container that cannot be safely recreated, and finding that out
// after the original is already stopped would be the worst possible moment.
func RecreatableContainerName(name string) bool {
	return ValidContainerName(name) && len(name) <= MaxRecreatableNameBytes
}

// ParkedContainerName derives the name the original is renamed to.
//
// Returns false when the inputs cannot produce a legal name, which the caller
// must treat as a refusal rather than fall back to a name of its own.
func ParkedContainerName(containerName, executionID string) (string, bool) {
	return derivedName(containerName, ParkedNameSuffix, executionID)
}

// QuarantineContainerName derives the name a failed replacement is renamed to.
func QuarantineContainerName(containerName, executionID string) (string, bool) {
	return derivedName(containerName, QuarantineNameSuffix, executionID)
}

// derivedName builds and re-validates one derived name.
//
// Both inputs are re-checked here even though every caller has checked them
// already. This is the last point before a name reaches a privileged socket,
// and it is the only one that cannot be bypassed by a new caller.
func derivedName(containerName, suffix, executionID string) (string, bool) {
	name := NormaliseContainerName(containerName)
	if !RecreatableContainerName(name) {
		return "", false
	}
	if !ValidExecutionID(executionID) {
		return "", false
	}

	derived := name + suffix + executionID
	// Belt and braces: the length arithmetic above guarantees this, and a
	// future edit to either constant could stop guaranteeing it.
	if !ValidContainerName(derived) {
		return "", false
	}
	return derived, true
}

// IsHarborMasterDerivedName reports whether a name was produced by this file.
//
// Used by the UI and by diagnostics to explain an unfamiliar container on the
// host. It is a display aid and never a security decision: a container an
// operator named this way themselves would match too, which is exactly why
// nothing acts on the strength of it.
func IsHarborMasterDerivedName(name string) bool {
	normalised := NormaliseContainerName(name)
	for _, suffix := range []string{ParkedNameSuffix, QuarantineNameSuffix} {
		index := strings.LastIndex(normalised, suffix)
		if index <= 0 {
			continue
		}
		if ValidExecutionID(normalised[index+len(suffix):]) {
			return true
		}
	}
	return false
}
