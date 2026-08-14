package domain

import (
	"sort"
	"strings"
)

// DefaultMaskPatterns are the variable-name fragments treated as secret-bearing.
//
// Matching is on the NAME, never the value. Scanning values for things that
// look like secrets produces both false positives (any long random string) and
// false negatives (a short password), and it would mean inspecting every value
// -- exactly what a masking layer is supposed to avoid.
//
// The list covers two categories. The first names the secret directly
// (PASSWORD, TOKEN, API_KEY). The second names a DESTINATION whose value
// routinely embeds a credential: DATABASE_URL=postgres://user:hunter2@db/app
// carries a password in a variable whose name contains none of the first
// category's fragments, and it is one of the most common secret-bearing
// variables in existence.
//
// Matching a destination name over-masks -- HEALTHCHECK_URL is not a secret --
// and that is the correct bias. A masked non-secret is an annoyance; a leaked
// secret is not recoverable.
var DefaultMaskPatterns = []string{
	// Named secrets.
	"PASSWORD",
	"PASSWD",
	"PASSPHRASE",
	"SECRET",
	"TOKEN",
	"API_KEY",
	"APIKEY",
	"ACCESS_KEY",
	"SECRET_KEY",
	"PRIVATE_KEY",
	"PRIVATE",
	"CREDENTIAL",
	"CREDENTIALS",
	"AUTH",
	"SALT",
	"SIGNING",
	"SESSION",
	"COOKIE",
	"CERT",
	"CERTIFICATE",

	// Destinations whose values embed credentials.
	"DSN",
	"CONNECTION_STRING",
	"CONNECTIONSTRING",
	"URL",
	"URI",
	"WEBHOOK",
	"SMTP",
	"SERVICE_ACCOUNT",
	"SERVICEACCOUNT",
}

// MaskMode selects the classification policy.
type MaskMode string

// Mask modes.
const (
	// MaskModeDefault classifies by name pattern.
	MaskModeDefault MaskMode = "default"
	// MaskModeAll treats every variable as sensitive regardless of name.
	//
	// The correct setting for an operator who cannot enumerate their variables,
	// and the honest one for a compliance context: no value is stored, and
	// everything gets a digest. It costs diff readability for non-secret
	// variables, which is a trade the operator opts into deliberately.
	MaskModeAll MaskMode = "all-sensitive"
)

// ValidMaskMode reports whether s names a known mode.
func ValidMaskMode(s string) bool {
	switch MaskMode(s) {
	case MaskModeDefault, MaskModeAll:
		return true
	default:
		return false
	}
}

// foldName normalizes a variable name for matching.
//
// Separator characters collapse to underscores so DB-URL, db.url, DB/URL, and
// DB URL all reach the same comparison. The stripped form drops separators
// entirely so DBURL matches a DB_URL pattern. Callers match against both:
// operators write the same variable five different ways, and a classifier that
// only recognises one of them is a classifier with four blind spots.
func foldName(name string) (folded, stripped string) {
	upper := strings.ToUpper(name)

	var withSeparators, withoutSeparators strings.Builder
	withSeparators.Grow(len(upper))
	withoutSeparators.Grow(len(upper))

	for _, r := range upper {
		switch r {
		case '-', '.', '/', '\\', ' ', '_', ':':
			withSeparators.WriteByte('_')
		default:
			withSeparators.WriteRune(r)
			withoutSeparators.WriteRune(r)
		}
	}
	return withSeparators.String(), withoutSeparators.String()
}

// Masker classifies environment variables and log options by name.
//
// The bias is deliberately towards over-masking: "AUTHOR" matches the AUTH
// pattern and gets masked. A masked non-secret is a small annoyance, while a
// leaked secret is not recoverable, and this runs on data from a privileged
// socket with no authentication in front of it.
type Masker struct {
	// patterns are the configured fragments, reported verbatim to operators.
	patterns []string
	// folded and stripped are the same fragments prepared for matching against
	// the two normalized forms of a variable name.
	folded   []string
	stripped []string
	mode     MaskMode

	// digester produces the keyed comparison evidence for a sensitive value.
	//
	// Optional, and nil in every construction that does not have an
	// installation key -- configuration parsing, tests of the classification
	// rules themselves. A nil digester leaves the evidence fields empty, and an
	// empty algorithm is not Comparable() with anything, so the downstream
	// answer is "cannot be compared" rather than a token that would match
	// another unkeyed one.
	digester EnvDigester
}

// EnvDigester produces keyed comparison evidence for a sensitive value.
//
// A function rather than the key itself, deliberately: the packages that
// classify environment variables handle raw values in memory but must never
// hold the installation key, and this hands them the one operation they need
// without the material behind it.
type EnvDigester func(value string) SecretDigest

// WithDigester returns a Masker that records comparison evidence.
//
// Separate from construction because the Masker is built from configuration,
// which is parsed before the installation key is loaded. The composition root
// attaches the digester once both exist.
func (m *Masker) WithDigester(digester EnvDigester) *Masker {
	if m == nil {
		return nil
	}
	clone := *m
	clone.digester = digester
	return &clone
}

