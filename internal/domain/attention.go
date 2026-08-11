package domain

import "strings"

// What a Docker administrator needs to know about a container without opening
// it.
//
// # The problem this solves
//
// HarborMaster knows a great deal about each container, spread across seven
// subsystems: the planner knows whether a newer image exists, the automation
// engine knows whether it is holding a decision, the pause table knows whether
// it gave up, the policy engine knows whether the container violates a rule,
// the drift engine knows whether it has moved from its baseline, the lineage
// record knows whether updates are followed at all, and the recreation history
// knows whether this container is a workload or a piece of preserved evidence.
//
// An operator scanning a list of containers had none of it. They had state,
// health, image, compose project, restart count and ports -- the same seven
// columns `docker ps` gives -- and every HarborMaster-specific fact required
// opening the row.
//
// # Why this is a pure function
//
// The rules below decide what an operator is told, and several of them are
// safety-relevant: "not checked" must not render as "up to date", an
// unresolvable recommendation must not render as "safe to update", and a
// container the automation engine is holding must say so louder than one with
// a routine patch available. Those are properties worth testing exhaustively,
// which means the decision cannot live in a SQL query or a React component.
//
// `AssessContainer` takes evidence and returns a verdict. It reads no clock,
// no database, and no network.
//
// # The evidence gap rule
//
// Absent evidence produces `AttentionNotChecked`, never `AttentionUpToDate`.
// The two look similar in a list and mean opposite things: one says
// HarborMaster looked and found nothing to do, the other says HarborMaster has
// not looked. A UI is free to render them in the same colour; it is not free to
// render them with the same words.

// AttentionState is the single headline verdict for one container.
//
// A closed vocabulary. The UI maps each to one badge, and a value it does not
// recognise must render as the raw string rather than as a default, so a new
// state added here cannot silently appear as "up to date".
type AttentionState string

// The states, most urgent first. `AttentionOrder` below is the authority on
// precedence; this list is documentation.
const (
	// AttentionPreserved marks a container HarborMaster itself parked as
	// evidence -- an original held during a recreation, a replacement that
	// failed verification, or a replacement a rollback moved aside. Not a
	// workload, and listing it as one was the audit finding that produced this.
	AttentionPreserved AttentionState = "preserved"

	// AttentionUnhealthy means the container's own healthcheck is failing.
	// Nothing HarborMaster might do to it matters more than that.
	AttentionUnhealthy AttentionState = "unhealthy"

	// AttentionApprovalRequired means an update policy has decided on a change
	// and is holding it for a person. The only state on this list that is
	// waiting on the operator personally.
	AttentionApprovalRequired AttentionState = "approvalRequired"

	// AttentionPaused means automation has stopped trying for this container.
	AttentionPaused AttentionState = "paused"

	// AttentionNeedsReview means an update exists and the planner asks for a
	// person before it happens, or advises against it.
	AttentionNeedsReview AttentionState = "needsReview"

	// AttentionCannotAdvise means an update may exist and HarborMaster could
	// not judge it. NOT the same as "no update" and NOT the same as "risky":
	// it is the absence of a verdict, and it is reported rather than resolved.
	AttentionCannotAdvise AttentionState = "cannotAdvise"

	// AttentionUpdateAvailable means a newer image exists and the assessment
	// does not argue against taking it.
	AttentionUpdateAvailable AttentionState = "updateAvailable"

	// AttentionNotTracked means HarborMaster cannot attribute the running
	// image to any tag, so no update will ever be found for this container.
	// An empty update column here means "we will never tell you", not "there
	// is nothing to tell".
	AttentionNotTracked AttentionState = "notTracked"

	// AttentionNotChecked means no assessment exists yet. The honest answer
	// before the planner has run, and the one that must never be rendered as
	// AttentionUpToDate.
	AttentionNotChecked AttentionState = "notChecked"

	// AttentionUpToDate means HarborMaster looked and found nothing to do.
	AttentionUpToDate AttentionState = "upToDate"
)

// AttentionOrder is the precedence, most urgent first.
//
// The order is the rule. `AssessContainer` walks it and returns the first
// state whose condition holds, so adding a state means deciding where it sits
// relative to every other one -- which is the decision that actually matters.
var AttentionOrder = []AttentionState{
	AttentionPreserved,
	AttentionUnhealthy,
	AttentionApprovalRequired,
	AttentionPaused,
	AttentionNeedsReview,
	AttentionCannotAdvise,
	AttentionUpdateAvailable,
	AttentionNotTracked,
	AttentionNotChecked,
	AttentionUpToDate,
}

// AttentionRank returns the precedence index, lower being more urgent.
//
// An unrecognised state ranks last rather than first: a value this file does
// not know about must not be able to push a real problem down the list.
func AttentionRank(state AttentionState) int {
	for index, known := range AttentionOrder {
		if known == state {
			return index
		}
	}
	return len(AttentionOrder)
}

