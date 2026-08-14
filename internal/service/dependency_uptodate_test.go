package service_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Blocker 1: an upstream HarborMaster positively assessed as CURRENT must
// release its dependents.
//
// # The live failure this reproduces
//
// Stage 5b, scenario D. `hm16-upstream` ran alpine:3.24.1 -- the newest tag,
// healthy, tracked, with a successful registry check. `hm16-downstream` had a
// real patch update waiting and depended on it. Every pass reported:
//
//	hm16-upstream    verdict=skip reason=noPlan
//	hm16-downstream  verdict=skip reason=dependencyIneligible blockedBy=hm16-upstream
//	                 "a container this one depends on needs an update that the
//	                  rules in force do not permit"
//
// which was untrue: the upstream needed nothing. The chain never advanced.
//
// # Why "no plan row" is the wrong thing to read
//
// The planner deliberately writes NO row for a container it assessed and found
// current (planner.go, planTracked: "Settled. Nothing to propose, and a row
// saying so for every current container is the noise the planner already
// declines to write"). So `noPlan` conflates two opposite facts:
//
//	A. HarborMaster looked and the container is current   -> may release
//	B. HarborMaster did not look, or could not            -> must hold
//
// TestATrackedContainerAtTheRegistryDigestProposesNothing, in
// lineage_planner_test.go, already proves the premise against the REAL planner:
// a tracked container sitting on the registry digest produces no plan. These
// tests do not restate that; they take the same lineage and registry evidence
// and require the dependency gate to reach the opposite conclusion from the
// one it reaches today.

// ------------------------------------------------------------- fixtures --

