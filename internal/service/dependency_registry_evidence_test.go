package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The reattachment plan is assessed with REAL registry evidence.
//
// # The defect this pins
//
// `image_intel` is keyed on the canonical reference: a container running
// `alpine:3.22.1` is stored as `docker.io/library/alpine:3.22.1`. The planner
// normalises before it asks. The dependency coordinator did not -- it passed the
// container's raw reference straight through -- so the lookup matched no row and
// every rebind was assessed with its registry evidence missing.
//
// The model reported that honestly, as "cannot advise". The acquisition service
// then refused the reattachment, because it only acquires an image when the
// assessment supports it. So the provider was replaced, the plan was written,
// and the dependent was never reattached: left holding a namespace reference to
// a container that no longer existed.
//
// Found live in Stage 5, three layers from the mistake. Nothing in the unit
// suite could catch it, because the coordinator's fake plan store ignored the
// reference argument entirely -- see the note on its GatherInputs.

// A rebind asks for the reference in the form the store can answer.
func TestARebindLooksUpTheCanonicalReference(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()
	ctx := context.Background()

	if _, err := harness.service().ProduceRebindPlans(ctx, operation.OperationID); err != nil {
		t.Fatalf("ProduceRebindPlans: %v", err)
	}

	asked := harness.plans.gathered()
	if len(asked) == 0 {
		t.Fatal("the coordinator asked for no image reference at all; the plan " +
			"cannot carry registry evidence it never requested")
	}
	for _, reference := range asked {
		normalised, err := domain.NormalizeImageRef(reference)
		if err != nil {
			t.Errorf("asked for %q, which is not a normalisable reference", reference)
			continue
		}
		if normalised.Canonical != reference {
			t.Errorf("asked for the raw reference %q; image_intel is keyed on the "+
				"canonical form %q, so this matches no row and the plan is "+
				"assessed with no registry evidence",
				reference, normalised.Canonical)
		}
	}
}

// And the plan it produces carries a usable recommendation.
//
// The end of the chain, and the thing that actually mattered: a plan the
// acquisition service will act on. `unknown` is refused, so a rebind that
// cannot be assessed is a rebind that never happens.
func TestARebindPlanCarriesAnActionableAssessment(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()
	ctx := context.Background()

	result, err := harness.service().ProduceRebindPlans(ctx, operation.OperationID)
	if err != nil {
		t.Fatalf("ProduceRebindPlans: %v", err)
	}
	planID, ok := result.Created[depDependent]
	if !ok {
		t.Fatalf("no plan was created for %s; skipped=%v satisfied=%v",
			depDependent, result.Skipped, result.Satisfied)
	}

	plan, err := harness.plans.Get(ctx, planID)
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}

	if plan.RegistryStatus != domain.CheckOK {
		t.Errorf("registry status = %q, want %q\n\n"+
			"The dependent's image HAS an intelligence record. A plan reporting "+
			"otherwise is a plan assessed on evidence it failed to look up.",
			plan.RegistryStatus, domain.CheckOK)
	}
	if plan.Risk.Recommendation == domain.RecommendUnknown {
		t.Errorf("recommendation = %q\n\n"+
			"The acquisition service only acquires an image when the assessment "+
			"supports it, so a reattachment the model cannot advise on is a "+
			"reattachment that never happens -- while the provider it was meant "+
			"to reattach to has already been replaced.",
			plan.Risk.Recommendation)
	}
	// And the summary does not say the thing that misled the operator.
	if strings.Contains(strings.ToLower(plan.Risk.Summary), "never been checked") {
		t.Errorf("the plan reports its image as never checked: %q", plan.Risk.Summary)
	}
}
