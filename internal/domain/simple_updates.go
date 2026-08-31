package domain

// Simple automatic updates: one switch, compiled to one ordinary policy.
//
// # What this is for
//
// A homelab operator who wants "keep my containers updated" should not have to
// learn what a selector, a priority, a strategy ceiling and a recommendation
// floor are before anything happens. C1 gives them a switch.
//
// # What it is NOT
//
// It is not a second automation engine, and it is not a second authorisation
// path. Turning the switch on writes ONE UpdatePolicy through the same service,
// the same Normalise, the same Validate and the same repository an operator's
// own policy goes through, and from that moment the engine cannot tell the
// difference. Every gate that applies to a hand-written policy applies to this
// one, unchanged:
//
//   - the planner still decides whether a change exists and how big it is;
//   - the recommendation floor still holds review-required changes back;
//   - `io.harbormaster.update.enabled=false` still opts a container out;
//   - a pause still stops it;
//   - dependencies still order it;
//   - the acquisition and execution preflights still re-verify against the
//     live host;
//   - HarborMaster still refuses to update itself.
//
// The switch simplifies CONFIGURATION. It may never simplify AUTHORISATION.
// That is the same rule the frontend preset file states, held here so the
// backend obeys it too.

// SimpleUpdatesPolicyID is the reserved identity of the managed policy.
//
// # Why an id rather than a name or a new column
//
// The switch has to be able to find, update and withdraw exactly the policy it
// created, and NOTHING else. A display name is mutable and an operator may
// legitimately use the same one, so matching on it would be a hidden ownership
// mechanism that breaks the first time somebody renames a rule.
//
// `policy_id` is already the right mechanism and already has the properties
// ownership needs: it is generated server-side, never accepted from a caller,
// never changed by an update, and carries a UNIQUE constraint. Reserving one
// value costs no migration and no new column.
//
// # Why this particular value
//
// All-`f` is the LARGEST id in the space, and that is load-bearing rather than
// decorative. `moreSpecific` breaks a final tie with `candidate.PolicyID <
// current.PolicyID`, so the largest id loses every tie it can reach. Combined
// with the minimum priority and the broadest scope, the managed policy is
// beaten on all three ordering keys by any policy an operator wrote. See
// SimpleUpdatesPolicy.
//
// NewUpdatePolicyID refuses to emit this value, so the reservation is
// structural rather than a 2^-80 bet.
const SimpleUpdatesPolicyID = UpdatePolicyIDPrefix + "ffffffffffffffffffff"

// SimpleUpdatesPolicyName is what the managed policy is called on screen.
//
// Presentation only. Nothing resolves the policy by this string -- see
// SimpleUpdatesPolicyID for the identity -- so renaming it breaks nothing.
const SimpleUpdatesPolicyName = "Automatic updates"

// SimpleUpdatesPolicyDescription explains the row to somebody who finds it in
// the advanced policy list without having opened the switch.
const SimpleUpdatesPolicyDescription = "Managed by HarborMaster. Created and " +
	"withdrawn by the automatic updates switch on the Automation page. Edit it " +
	"there rather than here, or turn the switch off to remove it."

// IsSimpleUpdatesPolicy reports whether a policy is the managed one.
func IsSimpleUpdatesPolicy(policyID string) bool {
	return policyID == SimpleUpdatesPolicyID
}

// SimpleUpdatesPolicy is the policy the switch writes.
//
// # Every value here is a measurement, not a preference
//
// `strategy: patch` with `minimumRecommendation: proceedWithCaution` is the
// existing "Keep containers safely updated" preset, whose derivation is
// recorded in web/src/api/automationPresets.ts. The floor is the looser of the
// two automatable verdicts ON PURPOSE: the risk model reaches the medium band
// on caution factors alone -- a republished digest, a mutable tag, an image
// published in the last 48 hours -- none of which say a change is unsafe. At
// the `proceed` floor the switch would do nothing at all for the commonest
// homelab workload, and would silently start working 48 hours later.
//
// `scope: allEligible` is what makes this a catch-all. "Eligible" is a
// POSITIVE test owned by update_scope.go, and it already refuses HarborMaster's
// own container, the parked and quarantined containers it keeps as evidence,
// and anything carrying `io.harbormaster.enabled=false`. The switch inherits
// all of that; it does not restate it.
//
// `priority: 0` is the minimum the schema allows.
//
// `failure.autoRollback: true` is accurate rather than aspirational.
// AutomationService.policyPermitsRollback re-reads this field after a failed
// recreation and invokes the existing rollback service, failing closed if the
// policy cannot be read. This is genuinely different from the MANUAL apply
// path, where rollback is not automatic, and the two must be described
// separately wherever either is explained.
func SimpleUpdatesPolicy() UpdatePolicy {
	return UpdatePolicy{
		PolicyID:    SimpleUpdatesPolicyID,
		Name:        SimpleUpdatesPolicyName,
		Description: SimpleUpdatesPolicyDescription,
		Enabled:     true,

		// Beaten on all three ordering keys by anything an operator wrote.
		Priority: 0,
		Scope:    ScopeAllEligible,
		// Empty on purpose. Under allEligible the scope decides what is
		// reached; an inclusion clause here is refused by validation, and
		// exclusions belong to the operator rather than to the switch.
		Selector: UpdateSelector{},

		Strategy:              StrategyPatch,
		MinimumRecommendation: RecommendCaution,
		Mode:                  ModeAutomatic,

		// No window. The switch does not invent a maintenance schedule on an
		// operator's behalf; an operator who wants one writes their own policy,
		// which outranks this by construction.
		Window: MaintenanceWindow{AlwaysOpen: true},

		Failure: UpdateFailureHandling{
			AutoRollback: true,
			// Stop retrying a container that keeps failing, and do not resume
			// by itself: a cooldown of zero means a person has to acknowledge.
			PauseAfterFailures: 2,
			PauseWindowHours:   24,
			CooldownHours:      0,
			MaxRetries:         1,
		},
	}
}
