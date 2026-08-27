package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Plan approvals, against the real migrated schema.
//
// # Why these are write-path tests rather than schema assertions
//
// This repository has repeatedly shipped a vocabulary that existed in Go and
// not in the database, or the reverse. Reading `sqlite_master` proves the text
// of a CHECK; only an INSERT proves the database accepts what the Go code
// actually writes. Every value below is written and read back.

var approvalAt = time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

// seedPlan writes a real change plan so an approval has something to bind to.
func seedPlan(t *testing.T, db *store.DB, planID, containerID, digest string) domain.ChangePlan {
	t.Helper()

	plan := domain.ChangePlan{
		PlanID:           planID,
		ContainerID:      containerID,
		ContainerName:    "web",
		CurrentImage:     "nginx:latest",
		ProposedImage:    "nginx:latest",
		CurrentDigest:    "sha256:" + strings.Repeat("a", 64),
		ProposedDigest:   digest,
		UpdateType:       domain.UpdateDigest,
		RestoreReadiness: domain.ReadinessUnknown,
		RegistryStatus:   domain.CheckOK,
		Risk: domain.RiskAssessment{
			Score:          54,
			Band:           domain.RiskHigh,
			Recommendation: domain.RecommendManualReview,
			Summary:        "Worth a look first",
		},
		PlanVersion: 1,
		InputDigest: strings.Repeat("b", 64) + containerID,
		GeneratedAt: approvalAt,
	}
	if _, err := db.Plans.InsertPlans(
		context.Background(), []domain.ChangePlan{plan}, approvalAt); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return plan
}

func TestAnApprovalBindsToOnePlanAndRecordsWhoMadeIt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	plan := seedPlan(t, db, "plan_"+strings.Repeat("1", 20), "container-web",
		"sha256:"+strings.Repeat("c", 64))

	approved, err := db.PlanApprovals.Approve(ctx, domain.PlanApproval{
		PlanID:                 plan.PlanID,
		ApprovedInputDigest:    plan.InputDigest,
		ApprovedProposedDigest: plan.ProposedDigest,
		ApprovedBy:             domain.Requester{UserID: "usr_1", Username: "colby"},
	}, approvalAt)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.State != domain.PlanApprovalActive {
		t.Fatalf("state = %q, want active", approved.State)
	}

	read, err := db.PlanApprovals.Active(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	switch {
	case read.ApprovedBy.Username != "colby":
		t.Fatalf("approved by %q", read.ApprovedBy.Username)
	case read.ApprovedInputDigest != plan.InputDigest:
		t.Fatal("the input-digest tripwire did not survive the round trip")
	case read.ApprovedProposedDigest != plan.ProposedDigest:
		t.Fatal("the proposed-digest tripwire did not survive the round trip")
	case !read.Active():
		t.Fatal("a fresh approval must be active")
	}
}

// TestAnApprovalWithNobodyAttachedIsRefused pins the attribution requirement at
// the write path, not just in a comment.
func TestAnApprovalWithNobodyAttachedIsRefused(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	plan := seedPlan(t, db, "plan_"+strings.Repeat("2", 20), "container-web",
		"sha256:"+strings.Repeat("c", 64))

	if _, err := db.PlanApprovals.Approve(ctx, domain.PlanApproval{
		PlanID: plan.PlanID,
	}, approvalAt); err == nil {
		t.Fatal("an approval nobody made must be refused")
	}
}

// TestApprovingAPlanThatDoesNotExistIsRefused pins the foreign key.
func TestApprovingAPlanThatDoesNotExistIsRefused(t *testing.T) {
	db := openTestDB(t)

	_, err := db.PlanApprovals.Approve(context.Background(), domain.PlanApproval{
		PlanID:     "plan_" + strings.Repeat("9", 20),
		ApprovedBy: domain.Requester{UserID: "usr_1", Username: "colby"},
	}, approvalAt)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an approval of a plan that does not exist, got %v", err)
	}
}

// TestOnlyOneActiveApprovalPerPlan is the idempotency constraint.
func TestOnlyOneActiveApprovalPerPlan(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	plan := seedPlan(t, db, "plan_"+strings.Repeat("3", 20), "container-web",
		"sha256:"+strings.Repeat("c", 64))

	approval := domain.PlanApproval{
		PlanID:     plan.PlanID,
		ApprovedBy: domain.Requester{UserID: "usr_1", Username: "colby"},
	}
	if _, err := db.PlanApprovals.Approve(ctx, approval, approvalAt); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := db.PlanApprovals.Approve(ctx, approval, approvalAt); !errors.Is(err, store.ErrPlanApprovalActive) {
		t.Fatalf("want ErrPlanApprovalActive, got %v", err)
	}

	// Revoking frees the slot, and the withdrawn decision stays in the history.
	if err := db.PlanApprovals.Revoke(ctx, plan.PlanID,
		domain.Requester{UserID: "usr_1", Username: "colby"}, approvalAt); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := db.PlanApprovals.Active(ctx, plan.PlanID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked approval must not read as active, got %v", err)
	}
	if _, err := db.PlanApprovals.Approve(ctx, approval, approvalAt); err != nil {
		t.Fatalf("re-approving after a revocation must be allowed: %v", err)
	}
}

// TestConcurrentApprovalsSettleOnOne proves the index does the work.
func TestConcurrentApprovalsSettleOnOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	plan := seedPlan(t, db, "plan_"+strings.Repeat("4", 20), "container-web",
		"sha256:"+strings.Repeat("c", 64))

	const callers = 8
	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			_, err := db.PlanApprovals.Approve(ctx, domain.PlanApproval{
				PlanID:     plan.PlanID,
				ApprovedBy: domain.Requester{UserID: "usr_1", Username: "colby"},
			}, approvalAt)
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()

	if succeeded != 1 {
		t.Fatalf("%d approvals succeeded, want exactly 1", succeeded)
	}
}

// TestPlanMutationIsDerivedFromExecutions pins the consumption rule's source.
//
// The approval table stores no `consumed` state. Whether an approval is spent is
// read from the execution that actually changed the host, which is where that
// fact is already written.
func TestPlanMutationIsDerivedFromExecutions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	plan := seedPlan(t, db, "plan_"+strings.Repeat("6", 20), "container-web",
		"sha256:"+strings.Repeat("c", 64))

	mutated, err := db.PlanApprovals.PlanHasMutated(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("PlanHasMutated: %v", err)
	}
	if mutated {
		t.Fatal("no execution has run; nothing can have mutated")
	}
}
