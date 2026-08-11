package domain

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Update policy validation.
//
// An update policy is the most dangerous object an administrator can create in
// HarborMaster: it is the only one that can cause the host to change without
// anybody watching. So it is validated as hostile input even though only an
// administrator can submit one, and every bound below exists to make the worst
// case of a policy computable rather than discovered.
//
// # What validation refuses outright
//
//   - A selector that matches everything. `Empty()` already matches nothing,
//     but a policy whose only clause is the pattern `*` is the same accident
//     with more typing, and it is rejected by name.
//   - A minimum recommendation of `unknown`, `manualReview`, or
//     `notRecommended`. The first two mean the planner wants a person; the
//     third means it argued against the change. None can be automated.
//   - An unbounded limit. A zero means "use the default", never "no limit".
//
// No validation message echoes the offending value.

// UpdatePolicyLimits bounds one policy definition.
//
// Passed in rather than read from a package variable, matching PolicyLimits, so
// the API layer's configuration is what is enforced and a test can shrink the
// bounds without mutating global state.
type UpdatePolicyLimits struct {
	MaxNameBytes        int `json:"maxNameBytes"`
	MaxDescriptionBytes int `json:"maxDescriptionBytes"`
	MaxSelectorEntries  int `json:"maxSelectorEntries"`
	MaxSelectorBytes    int `json:"maxSelectorBytes"`
	MaxLabelSelectors   int `json:"maxLabelSelectors"`
	MaxWeekdays         int `json:"maxWeekdays"`
	MaxConcurrent       int `json:"maxConcurrent"`
	MaxPerRun           int `json:"maxPerRun"`
	MaxTimeoutSeconds   int `json:"maxTimeoutSeconds"`
	MaxPauseHours       int `json:"maxPauseHours"`
	MaxRetries          int `json:"maxRetries"`
}

// Default update-policy bounds.
//
// The concurrency ceilings are the load-bearing ones. A pass that could start
// two hundred simultaneous pulls would be a denial of service HarborMaster
// inflicted on its own host and on the registry, so the maximum an operator may
// configure is small enough that the worst case is survivable and large enough
// that no real estate is inconvenienced.
const (
	DefaultUpdatePolicyMaxNameBytes        = 120
	DefaultUpdatePolicyMaxDescriptionBytes = 1000
	DefaultUpdatePolicyMaxSelectorEntries  = 64
	DefaultUpdatePolicyMaxSelectorBytes    = 256
	DefaultUpdatePolicyMaxLabelSelectors   = 16
	DefaultUpdatePolicyMaxWeekdays         = 7
	DefaultUpdatePolicyMaxConcurrent       = 8
	DefaultUpdatePolicyMaxPerRun           = 50
	DefaultUpdatePolicyMaxTimeoutSeconds   = 1800
	DefaultUpdatePolicyMaxPauseHours       = 720
	DefaultUpdatePolicyMaxRetries          = 5
)

// Defaults applied when a field is left at zero.
const (
	DefaultUpdateConcurrency        = 1
	DefaultUpdatePerRegistry        = 1
	DefaultUpdatePerRun             = 10
	DefaultAcquisitionTimeoutSecs   = 600
	DefaultRecreateTimeoutSecs      = 300
	DefaultHealthTimeoutSecs        = 120
	DefaultPauseAfterFailures       = 2
	DefaultPauseWindowHours         = 24
	DefaultUpdatePolicyMaxRetryStep = 1
)

// DefaultUpdatePolicyLimits returns the bounds used when none are configured.
func DefaultUpdatePolicyLimits() UpdatePolicyLimits {
	return UpdatePolicyLimits{
		MaxNameBytes:        DefaultUpdatePolicyMaxNameBytes,
		MaxDescriptionBytes: DefaultUpdatePolicyMaxDescriptionBytes,
		MaxSelectorEntries:  DefaultUpdatePolicyMaxSelectorEntries,
		MaxSelectorBytes:    DefaultUpdatePolicyMaxSelectorBytes,
		MaxLabelSelectors:   DefaultUpdatePolicyMaxLabelSelectors,
		MaxWeekdays:         DefaultUpdatePolicyMaxWeekdays,
		MaxConcurrent:       DefaultUpdatePolicyMaxConcurrent,
		MaxPerRun:           DefaultUpdatePolicyMaxPerRun,
		MaxTimeoutSeconds:   DefaultUpdatePolicyMaxTimeoutSeconds,
		MaxPauseHours:       DefaultUpdatePolicyMaxPauseHours,
		MaxRetries:          DefaultUpdatePolicyMaxRetries,
	}
}

