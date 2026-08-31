package domain

import "time"

// When an image HarborMaster superseded may be removed from the host.
//
// # Cleanup is subordinate to recovery
//
// Every other decision in this package answers "may HarborMaster change this
// container". This one answers "may HarborMaster destroy the thing that would
// put it back". Those are not symmetrical questions, and the asymmetry is the
// whole design:
//
//	A wrongly retained image costs disk space.
//	A wrongly removed image costs the ability to recover.
//
// So this function has ONE failure direction. Every uncertainty, every missing
// record, every unreadable source, every state it does not recognise returns
// Retain. There is no path through it that reaches Eligible without positive
// evidence of every safety condition at once.
//
// # Why it is a pure function
//
// The same reason DecideAutomation is: a decision that can destroy recovery
// evidence must be exercisable exhaustively without a Docker socket, without a
// database, and without a clock that moves on its own. Everything it needs is
// in its argument, and the argument is assembled by a caller that holds the
// capability. This function holds none.
//
// # What it deliberately does not do
//
// It does not decide WHICH images exist, does not talk to Docker, and cannot
// remove anything. It answers one question about one image, and the service
// that acts on the answer re-establishes the fast-moving half immediately
// before it acts. See ImageRetentionDecision.

// ImageRetentionVerdict is the answer.
type ImageRetentionVerdict string

const (
	// RetainImage means the image must not be removed. The default, and the
	// value every uncertain path produces.
	RetainImage ImageRetentionVerdict = "retain"
	// ImageEligibleForRemoval means every retention condition was positively
	// established as absent. It is permission to CONSIDER removal, not an
	// instruction: the caller re-checks the volatile conditions immediately
	// before acting, and Docker itself is the final authority.
	ImageEligibleForRemoval ImageRetentionVerdict = "eligible"
)

// ImageRetentionReason is why an image is being kept.
//
// A closed vocabulary, because these are HarborMaster's own words and they end
// up in an audit record. Nothing here is a daemon or registry string.
type ImageRetentionReason string

const (
	// ImageRetentionNone accompanies an eligible verdict.
	ImageRetentionNone ImageRetentionReason = ""

	// ---- the image is in use, or may be about to be ---------------------

	// ImageRetainedInUse means a present container is running it. The most
	// common reason by far, and the one Docker would refuse on anyway.
	ImageRetainedInUse ImageRetentionReason = "inUse"
	// ImageRetainedSelf means it is the image backing HarborMaster itself.
	// Refused whatever else is true: a process that removes the image it is
	// running cannot be restarted, and cannot report that it broke itself.
	ImageRetainedSelf ImageRetentionReason = "self"
	// ImageRetainedPreserved means a parked original or a quarantined
	// replacement still exists. Those containers ARE the recovery evidence.
	ImageRetainedPreserved ImageRetentionReason = "preserved"

	// ---- something is happening right now --------------------------------

	// ImageRetainedActiveAcquisition means an acquisition is in flight for it.
	ImageRetainedActiveAcquisition ImageRetentionReason = "activeAcquisition"
	// ImageRetainedActiveExecution means a recreation is in flight that names
	// it as either the old image or the target.
	ImageRetainedActiveExecution ImageRetentionReason = "activeExecution"
	// ImageRetainedActiveRollback means a rollback is in flight.
	ImageRetainedActiveRollback ImageRetentionReason = "activeRollback"

	// ---- something went wrong and has not been settled -------------------

	// ImageRetainedUnsettledFailure means a failed attempt has not reached a
	// settled conclusion. Both the original and the failed replacement are
	// kept: one restores service, the other explains why the update did not.
	ImageRetainedUnsettledFailure ImageRetentionReason = "unsettledFailure"
	// ImageRetainedRecoveryOutstanding means a recovery plan is recorded and
	// not discharged. An operator has been told what to do and has not done it.
	ImageRetainedRecoveryOutstanding ImageRetentionReason = "recoveryOutstanding"

	// ---- the update pipeline still points at it --------------------------

	// ImageRetainedPlanTarget means a current plan or a spent acquisition
	// proposes it. Removing an approved target turns a ready update into a
	// failure at the moment somebody presses the button.
	ImageRetainedPlanTarget ImageRetentionReason = "planTarget"

	// ---- the rollback contract -------------------------------------------

	// ImageRetainedRollbackGeneration means this is the most recent image a
	// workload was successfully moved off, and the policy keeps at least that
	// many generations. It is the practical rollback path.
	ImageRetainedRollbackGeneration ImageRetentionReason = "rollbackGeneration"
	// ImageRetainedWithinRetentionAge means the lifecycle settled safely but
	// the retention period has not elapsed.
	ImageRetainedWithinRetentionAge ImageRetentionReason = "withinRetentionAge"

	// ---- nothing was established -----------------------------------------

	// ImageRetainedNotSuperseded means no settled successful update moved a
	// workload off this image. It is not a candidate at all: HarborMaster has
	// no evidence it introduced it or that anything replaced it.
	ImageRetainedNotSuperseded ImageRetentionReason = "notSuperseded"
	// ImageRetainedEvidenceIncomplete means a source could not be read, or the
	// record set was not complete enough to establish the conditions above.
	// The fail-closed catch-all.
	ImageRetainedEvidenceIncomplete ImageRetentionReason = "evidenceIncomplete"
	// ImageRetainedCleanupDisabled means automatic cleanup is off. Stated as a
	// reason rather than skipped so an operator asking "why is this still here"
	// gets an answer.
	ImageRetainedCleanupDisabled ImageRetentionReason = "cleanupDisabled"
)

