package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The approval endpoints, from the outside.
//
// Two properties carry the weight:
//
//   - a caller names a PLAN and nothing else. There is no body, so there is
//     nowhere to put an image, a digest or a container -- and therefore no way
//     to approve a change other than the one that was reviewed;
//   - approving is an OPERATOR act and a viewer cannot do it, while READING an
//     approval is available to anyone who can read the plan.

// samplePlanID is declared in plan_api_test.go; reused here deliberately so
// the two files cannot disagree about what a plan identifier looks like.

var planApprovalPath = APIPrefix + "/plan-approvals/" + samplePlanID

// fakePlanApprovals records what the handler asked for.
type fakePlanApprovals struct {
	approval domain.PlanApproval
	refusal  domain.PlanApprovalRefusal
	err      error

	approved []string
	revoked  []string
}

func (f *fakePlanApprovals) Approve(
	_ context.Context, planID string, by domain.Requester, _ service.Actor,
) (domain.PlanApproval, error) {
	f.approved = append(f.approved, planID)
	if f.err != nil {
		return domain.PlanApproval{}, f.err
	}
	approval := f.approval
	approval.PlanID = planID
	approval.ApprovedBy = by
	return approval, nil
}

func (f *fakePlanApprovals) Get(
	_ context.Context, planID string,
) (domain.PlanApproval, domain.PlanApprovalRefusal, error) {
	if f.err != nil {
		return domain.PlanApproval{}, "", f.err
	}
	approval := f.approval
	approval.PlanID = planID
	return approval, f.refusal, nil
}

func (f *fakePlanApprovals) Revoke(
	_ context.Context, planID string, _ domain.Requester, _ service.Actor,
) error {
	f.revoked = append(f.revoked, planID)
	return f.err
}

func approvalOptions(approvals PlanApprovalService) Options {
	return Options{
		Health:        &fakeHealth{},
		PlanApprovals: approvals,
		Logger:        discardLogger(),
		Config:        config.Server{MaxRequestBytes: 8192},
		Assets:        testAssets(),
	}
}

func newApprovalServer(t *testing.T, approvals PlanApprovalService) *Server {
	t.Helper()
	return newAuthedServer(approvalOptions(approvals))
}

func TestApprovingAPlanNamesOnlyThePlan(t *testing.T) {
	approvals := &fakePlanApprovals{
		approval: domain.PlanApproval{State: domain.PlanApprovalActive},
	}
	srv := newApprovalServer(t, approvals)

	rec := doJSON(t, srv, http.MethodPost, planApprovalPath, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if len(approvals.approved) != 1 || approvals.approved[0] != samplePlanID {
		t.Fatalf("the service was asked to approve %v", approvals.approved)
	}
}

// TestApprovalRefusesAMalformedPlanIdentifier stops the endpoint being used to
// probe what a plan identifier looks like.
func TestApprovalRefusesAMalformedPlanIdentifier(t *testing.T) {
	approvals := &fakePlanApprovals{}
	srv := newApprovalServer(t, approvals)

	for _, id := range []string{"nonsense", "plan_short", "plan_!!!!"} {
		rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/plan-approvals/"+id, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", id, rec.Code)
		}
	}

	// A traversal attempt never reaches routing at all: the router cleans the
	// path and redirects, so the handler is not called. Asserted as "not
	// accepted" rather than as a specific code, because the defence is the
	// normalisation rather than the validator.
	rec := doJSON(t, srv, http.MethodPost,
		APIPrefix+"/plan-approvals/../../etc/passwd", "")
	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("a traversal attempt was accepted: status = %d", rec.Code)
	}
	if len(approvals.approved) != 0 {
		t.Fatal("a malformed identifier reached the service")
	}
}

// TestApprovalPermissions is the §31 matrix.
func TestApprovalPermissions(t *testing.T) {
	cases := []struct {
		role   string
		method string
		want   int
	}{
		{"viewer", http.MethodGet, http.StatusOK},
		{"viewer", http.MethodPost, http.StatusForbidden},
		{"viewer", http.MethodDelete, http.StatusForbidden},

		{"operator", http.MethodGet, http.StatusOK},
		{"operator", http.MethodPost, http.StatusCreated},
		{"operator", http.MethodDelete, http.StatusNoContent},

		{"administrator", http.MethodGet, http.StatusOK},
		{"administrator", http.MethodPost, http.StatusCreated},
		{"administrator", http.MethodDelete, http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.role+"/"+tc.method, func(t *testing.T) {
			approvals := &fakePlanApprovals{
				approval: domain.PlanApproval{State: domain.PlanApprovalActive},
			}
			srv, _, _ := asRole(approvalOptions(approvals), domain.Role(tc.role))

			rec := doJSON(t, srv, tc.method, planApprovalPath, "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusForbidden &&
				(len(approvals.approved) != 0 || len(approvals.revoked) != 0) {
				t.Fatal("a refused caller reached the service")
			}
		})
	}
}

// TestApprovalRoutesDeclareTheRightPermissions is the structural half.
//
// Reading needs plan:read; recording and withdrawing need plan:approve. The
// route table is where that is decided, and this reads it directly rather than
// inferring it from a response code.
func TestApprovalRoutesDeclareTheRightPermissions(t *testing.T) {
	want := map[string]domain.Permission{
		http.MethodGet:    domain.PermPlanRead,
		http.MethodPost:   domain.PermPlanApprove,
		http.MethodDelete: domain.PermPlanApprove,
	}
	found := map[string]bool{}

	for _, route := range (&Server{}).routeTable() {
		if route.pattern != APIPrefix+"/plan-approvals/{id}" || route.method == "" {
			continue
		}
		expected, known := want[route.method]
		if !known {
			t.Errorf("unexpected method %s on the approval route", route.method)
			continue
		}
		if route.access.permission != expected {
			t.Errorf("%s requires %q, want %q",
				route.method, route.access.permission, expected)
		}
		found[route.method] = true
	}

	for method := range want {
		if !found[method] {
			t.Errorf("no %s route for plan approval", method)
		}
	}
}

// TestPlanApproveIsAnOperatorPermission pins the role decision.
func TestPlanApproveIsAnOperatorPermission(t *testing.T) {
	if !domain.RoleOperator.Can(domain.PermPlanApprove) {
		t.Error("an operator must be able to approve a plan: they already hold " +
			"execution:create, and plan approval is narrower")
	}
	if !domain.RoleAdministrator.Can(domain.PermPlanApprove) {
		t.Error("an administrator must hold every operator permission")
	}
	if domain.RoleViewer.Can(domain.PermPlanApprove) {
		t.Error("a viewer changes nothing, ever")
	}
}
