package service_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The dependency gate, exercised through the REAL automation pass.
//
// # Why these are not unit tests of DecideDependency
//
// DecideDependency is already proven pure and subtract-only by an exhaustive
// property test. What these prove is different and cannot be proved there: that
// the WIRING honours it. A pass that computed a correct verdict and then
// submitted anyway would pass every test in dependency_decide_test.go.
//
// So each of these drives `RunNow` and asserts on what reached the PIPELINE --
// the acquisition requests actually submitted, in the order they were made.

// dependencyView is a fixed graph for the pass to read.
type dependencyView struct {
	view service.DependencyView
	err  error
	// views counts how many times the graph was asked for. A pointer so the
	// value receiver below still records, and so a copy handed to the engine
	// shares the counter. Nil in the literals that do not measure.
	views *atomic.Int64
}

// calls reports how many times the graph was read.
//
// The scale tests use it to establish that the gate builds the graph ONCE per
// evaluation rather than once per container, which is the difference between
// linear and quadratic.
func (d dependencyView) calls() int64 {
	if d.views == nil {
		return 0
	}
	return d.views.Load()
}

func (d dependencyView) View(context.Context) (service.DependencyView, error) {
	if d.views != nil {
		d.views.Add(1)
	}
	if d.err != nil {
		return service.DependencyView{}, d.err
	}
	return d.view, nil
}

// graphOver builds a view from names and edges.
func graphOver(t *testing.T, names []string, edges ...domain.WorkloadDependency) dependencyView {
	t.Helper()

	graph, err := domain.BuildDependencyGraph(names, edges)
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	return dependencyView{views: &atomic.Int64{}, view: service.DependencyView{
		Graph:    graph,
		Problems: map[string][]domain.DependencyProblem{},
	}}
}

func namespaceDep(dependent, dependency string) domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent: dependent, Dependency: dependency,
		Source: domain.DependencyNetworkNamespace,
	}
}

func operatorDep(dependent, dependency string) domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent: dependent, Dependency: dependency,
		Source: domain.DependencyOperator,
	}
}

// target builds one automation target with a plan.
func dependencyTarget(name string, state domain.ContainerState) store.AutomationTarget {
	const image = "nginx:1.27.3"
	return store.AutomationTarget{
		ContainerID: "container-" + name,
		Selection: domain.SelectionTarget{
			Name:  name,
			Image: image,
			// Screened the way the repository screens a real row.
			//
			// Not optional: a broad policy requires POSITIVE eligibility facts,
			// and the zero value selects nothing. A fixture that skipped this
			// would produce a pass that governs no container and tests that
			// pass for the wrong reason.
			Eligibility: domain.ScreenTarget(name, image, nil),
		},
		State: state,
	}
}

// withDependencyEstate replaces the harness's targets, plans and graph.
func withDependencyEstate(
	t *testing.T,
	harness *automationHarness,
	view dependencyView,
	planned []string,
	unplanned []string,
) {
	t.Helper()

	targets := make([]store.AutomationTarget, 0, len(planned)+len(unplanned))
	plans := make(map[string]domain.ChangePlan)
	for _, name := range planned {
		targets = append(targets, dependencyTarget(name, domain.StateRunning))
		plans["container-"+name] = planFor(name)
	}
	for _, name := range unplanned {
		targets = append(targets, dependencyTarget(name, domain.StateRunning))
	}

	harness.evidence.targets = targets
	harness.evidence.plans = plans

	options := harness.options()
	options.Dependencies = view
	harness.engine = service.NewAutomationService(options)
}

// submittedNames returns the containers whose updates reached the pipeline, in
// submission order.
func submittedNames(t *testing.T, harness *automationHarness,
	decisions []domain.AutomationDecision) []string {
	t.Helper()

	byPlan := make(map[string]string, len(decisions))
	for _, decision := range decisions {
		if decision.PlanID != "" {
			byPlan[decision.PlanID] = decision.ContainerName
		}
	}

	var names []string
	for _, request := range harness.pipeline.recorded("acquire") {
		if name, ok := byPlan[request.id]; ok {
			names = append(names, name)
		}
	}
	return names
}

