package domain_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// "Up to date" and what it costs to say it (C3A).
//
// # The defect
//
// A ChangePlan is a proposed CHANGE, and the planner deliberately writes none
// when it compares an image and finds nothing newer -- a plan recording a
// non-event is not a plan. That left the settled, successful, entirely ordinary
// case with no trace in the subsystem the list row was reading: a container
// HarborMaster had checked and found current, and one it had never looked at,
// both had no plan and both reported "not checked".
//
// # The rule these tests hold
//
// The fix reads the registry comparison where it actually happens -- the image
// intelligence record -- and the bar for using it is POSITIVE EVIDENCE and
// nothing weaker. `update_type` is a column with a default; on its own it
// cannot tell "compared, nothing newer" from "never compared". Batches B1 and
// B1.1 established that distinction for planner-derived evidence and this file
// exists to hold it for the new path.
//
// Seven of the cases below are the same assertion from seven directions: the
// only route to upToDate is a comparison that actually answered.

func settledAt(t *testing.T, when string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, when)
	if err != nil {
		t.Fatalf("parse %q: %v", when, err)
	}
	return &parsed
}

// checked is evidence for a container with no plan and a settled comparison.
func checked(t *testing.T, update domain.UpdateType) domain.ContainerEvidence {
	t.Helper()
	return domain.ContainerEvidence{
		Present:       true,
		State:         domain.StateRunning,
		Health:        domain.HealthNone,
		CheckSettled:  true,
		CheckedUpdate: update,
		CheckStatus:   domain.CheckOK,
		LastSuccessAt: settledAt(t, "2026-09-01T10:00:00Z"),
	}
}

// ------------------------------------------------------ 1. never checked --

func TestAContainerNothingHasLookedAtIsNotChecked(t *testing.T) {
	t.Parallel()

	// No plan and no comparison. The zero value must assert nothing, which is
	// what keeps a deployment whose image intelligence has not run producing
	// exactly the verdicts it produced before C3A.
	got := domain.AssessContainer(domain.ContainerEvidence{
		Present: true, State: domain.StateRunning,
	})
	if got.State != domain.AttentionNotChecked {
		t.Fatalf("state = %q, want notChecked", got.State)
	}
	if got.LastCheckedOkAt != nil || got.CheckStatus != "" {
		t.Errorf("an unchecked container carried check evidence: %+v", got)
	}
}

// ------------------------------------- 2. checked successfully, current --

func TestASettledComparisonFindingNothingIsUpToDate(t *testing.T) {
	t.Parallel()

	// THE DEFECT THIS BATCH FIXES. No plan exists, because the planner writes
	// none for a non-event -- and the row must still be able to say the true
	// thing.
	got := domain.AssessContainer(checked(t, domain.UpdateNone))

	if got.State != domain.AttentionUpToDate {
		t.Fatalf("state = %q, want upToDate -- a successful comparison that "+
			"found nothing newer is the evidence for it", got.State)
	}
	// And it says WHEN, so the claim is datable rather than timeless.
	if got.LastCheckedOkAt == nil {
		t.Error("upToDate carried no successful-check time")
	}
	if got.CheckStatus != domain.CheckOK {
		t.Errorf("checkStatus = %q, want ok", got.CheckStatus)
	}
	// No plan was invented to reach this.
	if got.UpdateType != "" || got.ProposedImage != "" {
		t.Errorf("a settled current container acquired plan fields: %+v", got)
	}
}

// ------------------------------- 3. a real update still reads as before --

func TestASettledComparisonFindingAnUpdateDoesNotSayUpToDate(t *testing.T) {
	t.Parallel()

	// Intelligence alone never makes a row actionable: that remains the
	// planner's job, and without a plan there is no recommendation to act on.
	// What matters here is only that it does not claim to be current.
	for _, update := range []domain.UpdateType{
		domain.UpdatePatch, domain.UpdateMinor, domain.UpdateMajor, domain.UpdateDigest,
	} {
		got := domain.AssessContainer(checked(t, update))
		if got.State == domain.AttentionUpToDate {
			t.Errorf("a %q update reported upToDate", update)
		}
	}
}

// ----------------------- 4-7. never-successful statuses cannot be current --

