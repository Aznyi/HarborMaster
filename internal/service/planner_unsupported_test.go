package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Unsupported image references, in the planner.
//
// # What an unsupported reference is
//
// One domain.NormalizeImageRef refuses: a registry on a port, an address
// literal, a host with no dot, a malformed string. It can never become a
// registry request, and refusing it is the SSRF boundary -- see the refusal
// list on NormalizeImageRef.
//
// # Why it must still be planned
//
// A refused reference is still TRACKED. The inventory sync writes an
// image_intel row for it under the RAW reference, with status unsupported and
// no scheduled check, precisely so the gap in coverage is visible rather than
// silent.
//
// The planner used to read that row under the CANONICAL reference, which for a
// refused reference is the empty string. The lookup therefore always missed,
// the container fell into the "never looked at" branch, and it was omitted from
// planning altogether -- so a container HarborMaster could not assess was
// indistinguishable from one it had never seen.
//
// # What these tests hold
//
//   - The record is found, under the key it is stored beneath.
//   - The container is planned, carrying the absence of evidence honestly.
//   - NOTHING is invented from a string the domain refused to parse: no
//     repository, no tag, no digest, no proposal, no recommendation.
//   - Containers cannot read one another's evidence through a shared key.
//   - Every other registry outcome keeps the meaning it had.

// unsupportedIntel is the record the inventory sync writes for a reference that
// could not be normalised. The RAW reference is the identity, there is no
// canonical form, and no lookup has ever happened.
func unsupportedIntel(raw string) domain.ImageIntel {
	return domain.ImageIntel{
		Reference:    raw,
		Familiar:     raw,
		Kind:         domain.RegistryUnknown,
		Status:       domain.CheckUnsupported,
		StatusDetail: "the image reference cannot be looked up: it names no public registry",
		Update:       domain.UpdateNone,
	}
}

// The reference forms the domain refuses, each for its own stated reason.
const (
	unsupportedPortRef    = "registry.internal:5000/app:1.2.3"
	unsupportedLiteralRef = "10.0.0.7/team/api:2.0"
	unsupportedMalformed  = "NOT A REFERENCE:::"
)

// The fixtures must actually be unsupported. A fixture that quietly started
// normalising would make every test below pass for the wrong reason.
func TestTheUnsupportedFixturesAreActuallyUnsupported(t *testing.T) {
	for _, raw := range []string{unsupportedPortRef, unsupportedLiteralRef, unsupportedMalformed} {
		if _, err := domain.NormalizeImageRef(raw); err == nil {
			t.Errorf("%q normalised; it is no longer an unsupported fixture", raw)
		}
	}
}

// THE DEFECT. A container HarborMaster observes, and has already recorded as
// unsupported, must be represented rather than silently omitted.
func TestAnUnsupportedReferenceStillGetsAPlan(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "internal-app", unsupportedPortRef),
	}
	fake.intel[unsupportedPortRef] = unsupportedIntel(unsupportedPortRef)

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 1 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want one plan recording that the image cannot be assessed", result)
	}

	plan := fake.plans()[0]
	if plan.ContainerID != "container-a" || plan.ContainerName != "internal-app" {
		t.Errorf("plan names the wrong container: %+v", plan)
	}

	// The registry state is reported as what it is, not collapsed into a
	// failure, an absence of updates, or a success.
	if plan.RegistryStatus != domain.CheckUnsupported {
		t.Errorf("registry status = %q, want %q", plan.RegistryStatus, domain.CheckUnsupported)
	}

	// NO FABRICATED TARGET. Nothing may be derived from a string the domain
	// refused to parse: no repository, no tag, no digest, no proposal.
	if plan.ProposedImage != "" {
		t.Errorf("proposed image = %q, want none for a reference that cannot be looked up",
			plan.ProposedImage)
	}
	if plan.ProposedDigest != "" {
		t.Errorf("proposed digest = %q, want none", plan.ProposedDigest)
	}
	// NOT UpdateNone. An unsupported reference is never looked up, so its
	// update column holds the column default rather than an observation, and
	// reporting that default as "none" claims a comparison that never happened.
	if plan.UpdateType != domain.UpdateUnknown {
		t.Errorf("update type = %q, want unknown", plan.UpdateType)
	}
	if plan.UpdateType.Available() {
		t.Error("an unsupported reference was reported as having an update available")
	}

	// The plan reports what the container actually declares, unaltered.
	if plan.CurrentImage != unsupportedPortRef {
		t.Errorf("current image = %q, want the declared reference %q",
			plan.CurrentImage, unsupportedPortRef)
	}

	// NON-ACTIONABLE. An unknown-severity factor forces an unknown
	// recommendation, which is what stops the workspace offering an approval.
	if plan.Risk.Recommendation != domain.RecommendUnknown {
		t.Errorf("recommendation = %q, want unknown", plan.Risk.Recommendation)
	}
	if plan.Risk.Summary == "" || len(plan.Risk.Factors) == 0 {
		t.Errorf("plan carries no reasoning: %+v", plan.Risk)
	}
}

