package domain

import "time"

// Plan approval: the record that a person reviewed one immutable change plan.
//
// # The two facts this design keeps apart
//
//	planner fact    this exact proposed update requires human review
//	approval fact   an authorised human reviewed this exact update and approved it
//
// Nothing here touches the first. An approval does not lower a risk score, does
// not remove a factor, does not change a recommendation, and does not mutate a
// ChangePlan -- a plan is insert-only and stays exactly as the planner wrote it.
// The approval sits NEXT to it.
//
// That separation is what makes the audit trail mean anything. A `proceed` plan
// executing tells you the model was comfortable; a `manualReview` plan executing
// with an approval row tells you a named person looked at that exact digest and
// said yes. Collapsing them would lose the second sentence entirely.
//
// # Why the plan is the binding, and not the container
//
// A container is a moving target: its image changes, its configuration changes,
// and the planner rewrites its assessment whenever the evidence moves. Binding
// approval to a container would let a human judgement about one proposed change
// authorise a completely different one later.
//
// `plan_id` names one immutable assessment, permanently. Everything else the
// approval needs -- the container, the image, the digests, the recommendation --
// is derivable from it, so none of it is stored again as authority.

// PlanApprovalState is what an approval currently is.
//
// Two values, deliberately. `superseded` is derivable -- a plan that is no
// longer current is superseded whether or not a row says so -- and `consumed` is
// derivable too, from whether an execution of the plan has mutated the host.
// Storing either would duplicate a fact that changes without this table being
// told, which is how a state machine goes stale.
type PlanApprovalState string

const (
	// PlanApprovalActive is a standing human authorisation for one plan.
	PlanApprovalActive PlanApprovalState = "active"
	// PlanApprovalRevoked is one a person withdrew.
	PlanApprovalRevoked PlanApprovalState = "revoked"
)

// PlanApprovalStates lists every value, for the vocabulary tests.
var PlanApprovalStates = []PlanApprovalState{
	PlanApprovalActive, PlanApprovalRevoked,
}

// ValidPlanApprovalState reports whether name is a known state.
func ValidPlanApprovalState(name string) bool {
	for _, state := range PlanApprovalStates {
		if string(state) == name {
			return true
		}
	}
	return false
}

