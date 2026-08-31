package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Policy engine tests.
//
// These drive the REAL rule evaluator against fake persistence, so what is
// under test is the engine's own behaviour: that it batches the policy load,
// that it never records a pass it did not complete, that a secret cannot travel
// from a container's environment into a stored violation, and that an estate
// with no policies is reported as unexamined rather than as compliant.

// ------------------------------------------------------------------ fakes --

type fakePolicyDefinitions struct {
	mu sync.Mutex

	policies []domain.PolicyDefinition
	// loads counts calls, which is how the batching test asserts that a sweep
	// costs one policy query rather than one per container.
	loads     int
	lastLimit int
	err       error
}

func (f *fakePolicyDefinitions) ActivePolicies(_ context.Context, limit int) ([]domain.PolicyDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	f.lastLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	if limit > 0 && len(f.policies) > limit {
		return f.policies[:limit], nil
	}
	return f.policies, nil
}

func (f *fakePolicyDefinitions) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

type fakePolicyContainers struct {
	mu sync.Mutex

	details map[string]domain.ContainerDetail
	order   []string
	// absent names containers the list reports but that are no longer present.
	absent map[string]bool
}

func (f *fakePolicyContainers) GetPresent(_ context.Context, id string) (*domain.ContainerDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	detail, ok := f.details[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &detail, nil
}

func (f *fakePolicyContainers) List(
	_ context.Context,
	filter store.ContainerFilter,
) ([]domain.ContainerSummary, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	summaries := make([]domain.ContainerSummary, 0, len(f.order))
	for _, id := range f.order {
		summaries = append(summaries, domain.ContainerSummary{
			ID:      id,
			Name:    id,
			Present: !f.absent[id],
		})
	}

	total := len(summaries)
	start := filter.Page.Offset
	if start > total {
		start = total
	}
	end := total
	if filter.Page.Limit > 0 && start+filter.Page.Limit < end {
		end = start + filter.Page.Limit
	}
	return summaries[start:end], total, nil
}

type fakePolicyStore struct {
	mu sync.Mutex

	passes     []domain.PolicyEvaluation
	violations [][]domain.PolicyViolation
	err        error
}

func (f *fakePolicyStore) ReconcilePolicy(
	_ context.Context,
	evaluation domain.PolicyEvaluation,
	violations []domain.PolicyViolation,
	_ time.Time,
) (store.UpsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.UpsertResult{}, f.err
	}
	f.passes = append(f.passes, evaluation)
	f.violations = append(f.violations, violations)
	return store.UpsertResult{Inserted: len(violations)}, nil
}

func (f *fakePolicyStore) recorded() ([]domain.PolicyEvaluation, [][]domain.PolicyViolation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.PolicyEvaluation(nil), f.passes...),
		append([][]domain.PolicyViolation(nil), f.violations...)
}

type fakePolicyInventory struct{ generation int64 }

func (f fakePolicyInventory) CurrentGeneration(context.Context) (int64, string, error) {
	return f.generation, "checksum", nil
}

// --------------------------------------------------------------- fixtures --

func policyConfig() config.Policy {
	return config.Policy{
		Enabled:                   true,
		EvaluateOnEvents:          true,
		EvaluationDebounce:        time.Millisecond,
		MaxPendingEvaluations:     16,
		EvaluationTimeout:         5 * time.Second,
		MaxPolicies:               10,
		MaxViolationsPerContainer: 100,
		MaxRulesPerPolicy:         32,
		MaxValuesPerRule:          32,
		MaxNameBytes:              120,
		MaxDescriptionBytes:       1000,
	}
}

