package service_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Snapshot assurance at the mutation point.
//
// # What these tests are about
//
// The plan being executed records the configuration snapshot it was assessed
// against. Assurance runs in the preflight and establishes which snapshot
// describes the container NOW. If those two are not the same snapshot, the
// plan's risk assessment was computed against a container that no longer exists
// in that form, and the recreation must not proceed.
//
// The assertion in every case below is the MUTATING CALL LIST on the production
// fake, not the error. A refusal that arrives after the stop is not a refusal:
// the original is already down and its dependents are already broken.

// ------------------------------------------------------------ capturer --

// scriptedCapturer returns a snapshot a test chose, so the preflight can be
// driven through every assurance outcome without a database.
//
// It is deliberately NOT more permissive than production: it returns exactly
// what store.SnapshotRepository.Create returns -- a row with an id, and
// Deduplicated set when the configuration was unchanged.
type scriptedCapturer struct {
	mu sync.Mutex

	snapshot domain.Snapshot
	err      error
	enabled  bool
	calls    int
}

func (c *scriptedCapturer) Capture(context.Context, service.CaptureRequest) (domain.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return domain.Snapshot{}, c.err
	}
	return c.snapshot, nil
}

func (c *scriptedCapturer) Enabled() bool { return c.enabled }

func (c *scriptedCapturer) captureCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// withAssurance wires an assurance whose capture returns the given snapshot.
func withAssurance(capturer *scriptedCapturer) func(*execHarness) {
	return func(h *execHarness) {
		h.assurance = service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
			Capturer: capturer,
		})
	}
}

// unchangedCapturer reports the baseline the healthy fixture's plan names:
// snapshot 7, deduplicated, ready. The world in which nothing has moved.
func unchangedCapturer() *scriptedCapturer {
	return &scriptedCapturer{
		enabled: true,
		snapshot: domain.Snapshot{
			ID:              7,
			ContainerID:     execContainerID,
			ReadinessStatus: domain.ReadinessReady,
			Deduplicated:    true,
		},
	}
}

// assertNoMutation is the assertion that matters. Nothing on the host moved.
func assertNoMutation(t *testing.T, harness *execHarness) {
	t.Helper()
	if calls := harness.mutator.Ops(); len(calls) != 0 {
		t.Errorf("the host was mutated by a refused recreation: %v", calls)
	}
}

// ------------------------------------------------------------ the cases --

// TestAssuranceOnAnUnchangedContainerLetsTheRecreationProceed is the control.
//
// Without it every test below would pass just as well if assurance refused
// everything, which is the failure mode a set of negative tests cannot see on
// its own.
func TestAssuranceOnAnUnchangedContainerLetsTheRecreationProceed(t *testing.T) {
	capturer := unchangedCapturer()
	harness := newExecHarness(t, withAssurance(capturer))

	if _, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID}); err != nil {
		t.Fatalf("a recreation whose baseline is unchanged was refused: %v", err)
	}
	if capturer.captureCount() == 0 {
		t.Error("assurance never ran; this test would pass vacuously")
	}
}

// TestAConfigurationChangeAfterThePlanRefusesTheRecreation is the heart of
// Stage 17.2.
//
// The container was reconfigured after its plan was written, so assurance
// captures a NEW snapshot. That snapshot is kept -- it is the evidence the next
// planner pass will build from -- but it does not authorise the plan already in
// flight.
func TestAConfigurationChangeAfterThePlanRefusesTheRecreation(t *testing.T) {
	capturer := &scriptedCapturer{
		enabled: true,
		snapshot: domain.Snapshot{
			// A NEW row. The plan names snapshot 7.
			ID:              8,
			ContainerID:     execContainerID,
			ReadinessStatus: domain.ReadinessUnknown,
			Deduplicated:    false,
		},
	}
	harness := newExecHarness(t, withAssurance(capturer))

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("a recreation proceeded on a plan whose baseline had moved")
	}
	if refusal := executionRefusalFrom(t, err); refusal != domain.ExecutionRefusalSnapshotChanged {
		t.Errorf("refusal = %q, want %q", refusal, domain.ExecutionRefusalSnapshotChanged)
	}
	assertNoMutation(t, harness)
}

// TestANewBaselineDoesNotBlessAPlanThatHadNone.
//
// The first-update shape: the plan was written before the container had any
// snapshot, so it records SnapshotID 0. Assurance captures one. That must NOT
// make the plan executable -- the plan's risk assessment was computed against
// "no baseline exists", and a baseline existing now does not change what was
// assessed.
func TestANewBaselineDoesNotBlessAPlanThatHadNone(t *testing.T) {
	capturer := &scriptedCapturer{
		enabled: true,
		snapshot: domain.Snapshot{
			ID:              12,
			ContainerID:     execContainerID,
			ReadinessStatus: domain.ReadinessUnknown,
			Deduplicated:    false,
		},
	}
	harness := newExecHarness(t, withAssurance(capturer), func(h *execHarness) {
		plan := h.evidence.plan
		plan.SnapshotID = 0
		plan.SnapshotAvailable = false
		plan.RestoreReadiness = domain.ReadinessUnknown
		h.evidence.plan = plan
		h.evidence.current = plan
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("a snapshot captured after the plan made that plan executable")
	}
	if refusal := executionRefusalFrom(t, err); refusal != domain.ExecutionRefusalSnapshotChanged {
		t.Errorf("refusal = %q, want %q", refusal, domain.ExecutionRefusalSnapshotChanged)
	}
	assertNoMutation(t, harness)
}

// TestAssuranceFailureCausesZeroHostMutation.
//
// The strict reading of the acceptance requirement: if the baseline cannot be
// established, nothing on the host moves. Asserted on the production fake's
// call list, so a refusal that happened after a stop would be visible.
func TestAssuranceFailureCausesZeroHostMutation(t *testing.T) {
	capturer := &scriptedCapturer{enabled: true, err: service.ErrCaptureInProgress}
	harness := newExecHarness(t, withAssurance(capturer))

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("a recreation proceeded with no established baseline")
	}
	if refusal := executionRefusalFrom(t, err); refusal != domain.ExecutionRefusalSnapshotMissing {
		t.Errorf("refusal = %q, want %q", refusal, domain.ExecutionRefusalSnapshotMissing)
	}
	assertNoMutation(t, harness)
}

