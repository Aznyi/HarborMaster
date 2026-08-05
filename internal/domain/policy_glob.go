package domain

import (
	"strings"
	"unicode/utf8"
)

// Bounded glob matching for policy patterns.
//
// # Why not regular expressions
//
// A policy pattern is administrator-supplied input to an unauthenticated API,
// so the matcher it feeds is an attack surface and is designed as one.
//
// Go's regexp package is RE2, which is linear in the subject and has no
// catastrophic backtracking, so the classic "regex denial of service" does not
// arise there either. Globs are still the better choice: they are what an
// operator actually wants for an image reference, they have no construct whose
// cost is non-obvious, and the entire syntax fits in the two characters below.
// Nothing in HarborMaster compiles a caller-supplied regular expression.
//
// # The syntax, in full
//
//	*  matches any run of characters, including none, including '/'
//	?  matches exactly one character
//
// That is the whole language. Character classes are deliberately ABSENT: they
// are where a glob implementation acquires its error cases and its ambiguous
// corners, and an allowlist of two metacharacters is easier to reason about
// than a denylist of the rest. Every other byte, including '[', ']' and '\\',
// matches itself literally.
//
// # Why the matcher cannot blow up
//
// MatchGlob is ITERATIVE and remembers exactly one backtrack point. It never
// recurses, so there is no exponential path -- which is the failure mode the
// phrase "regex DoS" names. Its worst case is proportional to the pattern
// length times the subject length, and both are bounded: patterns by
// ValidatePolicyPattern below, subjects by MaxPolicySubjectBytes. A pattern
// that exceeds either is refused at write time rather than trimmed at match
// time, so an expensive pattern never reaches the database.

// Pattern and subject bounds.
//
// These are the numbers the worst case is computed from, so they are stated
// together rather than scattered. A match costs at most
// MaxPolicyPatternBytes * MaxPolicySubjectBytes byte comparisons -- about
// 65,000, or tens of microseconds -- and the number of matches per container is
// bounded by the rule and value caps in PolicyLimits.
const (
	// MaxPolicyPatternBytes bounds one pattern. Generous for an image
	// reference, a label key, or an environment variable name, all of which
	// are far shorter in practice.
	MaxPolicyPatternBytes = 128
	// MaxPolicyWildcards bounds how many '*' one pattern may carry.
	//
	// This does not change the matcher's worst-case bound, which the length
	// caps already fix; it rejects the shape of pattern that only exists to be
	// expensive, and a legitimate pattern has one or two wildcards.
	MaxPolicyWildcards = 8
	// MaxPolicySubjectBytes bounds what a pattern is matched AGAINST. A
	// subject longer than this cannot match a pattern with wildcards, so the
	// comparison is refused rather than run: an image reference or variable
	// name this long is malformed, not a near miss.
	MaxPolicySubjectBytes = 512
)

// MatchGlob reports whether subject matches pattern.
//
// Case-sensitive: image references, label keys and environment variable names
// are all case-sensitive to Docker, and folding them here would make a policy
// match things the daemon considers different. Callers that need folding
// normalise the subject before calling -- capability rules do exactly that.
//
// A subject longer than MaxPolicySubjectBytes never matches. That is a refusal
// rather than a truncation: truncating would let a long value match a pattern
// its full form does not.
func MatchGlob(pattern, subject string) bool {
	if len(subject) > MaxPolicySubjectBytes || len(pattern) > MaxPolicyPatternBytes {
		return false
	}
	// The overwhelmingly common case: a literal with no metacharacters.
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == subject
	}

	var (
		s, p int
		// star is the position of the most recent '*' in the pattern, and mark
		// the subject position it was first tried at. Exactly ONE backtrack
		// point is retained, which is what keeps this iterative rather than
		// recursive and therefore bounded rather than exponential.
		star = -1
		mark int
	)

	for s < len(subject) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == subject[s]):
			s++
			p++
		case p < len(pattern) && pattern[p] == '*':
			star = p
			p++
			mark = s
		case star >= 0:
			// The run consumed by the last '*' grows by one and matching
			// resumes after it. mark only ever advances, so the total work is
			// bounded by len(subject) restarts.
			p = star + 1
			mark++
			s = mark
		default:
			return false
		}
	}

	// Trailing wildcards may still match the empty remainder.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// MatchAnyGlob reports whether subject matches any of the patterns, and returns
// the first pattern that matched.
func MatchAnyGlob(patterns []string, subject string) (string, bool) {
	for _, pattern := range patterns {
		if MatchGlob(pattern, subject) {
			return pattern, true
		}
	}
	return "", false
}

// ValidatePolicyPattern checks one pattern at write time.
//
// Everything expensive or ambiguous is refused here rather than handled at
// match time, so a pattern that reached the database is one the matcher can
// evaluate cheaply. The checks are, in order: non-empty, byte-bounded, valid
// UTF-8, no control characters, and a bounded number of wildcards.
//
// Returns a message naming the constraint, never echoing the pattern: an error
// message is not the place to reflect caller input.
func ValidatePolicyPattern(pattern string) string {
	switch {
	case pattern == "":
		return "must not be empty"
	case len(pattern) > MaxPolicyPatternBytes:
		return "must be at most 128 bytes"
	case !utf8.ValidString(pattern):
		return "must be valid UTF-8"
	}

	for _, r := range pattern {
		// Rejected rather than stripped. A control character in a pattern is
		// either a mistake or an attempt to make a stored value render as
		// something else in a log line or a UI.
		if r < 0x20 || r == 0x7f {
			return "must not contain control characters"
		}
	}

	if strings.Count(pattern, "*") > MaxPolicyWildcards {
		return "must contain at most 8 wildcards"
	}
	return ""
}