func TestNoNeverSuccessfulStatusCanReportUpToDate(t *testing.T) {
	t.Parallel()

	// The invariant, from every direction the schema allows.
	//
	// Each of these rows carries update_type = none -- the COLUMN DEFAULT, not
	// a verdict. That is exactly the value a naive read would present as "up to
	// date", and it is what B1.1 established must never be trusted. Here the
	// gate is CheckSettled, which the store sets only from
	// ImageIntel.ComparisonSettled.
	for _, status := range []domain.CheckStatus{
		domain.CheckUnsupported,  // 4. never queued, never comparable again
		domain.CheckPending,      // 5. queued and not yet answered
		domain.CheckUnauthorized, // 6. refused
		domain.CheckFailed,       // 7. errored
		domain.CheckRateLimited,
		domain.CheckNotFound,
	} {
		evidence := domain.ContainerEvidence{
			Present: true, State: domain.StateRunning,
			// The store would not set CheckSettled for any of these; asserting
			// on the domain side too means a future caller that sets the fields
			// by hand still cannot produce the wrong verdict.
			CheckSettled:  false,
			CheckedUpdate: domain.UpdateNone,
			CheckStatus:   status,
		}
		got := domain.AssessContainer(evidence)
		if got.State == domain.AttentionUpToDate {
			t.Errorf("status %q reported upToDate from a defaulted update_type", status)
		}
		if got.State != domain.AttentionNotChecked {
			t.Errorf("status %q = %q, want notChecked", status, got.State)
		}
	}
}

func TestTheDomainPredicateRefusesEveryUnsettledRecord(t *testing.T) {
	t.Parallel()

	// The predicate itself, since it is now the single definition both the
	// planner and the read projection depend on.
	when := settledAt(t, "2026-09-01T10:00:00Z")

	// Unsupported is refused however successful it once was: it is never
	// queued again, so any verdict it carries is frozen and unrefreshable.
	frozen := domain.ImageIntel{
		Status: domain.CheckUnsupported, Update: domain.UpdateNone, LastSuccessAt: when,
	}
	if frozen.ComparisonSettled() {
		t.Error("an unsupported reference reported a settled comparison")
	}
	if frozen.ObservedUpdate() != domain.UpdateUnknown {
		t.Errorf("unsupported observed %q, want unknown", frozen.ObservedUpdate())
	}

	// Never answered: the stored verdict is the column default.
	never := domain.ImageIntel{Status: domain.CheckPending, Update: domain.UpdateNone}
	if never.ComparisonSettled() {
		t.Error("a record that never answered reported a settled comparison")
	}
	if never.ObservedUpdate() != domain.UpdateUnknown {
		t.Errorf("never-answered observed %q, want unknown", never.ObservedUpdate())
	}

	// Answered: the verdict is real.
	real := domain.ImageIntel{Status: domain.CheckOK, Update: domain.UpdateNone, LastSuccessAt: when}
	if !real.ComparisonSettled() {
		t.Error("a successful comparison was not recognised")
	}
	if real.ObservedUpdate() != domain.UpdateNone {
		t.Errorf("observed %q, want none", real.ObservedUpdate())
	}
}

// ------------------------------------------------ 8. stale prior evidence --

func TestAFailedRecheckKeepsTheEarlierVerdictAndSaysSo(t *testing.T) {
	t.Parallel()

	// A real earlier comparison found the image current; the latest attempt
	// failed. B1.1 established that the earlier verdict is PRESERVED rather
	// than discarded -- it remains the best knowledge available -- so the row
	// still reads upToDate.
	//
	// What must not happen is presenting it as fresh certainty. The status of
	// the latest attempt travels with the verdict, so an interface can say "up
	// to date as of <time>" instead of an unqualified claim about right now.
	evidence := checked(t, domain.UpdateNone)
	evidence.CheckStatus = domain.CheckFailed

	got := domain.AssessContainer(evidence)
	if got.State != domain.AttentionUpToDate {
		t.Fatalf("state = %q; a failed re-check must not discard a real earlier "+
			"comparison (B1.1)", got.State)
	}
	if got.CheckStatus != domain.CheckFailed {
		t.Errorf("checkStatus = %q, want failed -- the row must be able to say "+
			"the latest attempt did not answer", got.CheckStatus)
	}
	if got.LastCheckedOkAt == nil {
		t.Error("no timestamp accompanied a stale verdict, so it cannot be dated")
	}
}

// ---------------------------------------- 9-10. the surrounding contract --