// The record is looked up under the key it is STORED beneath -- the raw
// reference -- and never under the empty canonical form.
func TestTheUnsupportedRecordIsLookedUpUnderTheRawReference(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "internal-app", unsupportedPortRef),
	}
	fake.intel[unsupportedPortRef] = unsupportedIntel(unsupportedPortRef)

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	fake.mu.Lock()
	asked := append([]string(nil), fake.requestedRefs...)
	fake.mu.Unlock()

	var sawRaw bool
	for _, reference := range asked {
		if reference == "" {
			t.Error("the planner asked for the empty reference, which names no record")
		}
		if reference == unsupportedPortRef {
			sawRaw = true
		}
	}
	if !sawRaw {
		t.Errorf("references asked for = %q, want the raw unsupported reference", asked)
	}
}

// COLLISION. Two containers on different unsupported references must read
// their OWN evidence. Under a shared sentinel key they would read one entry.
func TestTwoUnsupportedContainersDoNotShareEvidence(t *testing.T) {
	first := unsupportedIntel(unsupportedPortRef)
	second := unsupportedIntel(unsupportedLiteralRef)
	// Distinguishable evidence, so a shared entry is visible rather than
	// merely possible.
	second.Status = domain.CheckPending

	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "one", unsupportedPortRef),
		candidate("container-b", "two", unsupportedLiteralRef),
	}
	fake.intel[unsupportedPortRef] = first
	fake.intel[unsupportedLiteralRef] = second

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 2 {
		t.Fatalf("result = %+v, want a plan each", result)
	}

	byContainer := make(map[string]domain.ChangePlan)
	for _, plan := range fake.plans() {
		if _, duplicate := byContainer[plan.ContainerID]; duplicate {
			t.Fatalf("container %s received two plans in one pass", plan.ContainerID)
		}
		byContainer[plan.ContainerID] = plan
	}

	if got := byContainer["container-a"]; got.RegistryStatus != domain.CheckUnsupported ||
		got.CurrentImage != unsupportedPortRef {
		t.Errorf("container-a read the wrong evidence: status %q, image %q",
			got.RegistryStatus, got.CurrentImage)
	}
	if got := byContainer["container-b"]; got.RegistryStatus != domain.CheckPending ||
		got.CurrentImage != unsupportedLiteralRef {
		t.Errorf("container-b read the wrong evidence: status %q, image %q",
			got.RegistryStatus, got.CurrentImage)
	}
}

// A container that declares NO image has no key at all, and must not read the
// evidence of anything else. This is the empty-string collision, directly.
func TestAContainerWithNoReferenceReadsNobodyElsesEvidence(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "nameless", ""),
		candidate("container-b", "also-nameless", ""),
	}
	// Evidence filed under the empty key. Nothing may find it.
	fake.intel[""] = unsupportedIntel("")

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 0 || result.Skipped != 2 {
		t.Errorf("result = %+v, want two skips and no plan built on an empty key", result)
	}
}

// An unsupported reference HarborMaster has not recorded at all is still
// skipped -- exactly as a supported one is. "Not looked at yet" and "cannot be
// looked at" are different states, and only the second has evidence to report.
func TestAnUnsupportedReferenceWithNoRecordIsSkipped(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "web", unsupportedMalformed),
		candidate("container-b", "empty", ""),
	}

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 0 || result.Skipped != 2 {
		t.Errorf("result = %+v, want two skips", result)
	}
}