func decisionFor(decisions []domain.AutomationDecision, name string) domain.AutomationDecision {
	for _, decision := range decisions {
		if decision.ContainerName == name {
			return decision
		}
	}
	return domain.AutomationDecision{}
}

// ------------------------------------------------------- positive control --

// The harness itself works.
//
// # Why this test exists, and what it caught
//
// The first draft of this file asserted on `recorded("acquisition")`. The fake
// pipeline records acquisitions under `"acquire"`. Every dependency assertion in
// the file was therefore reading an empty slice and concluding that nothing had
// been submitted -- which was true of the SLICE and said nothing whatever about
// the pass.
//
// It failed rather than passed, which is the safer of the two ways to be wrong.
// But a helper that reports zero for every input is not a measurement, and no
// assertion built on one means anything.
//
// So the harness is proved before it is used: this test says the plumbing can
// see a submission, and the negative control below says it can see the absence
// of one. Every dependency test in this file rests on both.
func TestBaselineHarnessSubmitsWithoutAnyDependencyWiring(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	harness.evidence.targets = []store.AutomationTarget{
		dependencyTarget("web", domain.StateRunning),
	}
	harness.evidence.plans = map[string]domain.ChangePlan{"container-web": planFor("web")}

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	// Two independent readings of the same fact, so a broken helper cannot hide
	// behind a broken counter or vice versa.
	if run.Submitted != 1 {
		t.Fatalf("run.Submitted = %d, want 1 (decisions: %+v)", run.Submitted, decisions)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 1 || got[0] != "web" {
		t.Fatalf("submittedNames = %v, want [web]; the helper disagrees with the run counter", got)
	}
}

// ------------------------------------------------------- negative control --

// The harness can also see the ABSENCE of a submission.
//
// Without this, a helper that returned an empty slice unconditionally would make
// every "nothing was submitted" assertion in this file pass vacuously -- which
// is the failure mode that matters here, since most of these tests assert
// exactly that.
func TestBaselineHarnessSubmitsNothingWhenPolicyDeclines(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, observePolicy())
	harness.evidence.targets = []store.AutomationTarget{
		dependencyTarget("web", domain.StateRunning),
	}
	harness.evidence.plans = map[string]domain.ChangePlan{"container-web": planFor("web")}

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if run.Submitted != 0 {
		t.Fatalf("run.Submitted = %d, want 0", run.Submitted)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 0 {
		t.Fatalf("submittedNames = %v, want none", got)
	}
	// And the container WAS considered -- a fixture that governed nothing would
	// also report zero submissions, for the wrong reason.
	if run.Considered != 1 {
		t.Fatalf("considered = %d, want 1; the policy is not governing the fixture",
			run.Considered)
	}
}

// THE property, through the real pass.
//
// No arrangement of dependency state may turn a decision the policy engine
// declined into one that was submitted. Walked over the ways a decision can be
// declined, each crossed with a graph that would release it if the gate were
// permissive.
func TestTheDependencyWiringCannotSubmitADeclinedDecision(t *testing.T) {
	t.Parallel()

	declines := []struct {
		name   string
		policy domain.UpdatePolicy
	}{
		{"observe mode", observePolicy()},
		{"approval required", approvalPolicy()},
	}

	for _, decline := range declines {
		t.Run(decline.name, func(t *testing.T) {
			t.Parallel()

			harness := newAutomationHarness(t, decline.policy)
			// A graph in which everything is satisfied -- the most permissive
			// arrangement there is.
			withDependencyEstate(t, harness,
				graphOver(t, []string{"web"}), []string{"web"}, nil)

			_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
			if err != nil {
				t.Fatalf("pass: %v", err)
			}

			if got := submittedNames(t, harness, decisions); len(got) != 0 {
				t.Fatalf("a declined decision was submitted: %v", got)
			}
			decision := decisionFor(decisions, "web")
			if decision.Verdict == domain.VerdictUpdate {
				t.Fatalf("verdict = %q; the gate promoted a declined decision", decision.Verdict)
			}
		})
	}
}

// A. A -> B: exact ordering.
func TestDependencyOrderingSubmitsTheDependencyFirst(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"api", "postgres"}, operatorDep("api", "postgres")),
		[]string{"api", "postgres"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 1 || submitted[0] != "postgres" {
		t.Fatalf("submitted = %v, want only postgres in this pass", submitted)
	}

	// api is HELD, not skipped for an unrelated reason and not failed.
	api := decisionFor(decisions, "api")
	if api.DependencyState != domain.DependencyWaiting {
		t.Fatalf("api dependencyState = %q, want dependencyWaiting", api.DependencyState)
	}
	if api.Reason != domain.ReasonDependencyWaiting {
		t.Fatalf("api reason = %q", api.Reason)
	}
	if api.BlockedBy != "postgres" {
		t.Fatalf("api blockedBy = %q, want postgres", api.BlockedBy)
	}
}