// TestAFailedReadinessCheckRefusesBeforeAnyMutation.
func TestAFailedReadinessCheckRefusesBeforeAnyMutation(t *testing.T) {
	capturer := &scriptedCapturer{
		enabled: true,
		snapshot: domain.Snapshot{
			ID:              7,
			ContainerID:     execContainerID,
			ReadinessStatus: domain.ReadinessNotReady,
			Deduplicated:    true,
		},
	}
	harness := newExecHarness(t, withAssurance(capturer))

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("a recreation proceeded on a baseline that failed its readiness check")
	}
	if refusal := executionRefusalFrom(t, err); refusal != domain.ExecutionRefusalRestoreReadiness {
		t.Errorf("refusal = %q, want %q", refusal, domain.ExecutionRefusalRestoreReadiness)
	}
	assertNoMutation(t, harness)
}

// ------------------------------------------------- the configuration gates --

// TestTheSnapshotGateMatrix walks the three settings that decide whether
// snapshot evidence is required, and pins what each combination does.
//
// # The combination that matters most is the last one
//
// An operator who set EXECUTION_REQUIRE_SNAPSHOT=false has said they will
// recreate without snapshot evidence. On such a deployment a plan legitimately
// carries SnapshotID 0 while assurance produces a real id, so refusing on that
// difference would break a working configuration. The staleness check is
// therefore gated with the rest of the snapshot policy, and this is the test
// that says so rather than the comment.
func TestTheSnapshotGateMatrix(t *testing.T) {
	newSnapshot := domain.Snapshot{
		ID: 8, ContainerID: execContainerID,
		ReadinessStatus: domain.ReadinessUnknown, Deduplicated: false,
	}

	cases := []struct {
		name string
		// snapshotsEnabled is SNAPSHOTS_ENABLED, expressed as whether the
		// capturer reports itself available.
		snapshotsEnabled bool
		requireSnapshot  bool
		wantRefusal      domain.ExecutionRefusal
		why              string
	}{
		{
			name:             "snapshots on, gate on",
			snapshotsEnabled: true,
			requireSnapshot:  true,
			wantRefusal:      domain.ExecutionRefusalSnapshotChanged,
			why:              "the baseline moved and the deployment asked to be stopped for that",
		},
		{
			name:             "snapshots on, gate off",
			snapshotsEnabled: true,
			requireSnapshot:  false,
			wantRefusal:      domain.ExecutionRefusalNone,
			why: "the operator switched the snapshot gate off; assurance still " +
				"captures the evidence but does not block on it",
		},
		{
			name:             "snapshots off, gate off",
			snapshotsEnabled: false,
			requireSnapshot:  false,
			wantRefusal:      domain.ExecutionRefusalNone,
			why:              "both switched off explicitly; nothing may resurrect the requirement",
		},
		{
			name:             "snapshots off, gate on",
			snapshotsEnabled: false,
			requireSnapshot:  true,
			wantRefusal:      domain.ExecutionRefusalSnapshotMissing,
			why: "a configuration conflict: evidence is required and cannot be " +
				"produced, so it fails closed",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			capturer := &scriptedCapturer{
				enabled:  testCase.snapshotsEnabled,
				snapshot: newSnapshot,
			}
			harness := newExecHarness(t, withAssurance(capturer), func(h *execHarness) {
				if !testCase.requireSnapshot {
					no := false
					h.requireSnapshot = &no
				}
				if !testCase.snapshotsEnabled {
					// With snapshots off there is no baseline to read either, so
					// the existing lookup finds nothing. Modelling only half of
					// that would make the case unrepresentative.
					h.evidence.baselineErr = store.ErrNotFound
				}
			})

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})

			if testCase.wantRefusal == domain.ExecutionRefusalNone {
				if err != nil {
					t.Fatalf("%s: refused with %v; %s", testCase.name, err, testCase.why)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: proceeded; %s", testCase.name, testCase.why)
			}
			if refusal := executionRefusalFrom(t, err); refusal != testCase.wantRefusal {
				t.Errorf("%s: refusal = %q, want %q; %s",
					testCase.name, refusal, testCase.wantRefusal, testCase.why)
			}
			assertNoMutation(t, harness)
		})
	}
}

// TestAnUnwiredAssuranceLeavesTheSnapshotBlockUnchanged.
//
// The upgrade path: a build or a test that does not wire assurance must behave
// exactly as it did before Phase 17. Nothing captured, and the existing baseline
// lookup is still what decides.
func TestAnUnwiredAssuranceLeavesTheSnapshotBlockUnchanged(t *testing.T) {
	harness := newExecHarness(t)
	if harness.assurance != nil {
		t.Fatal("fixture defect: this case is about the unwired build")
	}

	if _, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID}); err != nil {
		t.Fatalf("the pre-Phase-17 path was refused: %v", err)
	}
}
