package domain_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Image cleanup, one condition at a time.
//
// # The asymmetry these tests defend
//
//	A wrongly retained image costs disk space.
//	A wrongly removed image costs the ability to recover.
//
// So almost every case below asserts RETAIN, and the handful that assert
// eligibility each construct a state where every single safety condition has
// been positively established as absent. The decision has exactly one Eligible
// exit and it sits at the bottom of the function, guarded by every branch above
// it -- these tests walk those branches.
//
// # Non-vacuity
//
// A test suite for a fail-closed function can pass by never reaching the
// interesting path. TestAFullySettledImageIsEligible is the control: it proves
// the eligible exit is reachable at all, so every retain assertion above it is
// a real refusal rather than a function that never says yes.

var retentionNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// enabledPolicy keeps one generation and one hour, so the age and generation
// rules can be exercised separately.
func enabledPolicy() domain.ImageRetentionPolicy {
	return domain.ImageRetentionPolicy{
		Enabled: true, MinAge: time.Hour, KeepGenerations: 1,
	}
}

// settledLongAgo is a lifecycle that reached its safe terminal state well
// outside any retention period used here.
func settledLongAgo() *time.Time {
	at := retentionNow.Add(-30 * 24 * time.Hour)
	return &at
}

// eligible is the evidence for an image nothing needs: settled long ago, one
// newer generation behind it, and every reference count zero.
func eligible() domain.ImageRetentionEvidence {
	return domain.ImageRetentionEvidence{
		ImageID:                    "sha256:aaaa",
		EvidenceComplete:           true,
		SettledAt:                  settledLongAgo(),
		NewerSupersededGenerations: 1,
	}
}

func decide(evidence domain.ImageRetentionEvidence) domain.ImageRetentionDecision {
	return domain.DecideImageRetention(evidence, enabledPolicy(), retentionNow)
}

// ------------------------------------------------- the control (non-vacuity) --

func TestAFullySettledImageIsEligible(t *testing.T) {
	t.Parallel()

	got := decide(eligible())
	if !got.Removable() {
		t.Fatalf("verdict = %q (%s); the eligible path is unreachable, so every "+
			"retain assertion in this file proves nothing",
			got.Verdict, got.Reason.Explain())
	}
	if got.Reason != domain.ImageRetentionNone {
		t.Errorf("an eligible decision carried the reason %q", got.Reason)
	}
}

// ------------------------------------------------------ the reference model --

func TestEveryHardReferenceRetains(t *testing.T) {
	t.Parallel()

	// One field at a time, against evidence that is otherwise perfectly
	// eligible. Each case therefore proves that THAT field alone is enough to
	// stop a removal.
	cases := []struct {
		name  string
		apply func(*domain.ImageRetentionEvidence)
		want  domain.ImageRetentionReason
	}{
		{"a present container runs it", func(e *domain.ImageRetentionEvidence) {
			e.PresentContainers = 1
		}, domain.ImageRetainedInUse},

		{"two present containers share it", func(e *domain.ImageRetentionEvidence) {
			e.PresentContainers = 2
		}, domain.ImageRetainedInUse},

		{"a parked original still uses it", func(e *domain.ImageRetentionEvidence) {
			e.PreservedContainers = 1
		}, domain.ImageRetainedPreserved},

		{"it is HarborMaster's own image", func(e *domain.ImageRetentionEvidence) {
			e.IsSelf = true
		}, domain.ImageRetainedSelf},

		{"an acquisition is in flight", func(e *domain.ImageRetentionEvidence) {
			e.ActiveAcquisitions = 1
		}, domain.ImageRetainedActiveAcquisition},

		{"a recreation is in flight", func(e *domain.ImageRetentionEvidence) {
			e.ActiveExecutions = 1
		}, domain.ImageRetainedActiveExecution},

		{"a rollback is in flight", func(e *domain.ImageRetentionEvidence) {
			e.ActiveRollbacks = 1
		}, domain.ImageRetainedActiveRollback},

		{"a failed update is unsettled", func(e *domain.ImageRetentionEvidence) {
			e.UnsettledFailures = 1
		}, domain.ImageRetainedUnsettledFailure},

		{"a recovery plan is outstanding", func(e *domain.ImageRetentionEvidence) {
			e.OutstandingRecoveries = 1
		}, domain.ImageRetainedRecoveryOutstanding},

		{"a current plan proposes it", func(e *domain.ImageRetentionEvidence) {
			e.PlanTargets = 1
		}, domain.ImageRetainedPlanTarget},

		{"nothing superseded it", func(e *domain.ImageRetentionEvidence) {
			e.SettledAt = nil
		}, domain.ImageRetainedNotSuperseded},

		{"it is the previous rollback generation", func(e *domain.ImageRetentionEvidence) {
			e.NewerSupersededGenerations = 0
		}, domain.ImageRetainedRollbackGeneration},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := eligible()
			tc.apply(&evidence)

			got := decide(evidence)
			if got.Removable() {
				t.Fatalf("REMOVABLE, want retained (%s)", tc.want)
			}
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
			if got.Reason.Explain() == "" {
				t.Error("a retention reason must carry HarborMaster's own sentence")
			}
		})
	}

	// Non-vacuity for the table: every reason it claims to cover is a reason
	// the vocabulary actually declares.
	declared := make(map[domain.ImageRetentionReason]bool, len(domain.ImageRetentionReasons))
	for _, reason := range domain.ImageRetentionReasons {
		declared[reason] = true
	}
	for _, tc := range cases {
		if !declared[tc.want] {
			t.Errorf("%q is not in ImageRetentionReasons", tc.want)
		}
	}
}