// PlanApproval is one human authorisation of one immutable plan.
type PlanApproval struct {
	ID int64 `json:"-"`

	// PlanID is the authority. Everything else about the change is read from
	// the plan it names.
	PlanID string            `json:"planId"`
	State  PlanApprovalState `json:"state"`

	// The comparison tripwires.
	//
	// NEVER read as authority and never supplied by a caller: they are recorded
	// from the plan at approval time and compared against the plan again at
	// execution time. A mismatch means something rewrote an insert-only row,
	// which is a condition that must refuse rather than proceed.
	//
	// They exist because "the plan is never updated" is currently upheld by the
	// absence of an update method rather than by a constraint, and digest
	// substitution is the one attack this feature would otherwise enable.
	ApprovedInputDigest    string `json:"-"`
	ApprovedProposedDigest string `json:"-"`

	ApprovedBy Requester `json:"approvedBy,omitzero"`
	ApprovedAt time.Time `json:"approvedAt"`

	RevokedBy Requester  `json:"revokedBy,omitzero"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// Active reports whether this approval still authorises anything.
//
// State only. Whether it is still VALID for a given plan is a larger question
// answered by PlanApprovalValid, which needs the plan too.
func (a PlanApproval) Active() bool {
	return a.State == PlanApprovalActive && a.RevokedAt == nil
}

// PlanApprovalRefusal is why an approval could not be granted or used.
type PlanApprovalRefusal string

const (
	// PlanApprovalRefusalNone means it is valid.
	PlanApprovalRefusalNone PlanApprovalRefusal = ""
	// PlanApprovalRefusalNotApprovable means the plan's recommendation is not
	// one a human review can authorise.
	PlanApprovalRefusalNotApprovable PlanApprovalRefusal = "notApprovable"
	// PlanApprovalRefusalSuperseded means a newer plan replaced this one.
	PlanApprovalRefusalSuperseded PlanApprovalRefusal = "superseded"
	// PlanApprovalRefusalRevoked means a person withdrew it.
	PlanApprovalRefusalRevoked PlanApprovalRefusal = "revoked"
	// PlanApprovalRefusalEvidenceChanged means a tripwire did not match.
	PlanApprovalRefusalEvidenceChanged PlanApprovalRefusal = "evidenceChanged"
	// PlanApprovalRefusalAlreadyActed means an execution of this plan has
	// already changed the host, so the approval is spent.
	PlanApprovalRefusalAlreadyActed PlanApprovalRefusal = "alreadyActed"
)

// Explain renders a refusal in HarborMaster's own words.
func (r PlanApprovalRefusal) Explain() string {
	switch r {
	case PlanApprovalRefusalNotApprovable:
		return "this plan does not ask for human review, so there is nothing to approve"
	case PlanApprovalRefusalSuperseded:
		return "a newer change plan has replaced the one that was approved"
	case PlanApprovalRefusalRevoked:
		return "the approval for this plan was withdrawn"
	case PlanApprovalRefusalEvidenceChanged:
		return "the plan's evidence no longer matches what was approved"
	case PlanApprovalRefusalAlreadyActed:
		return "this approval has already been used to change the container, " +
			"so applying it again needs a fresh review"
	default:
		return ""
	}
}

// PlanApprovable reports whether a plan is one this workflow may approve.
//
// # Only manualReview
//
// `proceed` and `proceedWithCaution` need no approval -- automation and the
// manual path both act on them already, and offering a button would imply the
// opposite. `notRecommended` is the model arguing against the change and
// `unknown` is a gap in evidence; neither is something a person can vouch for by
// clicking, and admitting them would turn plan approval into a generic override.
//
// So exactly one verdict is approvable, and the refusal for the rest is a fact
// about the plan rather than about the operator.
func PlanApprovable(recommendation Recommendation) bool {
	return recommendation == RecommendManualReview
}

// PlanApprovalValid decides whether an approval authorises a plan right now.
//
// # One validator, used everywhere
//
// The execution preflight, the read endpoint and the UI must all agree about
// what "approved" means, and the way to guarantee that is for there to be one
// function. Pure, so it can be exercised exhaustively without a database.
//
// `mutatedAlready` is the derived consumption rule: an approval is authority for
// its plan until an execution of that plan has actually changed the host. After
// that, applying the same plan again is a new decision and needs a new one --
// the caller reads that fact from `executions.mutated_at` rather than from a
// state stored here.
//
// `currentPlanID` is the plan that is current for the container, and
// `currentResolved` says whether that was ESTABLISHED. The pair is deliberate:
// an empty id means two opposite things, and reading it as one of them was a
// fail-open.
//
//	resolved, id set    that plan is current
//	resolved, id empty  NOTHING is current -- the container has no plan, or
//	                    settled registry evidence retired the one it had
//	unresolved          currency could not be established, so this asserts
//	                    nothing and the other checks decide
//
// A retired plan reaches here as (resolved, empty). Before C3B that was
// indistinguishable from "could not look", and the check was skipped -- so an
// approval granted while the registry was still pending stayed valid after the
// registry answered "nothing newer". It authorised a change that no longer
// existed.
func PlanApprovalValid(
	approval PlanApproval,
	plan ChangePlan,
	currentPlanID string,
	currentResolved bool,
	mutatedAlready bool,
) PlanApprovalRefusal {
	switch {
	case !approval.Active():
		return PlanApprovalRefusalRevoked
	case approval.PlanID != plan.PlanID:
		// Not this plan's approval. Cannot happen through the repository, which
		// looks up by plan, and checked anyway: an approval that authorised a
		// plan it does not name is the whole failure mode.
		return PlanApprovalRefusalSuperseded
	case !PlanApprovable(plan.Risk.Recommendation):
		return PlanApprovalRefusalNotApprovable
	case currentResolved && currentPlanID != plan.PlanID:
		// Covers both "a different plan is current" and "no plan is current at
		// all". The second is what a settled comparison produces: the planner
		// would not write this plan today, so nothing stands behind it.
		return PlanApprovalRefusalSuperseded
	case approval.ApprovedInputDigest != "" &&
		approval.ApprovedInputDigest != plan.InputDigest:
		return PlanApprovalRefusalEvidenceChanged
	case approval.ApprovedProposedDigest != "" &&
		approval.ApprovedProposedDigest != plan.ProposedDigest:
		return PlanApprovalRefusalEvidenceChanged
	case mutatedAlready:
		return PlanApprovalRefusalAlreadyActed
	default:
		return PlanApprovalRefusalNone
	}
}
