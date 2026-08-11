package domain

import "strings"

// What an update policy is pointed at.
//
// # Why a scope is a TYPE and not a selector value
//
// "Keep everything up to date" is the single most common thing an operator
// wants from an updater, and before this it could only be expressed by typing
// something into a selector field. Every way of doing that was wrong:
//
//   - The image pattern `*`. Validation refuses it by name, and rightly:
//     a field labelled "images" that silently means "the whole estate" is an
//     accident waiting to be typed.
//   - An empty selector. Refused too, and it means the opposite -- an empty
//     selector governs NOTHING, which is the safe reading of "the operator has
//     not said yet".
//   - A magic name in `include`. A container could be called that.
//
// All three share one defect: the breadth of a policy would be a property of a
// STRING, discovered by parsing, rather than a property an operator chose. So
// breadth is its own field, with its own closed vocabulary, its own column, and
// its own check -- and the string fields keep meaning exactly what they say.
//
// # Broad SELECTION is not broad AUTHORISATION
//
// ScopeAllEligible widens what a policy may look at. It widens nothing else.
// Every container it selects still has to pass, in order: the pause, the
// container's own opt-out labels, the strategy ceiling, the deployment's
// major-version rule, the planner's recommendation, the maintenance window, the
// in-flight check, the mode, the run and concurrency budgets, the acquisition
// preflight, the digest verification, the recreation preflight, the
// preservation comparison, and the health proof. A policy in this scope with
// mode `observe` changes nothing, exactly like any other policy in `observe`.
//
// # And it is not "every container that exists"
//
// A container being present is not evidence that HarborMaster may consider it.
// BroadlySelectable requires POSITIVE facts, each established by HarborMaster
// from its own records, and the zero value of those facts selects nothing. The
// disqualifications are the containers that could never be legitimately
// enrolled by a rule an operator wrote in one click:
//
//   - HarborMaster's own container, which it cannot update at all.
//   - The parked originals and quarantined replacements HarborMaster creates
//     during a recreation. Those are EVIDENCE. Enrolling the wreckage of a
//     failed update into the automation that produced it is how one bad image
//     becomes two.
//   - A container carrying the estate-wide `io.harbormaster.enabled=false`
//     opt-out.
//   - A workload HarborMaster could not recreate anyway, because its name
//     cannot survive the parking step.
//
// Every one of those is a REFUSAL derived from a fact. None of them is a
// container promoting itself: a label can only ever move a container out of
// scope here, never into it, which is the same asymmetry ParseUpdateLabels
// enforces for the policy overrides.

// UpdateScope is what a policy is pointed at.
//
// A closed vocabulary. The zero value is not one of them, and Normalise maps it
// to ScopeSelector -- the narrow reading -- so a policy written before this
// field existed, or by a client that does not send it, keeps exactly the
// breadth its selector always had.
type UpdateScope string

const (
	// ScopeSelector governs the containers the selector names. The original
	// behaviour, and still the default.
	ScopeSelector UpdateScope = "selector"

	// ScopeAllEligible governs every container HarborMaster may legitimately
	// consider, minus the policy's own exclusions.
	//
	// Never a default, and never reachable by leaving a field blank.
	ScopeAllEligible UpdateScope = "allEligible"
)

// UpdateScopes lists every scope, narrowest first.
var UpdateScopes = []UpdateScope{ScopeSelector, ScopeAllEligible}

// ValidUpdateScope reports whether value names a scope this build understands.
func ValidUpdateScope(value string) bool {
	for _, scope := range UpdateScopes {
		if string(scope) == value {
			return true
		}
	}
	return false
}

// Broad reports whether the scope selects beyond what the selector names.
func (s UpdateScope) Broad() bool { return s == ScopeAllEligible }

// Describe renders a scope in operator-facing words.
func (s UpdateScope) Describe() string {
	switch s {
	case ScopeSelector:
		return "the containers this policy names"
	case ScopeAllEligible:
		return "all eligible containers"
	default:
		return "an unrecognised scope, which selects nothing"
	}
}

// ------------------------------------------------------------ eligibility --

