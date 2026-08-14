package domain

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// The dependency operation: one coordinated, dependency-safe provider update.
//
// # Why this record exists at all
//
// Invariant B. A provider reaching ExecutionSucceeded means ONE CONTAINER was
// replaced. It does not mean the estate is consistent: every container that
// shared that provider's namespace is, at that instant, attached to a namespace
// that no longer exists -- running, logging nothing, with no network.
//
// So "the provider succeeded" and "the dependency operation succeeded" are
// different facts, and conflating them is the exact failure the live experiment
// exposed. This record is the second fact, and it does not clear until every
// mandatory rebind has itself reached verified success.
//
// # Why it is persisted rather than held in memory
//
// A provider update plus three rebinds is minutes of work across four separate
// executions, and HarborMaster can be killed at any point in it. An in-memory
// coordinator would forget which rebinds remained and would have to guess --
// and the safe guess ("redo them all") is a second unattended mutation of
// containers that may already be correct.
//
// So progress is derivable from rows. RecoverOperation reconstructs it from the
// members and the execution records they name, and nothing about the answer
// depends on this process having been alive for any of it.
//
// # It is bookkeeping, not capability
//
// Nothing on this type reaches Docker. It records what HarborMaster decided and
// what happened; the mutations themselves belong to the acquisition, execution,
// and rollback services, which own their capabilities and re-run their own
// preflights. A row here cannot cause anything.

// DependencyOperationState is where a coordinated update has got to.
//
// A closed vocabulary. The zero value is not a member, so a row whose state was
// never set is invalid rather than defaulting into "succeeded" or "queued".
type DependencyOperationState string

// Dependency operation states.
const (
	// OperationQueued means the operation exists and its member set has been
	// established, but the provider has not been touched.
	//
	// Reaching this state is the precondition for stopping the provider: the
	// members are written BEFORE the mutation, so a crash between the two leaves
	// a record of what was supposed to happen rather than a provider nobody
	// knows was mid-operation.
	OperationQueued DependencyOperationState = "queued"

	// OperationProviderRunning means the provider's recreation is under way.
	OperationProviderRunning DependencyOperationState = "providerRunning"

	// OperationProviderVerified means the provider's execution reached verified
	// success. NOT the end of the operation -- every dependent is now attached
	// to a namespace that no longer exists.
	OperationProviderVerified DependencyOperationState = "providerVerified"

	// OperationRebindPending means the provider is verified and rebinds remain
	// to be started.
	OperationRebindPending DependencyOperationState = "rebindPending"

	// OperationRebindRunning means at least one rebind is in flight.
	OperationRebindRunning DependencyOperationState = "rebindRunning"

	// OperationSucceeded means the provider AND every mandatory rebind reached
	// verified success. The only state that means the estate is consistent.
	OperationSucceeded DependencyOperationState = "succeeded"

	// OperationFailed means the provider or a mandatory rebind did not succeed.
	//
	// The provider is NOT rolled back and successfully rebound dependents are
	// NOT reverted -- see the note on GroupRollbackIsNotPerformed.
	OperationFailed DependencyOperationState = "failed"

	// OperationBlocked means the operation could not proceed safely: a
	// dependent stopped being rebindable, the graph became unorderable, or the
	// member set could not be re-established.
	OperationBlocked DependencyOperationState = "blocked"

	// OperationInterrupted means a restart found the operation mid-flight and
	// could not place it. Distinct from failed: nothing is known to have gone
	// wrong, only that this process did not see the end of it.
	OperationInterrupted DependencyOperationState = "interrupted"
)

// DependencyOperationStates lists every state, in lifecycle order.
var DependencyOperationStates = []DependencyOperationState{
	OperationQueued, OperationProviderRunning, OperationProviderVerified,
	OperationRebindPending, OperationRebindRunning,
	OperationSucceeded, OperationFailed, OperationBlocked, OperationInterrupted,
}

// ValidDependencyOperationState reports whether value names a known state.
func ValidDependencyOperationState(value string) bool {
	for _, state := range DependencyOperationStates {
		if string(state) == value {
			return true
		}
	}
	return false
}

// Terminal reports whether the operation has reached a conclusion.
//
// An ALLOWLIST of the three endings, not a "is it still running" test. An
// unrecognised state is NOT terminal, so a state this build does not understand
// keeps being looked at rather than being quietly treated as finished.
func (s DependencyOperationState) Terminal() bool {
	switch s {
	case OperationSucceeded, OperationFailed, OperationBlocked:
		return true
	default:
		return false
	}
}

