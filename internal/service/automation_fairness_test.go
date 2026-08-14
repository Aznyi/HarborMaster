package service_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Fairness across repeated passes, and the starvation question.
//
// # What could go wrong
//
// Stage ordering is a deterministic sort, and within a stage it preserves the
// target order the repository produced -- which is `ORDER BY id`. With a tight
// MaxPerRun that means the same container is offered to the budget first on
// every pass. If nothing else changed, the second independent branch would never
// be admitted: deterministic ordering would become deterministic starvation.
//
// # Why it does not happen, established empirically below
//
// A container that IS submitted stops competing. `loadContainerEvidence` reads
// AcquisitionActive and ExecutionActive, and DecideAutomation step 10 turns an
// in-flight container into ReasonInFlight -- which is not eligible, so it is not
// offered to the budget at all on the next pass.
//
// So the ordering is deterministic WITHIN a pass and self-rotating ACROSS
// passes: the front of the queue removes itself by being served. That is a
// property of the existing in-flight rule rather than anything Phase 16 added,
// and these tests pin it so a future change to either mechanism cannot quietly
// reintroduce starvation.
//
// The tests are BOUNDED. Each runs a fixed number of passes and asserts every
// branch was admitted within them; none of them says "eventually".

// fairnessHarness runs repeated passes over a persistent estate.
//
// `submitted` accumulates across passes and feeds the in-flight evidence, which
// is what models production: a container with outstanding work is not offered
// again until that work settles.
type fairnessHarness struct {
	harness   *automationHarness
	view      dependencyView
	submitted []string
}

func newFairnessHarness(
	t *testing.T,
	view dependencyView,
	maxPerRun int,
	names ...string,
) *fairnessHarness {
	t.Helper()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness, view, names, nil)

	options := harness.options()
	options.Dependencies = view
	options.Config.MaxPerRun = maxPerRun
	// The per-policy ceiling is a separate mechanism with its own coverage.
	// Raised here so the pass-level MaxPerRun is what this test measures.
	harness.engine = service.NewAutomationService(options)

	return &fairnessHarness{harness: harness, view: view}
}

// pass runs one decision pass and records what it submitted.
//
// Submitted containers are marked in flight, exactly as production does: the
// acquisition they started is outstanding until it settles, and DecideAutomation
// declines a container with outstanding work.
func (f *fairnessHarness) pass(t *testing.T) []string {
	t.Helper()

	before := len(f.harness.pipeline.recorded("acquire"))
	_, decisions, err := f.harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	byPlan := make(map[string]string, len(decisions))
	for _, decision := range decisions {
		if decision.PlanID != "" {
			byPlan[decision.PlanID] = decision.ContainerName
		}
	}

	var admitted []string
	for _, request := range f.harness.pipeline.recorded("acquire")[before:] {
		name, ok := byPlan[request.id]
		if !ok {
			continue
		}
		admitted = append(admitted, name)
		// It now has outstanding work, so it stops competing.
		f.harness.evidence.inFlight["container-"+name] = true
	}
	f.submitted = append(f.submitted, admitted...)
	return admitted
}

// settle clears a container's outstanding work, so it may compete again.
func (f *fairnessHarness) settle(name string) {
	delete(f.harness.evidence.inFlight, "container-"+name)
}

// A. Two independent ready nodes, MaxPerRun = 1.
//
// The starvation case in its simplest form: the same node sorts first every
// time. Both must be admitted within two passes.
func TestFairnessTwoIndependentNodesAtBudgetOne(t *testing.T) {
	t.Parallel()

	fair := newFairnessHarness(t, graphOver(t, []string{"alpha", "beta"}), 1, "alpha", "beta")

	first := fair.pass(t)
	if len(first) != 1 {
		t.Fatalf("pass 1 admitted %v, want exactly one", first)
	}
	second := fair.pass(t)
	if len(second) != 1 {
		t.Fatalf("pass 2 admitted %v, want exactly one", second)
	}

	// THE assertion: the second pass admitted the OTHER one.
	if second[0] == first[0] {
		t.Fatalf("both passes admitted %q; the first-sorted node starved the other\n"+
			"\tDeterministic ordering must not become deterministic starvation. A "+
			"container that was submitted has outstanding work and should stop "+
			"competing until it settles.", first[0])
	}

	admitted := map[string]bool{first[0]: true, second[0]: true}
	for _, name := range []string{"alpha", "beta"} {
		if !admitted[name] {
			t.Errorf("%q was never admitted in two passes", name)
		}
	}
}

