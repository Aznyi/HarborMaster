package api

import (
	"context"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// PlanApprovalService is the plan-review capability the API depends on.
//
// Three methods, and every one takes a PLAN IDENTIFIER plus who is asking.
// Nothing on this interface accepts an image, a digest, a container, a
// recommendation or a risk score, so there is nowhere for a caller-supplied fact
// to enter -- HarborMaster reads all of them from the plan itself.
//
// Note what is absent: nothing here acquires an image or recreates a container.
// Approving a plan and applying it are separate acts with separate permissions,
// and this interface can only do the first.
type PlanApprovalService interface {
	Approve(ctx context.Context, planID string, by domain.Requester, actor service.Actor) (domain.PlanApproval, error)
	Get(ctx context.Context, planID string) (domain.PlanApproval, domain.PlanApprovalRefusal, error)
	Revoke(ctx context.Context, planID string, by domain.Requester, actor service.Actor) error
}