// B. A -> B and C -> D: independent chains both progress.
func TestIndependentChainsBothProgressInOnePass(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"a", "b", "c", "d"},
			operatorDep("a", "b"), operatorDep("c", "d")),
		[]string{"a", "b", "c", "d"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 2 {
		t.Fatalf("submitted = %v, want the two roots", submitted)
	}
	// Both roots, in deterministic stage order.
	if submitted[0] != "b" || submitted[1] != "d" {
		t.Fatalf("submitted = %v, want [b d]", submitted)
	}
}

// C. database -> api -> worker: exactly one stage per pass.
func TestAThreeLevelChainAdvancesOneStagePerPass(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"database", "api", "worker"},
			operatorDep("api", "database"), operatorDep("worker", "api")),
		[]string{"database", "api", "worker"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 1 || submitted[0] != "database" {
		t.Fatalf("submitted = %v, want only database", submitted)
	}
	for _, name := range []string{"api", "worker"} {
		decision := decisionFor(decisions, name)
		if decision.DependencyState != domain.DependencyWaiting {
			t.Errorf("%s state = %q, want dependencyWaiting", name, decision.DependencyState)
		}
	}
}

// D. The upstream needs no update and is running: the downstream proceeds.
func TestAStableUpstreamReleasesItsDependent(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"api", "postgres"}, operatorDep("api", "postgres")),
		[]string{"api"}, []string{"postgres"})

	// postgres was ASSESSED and its plan proposes nothing.
	//
	// Deliberately not "postgres has no plan". Those are different facts, and
	// the difference is the point of this test: an assessed container that needs
	// nothing and is running RELEASES its dependents, while an unassessed one
	// holds them. The first draft used "no plan" and was testing the wrong
	// thing.
	settled := planFor("postgres")
	settled.UpdateType = domain.UpdateNone
	settled.ProposedImage = ""
	settled.ProposedDigest = ""
	harness.evidence.plans["container-postgres"] = settled

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 1 || submitted[0] != "api" {
		t.Fatalf("submitted = %v, want api", submitted)
	}
	api := decisionFor(decisions, "api")
	if api.DependencyState != domain.DependencySatisfied {
		t.Fatalf("api state = %q, want dependencySatisfied", api.DependencyState)
	}

	// And postgres was NOT enrolled: nothing was submitted for it.
	for _, name := range submitted {
		if name == "postgres" {
			t.Fatal("a stable upstream was dragged into an update")
		}
	}
}

