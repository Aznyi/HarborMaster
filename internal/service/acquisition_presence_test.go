package service_test

import (
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Presence, on the path that can pull an image (C3E).
//
// # The defect
//
// The gate in front of image acquisition asked ContainerRepository.Get whether
// a row came back, and treated success as "the container exists". The inventory
// keeps a departed container's row -- history reads it -- so a container that
// had been removed from the host passed the gate on the strength of its own
// tombstone.
//
// It is now ContainerRepository.GetPresent, which answers the question the
// gate's name promises. These pin the two properties that matter: a departed
// container is refused, and a presence read that FAILS is refused too.

func TestAnAbsentContainerCannotStartAnAcquisition(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.evidence.present = false
	})

	_, err := harness.service.Request(t.Context(),
		service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("a departed container produced an acquisition")
	}
	if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalContainerMissing {
		t.Fatalf("refusal = %q, want containerMissing", refusal)
	}
}

func TestAnUnreadablePresenceRefusesRatherThanAssumingPresent(t *testing.T) {
	// FAIL CLOSED. A presence read that could not be performed establishes
	// nothing, and "we could not tell" must never be spent as "it is there" on
	// a path that pulls an image.
	//
	// The request fails outright rather than producing a refusal, which is the
	// right shape: a refusal is a decision HarborMaster reached, and it did not
	// reach one here.
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.evidence.present = true
		h.evidence.presentErr = errors.New("the presence read failed")
	})

	_, err := harness.service.Request(t.Context(),
		service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("an unreadable presence produced an acquisition; it must refuse")
	}
	// Not a refusal: a refusal is a decision HarborMaster reached, and it did
	// not reach one here.
	var refused service.ErrAcquisitionRefused
	if errors.As(err, &refused) {
		t.Errorf("an unreadable presence produced the refusal %q rather than a "+
			"failure; the check did not happen", refused.Refusal)
	}
}