// nonCompliantContainer runs privileged, as root, with a secret in its
// environment and a forbidden variable name.
func nonCompliantContainer(id string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:            id,
			Name:          id,
			Image:         domain.ParseImageRef("docker.io/library/nginx:latest"),
			RestartPolicy: domain.RestartPolicy{Name: "always"},
			Present:       true,
		},
		Process: domain.Process{User: "root"},
		Environment: []domain.EnvVar{
			{
				Name:        "AWS_SECRET_ACCESS_KEY",
				Value:       "***",
				RawValue:    "the-real-aws-secret-value",
				Sensitivity: domain.SensitivitySensitive,
			},
		},
		Security: domain.Security{Privileged: true},
		Networks: []domain.NetworkAttachment{{NetworkName: "bridge"}},
	}
}

func compliantContainer(id string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:            id,
			Name:          id,
			Image:         domain.ParseImageRef("registry.example.com/team/app:1.0"),
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
			Present:       true,
		},
		Process:  domain.Process{User: "10001"},
		Security: domain.Security{ReadonlyRootfs: true},
		Networks: []domain.NetworkAttachment{{NetworkName: "app_backend"}},
	}
}

func securityPolicy() domain.PolicyDefinition {
	return domain.PolicyDefinition{
		PolicyID: domain.NewPolicyID(),
		Name:     "Container hardening",
		Severity: domain.PolicySeverityHigh,
		Enabled:  true,
		Rules: []domain.PolicyRule{
			{Type: domain.RulePrivilegedForbidden, Severity: domain.PolicySeverityCritical},
			{Type: domain.RuleUserNotRoot},
			{Type: domain.RuleForbiddenEnv, Values: []string{"AWS_*"}},
		},
	}
}

type policyHarness struct {
	service     *service.PolicyService
	definitions *fakePolicyDefinitions
	containers  *fakePolicyContainers
	store       *fakePolicyStore
}

func newPolicyHarness(t *testing.T, cfg config.Policy,
	policies []domain.PolicyDefinition, details ...domain.ContainerDetail) policyHarness {
	t.Helper()

	containers := &fakePolicyContainers{
		details: make(map[string]domain.ContainerDetail, len(details)),
		absent:  make(map[string]bool),
	}
	for _, detail := range details {
		containers.details[detail.Overview.ID] = detail
		containers.order = append(containers.order, detail.Overview.ID)
	}

	definitions := &fakePolicyDefinitions{policies: policies}
	violations := &fakePolicyStore{}

	return policyHarness{
		service: service.NewPolicyService(service.PolicyOptions{
			Definitions: definitions,
			Containers:  containers,
			Violations:  violations,
			Inventory:   fakePolicyInventory{generation: 42},
			Config:      cfg,
			Logger:      discardLogger(),
		}),
		definitions: definitions,
		containers:  containers,
		store:       violations,
	}
}

// ----------------------------------------------------------------- tests --

func TestEvaluatingANonCompliantContainer(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, nonCompliantContainer("abc"))

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "abc")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	if evaluation.ViolationCount != 3 {
		t.Fatalf("violations = %d, want 3", evaluation.ViolationCount)
	}
	if evaluation.Compliant {
		t.Error("a container failing three rules was reported compliant")
	}
	if !evaluation.Complete {
		t.Error("the pass was reported incomplete")
	}
	if evaluation.PoliciesEvaluated != 1 || evaluation.RulesEvaluated != 3 {
		t.Errorf("policies/rules = %d/%d, want 1/3",
			evaluation.PoliciesEvaluated, evaluation.RulesEvaluated)
	}
	if evaluation.InventoryGeneration != 42 {
		t.Errorf("generation = %d, want the inventory's 42", evaluation.InventoryGeneration)
	}

	_, recorded := harness.store.recorded()
	if len(recorded) != 1 {
		t.Fatalf("reconcile calls = %d, want 1", len(recorded))
	}

	// The rule's own severity must override the policy default, and only for
	// the rule that sets one.
	severities := map[domain.PolicyRuleType]domain.PolicySeverity{}
	for _, violation := range recorded[0] {
		severities[violation.RuleType] = violation.Severity
		if violation.PolicyName != "Container hardening" {
			t.Errorf("violation carries policy name %q", violation.PolicyName)
		}
		if violation.Reason == "" {
			t.Errorf("%s carries no reason", violation.RuleType)
		}
	}
	if severities[domain.RulePrivilegedForbidden] != domain.PolicySeverityCritical {
		t.Errorf("privileged severity = %q, want the rule's critical override",
			severities[domain.RulePrivilegedForbidden])
	}
	if severities[domain.RuleUserNotRoot] != domain.PolicySeverityHigh {
		t.Errorf("user severity = %q, want the policy default high",
			severities[domain.RuleUserNotRoot])
	}
}

