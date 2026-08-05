package domain

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Policy validation.
//
// A policy definition arrives from an unauthenticated API, so it is validated
// as hostile input rather than as a form an administrator filled in. Everything
// below is an ALLOWLIST: a rule type must be in the catalogue, a restart policy
// must be one of Docker's four, a capability must have the shape of a
// capability, and a pattern must satisfy the bounds the matcher's worst case is
// computed from.
//
// No validation message ever echoes the offending value. A message names the
// field and the constraint, which is everything the caller needs and nothing an
// attacker can use to make the response reflect their input.

// PolicyLimits bounds one policy definition.
//
// Passed in rather than read from a package-level variable so the API layer's
// configured limits are the ones enforced, and so a test can shrink them
// without mutating global state.
type PolicyLimits struct {
	MaxRules            int `json:"maxRules"`
	MaxValuesPerRule    int `json:"maxValuesPerRule"`
	MaxNameBytes        int `json:"maxNameBytes"`
	MaxDescriptionBytes int `json:"maxDescriptionBytes"`
}

// Default policy bounds.
//
// Generous for any real policy and small enough that a caller cannot make one
// evaluation expensive: the per-container cost of a policy is bounded by
// MaxRules * MaxValuesPerRule glob matches, each of which is itself bounded by
// the pattern and subject caps in policy_glob.go.
const (
	DefaultPolicyMaxRules            = 32
	DefaultPolicyMaxValuesPerRule    = 32
	DefaultPolicyMaxNameBytes        = 120
	DefaultPolicyMaxDescriptionBytes = 1000
)

// DefaultPolicyLimits returns the bounds used when none are configured.
func DefaultPolicyLimits() PolicyLimits {
	return PolicyLimits{
		MaxRules:            DefaultPolicyMaxRules,
		MaxValuesPerRule:    DefaultPolicyMaxValuesPerRule,
		MaxNameBytes:        DefaultPolicyMaxNameBytes,
		MaxDescriptionBytes: DefaultPolicyMaxDescriptionBytes,
	}
}

// normalise fills any unset bound with its default, so a partially configured
// limit set cannot accidentally mean "unbounded".
func (l PolicyLimits) normalise() PolicyLimits {
	defaults := DefaultPolicyLimits()
	if l.MaxRules < 1 {
		l.MaxRules = defaults.MaxRules
	}
	if l.MaxValuesPerRule < 1 {
		l.MaxValuesPerRule = defaults.MaxValuesPerRule
	}
	if l.MaxNameBytes < 1 {
		l.MaxNameBytes = defaults.MaxNameBytes
	}
	if l.MaxDescriptionBytes < 1 {
		l.MaxDescriptionBytes = defaults.MaxDescriptionBytes
	}
	return l
}

// PolicyValidationError names the field that failed and the constraint it
// broke. The offending value is deliberately absent.
type PolicyValidationError struct {
	Field   string
	Message string
}

func (e PolicyValidationError) Error() string { return e.Field + " " + e.Message }

// RestartPolicyNames is the closed vocabulary a restart-policy rule may name.
//
// Docker's four, plus the empty policy's canonical spelling. An allowlist
// rather than free text: a policy listing "Always" with a capital A would
// otherwise silently match nothing and read as an estate in perfect
// compliance.
var RestartPolicyNames = []string{"no", "always", "on-failure", "unless-stopped"}

// Normalise trims and deduplicates a definition in place.
//
// Called before Validate so the bounds apply to what will actually be stored:
// a rule carrying the same pattern twenty times must not consume twenty of its
// value budget, and trailing whitespace must not make two identical patterns
// look distinct.
func (p *PolicyDefinition) Normalise() {
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)

	rules := make([]PolicyRule, 0, len(p.Rules))
	for _, rule := range p.Rules {
		rule.Type = PolicyRuleType(strings.TrimSpace(string(rule.Type)))
		rule.Severity = PolicySeverity(strings.TrimSpace(string(rule.Severity)))

		seen := make(map[string]struct{}, len(rule.Values))
		values := make([]string, 0, len(rule.Values))
		for _, value := range rule.Values {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			// Capability names are compared after normalisation, so they are
			// stored normalised too. Otherwise "cap_sys_admin" and "SYS_ADMIN"
			// would occupy two slots and render as two distinct requirements.
			if spec, ok := policyRuleCatalogue[rule.Type]; ok && spec.ValueKind == ValueKindCapability {
				trimmed = NormaliseCapability(trimmed)
			}
			if _, duplicate := seen[trimmed]; duplicate {
				continue
			}
			seen[trimmed] = struct{}{}
			values = append(values, trimmed)
		}
		rule.Values = values
		rules = append(rules, rule)
	}
	p.Rules = rules
}