// NeedsOperator reports whether a state is asking something of a person.
//
// Used to group a list into "action required" and everything else. Preserved
// containers are excluded deliberately: they are evidence an operator may
// eventually want to remove, not work that is blocked on them.
func (state AttentionState) NeedsOperator() bool {
	switch state {
	case AttentionUnhealthy, AttentionApprovalRequired, AttentionPaused,
		AttentionNeedsReview:
		return true
	default:
		return false
	}
}

// PreservedKind says why HarborMaster is keeping a container that is not a
// workload.
type PreservedKind string

// The kinds. Each corresponds to a name HarborMaster derived itself and wrote
// into its own execution or rollback record.
const (
	PreservedNone PreservedKind = ""
	// PreservedOriginal is the container an update replaced, stopped and held
	// until the replacement proved itself. Its presence after a completed
	// update means the removal step did not finish.
	PreservedOriginal PreservedKind = "original"
	// PreservedFailed is a replacement that failed its own verification. Kept
	// deliberately: it is the evidence explaining why the new image did not
	// work.
	PreservedFailed PreservedKind = "failed"
	// PreservedRolledBack is a replacement a rollback moved aside. It may have
	// been perfectly healthy; a rollback is not a verdict on it.
	PreservedRolledBack PreservedKind = "rolledBack"
	// PreservedSuspected is a container whose NAME matches the shape
	// HarborMaster uses, with no record to back it up.
	//
	// Reported separately and never acted on. A container an operator named
	// this way themselves matches too, so this can flag a workload as evidence
	// but must never let HarborMaster hide one: the default listing excludes
	// only the kinds above, which come from records HarborMaster wrote.
	PreservedSuspected PreservedKind = "suspected"
)

// Evidenced reports whether a HarborMaster record backs this classification.
//
// The line between "we know, because we wrote it down" and "the name looks
// like ours". Only the first may remove a container from the default view.
func (kind PreservedKind) Evidenced() bool {
	switch kind {
	case PreservedOriginal, PreservedFailed, PreservedRolledBack:
		return true
	default:
		return false
	}
}

// ContainerEvidence is everything the assessment reads, and nothing else.
//
// Assembled by the store in a fixed number of batched queries. Note what is
// absent: no Docker handle, no registry client, no clock. Rendering a list
// must not become a reason to talk to a privileged socket.
type ContainerEvidence struct {
	// From the inventory row itself.
	Health  HealthState
	State   ContainerState
	Present bool

	// PlanKnown distinguishes "the planner has assessed this container" from
	// "it has not run yet". Without it, a container with no plan and a
	// container with a plan proposing nothing are indistinguishable.
	PlanKnown      bool
	UpdateType     UpdateType
	Recommendation Recommendation
	ProposedImage  string

	// LineageKnown distinguishes "we looked and there is nothing to follow"
	// from "we have not looked".
	LineageKnown      bool
	Tracked           bool
	TrackingReference string

	// From the automation subsystem.
	AwaitingApproval bool
	AutomationPaused bool

	// Open findings. Counts only; the detail lives on the pages that own it.
	OpenViolations  int
	HighestSeverity PolicySeverity
	OpenDrift       int

	// Preserved, and the workload the preserved container belongs to.
	Preserved    PreservedKind
	PreservedFor string

	// What HarborMaster last DID to this container, if anything. Nil means no
	// record exists, which is different from a record saying nothing happened.
	LastUpdate   *ActionOutcome
	LastRollback *ActionOutcome
}

// ActionOutcome is the last thing HarborMaster did to a container.
//
// Deliberately thin: an identifier to open the full record, the state it
// finished in, and when. Everything explaining WHY lives on the record itself,
// and duplicating any of it here would create a second place to keep correct.
type ActionOutcome struct {
	ID    string `json:"id"`
	State string `json:"state"`
	// Failure is the closed-vocabulary reason, empty unless it failed.
	Failure string `json:"failure,omitempty"`
	At      string `json:"at,omitempty"`
	// NeedsAttention marks a failure that left containers on the host.
	NeedsAttention bool `json:"needsAttention"`
}

// ContainerAttention is the verdict plus the facts a row renders beside it.
type ContainerAttention struct {
	State AttentionState `json:"state"`

	// UpdateType and Recommendation are carried through so a row can say
	// "minor update" rather than only "update available". Empty when no
	// assessment exists -- never defaulted to "none", which would claim the
	// planner had looked.
	UpdateType     UpdateType     `json:"updateType,omitempty"`
	Recommendation Recommendation `json:"recommendation,omitempty"`
	ProposedImage  string         `json:"proposedImage,omitempty"`

	// Tracking is the reference update discovery follows, when there is one.
	Tracking string `json:"tracking,omitempty"`
	// TrackingKnown is false when HarborMaster has not yet established
	// whether this container follows anything.
	TrackingKnown bool `json:"trackingKnown"`

	AwaitingApproval bool `json:"awaitingApproval"`
	AutomationPaused bool `json:"automationPaused"`

	OpenViolations  int            `json:"openViolations"`
	HighestSeverity PolicySeverity `json:"highestSeverity,omitempty"`
	OpenDrift       int            `json:"openDrift"`

	Preserved    PreservedKind `json:"preserved,omitempty"`
	PreservedFor string        `json:"preservedFor,omitempty"`

	// The last update and the last rollback HarborMaster performed here.
	// Absent when there has never been one -- which the UI must render as
	// "HarborMaster has not changed this container", never as a success.
	LastUpdate   *ActionOutcome `json:"lastUpdate,omitempty"`
	LastRollback *ActionOutcome `json:"lastRollback,omitempty"`
}

