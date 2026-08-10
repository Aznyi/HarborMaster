package service_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Rollback and image lineage.
//
// # The live defect this file exists to hold shut
//
// A real automated update failed verification, rolled back correctly, and left
// the estate in this state:
//
//	the container    running digest A   (the original, restored)
//	lineage claiming running digest B   (the replacement, already removed)
//
// Nothing about that is visible to an operator, and it is not self-correcting:
// EvaluateLineage compares the registry's answer for the tracking tag against
// RunningDigest, so a row claiming B answers "the newest tag already resolves to
// the digest this container is running" -- for a container running A. The
// workload silently stops being offered the update it still needs. That is the
// Phase 13 defect, reintroduced through the failure path.
//
// The cause was that the digest never made it into the execution record:
// OldImageDigest was read from the DECLARED REFERENCE, which is empty for every
// container created from a tag, and RestoreLineage will not overwrite a known
// digest with an empty one. See domain.RunningDigestFor.

// TestARollbackReturnsLineageToTheRestoredDigest is the regression.
func TestARollbackReturnsLineageToTheRestoredDigest(t *testing.T) {
	const (
		digestA = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
		digestB = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
		tracked = "docker.io/library/nginx:1.27.0"
	)

	harness := newRollbackHarness(t, func(h *rbHarness) {
		// The recreation recorded what the original was running. With the fix
		// this is the digest resolved from the local image; before it, empty.
		h.evidence.execution.OldImageDigest = digestA

		// Lineage as the succeeded recreation left it: advanced to the
		// replacement's digest, still following the operator's tag.
		h.lineage.rows[rbContainerName] = domain.ImageLineage{
			ContainerName:     rbContainerName,
			State:             domain.LineageTracked,
			Origin:            domain.LineageRecreated,
			TrackingReference: tracked,
			TrackingFamiliar:  "nginx:1.27.0",
			Repository:        "library/nginx",
			RunningDigest:     digestB,
		}
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q/%q, want succeeded", final.State, final.Failure, final.Refusal)
	}

	// ---- the host is the ground truth -------------------------------------

	if !harness.host.running(rbOriginalID) {
		t.Fatal("the original is not running; the rest of this test would be meaningless")
	}
	if name := harness.host.nameOf(rbOriginalID); name != rbContainerName {
		t.Fatalf("the original is named %q, want %q", name, rbContainerName)
	}

	// ---- lineage must agree with it ---------------------------------------

	row, err := harness.lineage.Get(t.Context(), rbContainerName)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}

	if row.RunningDigest != digestA {
		t.Fatalf("lineage RunningDigest = %q, want %q\n"+
			"\tthe rollback restored the original but lineage still claims the "+
			"replacement's digest; the next pass will read this workload as already "+
			"up to date and never offer the update again", row.RunningDigest, digestA)
	}

	// The tracking reference is the operator's choice. A rollback undoes which
	// ARTEFACT runs, never which tag is followed -- losing it here would drop
	// the workload out of update discovery entirely.
	if row.TrackingReference != tracked {
		t.Errorf("TrackingReference = %q, want %q", row.TrackingReference, tracked)
	}
	if !row.Tracked() {
		t.Errorf("lineage is no longer tracked after a rollback: %+v", row)
	}

	// The container the row points at is the ORIGINAL, not the parked
	// replacement: a stale id is how a later reconciliation would decide the
	// container had been replaced behind HarborMaster's back.
	if row.ContainerID != rbOriginalID {
		t.Errorf("ContainerID = %q, want the restored original %q", row.ContainerID, rbOriginalID)
	}

	// ---- and the next pass evaluates from A -------------------------------
	//
	// The assertion that matters most, stated in the terms automation uses: with
	// the registry still serving B for the tracked tag, the update must be
	// offered again rather than reported as settled.
	proposal := domain.EvaluateLineage(row, domain.ImageIntel{
		Reference:    tracked,
		Repository:   "library/nginx",
		Status:       domain.CheckOK,
		RemoteDigest: digestB,
	}, row.RunningDigest)

	if !proposal.Usable {
		t.Fatalf("the proposal is not usable after a rollback: %s", proposal.Reason)
	}
	if proposal.Update == domain.UpdateNone {
		t.Fatalf("after the rollback the workload reports no update available: %s\n"+
			"\tit is running A, the tag serves B, and the update it was rolled back "+
			"from is exactly the one it still needs", proposal.Reason)
	}
	if proposal.Digest != digestB {
		t.Errorf("proposed digest = %q, want %q", proposal.Digest, digestB)
	}
}

// TestARollbackWithNoRecordedOriginalDigestLeavesTrackingIntact.
//
// The degraded case: the original's digest could not be established at all -- a
// locally built image, or RepoDigests too ambiguous to choose between. The
// rollback must still complete, and must not invent a digest or forget the tag.
//
// Lineage is left claiming the old digest here, which is wrong-but-recoverable:
// reconciliation adopts what is actually running on the next pass, because it
// resolves the digest from the local image rather than from the reference. That
// path is proved by TestReconciliationCorrectsALineageDigestTheHostContradicts.
func TestARollbackWithNoRecordedOriginalDigestLeavesTrackingIntact(t *testing.T) {
	const tracked = "docker.io/library/nginx:1.27.0"

	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OldImageDigest = "" // unestablished
		h.lineage.rows[rbContainerName] = domain.ImageLineage{
			ContainerName:     rbContainerName,
			State:             domain.LineageTracked,
			Origin:            domain.LineageRecreated,
			TrackingReference: tracked,
			TrackingFamiliar:  "nginx:1.27.0",
			Repository:        "library/nginx",
			RunningDigest:     "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222",
		}
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q, want succeeded", final.State)
	}

	row, err := harness.lineage.Get(t.Context(), rbContainerName)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	if row.TrackingReference != tracked || !row.Tracked() {
		t.Fatalf("a rollback that could not establish a digest dropped the tracking "+
			"reference: %+v", row)
	}
}

// TestARollbackDoesNotEstablishLineageForAnUnmanagedContainer.
//
// A rollback restores a container that existed before HarborMaster's
// involvement in this change. It is not evidence about what that container
// follows, so it must not create a lineage record where none existed.
func TestARollbackDoesNotEstablishLineageForAnUnmanagedContainer(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OldImageDigest = "sha256:" +
			"1111111111111111111111111111111111111111111111111111111111111111"
		// No lineage row at all.
	})

	if final := harness.runOnce(t, harness.request(t)); final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q, want succeeded", final.State)
	}

	if _, err := harness.lineage.Get(t.Context(), rbContainerName); err == nil {
		t.Fatal("a rollback invented lineage for a container HarborMaster held no record of")
	}
}
