package domain

import (
	"strings"
	"unicode/utf8"
)

// Bounded display text.
//
// # Why this exists
//
// Some strings HarborMaster shows an operator did not originate in HarborMaster.
// A Docker pull's progress status is relayed by the daemon but INFLUENCED BY THE
// REGISTRY, which is a third party. Such a string reaches a log line, a database
// column, an API response, and a browser, and each of those has a different way
// of going wrong: a newline forges a log entry, a control character corrupts a
// terminal, an unbounded string fills a column, and invalid UTF-8 makes the JSON
// encoder replace bytes silently.
//
// Rather than defend at each of those four points, the value is made SAFE ONCE,
// at the boundary where it enters HarborMaster.
//
// # What this is not
//
// It is not an HTML escaper and must not be relied on as one. Output encoding
// belongs to the renderer -- the JSON encoder escapes what it writes, and React
// escapes what it renders. This function makes a string bounded and printable;
// it does not make an unsafe rendering path safe.

// SanitiseDisplayText bounds and cleans third-party text for display.
//
// Truncated to limit BYTES, stripped of control characters, and guaranteed
// valid UTF-8. An empty result is returned for anything that survives none of
// that, because an empty status is honest where a mangled one is not.
//
// Truncation is on a rune boundary: cutting mid-sequence would produce exactly
// the invalid UTF-8 this is supposed to prevent.
func SanitiseDisplayText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > limit {
		trimmed = trimmed[:limit]
		// Back off to the last complete rune, so a multi-byte character cut in
		// half does not become a replacement character downstream.
		for len(trimmed) > 0 && !utf8.ValidString(trimmed) {
			trimmed = trimmed[:len(trimmed)-1]
		}
	}

	var builder strings.Builder
	builder.Grow(len(trimmed))
	for _, r := range trimmed {
		switch {
		case r == utf8.RuneError:
			// Invalid input rather than a legitimate replacement character;
			// dropped rather than preserved.
			continue
		case r == '\t', r == '\n', r == '\r':
			// Whitespace that carries structural meaning in a log line becomes
			// an ordinary space. Dropping it entirely would run words together.
			builder.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Other C0 controls and DEL: no display meaning, and several have
			// terminal side effects.
			continue
		case r >= 0x80 && r <= 0x9f:
			// C1 controls, which some terminals still interpret.
			continue
		default:
			builder.WriteRune(r)
		}
	}

	return strings.TrimSpace(builder.String())
}
