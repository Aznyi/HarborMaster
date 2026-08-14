package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The container attention model.
//
// This is the function that decides what a Docker administrator is told about
// a container they have not opened, so the properties worth defending are the
// ones where a wrong answer would mislead someone into inaction:
//
//   - "not checked" never renders as "up to date"
//   - an unresolvable recommendation never renders as "update available"
//   - a container running an unattributable digest is never declared current
//   - a container HarborMaster parked is never presented as a workload
//   - a held approval outranks everything except the container being unhealthy
//
// Every branch below is reachable from a real evidence combination; none of
// them is a synthetic input the store cannot produce.

// healthy is the baseline: a running workload the planner has assessed and
// found current. Every test below starts here and changes ONE thing, so the
// change is what the test is about.
func healthy() domain.ContainerEvidence {
	return domain.ContainerEvidence{
		Health:            domain.HealthHealthy,
		State:             domain.StateRunning,
		Present:           true,
		PlanKnown:         true,
		UpdateType:        domain.UpdateNone,
		Recommendation:    domain.RecommendProceed,
		LineageKnown:      true,
		Tracked:           true,
		TrackingReference: "docker.io/library/nginx:1.27",
	}
}

func assess(t *testing.T, evidence domain.ContainerEvidence) domain.ContainerAttention {
	t.Helper()
	return domain.AssessContainer(evidence)
}

// ------------------------------------------------------- the evidence gap --

func TestNoAssessmentIsNotUpToDate(t *testing.T) {
	// The headline property. A container the planner has never looked at must
	// not sit in a list next to one it assessed and cleared, wearing the same
	// badge.
	evidence := healthy()
	evidence.PlanKnown = false
	evidence.UpdateType = ""
	evidence.Recommendation = ""

	if got := assess(t, evidence).State; got != domain.AttentionNotChecked {
		t.Fatalf("state = %q, want %q", got, domain.AttentionNotChecked)
	}
}

func TestNoAssessmentCarriesNoUpdateType(t *testing.T) {
	// A zero UpdateType serialises as "" and a careless default would make it
	// "none", which claims the planner compared something. It did not.
	evidence := healthy()
	evidence.PlanKnown = false
	evidence.UpdateType = domain.UpdateMajor
	evidence.Recommendation = domain.RecommendProceed
	evidence.ProposedImage = "nginx:2.0"

	attention := assess(t, evidence)
	if attention.UpdateType != "" || attention.Recommendation != "" ||
		attention.ProposedImage != "" {
		t.Fatalf("an unassessed container reported %+v", attention)
	}
}

func TestAnUntrackedContainerIsNeverDeclaredCurrent(t *testing.T) {
	// "none" from the planner means "nothing to compare against" when there is
	// no tracking reference. Rendering that as "up to date" would tell an
	// operator their container is current when HarborMaster will never find an
	// update for it at all.
	evidence := healthy()
	evidence.Tracked = false
	evidence.TrackingReference = ""

	if got := assess(t, evidence).State; got != domain.AttentionNotTracked {
		t.Fatalf("state = %q, want %q", got, domain.AttentionNotTracked)
	}
}

func TestUntrackedOutranksUnassessed(t *testing.T) {
	// "we looked and there is nothing to follow" is a stronger, more useful
	// answer than "no assessment exists", which suggests one might yet appear.
	evidence := healthy()
	evidence.PlanKnown = false
	evidence.UpdateType = ""
	evidence.Recommendation = ""
	evidence.Tracked = false

	if got := assess(t, evidence).State; got != domain.AttentionNotTracked {
		t.Fatalf("state = %q, want %q", got, domain.AttentionNotTracked)
	}
}

func TestLineageNotYetEstablishedIsNotUntracked(t *testing.T) {
	// A container whose lineage has not been worked out yet must not be
	// reported as one HarborMaster has established it cannot follow.
	evidence := healthy()
	evidence.PlanKnown = false
	evidence.UpdateType = ""
	evidence.Recommendation = ""
	evidence.LineageKnown = false
	evidence.Tracked = false

	attention := assess(t, evidence)
	if attention.State != domain.AttentionNotChecked {
		t.Fatalf("state = %q, want %q", attention.State, domain.AttentionNotChecked)
	}
	if attention.TrackingKnown {
		t.Fatal("tracking must not be reported as established")
	}
}

// -------------------------------------------------------- the non-answers --

func TestAnUnsizedUpdateCannotBeAdvisedOn(t *testing.T) {
	// A calendar tag, an incomplete listing, a comparison the parser refused.
	// An update may exist and HarborMaster cannot say how big it is.
	evidence := healthy()
	evidence.UpdateType = domain.UpdateUnknown

	if got := assess(t, evidence).State; got != domain.AttentionCannotAdvise {
		t.Fatalf("state = %q, want %q", got, domain.AttentionCannotAdvise)
	}
}