// Supported and unsupported containers in one batch are assessed
// independently: the supported one still gets its real proposal, the
// unsupported one still gets none.
func TestAMixedBatchAssessesEachContainerIndependently(t *testing.T) {
	fake := oneContainerEstate()
	fake.candidates = append(fake.candidates,
		candidate("container-b", "internal-app", unsupportedPortRef))
	fake.intel[unsupportedPortRef] = unsupportedIntel(unsupportedPortRef)

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 2 {
		t.Fatalf("result = %+v, want a plan each", result)
	}

	byContainer := make(map[string]domain.ChangePlan)
	for _, plan := range fake.plans() {
		byContainer[plan.ContainerID] = plan
	}

	supported := byContainer["container-a"]
	if supported.ProposedImage != "nginx:1.27.1" || supported.ProposedDigest != planLatestDigest {
		t.Errorf("the supported container lost its proposal: %q -> %q",
			supported.CurrentImage, supported.ProposedImage)
	}
	if supported.RegistryStatus != domain.CheckOK {
		t.Errorf("the supported container's status = %q, want ok", supported.RegistryStatus)
	}

	unsupported := byContainer["container-b"]
	if unsupported.ProposedImage != "" || unsupported.ProposedDigest != "" {
		t.Errorf("the unsupported container gained a proposal: %q / %q",
			unsupported.ProposedImage, unsupported.ProposedDigest)
	}
	if unsupported.RegistryStatus != domain.CheckUnsupported {
		t.Errorf("the unsupported container's status = %q", unsupported.RegistryStatus)
	}
}

// THE CONTAINER LIST must not read the new plan as reassurance.
//
// Making the plan reachable put an unsupported container through the branch
// that reports UpdateNone as "up to date". It is not up to date: nothing was
// ever compared. This walks the plan the planner builds through the container
// list's own classifier and requires a non-answer.
func TestAnUnsupportedPlanIsNotRenderedAsUpToDate(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "internal-app", unsupportedPortRef),
	}
	fake.intel[unsupportedPortRef] = unsupportedIntel(unsupportedPortRef)

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	plan := fake.plans()[0]
	state := domain.AssessContainer(domain.ContainerEvidence{
		PlanKnown:      true,
		UpdateType:     plan.UpdateType,
		Recommendation: plan.Risk.Recommendation,
		ProposedImage:  plan.ProposedImage,
	}).State

	if state == domain.AttentionUpToDate {
		t.Error("a container whose image was never looked up is reported as up to date")
	}
	if state.NeedsOperator() {
		t.Errorf("attention state = %q, which puts an unactionable container on the "+
			"operator's list", state)
	}
	if state != domain.AttentionCannotAdvise {
		t.Errorf("attention state = %q, want %q", state, domain.AttentionCannotAdvise)
	}
}

// The unsupported case obeys the same duplicate suppression as every other. A
// second pass over an estate nothing has moved in writes nothing.
func TestASecondPassOverAnUnsupportedContainerWritesNothing(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "internal-app", unsupportedPortRef),
	}
	fake.intel[unsupportedPortRef] = unsupportedIntel(unsupportedPortRef)

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("first Generate: %v", err)
	}

	second, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if second.Generated != 0 || second.Unchanged != 1 {
		t.Errorf("second pass = %+v, want one unchanged and nothing written", second)
	}
	if len(fake.plans()) != 1 {
		t.Errorf("stored %d plans over two passes, want 1", len(fake.plans()))
	}
}

// Every registry outcome that is NOT unsupported keeps the behaviour it had.
// This is the guard against the fix collapsing several distinct states into
// one: each still reaches the plan as itself.
func TestEveryRegistryOutcomeReachesThePlanAsItself(t *testing.T) {
	cases := []struct {
		name   string
		status domain.CheckStatus
	}{
		{"pending", domain.CheckPending},
		{"failed", domain.CheckFailed},
		{"rate limited", domain.CheckRateLimited},
		{"unauthorized", domain.CheckUnauthorized},
		{"not found", domain.CheckNotFound},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := oneContainerEstate()
			intel := fake.intel["docker.io/library/nginx:1.27.0"]
			intel.Update = domain.UpdateNone
			intel.LatestTag = ""
			intel.LatestDigest = ""
			intel.RemoteDigest = ""
			intel.Status = testCase.status
			fake.intel["docker.io/library/nginx:1.27.0"] = intel

			planner := plannerAt(fake, plannerNow(t))
			result, err := planner.Generate(context.Background())
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if result.Generated != 1 {
				t.Fatalf("result = %+v, want a plan recording the gap", result)
			}

			plan := fake.plans()[0]
			if plan.RegistryStatus != testCase.status {
				t.Errorf("registry status = %q, want %q", plan.RegistryStatus, testCase.status)
			}
			if plan.RegistryStatus == domain.CheckUnsupported {
				t.Error("a lookup that happened was reported as one that cannot happen")
			}
			if plan.ProposedImage != "" || plan.ProposedDigest != "" {
				t.Errorf("a proposal was invented from an unusable lookup: %q / %q",
					plan.ProposedImage, plan.ProposedDigest)
			}
			if plan.Risk.Recommendation != domain.RecommendUnknown {
				t.Errorf("recommendation = %q, want unknown", plan.Risk.Recommendation)
			}
		})
	}
}