// NeedsOperator reports whether a person has to look at this.
func (s DependencyOperationState) NeedsOperator() bool {
	switch s {
	case OperationFailed, OperationBlocked, OperationInterrupted:
		return true
	default:
		return false
	}
}

// Explain renders the state in operator-facing words.
func (s DependencyOperationState) Explain() string {
	switch s {
	case OperationQueued:
		return "the containers that must be reattached have been established; nothing has been changed yet"
	case OperationProviderRunning:
		return "the shared-namespace container is being replaced"
	case OperationProviderVerified:
		return "the shared-namespace container was replaced successfully; the containers that share its namespace still need reattaching"
	case OperationRebindPending:
		return "waiting to reattach the containers that share the replaced namespace"
	case OperationRebindRunning:
		return "reattaching the containers that share the replaced namespace"
	case OperationSucceeded:
		return "the container was replaced and everything sharing its namespace was reattached"
	case OperationFailed:
		return "something that shares the replaced namespace could not be reattached"
	case OperationBlocked:
		return "HarborMaster stopped this operation because it could no longer establish that it was safe to continue"
	case OperationInterrupted:
		return "HarborMaster restarted while this operation was in progress"
	default:
		return "this operation is in a state this build does not recognise"
	}
}

// DependencyMemberState is where one mandatory rebind has got to.
//
// # Why "acquired" and "executing" are not "done"
//
// An acquisition id existing means a pull was REQUESTED. An execution id
// existing means a recreation was REQUESTED. Neither says the container is
// attached to anything. Only MemberVerified does, and it is set from the
// execution record's own terminal success -- after the health proof, the digest
// verification, the preservation comparison, and the network check.
//
// Inferring completion from the presence of an id is the specific mistake this
// vocabulary exists to make impossible.
type DependencyMemberState string

// Dependency member states.
const (
	// MemberPending means the rebind is required and has not been started.
	MemberPending DependencyMemberState = "pending"
	// MemberPlanCreated means a rebind change plan exists.
	MemberPlanCreated DependencyMemberState = "planCreated"
	// MemberAcquired means the image acquisition succeeded.
	MemberAcquired DependencyMemberState = "acquired"
	// MemberExecuting means the recreation is under way.
	MemberExecuting DependencyMemberState = "executing"
	// MemberVerified means the recreation reached verified success. The ONLY
	// state that clears a mandatory rebind.
	MemberVerified DependencyMemberState = "verified"
	// MemberBlocked means the dependent stopped being safely rebindable.
	MemberBlocked DependencyMemberState = "blocked"
	// MemberFailed means the rebind was attempted and did not succeed.
	MemberFailed DependencyMemberState = "failed"
	// MemberInterrupted means a restart found it mid-flight.
	MemberInterrupted DependencyMemberState = "interrupted"
)

// DependencyMemberStates lists every member state, in lifecycle order.
var DependencyMemberStates = []DependencyMemberState{
	MemberPending, MemberPlanCreated, MemberAcquired, MemberExecuting,
	MemberVerified, MemberBlocked, MemberFailed, MemberInterrupted,
}

// ValidDependencyMemberState reports whether value names a known state.
func ValidDependencyMemberState(value string) bool {
	for _, state := range DependencyMemberStates {
		if string(state) == value {
			return true
		}
	}
	return false
}

// Clears reports whether this member no longer holds the operation open.
//
// An allowlist of exactly ONE state. Written this way rather than as "not
// pending and not failed" so that a state added later cannot accidentally clear
// an operation by failing to appear in a deny list.
func (s DependencyMemberState) Clears() bool { return s == MemberVerified }

// Settled reports whether this member has stopped moving.
func (s DependencyMemberState) Settled() bool {
	switch s {
	case MemberVerified, MemberBlocked, MemberFailed:
		return true
	default:
		return false
	}
}

// Explain renders the member state in operator-facing words.
func (s DependencyMemberState) Explain() string {
	switch s {
	case MemberPending:
		return "waiting to be reattached"
	case MemberPlanCreated:
		return "a reattachment has been planned"
	case MemberAcquired:
		return "the image it is running has been verified locally"
	case MemberExecuting:
		return "being reattached"
	case MemberVerified:
		return "reattached and verified"
	case MemberBlocked:
		return "HarborMaster could no longer establish that this container could be reattached safely"
	case MemberFailed:
		return "the reattachment did not succeed"
	case MemberInterrupted:
		return "HarborMaster restarted while this container was being reattached"
	default:
		return "in a state this build does not recognise"
	}
}

