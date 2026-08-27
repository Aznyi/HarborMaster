package arch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stage 17.4 readiness: structural proof that a QUERY cannot become an ENGINE.
//
// # What these add to the guards that already exist
//
// `internal/service/automation_readiness.go` begins with "automation", so every
// guard in automation_arch_test.go already covers it: it may not name a Docker
// capability, may not perform a Docker operation, and may not reach the
// moby SDK. Those are not restated here.
//
// What is left is the readiness-specific half, and it is the half a future
// change is most likely to break: readiness must keep DELEGATING the decision
// rather than making one, and must keep writing nothing. Both are easy to
// violate with a well-meant edit -- "just check the recommendation here", "just
// mark the policy as previewed" -- and neither would fail a behavioural test
// that only looked at the answer.

// readinessSource reads the readiness implementation.
func readinessSource(t *testing.T) string {
	t.Helper()

	path := filepath.Join(moduleRoot(t), "internal", "service", "automation_readiness.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read automation_readiness.go: %v", err)
	}
	return string(source)
}

// TestReadinessDelegatesTheDecisionRatherThanMakingOne is the load-bearing one.
//
// The whole design rests on readiness reaching the SAME decision function a
// pass reaches. A readiness surface that inspected a plan's recommendation, or
// re-tested a strategy ceiling, would be a second eligibility model -- and the
// number an operator reads would stop being the number automation honours.
func TestReadinessDelegatesTheDecisionRatherThanMakingOne(t *testing.T) {
	source := readinessSource(t)

	// It must reach the real decision function.
	if !strings.Contains(source, "DecideAutomation(") {
		t.Error("automation_readiness.go does not call DecideAutomation\n" +
			"\treadiness is a query over the real decision, not a second implementation")
	}
	// And the real dependency gate.
	if !strings.Contains(source, "applyDependencyGate(") {
		t.Error("automation_readiness.go does not apply the dependency gate\n" +
			"\tomitting it is exactly the defect Stage 17.4 fixed: a container the " +
			"pass holds was reported as eligible")
	}

	// It must not re-derive any part of the decision itself.
	forbidden := map[string]string{
		"AssessRisk":              "the risk model belongs to the planner",
		"recommendationSatisfies": "the recommendation floor is DecideAutomation's step 8",
		".Permits(":               "the strategy ceiling is DecideAutomation's step 7",
		"SelectUpdatePolicy(":     "policy selection is DecideAutomation's step 3",
		"ParseUpdateLabels(":      "the label opt-out is DecideAutomation's step 2",
		"SelfMatch(":              "the self-update refusal is DecideAutomation's step 0",
		"DecideDependency(":       "the dependency verdict belongs to the gate",
	}
	for symbol, why := range forbidden {
		if strings.Contains(source, symbol) {
			t.Errorf("automation_readiness.go names %s\n\t%s; readiness must ask, not decide",
				symbol, why)
		}
	}
}

// TestReadinessCannotSubmitWork proves the absence of every mutation path.
//
// Readiness holds the same service the pass holds, so the pipeline is reachable
// from `s`. Nothing stops a future edit calling it except this.
func TestReadinessCannotSubmitWork(t *testing.T) {
	source := readinessSource(t)

	forbidden := []string{
		"s.pipeline",
		"RequestAcquisition(",
		"RequestExecution(",
		"RequestRollback(",
		"s.submit(",
		"StartRun(",
		"FinishRun(",
		"RecordDecisions(",
		"s.store.Pause(",
		"s.store.Resume(",
		"recordAudit(",
		"advanceDependencyOperations(",
	}
	for _, symbol := range forbidden {
		if strings.Contains(source, symbol) {
			t.Errorf("automation_readiness.go names %s\n"+
				"\treadiness answers a question; it may not submit work, record a run, "+
				"or write any state", symbol)
		}
	}
}

// TestReadinessCannotWritePolicyState is the §16 side-effect guard.
//
// A preview that quietly saved the thing being previewed would be the worst
// possible failure of this feature: the operator asked what WOULD happen and
// caused it instead.
func TestReadinessCannotWritePolicyState(t *testing.T) {
	source := readinessSource(t)

	for _, symbol := range []string{
		"policies.Create(", "policies.Update(", "policies.Archive(",
		"UpdatePolicyChange", "Normalise()", "Validate(",
	} {
		if strings.Contains(source, symbol) {
			t.Errorf("automation_readiness.go names %s\n"+
				"\ta candidate policy is assembled and validated at the API boundary; "+
				"the query neither stores one nor rewrites one", symbol)
		}
	}
}

// TestTheReadinessCandidateIdentifierCannotBePersisted pins the sentinel.
//
// The preview identifier must not be a storable one. If it were, a candidate
// could match a real row, and a preview could be confused with a policy.
func TestTheReadinessCandidateIdentifierCannotBePersisted(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "internal", "domain", "automation_readiness.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read domain/automation_readiness.go: %v", err)
	}

	// The constant exists and is built from the prefix plus a run of `z`, which
	// is outside the hex alphabet a generated identifier uses. The behavioural
	// half -- ValidUpdatePolicyID refusing it -- is asserted in the domain
	// tests; this is the structural half, so the constant cannot quietly become
	// a valid identifier.
	if !strings.Contains(string(source), "AutomationReadinessCandidatePolicyID") {
		t.Fatal("the preview sentinel is gone")
	}
	if !strings.Contains(string(source), `"zzzzzzzzzzzzzzzzzzzz"`) {
		t.Error("the preview sentinel is no longer outside the generated-identifier alphabet\n" +
			"\tit must be unstorable AND must lose every identifier tie-break")
	}
}