// NewMasker builds a Masker from name fragments. Patterns are upper-cased and
// de-duplicated; empty entries are dropped. An empty pattern list yields a
// Masker that masks nothing, which is a valid, explicitly configured choice.
func NewMasker(patterns []string) *Masker {
	seen := make(map[string]struct{}, len(patterns))
	normalized := make([]string, 0, len(patterns))

	for _, pattern := range patterns {
		trimmed := strings.ToUpper(strings.TrimSpace(pattern))
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}

	// Sorted so the pattern list is deterministic wherever it is reported.
	sort.Strings(normalized)

	m := &Masker{
		patterns: normalized,
		folded:   make([]string, 0, len(normalized)),
		stripped: make([]string, 0, len(normalized)),
		mode:     MaskModeDefault,
	}
	// Patterns are folded once at construction rather than on every lookup:
	// IsSensitive runs per variable per container per refresh.
	for _, pattern := range normalized {
		folded, stripped := foldName(pattern)
		m.folded = append(m.folded, folded)
		if stripped != "" {
			m.stripped = append(m.stripped, stripped)
		}
	}
	return m
}

// NewDefaultMasker builds a Masker with DefaultMaskPatterns.
func NewDefaultMasker() *Masker { return NewMasker(DefaultMaskPatterns) }

// NewMaskerWithExtra merges additional fragments with the defaults.
//
// Additive on purpose: this is the setting an operator reaches for to cover a
// variable HarborMaster does not know about, and it must not be able to drop
// the defaults as a side effect. Replacing them requires the explicit override
// path, which warns.
func NewMaskerWithExtra(extra []string) *Masker {
	merged := make([]string, 0, len(DefaultMaskPatterns)+len(extra))
	merged = append(merged, DefaultMaskPatterns...)
	merged = append(merged, extra...)
	return NewMasker(merged)
}

// NewMaskerWithMode builds a Masker with an explicit mode.
//
// A nil pattern list means the defaults; an empty non-nil list means "no
// patterns", which is a different, explicitly configured choice.
func NewMaskerWithMode(patterns []string, mode MaskMode) *Masker {
	if patterns == nil {
		patterns = DefaultMaskPatterns
	}
	m := NewMasker(patterns)
	if ValidMaskMode(string(mode)) {
		m.mode = mode
	}
	return m
}

// Mode reports the configured classification mode.
func (m *Masker) Mode() MaskMode {
	if m == nil {
		return MaskModeDefault
	}
	return m.mode
}

// Patterns returns the configured patterns. Names only -- these are not
// secrets, and reporting them lets an operator confirm the policy in effect.
func (m *Masker) Patterns() []string {
	out := make([]string, len(m.patterns))
	copy(out, m.patterns)
	return out
}

// IsSensitive reports whether a name should be treated as secret-bearing.
//
// In MaskModeAll every name is sensitive, including the empty one: the mode
// exists to remove the judgement call entirely, so there is no exception.
func (m *Masker) IsSensitive(name string) bool {
	if m == nil {
		return false
	}
	if m.mode == MaskModeAll {
		return true
	}

	folded, stripped := foldName(name)
	for _, pattern := range m.folded {
		if strings.Contains(folded, pattern) {
			return true
		}
	}
	for _, pattern := range m.stripped {
		if strings.Contains(stripped, pattern) {
			return true
		}
	}
	return false
}

// Classify builds an EnvVar with Value already masked where required.
//
// RawValue always carries the real value; it is never serialised (see EnvVar).
func (m *Masker) Classify(name, value string) EnvVar {
	if m.IsSensitive(name) {
		sensitive := EnvVar{
			Name:        name,
			Value:       MaskedValue,
			Sensitivity: SensitivitySensitive,
			RawValue:    value,
		}
		// THE DIGEST IS TAKEN HERE, and here is the only place it can be.
		//
		// This is the instant a value is both present and known to be
		// sensitive. A few frames later it crosses the persistence boundary and
		// RawValue is gone for good, which is exactly what is wanted for the
		// value and exactly wrong for the evidence about it.
		//
		// The masked placeholder is never the input: it is a presentation
		// string, identical for every secret, and digesting it is precisely the
		// defect this replaces.
		if m.digester != nil {
			evidence := m.digester(value)
			sensitive.Digest = evidence.Digest
			sensitive.DigestAlgorithm = evidence.Algorithm
			sensitive.DigestKeyID = evidence.KeyID
			sensitive.ValueLength = evidence.Length
		}
		return sensitive
	}
	return EnvVar{
		Name:        name,
		Value:       value,
		Sensitivity: SensitivityNormal,
		RawValue:    value,
	}
}

// ClassifyEnvironment normalizes a "NAME=value" list.
//
// A entry with no "=" is recorded with an empty value rather than dropped: the
// runtime accepted it, so the inventory should show it. Order is preserved,
// because environment order is semantically meaningful to some programs.
func (m *Masker) ClassifyEnvironment(entries []string) []EnvVar {
	vars := make([]EnvVar, 0, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			value = ""
		}
		vars = append(vars, m.Classify(name, value))
	}
	return vars
}

// ClassifyMap normalizes a map of options, sorted by key for determinism.
func (m *Masker) ClassifyMap(options map[string]string) []EnvVar {
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	vars := make([]EnvVar, 0, len(keys))
	for _, key := range keys {
		vars = append(vars, m.Classify(key, options[key]))
	}
	return vars
}