// ------------------------------------------------------------ fail closed --

func TestIncompleteEvidenceRetains(t *testing.T) {
	t.Parallel()

	// THE CATCH-ALL. A caller that could not read a repository, or that added a
	// field and forgot to populate it, must not get a removal. EvidenceComplete
	// has to be set deliberately; its zero value is the safe one.
	evidence := eligible()
	evidence.EvidenceComplete = false

	got := decide(evidence)
	if got.Removable() {
		t.Fatal("an image was removable on evidence the caller did not establish")
	}
	if got.Reason != domain.ImageRetainedEvidenceIncomplete {
		t.Errorf("reason = %q, want evidenceIncomplete", got.Reason)
	}
}

func TestAnEmptyImageIdRetains(t *testing.T) {
	t.Parallel()

	evidence := eligible()
	evidence.ImageID = ""
	if decide(evidence).Removable() {
		t.Fatal("a decision about no image at all was removable")
	}
}

func TestTheZeroEvidenceIsRetained(t *testing.T) {
	t.Parallel()

	// The strongest fail-closed statement: a completely unpopulated argument
	// must produce retain, not "no references found".
	if decide(domain.ImageRetentionEvidence{}).Removable() {
		t.Fatal("zero evidence produced a removable image")
	}
}

// ------------------------------------------------------------- the policy --

func TestCleanupIsOffUnlessEnabled(t *testing.T) {
	t.Parallel()

	got := domain.DecideImageRetention(eligible(),
		domain.ImageRetentionPolicy{Enabled: false, MinAge: time.Hour}, retentionNow)
	if got.Removable() {
		t.Fatal("a deployment that never opted in removed an image")
	}
	if got.Reason != domain.ImageRetainedCleanupDisabled {
		t.Errorf("reason = %q, want cleanupDisabled", got.Reason)
	}
}

func TestAnEnabledPolicyWithNoAgeIsRefusedRatherThanCorrected(t *testing.T) {
	t.Parallel()

	// Zero age would make every settled image immediately removable -- the
	// configuration most likely to be a mistake and the one whose consequences
	// cannot be undone. Refused, because guessing what an operator meant about
	// deletion is not HarborMaster's decision.
	for _, age := range []time.Duration{0, -time.Hour} {
		policy := domain.ImageRetentionPolicy{Enabled: true, MinAge: age, KeepGenerations: 1}
		if policy.Usable() {
			t.Errorf("a policy with age %s reported itself usable", age)
		}
		got := domain.DecideImageRetention(eligible(), policy, retentionNow)
		if got.Removable() {
			t.Fatalf("age %s produced a removable image", age)
		}
	}
}