const (
	upToDateRef      = "docker.io/library/alpine:3.24.1"
	upToDateFamiliar = "alpine:3.24.1"
	upToDateRepo     = "docker.io/library/alpine"
	upToDateDigest   = "sha256:" + "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	upToDateNewer    = "sha256:" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// currentLineage is the upstream as HarborMaster records it: tracked, on a
// known reference, running the digest HarborMaster itself put there.
func currentLineage(name, containerID, running string) domain.ImageLineage {
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	return domain.ImageLineage{
		ContainerName:     name,
		ContainerID:       containerID,
		State:             domain.LineageTracked,
		Origin:            domain.LineageRecreated,
		TrackingReference: upToDateRef,
		TrackingFamiliar:  upToDateFamiliar,
		Repository:        upToDateRepo,
		RunningDigest:     running,
		CreatedAt:         at,
		UpdatedAt:         at,
	}
}

// currentIntel is a SUCCESSFUL registry check whose answer is "nothing newer".
func currentIntel(checked time.Time) domain.ImageIntel {
	return domain.ImageIntel{
		Reference:    upToDateRef,
		Familiar:     upToDateFamiliar,
		Repository:   upToDateRepo,
		Tag:          "3.24.1",
		RemoteDigest: upToDateDigest,
		Status:       domain.CheckOK,
		Update:       domain.UpdateNone,
		LastCheckedAt: func() *time.Time {
			at := checked
			return &at
		}(),
	}
}

// ------------------------------------------------------- the regression --

// A positively-assessed, current upstream releases its dependent.
//
// The upstream has NO plan row -- exactly as the real planner leaves it -- and
// carries HarborMaster's own positive finding that it is current. The dependent
// has a real update waiting and must be submitted.
func TestAPositivelyCurrentUpstreamReleasesItsDependent(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	view := graphOver(t, []string{"upstream", "downstream"},
		operatorDep("downstream", "upstream"))

	// HarborMaster's own positive finding, from its own persisted lineage and
	// registry evidence. Nothing here is caller-supplied.
	//
	// Set BEFORE the estate helper, which is what builds the engine with the
	// dependency view wired in.
	harness.evidence.assessments = map[string]domain.CurrentAssessment{
		"upstream": domain.AssessCurrent(
			currentLineage("upstream", "container-upstream", upToDateDigest),
			currentIntel(harness.now.Add(-time.Hour)),
			"container-upstream",
			harness.now,
			24*time.Hour,
		),
	}

	// upstream: no plan row. downstream: a real update.
	withDependencyEstate(t, harness, view, []string{"downstream"}, []string{"upstream"})

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	upstream := decisionFor(decisions, "upstream")
	if upstream.Reason != domain.ReasonNoPlan {
		t.Fatalf("upstream reason = %q, want %q; the fixture must reproduce the real "+
			"planner's behaviour of writing no row for a settled container",
			upstream.Reason, domain.ReasonNoPlan)
	}

	downstream := decisionFor(decisions, "downstream")
	if downstream.DependencyState != domain.DependencySatisfied {
		t.Fatalf("downstream dependency state = %q (reason %q, blockedBy %q), want %q; "+
			"an upstream HarborMaster positively established as current must release "+
			"its dependents",
			downstream.DependencyState, downstream.Reason,
			downstream.BlockedBy, domain.DependencySatisfied)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 1 || got[0] != "downstream" {
		t.Fatalf("submitted = %v, want [downstream]", got)
	}
}

// An upstream with NO assessment still holds its dependent.
//
// This is the safety half, and it must never regress: "HarborMaster did not
// look" is not "the container is fine". Stage 4 fixed exactly this inversion.
func TestAnUnassessedUpstreamStillHoldsItsDependent(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	view := graphOver(t, []string{"upstream", "downstream"},
		operatorDep("downstream", "upstream"))
	// No assessment at all: the zero map. Nothing was established.
	withDependencyEstate(t, harness, view, []string{"downstream"}, []string{"upstream"})

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	downstream := decisionFor(decisions, "downstream")
	if downstream.DependencyState == domain.DependencySatisfied {
		t.Fatalf("downstream was released with no assessment of its upstream; "+
			"unknown must hold (state %q)", downstream.DependencyState)
	}
	if got := submittedNames(t, harness, decisions); len(got) != 0 {
		t.Fatalf("submitted = %v, want none", got)
	}
}

// ------------------------------------------------- the assessment itself --

// AssessCurrent establishes nothing unless every positive fact holds.
//
// The zero value of the result must mean UNKNOWN. Each row below removes one
// fact from an otherwise-current container and requires the assessment to stop
// being established.
func TestAssessCurrentRequiresEveryPositiveFact(t *testing.T) {
	t.Parallel()

	const containerID = "container-upstream"
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fresh := 24 * time.Hour

	good := func() (domain.ImageLineage, domain.ImageIntel) {
		return currentLineage("upstream", containerID, upToDateDigest),
			currentIntel(now.Add(-time.Hour))
	}

	// Positive control: the whole point is that this one IS established.
	lineage, intel := good()
	if got := domain.AssessCurrent(lineage, intel, containerID, now, fresh); !got.Established {
		t.Fatalf("a tracked, current, freshly-checked container was not established "+
			"as current (%q); every negative case below would then pass vacuously",
			got.Reason)
	}

	cases := []struct {
		name    string
		mutate  func(*domain.ImageLineage, *domain.ImageIntel)
		observe string
	}{
		{
			name:   "untracked lineage",
			mutate: func(l *domain.ImageLineage, _ *domain.ImageIntel) { l.State = domain.LineageUntracked },
		},
		{
			name:   "no tracking reference",
			mutate: func(l *domain.ImageLineage, _ *domain.ImageIntel) { l.TrackingReference = "" },
		},
		{
			name: "registry check did not succeed",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				i.Status = domain.CheckFailed
			},
		},
		{
			name: "registry check is missing entirely",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				*i = domain.ImageIntel{}
			},
		},
		{
			name: "registry evidence belongs to another reference",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				i.Reference = "docker.io/library/other:1.0"
			},
		},
		{
			name: "registry evidence has never been checked",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				i.LastCheckedAt = nil
			},
		},
		{
			name: "registry evidence is stale",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				old := now.Add(-48 * time.Hour)
				i.LastCheckedAt = &old
			},
		},
		{
			name: "the registry serves a different digest than the container runs",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				i.RemoteDigest = upToDateNewer
			},
		},
		{
			name: "a newer tag is published",
			mutate: func(_ *domain.ImageLineage, i *domain.ImageIntel) {
				i.Update = domain.UpdatePatch
				i.LatestTag = "3.24.2"
				i.LatestDigest = upToDateNewer
			},
		},
		{
			name:   "the running digest was never established",
			mutate: func(l *domain.ImageLineage, _ *domain.ImageIntel) { l.RunningDigest = "" },
		},
		{
			name:    "the container was replaced outside HarborMaster",
			observe: "some-other-container-id",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			lineage, intel := good()
			if testCase.mutate != nil {
				testCase.mutate(&lineage, &intel)
			}
			observed := containerID
			if testCase.observe != "" {
				observed = testCase.observe
			}

			got := domain.AssessCurrent(lineage, intel, observed, now, fresh)
			if got.Established {
				t.Fatalf("assessment was established despite %s; unknown must not "+
					"read as current", testCase.name)
			}
		})
	}
}

