package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Self-update refusal, exercised through the engine rather than asserted about
// the function that implements it.
//
// The architecture tests prove the three refusals EXIST. These prove they
// WORK — that a container HarborMaster believes is itself reaches no service,
// under every signal, and whatever else the policy says.

// selfReporter is a fixed identity.
type selfReporter struct{ identity domain.SelfIdentity }

func (s selfReporter) Identity() domain.SelfIdentity { return s.identity }

// harborMasterIdentity is a fully-resolved identity for the fixture container.
func harborMasterIdentity() domain.SelfIdentity {
	return domain.SelfIdentity{
		ContainerID:   "container-web",
		ContainerName: "web",
		ImageRef:      "nginx:1.27.3",
		Source:        domain.SelfSourceRuntime,
		Detail:        "read from this process's own control group",
	}
}

func TestAPassNeverSubmitsAnUpdateForHarborMasterItself(t *testing.T) {
	// The fixture container `web` is the one the automatic policy selects and
	// the one with an eligible plan. Making it HarborMaster must turn a
	// certain update into a certain refusal.
	harness := newAutomationHarness(t)
	harness.engine = service.NewAutomationService(harness.optionsWithSelf(harborMasterIdentity()))

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 0 {
		t.Fatalf("submitted = %d; HarborMaster must never submit an update for itself", run.Submitted)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("a self-update reached the acquisition service")
	}

	if len(decisions) == 0 {
		t.Fatal("the pass recorded no decisions")
	}
	if decisions[0].Reason != domain.ReasonSelfUpdate {
		t.Fatalf("reason = %q, want selfUpdate", decisions[0].Reason)
	}
	// The refusal has to tell an operator what to do instead, or it reads as a
	// bug rather than as a deliberate limit.
	if !strings.Contains(decisions[0].Detail, "docker compose") {
		t.Fatalf("the refusal must say how to update HarborMaster: %q", decisions[0].Detail)
	}
}

func TestSelfUpdateIsRefusedBeforeEveryOtherCheck(t *testing.T) {
	// Check 0. A container that is HarborMaster is refused for THAT reason,
	// not for whichever other check happens to fire first — otherwise the
	// record would send an operator to fix a window or a policy that is not
	// the reason at all.
	cases := map[string]func(*service.AutomationInput){
		"even when paused": func(in *service.AutomationInput) {
			in.IsPaused = true
			in.Pause = domain.PausedContainer{
				ContainerName: "web",
				Reason:        domain.PauseRolledBack,
				PausedAt:      decideAt,
			}
		},
		"even when opted out by label": func(in *service.AutomationInput) {
			in.Target.Labels = map[string]string{domain.LabelUpdateEnabled: "false"}
		},
		"even when no policy selects it": func(in *service.AutomationInput) {
			in.Policies = nil
		},
		"even when there is no plan": func(in *service.AutomationInput) {
			in.HasPlan = false
			in.Plan = domain.ChangePlan{}
		},
		"even when work is in flight": func(in *service.AutomationInput) {
			in.InFlight = true
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := eligibleInput()
			input.Self = harborMasterIdentity()
			mutate(&input)

			outcome := service.DecideAutomation(input)
			if outcome.Eligible() {
				t.Fatal("HarborMaster must never be eligible")
			}
			if outcome.Decision.Reason != domain.ReasonSelfUpdate {
				t.Fatalf("reason = %q, want selfUpdate -- the record must name the "+
					"reason an operator cannot fix", outcome.Decision.Reason)
			}
		})
	}
}

func TestEachSelfSignalAloneStopsAPass(t *testing.T) {
	// One probe working is enough. These are the states a real deployment
	// reaches when the others fail.
	cases := map[string]domain.SelfIdentity{
		"only the container id was established": {
			ContainerID: "container-web",
			Source:      domain.SelfSourceRuntime,
		},
		"only the image was established": {
			ImageRef: "nginx:1.27.3",
			Source:   domain.SelfSourceNone,
		},
		"only the name was established": {
			ContainerName: "web",
			Source:        domain.SelfSourceHostname,
		},
	}

	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			harness := newAutomationHarness(t)
			harness.engine = service.NewAutomationService(harness.optionsWithSelf(identity))

			run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
			if err != nil {
				t.Fatalf("RunNow: %v", err)
			}
			if run.Submitted != 0 {
				t.Fatal("one signal on its own must be enough to refuse")
			}
			if len(harness.pipeline.recorded("acquire")) != 0 {
				t.Fatal("a self-update reached the acquisition service")
			}
		})
	}
}

func TestTheLabelSignalStopsAPassWithNoIdentityAtAll(t *testing.T) {
	// Every probe failed, and the container says what it is. This is the case
	// the label exists for.
	harness := newAutomationHarness(t)
	harness.evidence.targets[0].Selection.Labels = map[string]string{
		domain.LabelSelfIdentity: "true",
	}
	harness.engine = service.NewAutomationService(
		harness.optionsWithSelf(domain.SelfIdentity{Source: domain.SelfSourceNone}))

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 0 {
		t.Fatal("a self-identifying label must stop the update")
	}
	if decisions[0].Reason != domain.ReasonSelfUpdate {
		t.Fatalf("reason = %q, want selfUpdate", decisions[0].Reason)
	}
}

func TestApprovingASelfUpdateIsRefused(t *testing.T) {
	// The approval path is a second caller of the acquisition service. A
	// decision that predates detection must not become a way in.
	policy := automaticPolicy()
	policy.Mode = domain.ModeApprove
	harness := newAutomationHarness(t, policy)

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	// Detection catches up after the decision was recorded.
	harness.engine = service.NewAutomationService(harness.optionsWithSelf(harborMasterIdentity()))

	_, err = harness.engine.Approve(context.Background(), run.RunID, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, service.Actor{})
	if err == nil {
		t.Fatal("approving an update for HarborMaster itself must be refused")
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("an approved self-update reached the acquisition service")
	}
}

func TestAnUnknownIdentityDoesNotStopTheEstate(t *testing.T) {
	// The fail-safe direction, end to end: a deployment where every probe
	// failed still updates everything else. A protection that broke the
	// feature would be turned off, and then it would protect nothing.
	harness := newAutomationHarness(t)
	harness.engine = service.NewAutomationService(
		harness.optionsWithSelf(domain.SelfIdentity{
			Source: domain.SelfSourceNone,
			Detail: "could not determine which container this is",
		}))

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1 -- an unknown identity must exclude nothing",
			run.Submitted)
	}
}
