package service_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// A REATTACHMENT is checked against itself, not against the registry.
//
// # The defect this pins
//
// The acquisition preflight's TOCTOU check compares the plan's proposal against
// `proposedChange(intel)` -- what the registry would UPGRADE this image to. A
// rebind proposes the opposite: the digest the container is already running, so
// it can be recreated unchanged and reattached to a replaced provider.
//
// The two are different by construction, so every reattachment was refused with
// "the digest on offer has changed since the plan was written" -- while the
// provider it was meant to reattach to had already been replaced. The dependent
// was left holding a namespace reference to a container that no longer existed.
//
// Found live in Stage 5 against Docker 29.7.2.
//
// # What replaced it, and why it is the same guarantee
//
// The property the check exists for is "the image that gets pulled is the image
// the plan named, and nothing moved underneath". A rebind's target came from
// HarborMaster's own inventory of what the container is running, re-read
// immediately before the plan was built. So the equivalent statement is that
// the plan is SELF-CONSISTENT: it proposes the reference and digest it has.
//
// Every other gate is untouched, and the tests below assert that.

// asRebind turns the harness's plan into a reattachment of the current image.
func asRebind(h *acquisitionHarness) {
	for _, plan := range []*domain.ChangePlan{&h.evidence.plan, &h.evidence.current} {
		plan.UpdateType = domain.UpdateRebind
		plan.ProposedImage = plan.CurrentImage
		plan.ProposedDigest = acqTestDigest
		plan.CurrentDigest = acqTestDigest
	}
}

func TestAReattachmentIsNotRefusedByTheRegistryComparison(t *testing.T) {
	harness := newAcquisitionHarness(t, asRebind, func(h *acquisitionHarness) {
		// The registry is offering something ELSE entirely -- a newer tag with a
		// different digest. Which is the ordinary case: a reattachment happens
		// precisely when the estate has moved on.
		h.evidence.intel.LatestTag = "1.99.0"
		h.evidence.intel.LatestDigest = acqOtherDigest
	})

	acquisition, err := harness.service.Request(t.Context(),
		service.AcquisitionRequest{PlanID: acqPlanID})
	if err != nil {
		t.Fatalf("a reattachment was refused: %v\n\n"+
			"A rebind proposes the digest the container is ALREADY RUNNING. "+
			"Comparing that against what the registry would upgrade it to refuses "+
			"every reattachment -- while the provider it was meant to reattach to "+
			"has already been replaced.", err)
	}

	// And it acquires the digest the container is running, not the newer one.
	if acquisition.Target.Digest != acqTestDigest {
		t.Errorf("the acquisition targets %q, want the running digest %q",
			acquisition.Target.Digest, acqTestDigest)
	}
}

// A rebind whose image fields were CROSSED is still refused.
//
// The self-consistency check is not a bypass: a reattachment proposing anything
// other than what it currently has is exactly the crossing the ordinary path
// refuses, and it must refuse here too.
func TestACrossedReattachmentIsStillRefused(t *testing.T) {
	for name, mutate := range map[string]func(*acquisitionHarness){
		"the proposed reference is not the current one": func(h *acquisitionHarness) {
			asRebind(h)
			h.evidence.plan.ProposedImage = "nginx:1.99.0"
			h.evidence.current.ProposedImage = "nginx:1.99.0"
		},
		"the proposed digest is not the current one": func(h *acquisitionHarness) {
			asRebind(h)
			h.evidence.plan.ProposedDigest = acqOtherDigest
			h.evidence.current.ProposedDigest = acqOtherDigest
		},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newAcquisitionHarness(t, mutate)
			_, err := harness.service.Request(t.Context(),
				service.AcquisitionRequest{PlanID: acqPlanID})
			if err == nil {
				t.Fatal("a crossed reattachment was accepted")
			}
			if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalDigestChanged {
				t.Errorf("refusal = %q, want %q", refusal, domain.AcquisitionRefusalDigestChanged)
			}
		})
	}
}

// Every other gate still applies to a reattachment.
//
// The change touched one comparison. A reattachment of HarborMaster's own
// container, of a container with a critical violation, or one whose registry
// record is missing must be refused exactly as before.
func TestAReattachmentStillPassesEveryOtherGate(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*acquisitionHarness)
		want   domain.AcquisitionRefusal
	}{
		"the registry has never been checked": {
			mutate: func(h *acquisitionHarness) {
				asRebind(h)
				h.evidence.intel.Status = domain.CheckPending
				h.evidence.intel.LastSuccessAt = nil
			},
			want: domain.AcquisitionRefusalRegistryStale,
		},
		"the plan does not recommend the change": {
			mutate: func(h *acquisitionHarness) {
				asRebind(h)
				h.evidence.plan.Risk.Recommendation = domain.RecommendAgainst
				h.evidence.current.Risk.Recommendation = domain.RecommendAgainst
			},
			want: domain.AcquisitionRefusalRecommendation,
		},
		"the plan names no digest": {
			mutate: func(h *acquisitionHarness) {
				asRebind(h)
				h.evidence.plan.ProposedDigest = ""
				h.evidence.current.ProposedDigest = ""
			},
			want: domain.AcquisitionRefusalDigestUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newAcquisitionHarness(t, test.mutate)
			_, err := harness.service.Request(t.Context(),
				service.AcquisitionRequest{PlanID: acqPlanID})
			if err == nil {
				t.Fatalf("a reattachment passed the %s gate", name)
			}
			if refusal := refusalFrom(t, err); refusal != test.want {
				t.Errorf("refusal = %q, want %q", refusal, test.want)
			}
		})
	}
}

// An ordinary update is unaffected.
//
// The non-vacuity guard on the whole change: the registry comparison must still
// refuse a plan whose upgrade target moved after it was written.
func TestAnOrdinaryUpdateStillFailsTheRegistryComparison(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.evidence.intel.LatestDigest = acqOtherDigest
	})

	_, err := harness.service.Request(t.Context(),
		service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("an ordinary update whose digest moved was accepted")
	}
	if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalDigestChanged {
		t.Errorf("refusal = %q, want %q", refusal, domain.AcquisitionRefusalDigestChanged)
	}
}