// Validate checks a definition against the limits and the catalogue.
//
// Returns the FIRST failure. A caller fixing one problem at a time is the
// normal case, and accumulating every complaint would mean building a response
// shape whose only consumer is an error path.
func (p PolicyDefinition) Validate(limits PolicyLimits) error {
	limits = limits.normalise()

	if err := validatePolicyText("name", p.Name, limits.MaxNameBytes, true); err != nil {
		return err
	}
	if err := validatePolicyText("description", p.Description, limits.MaxDescriptionBytes, false); err != nil {
		return err
	}
	if !ValidPolicySeverity(string(p.Severity)) {
		return PolicyValidationError{Field: "severity", Message: "must be one of critical, high, medium, low"}
	}

	switch {
	case len(p.Rules) == 0:
		// A policy with no rules can never fail and can never pass. Storing one
		// would put a row on the dashboard that reports compliance it never
		// checked.
		return PolicyValidationError{Field: "rules", Message: "must contain at least one rule"}
	case len(p.Rules) > limits.MaxRules:
		return PolicyValidationError{
			Field:   "rules",
			Message: "must contain at most " + strconv.Itoa(limits.MaxRules) + " rules",
		}
	}

	seen := make(map[PolicyRuleType]struct{}, len(p.Rules))
	for index, rule := range p.Rules {
		field := "rules[" + strconv.Itoa(index) + "]"

		spec, known := policyRuleCatalogue[rule.Type]
		if !known {
			return PolicyValidationError{Field: field + ".type", Message: "is not a known rule type"}
		}
		// One rule per type. A violation is identified by (container, policy,
		// rule type), so two rules of one type inside a policy would compete
		// for the same history row and the loser's failures would vanish. Two
		// such rules are also redundant: their value lists merge.
		if _, duplicate := seen[rule.Type]; duplicate {
			return PolicyValidationError{
				Field:   field + ".type",
				Message: "is already used by another rule in this policy; merge their values instead",
			}
		}
		seen[rule.Type] = struct{}{}

		if rule.Severity != "" && !ValidPolicySeverity(string(rule.Severity)) {
			return PolicyValidationError{
				Field:   field + ".severity",
				Message: "must be one of critical, high, medium, low",
			}
		}

		if err := validateRuleValues(field, spec, rule.Values, limits.MaxValuesPerRule); err != nil {
			return err
		}
	}
	return nil
}

// validateRuleValues applies the bounds and the per-kind shape checks.
func validateRuleValues(field string, spec PolicyRuleSpec, values []string, maxValues int) error {
	if !spec.RequiresValues {
		// Rejected rather than ignored. A caller that attached values to a
		// parameterless rule believes those values do something, and silently
		// dropping them is how someone ends up trusting a rule that is not
		// checking what they think it is.
		if len(values) > 0 {
			return PolicyValidationError{Field: field + ".values", Message: "must be empty for this rule type"}
		}
		return nil
	}

	switch {
	case len(values) == 0:
		return PolicyValidationError{Field: field + ".values", Message: "must contain at least one value"}
	case len(values) > maxValues:
		return PolicyValidationError{
			Field:   field + ".values",
			Message: "must contain at most " + strconv.Itoa(maxValues) + " values",
		}
	}

	for index, value := range values {
		valueField := field + ".values[" + strconv.Itoa(index) + "]"
		if message := validateRuleValue(spec.ValueKind, value); message != "" {
			return PolicyValidationError{Field: valueField, Message: message}
		}
	}
	return nil
}

// validateRuleValue checks one value against its kind, returning a message or
// the empty string.
func validateRuleValue(kind PolicyValueKind, value string) string {
	switch kind {
	case ValueKindImagePattern, ValueKindLabelPattern, ValueKindEnvPattern:
		return ValidatePolicyPattern(value)

	case ValueKindCapability:
		// Compared exactly, so a wildcard would silently match nothing and
		// leave an operator believing a capability was covered.
		if strings.ContainsAny(value, "*?") {
			return "must not contain wildcards; capabilities are matched exactly"
		}
		if value == "" || len(value) > 64 {
			return "must be between 1 and 64 characters"
		}
		// The shape of a capability name after normalisation. Not an allowlist
		// of names: the kernel gains capabilities over time and a stale list
		// would reject a legitimate new one.
		for _, r := range value {
			if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
				return "must contain only A-Z, 0-9 and underscore"
			}
		}
		return ""

	case ValueKindRestartPolicy:
		if !containsFold(RestartPolicyNames, value) {
			return "must be one of " + strings.Join(RestartPolicyNames, ", ")
		}
		return ""

	case ValueKindNetwork:
		if strings.ContainsAny(value, "*?") {
			return "must not contain wildcards; network names are matched exactly"
		}
		if value == "" || len(value) > 128 {
			return "must be between 1 and 128 characters"
		}
		// Docker's own network-name shape.
		for index, r := range value {
			alphanumeric := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if index == 0 && !alphanumeric {
				return "must start with a letter or digit"
			}
			if !alphanumeric && r != '_' && r != '.' && r != '-' {
				return "must contain only letters, digits, underscore, dot and hyphen"
			}
		}
		return ""

	case ValueKindNone:
		// Unreachable: a kind of none implies RequiresValues is false, and
		// validateRuleValues returns before reaching here. Handled rather than
		// defaulted so adding a kind is a compile-time decision.
		return "is not accepted for this rule type"
	}
	return "is not accepted for this rule type"
}

// validatePolicyText applies the text rules to a name or description.
//
// Length is measured in BYTES and validity in runes: a limit expressed in
// characters would let a caller send far more data than intended under a
// "120-character" cap.
func validatePolicyText(field, value string, maxBytes int, required bool) error {
	if value == "" {
		if required {
			return PolicyValidationError{Field: field, Message: "must not be empty"}
		}
		return nil
	}
	if len(value) > maxBytes {
		return PolicyValidationError{
			Field:   field,
			Message: "must be at most " + strconv.Itoa(maxBytes) + " bytes",
		}
	}
	if !utf8.ValidString(value) {
		return PolicyValidationError{Field: field, Message: "must be valid UTF-8"}
	}
	for _, r := range value {
		// Rejected rather than stripped. A name is prose an operator typed;
		// one carrying a control character is either a mistake or an attempt
		// to make the stored value render as something else -- in a log line,
		// in a terminal, or in a UI.
		if r < 0x20 || r == 0x7f {
			return PolicyValidationError{Field: field, Message: "must not contain control characters"}
		}
	}
	return nil
}