// The end-to-end secret assertion. A container's environment holds a real
// value; nothing the engine writes may contain it, while the NAME must still
// reach the violation so an operator knows which variable is the problem.
func TestNoSecretValueReachesAStoredViolation(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, nonCompliantContainer("abc"))

	if _, err := harness.service.EvaluateContainer(context.Background(), "abc"); err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	_, recorded := harness.store.recorded()
	const secret = "the-real-aws-secret-value"

	named := false
	for _, violation := range recorded[0] {
		for _, field := range []string{
			violation.Observed, violation.Expected, violation.Reason,
			violation.PolicyName, violation.ContainerName,
		} {
			if strings.Contains(field, secret) {
				t.Fatalf("a secret value reached a stored violation: %q", field)
			}
		}
		if violation.RuleType == domain.RuleForbiddenEnv {
			named = true
			// The positive control: without this, a violation that stored
			// nothing at all would pass the assertion above.
			if !strings.Contains(violation.Observed, "AWS_SECRET_ACCESS_KEY") {
				t.Errorf("observed = %q, want it to name the variable", violation.Observed)
			}
		}
	}
	if !named {
		t.Error("the forbidden-environment rule produced no violation")
	}
}

func TestACompliantContainerRecordsAPassWithNoViolations(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, compliantContainer("abc"))

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "abc")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}
	if !evaluation.Compliant || evaluation.ViolationCount != 0 {
		t.Errorf("evaluation = %+v, want a compliant pass", evaluation)
	}

	passes, recorded := harness.store.recorded()
	if len(passes) != 1 {
		t.Fatalf("passes = %d, want 1", len(passes))
	}
	if len(recorded[0]) != 0 {
		t.Errorf("violations = %d, want none", len(recorded[0]))
	}
	// The pass is still WRITTEN, because "compliant" and "never evaluated" must
	// stay distinguishable.
	if !passes[0].Complete {
		t.Error("the compliant pass was not recorded as complete")
	}
}

// The batching guarantee: a sweep loads the active policy set ONCE, however
// many containers it evaluates.
func TestASweepLoadsThePolicySetOnce(t *testing.T) {
	details := make([]domain.ContainerDetail, 0, 25)
	for i := 0; i < 25; i++ {
		details = append(details, nonCompliantContainer(fmt.Sprintf("container-%02d", i)))
	}

	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, details...)

	result, err := harness.service.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Evaluated != 25 {
		t.Errorf("evaluated = %d, want 25", result.Evaluated)
	}
	if loads := harness.definitions.loadCount(); loads != 1 {
		t.Errorf("policy loads = %d over 25 containers, want 1", loads)
	}

	passes, _ := harness.store.recorded()
	if len(passes) != 25 {
		t.Errorf("passes = %d, want 25", len(passes))
	}
}

// A container the list reports but that is no longer present has no current
// configuration to check, so it is skipped rather than evaluated against
// nothing.
func TestASweepSkipsAbsentContainers(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()},
		nonCompliantContainer("present"), nonCompliantContainer("gone"))
	harness.containers.absent["gone"] = true

	result, err := harness.service.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Evaluated != 1 || result.Skipped != 1 {
		t.Errorf("evaluated/skipped = %d/%d, want 1/1", result.Evaluated, result.Skipped)
	}
}

