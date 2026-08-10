package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Stale plans, once image lineage can change underneath one.
//
// # Why this needs its own file now
//
// Before lineage, a plan went stale for reasons an operator could see: a newer
// assessment, a container somebody edited. Lineage adds a quieter one. The
// authoritative record of what a container is RUNNING now lives in the lineage
// table, and reconciliation rewrites it whenever the host disagrees -- so a plan
// approved against "this container runs A" can be left describing a starting
// point that no longer exists, without the container's own reference changing at
// all.
//
// Nothing in this file adds a lineage-specific gate. The refusals asserted here
// are the ones the execution preflight already performs; the point is to prove
// they still catch this, because a plan that survives a lineage change would
// recreate a container from a digest nobody re-assessed.

// TestAPlanSupersededByALineageDrivenReassessmentIsRefused.
//
// Reconciliation adopts what the host is running, the planner re-assesses from
// the corrected starting point and writes a new plan, and the old approval must
// no longer be executable.
func TestAPlanSupersededByALineageDrivenReassessmentIsRefused(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		// The plan the acquisition was approved against still exists...
		superseding := h.evidence.plan
		// ...but a later pass produced a different assessment for the same
		// container, which is what a corrected running digest causes.
		superseding.PlanID = "plan_ffeeddccbbaa99887766"
		superseding.ProposedDigest = "sha256:" + strings.Repeat("d", 64)
		h.evidence.current = superseding
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("a superseded plan was executed; the container would have been " +
			"recreated from a digest the newest assessment does not propose")
	}

	if refusal := executionRefusalFrom(t, err); refusal != domain.ExecutionRefusalPlanSuperseded {
		t.Fatalf("refusal = %q, want %q", refusal, domain.ExecutionRefusalPlanSuperseded)
	}

	// Nothing on the host, and nothing acquired, was touched.
	assertNoMutationHappened(t, harness)
}

// TestAPlanIsRefusedOnceTheContainerNoLongerRunsWhatItAssessed.
//
// The host-side half of the same problem: something moved the container while
// the approval was outstanding. The preflight compares the container's CURRENT
// image against the plan's `CurrentImage`, and refuses rather than reconciling.
//
// This is the case reconciliation then resolves -- it re-establishes lineage
// from what is actually running -- and until it does, the container is
// deliberately not planned from. Refusing here is what makes that safe.
func TestAPlanIsRefusedOnceTheContainerNoLongerRunsWhatItAssessed(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		moved := execContainerDetail(execContainerID)
		// Somebody moved it onto a different artefact behind HarborMaster's back.
		moved.Overview.Image = domain.ParseImageRef("nginx:1.27.2")
		moved.RunningDigest = "sha256:" + strings.Repeat("e", 64)
		h.evidence.container = &moved
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("a plan was executed against a container that is no longer running " +
			"the image the plan assessed")
	}

	if refusal := executionRefusalFrom(t, err); refusal != domain.ExecutionRefusalContainerChanged {
		t.Fatalf("refusal = %q, want %q", refusal, domain.ExecutionRefusalContainerChanged)
	}

	assertNoMutationHappened(t, harness)
}

// TestARefusedStalePlanNeverAdvancesLineage.
//
// The property that ties this file to the lineage work: a refusal must leave
// lineage exactly as it was. Lineage advances only from a VERIFIED recreation,
// so a plan that never ran must not move the digest HarborMaster believes is
// executing -- doing so would make the next pass compare against something that
// was never installed.
func TestARefusedStalePlanNeverAdvancesLineage(t *testing.T) {
	const tracked = "docker.io/library/nginx:1.27.0"
	before := domain.ImageLineage{
		ContainerName:     "web",
		State:             domain.LineageTracked,
		Origin:            domain.LineageObserved,
		TrackingReference: tracked,
		TrackingFamiliar:  "nginx:1.27.0",
		Repository:        "library/nginx",
		RunningDigest:     "sha256:" + strings.Repeat("a", 64),
	}

	lineage := newFakeLineageStore()
	lineage.rows["web"] = before

	harness := newExecHarness(t, func(h *execHarness) {
		h.lineage = lineage
		superseding := h.evidence.plan
		superseding.PlanID = "plan_ffeeddccbbaa99887766"
		h.evidence.current = superseding
	})

	if _, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID}); err == nil {
		t.Fatal("the superseded plan was not refused")
	}

	after, err := lineage.Get(context.Background(), "web")
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	if after != before {
		t.Fatalf("lineage moved on a refused recreation:\n\tbefore %+v\n\tafter  %+v\n"+
			"\tlineage may only advance from a recreation that was verified",
			before, after)
	}
}

// assertNoMutationHappened proves a refusal changed nothing on the host.
func assertNoMutationHappened(t *testing.T, harness *execHarness) {
	t.Helper()

	if operations := harness.mutator.Ops(); len(operations) != 0 {
		t.Errorf("a refused recreation performed %d Docker mutation(s): %v",
			len(operations), operations)
	}
}