func TestAnUnknownRecommendationCannotBeAdvisedOn(t *testing.T) {
	// The size is known; the judgement is not. Reporting this as "update
	// available" would present an unassessed change as a cleared one.
	for _, recommendation := range []domain.Recommendation{
		domain.RecommendUnknown, "",
	} {
		evidence := healthy()
		evidence.UpdateType = domain.UpdateMinor
		evidence.Recommendation = recommendation

		if got := assess(t, evidence).State; got != domain.AttentionCannotAdvise {
			t.Errorf("recommendation %q: state = %q, want %q",
				recommendation, got, domain.AttentionCannotAdvise)
		}
	}
}

func TestAReviewOrRefusalAsksForAPerson(t *testing.T) {
	for _, recommendation := range []domain.Recommendation{
		domain.RecommendManualReview, domain.RecommendAgainst,
	} {
		evidence := healthy()
		evidence.UpdateType = domain.UpdateMajor
		evidence.Recommendation = recommendation

		if got := assess(t, evidence).State; got != domain.AttentionNeedsReview {
			t.Errorf("recommendation %q: state = %q, want %q",
				recommendation, got, domain.AttentionNeedsReview)
		}
	}
}

func TestAClearedUpdateIsOfferedAsOne(t *testing.T) {
	for _, recommendation := range []domain.Recommendation{
		domain.RecommendProceed, domain.RecommendCaution,
	} {
		for _, updateType := range []domain.UpdateType{
			domain.UpdatePatch, domain.UpdateMinor, domain.UpdateMajor,
			domain.UpdateDigest, domain.UpdatePrerelease,
		} {
			evidence := healthy()
			evidence.UpdateType = updateType
			evidence.Recommendation = recommendation

			attention := assess(t, evidence)
			if attention.State != domain.AttentionUpdateAvailable {
				t.Errorf("%s/%s: state = %q, want %q", updateType, recommendation,
					attention.State, domain.AttentionUpdateAvailable)
			}
			// And the row can say WHICH kind, rather than only that one exists.
			if attention.UpdateType != updateType {
				t.Errorf("%s: update type lost", updateType)
			}
		}
	}
}

// ------------------------------------------------------------ precedence --

func TestUnhealthyOutranksEveryUpdateState(t *testing.T) {
	// Nothing HarborMaster might do to a container matters more than the
	// container's own healthcheck failing.
	evidence := healthy()
	evidence.Health = domain.HealthUnhealthy
	evidence.UpdateType = domain.UpdateMajor
	evidence.Recommendation = domain.RecommendProceed
	evidence.AwaitingApproval = true
	evidence.AutomationPaused = true

	if got := assess(t, evidence).State; got != domain.AttentionUnhealthy {
		t.Fatalf("state = %q, want %q", got, domain.AttentionUnhealthy)
	}
}

func TestAHeldApprovalOutranksAPause(t *testing.T) {
	// An approval is waiting on THIS PERSON; a pause is waiting on an
	// investigation. The one they can finish in a click comes first.
	evidence := healthy()
	evidence.AwaitingApproval = true
	evidence.AutomationPaused = true

	if got := assess(t, evidence).State; got != domain.AttentionApprovalRequired {
		t.Fatalf("state = %q, want %q", got, domain.AttentionApprovalRequired)
	}
}

func TestAPauseOutranksAnAvailableUpdate(t *testing.T) {
	evidence := healthy()
	evidence.AutomationPaused = true
	evidence.UpdateType = domain.UpdatePatch

	if got := assess(t, evidence).State; got != domain.AttentionPaused {
		t.Fatalf("state = %q, want %q", got, domain.AttentionPaused)
	}
}

func TestEveryStateHasARank(t *testing.T) {
	seen := make(map[int]domain.AttentionState, len(domain.AttentionOrder))
	for _, state := range domain.AttentionOrder {
		rank := domain.AttentionRank(state)
		if other, clash := seen[rank]; clash {
			t.Fatalf("%q and %q share rank %d", state, other, rank)
		}
		seen[rank] = state
	}
	// An unknown value ranks LAST. A state this file does not know about must
	// never be able to push a real problem down a sorted list.
	if domain.AttentionRank("something-new") != len(domain.AttentionOrder) {
		t.Fatal("an unrecognised state must rank last, not first")
	}
}

func TestOnlyTheStatesAskingSomethingNeedAnOperator(t *testing.T) {
	wants := map[domain.AttentionState]bool{
		domain.AttentionUnhealthy:        true,
		domain.AttentionApprovalRequired: true,
		domain.AttentionPaused:           true,
		domain.AttentionNeedsReview:      true,

		// The three dependency states nothing resolves by itself. A failed
		// reattachment is never retried, a loop cannot be broken by a pass, and
		// an unresolvable dependency stays unresolvable until the container
		// configuration or the inventory changes.
		//
		// AttentionDependencyBlocked is deliberately NOT here: it is frequently
		// the ordinary consequence of a dependency due to be updated on the
		// next pass, and listing it as work for a person would make this list
		// noisy enough to stop being read.
		domain.AttentionDependencyFailed:     true,
		domain.AttentionDependencyCycle:      true,
		domain.AttentionDependencyUnresolved: true,
	}
	for _, state := range domain.AttentionOrder {
		if got := state.NeedsOperator(); got != wants[state] {
			t.Errorf("%q.NeedsOperator() = %v, want %v", state, got, wants[state])
		}
	}
}