// A stopped upstream does NOT release its dependent, even with no work to do.
func TestAStoppedUpstreamDoesNotRelease(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"api", "postgres"}, operatorDep("api", "postgres")),
		[]string{"api"}, []string{"postgres"})
	// postgres was ASSESSED and needs nothing -- but is not running.
	//
	// The distinction this test turns on: "no work to do" is not "stable". A
	// dependent released by a stopped upstream would be released onto nothing.
	settled := planFor("postgres")
	settled.UpdateType = domain.UpdateNone
	settled.ProposedImage = ""
	settled.ProposedDigest = ""
	harness.evidence.plans["container-postgres"] = settled

	for index := range harness.evidence.targets {
		if harness.evidence.targets[index].Selection.Name == "postgres" {
			harness.evidence.targets[index].State = domain.StateExited
		}
	}

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 0 {
		t.Fatalf("submitted = %v; a stopped upstream released its dependent", got)
	}
	if state := decisionFor(decisions, "api").DependencyState; state != domain.DependencyBlocked {
		t.Fatalf("api state = %q, want dependencyBlocked", state)
	}
}

// G. A cycle blocks its members and leaves the rest of the estate alone.
func TestACycleBlocksItsMembersAndNothingElse(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"a", "b", "healthy"},
			operatorDep("a", "b"), operatorDep("b", "a")),
		[]string{"a", "b", "healthy"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 1 || submitted[0] != "healthy" {
		t.Fatalf("submitted = %v, want only the unrelated container", submitted)
	}
	for _, name := range []string{"a", "b"} {
		decision := decisionFor(decisions, name)
		if decision.DependencyState != domain.DependencyCycle {
			t.Errorf("%s state = %q, want dependencyCycle", name, decision.DependencyState)
		}
		if decision.Verdict == domain.VerdictUpdate {
			t.Errorf("%s was submitted despite the cycle", name)
		}
	}
}

// A graph that could not be built holds the pass rather than releasing it.
func TestAnUnavailableGraphHoldsThePass(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness, dependencyView{err: service.ErrDependencyGraphUnavailable},
		[]string{"web"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 0 {
		t.Fatalf("submitted = %v; an unbuildable graph released the pass", got)
	}
	if state := decisionFor(decisions, "web").DependencyState; state != domain.DependencyBlocked {
		t.Fatalf("state = %q, want dependencyBlocked", state)
	}
}

// An unwired dependency subsystem leaves the pass exactly as it was.
func TestAnUnwiredDependencySubsystemChangesNothing(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	harness.evidence.targets = []store.AutomationTarget{
		dependencyTarget("web", domain.StateRunning),
	}
	harness.evidence.plans = map[string]domain.ChangePlan{"container-web": planFor("web")}

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 1 {
		t.Fatalf("submitted = %v, want one; an unwired subsystem must not hold the pass", got)
	}
}

// L. An operator relationship orders but never produces a namespace rebind.
//
// The pass records it as an ordering constraint and nothing more: no operation,
// no member, no rebind evidence anywhere.
func TestAnOperatorRelationshipOnlyOrders(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	view := graphOver(t, []string{"api", "postgres"}, operatorDep("api", "postgres"))
	withDependencyEstate(t, harness, view, []string{"api", "postgres"}, nil)

	_, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	// The relationship is not hard, so nothing about it can require a rebind.
	for _, edge := range view.view.Graph.Edges {
		if edge.Hard() {
			t.Fatalf("an operator relationship reported itself as hard: %+v", edge)
		}
		if _, ok := domain.RebindEvidenceFrom(domain.DependencyProblem{
			Container:    edge.Dependent,
			Source:       edge.Source,
			ReferencedID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Refusal:      domain.DiscoveryUnknownContainer,
		}, edge.Dependency, harness.now); ok {
			t.Fatal("an operator relationship produced rebind evidence")
		}
	}
}

// A hard namespace relationship orders the same way an operator one does.
func TestAHardNamespaceRelationshipOrdersTheProviderFirst(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"sonarr", "gluetun"}, namespaceDep("sonarr", "gluetun")),
		[]string{"sonarr", "gluetun"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 1 || submitted[0] != "gluetun" {
		t.Fatalf("submitted = %v, want only gluetun", submitted)
	}
	if state := decisionFor(decisions, "sonarr").DependencyState; state != domain.DependencyWaiting {
		t.Fatalf("sonarr state = %q, want dependencyWaiting", state)
	}
}