// normalise fills any unset bound with its default, so a partially configured
// limit set cannot accidentally mean "unbounded".
func (l UpdatePolicyLimits) normalise() UpdatePolicyLimits {
	defaults := DefaultUpdatePolicyLimits()
	if l.MaxNameBytes < 1 {
		l.MaxNameBytes = defaults.MaxNameBytes
	}
	if l.MaxDescriptionBytes < 1 {
		l.MaxDescriptionBytes = defaults.MaxDescriptionBytes
	}
	if l.MaxSelectorEntries < 1 {
		l.MaxSelectorEntries = defaults.MaxSelectorEntries
	}
	if l.MaxSelectorBytes < 1 {
		l.MaxSelectorBytes = defaults.MaxSelectorBytes
	}
	if l.MaxLabelSelectors < 1 {
		l.MaxLabelSelectors = defaults.MaxLabelSelectors
	}
	if l.MaxWeekdays < 1 {
		l.MaxWeekdays = defaults.MaxWeekdays
	}
	if l.MaxConcurrent < 1 {
		l.MaxConcurrent = defaults.MaxConcurrent
	}
	if l.MaxPerRun < 1 {
		l.MaxPerRun = defaults.MaxPerRun
	}
	if l.MaxTimeoutSeconds < 1 {
		l.MaxTimeoutSeconds = defaults.MaxTimeoutSeconds
	}
	if l.MaxPauseHours < 1 {
		l.MaxPauseHours = defaults.MaxPauseHours
	}
	if l.MaxRetries < 1 {
		l.MaxRetries = defaults.MaxRetries
	}
	return l
}

// AutomatableRecommendations is the closed set a policy may require.
//
// The whole allowlist, stated once. Anything absent from it is a verdict that
// asked for a person, and a policy naming one would be a policy that automated
// the planner's request for human judgement.
var AutomatableRecommendations = []Recommendation{RecommendProceed, RecommendCaution}

// AutomatableRecommendation reports whether a verdict may gate automation.
func AutomatableRecommendation(value Recommendation) bool {
	for _, allowed := range AutomatableRecommendations {
		if allowed == value {
			return true
		}
	}
	return false
}

// Normalise trims, deduplicates, and applies defaults in place.
//
// Called before Validate so the bounds apply to what will actually be stored,
// and so the persisted policy is the one the scheduler will read: a policy
// whose defaults were filled in at read time would behave differently depending
// on which build read it.
func (p *UpdatePolicy) Normalise() {
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)
	p.Strategy = UpdateStrategy(strings.TrimSpace(string(p.Strategy)))
	p.Mode = AutomationMode(strings.TrimSpace(string(p.Mode)))
	p.MinimumRecommendation = Recommendation(strings.TrimSpace(string(p.MinimumRecommendation)))

	// An absent scope is the NARROW one. This is the whole backward-compatibility
	// story for the field: a policy stored before it existed, and a client that
	// does not send it, both land on exactly the behaviour they already had.
	//
	// Only the empty string is defaulted. A scope that was supplied and is not
	// recognised is left alone for Validate to refuse by name, because silently
	// treating an unknown breadth as the narrow one would tell an operator their
	// policy was stored as something other than what they asked for.
	p.Scope = UpdateScope(strings.TrimSpace(string(p.Scope)))
	if p.Scope == "" {
		p.Scope = ScopeSelector
	}

	p.Selector.Images = normaliseSelectorList(p.Selector.Images)
	p.Selector.Include = normaliseSelectorList(p.Selector.Include)
	p.Selector.Exclude = normaliseSelectorList(p.Selector.Exclude)
	if len(p.Selector.Labels) > 0 {
		labels := make(map[string]string, len(p.Selector.Labels))
		for key, value := range p.Selector.Labels {
			trimmed := strings.TrimSpace(key)
			if trimmed == "" {
				continue
			}
			labels[trimmed] = strings.TrimSpace(value)
		}
		p.Selector.Labels = labels
	}

	p.Window.Timezone = strings.TrimSpace(p.Window.Timezone)
	p.Window.Start = strings.TrimSpace(p.Window.Start)
	p.Window.End = strings.TrimSpace(p.Window.End)
	p.Window.Weekdays = normaliseWeekdays(p.Window.Weekdays)

	// A zero is "unset", never "unlimited".
	if p.Limits.MaxConcurrent < 1 {
		p.Limits.MaxConcurrent = DefaultUpdateConcurrency
	}
	if p.Limits.MaxPerRegistry < 1 {
		p.Limits.MaxPerRegistry = DefaultUpdatePerRegistry
	}
	if p.Limits.MaxPerRun < 1 {
		p.Limits.MaxPerRun = DefaultUpdatePerRun
	}
	if p.Limits.AcquisitionTimeoutSeconds < 1 {
		p.Limits.AcquisitionTimeoutSeconds = DefaultAcquisitionTimeoutSecs
	}
	if p.Limits.RecreateTimeoutSeconds < 1 {
		p.Limits.RecreateTimeoutSeconds = DefaultRecreateTimeoutSecs
	}
	if p.Limits.HealthTimeoutSeconds < 1 {
		p.Limits.HealthTimeoutSeconds = DefaultHealthTimeoutSecs
	}
	if p.Failure.PauseWindowHours < 1 {
		p.Failure.PauseWindowHours = DefaultPauseWindowHours
	}
	if p.Failure.PauseAfterFailures < 0 {
		p.Failure.PauseAfterFailures = 0
	}
	if p.Failure.CooldownHours < 0 {
		p.Failure.CooldownHours = 0
	}
	if p.Failure.MaxRetries < 0 {
		p.Failure.MaxRetries = 0
	}
}

