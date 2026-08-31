package domain

import (
	"strconv"
	"strings"
)

// Container labels that override an update policy.
//
// # Precedence, stated once and enforced in one function
//
//	container label  →  policy  →  global default
//
// A label is the most specific statement about a container and wins over the
// policy that selected it. The policy wins over the built-in defaults. Nothing
// else participates.
//
// # The one exception, and why it exists
//
// A label may only ever make automation SAFER, never more permissive in the
// direction that matters most:
//
//   - `enabled=false` and `pause=true` always win.
//   - `enabled=true` cannot enrol a container that no policy selected. A label
//     is set by whoever can run `docker run`, and if that were enough to opt a
//     container into automation, then anyone able to start a container could
//     also decide HarborMaster should start updating it.
//   - `strategy` may only NARROW the policy's strategy, never widen it. A
//     container cannot label its way from `patch` to `major`.
//
// The asymmetry is deliberate. Labels are operator convenience for restraint,
// not a second authorisation system.

// Label keys HarborMaster reads. A closed set: an unknown
// `io.harbormaster.update.*` key is reported rather than ignored, because a
// typo in a safety control should be visible.
const (
	LabelUpdateEnabled  = "io.harbormaster.update.enabled"
	LabelUpdateStrategy = "io.harbormaster.update.strategy"
	LabelUpdateWindow   = "io.harbormaster.update.window"
	LabelUpdateRollback = "io.harbormaster.update.rollback"
	LabelUpdatePause    = "io.harbormaster.update.pause"
)

// UpdateLabelPrefix is the namespace HarborMaster claims.
const UpdateLabelPrefix = "io.harbormaster.update."

// UpdateLabelKeys lists every key this build understands.
var UpdateLabelKeys = []string{
	LabelUpdateEnabled, LabelUpdateStrategy, LabelUpdateWindow,
	LabelUpdateRollback, LabelUpdatePause,
}

// LabelOverrides is what a container's labels asked for.
//
// Every field is a pointer or a zero-meaning-absent value, so "the label was
// not set" is distinguishable from "the label was set to the zero value".
type LabelOverrides struct {
	// Disabled is set when enabled=false. There is no Enabled counterpart: a
	// label cannot enrol a container. See the file header.
	Disabled bool `json:"disabled,omitempty"`
	// Paused is set when pause=true.
	Paused bool `json:"paused,omitempty"`
	// Strategy narrows the policy's ceiling. Empty means no opinion.
	Strategy UpdateStrategy `json:"strategy,omitempty"`
	// Window replaces the policy's window times, keeping its timezone and
	// weekdays. Nil means no opinion.
	Window *MaintenanceWindow `json:"window,omitempty"`
	// Rollback overrides automatic rollback. Nil means no opinion.
	Rollback *bool `json:"rollback,omitempty"`

	// Unknown lists `io.harbormaster.update.*` keys this build does not
	// understand, and Invalid lists known keys whose value could not be read.
	// Both are surfaced rather than dropped: a misspelled safety label that
	// silently does nothing is worse than one that complains.
	Unknown []string `json:"unknown,omitempty"`
	Invalid []string `json:"invalid,omitempty"`
}

// ParseUpdateLabels reads HarborMaster's labels off a container.
func ParseUpdateLabels(labels map[string]string) LabelOverrides {
	var overrides LabelOverrides

	for key, value := range labels {
		if !strings.HasPrefix(key, UpdateLabelPrefix) {
			continue
		}

		switch key {
		case LabelUpdateEnabled:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				overrides.Invalid = append(overrides.Invalid, key)
				continue
			}
			// Only the false case is honoured. See the file header.
			if !parsed {
				overrides.Disabled = true
			}

		case LabelUpdatePause:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				overrides.Invalid = append(overrides.Invalid, key)
				continue
			}
			overrides.Paused = parsed

		case LabelUpdateRollback:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				overrides.Invalid = append(overrides.Invalid, key)
				continue
			}
			rollback := parsed
			overrides.Rollback = &rollback

		case LabelUpdateStrategy:
			candidate := strings.TrimSpace(value)
			if !ValidUpdateStrategy(candidate) {
				overrides.Invalid = append(overrides.Invalid, key)
				continue
			}
			overrides.Strategy = UpdateStrategy(candidate)

		case LabelUpdateWindow:
			window, ok := parseWindowLabel(value)
			if !ok {
				overrides.Invalid = append(overrides.Invalid, key)
				continue
			}
			overrides.Window = &window

		default:
			overrides.Unknown = append(overrides.Unknown, key)
		}
	}

	sortStrings(overrides.Unknown)
	sortStrings(overrides.Invalid)
	return overrides
}

// parseWindowLabel reads "HH:MM-HH:MM".
//
// Times only. A label cannot set a timezone or weekdays: those belong to the
// policy, and letting a container choose its own zone would make one estate's
// windows unreadable as a set.
func parseWindowLabel(value string) (MaintenanceWindow, bool) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 2 {
		return MaintenanceWindow{}, false
	}
	start, okStart := minutesOfDay(parts[0])
	end, okEnd := minutesOfDay(parts[1])
	if !okStart || !okEnd || start == end {
		return MaintenanceWindow{}, false
	}
	return MaintenanceWindow{
		Start: strings.TrimSpace(parts[0]),
		End:   strings.TrimSpace(parts[1]),
	}, true
}