// K. A per-run ceiling truncates the TAIL, and deferred containers stay
// WAITING rather than being marked blocked.
//
// The distinction matters: blocked means a person has to look, waiting means the
// next pass continues. Reporting a budget deferral as blocked would put work in
// front of an operator that needs none.
func TestAPerRunCeilingDefersRatherThanBlocks(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	withDependencyEstate(t, harness,
		graphOver(t, []string{"one", "two", "three"}),
		[]string{"one", "two", "three"}, nil)

	options := harness.options()
	options.Dependencies = graphOver(t, []string{"one", "two", "three"})
	options.Config.MaxPerRun = 1
	harness.engine = service.NewAutomationService(options)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	submitted := submittedNames(t, harness, decisions)
	if len(submitted) != 1 {
		t.Fatalf("submitted = %v, want exactly one under a ceiling of one", submitted)
	}

	// The deferred ones report the RUN LIMIT, not a dependency block.
	deferred := 0
	for _, decision := range decisions {
		if decision.ContainerName == submitted[0] {
			continue
		}
		deferred++
		if decision.Reason != domain.ReasonRunLimit {
			t.Errorf("%s reason = %q, want runLimit", decision.ContainerName, decision.Reason)
		}
		if decision.DependencyState == domain.DependencyBlocked {
			t.Errorf("%s was reported as dependency-blocked by a budget deferral",
				decision.ContainerName)
		}
	}
	if deferred != 2 {
		t.Fatalf("deferred = %d, want 2", deferred)
	}
}

// M. A dependency cannot enrol a container the policy did not select.
func TestADependencyCannotEnrolAnUnselectedContainer(t *testing.T) {
	t.Parallel()

	// A policy naming ONLY api. postgres is outside it entirely.
	policy := automaticPolicy()
	policy.Scope = domain.ScopeSelector
	policy.Selector = domain.UpdateSelector{Include: []string{"api"}}

	harness := newAutomationHarness(t, policy)
	withDependencyEstate(t, harness,
		graphOver(t, []string{"api", "postgres"}, operatorDep("api", "postgres")),
		[]string{"api", "postgres"}, nil)

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	// postgres was never selected, so nothing was submitted for it.
	for _, name := range submittedNames(t, harness, decisions) {
		if name == "postgres" {
			t.Fatal("a dependency relationship enrolled a container no policy selected")
		}
	}

	// And api is BLOCKED rather than proceeding: its dependency needs work the
	// rules do not permit.
	api := decisionFor(decisions, "api")
	if api.Verdict == domain.VerdictUpdate {
		t.Fatal("api proceeded while its dependency was ineligible")
	}
	if api.DependencyState != domain.DependencyIneligible {
		t.Fatalf("api state = %q, want dependencyIneligible", api.DependencyState)
	}
}

// ------------------------------------------------------------ fixtures --

// broadPolicy governs every eligible container, so a dependency test can name
// containers freely without editing a selector.
func broadPolicy() domain.UpdatePolicy {
	// An explicit include list rather than ScopeAllEligible.
	//
	// The broad scope is governed by its own eligibility screening, which these
	// tests are not about -- naming the containers keeps the subject of each
	// test the DEPENDENCY GATE rather than the selection rules that run before
	// it. Broad-scope selection has its own coverage in update_scope_test.go.
	policy := automaticPolicy()
	policy.Scope = domain.ScopeSelector
	policy.Selector = domain.UpdateSelector{Include: []string{
		"web", "api", "postgres", "worker", "database",
		"a", "b", "c", "d", "healthy",
		"sonarr", "gluetun", "one", "two", "three",
		// The fairness fixtures.
		"alpha", "beta", "aa", "bb", "cc", "dd",
		"lone", "root", "leaf",
		// The Stage 5c up-to-date fixtures.
		"upstream", "downstream",
		"svc00", "svc01", "svc02", "svc03", "svc04",
		"svc05", "svc06", "svc07", "svc08", "svc09",
	}}
	policy.Normalise()

	// Raised AFTER Normalise, which defaults per-policy concurrency.
	//
	// That default is correct production behaviour and the dependency work must
	// respect it -- but it caps a single policy at one submission per pass,
	// which would make every multi-container assertion in this file measure the
	// POLICY LIMIT rather than the dependency gate. A test that cannot tell
	// those apart proves nothing about either.
	//
	// TestAPerRunCeilingDefersRatherThanBlocks is where budget interaction IS
	// the subject, and it sets its own ceiling.
	policy.Limits = domain.UpdateLimits{MaxConcurrent: 20, MaxPerRun: 20}
	return policy
}