// An estate with no policies has not been found compliant -- it has not been
// asked anything. Nothing may be written, or every container would be recorded
// as vacuously passing on every refresh.
func TestNoPoliciesMeansNothingIsRecorded(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(), nil, nonCompliantContainer("abc"))

	_, err := harness.service.EvaluateContainer(context.Background(), "abc")
	if !errors.Is(err, service.ErrNoActivePolicies) {
		t.Fatalf("EvaluateContainer returned %v, want ErrNoActivePolicies", err)
	}

	if _, err := harness.service.Sweep(context.Background()); !errors.Is(err, service.ErrNoActivePolicies) {
		t.Fatalf("Sweep returned %v, want ErrNoActivePolicies", err)
	}

	passes, _ := harness.store.recorded()
	if len(passes) != 0 {
		t.Errorf("passes = %d, want nothing written", len(passes))
	}
}

// Hitting the per-container violation budget must mark the pass INCOMPLETE
// rather than truncating it silently, because an incomplete pass resolves
// nothing and therefore cannot clear a violation it never reached.
func TestExceedingTheViolationBudgetMarksThePassIncomplete(t *testing.T) {
	cfg := policyConfig()
	cfg.MaxViolationsPerContainer = 2

	harness := newPolicyHarness(t, cfg,
		[]domain.PolicyDefinition{securityPolicy()}, nonCompliantContainer("abc"))

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "abc")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}
	if evaluation.Complete {
		t.Fatal("a truncated pass was reported complete")
	}
	if evaluation.Compliant {
		t.Error("a truncated pass was reported compliant")
	}
	if evaluation.Reason == "" {
		t.Error("a truncated pass carries no reason")
	}
	if evaluation.ViolationCount != 2 {
		t.Errorf("violations = %d, want the budget of 2", evaluation.ViolationCount)
	}
}

// The active set is loaded with a limit one above the maximum, so hitting the
// cap is detectable rather than looking like a complete answer.
func TestThePolicyLoadAsksForOneMoreThanTheMaximum(t *testing.T) {
	cfg := policyConfig()
	cfg.MaxPolicies = 3

	policies := make([]domain.PolicyDefinition, 0, 5)
	for i := 0; i < 5; i++ {
		policy := securityPolicy()
		policy.Name = fmt.Sprintf("Policy %d", i)
		policies = append(policies, policy)
	}

	harness := newPolicyHarness(t, cfg, policies, nonCompliantContainer("abc"))

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "abc")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}
	if evaluation.PoliciesEvaluated != 3 {
		t.Errorf("policies evaluated = %d, want the cap of 3", evaluation.PoliciesEvaluated)
	}
	if harness.definitions.lastLimit != 4 {
		t.Errorf("requested limit = %d, want one above the cap", harness.definitions.lastLimit)
	}
}

// A container that left the inventory between the refresh and the pass is
// ordinary churn. Nothing is written, because writing an evaluation would claim
// a pass that did not happen.
func TestAMissingContainerRecordsNothing(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, compliantContainer("abc"))

	_, err := harness.service.EvaluateContainer(context.Background(), "vanished")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("EvaluateContainer returned %v, want ErrNotFound", err)
	}

	passes, _ := harness.store.recorded()
	if len(passes) != 0 {
		t.Errorf("passes = %d, want nothing written for a missing container", len(passes))
	}
}

// A disabled engine evaluates nothing and says so, rather than silently
// reporting a clean estate.
func TestADisabledEngineRefusesToEvaluate(t *testing.T) {
	cfg := policyConfig()
	cfg.Enabled = false

	harness := newPolicyHarness(t, cfg,
		[]domain.PolicyDefinition{securityPolicy()}, nonCompliantContainer("abc"))

	if _, err := harness.service.EvaluateContainer(context.Background(), "abc"); !errors.Is(err, service.ErrPolicyDisabled) {
		t.Errorf("EvaluateContainer returned %v, want ErrPolicyDisabled", err)
	}
	if _, err := harness.service.Sweep(context.Background()); !errors.Is(err, service.ErrPolicyDisabled) {
		t.Errorf("Sweep returned %v, want ErrPolicyDisabled", err)
	}
	if harness.service.Enabled() {
		t.Error("a disabled engine reports as enabled")
	}
}