// EffectivePolicy is a policy with one container's labels applied.
//
// The result of the whole precedence chain, computed once so every downstream
// check reads the same answer. Nothing below this point consults labels again.
type EffectivePolicy struct {
	// Policy is the governing policy, unmodified, for attribution.
	Policy UpdatePolicy `json:"policy"`
	// Overrides is what the container's labels asked for.
	Overrides LabelOverrides `json:"overrides"`
	// Preference is the behaviour an operator chose for this container, when
	// one was chosen. Carried unmodified for attribution, exactly like Policy:
	// the interface has to be able to say "you asked for X and Y decided".
	Preference UpdateBehavior `json:"preference,omitempty"`
	// PreferenceApplied reports that the preference actually narrowed something.
	// False when no preference exists, and ALSO false when the preference asked
	// for more than the policy already permitted -- which is the case the
	// interface must never render as though the operator got what they picked.
	PreferenceApplied bool `json:"preferenceApplied,omitempty"`

	// The resolved settings. These are what the scheduler acts on.
	Strategy     UpdateStrategy    `json:"strategy"`
	Window       MaintenanceWindow `json:"window"`
	Mode         AutomationMode    `json:"mode"`
	AutoRollback bool              `json:"autoRollback"`

	// Disabled reports that automation must not act on this container at all,
	// and Reason says which clause decided that.
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason,omitempty"`
}

// Resolve applies a container's labels and its chosen behaviour to the policy
// that governs it.
//
// The single place precedence is implemented. Every caller takes the result;
// none re-reads a label, and none re-derives a preference.
//
// # Both inputs may only narrow
//
// A label narrows the strategy and never widens it. A preference narrows the
// MODE and never widens it. Neither can raise a ceiling the governing policy
// set, so the worst either can do is stop something from happening.
//
// That is the whole reason a per-container control can exist without becoming a
// second authorisation path. An operator who selects "Automatic" on a container
// an update policy holds for review does not get automatic: they get
// approvalRequired, and `Preference` and `PreferenceApplied` are what let the
// interface say so instead of lying about it.
//
// Pass the empty behaviour when no preference exists.
func Resolve(
	policy UpdatePolicy,
	labels map[string]string,
	preference UpdateBehavior,
) EffectivePolicy {
	overrides := ParseUpdateLabels(labels)

	effective := EffectivePolicy{
		Policy:       policy,
		Overrides:    overrides,
		Preference:   preference,
		Strategy:     policy.Strategy,
		Window:       policy.Window,
		Mode:         policy.Mode,
		AutoRollback: policy.Failure.AutoRollback,
	}

	// A preference may narrow the mode, never widen it.
	//
	// `Automatic` caps at nothing and is the same instruction as "no preference
	// at all": it is stored so the interface can tell a deliberate choice from
	// an absent one, and it deliberately changes nothing here.
	if capped := preference.Mode(); capped != "" && narrowerMode(capped, effective.Mode) {
		effective.Mode = capped
		effective.PreferenceApplied = true
	}

	// A label may narrow the strategy, never widen it.
	if overrides.Strategy != "" && narrower(overrides.Strategy, policy.Strategy) {
		effective.Strategy = overrides.Strategy
	}

	// A label may replace the window's TIMES, keeping the policy's zone and
	// weekdays, so an estate's windows stay comparable.
	if overrides.Window != nil && !policy.Window.AlwaysOpen {
		effective.Window = MaintenanceWindow{
			Timezone: policy.Window.Timezone,
			Weekdays: policy.Window.Weekdays,
			Start:    overrides.Window.Start,
			End:      overrides.Window.End,
		}
	}

	if overrides.Rollback != nil {
		effective.AutoRollback = *overrides.Rollback
	}

	switch {
	case overrides.Disabled:
		effective.Disabled = true
		effective.Reason = "the container carries " + LabelUpdateEnabled + "=false"
	case overrides.Paused:
		effective.Disabled = true
		effective.Reason = "the container carries " + LabelUpdatePause + "=true"
	}

	return effective
}

// narrower reports whether candidate permits strictly less than current.
func narrower(candidate, current UpdateStrategy) bool {
	return strategyRank(candidate) < strategyRank(current)
}

// strategyRank orders strategies by blast radius.
func strategyRank(s UpdateStrategy) int {
	switch s {
	case StrategyDigestOnly:
		return 0
	case StrategyPatch:
		return 1
	case StrategyMinor:
		return 2
	case StrategyMajor:
		return 3
	default:
		// An unrecognised strategy ranks as the most permissive, so `narrower`
		// refuses to adopt it. Fails closed.
		return 99
	}
}

// sortStrings sorts in place without pulling sort into every caller.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