func TestAtLeastOneGenerationIsAlwaysKept(t *testing.T) {
	t.Parallel()

	// A configuration asking to keep none is raised to one. A workload with no
	// retained previous image has no rollback path at all, and an operator
	// setting zero almost certainly means "as few as possible".
	for _, configured := range []int{-5, 0} {
		policy := domain.ImageRetentionPolicy{
			Enabled: true, MinAge: time.Hour, KeepGenerations: configured,
		}
		if got := policy.KeptGenerations(); got != 1 {
			t.Errorf("KeepGenerations %d became %d, want 1", configured, got)
		}

		// And the effect: the immediately previous generation still retains.
		evidence := eligible()
		evidence.NewerSupersededGenerations = 0
		got := domain.DecideImageRetention(evidence, policy, retentionNow)
		if got.Removable() {
			t.Fatalf("KeepGenerations %d removed the only rollback generation", configured)
		}
	}
}

// ------------------------------------------------------------- generations --

func TestGenerationsAgeOutInOrder(t *testing.T) {
	t.Parallel()

	// A -> B -> C, with a policy keeping one generation.
	//
	//	B is what C replaced          -> the rollback generation, retained
	//	A is one further back         -> eligible once settled long enough
	policy := domain.ImageRetentionPolicy{Enabled: true, MinAge: time.Hour, KeepGenerations: 1}

	b := eligible()
	b.ImageID = "sha256:bbbb"
	b.NewerSupersededGenerations = 0
	if got := domain.DecideImageRetention(b, policy, retentionNow); got.Removable() {
		t.Fatal("the image the most recent update replaced was removable")
	}

	a := eligible()
	a.ImageID = "sha256:aaaa"
	a.NewerSupersededGenerations = 1
	if got := domain.DecideImageRetention(a, policy, retentionNow); !got.Removable() {
		t.Fatalf("an older fully settled generation was retained: %s", got.Reason.Explain())
	}
}

func TestKeepingMoreGenerationsRetainsMore(t *testing.T) {
	t.Parallel()

	// The same image, under a policy that keeps two.
	policy := domain.ImageRetentionPolicy{Enabled: true, MinAge: time.Hour, KeepGenerations: 2}

	evidence := eligible()
	evidence.NewerSupersededGenerations = 1
	if got := domain.DecideImageRetention(evidence, policy, retentionNow); got.Removable() {
		t.Fatal("a policy keeping two generations removed the second")
	}

	evidence.NewerSupersededGenerations = 2
	if got := domain.DecideImageRetention(evidence, policy, retentionNow); !got.Removable() {
		t.Fatalf("the third generation back was retained: %s", got.Reason.Explain())
	}
}

// ------------------------------------------------------------- the clock --

func TestTheRetentionClockStartsWhenTheLifecycleSettles(t *testing.T) {
	t.Parallel()

	policy := domain.ImageRetentionPolicy{
		Enabled: true, MinAge: 24 * time.Hour, KeepGenerations: 1,
	}

	// Settled an hour ago: inside the period.
	recent := retentionNow.Add(-time.Hour)
	evidence := eligible()
	evidence.SettledAt = &recent

	got := domain.DecideImageRetention(evidence, policy, retentionNow)
	if got.Removable() {
		t.Fatal("an image settled an hour ago was removable under a 24h policy")
	}
	if got.Reason != domain.ImageRetainedWithinRetentionAge {
		t.Fatalf("reason = %q, want withinRetentionAge", got.Reason)
	}
	// And the operator is told when the wait ends rather than left guessing.
	if got.EligibleAt == nil {
		t.Fatal("a time-retained image carried no eligible-at")
	}
	if want := recent.Add(24 * time.Hour); !got.EligibleAt.Equal(want) {
		t.Errorf("eligibleAt = %s, want %s", got.EligibleAt, want)
	}

	// Exactly at the boundary: the wait is over.
	if got := domain.DecideImageRetention(evidence, policy, want24(recent)); !got.Removable() {
		t.Errorf("at the boundary the image was still retained: %s", got.Reason.Explain())
	}
}

func want24(from time.Time) time.Time { return from.Add(24 * time.Hour) }

func TestAgeIsMeasuredFromSettlementNotImageCreation(t *testing.T) {
	t.Parallel()

	// An image built long ago but only just replaced is NOT old. The decision
	// has no creation date to be misled by -- this asserts that the only time
	// input is the settlement moment, by moving it and watching the verdict
	// follow.
	policy := domain.ImageRetentionPolicy{
		Enabled: true, MinAge: 24 * time.Hour, KeepGenerations: 1,
	}
	justSettled := retentionNow.Add(-time.Minute)

	evidence := eligible()
	evidence.SettledAt = &justSettled

	if got := domain.DecideImageRetention(evidence, policy, retentionNow); got.Removable() {
		t.Fatal("an image whose update settled a minute ago was removable")
	}
}