// TargetEligibility is what HarborMaster established about one container, from
// its OWN records, before any policy was consulted.
//
// # Every field is a POSITIVE fact, and the zero value selects nothing
//
// Deliberately not a set of "is it bad" flags. A flag that defaults to false
// means a target nobody screened looks clean, and the one place that must never
// happen is the scope that would otherwise reach the whole estate. So the
// broadly-selectable test requires Recreatable to be TRUE, and a caller that
// forgets to populate this struct gets a policy that governs nothing rather
// than one that governs everything.
//
// The disqualifications below are separate booleans because each is a different
// sentence to an operator asking "why was that container not considered".
type TargetEligibility struct {
	// Recreatable records that this is an ordinary workload HarborMaster could
	// act on at all: it has a usable image reference, and a name that can
	// survive the parking step of a recreation. Established from the inventory
	// row, and REQUIRED for broad selection.
	//
	// A container that fails this would be refused by the execution preflight
	// anyway. Refusing it at selection time means it is reported as "not
	// eligible" in a decision an operator can read, rather than selected,
	// planned, and then declined at the mutation point.
	Recreatable bool `json:"recreatable"`

	// Derived marks a container HarborMaster itself created during a
	// recreation: a parked original or a quarantined replacement.
	//
	// Recognised by the name HarborMaster derived for it. That test is not
	// evidence of provenance -- an operator could name a container the same way
	// -- and nothing may ACT on the strength of it. Declining to enrol one is
	// not acting: the false positive costs an operator a container that a broad
	// policy skips and an explicit selector still reaches.
	Derived bool `json:"derived,omitempty"`

	// OptedOut marks the estate-wide `io.harbormaster.enabled=false` label.
	//
	// Distinct from the `io.harbormaster.update.enabled=false` opt-out that
	// DecideAutomation reads: that one is specific to updates and is checked for
	// every policy in every scope. This one is the older, broader statement that
	// HarborMaster should leave the container alone, and a rule an operator
	// wrote in one click must honour it.
	OptedOut bool `json:"optedOut,omitempty"`
}

// SelectionTarget is what a selector is matched against.
//
// A projection rather than the whole container: a selector must not be able to
// reach configuration it has no business reading, and a narrow input makes the
// matching function pure and cheap to test.
type SelectionTarget struct {
	Name   string
	Image  string
	Labels map[string]string

	// Eligibility is what HarborMaster established about this container before
	// any policy was consulted. Read ONLY by the broad scope; a selector that
	// names a container by name or image reaches it regardless, because an
	// operator who typed the name has said which container they mean.
	Eligibility TargetEligibility
}

// BroadlySelectable reports whether a policy in ScopeAllEligible may consider
// this container, and says why not when it may not.
//
// # Self is passed in rather than precomputed
//
// The identity is a parameter so the compiler requires every caller to have one
// in hand. A precomputed "is self" boolean on the target would default to
// false, which is the wrong direction for the one exclusion that must never be
// forgotten.
//
// The zero identity matches nothing, which is honest: a build that could not
// work out which container it is excludes none rather than the wrong one. That
// is the same choice SelfMatch documents, and it is safe here for the same
// reason -- the refusal is repeated in DecideAutomation, in the acquisition
// preflight, and in the execution preflight, and a broad scope does not remove
// any of the three.
func (t SelectionTarget) BroadlySelectable(self SelfIdentity) (bool, string) {
	if match, _ := self.SelfMatch(SelfTarget{
		ContainerName: t.Name,
		ImageRef:      t.Image,
		Labels:        t.Labels,
	}); match {
		return false, "this is the container HarborMaster is running in"
	}
	if t.Eligibility.Derived {
		return false, "this container is the parked original or the quarantined " +
			"replacement of an earlier update, which HarborMaster keeps as evidence"
	}
	if t.Eligibility.OptedOut {
		return false, "the container carries " + LabelHarborMasterEnabled + "=false"
	}
	if !t.Eligibility.Recreatable {
		return false, "HarborMaster could not establish that this is a workload it " +
			"could recreate, so a broad policy does not enrol it"
	}
	return true, ""
}

// ScreenTarget establishes the eligibility facts for one container.
//
// The one place they are derived, from an inventory row HarborMaster wrote and
// the labels it read off the daemon. Called by the repository that loads a
// pass's targets, so a pass costs no extra query for any of this.
func ScreenTarget(name, imageRef string, labels map[string]string) TargetEligibility {
	normalised := NormaliseContainerName(name)
	eligibility := TargetEligibility{
		Recreatable: strings.TrimSpace(imageRef) != "" &&
			RecreatableContainerName(normalised),
		Derived: IsHarborMasterDerivedName(normalised),
	}

	// Only the explicit false opts out. An absent label and an unreadable value
	// both mean "the operator has not said", which is not an opt-out -- and a
	// value this cannot read must not become one, or a typo would silently
	// remove a container from a policy an operator believes covers it.
	if value, ok := labels[LabelHarborMasterEnabled]; ok {
		if falsyLabel(value) {
			eligibility.OptedOut = true
		}
	}
	return eligibility
}

// falsyLabel reads the explicit negative of a boolean label.
//
// The mirror of truthyLabel, and deliberately not `!truthyLabel(value)`: an
// unrecognised value must be neither true nor false here, because both of the
// two questions this package asks of a label ("is this HarborMaster" and "did
// the operator opt out") take their safe answer from "the label did not say".
func falsyLabel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "no", "off":
		return true
	default:
		return false
	}
}