// B. Ten independent ready nodes, MaxPerRun = 2.
//
// Five passes must cover all ten, with no container admitted twice while its
// work is outstanding.
func TestFairnessTenIndependentNodesAtBudgetTwo(t *testing.T) {
	t.Parallel()

	names := make([]string, 0, 10)
	for i := range 10 {
		names = append(names, fmt.Sprintf("svc%02d", i))
	}

	fair := newFairnessHarness(t, graphOver(t, names), 2, names...)

	seen := make(map[string]int)
	for pass := 1; pass <= 5; pass++ {
		admitted := fair.pass(t)
		if len(admitted) != 2 {
			t.Fatalf("pass %d admitted %v, want exactly two", pass, admitted)
		}
		for _, name := range admitted {
			seen[name]++
		}
	}

	if len(seen) != 10 {
		t.Fatalf("%d distinct containers admitted across five passes, want 10: %v",
			len(seen), seen)
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("%q was admitted %d times while its work was outstanding", name, count)
		}
	}
}

// C. Two independent chains, MaxPerRun = 1.
//
// Stage 1 is {A, C}; stage 2 is {B, D}. Neither downstream node may be admitted
// before its own upstream is satisfied, and both upstreams must get their turn.
func TestFairnessTwoChainsAtBudgetOne(t *testing.T) {
	t.Parallel()

	view := graphOver(t, []string{"aa", "bb", "cc", "dd"},
		operatorDep("bb", "aa"), operatorDep("dd", "cc"))
	fair := newFairnessHarness(t, view, 1, "aa", "bb", "cc", "dd")

	first := fair.pass(t)
	second := fair.pass(t)

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("passes admitted %v then %v, want one each", first, second)
	}

	// STAGE BEATS FAIRNESS. Neither downstream node may appear while its
	// upstream is unsatisfied, whatever the rotation does.
	for _, admitted := range [][]string{first, second} {
		for _, name := range admitted {
			if name == "bb" || name == "dd" {
				t.Fatalf("%q was admitted before its dependency was satisfied", name)
			}
		}
	}

	// Both stage-1 roots got a turn.
	if first[0] == second[0] {
		t.Fatalf("both passes admitted %q; one chain starved the other", first[0])
	}
	roots := map[string]bool{first[0]: true, second[0]: true}
	if !roots["aa"] || !roots["cc"] {
		t.Fatalf("admitted %v; want both chain roots", roots)
	}
}

// D. A mixed estate: independent work alongside a chain.
func TestFairnessMixedIndependentAndChainWorkload(t *testing.T) {
	t.Parallel()

	view := graphOver(t, []string{"lone", "root", "leaf"},
		operatorDep("leaf", "root"))
	fair := newFairnessHarness(t, view, 1, "lone", "root", "leaf")

	admitted := make(map[string]bool)
	for pass := 1; pass <= 3; pass++ {
		step := fair.pass(t)
		t.Logf("pass %d admitted %v", pass, step)
		for _, name := range step {
			if name == "leaf" && !admitted["root"] {
				t.Fatal("leaf was admitted before root")
			}
			admitted[name] = true
		}
	}

	// The independent container and the chain root both got their turn within
	// the bound; neither branch monopolised the budget.
	if !admitted["lone"] || !admitted["root"] {
		t.Fatalf("admitted %v; both the independent container and the chain root "+
			"should have progressed", admitted)
	}
}