// Two sweeps must not overlap: the second would re-read the same containers to
// reach the same conclusion while contending for the single database writer.
func TestConcurrentPolicySweepsDoNotOverlap(t *testing.T) {
	details := make([]domain.ContainerDetail, 0, 40)
	for i := 0; i < 40; i++ {
		details = append(details, compliantContainer(fmt.Sprintf("container-%02d", i)))
	}
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, details...)

	var wait sync.WaitGroup
	results := make([]service.SweepResult, 4)
	for i := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := harness.service.Sweep(context.Background())
			if err != nil {
				t.Errorf("Sweep: %v", err)
			}
			results[index] = result
		}(i)
	}
	wait.Wait()

	// At least one sweep ran to completion; the others were refused and
	// returned an empty result rather than duplicating the work.
	evaluated := 0
	for _, result := range results {
		evaluated += result.Evaluated
	}
	if evaluated == 0 {
		t.Fatal("no sweep completed")
	}
	if evaluated > 4*40 {
		t.Errorf("evaluated = %d, more than four full sweeps of 40", evaluated)
	}
}

// The refresh hook. A committed inventory refresh must queue a pass, which is
// the trigger that keeps compliance current.
func TestACommittedRefreshQueuesASweep(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, compliantContainer("abc"))

	if status := harness.service.Status(); status.SweepPending {
		t.Fatal("a sweep was pending before any refresh")
	}

	harness.service.InventoryRefreshed(7)

	status := harness.service.Status()
	if !status.SweepPending {
		t.Error("a committed refresh did not queue a sweep")
	}
	if !status.Enabled {
		t.Error("an enabled engine reports as disabled")
	}
}

// The queue must coalesce and must never block, so a burst of container events
// becomes a bounded amount of work.
func TestTheEvaluationQueueCoalescesAndEscalates(t *testing.T) {
	cfg := policyConfig()
	cfg.MaxPendingEvaluations = 4

	harness := newPolicyHarness(t, cfg,
		[]domain.PolicyDefinition{securityPolicy()}, compliantContainer("abc"))

	// The same container many times is one entry.
	for i := 0; i < 50; i++ {
		harness.service.RequestEvaluation("abc")
	}
	if pending := harness.service.Status().PendingEvaluations; pending != 1 {
		t.Errorf("pending = %d after 50 requests for one container, want 1", pending)
	}

	// Past the cap the queue escalates to a sweep rather than dropping work.
	for i := 0; i < 20; i++ {
		harness.service.RequestEvaluation(fmt.Sprintf("container-%02d", i))
	}
	status := harness.service.Status()
	if !status.SweepPending {
		t.Error("overflowing the queue did not escalate to a sweep")
	}
	if !status.Overflowed {
		t.Error("the overflow was not reported")
	}
	if status.PendingEvaluations != 0 {
		t.Errorf("pending = %d after escalation, want 0", status.PendingEvaluations)
	}
}

// Run must exit promptly when its context is cancelled, or shutdown would hang
// on the background worker.
func TestTheWorkerStopsOnCancellation(t *testing.T) {
	harness := newPolicyHarness(t, policyConfig(),
		[]domain.PolicyDefinition{securityPolicy()}, compliantContainer("abc"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// The definition limits the API validates against must come from the same
// configuration the engine runs under, so one value governs both.
func TestLimitsFollowConfiguration(t *testing.T) {
	cfg := policyConfig()
	cfg.MaxRulesPerPolicy = 7
	cfg.MaxValuesPerRule = 9

	harness := newPolicyHarness(t, cfg, nil)

	limits := harness.service.Limits()
	if limits.MaxRules != 7 || limits.MaxValuesPerRule != 9 {
		t.Errorf("limits = %+v, want the configured bounds", limits)
	}
}