// ------------------------------------------------------------- the records --

// DependencyOperation is one coordinated provider update.
type DependencyOperation struct {
	OperationID string `json:"operationId"`

	// Provider is the container whose namespace is shared, by stable NAME.
	Provider string `json:"provider"`
	// ProviderPlanID and ProviderExecutionID are the records the provider's own
	// update is being performed under. Both are HarborMaster-generated.
	ProviderPlanID      string `json:"providerPlanId,omitempty"`
	ProviderExecutionID string `json:"providerExecutionId,omitempty"`

	State DependencyOperationState `json:"state"`
	// Failure is the closed-vocabulary reason a non-successful operation ended.
	// Never a daemon string.
	Failure DependencyOperationFailure `json:"failure,omitempty"`

	// RequestedBy attributes the operation. Two fields only, like every other
	// asynchronous record: see Requester.
	RequestedBy Requester `json:"requestedBy,omitzero"`

	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`

	// Members are the mandatory rebinds, sorted by dependent name.
	Members []DependencyMember `json:"members,omitempty"`
}

// DependencyOperationFailure is why an operation did not succeed.
//
// A closed vocabulary so the API and the UI can branch, and so no daemon text
// ever reaches a stored reason.
type DependencyOperationFailure string

// Dependency operation failures.
const (
	OperationFailureNone DependencyOperationFailure = ""
	// OperationFailureProvider means the provider's own update did not succeed.
	OperationFailureProvider DependencyOperationFailure = "providerFailed"
	// OperationFailureRebind means a mandatory rebind did not succeed.
	OperationFailureRebind DependencyOperationFailure = "rebindFailed"
	// OperationFailureNotRebindable means a dependent stopped being safely
	// rebindable after the operation was created.
	OperationFailureNotRebindable DependencyOperationFailure = "dependentNotRebindable"
	// OperationFailureEvidence means the member set could not be re-established.
	OperationFailureEvidence DependencyOperationFailure = "evidenceUnavailable"
)

// DependencyOperationFailures lists every failure.
var DependencyOperationFailures = []DependencyOperationFailure{
	OperationFailureProvider, OperationFailureRebind,
	OperationFailureNotRebindable, OperationFailureEvidence,
}

// ValidDependencyOperationFailure reports whether value names a known failure.
func ValidDependencyOperationFailure(value string) bool {
	if value == "" {
		return true
	}
	for _, failure := range DependencyOperationFailures {
		if string(failure) == value {
			return true
		}
	}
	return false
}

// Explain renders the failure in HarborMaster's own words.
func (f DependencyOperationFailure) Explain() string {
	switch f {
	case OperationFailureNone:
		return ""
	case OperationFailureProvider:
		return "the shared-namespace container could not be replaced"
	case OperationFailureRebind:
		return "a container sharing the replaced namespace could not be reattached"
	case OperationFailureNotRebindable:
		return "a container sharing the replaced namespace stopped being one HarborMaster could reattach safely"
	case OperationFailureEvidence:
		return "HarborMaster could no longer establish which containers share the replaced namespace"
	default:
		return "the operation failed for a reason this build does not recognise"
	}
}

// DependencyMember is one mandatory rebind within an operation.
type DependencyMember struct {
	OperationID string `json:"operationId"`
	// Dependent is the container to be reattached, by stable NAME.
	Dependent string `json:"dependent"`
	// Source is which namespace the two containers share. Always a HARD source:
	// an operator relationship constrains order and never requires a rebind.
	Source DependencySource `json:"source"`
	// Provider is the container it must reattach to, by stable NAME.
	Provider string `json:"provider"`

	// ExpectedProviderID is the provider container id the dependent NAMED when
	// the operation was created -- the one about to become stale.
	ExpectedProviderID string `json:"expectedProviderId,omitempty"`
	// TargetProviderID is the replacement's id, once the provider's recreation
	// has established one.
	TargetProviderID string `json:"targetProviderId,omitempty"`

	// PlanID, AcquisitionID and ExecutionID are the records the rebind is being
	// performed under. Their PRESENCE says work was requested; only State says
	// what happened.
	PlanID        string `json:"planId,omitempty"`
	AcquisitionID string `json:"acquisitionId,omitempty"`
	ExecutionID   string `json:"executionId,omitempty"`

	State DependencyMemberState `json:"state"`
	// Refusal is the closed-vocabulary reason a blocked member was blocked.
	Refusal RebindRefusal `json:"refusal,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SortDependencyMembers orders members deterministically by dependent name.
func SortDependencyMembers(members []DependencyMember) {
	sort.SliceStable(members, func(i, j int) bool {
		return members[i].Dependent < members[j].Dependent
	})
}

// ------------------------------------------------------ derived conclusions --

// Complete reports whether every mandatory rebind has reached verified success.
//
// DERIVED from the members on every call rather than stored as a flag.
//
// That is deliberate and it is the whole restart story. A stored "complete"
// boolean is a second copy of a fact, and a process killed between writing the
// last member and writing the flag would leave the two disagreeing -- with the
// flag, the thing everything reads, being the wrong one. Recomputing cannot
// disagree with itself.
//
// An operation with NO members is not complete by this test unless the provider
// also succeeded; see Successful.
func (o DependencyOperation) Complete() bool {
	for _, member := range o.Members {
		if !member.State.Clears() {
			return false
		}
	}
	return true
}

// Successful is the exact definition of dependency-operation success.
//
// BOTH halves, and neither implies the other:
//
//  1. the provider's own update reached verified success, AND
//  2. every mandatory hard-dependent rebind reached verified success.
//
// A provider execution in ExecutionSucceeded with one member still pending is
// NOT a successful operation, and this method is the one place that is decided.
func (o DependencyOperation) Successful(providerVerified bool) bool {
	return providerVerified && o.Complete()
}

// Outstanding returns the members that still hold the operation open, sorted.
//
// The answer restart recovery needs: what remains to be done. A member that is
// blocked or failed does NOT appear here -- it has stopped moving, and the
// operation is failed rather than waiting.
func (o DependencyOperation) Outstanding() []DependencyMember {
	var outstanding []DependencyMember
	for _, member := range o.Members {
		if member.State.Clears() || member.State.Settled() {
			continue
		}
		outstanding = append(outstanding, member)
	}
	SortDependencyMembers(outstanding)
	return outstanding
}

// Unsuccessful returns the members that settled without succeeding, sorted.
func (o DependencyOperation) Unsuccessful() []DependencyMember {
	var bad []DependencyMember
	for _, member := range o.Members {
		if member.State.Settled() && !member.State.Clears() {
			bad = append(bad, member)
		}
	}
	SortDependencyMembers(bad)
	return bad
}

// GroupRollbackIsNotPerformed documents a load-bearing design choice.
//
// # Phase 16 introduces no automatic group rollback
//
// When a provider succeeds and a dependent's rebind fails, HarborMaster does
// NOT roll the provider back, and does NOT revert dependents that were already
// reattached successfully.
//
// Three reasons, each sufficient:
//
//  1. Rolling the provider back is a THIRD unattended mutation, decided at the
//     moment HarborMaster has just demonstrated that its model of the host was
//     wrong. That is the same reasoning that made the recreation pipeline refuse
//     to undo its own work.
//  2. Dependents already rebound are attached to the provider's NEW identity.
//     Rolling the provider back would break exactly the containers that are
//     currently correct -- turning one broken container into several.
//  3. The failed dependent already has recovery semantics of its own. Its
//     execution parked the original, quarantined the replacement, and recorded a
//     manual recovery plan. Inventing a second mechanism on top would produce
//     two components trying to repair the same container.
//
// So the operation is marked failed, the operator is told precisely which
// container is still attached to the old namespace, and nothing further moves.
//
// This function exists to be cited. It is referenced by the tests that pin the
// behaviour, so deleting the policy means deleting something a test names.
func GroupRollbackIsNotPerformed() string {
	return "HarborMaster does not undo a dependency operation automatically: the " +
		"replaced container stays replaced, containers already reattached stay " +
		"reattached, and the one that failed keeps its own recovery record"
}

// ------------------------------------------------------------ identifiers --

// DependencyOperationIDPrefix is the fixed prefix of a generated operation id.
const DependencyOperationIDPrefix = "depop_"

// DependencyOperationIDHexLength is how many hex characters follow the prefix.
const DependencyOperationIDHexLength = 20

// NewDependencyOperationID generates an immutable public identifier.
func NewDependencyOperationID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return DependencyOperationIDPrefix + hex.EncodeToString(raw[:])
}

// ValidDependencyOperationID reports whether id has the shape of a generated id.
func ValidDependencyOperationID(id string) bool {
	if len(id) != len(DependencyOperationIDPrefix)+DependencyOperationIDHexLength {
		return false
	}
	if !strings.HasPrefix(id, DependencyOperationIDPrefix) {
		return false
	}
	for _, char := range id[len(DependencyOperationIDPrefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
