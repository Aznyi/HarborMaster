package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Recovery plans for a rollback that had no replacement to move.
//
// A recreation that failed at the create or at the park rename leaves one
// container: the original, stopped, under either its parked name or its own.
// A plan that talks about "the replacement" in that situation sends an
// operator looking for a container that does not exist.

const (
	rbPlanOriginalID = "1111111111111111111111111111111111111111111111111111111111111111"
	rbPlanName       = "web"
	rbPlanParked     = "web.hm-old-exec_00112233445566778899"
)

func noReplacementContext(checkpoint domain.RollbackCheckpoint, holdsName bool) domain.RollbackRecoveryContext {
	return domain.RollbackRecoveryContext{
		RollbackID:        "rb_00112233445566778899",
		ExecutionID:       "exec_00112233445566778899",
		ContainerName:     rbPlanName,
		OriginalID:        rbPlanOriginalID,
		ParkedName:        rbPlanParked,
		Checkpoint:        checkpoint,
		OriginalHoldsName: holdsName,
	}
}

func planText(plan *domain.RecoveryPlan) string {
	var text strings.Builder
	text.WriteString(plan.Situation)
	for _, step := range plan.Steps {
		text.WriteString(" ")
		text.WriteString(step.Description)
		text.WriteString(" ")
		text.WriteString(step.Command)
	}
	return strings.ToLower(text.String())
}

func TestNoReplacementRollbackPlansDescribeOnlyTheOriginal(t *testing.T) {
	cases := []struct {
		name        string
		context     domain.RollbackRecoveryContext
		interrupted bool
		wantCommand []string
	}{
		{
			name:        "untouched, original parked",
			context:     noReplacementContext(domain.RollbackCheckpointNone, false),
			interrupted: true,
			wantCommand: []string{
				"docker rename " + rbPlanParked + " " + rbPlanName,
				"docker start " + rbPlanName,
			},
		},
		{
			name:        "untouched, original holds its own name",
			context:     noReplacementContext(domain.RollbackCheckpointNone, true),
			interrupted: true,
			wantCommand: []string{"docker start " + rbPlanName},
		},
		{
			name:        "name restored, not started",
			context:     noReplacementContext(domain.RollbackCheckpointOriginalRestored, false),
			interrupted: true,
			wantCommand: []string{"docker start " + rbPlanOriginalID[:12]},
		},
		{
			name:        "started, verification failed",
			context:     noReplacementContext(domain.RollbackCheckpointOriginalStarted, false),
			interrupted: false,
			wantCommand: []string{"docker logs --tail 100 " + rbPlanOriginalID[:12]},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := domain.BuildRollbackRecoveryPlan(testCase.context)
			if plan == nil {
				t.Fatal("no plan was built")
			}
			if plan.ServiceInterrupted != testCase.interrupted {
				t.Errorf("serviceInterrupted = %v, want %v", plan.ServiceInterrupted, testCase.interrupted)
			}

			text := planText(plan)
			// The situation may SAY that no replacement was created; no step may
			// send the operator to one.
			for _, step := range plan.Steps {
				if strings.Contains(strings.ToLower(step.Description+" "+step.Command), "replacement") {
					t.Errorf("a step talks about a replacement that never existed: %+v", step)
				}
			}
			if strings.Contains(strings.ToLower(plan.Situation), "the replacement") {
				t.Errorf("the situation describes a replacement that never existed:\n%s", plan.Situation)
			}
			for _, want := range testCase.wantCommand {
				if !strings.Contains(text, strings.ToLower(want)) {
					t.Errorf("the plan does not offer %q:\n%s", want, text)
				}
			}
			for _, step := range plan.Steps {
				if step.Destructive {
					t.Errorf("the plan recommends a destructive step with only one container on the host: %+v", step)
				}
			}
		})
	}
}

// TestTheParkedRecreationPlanSaysNoReplacementWasRecorded: a create that failed
// on the client may have landed on the daemon. "No replacement was created" is
// a claim HarborMaster cannot make; "none was recorded" is the truth, and the
// plan has the operator look.
func TestTheParkedRecreationPlanSaysNoReplacementWasRecorded(t *testing.T) {
	plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:       "exec_00112233445566778899",
		ContainerName:     rbPlanName,
		OriginalID:        rbPlanOriginalID,
		ParkedName:        rbPlanParked,
		Checkpoint:        domain.CheckpointOriginalParked,
		MutationAttempted: true,
	})
	situation := strings.ToLower(plan.Situation)
	if strings.Contains(situation, "was created") {
		t.Errorf("the plan asserts nothing was created, which it cannot know:\n%s", plan.Situation)
	}
	if !strings.Contains(situation, "recorded") {
		t.Errorf("the plan does not say no replacement was recorded:\n%s", plan.Situation)
	}
	var looks bool
	for _, step := range plan.Steps {
		if strings.Contains(step.Command, "docker ps -a") && strings.Contains(step.Command, rbPlanName) {
			looks = true
		}
	}
	if !looks {
		t.Errorf("the plan never has the operator check what holds the production name: %+v", plan.Steps)
	}
}

// TestASucceededRollbackWhoseParkedReplacementWasNotNeutralisedGetsAPlan: the
// original is serving, so the rollback is a success -- but the replacement it
// parked still carries a restart policy that could bring it back beside the
// original after a daemon restart. The plan says so, at attention rather than
// urgent, and names the exact command.
func TestASucceededRollbackWhoseParkedReplacementWasNotNeutralisedGetsAPlan(t *testing.T) {
	plan := domain.BuildRollbackUnsecuredReplacementPlan(domain.RollbackRecoveryContext{
		RollbackID:            "rb_00112233445566778899",
		ExecutionID:           "exec_00112233445566778899",
		ContainerName:         rbPlanName,
		OriginalID:            rbPlanOriginalID,
		ReplacementID:         "2222222222222222222222222222222222222222222222222222222222222222",
		ReplacementParkedName: "web.hm-rolledback-rb_00112233445566778899",
		OriginalRestartPolicy: "always",
		Checkpoint:            domain.RollbackCheckpointOriginalVerified,
	})
	if plan == nil {
		t.Fatal("no plan")
	}
	if plan.ServiceInterrupted {
		t.Error("the plan says the service is down; the original is serving")
	}
	if plan.Urgency != domain.RecoveryAttention {
		t.Errorf("urgency %q, want attention", plan.Urgency)
	}
	text := planText(plan)
	if !strings.Contains(text, "docker update --restart=no web.hm-rolledback-rb_00112233445566778899") {
		t.Errorf("the plan does not name the neutralising command:\n%s", text)
	}
	for _, step := range plan.Steps {
		if step.Destructive {
			t.Errorf("the plan recommends removing evidence: %+v", step)
		}
	}
}