// ------------------------------------------------- degraded evidence path --

// An unreadable assessment read holds dependents rather than releasing them.
//
// The read is not a refusal -- an empty result establishes nothing, and
// establishing nothing already holds -- so this proves the degraded direction
// is the conservative one rather than a fail-open.
//
// The exhaustive reason-by-assessment table lives in
// TestOnlyAnAssessedNoUpdateClearsAnUpstream, an internal test over needsWork
// itself; walking it through the pass would need a contrived estate per reason.
func TestAnUnreadableAssessmentHoldsDependents(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	view := graphOver(t, []string{"upstream", "downstream"},
		operatorDep("downstream", "upstream"))
	harness.evidence.assessErr = errors.New("the lineage table could not be read")
	withDependencyEstate(t, harness, view, []string{"downstream"}, []string{"upstream"})

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	downstream := decisionFor(decisions, "downstream")
	if downstream.DependencyState == domain.DependencySatisfied {
		t.Fatal("a dependent was released while the up-to-date evidence could not " +
			"be read; an unreadable assessment must establish nothing")
	}
	if got := submittedNames(t, harness, decisions); len(got) != 0 {
		t.Fatalf("submitted = %v, want none", got)
	}
}

// A positive assessment does not make an ineligible container eligible.
//
// The gate may only ever SUBTRACT. An upstream that is current must not become
// a container automation acts on, and the assessment must not touch the
// upstream's own verdict.
func TestAPositiveAssessmentNeverEnrolsTheUpstream(t *testing.T) {
	t.Parallel()

	harness := newAutomationHarness(t, broadPolicy())
	view := graphOver(t, []string{"upstream", "downstream"},
		operatorDep("downstream", "upstream"))

	harness.evidence.assessments = map[string]domain.CurrentAssessment{
		"upstream": domain.AssessCurrent(
			currentLineage("upstream", "container-upstream", upToDateDigest),
			currentIntel(harness.now.Add(-time.Hour)),
			"container-upstream", harness.now, 24*time.Hour,
		),
	}
	withDependencyEstate(t, harness, view, []string{"downstream"}, []string{"upstream"})

	_, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	upstream := decisionFor(decisions, "upstream")
	if upstream.Verdict == domain.VerdictUpdate {
		t.Fatalf("a current upstream was enrolled for an update; the dependency "+
			"subsystem may only ever subtract (reason %q)", upstream.Reason)
	}
	for _, name := range submittedNames(t, harness, decisions) {
		if name == "upstream" {
			t.Fatal("the upstream was submitted; a positive current assessment must " +
				"never cause a mutation")
		}
	}
}

// The assessment carries no container id, image, digest or registry a caller
// could steer.
//
// Guards the same property the dependency create request is guarded for: there
// must be nowhere on this type to put a target.
func TestCurrentAssessmentCarriesNoTarget(t *testing.T) {
	t.Parallel()

	forbidden := []string{"containerid", "image", "digest", "registry", "reference",
		"repository", "tag", "plan", "provider"}

	assessment := reflect.TypeOf(domain.CurrentAssessment{})
	for index := 0; index < assessment.NumField(); index++ {
		field := assessment.Field(index).Name
		lowered := strings.ToLower(field)
		for _, bad := range forbidden {
			if strings.Contains(lowered, bad) {
				t.Fatalf("CurrentAssessment has a field %q resembling a target (%q); "+
					"the assessment states a FACT and must never name a thing to act on",
					field, bad)
			}
		}
	}
}

// Sanity: the store adapter reports nothing when its evidence is unavailable.
//
// A deployment without lineage or registry evidence must establish NOTHING,
// which holds every dependent, rather than releasing them all.
func TestAbsentEvidenceEstablishesNothing(t *testing.T) {
	t.Parallel()

	evidence := service.NewAutomationEvidence(nil, nil, nil, nil, nil, nil)
	got, err := evidence.CurrentAssessments(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("CurrentAssessments with no repositories: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CurrentAssessments = %v, want empty; absent evidence must "+
			"establish nothing", got)
	}
}

// Guard against the fixture drifting away from the real store row shape.
var _ = store.AutomationTarget{}
