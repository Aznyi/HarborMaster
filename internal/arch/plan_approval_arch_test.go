package arch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stage 17.7: structural proof that an approval is EVIDENCE, not a capability.
//
// # The three ways this feature could go wrong
//
//  1. it could apply the update it approves, turning one click into a
//     recreation;
//  2. it could rewrite the plan it approves, so the assessment an operator
//     reviewed stops being the assessment that authorised the change;
//  3. automation could start consuming approvals, turning one human judgement
//     about one plan into standing unattended authority.
//
// None of the three would fail a behavioural test that only checked "an
// approved plan executes". Each is a structural property, so each is asserted
// structurally.

func readSource(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{moduleRoot(t)}, parts...)...)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(source)
}

// TestPlanApprovalHoldsNoCapability is (1).
//
// The service reads plans and writes approval rows. It must not be able to
// download an image, recreate a container, or reach Docker at all.
func TestPlanApprovalHoldsNoCapability(t *testing.T) {
	source := readSource(t, "internal", "service", "plan_approval.go")

	forbidden := map[string]string{
		"docker.":             "the approval service holds no Docker capability",
		"moby":                "the Docker SDK never leaves internal/docker",
		"RequestAcquisition(": "approving is not downloading",
		"RequestExecution(":   "approving is not applying",
		"RequestRollback(":    "approving is not rolling back",
		"AcquisitionService":  "the approval service reaches no pipeline",
		"ExecutionService":    "the approval service reaches no pipeline",
		"registry.":           "approval reads stored evidence; it contacts no registry",
		"SnapshotCapturer":    "approval captures nothing",
	}
	for symbol, why := range forbidden {
		if strings.Contains(source, symbol) {
			t.Errorf("internal/service/plan_approval.go names %s\n\t%s", symbol, why)
		}
	}
}

// TestPlanApprovalCannotWriteAPlan is (2), and the load-bearing one.
//
// A ChangePlan is insert-only. An approval that could rewrite one -- lower a
// score, drop a factor, change a recommendation -- would destroy the property
// the whole design rests on: that the assessment an operator reviewed IS the
// assessment that authorised the change.
//
// The service's plan interface is READ-only by construction; this asserts that
// nobody widened it.
func TestPlanApprovalCannotWriteAPlan(t *testing.T) {
	source := readSource(t, "internal", "service", "plan_approval.go")

	for _, symbol := range []string{
		"InsertPlans(", "plans.Insert", "plans.Update", "plans.Delete",
		"PruneOrphans(", "Risk.Score =", "Risk.Recommendation =", "Risk.Factors =",
	} {
		if strings.Contains(source, symbol) {
			t.Errorf("internal/service/plan_approval.go names %s\n"+
				"\ta change plan is immutable evidence; an approval sits NEXT to it "+
				"and never rewrites it", symbol)
		}
	}

	// And the interface it declares must expose no writer.
	if !strings.Contains(source, "type PlanApprovalPlans interface") {
		t.Fatal("the plan reader interface is gone; this guard no longer checks anything")
	}
	reader := source[strings.Index(source, "type PlanApprovalPlans interface"):]
	reader = reader[:strings.Index(reader, "}")]
	for _, verb := range []string{"Insert", "Update", "Delete", "Write", "Save"} {
		if strings.Contains(reader, verb) {
			t.Errorf("PlanApprovalPlans exposes %s; it must read only", verb)
		}
	}
}

// TestAutomationDoesNotConsumePlanApprovals is (3).
//
// This is what keeps Stage 17.7 a MANUAL workflow. A human judgement about one
// plan must not become standing unattended authority, so no part of the
// automation decision path may read an approval.
//
// `automation_query.go` is allowed to name `PlanApprovable` -- it reads the
// PLAN's recommendation to refuse a doomed release -- but must never look up an
// approval.
func TestAutomationDoesNotConsumePlanApprovals(t *testing.T) {
	for _, file := range []string{
		"automation_decide.go",
		"automation.go",
		"automation_query.go",
		"automation_readiness.go",
		"automation_dependency.go",
		"automation_follow.go",
	} {
		source := readSource(t, "internal", "service", file)
		for _, symbol := range []string{
			"PlanApprovalService", "PlanApprovals", "ApprovalFor(",
			"PlanApprovalValid", "plan_approvals",
		} {
			if strings.Contains(source, symbol) {
				t.Errorf("internal/service/%s names %s\n"+
					"\tautomation must not consume a plan approval: one human judgement "+
					"about one plan may never become standing unattended authority",
					file, symbol)
			}
		}
	}
}

// TestOnlyTheExecutionPreflightConsumesAnApproval pins where the evidence is
// used.
//
// Exactly one place turns an approval into permission, and it is the preflight
// that runs immediately before anything is stopped. A second consumer would be
// a second authorisation model.
func TestOnlyTheExecutionPreflightConsumesAnApproval(t *testing.T) {
	root := filepath.Join(moduleRoot(t), "internal", "service")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read internal/service: %v", err)
	}

	var consumers []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		source := readSource(t, "internal", "service", name)
		if strings.Contains(source, "ApprovalFor(") {
			consumers = append(consumers, name)
		}
	}

	// plan_approval.go DEFINES it; execution.go declares the interface;
	// execution_preflight.go is the only place that CALLS it for a decision.
	expected := map[string]bool{
		"plan_approval.go":       true,
		"execution.go":           true,
		"execution_preflight.go": true,
	}
	for _, name := range consumers {
		if !expected[name] {
			t.Errorf("internal/service/%s consumes a plan approval\n"+
				"\tonly the execution preflight may turn an approval into permission",
				name)
		}
	}
	if !contains(consumers, "execution_preflight.go") {
		t.Error("the execution preflight no longer consumes an approval; " +
			"this guard would pass while the feature was gone")
	}
}

// TestTheApprovalHandlerAcceptsNoEvidence is the API half.
//
// The request carries a plan identifier in the path and nothing else. A body
// field naming an image, a digest or a container would let a caller approve a
// change other than the one they were shown.
func TestTheApprovalHandlerAcceptsNoEvidence(t *testing.T) {
	source := readSource(t, "internal", "api", "plan_approval_handlers.go")

	for _, symbol := range []string{
		"decodeJSONBody(", "json.NewDecoder", "r.Body",
	} {
		if strings.Contains(source, symbol) {
			t.Errorf("internal/api/plan_approval_handlers.go names %s\n"+
				"\tthe approval endpoints take NO request body: every fact about the "+
				"change is read from the plan the URL names", symbol)
		}
	}
	if strings.Contains(source, "internal/docker") {
		t.Error("the approval handler imports internal/docker")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
