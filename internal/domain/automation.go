package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// What the automation engine decided, and why.
//
// # Every pass is a record, including the passes that did nothing
//
// The hardest question an operator asks an automation system is "why did you
// not update that container", and the second hardest is "why did you update
// that one". Both are unanswerable unless the reasoning is recorded at the
// moment it happened, from the evidence it happened on.
//
// So every scheduler pass writes a run, every container it considered gets a
// decision with a closed-vocabulary reason, and both survive the containers
// they describe.

// AutomationRunIDPrefix and AutomationRunIDHexLength shape a run identifier.
const (
	AutomationRunIDPrefix    = "run_"
	AutomationRunIDHexLength = 20
)

// NewAutomationRunID generates a run identifier.
func NewAutomationRunID() string {
	var raw [AutomationRunIDHexLength / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("automation run id: " + err.Error())
	}
	return AutomationRunIDPrefix + hex.EncodeToString(raw[:])
}

// ValidAutomationRunID reports whether id has the generated shape.
func ValidAutomationRunID(id string) bool {
	if len(id) != len(AutomationRunIDPrefix)+AutomationRunIDHexLength {
		return false
	}
	if id[:len(AutomationRunIDPrefix)] != AutomationRunIDPrefix {
		return false
	}
	for _, r := range id[len(AutomationRunIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// --------------------------------------------------------------- verdicts --

// AutomationVerdict is what the engine concluded for one container.
type AutomationVerdict string

const (
	// VerdictUpdate means the container is eligible and work was submitted.
	VerdictUpdate AutomationVerdict = "update"
	// VerdictWouldUpdate means eligible, but the mode is observe or dry run.
	VerdictWouldUpdate AutomationVerdict = "wouldUpdate"
	// VerdictAwaitingApproval means eligible, but a person must release it.
	VerdictAwaitingApproval AutomationVerdict = "awaitingApproval"
	// VerdictSkip means not eligible. The reason says which check declined.
	VerdictSkip AutomationVerdict = "skip"
)

// AutomationReason is the closed vocabulary of "why not", and "why".
//
// A closed set so a UI can branch and an operator can filter. Every one of
// these is a sentence HarborMaster can defend from its own records.
type AutomationReason string

const (
	// Positive outcomes.
	ReasonEligible AutomationReason = "eligible"

	// No work to do.
	ReasonNoPlan      AutomationReason = "noPlan"
	ReasonNoUpdate    AutomationReason = "noUpdate"
	ReasonNotSelected AutomationReason = "notSelected"
	// ReasonNotEligible is a container a BROAD policy passed over.
	//
	// Its own reason rather than notSelected, because the two answer different
	// questions. "No selector names this container" tells an operator to write
	// one. "A policy covering all eligible containers did not enrol this one"
	// tells them something about the CONTAINER, and the detail says which fact
	// decided it -- HarborMaster's own container, evidence from an earlier
	// update, an opt-out label, or a workload it could not have recreated.
	ReasonNotEligible  AutomationReason = "notEligible"
	ReasonNoPolicy     AutomationReason = "noPolicy"
	ReasonPolicyOff    AutomationReason = "policyDisabled"
	ReasonLabelOff     AutomationReason = "labelDisabled"
	ReasonLabelPaused  AutomationReason = "labelPaused"
	ReasonObserveMode  AutomationReason = "observeMode"
	ReasonDryRunMode   AutomationReason = "dryRunMode"
	ReasonNeedApproval AutomationReason = "approvalRequired"

	// Declined by evidence.
	ReasonStrategy       AutomationReason = "strategyCeiling"
	ReasonRecommendation AutomationReason = "recommendation"
	ReasonWindowClosed   AutomationReason = "windowClosed"
	ReasonWindowInvalid  AutomationReason = "windowUnresolvable"
	ReasonPaused         AutomationReason = "automationPaused"
	ReasonInFlight       AutomationReason = "alreadyInFlight"
	ReasonConcurrency    AutomationReason = "concurrencyLimit"
	ReasonRegistryLimit  AutomationReason = "registryLimit"
	ReasonRunLimit       AutomationReason = "runLimit"

	// Declined by a downstream service. The engine records the refusal it was
	// given rather than reinterpreting it.
	ReasonRefused AutomationReason = "refusedByService"
	ReasonError   AutomationReason = "error"

	// ReasonSelfUpdate is the container HarborMaster is running in.
	//
	// Its own category rather than a refusal, because the remedy is different
	// from every other reason here: no policy, window, or approval makes it
	// succeed. HarborMaster is updated from outside itself.
	ReasonSelfUpdate AutomationReason = "selfUpdate"

	// Declined by a dependency.
	//
	// Six reasons rather than one, and deliberately not folded into
	// ReasonRefused. A dependency block is not a service refusing and not an
	// error: HarborMaster looked at what this container needs and correctly
	// declined to move it yet. Each of these is a different sentence and a
	// different remedy, and an operator filtering for "why did that not update"
	// needs to tell them apart.
	//
	// The full vocabulary and its meanings live on domain.DependencyState; these
	// are the automation-decision spellings of the same six answers.
	ReasonDependencyWaiting    AutomationReason = "dependencyWaiting"
	ReasonDependencyBlocked    AutomationReason = "dependencyBlocked"
	ReasonDependencyCycle      AutomationReason = "dependencyCycle"
	ReasonDependencyMissing    AutomationReason = "dependencyMissing"
	ReasonDependencyIneligible AutomationReason = "dependencyIneligible"
	// ReasonDependentsNotRebindable is invariant A: this container is a hard
	// namespace provider and something that shares its namespace could not be
	// established as repairable, so stopping it would break that container
	// silently.
	ReasonDependentsNotRebindable AutomationReason = "dependentsNotRebindable"
)

// AutomationReasons lists every reason a decision can carry.
//
// # Why the vocabulary is enumerable
//
// Because it is rendered. Every one of these becomes a sentence in the
// Automation page, and a value the renderer does not know shows an operator the
// raw identifier. That had already happened once: the seven reasons below
// `ReasonSelfUpdate` were added without the published schema or the frontend
// map learning about them, so a container held by a dependency reported
// `dependencyWaiting` verbatim.
//
// With a list, the schema and the client can both be pinned against it by test
// rather than by memory. See TestAutomationReasonsMatchThePublishedSchema.
//
// Order is the declaration order above: positive, no-work, declined, refused,
// self, dependency.
var AutomationReasons = []AutomationReason{
	ReasonEligible,

	ReasonNoPlan, ReasonNoUpdate, ReasonNotSelected, ReasonNotEligible,
	ReasonNoPolicy, ReasonPolicyOff, ReasonLabelOff, ReasonLabelPaused,
	ReasonObserveMode, ReasonDryRunMode, ReasonNeedApproval,

	ReasonStrategy, ReasonRecommendation, ReasonWindowClosed,
	ReasonWindowInvalid, ReasonPaused, ReasonInFlight, ReasonConcurrency,
	ReasonRegistryLimit, ReasonRunLimit,

	ReasonRefused, ReasonError,

	ReasonSelfUpdate,

	ReasonDependencyWaiting, ReasonDependencyBlocked, ReasonDependencyCycle,
	ReasonDependencyMissing, ReasonDependencyIneligible,
	ReasonDependentsNotRebindable,
}

// ValidAutomationReason reports whether value names a known reason.
func ValidAutomationReason(value string) bool {
	for _, reason := range AutomationReasons {
		if string(reason) == value {
			return true
		}
	}
	return false
}

// DependencyReasonFor maps a dependency state onto its automation reason.
//
// One mapping, in one place, so the two closed vocabularies cannot drift. An
// unrecognised state maps to ReasonDependencyBlocked: a state this build does
// not understand is not a state that may proceed.
func DependencyReasonFor(state DependencyState) AutomationReason {
	switch state {
	case DependencyWaiting:
		return ReasonDependencyWaiting
	case DependencyBlocked:
		return ReasonDependencyBlocked
	case DependencyCycle:
		return ReasonDependencyCycle
	case DependencyMissing:
		return ReasonDependencyMissing
	case DependencyIneligible:
		return ReasonDependencyIneligible
	default:
		return ReasonDependencyBlocked
	}
}

// Explain renders a reason in operator-facing words.
func (r AutomationReason) Explain() string {
	switch r {
	case ReasonEligible:
		return "every check passed and the update was submitted"
	case ReasonNoPlan:
		return "no current change plan exists for this container"
	case ReasonNoUpdate:
		return "the plan proposes no change"
	case ReasonNotSelected:
		return "no update policy selects this container"
	case ReasonNotEligible:
		return "this container is not one a broad policy may enrol"
	case ReasonNoPolicy:
		return "no update policy is defined"
	case ReasonPolicyOff:
		return "the governing policy is disabled"
	case ReasonLabelOff:
		return "the container is opted out by label"
	case ReasonLabelPaused:
		return "the container is paused by label"
	case ReasonObserveMode:
		return "the policy is in observe mode, so nothing was changed"
	case ReasonDryRunMode:
		return "the policy is in dry-run mode, so nothing was changed"
	case ReasonNeedApproval:
		return "the policy requires a person to approve each update"
	case ReasonStrategy:
		return "the change is larger than the policy's strategy permits"
	case ReasonRecommendation:
		return "the planner's recommendation is below what the policy requires"
	case ReasonWindowClosed:
		return "the maintenance window is closed"
	case ReasonWindowInvalid:
		return "the maintenance window could not be evaluated, so it is treated as closed"
	case ReasonPaused:
		return "automation is paused for this container after repeated failures"
	case ReasonInFlight:
		return "work is already in flight for this container"
	case ReasonConcurrency:
		return "the concurrency limit is reached"
	case ReasonRegistryLimit:
		return "the per-registry concurrency limit is reached"
	case ReasonRunLimit:
		return "this pass has already started as many updates as it may"
	case ReasonRefused:
		return "a downstream service refused the request"
	case ReasonError:
		return "the decision could not be completed"
	case ReasonSelfUpdate:
		return "this is the container HarborMaster is running in, and it cannot update itself"

	// The dependency answers. Each defers to the state vocabulary's own
	// sentence, so the six spellings cannot drift from the six meanings.
	case ReasonDependencyWaiting:
		return DependencyWaiting.Explain()
	case ReasonDependencyBlocked:
		return DependencyBlocked.Explain()
	case ReasonDependencyCycle:
		return DependencyCycle.Explain()
	case ReasonDependencyMissing:
		return DependencyMissing.Explain()
	case ReasonDependencyIneligible:
		return DependencyIneligible.Explain()
	case ReasonDependentsNotRebindable:
		return "another container shares this one's namespace and HarborMaster could " +
			"not establish that it could be reattached afterwards, so stopping this " +
			"container would break it"

	default:
		return string(r)
	}
}

// -------------------------------------------------------------- decision --

// AutomationDecision is one container's outcome in one pass.
//
// Every identity here is copied from HarborMaster's own records. Nothing on
// this type is ever filled from a caller.
type AutomationDecision struct {
	ID    int64  `json:"-"`
	RunID string `json:"runId"`

	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`

	// PolicyID is the policy that governed the decision, when one did.
	PolicyID   string `json:"policyId,omitempty"`
	PolicyName string `json:"policyName,omitempty"`

	Verdict AutomationVerdict `json:"verdict"`
	Reason  AutomationReason  `json:"reason"`

	// ContainerState is the lifecycle state the pass saw.
	//
	// Recorded for one question: whether a container that needed no update was
	// nevertheless RUNNING, which is what releases anything depending on it.
	// "Present" is not "stable", and a dependent released on a stopped provider
	// would be released on nothing.
	ContainerState ContainerState `json:"containerState,omitempty"`

	// DependencyState is the dependency verdict for this container.
	//
	// Always populated once the gate has run, even when the gate changed
	// nothing: an operator asking why a container is waiting needs the answer
	// whether or not the decision was altered.
	//
	// Distinct from Reason. Reason says what the ENGINE concluded;
	// DependencyState says what its dependencies looked like, and a container
	// can be skipped for an unrelated reason while its dependencies are fine.
	DependencyState DependencyState `json:"dependencyState,omitempty"`
	// BlockedBy names the container responsible, when one is.
	//
	// A NAME, and one HarborMaster read from its own inventory -- never text
	// from a daemon and never caller input.
	BlockedBy string `json:"blockedBy,omitempty"`
	// Detail is HarborMaster's own sentence. Never a daemon or registry string.
	Detail string `json:"detail,omitempty"`

	// The change that was, or would have been, applied.
	PlanID         string         `json:"planId,omitempty"`
	CurrentImage   string         `json:"currentImage,omitempty"`
	ProposedImage  string         `json:"proposedImage,omitempty"`
	ProposedDigest string         `json:"proposedDigest,omitempty"`
	UpdateType     UpdateType     `json:"updateType,omitempty"`
	Recommendation Recommendation `json:"recommendation,omitempty"`

	// The records the engine created, when it acted. Empty in observe and dry
	// run, which is the whole difference between them and automatic.
	AcquisitionID string `json:"acquisitionId,omitempty"`
	ExecutionID   string `json:"executionId,omitempty"`
	RollbackID    string `json:"rollbackId,omitempty"`

	// Position is this decision's place in the pass's execution order, so a
	// dry run can render "what would happen, in what sequence".
	Position int `json:"position"`

	DecidedAt time.Time `json:"decidedAt"`
}

// Acted reports whether the decision changed anything.
func (d AutomationDecision) Acted() bool { return d.AcquisitionID != "" }

// ------------------------------------------------------------------- run --

// AutomationRunState is where one pass got to.
type AutomationRunState string

const (
	RunRunning   AutomationRunState = "running"
	RunCompleted AutomationRunState = "completed"
	RunFailed    AutomationRunState = "failed"
	// RunInterrupted marks a pass a restart cut short. A pass changes nothing
	// itself -- it submits work to services that checkpoint their own -- so an
	// interrupted run is a bookkeeping gap, never a host in an unknown state.
	RunInterrupted AutomationRunState = "interrupted"
)

// AutomationTrigger is what started a pass.
//
// Named apart from the inventory RefreshTrigger constants deliberately: the two
// vocabularies overlap in wording but not in meaning, and a shared name would
// let one be passed where the other belongs.
type AutomationTrigger string

const (
	AutoTriggerSchedule AutomationTrigger = "schedule"
	AutoTriggerManual   AutomationTrigger = "manual"
	AutoTriggerDryRun   AutomationTrigger = "dryRun"
	AutoTriggerStartup  AutomationTrigger = "startup"
)

// ValidAutomationTrigger reports whether a trigger is one this build knows.
func ValidAutomationTrigger(value string) bool {
	switch AutomationTrigger(value) {
	case AutoTriggerSchedule, AutoTriggerManual, AutoTriggerDryRun, AutoTriggerStartup:
		return true
	default:
		return false
	}
}

// AutomationRun is one scheduler pass.
type AutomationRun struct {
	ID    int64  `json:"-"`
	RunID string `json:"runId"`

	Trigger AutomationTrigger  `json:"trigger"`
	State   AutomationRunState `json:"state"`
	// DryRun marks a pass that could never have acted, whatever the policies
	// said. Recorded on the run so a history is readable without joining every
	// decision back to the policy mode at the time.
	DryRun bool `json:"dryRun"`

	// Counters. Considered is every container examined; the rest partition it.
	Considered int `json:"considered"`
	Eligible   int `json:"eligible"`
	Submitted  int `json:"submitted"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`

	// RequestedBy is the account that asked for a manual pass. Empty for a
	// scheduled one, which nobody asked for.
	RequestedBy Requester `json:"requestedBy,omitzero"`

	Message string `json:"message,omitempty"`

	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	DurationMs  int64      `json:"durationMs"`
}

// ---------------------------------------------------------------- pausing --

// PauseReason is why automation stopped for a container.
type PauseReason string

const (
	// PauseRepeatedFailure is the automatic case: too many failures in the
	// policy's window.
	PauseRepeatedFailure PauseReason = "repeatedFailure"
	// PauseRolledBack means an update was automatically rolled back. Always a
	// pause, whatever the failure count says: a rollback means the change was
	// wrong AND the host was moved twice, and retrying that on a timer is how
	// a bad image becomes an outage loop.
	PauseRolledBack PauseReason = "automaticRollback"
	// PauseOperator is a person pausing it by hand.
	PauseOperator PauseReason = "operator"
)

// Explain renders a pause reason.
func (p PauseReason) Explain() string {
	switch p {
	case PauseRepeatedFailure:
		return "automation paused after repeated failures"
	case PauseRolledBack:
		return "automation paused after an update was rolled back"
	case PauseOperator:
		return "automation paused by an operator"
	default:
		return string(p)
	}
}

// PausedContainer is one container automation will not touch.
type PausedContainer struct {
	ID int64 `json:"-"`

	ContainerName string `json:"containerName"`
	// ContainerID is the container as it was when the pause was recorded. It
	// changes on every recreation, so the NAME is the identity a pause is
	// keyed on and this is kept for the audit trail only.
	ContainerID string `json:"containerId,omitempty"`

	Reason PauseReason `json:"reason"`
	Detail string      `json:"detail,omitempty"`

	// Failures is how many were counted when the pause was applied.
	Failures int `json:"failures"`

	PolicyID string `json:"policyId,omitempty"`
	// RollbackID links a rollback-triggered pause to the record that caused it.
	RollbackID  string `json:"rollbackId,omitempty"`
	ExecutionID string `json:"executionId,omitempty"`

	PausedAt time.Time `json:"pausedAt"`
	// ResumeAfter is when a cooldown lets automation resume by itself. Nil
	// means it never does, and a person must acknowledge -- the default, and
	// the safer reading of "this went wrong repeatedly".
	ResumeAfter *time.Time `json:"resumeAfter,omitempty"`

	// Acknowledged records a person clearing the pause.
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	AcknowledgedBy Requester  `json:"acknowledgedBy,omitzero"`
}

// Active reports whether the pause still blocks automation at an instant.
func (p PausedContainer) Active(at time.Time) bool {
	if p.AcknowledgedAt != nil {
		return false
	}
	if p.ResumeAfter == nil {
		// No cooldown: only an acknowledgement clears it.
		return true
	}
	return at.Before(*p.ResumeAfter)
}

// ------------------------------------------------------------- summaries --

// AutomationStatus is the engine's current state, for the dashboard.
type AutomationStatus struct {
	// Enabled reports whether the engine is switched on at all.
	Enabled bool `json:"enabled"`
	// Running reports a pass in flight right now.
	Running bool `json:"running"`

	Policies         int `json:"policies"`
	EnabledPolicies  int `json:"enabledPolicies"`
	PausedContainers int `json:"pausedContainers"`
	// AwaitingApproval counts decisions a person has to release.
	AwaitingApproval int `json:"awaitingApproval"`

	LastRunAt   *time.Time `json:"lastRunAt,omitempty"`
	NextRunAt   *time.Time `json:"nextRunAt,omitempty"`
	LastRunID   string     `json:"lastRunId,omitempty"`
	LastOutcome string     `json:"lastOutcome,omitempty"`

	// WindowOpen reports whether ANY enabled policy's window admits work now,
	// and NextWindowOpensAt when the soonest closed one opens. Both are what
	// the dashboard needs to say "automation is waiting for 02:00".
	WindowOpen         bool       `json:"windowOpen"`
	NextWindowOpensAt  *time.Time `json:"nextWindowOpensAt,omitempty"`
	NextWindowPolicyID string     `json:"nextWindowPolicyId,omitempty"`

	// Self is the container HarborMaster believes it is running in, and which
	// it therefore refuses to update.
	//
	// Reported so the exclusion is VISIBLE rather than mysterious. An operator
	// whose HarborMaster container is on an old image and never appears in a
	// plan should be able to read why, and an operator running outside a
	// container should be able to see that HarborMaster knows that too -- the
	// zero value is an honest "no identity", which matches nothing.
	Self SelfIdentity `json:"self"`
}

// AutomationRunSummary is the history aggregate.
type AutomationRunSummary struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Submitted int `json:"submitted"`
}