// ImageRetentionReasons lists every reason, most decisive first.
var ImageRetentionReasons = []ImageRetentionReason{
	ImageRetainedCleanupDisabled,
	ImageRetainedEvidenceIncomplete,
	ImageRetainedSelf,
	ImageRetainedInUse,
	ImageRetainedPreserved,
	ImageRetainedActiveAcquisition,
	ImageRetainedActiveExecution,
	ImageRetainedActiveRollback,
	ImageRetainedUnsettledFailure,
	ImageRetainedRecoveryOutstanding,
	ImageRetainedPlanTarget,
	ImageRetainedNotSuperseded,
	ImageRetainedRollbackGeneration,
	ImageRetainedWithinRetentionAge,
}

// Explain renders a reason as the sentence an operator reads.
//
// HarborMaster's own words, from this fixed set. A reason with no sentence
// would be a decision nobody can audit.
func (r ImageRetentionReason) Explain() string {
	switch r {
	case ImageRetentionNone:
		return "every retention condition was checked and none applies"
	case ImageRetainedCleanupDisabled:
		return "automatic image cleanup is switched off in this deployment"
	case ImageRetainedEvidenceIncomplete:
		return "HarborMaster could not establish that this image is unused, so it was kept"
	case ImageRetainedSelf:
		return "this is the image HarborMaster itself is running"
	case ImageRetainedInUse:
		return "a container on this host is running this image"
	case ImageRetainedPreserved:
		return "a parked or quarantined container from an earlier update still uses it"
	case ImageRetainedActiveAcquisition:
		return "an image download naming it has not finished"
	case ImageRetainedActiveExecution:
		return "a recreation naming it has not finished"
	case ImageRetainedActiveRollback:
		return "a rollback naming it has not finished"
	case ImageRetainedUnsettledFailure:
		return "an update that failed has not been settled, and this image may be needed to recover"
	case ImageRetainedRecoveryOutstanding:
		return "a recovery plan for this container has not been discharged"
	case ImageRetainedPlanTarget:
		return "a current change plan or a downloaded update proposes this image"
	case ImageRetainedNotSuperseded:
		return "no completed update moved a container off this image"
	case ImageRetainedRollbackGeneration:
		return "it is the image the most recent update replaced, kept so that update can be undone"
	case ImageRetainedWithinRetentionAge:
		return "the update that replaced it has not been settled for long enough yet"
	default:
		return "this image was kept"
	}
}

// ImageRetentionPolicy is the operator's configuration.
//
// Two numbers and a switch. Deliberately small: an operator deciding whether to
// let HarborMaster delete things needs to be able to hold the whole rule in
// their head, and every additional knob is another way to configure a data-loss
// footgun.
type ImageRetentionPolicy struct {
	// Enabled gates the whole feature. OFF is the default and the value an
	// absent or unparseable configuration produces.
	Enabled bool
	// MinAge is how long a settled update must have been settled before the
	// image it replaced may be removed. Measured from the moment the lifecycle
	// reached a safe terminal state, never from the image's creation date: an
	// image built two years ago and deployed this morning is not old.
	MinAge time.Duration
	// KeepGenerations is how many superseded images to keep per workload
	// regardless of age. At least one is enforced below, because a workload
	// with no retained previous image has no rollback path at all.
	KeepGenerations int
}