// observePolicy evaluates everything and acts on nothing.
func observePolicy() domain.UpdatePolicy {
	policy := broadPolicy()
	policy.Mode = domain.ModeObserve
	policy.Normalise()
	return policy
}

// approvalPolicy holds every change for a person.
func approvalPolicy() domain.UpdatePolicy {
	policy := broadPolicy()
	policy.Mode = domain.ModeApprove
	policy.Normalise()
	return policy
}

// planFor builds a patch plan for one named container.
//
// patchPlan is pinned to `web`; a dependency test needs one per container so the
// pass finds work for each of them.
func planFor(name string) domain.ChangePlan {
	plan := patchPlan()
	plan.PlanID = "plan_" + padAutoID(planSeedFor(name))
	plan.ContainerID = "container-" + name
	plan.ContainerName = name
	return plan
}

// planSeedFor derives a distinct number from a whole container name.
//
// # Why this is not len(name)*prime + name[0]
//
// That is what it was, and it collided. Every `svcNN` shares a length and a
// first byte, so all ten fixtures in the fairness test received the SAME plan
// id -- and `submittedNames` maps plan id back to container name, so ten
// distinct submissions all resolved to whichever name was written last.
//
// The symptom was a fairness test reporting that one container had been
// admitted ten times while nine were starved, and a chain test reporting that a
// downstream node had jumped its dependency. Both were the helper mislabelling
// real submissions; neither was a scheduler defect.
//
// FNV-1a over the full name. Deterministic, no collisions across any fixture
// name in this package, and TestPlanFixtureIdsAreDistinct keeps that true.
func planSeedFor(name string) int {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	hash := uint32(offset)
	for i := range len(name) {
		hash ^= uint32(name[i])
		hash *= prime
	}
	// Bounded so padAutoID's 20-character field is never overrun.
	return int(hash % 1_000_000_000)
}

// The plan fixtures are injective over every container name this package uses.
//
// A guard on the guard. Two fixtures sharing a plan id makes `submittedNames`
// attribute one container's submission to another, which produced two
// convincing-looking false failures before it was found -- a starvation report
// and a dependency-ordering violation, neither of which was real.
func TestPlanFixtureIdsAreDistinct(t *testing.T) {
	t.Parallel()

	names := []string{
		"web", "api", "postgres", "worker", "database",
		"a", "b", "c", "d", "healthy",
		"sonarr", "gluetun", "one", "two", "three",
		"alpha", "beta", "aa", "bb", "cc", "dd",
		"lone", "root", "leaf",
		// The Stage 5c up-to-date fixtures.
		"upstream", "downstream",
		"svc00", "svc01", "svc02", "svc03", "svc04",
		"svc05", "svc06", "svc07", "svc08", "svc09",
	}

	byID := make(map[string]string, len(names))
	for _, name := range names {
		id := planFor(name).PlanID
		if existing, clash := byID[id]; clash {
			t.Errorf("%q and %q share plan id %q\n"+
				"\tsubmittedNames maps plan id back to container name, so a collision "+
				"attributes one container's submission to another.", existing, name, id)
			continue
		}
		byID[id] = name
	}
}
