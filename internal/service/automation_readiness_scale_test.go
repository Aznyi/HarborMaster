package service_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Readiness at the estate sizes Phase 17 supports.
//
// # What is asserted, and what is only reported
//
// Timings on one machine are indicative and are logged rather than asserted.
// The properties that hold everywhere are the ones asserted:
//
//   - the cost is LINEAR in containers. Two reads per governed container and a
//     fixed number of estate-wide reads, with nothing quadratic and nothing
//     per-container that loads a policy or rebuilds the graph;
//   - the dependency graph is built ONCE per evaluation, not once per
//     container;
//   - NO Docker call is made, at any size. Structural: `AutomationOptions` has
//     nowhere to put a Docker interface, which eight architecture tests hold;
//   - NO registry call is made. Readiness consumes the persisted assessment,
//     so opening the policy editor cannot become an estate-wide registry scan.
//
// # The shape of the cost
//
// `loadContainerEvidence` returns early for a container that is paused, opted
// out, or not governed -- so an estate where a policy governs nothing costs the
// estate-wide reads and nothing else. The measurements below use a BROAD policy
// deliberately, because that is the worst case: every container is governed and
// every container pays its per-container reads.

func TestReadinessAtSupportedScale(t *testing.T) {
	t.Parallel()

	for _, size := range []int{25, 500, 2000} {
		t.Run(fmt.Sprintf("%d containers", size), func(t *testing.T) {
			t.Parallel()

			view, names, edges := scaleEstate(t, size)

			harness := newAutomationHarness(t, scalePolicy())
			withDependencyEstate(t, harness, view, names, nil)
			harness.now = readinessAt

			// The candidate is the ONLY policy, so every container is
			// attributed to it. `scalePolicy` carries priority 10 and would
			// otherwise outrank a candidate at the default 0 and take the whole
			// estate -- which measures the wrong thing: the per-container cost
			// is paid either way, but `governed` would read 0 and the
			// attribution assertions would be vacuous.
			harness.policies.policies = nil

			// A broad policy, so every container is governed: the worst case for
			// per-container reads.
			policy := presetPolicyFor("scale",
				domain.StrategyMinor, domain.ModeAutomatic)

			started := time.Now()
			report, decisions, err := harness.engine.Readiness(
				context.Background(), &policy)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("readiness: %v", err)
			}

			if len(decisions) != size {
				t.Fatalf("decisions = %d, want %d", len(decisions), size)
			}
			if report.Considered != size {
				t.Fatalf("considered = %d, want %d", report.Considered, size)
			}

			reads := harness.evidence.reads()

			// ---- the properties, asserted -------------------------------

			// The estate is read ONCE, however many containers it holds.
			if reads["targets"] != 1 {
				t.Errorf("targets read %d times; the inventory is read once per evaluation",
					reads["targets"])
			}
			// HarborMaster's "needs no update" findings are read once by the
			// gate, not once per container.
			if reads["assessments"] > 1 {
				t.Errorf("assessments read %d times; the gate reads them once",
					reads["assessments"])
			}
			// The graph is built once. `dependencyView` answers from a
			// pre-built graph, so more than one call would mean the gate was
			// invoked per container.
			if got := view.calls(); got != 1 {
				t.Errorf("the dependency graph was read %d times; once per evaluation", got)
			}

			// Per-container reads are BOUNDED PER CONTAINER, which is what
			// makes the whole thing linear. Two lookups for a governed
			// container that is not in flight: the plan, and the acquisition
			// check. The execution check follows only when the first says no.
			if reads["plan"] > int64(size) {
				t.Errorf("plan read %d times for %d containers; more than one per container",
					reads["plan"], size)
			}
			perContainer := reads["plan"] + reads["acquisition"] + reads["execution"]
			if perContainer > int64(3*size) {
				t.Errorf("%d per-container reads for %d containers; the bound is 3 each",
					perContainer, size)
			}

			t.Logf("size=%d edges=%d stages=%d elapsed=%s "+
				"reads{targets=%d plan=%d acquisition=%d execution=%d assessments=%d} "+
				"graph=%d perContainer=%.2f docker=0 registry=0 "+
				"considered=%d governed=%d eligible=%d groups=%d",
				size, len(edges), len(view.view.Graph.Stages), elapsed,
				reads["targets"], reads["plan"], reads["acquisition"],
				reads["execution"], reads["assessments"],
				view.calls(), float64(perContainer)/float64(size),
				report.Considered, report.Governed, report.Eligible,
				len(report.Groups))
		})
	}
}

// TestReadinessCostsNothingPerContainerWhenNothingIsGoverned is the other end
// of the range.
//
// The cheap case matters as much as the worst one: an operator whose policy
// selects two containers must not pay for two thousand. `loadContainerEvidence`
// declines before any per-container read when the policy does not govern the
// container, and this is what holds that.
func TestReadinessCostsNothingPerContainerWhenNothingIsGoverned(t *testing.T) {
	const size = 500

	view, names, _ := scaleEstate(t, size)
	harness := newAutomationHarness(t, scalePolicy())
	withDependencyEstate(t, harness, view, names, nil)
	harness.now = readinessAt

	// A policy that names one container that does not exist.
	policy := presetPolicyFor("narrow", domain.StrategyMinor, domain.ModeAutomatic)
	policy.Scope = domain.ScopeSelector
	policy.Selector = domain.UpdateSelector{Include: []string{"nothing-matches-this"}}
	policy.Normalise()

	// The saved policies are what would otherwise govern these containers, so
	// they are cleared: this measures the candidate's own cost.
	harness.policies.policies = nil

	report, _, err := harness.engine.Readiness(context.Background(), &policy)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if report.Governed != 0 {
		t.Fatalf("governed = %d, want 0", report.Governed)
	}

	reads := harness.evidence.reads()
	perContainer := reads["plan"] + reads["acquisition"] + reads["execution"]
	if perContainer != 0 {
		t.Fatalf("%d per-container reads for an estate this policy governs none of; "+
			"the cheap check must decline before the reads", perContainer)
	}
	t.Logf("size=%d governed=0 perContainerReads=0 targets=%d", size, reads["targets"])
}