func TestUpToDateWithoutAPlanStillDefersToStrongerAnswers(t *testing.T) {
	t.Parallel()

	// The new branch mirrors the plan branch's guards rather than inventing
	// its own precedence. An unattributable digest cannot be declared current:
	// "nothing newer" means "nothing to compare against" there.
	untracked := checked(t, domain.UpdateNone)
	untracked.LineageKnown = true
	untracked.Tracked = false
	if got := domain.AssessContainer(untracked); got.State != domain.AttentionNotTracked {
		t.Errorf("an untracked digest reported %q, want notTracked", got.State)
	}

	// And the states that outrank every image verdict still outrank this one.
	for _, probe := range []struct {
		name  string
		mutex func(*domain.ContainerEvidence)
		want  domain.AttentionState
	}{
		{"unhealthy", func(e *domain.ContainerEvidence) {
			e.Health = domain.HealthUnhealthy
		}, domain.AttentionUnhealthy},
		{"awaiting approval", func(e *domain.ContainerEvidence) {
			e.AwaitingApproval = true
		}, domain.AttentionApprovalRequired},
		{"paused", func(e *domain.ContainerEvidence) {
			e.AutomationPaused = true
		}, domain.AttentionPaused},
		{"preserved", func(e *domain.ContainerEvidence) {
			e.Preserved = domain.PreservedOriginal
		}, domain.AttentionPreserved},
	} {
		evidence := checked(t, domain.UpdateNone)
		probe.mutex(&evidence)
		if got := domain.AssessContainer(evidence); got.State != probe.want {
			t.Errorf("%s reported %q, want %q -- C3A must not reorder precedence",
				probe.name, got.State, probe.want)
		}
	}
}

func TestThePlanIsTheAuthorityUntilAComparisonSettles(t *testing.T) {
	t.Parallel()

	// Wherever nothing has been established, the plan decides exactly as it
	// always did. This is the ordinary case and the one that must not move.
	evidence := domain.ContainerEvidence{
		Present: true, State: domain.StateRunning,
		PlanKnown:      true,
		UpdateType:     domain.UpdateMinor,
		Recommendation: domain.RecommendProceed,
	}
	if got := domain.AssessContainer(evidence); got.State != domain.AttentionUpdateAvailable {
		t.Fatalf("state = %q, want updateAvailable", got.State)
	}

	// And a settled comparison that still finds an update leaves the plan to
	// say how big it is and what to do about it.
	evidence.CheckSettled = true
	evidence.CheckedUpdate = domain.UpdateMinor
	evidence.CheckStatus = domain.CheckOK
	evidence.LastSuccessAt = settledAt(t, "2026-09-01T10:00:00Z")
	if got := domain.AssessContainer(evidence); got.State != domain.AttentionUpdateAvailable {
		t.Fatalf("state = %q, want updateAvailable", got.State)
	}
}

func TestASettledCurrentVerdictSupersedesAStalePlan(t *testing.T) {
	t.Parallel()

	// THE CASE FOUND ON A REAL BACKEND, and the reason the settled comparison
	// is checked above the plan rather than only in its absence.
	//
	// Plans are written before the first registry check completes, when nothing
	// is established and the honest verdict is `unknown`. Once the check
	// settles on `none` the planner SKIPS the container -- permanently, by its
	// own rule -- so it never writes a superseding row and the pre-check plan
	// survives forever. A model reading the newest plan reports "cannot advise"
	// for a container HarborMaster has known to be current for weeks.
	stale := checked(t, domain.UpdateNone)
	stale.PlanKnown = true
	stale.UpdateType = domain.UpdateUnknown

	if got := domain.AssessContainer(stale); got.State != domain.AttentionUpToDate {
		t.Fatalf("state = %q, want upToDate -- a pre-check plan must not mask a "+
			"comparison that has since answered", got.State)
	}

	// The same holds for a plan that proposed a change before the comparison
	// settled: a settled `none` is the state in which the planner declines to
	// stand behind any plan at all, so a surviving row is one it would not
	// write today.
	superseded := checked(t, domain.UpdateNone)
	superseded.PlanKnown = true
	superseded.UpdateType = domain.UpdatePatch
	superseded.Recommendation = domain.RecommendProceed

	if got := domain.AssessContainer(superseded); got.State != domain.AttentionUpToDate {
		t.Fatalf("state = %q, want upToDate", got.State)
	}
}