// ------------------------------------------------------------- preserved --

func TestAPreservedContainerIsNotAWorkload(t *testing.T) {
	// Its health is meaningless (it is stopped on purpose) and its image is
	// deliberately the old one, so every judgement below it would be wrong.
	for _, kind := range []domain.PreservedKind{
		domain.PreservedOriginal, domain.PreservedFailed,
		domain.PreservedRolledBack, domain.PreservedSuspected,
	} {
		evidence := healthy()
		evidence.Preserved = kind
		evidence.Health = domain.HealthUnhealthy
		evidence.UpdateType = domain.UpdateMajor

		attention := assess(t, evidence)
		if attention.State != domain.AttentionPreserved {
			t.Errorf("%q: state = %q, want %q", kind, attention.State,
				domain.AttentionPreserved)
		}
		if attention.Preserved != kind {
			t.Errorf("%q: the kind was lost", kind)
		}
	}
}

func TestOnlyRecordedEvidenceMayHideAContainer(t *testing.T) {
	// The line that matters: HarborMaster may exclude a container from the
	// default view because it holds a record saying it parked it. It may NOT
	// exclude one because the name looks familiar -- an operator who named a
	// container that way themselves would watch it vanish.
	evidenced := map[domain.PreservedKind]bool{
		domain.PreservedOriginal:   true,
		domain.PreservedFailed:     true,
		domain.PreservedRolledBack: true,
		domain.PreservedSuspected:  false,
		domain.PreservedNone:       false,
	}
	for kind, want := range evidenced {
		if got := kind.Evidenced(); got != want {
			t.Errorf("%q.Evidenced() = %v, want %v", kind, got, want)
		}
	}
}

func TestNameClassificationRecognisesAllThreeSuffixes(t *testing.T) {
	execID := "exec_00112233445566778899"
	rollbackID := "rbk_00112233445566778899"

	cases := map[string]domain.PreservedKind{
		"web" + domain.ParkedNameSuffix + execID:             domain.PreservedSuspected,
		"web" + domain.QuarantineNameSuffix + execID:         domain.PreservedSuspected,
		"web" + domain.RollbackParkedNameSuffix + rollbackID: domain.PreservedSuspected,
		// Not ours: the suffix without a well-formed id it could have come from.
		"web" + domain.ParkedNameSuffix + "nonsense":     domain.PreservedNone,
		"web" + domain.RollbackParkedNameSuffix + "abcd": domain.PreservedNone,
		"web": domain.PreservedNone,
		"":    domain.PreservedNone,
		// A leading suffix with no name before it is not a derived name.
		domain.ParkedNameSuffix + execID: domain.PreservedNone,
	}
	for name, want := range cases {
		if got := domain.ClassifyPreservedName(name); got != want {
			t.Errorf("ClassifyPreservedName(%q) = %q, want %q", name, got, want)
		}
	}
}

// ----------------------------------------------------------- passthrough --

func TestFindingsAreCarriedWithoutBecomingTheVerdict(t *testing.T) {
	// Open violations and drift are observations, not work HarborMaster is
	// waiting to do. They travel on the row so it can show a marker, and they
	// do not displace the update story.
	evidence := healthy()
	evidence.UpdateType = domain.UpdatePatch
	evidence.OpenViolations = 3
	evidence.HighestSeverity = domain.PolicySeverityCritical
	evidence.OpenDrift = 2

	attention := assess(t, evidence)
	if attention.State != domain.AttentionUpdateAvailable {
		t.Fatalf("state = %q, want %q", attention.State, domain.AttentionUpdateAvailable)
	}
	if attention.OpenViolations != 3 || attention.OpenDrift != 2 ||
		attention.HighestSeverity != domain.PolicySeverityCritical {
		t.Fatalf("findings were lost: %+v", attention)
	}
}

func TestTheAssessmentReadsNothingButItsInput(t *testing.T) {
	// Called twice with identical evidence, it must produce identical output.
	// The guard against somebody reaching for a clock or a package-level cache
	// inside what is documented as a pure function.
	evidence := healthy()
	evidence.UpdateType = domain.UpdateMinor

	first := domain.AssessContainer(evidence)
	second := domain.AssessContainer(evidence)
	if first != second {
		t.Fatalf("two calls disagreed:\n%+v\n%+v", first, second)
	}
}
