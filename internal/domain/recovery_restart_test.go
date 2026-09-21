package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Recovery plans must never describe a container as safely parked when the
// restart policy that could bring it back was not neutralised.

const (
	rsOriginalID    = "1111111111111111111111111111111111111111111111111111111111111111"
	rsReplacementID = "2222222222222222222222222222222222222222222222222222222222222222"
	rsExecutionID   = "exec_00112233445566778899"
)

func rsContext() domain.RecoveryContext {
	return domain.RecoveryContext{
		ExecutionID:           rsExecutionID,
		ContainerName:         "web",
		OriginalID:            rsOriginalID,
		ParkedName:            "web.hm-old-" + rsExecutionID,
		Checkpoint:            domain.CheckpointOriginalParked,
		MutationAttempted:     true,
		OriginalRestartPolicy: "always",
	}
}

func commands(plan *domain.RecoveryPlan) string {
	var out []string
	for _, step := range plan.Steps {
		out = append(out, step.Description+" "+step.Command)
	}
	return strings.ToLower(strings.Join(out, "\n"))
}

func TestAParkedOriginalWhoseSuspensionFailedIsNotDescribedAsSafe(t *testing.T) {
	context := rsContext()
	context.Failure = domain.ExecutionFailureRestartPolicy

	plan := domain.BuildRecoveryPlan(context)
	if plan == nil || !plan.ServiceInterrupted || plan.Urgency != domain.RecoveryUrgent {
		t.Fatalf("plan = %+v, want urgent and interrupted", plan)
	}
	text := commands(plan)
	if !strings.Contains(text, "docker update --restart=no "+strings.ToLower(context.ParkedName)) {
		t.Errorf("the plan does not tell the operator how to keep the parked original from restarting:\n%s", text)
	}
	if !strings.Contains(strings.ToLower(plan.Situation), "restart") {
		t.Errorf("the situation does not mention the restart hazard: %s", plan.Situation)
	}
}

func TestAParkedOriginalWhoseSuspensionSucceededCarriesNoRestartStep(t *testing.T) {
	context := rsContext()
	context.Failure = domain.ExecutionFailureCreate

	plan := domain.BuildRecoveryPlan(context)
	if strings.Contains(commands(plan), "--restart=no") {
		t.Errorf("a plan for a neutralised original tells the operator to neutralise it again:\n%s", commands(plan))
	}
}

// TestAQuarantinedReplacementWhoseSuspensionFailedIsNotDescribedAsSafe: the
// replacement was renamed to its quarantine name but the checkpoint never
// reached replacementQuarantined, which is the durable sign that the restart
// policy was not neutralised.
func TestAQuarantinedReplacementWhoseSuspensionFailedIsNotDescribedAsSafe(t *testing.T) {
	context := rsContext()
	context.Checkpoint = domain.CheckpointReplacementStarted
	context.ReplacementID = rsReplacementID
	context.QuarantineName = "web.hm-failed-" + rsExecutionID
	context.Failure = domain.ExecutionFailureUnhealthy

	plan := domain.BuildRecoveryPlan(context)
	text := commands(plan)
	if !strings.Contains(text, "docker update --restart=no "+strings.ToLower(context.QuarantineName)) {
		t.Errorf("the plan does not tell the operator to keep the quarantined replacement from restarting:\n%s", text)
	}

	// Once the checkpoint says quarantined, the policy was neutralised and the
	// step disappears.
	context.Checkpoint = domain.CheckpointReplacementQuarantined
	if strings.Contains(commands(domain.BuildRecoveryPlan(context)), "--restart=no") {
		t.Error("a neutralised quarantine still carries the restart step")
	}
}

func TestARestoredExecutionPlanSaysTheServiceIsBack(t *testing.T) {
	plan := domain.RestoredRecoveryPlan("web", "rbk_0123456789abcdef0123", rsReplacementID,
		"web.hm-rolledback-rbk_0123456789abcdef0123")
	if plan.ServiceInterrupted {
		t.Error("a restored plan claims the service is interrupted")
	}
	if plan.Urgency == domain.RecoveryUrgent {
		t.Error("a restored plan is urgent")
	}
	if !strings.Contains(strings.ToLower(plan.Situation), "restored") {
		t.Errorf("the situation does not say the original was restored: %s", plan.Situation)
	}
	for _, step := range plan.Steps {
		if step.Destructive {
			t.Errorf("a restored plan recommends a destructive step by default: %+v", step)
		}
	}
}