// minKeptGenerations is the floor KeepGenerations is raised to.
//
// One, and not zero. HarborMaster's whole recovery story for a bad update is
// that the previous artefact is still on the host; a configuration that keeps
// none would silently remove that guarantee, and an operator setting zero
// almost certainly means "keep as few as possible" rather than "make rollback
// impossible".
const minKeptGenerations = 1

// KeptGenerations returns the effective generation floor.
func (p ImageRetentionPolicy) KeptGenerations() int {
	if p.KeepGenerations < minKeptGenerations {
		return minKeptGenerations
	}
	return p.KeepGenerations
}

// Usable reports whether the policy can be acted on.
//
// An enabled policy with a non-positive age is NOT usable: it would make every
// settled image immediately removable, which is the configuration most likely
// to be a mistake and the one whose consequences cannot be undone. Refused
// rather than corrected, because guessing what an operator meant about deletion
// is not HarborMaster's decision to make.
func (p ImageRetentionPolicy) Usable() bool {
	return p.Enabled && p.MinAge > 0
}

// ImageRetentionEvidence is everything the decision reads, and nothing else.
//
// Note what is absent: no Docker handle, no repository, no clock. Assembled by
// the caller from records HarborMaster wrote itself, so this function cannot
// reach past its argument to find a reason to delete something.
//
// Every count is a COUNT rather than a boolean so a caller cannot accidentally
// pass "I did not look" as "there are none": the zero value of a bool is false,
// which reads as safe, while EvidenceComplete below has to be set deliberately.
type ImageRetentionEvidence struct {
	// ImageID is the local image this decision is about. Empty is refused.
	ImageID string

	// EvidenceComplete is the caller's assertion that every field below was
	// actually established. False -- the zero value -- means the decision
	// cannot be made and the image is kept.
	EvidenceComplete bool

	// ---- in use ----------------------------------------------------------

	// PresentContainers is how many containers currently on the host run it.
	PresentContainers int
	// PreservedContainers is how many parked originals or quarantined
	// replacements run it. Counted separately from PresentContainers because
	// the reason an operator is given differs, and because these are evidence
	// rather than workloads.
	PreservedContainers int
	// IsSelf marks the image backing HarborMaster's own container.
	IsSelf bool

	// ---- in flight -------------------------------------------------------

	ActiveAcquisitions int
	ActiveExecutions   int
	ActiveRollbacks    int

	// ---- unsettled -------------------------------------------------------

	// UnsettledFailures is how many failed attempts naming this image have not
	// reached a settled conclusion.
	UnsettledFailures int
	// OutstandingRecoveries is how many recorded recovery plans naming it are
	// not discharged.
	OutstandingRecoveries int

	// ---- still proposed --------------------------------------------------

	// PlanTargets is how many current plans or spent acquisitions propose it.
	PlanTargets int

	// ---- the candidate itself --------------------------------------------

	// SettledAt is when the successful update that moved a workload off this
	// image reached its safe terminal state: replacement verified, success
	// durably recorded, original removed, nothing outstanding.
	//
	// Nil means no such update exists, which makes this not a candidate at all
	// rather than an old candidate.
	SettledAt *time.Time
	// NewerSupersededGenerations is how many MORE RECENT images the same
	// workload has since been moved off. Zero means this is the immediately
	// previous generation -- the one a rollback would use.
	NewerSupersededGenerations int
}

// ImageRetentionDecision is the answer and the reason for it.
type ImageRetentionDecision struct {
	ImageID string
	Verdict ImageRetentionVerdict
	Reason  ImageRetentionReason
	// EligibleAt is when a retained-by-time image becomes eligible, when that
	// is knowable. Present only for ImageRetainedWithinRetentionAge, so an
	// operator can see the wait rather than guess at it.
	EligibleAt *time.Time
}

// Removable reports whether the decision permits removal.
//
// A method rather than a comparison at each call site, so no caller can invert
// the test by accident.
func (d ImageRetentionDecision) Removable() bool {
	return d.Verdict == ImageEligibleForRemoval
}