// normaliseSelectorList trims, drops empties, and deduplicates.
func normaliseSelectorList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normaliseWeekdays deduplicates and sorts, leaving out-of-range values for
// Validate to reject rather than silently discarding them.
func normaliseWeekdays(days []int) []int {
	if len(days) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(days))
	out := make([]int, 0, len(days))
	for _, day := range days {
		if _, duplicate := seen[day]; duplicate {
			continue
		}
		seen[day] = struct{}{}
		out = append(out, day)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Validate checks an update policy against the limits.
//
// Returns the FIRST failure, matching PolicyDefinition.Validate.
func (p UpdatePolicy) Validate(limits UpdatePolicyLimits) error {
	limits = limits.normalise()

	if err := validatePolicyText("name", p.Name, limits.MaxNameBytes, true); err != nil {
		return err
	}
	if err := validatePolicyText("description", p.Description, limits.MaxDescriptionBytes, false); err != nil {
		return err
	}
	if !ValidUpdateScope(string(p.Scope)) {
		return PolicyValidationError{
			Field:   "scope",
			Message: "must be one of selector, allEligible",
		}
	}
	if !ValidUpdateStrategy(string(p.Strategy)) {
		return PolicyValidationError{
			Field:   "strategy",
			Message: "must be one of digestOnly, patch, minor, major",
		}
	}
	if !ValidAutomationMode(string(p.Mode)) {
		return PolicyValidationError{
			Field:   "mode",
			Message: "must be one of observe, dryRun, approvalRequired, automatic",
		}
	}
	if !AutomatableRecommendation(p.MinimumRecommendation) {
		return PolicyValidationError{
			Field:   "minimumRecommendation",
			Message: "must be proceed or proceedWithCaution; a verdict that asks for human review cannot be automated",
		}
	}
	if p.Priority < 0 || p.Priority > 1000 {
		return PolicyValidationError{Field: "priority", Message: "must be between 0 and 1000"}
	}

	if err := validateUpdateSelector(p.Scope, p.Selector, limits); err != nil {
		return err
	}
	if err := validateUpdateWindow(p.Window, limits); err != nil {
		return err
	}
	return validateUpdateLimits(p.Limits, p.Failure, limits)
}

// validateUpdateSelector bounds every clause and refuses a match-everything
// policy.
//
// # The two scopes have opposite requirements, and both are refusals
//
// Under ScopeSelector an empty selector is refused: a policy that names nothing
// governs nothing, and storing one would be storing a rule that silently does
// not work.
//
// Under ScopeAllEligible an inclusion clause is refused. Breadth is already
// decided by the scope, so `include`, `images`, and `labels` have nothing left
// to say -- a policy carrying both would be one whose meaning depends on which
// field a reader looks at first. That ambiguity is exactly what the scope field
// was introduced to remove, so it is rejected rather than resolved.
//
// `exclude` survives into both, because it is the one clause that means the
// same thing under either: never this container.
func validateUpdateSelector(scope UpdateScope, selector UpdateSelector, limits UpdatePolicyLimits) error {
	switch scope {
	case ScopeAllEligible:
		if !selector.Empty() {
			return PolicyValidationError{
				Field: "selector",
				Message: "must not name labels, images or include when the scope is " +
					"allEligible; the scope already decides what the policy reaches, " +
					"and exclusions are the only selector clause that applies to it",
			}
		}
	default:
		if selector.Empty() {
			return PolicyValidationError{
				Field:   "selector",
				Message: "must name at least one of labels, images or include; an empty selector governs nothing",
			}
		}
	}

	lists := []struct {
		field  string
		values []string
	}{
		{"selector.images", selector.Images},
		{"selector.include", selector.Include},
		{"selector.exclude", selector.Exclude},
	}
	for _, list := range lists {
		if len(list.values) > limits.MaxSelectorEntries {
			return PolicyValidationError{
				Field:   list.field,
				Message: "must contain at most " + strconv.Itoa(limits.MaxSelectorEntries) + " entries",
			}
		}
		for index, value := range list.values {
			field := list.field + "[" + strconv.Itoa(index) + "]"
			if message := validateSelectorEntry(value, limits.MaxSelectorBytes); message != "" {
				return PolicyValidationError{Field: field, Message: message}
			}
			// A bare wildcard is the accident this check exists for: it makes
			// the policy govern the whole estate, which is never what somebody
			// meant to type into a field labelled "images".
			if list.field == "selector.images" && strings.TrimSpace(value) == "*" {
				return PolicyValidationError{
					Field:   field,
					Message: "must not be a bare wildcard; name the images this policy governs",
				}
			}
		}
	}

	if len(selector.Labels) > limits.MaxLabelSelectors {
		return PolicyValidationError{
			Field:   "selector.labels",
			Message: "must contain at most " + strconv.Itoa(limits.MaxLabelSelectors) + " labels",
		}
	}
	for key, value := range selector.Labels {
		if message := validateSelectorEntry(key, limits.MaxSelectorBytes); message != "" {
			return PolicyValidationError{Field: "selector.labels", Message: "key " + message}
		}
		if value != "" {
			if message := validateSelectorEntry(value, limits.MaxSelectorBytes); message != "" {
				return PolicyValidationError{Field: "selector.labels", Message: "value " + message}
			}
		}
	}
	return nil
}

// validateSelectorEntry applies the shape rules one selector string must meet.
func validateSelectorEntry(value string, maxBytes int) string {
	if strings.TrimSpace(value) == "" {
		return "must not be empty"
	}
	if len(value) > maxBytes {
		return "must be at most " + strconv.Itoa(maxBytes) + " bytes"
	}
	if !utf8.ValidString(value) {
		return "must be valid UTF-8"
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "must not contain control characters"
		}
	}
	// The glob matcher walks the pattern once per `*`, so the count is what
	// bounds its cost. Eight is far more than any reference pattern needs.
	if strings.Count(value, "*") > 8 {
		return "must not contain more than 8 wildcards"
	}
	return ""
}

