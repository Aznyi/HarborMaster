package domain

import (
	"testing"
	"time"
)

// A rebind plan must be STORABLE.
//
// # Why this is its own test
//
// Every other property of a rebind plan is about what it proposes. This one is
// about whether it can be written down at all, and it is the property that
// failed live: the plan was correct in every respect and the schema refused it,
// so the reattachment could never happen and the provider had already been
// replaced.
//
// The enumerated columns on `change_plans` each carry a CHECK. A plan field
// whose Go zero value is not a member of the corresponding vocabulary produces
// a plan that is valid to the type system and impossible to persist -- which
// surfaces as a follower retrying forever rather than as an error anybody sees.

// rebindFixture returns a candidate and evidence that produce a plan.
func rebindFixture(t *testing.T) (RebindEvidence, RebindCandidate) {
	t.Helper()

	const providerID = "1111111111111111111111111111111111111111111111111111111111111111"
	const digest = "sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1"

	evidence, ok := RebindEvidenceFrom(DependencyProblem{
		Container:    "sonarr",
		Source:       DependencyNetworkNamespace,
		ReferencedID: providerID,
		Refusal:      DiscoveryUnknownContainer,
	}, "gluetun", time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("the evidence fixture did not construct; this test would prove nothing")
	}

	candidate := RebindCandidate{
		Name:               "sonarr",
		Provider:           "gluetun",
		ContainerID:        "2222222222222222222222222222222222222222222222222222222222222222",
		ImageRef:           "alpine:3.22.1",
		RunningReference:   "alpine:3.22.1",
		RunningDigest:      digest,
		Present:            true,
		Recreatable:        true,
		NamespacesObserved: true,
		Evidence:           evidence,
	}
	return evidence, candidate
}

// Every enumerated field the schema constrains comes out inside its vocabulary.
//
// Asserted against the domain's own validators rather than against a database,
// so the check lives beside the function that fills the fields. The database
// half is TestEveryUpdateTypeIsAcceptedByTheSchema in internal/store.
func TestARebindPlanIsStorableWithNoRegistryRecord(t *testing.T) {
	t.Parallel()

	evidence, candidate := rebindFixture(t)

	// PlanInputs as the coordinator assembles them when the dependent's image
	// intelligence record is NOT in the batch -- the case that failed live.
	inputs := PlanInputs{
		ContainerID:   candidate.ContainerID,
		ContainerName: candidate.Name,
		CurrentDigest: candidate.RunningDigest,
	}

	plan, refusal := BuildRebindPlan(evidence, candidate, SelfIdentity{}, inputs,
		NewPlanID(), time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if refusal != RebindRefusalNone {
		t.Fatalf("the plan was refused: %q", refusal)
	}

	if plan.RegistryStatus == "" {
		t.Error("the plan carries an empty registry status\n\n" +
			"The zero CheckStatus is not a member of the stored vocabulary, so " +
			"the schema refuses the row. A refused plan insert is not a failed " +
			"request: the dependency follower logs it and retries, so this ships " +
			"as a reattachment that can never happen while the provider has " +
			"already been replaced.")
	}
	if !ValidCheckStatus(string(plan.RegistryStatus)) {
		t.Errorf("registry status %q is not in the vocabulary", plan.RegistryStatus)
	}
	if !ValidReadinessStatus(string(plan.RestoreReadiness)) {
		t.Errorf("restore readiness %q is not in the vocabulary", plan.RestoreReadiness)
	}
	if !ValidRiskBand(string(plan.Risk.Band)) {
		t.Errorf("risk band %q is not in the vocabulary", plan.Risk.Band)
	}
	if !ValidRecommendation(string(plan.Risk.Recommendation)) {
		t.Errorf("recommendation %q is not in the vocabulary", plan.Risk.Recommendation)
	}
	if !ValidUpdateType(string(plan.UpdateType)) {
		t.Errorf("update type %q is not in the vocabulary", plan.UpdateType)
	}
}

// A real registry record is carried through unchanged.
//
// The default above must not become a blanket overwrite: when the dependent's
// intelligence record IS available, the plan reports what it actually says.
func TestARebindPlanKeepsARealRegistryStatus(t *testing.T) {
	t.Parallel()

	evidence, candidate := rebindFixture(t)
	inputs := PlanInputs{
		ContainerID:    candidate.ContainerID,
		ContainerName:  candidate.Name,
		CurrentDigest:  candidate.RunningDigest,
		RegistryStatus: CheckRateLimited,
		RegistryDetail: "the registry asked HarborMaster to slow down",
	}

	plan, refusal := BuildRebindPlan(evidence, candidate, SelfIdentity{}, inputs,
		NewPlanID(), time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if refusal != RebindRefusalNone {
		t.Fatalf("the plan was refused: %q", refusal)
	}
	if plan.RegistryStatus != CheckRateLimited {
		t.Errorf("registry status = %q, want the real record %q",
			plan.RegistryStatus, CheckRateLimited)
	}
}

// The reattachment still proposes exactly what it proposed before.
//
// A guard on the fix itself: defaulting one telemetry field must not have
// touched the image fields, which are the whole point of a rebind.
func TestTheRegistryDefaultDoesNotTouchTheImageFields(t *testing.T) {
	t.Parallel()

	evidence, candidate := rebindFixture(t)
	plan, refusal := BuildRebindPlan(evidence, candidate, SelfIdentity{}, PlanInputs{
		ContainerID:   candidate.ContainerID,
		ContainerName: candidate.Name,
	}, NewPlanID(), time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if refusal != RebindRefusalNone {
		t.Fatalf("the plan was refused: %q", refusal)
	}

	if plan.CurrentImage != plan.ProposedImage {
		t.Errorf("a rebind moved the image: %q -> %q", plan.CurrentImage, plan.ProposedImage)
	}
	if plan.ProposedDigest != candidate.RunningDigest {
		t.Errorf("the pinned digest is %q, want the running one %q",
			plan.ProposedDigest, candidate.RunningDigest)
	}
	if plan.UpdateType != UpdateRebind {
		t.Errorf("update type = %q", plan.UpdateType)
	}
}
