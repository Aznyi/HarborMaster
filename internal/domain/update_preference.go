package domain

import "strings"

// One container's chosen update behaviour.
//
// # What this is
//
// The per-container half of the simple experience C1 began: an operator opens a
// container, picks how it should be updated, and never learns what a selector
// is. It is a PREFERENCE composed onto the policy that governs the container --
// not a policy, not a second selection rule, and not a way to reach the engine
// directly.
//
// # The one rule that makes it safe
//
// A preference may only make automation SAFER.
//
// It narrows the governing policy's mode and can never widen it, exactly as a
// container label narrows a strategy and can never widen one. So the worst a
// preference can do is stop something from happening. An operator who picks
// "Automatic" on a container an update policy holds for review gets the
// policy's behaviour, and the interface says which rule decided that rather
// than pretending otherwise.
//
// That asymmetry is what keeps this a presentation of the existing engine. If a
// preference could raise a ceiling it would be a second authorisation path, and
// the question "may this container be updated without asking" would have two
// answers.

// UpdateBehavior is what an operator chose for one container.
type UpdateBehavior string

const (
	// BehaviorAutomatic imposes NO restriction. The governing policy decides,
	// which is also what happens when no preference exists.
	//
	// Stored rather than represented by absence, so "an operator looked at this
	// container and chose to leave it automatic" and "nobody has ever looked"
	// remain different facts. The engine treats them identically; the interface
	// does not have to.
	BehaviorAutomatic UpdateBehavior = "automatic"

	// BehaviorReviewFirst caps the mode at approvalRequired: automation still
	// decides, and a person releases each decision.
	//
	// This maps onto ModeApprove, which already means exactly that --
	// "automation chooses; a human commits". It is not an approximation of
	// observe, and it does not touch the planner's recommendation: the risk
	// model stays the sole author of that verdict.
	BehaviorReviewFirst UpdateBehavior = "reviewFirst"

	// BehaviorMonitorOnly caps the mode at observe: HarborMaster keeps checking
	// registries and keeps reporting what it finds, and never changes the
	// container by itself.
	//
	// # Why there is no separate "Excluded"
	//
	// Because it would be the same state wearing a second name. "Excluded" can
	// mean two things: stop ACTING on this container, or stop WATCHING it. The
	// first is exactly this value. The second would remove the container from
	// the Updates workspace, so an operator would stop being told that their
	// database has a critical patch waiting -- a worse outcome than the one they
	// were trying to choose, and not what anybody means by "leave it alone".
	//
	// An operator who genuinely wants HarborMaster to ignore a container still
	// has the label that already does that: io.harbormaster.enabled=false,
	// which is set where the container is defined and outranks everything here.
	BehaviorMonitorOnly UpdateBehavior = "monitorOnly"
)

// UpdateBehaviors lists every behaviour, most permissive first.
var UpdateBehaviors = []UpdateBehavior{
	BehaviorAutomatic, BehaviorReviewFirst, BehaviorMonitorOnly,
}

// ValidUpdateBehavior reports whether value names a behaviour.
func ValidUpdateBehavior(value string) bool {
	for _, b := range UpdateBehaviors {
		if string(b) == value {
			return true
		}
	}
	return false
}

// Mode is the automation mode this behaviour caps at.
//
// Returns the empty mode for Automatic, which imposes no cap. A caller must
// treat the empty value as "no restriction" rather than as a mode.
func (b UpdateBehavior) Mode() AutomationMode {
	switch b {
	case BehaviorReviewFirst:
		return ModeApprove
	case BehaviorMonitorOnly:
		return ModeObserve
	default:
		return ""
	}
}

// Describe renders a behaviour in operator-facing words.
func (b UpdateBehavior) Describe() string {
	switch b {
	case BehaviorAutomatic:
		return "updated automatically when an eligible update is available"
	case BehaviorReviewFirst:
		return "held for review, so somebody releases each update"
	case BehaviorMonitorOnly:
		return "watched and reported, and never changed automatically"
	default:
		return "an unrecognised behaviour, which restricts nothing"
	}
}

// ContainerUpdatePreference is one stored choice.
//
// Keyed by container NAME because it has to survive the update it authorises:
// a successful recreation gives the container a new Docker id, and a preference
// keyed on the id would be discarded at exactly the wrong moment. The name is
// the identity automation_pauses and image_lineage already use for the same
// reason.
type ContainerUpdatePreference struct {
	ContainerName string         `json:"containerName"`
	Behavior      UpdateBehavior `json:"behavior"`

	// ContainerID is evidence of what was observed when this was written.
	// Nothing resolves the preference by it.
	ContainerID string `json:"containerId,omitempty"`

	SetByUsername string `json:"setByUsername,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

// NormaliseUpdateBehavior trims and lowercases a caller-supplied value.
//
// Returns the empty behaviour for anything unrecognised, so an unknown value
// restricts nothing rather than being guessed at. Validation refuses it by name
// before it can be stored.
func NormaliseUpdateBehavior(raw string) UpdateBehavior {
	trimmed := UpdateBehavior(strings.TrimSpace(raw))
	if ValidUpdateBehavior(string(trimmed)) {
		return trimmed
	}
	return ""
}

// modeRank orders automation modes by how much they may do.
//
// The order is AutomationModes' own -- observe, dryRun, approvalRequired,
// automatic -- read as a ceiling rather than a list. Narrowing means moving
// DOWN it.
func modeRank(m AutomationMode) int {
	switch m {
	case ModeObserve:
		return 0
	case ModeDryRun:
		return 1
	case ModeApprove:
		return 2
	case ModeAutomatic:
		return 3
	default:
		// An unrecognised mode ranks below everything, so it can never be the
		// wider of a pair and can never widen a policy.
		return -1
	}
}

// narrowerMode reports whether candidate permits strictly less than current.
func narrowerMode(candidate, current AutomationMode) bool {
	return modeRank(candidate) < modeRank(current)
}
