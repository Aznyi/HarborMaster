package service_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Stage 17.2 reproduction, at the service layer.
//
// The store-level half of this reproduction lives in
// internal/store/plan_snapshot_evidence_test.go and establishes that a snapshot
// captured after a plan does not enter that plan. This half establishes the
// consequence: the acquisition preflight refuses, and -- the part that matters
// -- it refuses BEFORE anything is pulled.
//
// Together they answer the question Stage 17.2 turns on. Automatic snapshot
// assurance cannot be implemented by capturing a snapshot at acquisition time,
// because the plan being acquired was assessed against a world without one and
// no later capture can retroactively become part of that assessment.

// TestCaseA_AcquisitionStillRefusesAfterALaterSnapshot.
//
// The plan carries no snapshot evidence. A snapshot existing in the world does
// not change that, so the refusal stands -- and it is the RIGHT refusal, not an
// accident of some other check.
func TestCaseA_AcquisitionStillRefusesAfterALaterSnapshot(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		// The plan as the planner would have written it for a container with no
		// baseline: no snapshot id, no availability, readiness unknown.
		plan := h.evidence.plan
		plan.SnapshotID = 0
		plan.SnapshotAvailable = false
		plan.RestoreReadiness = domain.ReadinessUnknown
		h.evidence.plan = plan
		h.evidence.current = plan
	})

	_, err := harness.service.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("an acquisition for a plan with no snapshot evidence was accepted")
	}
	if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalRestoreReadiness {
		t.Errorf("refusal = %q, want %q", refusal, domain.AcquisitionRefusalRestoreReadiness)
	}

	// The property that makes this fail-closed rather than merely correct.
	// Counted on the PRODUCTION fake, which performs the same target validation
	// the real client does.
	if harness.acquirer.Calls != 0 {
		t.Errorf("%d pull attempts, want 0; a refused acquisition reached the daemon", harness.acquirer.Calls)
	}
	if len(harness.acquirer.Targets) != 0 {
		t.Errorf("%d pull targets recorded, want none", len(harness.acquirer.Targets))
	}
}

// TestCaseA_APlanCarryingSnapshotEvidenceIsAccepted is the control.
//
// Without it the test above would pass just as well if acquisition refused
// everything, which is the failure mode a negative test cannot see on its own.
func TestCaseA_APlanCarryingSnapshotEvidenceIsAccepted(t *testing.T) {
	harness := newAcquisitionHarness(t)

	if harness.evidence.plan.SnapshotAvailable != true {
		t.Fatal("fixture defect: the control must start with snapshot evidence")
	}

	if _, err := harness.service.Request(
		t.Context(), service.AcquisitionRequest{PlanID: acqPlanID}); err != nil {
		t.Fatalf("an acquisition with complete evidence was refused: %v", err)
	}
}

// TestCaseA_TheRefusalIsDrivenByThePlanNotByTheWorld.
//
// The sharpest form of the invariant available at this layer: the acquisition
// preflight's snapshot gate reads the PLAN, and there is no input to it that
// describes the live baseline. A caller cannot supply one, and neither can a
// capture that happened after the plan was written.
//
// Expressed as a behavioural test rather than a comment: two requests differing
// only in what the plan records get different answers, while nothing about the
// surrounding world has moved at all.
func TestCaseA_TheRefusalIsDrivenByThePlanNotByTheWorld(t *testing.T) {
	withEvidence := newAcquisitionHarness(t)
	if _, err := withEvidence.service.Request(
		t.Context(), service.AcquisitionRequest{PlanID: acqPlanID}); err != nil {
		t.Fatalf("plan with evidence was refused: %v", err)
	}

	withoutEvidence := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		plan := h.evidence.plan
		plan.SnapshotAvailable = false
		plan.RestoreReadiness = domain.ReadinessUnknown
		h.evidence.plan = plan
		h.evidence.current = plan
	})
	_, err := withoutEvidence.service.Request(
		t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("plan without evidence was accepted")
	}
	if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalRestoreReadiness {
		t.Errorf("refusal = %q, want %q", refusal, domain.AcquisitionRefusalRestoreReadiness)
	}
}

// TestCaseA_ANotReadyBaselineOnThePlanRefusesToo.
//
// The plan can carry a snapshot and still be unusable. Assurance must treat
// "not ready" the same way the existing gate does, so this pins the behaviour
// the assurance service has to preserve rather than re-derive.
func TestCaseA_ANotReadyBaselineOnThePlanRefusesToo(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		plan := h.evidence.plan
		plan.SnapshotAvailable = true
		plan.RestoreReadiness = domain.ReadinessNotReady
		h.evidence.plan = plan
		h.evidence.current = plan
	})

	_, err := harness.service.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("an acquisition on a not-ready baseline was accepted")
	}
	if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalRestoreReadiness {
		t.Errorf("refusal = %q, want %q", refusal, domain.AcquisitionRefusalRestoreReadiness)
	}
	if harness.acquirer.Calls != 0 {
		t.Errorf("%d pull attempts, want 0", harness.acquirer.Calls)
	}
}