// E. A restart halfway through does not put one branch permanently behind.
//
// The rotation is driven by persisted in-flight facts rather than by anything
// the process remembers, so a new engine over the same evidence continues from
// where the old one left off.
func TestFairnessSurvivesARestart(t *testing.T) {
	t.Parallel()

	fair := newFairnessHarness(t, graphOver(t, []string{"alpha", "beta"}), 1, "alpha", "beta")

	first := fair.pass(t)

	// The restart: a brand new engine over the same evidence and pipeline.
	options := fair.harness.options()
	options.Dependencies = fair.view
	options.Config.MaxPerRun = 1
	fair.harness.engine = service.NewAutomationService(options)

	second := fair.pass(t)

	if len(second) != 1 {
		t.Fatalf("pass after restart admitted %v, want one", second)
	}
	if second[0] == first[0] {
		t.Fatalf("a restart re-admitted %q while its work was outstanding; "+
			"fairness reset and the other branch is starved", first[0])
	}
}

// A settled container competes again, and does not monopolise.
//
// The other half of the rotation: work leaving the in-flight set must let a
// container back in, or a busy estate would eventually admit nothing.
func TestASettledContainerCompetesAgain(t *testing.T) {
	t.Parallel()

	fair := newFairnessHarness(t, graphOver(t, []string{"alpha", "beta"}), 1, "alpha", "beta")

	first := fair.pass(t)
	fair.pass(t)
	// Both are now outstanding. Settle the first.
	fair.settle(first[0])

	third := fair.pass(t)
	if len(third) != 1 || third[0] != first[0] {
		t.Fatalf("pass 3 admitted %v, want the settled container %q", third, first[0])
	}
}

// Deferred work reports the BUDGET, never a dependency failure.
//
// The distinction an operator acts on: runLimit resolves itself on the next
// pass; dependencyBlocked needs a person.
func TestBudgetDeferralIsNeverReportedAsADependencyFailure(t *testing.T) {
	t.Parallel()

	fair := newFairnessHarness(t, graphOver(t, []string{"alpha", "beta"}), 1, "alpha", "beta")

	_, decisions, err := fair.harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	deferred := 0
	for _, decision := range decisions {
		if decision.Reason != domain.ReasonRunLimit {
			continue
		}
		deferred++
		if decision.DependencyState == domain.DependencyBlocked ||
			decision.DependencyState == domain.DependencyIneligible ||
			decision.DependencyState == domain.DependencyCycle {
			t.Errorf("%q was deferred by the budget but reports %q",
				decision.ContainerName, decision.DependencyState)
		}
	}
	if deferred != 1 {
		t.Fatalf("deferred = %d, want 1", deferred)
	}
}

// A container with outstanding dependency rebind work is not given new work.
//
// The follower/pass interaction: in-flight work retains precedence, and the pass
// must not submit a second acquisition for a container already being reattached.
func TestThePassDoesNotCompeteWithOutstandingRebindWork(t *testing.T) {
	t.Parallel()

	fair := newFairnessHarness(t, graphOver(t, []string{"alpha", "beta"}), 10, "alpha", "beta")

	// alpha is mid-rebind: it has an outstanding acquisition, exactly as the
	// follower would have left it.
	fair.harness.evidence.inFlight["container-alpha"] = true

	_, decisions, err := fair.harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	for _, request := range fair.harness.pipeline.recorded("acquire") {
		for _, decision := range decisions {
			if decision.PlanID == request.id && decision.ContainerName == "alpha" {
				t.Fatal("the pass submitted new work for a container already being reattached")
			}
		}
	}

	// And the unrelated container still progressed.
	var betaSubmitted bool
	for _, decision := range decisions {
		if decision.ContainerName == "beta" && decision.Verdict == domain.VerdictUpdate {
			betaSubmitted = true
		}
	}
	if !betaSubmitted {
		t.Fatal("unrelated work was blocked by another container's outstanding rebind")
	}

	// alpha reports the in-flight rule, not a dependency problem.
	for _, decision := range decisions {
		if decision.ContainerName == "alpha" && decision.Reason != domain.ReasonInFlight {
			t.Fatalf("alpha reason = %q, want alreadyInFlight", decision.Reason)
		}
	}
}

// Compile-time reminder that the fairness harness uses the real target type.
var _ = store.AutomationTarget{}