// AssessContainer turns evidence into the verdict a list row shows.
//
// Precedence walks AttentionOrder. Every branch is reachable and every branch
// is tested; the pure-function shape is what makes that possible.
func AssessContainer(evidence ContainerEvidence) ContainerAttention {
	attention := ContainerAttention{
		Tracking:         evidence.TrackingReference,
		TrackingKnown:    evidence.LineageKnown,
		AwaitingApproval: evidence.AwaitingApproval,
		AutomationPaused: evidence.AutomationPaused,
		OpenViolations:   evidence.OpenViolations,
		HighestSeverity:  evidence.HighestSeverity,
		OpenDrift:        evidence.OpenDrift,
		Preserved:        evidence.Preserved,
		PreservedFor:     evidence.PreservedFor,
		LastUpdate:       evidence.LastUpdate,
		LastRollback:     evidence.LastRollback,
	}
	// Carried only when an assessment exists. A zero UpdateType would
	// serialise as the empty string anyway; being explicit here means a future
	// caller that defaults it to "none" has to do so deliberately.
	if evidence.PlanKnown {
		attention.UpdateType = evidence.UpdateType
		attention.Recommendation = evidence.Recommendation
		attention.ProposedImage = evidence.ProposedImage
	}

	attention.State = assessState(evidence)
	return attention
}

// assessState is the precedence walk itself, kept separate so the ordering is
// readable as one list of conditions.
func assessState(evidence ContainerEvidence) AttentionState {
	// A container HarborMaster parked is not a workload, and every judgement
	// below assumes it is one. Its health is meaningless (it is stopped on
	// purpose) and its image is deliberately the old one.
	if evidence.Preserved != PreservedNone {
		return AttentionPreserved
	}

	if evidence.Health == HealthUnhealthy {
		return AttentionUnhealthy
	}

	// Above paused, because an approval is waiting on THIS PERSON while a
	// pause is waiting on an investigation.
	if evidence.AwaitingApproval {
		return AttentionApprovalRequired
	}

	if evidence.AutomationPaused {
		return AttentionPaused
	}

	// Everything from here down is about the proposed image, so a container
	// with no assessment cannot reach any of it.
	if !evidence.PlanKnown {
		// ...except this: "we established that nothing can be followed" is a
		// stronger answer than "no assessment exists", and reporting the
		// weaker one would suggest the planner might yet find something.
		if evidence.LineageKnown && !evidence.Tracked {
			return AttentionNotTracked
		}
		return AttentionNotChecked
	}

	switch evidence.UpdateType {
	case UpdateNone:
		// The planner ran, compared, and found the image current. The only
		// path to AttentionUpToDate, and it requires PlanKnown above.
		if evidence.LineageKnown && !evidence.Tracked {
			// A container running an unattributable digest cannot be declared
			// current: "none" here means "nothing to compare against".
			return AttentionNotTracked
		}
		return AttentionUpToDate

	case UpdateUnknown, "":
		// An update may exist and its size could not be determined. Reported
		// as the non-answer it is.
		return AttentionCannotAdvise
	}

	// An update of a known size exists. What the planner advises about it
	// decides how loudly the row says so.
	switch evidence.Recommendation {
	case RecommendManualReview, RecommendAgainst:
		return AttentionNeedsReview
	case RecommendUnknown, "":
		return AttentionCannotAdvise
	default:
		return AttentionUpdateAvailable
	}
}

// ClassifyPreservedName reports what a name LOOKS like, with no record behind
// it.
//
// The weakest signal in this file, and the reason PreservedSuspected exists as
// a separate kind. Used only to explain a container the operator can see on
// their host; never used to exclude one from a listing, because an operator
// who named a container this way themselves would have it disappear.
func ClassifyPreservedName(name string) PreservedKind {
	if IsHarborMasterDerivedName(name) || isRollbackParkedName(name) {
		return PreservedSuspected
	}
	return PreservedNone
}

// isRollbackParkedName recognises the third suffix.
//
// IsHarborMasterDerivedName covers the two the recreation path derives;
// rollback parking has its own suffix and its own id vocabulary, and a name
// carrying it is just as much HarborMaster's doing.
func isRollbackParkedName(name string) bool {
	normalised := NormaliseContainerName(name)
	index := strings.LastIndex(normalised, RollbackParkedNameSuffix)
	if index <= 0 {
		return false
	}
	return ValidRollbackID(normalised[index+len(RollbackParkedNameSuffix):])
}
