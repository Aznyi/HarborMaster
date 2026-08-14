package domain

import (
	"strings"
	"time"
)

// Establishing that a container needs no update.
//
// # The fact this type exists to carry, and why it needed one
//
// The planner does not write a change plan for a container it assessed and
// found current -- a row per settled container is noise, and declining to write
// it is deliberate. So the automation pass sees "no plan" for two OPPOSITE
// situations:
//
//	A. HarborMaster looked, and this container is on what it should be running.
//	B. HarborMaster did not look, or looked and could not establish anything.
//
// For every purpose but one, collapsing them is harmless: both mean "do not
// update this container now". For DEPENDENCY ORDERING they are opposites. A
// dependent may proceed past an upstream in state A and must wait behind one in
// state B, and reading B as A is the inversion the whole dependency subsystem
// exists to prevent -- in the one direction that lets work proceed rather than
// holding it.
//
// Stage 5b found the consequence live: an upstream running the newest published
// tag, healthy and freshly checked, blocked its dependent for ever with
// "a container this one depends on needs an update that the rules in force do
// not permit". Nothing was wrong with the upstream at all.
//
// # Why this is a fact and not a target
//
// CurrentAssessment says something TRUE about a container. It names no container
// id, image, digest, registry, or plan, so there is nowhere on it to put a
// thing to act on, and no code path turns one into a mutation. It can only ever
// permit a decision that was ALREADY eligible to proceed; it cannot make an
// ineligible container eligible, and the dependency gate that reads it is
// structurally subtract-only.
//
// # Purity
//
// No clock, no database, no Docker, no configuration lookup: `now` and the
// freshness bound are parameters. That is what lets the negative cases be
// enumerated exhaustively.

// CurrentAssessment is HarborMaster's own finding about whether a container
// needs an update.
//
// THE ZERO VALUE ESTABLISHES NOTHING. A caller that failed to assess a
// container, or could not, holds the zero value and must treat it as "unknown",
// never as "current".
type CurrentAssessment struct {
	// Established is true only when HarborMaster positively determined that
	// this container is running what it should be running.
	//
	// False covers every other case, including every kind of not-knowing, and
	// they are deliberately not distinguished here: a caller's action is the
	// same for all of them.
	Established bool
	// Reason is HarborMaster's own sentence. Never interpolates a daemon
	// message, a registry response, or an error's text.
	Reason string
}

// assessmentReasons. A closed set, so nothing a registry or a daemon said can
// reach an operator through this type.
const (
	assessedCurrent      = "HarborMaster checked this container's tracking reference and it is running the digest that reference resolves to"
	assessedNotTracked   = "HarborMaster has no tracking reference for this container, so there is nothing to compare it against"
	assessedNoEvidence   = "HarborMaster has no successful registry check for this container's tracking reference"
	assessedStale        = "the registry check for this container's tracking reference is too old to act on"
	assessedReplaced     = "this container was replaced without HarborMaster's involvement, so what it is running has not been re-established"
	assessedNeedsUpdate  = "a newer image is available for this container"
	assessedNotEstablish = "HarborMaster could not establish whether this container is up to date"
)

// AssessCurrent reports whether a container is positively established as
// current.
//
// Every requirement is a POSITIVE fact, and each is checked against evidence
// HarborMaster wrote itself:
//
//  1. A tracking reference exists and is trusted (lineage).
//  2. The container HarborMaster recorded is the container that is there now,
//     so nothing replaced it unobserved.
//  3. A registry check for THAT reference succeeded, and recently enough.
//  4. The comparison "what the reference resolves to" against "what is running"
//     says there is nothing to move onto.
//
// Any of them missing returns an UNESTABLISHED assessment. There is no branch
// that returns Established from an absence.
//
// `observedContainerID` is the id the pass read from the inventory. `freshness`
// is how recent the registry check must be; a non-positive value does not relax
// the check, it fails it -- a bound that can be configured away is not a bound.
func AssessCurrent(
	lineage ImageLineage,
	intel ImageIntel,
	observedContainerID string,
	now time.Time,
	freshness time.Duration,
) CurrentAssessment {
	// 1. Something must say what this container follows.
	if !lineage.Tracked() {
		return CurrentAssessment{Reason: assessedNotTracked}
	}

	// 2. The container HarborMaster knows about must be the one that is there.
	//
	// Lineage is keyed by NAME and carries the id as evidence. A different id
	// means something replaced the container while HarborMaster was not
	// looking, so the running digest recorded here describes a container that
	// is gone. Reconciliation re-establishes lineage; until it has, this
	// establishes nothing.
	//
	// An empty observed id is not a match. A caller that could not read the
	// container's identity has not shown that it is the same one.
	observed := strings.TrimSpace(observedContainerID)
	if observed == "" || !strings.EqualFold(observed, strings.TrimSpace(lineage.ContainerID)) {
		return CurrentAssessment{Reason: assessedReplaced}
	}

	// 3. The registry answer must exist, belong to this reference, have
	// succeeded, and be recent.
	//
	// EvaluateLineage already refuses evidence for the wrong reference or
	// repository and any status but CheckOK. What it does not judge is AGE, so
	// that is checked here: an answer from last month does not establish that
	// the registry still serves the same digest today.
	if freshness <= 0 {
		return CurrentAssessment{Reason: assessedStale}
	}
	if intel.LastCheckedAt == nil {
		return CurrentAssessment{Reason: assessedNoEvidence}
	}
	if intel.LastCheckedAt.Before(now.Add(-freshness)) {
		return CurrentAssessment{Reason: assessedStale}
	}

	// 4. The same comparison the planner makes, through the same function, so
	// the two can never disagree about whether a container is settled.
	//
	// The running digest comes from LINEAGE -- what HarborMaster approved and
	// believes is running -- and never from a caller.
	proposal := EvaluateLineage(lineage, intel, lineage.RunningDigest)
	switch {
	case !proposal.Usable:
		// "Not established", never "no update". This is the branch that used to
		// be indistinguishable from success.
		return CurrentAssessment{Reason: assessedNotEstablish}
	case proposal.Update != UpdateNone:
		return CurrentAssessment{Reason: assessedNeedsUpdate}
	default:
		return CurrentAssessment{Established: true, Reason: assessedCurrent}
	}
}
