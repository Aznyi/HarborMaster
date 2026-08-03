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
var DefaultMaskPatterns = []string{
	"PASSWORD",
	"PASSWD",
	"SECRET",
	"TOKEN",
	"API_KEY",
	"APIKEY",
	"PRIVATE_KEY",
	"CREDENTIAL",
	"AUTH",
}

// Masker classifies environment variables and log options by name.
//
// The bias is deliberately towards over-masking: "AUTHOR" matches the AUTH
// pattern and gets masked. A masked non-secret is a small annoyance, while a
// leaked secret is not recoverable, and this runs on data from a privileged
// socket with no authentication in front of it.
type Masker struct {
	patterns []string
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
	return &Masker{patterns: normalized}
}

// NewDefaultMasker builds a Masker with DefaultMaskPatterns.
func NewDefaultMasker() *Masker { return NewMasker(DefaultMaskPatterns) }

// Patterns returns the configured patterns. Names only -- these are not
// secrets, and reporting them lets an operator confirm the policy in effect.
func (m *Masker) Patterns() []string {
	out := make([]string, len(m.patterns))
	copy(out, m.patterns)
	return out
}

// IsSensitive reports whether a name matches any pattern.
func (m *Masker) IsSensitive(name string) bool {
	if m == nil {
		return false
	}
	upper := strings.ToUpper(name)
	for _, pattern := range m.patterns {
		if strings.Contains(upper, pattern) {
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
		return EnvVar{
			Name:        name,
			Value:       MaskedValue,
			Sensitivity: SensitivitySensitive,
			RawValue:    value,
		}
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