// DecideImageRetention decides whether one image may be removed.
//
// # The order is the rule
//
// Read top to bottom. Every branch returns, and every branch above the last one
// returns Retain -- so reaching the bottom means every condition was checked
// and none applied. That shape is deliberate: a new retention reason is added
// by inserting a branch, and forgetting to insert one cannot make an image
// MORE removable, because the default at the bottom is the only Eligible exit
// and it is guarded by everything above it.
//
// # Why the disabled and incomplete checks come first
//
// They are the two states in which no other check can be trusted. A deployment
// that never opted in must never reach the reasoning at all, and evidence the
// caller could not assemble must not be read as an absence of references.
func DecideImageRetention(
	evidence ImageRetentionEvidence,
	policy ImageRetentionPolicy,
	now time.Time,
) ImageRetentionDecision {
	decision := ImageRetentionDecision{ImageID: evidence.ImageID}

	retain := func(reason ImageRetentionReason) ImageRetentionDecision {
		decision.Verdict = RetainImage
		decision.Reason = reason
		return decision
	}

	// Not opted in, or opted in with a configuration that cannot be acted on
	// safely. Both are refusals rather than defaults.
	if !policy.Usable() {
		return retain(ImageRetainedCleanupDisabled)
	}

	// The caller could not establish the facts below, or is asking about
	// nothing. An unestablished check is not a passed check.
	if !evidence.EvidenceComplete || evidence.ImageID == "" {
		return retain(ImageRetainedEvidenceIncomplete)
	}

	// ---- the image is in use, or is HarborMaster's own ---------------------
	//
	// Self first. A process that deletes the image it is running cannot be
	// restarted and cannot report what it did, so this outranks every other
	// consideration including an otherwise perfect eligibility case.
	if evidence.IsSelf {
		return retain(ImageRetainedSelf)
	}
	if evidence.PresentContainers > 0 {
		return retain(ImageRetainedInUse)
	}
	if evidence.PreservedContainers > 0 {
		return retain(ImageRetainedPreserved)
	}

	// ---- something is in flight -------------------------------------------
	//
	// Above the failure checks because an operation still running has not
	// decided anything yet: it may succeed, it may fail, and either way the
	// image it names is part of an outcome that does not exist yet.
	if evidence.ActiveAcquisitions > 0 {
		return retain(ImageRetainedActiveAcquisition)
	}
	if evidence.ActiveExecutions > 0 {
		return retain(ImageRetainedActiveExecution)
	}
	if evidence.ActiveRollbacks > 0 {
		return retain(ImageRetainedActiveRollback)
	}

	// ---- something failed and was not settled -----------------------------
	//
	// Both the original and the failed replacement are kept. The original is
	// the way back; the replacement is the evidence for why the update did not
	// work, and removing it would leave an operator with a failure they cannot
	// investigate.
	if evidence.UnsettledFailures > 0 {
		return retain(ImageRetainedUnsettledFailure)
	}
	if evidence.OutstandingRecoveries > 0 {
		return retain(ImageRetainedRecoveryOutstanding)
	}

	// ---- the pipeline still points at it ----------------------------------
	if evidence.PlanTargets > 0 {
		return retain(ImageRetainedPlanTarget)
	}

	// ---- is it a candidate at all? ----------------------------------------
	//
	// Everything above says "nothing needs it". This says "and HarborMaster
	// knows why it is here". An image with no settled successful update behind
	// it was never introduced by an update HarborMaster completed, and cleanup
	// does not delete images it cannot account for -- that is the operator's
	// image store, not HarborMaster's.
	if evidence.SettledAt == nil {
		return retain(ImageRetainedNotSuperseded)
	}

	// ---- the rollback generation ------------------------------------------
	//
	// At least one previous image per workload, whatever its age. This is the
	// practical rollback path: an operator who discovers at the end of the week
	// that Tuesday's update was wrong needs Tuesday's artefact to still exist.
	if evidence.NewerSupersededGenerations < policy.KeptGenerations() {
		return retain(ImageRetainedRollbackGeneration)
	}

	// ---- the clock --------------------------------------------------------
	//
	// Measured from when the lifecycle SETTLED, never from when the image was
	// built. The question is how long the replacement has been proving itself,
	// and an image's creation date says nothing about that.
	eligibleAt := evidence.SettledAt.UTC().Add(policy.MinAge)
	if now.UTC().Before(eligibleAt) {
		held := retain(ImageRetainedWithinRetentionAge)
		held.EligibleAt = &eligibleAt
		return held
	}

	// Every condition checked, none applies.
	decision.Verdict = ImageEligibleForRemoval
	decision.Reason = ImageRetentionNone
	return decision
}