// --------------------------------------------- failure and rollback shapes --

func TestAFailedUpdateRetainsBothImages(t *testing.T) {
	t.Parallel()

	// A -> B fails verification. Both are kept: A restores service, B is the
	// evidence for why the update did not work.
	original := eligible()
	original.ImageID = "sha256:original"
	original.UnsettledFailures = 1

	replacement := eligible()
	replacement.ImageID = "sha256:replacement"
	replacement.UnsettledFailures = 1

	for _, evidence := range []domain.ImageRetentionEvidence{original, replacement} {
		got := decide(evidence)
		if got.Removable() {
			t.Fatalf("%s was removable while a failed update was unsettled", evidence.ImageID)
		}
		if got.Reason != domain.ImageRetainedUnsettledFailure {
			t.Errorf("%s reason = %q", evidence.ImageID, got.Reason)
		}
	}
}

func TestAQuarantinedReplacementKeepsItsImage(t *testing.T) {
	t.Parallel()

	// After a failed verification the replacement is stopped and renamed, not
	// removed -- it is the evidence. Its image must survive with it.
	evidence := eligible()
	evidence.PreservedContainers = 1
	evidence.UnsettledFailures = 0

	got := decide(evidence)
	if got.Removable() {
		t.Fatal("the image of a quarantined replacement was removable")
	}
	if got.Reason != domain.ImageRetainedPreserved {
		t.Errorf("reason = %q, want preserved", got.Reason)
	}
}

func TestAnAutomaticRollbackKeepsTheServingImageAndTheFailedTarget(t *testing.T) {
	t.Parallel()

	// Automation reverted B -> A. A is serving, so it is in use. B is the
	// failed target and stays until its lifecycle settles.
	serving := eligible()
	serving.ImageID = "sha256:A"
	serving.PresentContainers = 1
	if got := decide(serving); got.Reason != domain.ImageRetainedInUse {
		t.Errorf("the serving image reason = %q, want inUse", got.Reason)
	}

	// While the rollback is still running, nothing about B is settled.
	failed := eligible()
	failed.ImageID = "sha256:B"
	failed.ActiveRollbacks = 1
	if got := decide(failed); got.Removable() {
		t.Fatal("the failed target was removable during a rollback")
	}

	// Once the rollback finishes but a recovery plan stands, it is still kept.
	failed.ActiveRollbacks = 0
	failed.OutstandingRecoveries = 1
	if got := decide(failed); got.Reason != domain.ImageRetainedRecoveryOutstanding {
		t.Errorf("after rollback the reason = %q, want recoveryOutstanding", got.Reason)
	}
}

func TestManualRecoveryIsProtectedTheSameAsAutomatic(t *testing.T) {
	t.Parallel()

	// Eligibility is not coupled to whether automatic rollback is configured.
	// A manual operator who has not yet acted is protected by exactly the same
	// unsettled-failure and outstanding-recovery checks -- there is no field in
	// the evidence that says "automation is on", which is the point.
	manual := eligible()
	manual.OutstandingRecoveries = 1

	if got := decide(manual); got.Removable() {
		t.Fatal("an image was removed while a manual recovery was outstanding")
	}
}

// ------------------------------------------------------------ precedence --

func TestSelfOutranksEveryOtherCondition(t *testing.T) {
	t.Parallel()

	// Even a perfectly eligible image is kept if it is HarborMaster's own. A
	// process that removes the image it is running cannot be restarted and
	// cannot report that it broke itself.
	evidence := eligible()
	evidence.IsSelf = true

	got := decide(evidence)
	if got.Removable() {
		t.Fatal("HarborMaster's own image was removable")
	}
	if got.Reason != domain.ImageRetainedSelf {
		t.Errorf("reason = %q, want self", got.Reason)
	}
}

func TestEveryReasonHasASentence(t *testing.T) {
	t.Parallel()

	// A reason with no sentence is a decision nobody can audit.
	for _, reason := range domain.ImageRetentionReasons {
		if reason.Explain() == "" {
			t.Errorf("%q has no explanation", reason)
		}
	}
	if domain.ImageRetentionNone.Explain() == "" {
		t.Error("the eligible reason has no explanation")
	}
}