// validateUpdateWindow checks the maintenance window.
func validateUpdateWindow(window MaintenanceWindow, limits UpdatePolicyLimits) error {
	if len(window.Weekdays) > limits.MaxWeekdays {
		return PolicyValidationError{
			Field:   "window.weekdays",
			Message: "must contain at most " + strconv.Itoa(limits.MaxWeekdays) + " days",
		}
	}
	if len(window.Timezone) > 64 {
		return PolicyValidationError{Field: "window.timezone", Message: "must be at most 64 bytes"}
	}
	if err := window.Validate(); err != nil {
		// The window's own message names the constraint and never the value.
		return PolicyValidationError{Field: "window", Message: err.Error()}
	}
	return nil
}

// validateUpdateLimits bounds the concurrency, timeout, and failure settings.
func validateUpdateLimits(values UpdateLimits, failure UpdateFailureHandling, limits UpdatePolicyLimits) error {
	checks := []struct {
		field string
		value int
		max   int
	}{
		{"limits.maxConcurrent", values.MaxConcurrent, limits.MaxConcurrent},
		{"limits.maxPerRegistry", values.MaxPerRegistry, limits.MaxConcurrent},
		{"limits.maxPerRun", values.MaxPerRun, limits.MaxPerRun},
		{"limits.acquisitionTimeoutSeconds", values.AcquisitionTimeoutSeconds, limits.MaxTimeoutSeconds},
		{"limits.recreateTimeoutSeconds", values.RecreateTimeoutSeconds, limits.MaxTimeoutSeconds},
		{"limits.healthTimeoutSeconds", values.HealthTimeoutSeconds, limits.MaxTimeoutSeconds},
	}
	for _, check := range checks {
		if check.value < 1 || check.value > check.max {
			return PolicyValidationError{
				Field:   check.field,
				Message: "must be between 1 and " + strconv.Itoa(check.max),
			}
		}
	}
	// Per-registry concurrency above overall concurrency is not wrong, only
	// meaningless, and a setting that silently does nothing is a setting an
	// operator will believe in.
	if values.MaxPerRegistry > values.MaxConcurrent {
		return PolicyValidationError{
			Field:   "limits.maxPerRegistry",
			Message: "must not exceed limits.maxConcurrent",
		}
	}

	if failure.PauseAfterFailures < 0 || failure.PauseAfterFailures > 100 {
		return PolicyValidationError{
			Field:   "failure.pauseAfterFailures",
			Message: "must be between 0 and 100",
		}
	}
	if failure.PauseWindowHours < 1 || failure.PauseWindowHours > limits.MaxPauseHours {
		return PolicyValidationError{
			Field:   "failure.pauseWindowHours",
			Message: "must be between 1 and " + strconv.Itoa(limits.MaxPauseHours),
		}
	}
	if failure.CooldownHours < 0 || failure.CooldownHours > limits.MaxPauseHours {
		return PolicyValidationError{
			Field:   "failure.cooldownHours",
			Message: "must be between 0 and " + strconv.Itoa(limits.MaxPauseHours),
		}
	}
	if failure.MaxRetries < 0 || failure.MaxRetries > limits.MaxRetries {
		return PolicyValidationError{
			Field:   "failure.maxRetries",
			Message: "must be between 0 and " + strconv.Itoa(limits.MaxRetries),
		}
	}
	return nil
}

// Warnings reports settings that are legal but worth telling an operator about.
//
// Separate from Validate because none of these should refuse a policy: an
// administrator may genuinely want an unattended major-version update at any
// hour. They should just have to see HarborMaster say so first.
func (p UpdatePolicy) Warnings() []string {
	var warnings []string

	// Breadth first, because it is the setting that decides how many containers
	// every other warning below applies to.
	if p.Scope == ScopeAllEligible && p.Mode == ModeAutomatic {
		warnings = append(warnings,
			"this policy may update every eligible container on this host without asking; "+
				"HarborMaster's own container, the parked and quarantined containers it "+
				"keeps as evidence, and anything you exclude are still refused")
	}
	if p.Scope == ScopeAllEligible && len(p.Selector.Exclude) == 0 {
		warnings = append(warnings,
			"this policy covers all eligible containers and excludes none; a container "+
				"you would not want restarted has to be excluded here or opted out by label")
	}

	if p.Mode == ModeAutomatic && p.Strategy == StrategyMajor {
		warnings = append(warnings,
			"this policy applies major version updates without asking; major versions are where publishers put breaking changes")
	}
	if p.Mode == ModeAutomatic && p.Window.AlwaysOpen {
		warnings = append(warnings,
			"this policy has no maintenance window, so it may restart containers at any hour")
	}
	if p.Mode == ModeAutomatic && !p.Failure.AutoRollback {
		warnings = append(warnings,
			"automatic rollback is off, so a failed update will leave the replacement container in place for a person to resolve")
	}
	if p.Failure.PauseAfterFailures == 0 {
		warnings = append(warnings,
			"pausing after repeated failures is off, so a container that fails every pass will be retried every pass")
	}
	if p.MinimumRecommendation == RecommendCaution && p.Mode == ModeAutomatic {
		warnings = append(warnings,
			"this policy automates changes the planner flagged for caution")
	}
	return warnings
}
