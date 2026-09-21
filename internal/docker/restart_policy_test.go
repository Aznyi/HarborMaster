package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The restart-policy capability.
//
// Two typed operations and nothing more general. SuspendRestart may only act
// on a container HarborMaster has parked or quarantined -- one whose name
// carries a HarborMaster marker -- and only ever writes "no". RestoreRestart
// may only act on a container under a production name, and only writes a
// policy from the closed vocabulary. Neither is a way to reconfigure an
// arbitrary container.

const rpParked = "web" + domain.ParkedNameSuffix + "exec_00112233445566778899"

func TestRestartPolicyRequestsTargetAFullContainerID(t *testing.T) {
	if err := (SuspendRestartRequest{ContainerID: testContainerID}).Validate(); err != nil {
		t.Fatalf("a suspend naming a full id was refused: %v", err)
	}
	if err := (SuspendRestartRequest{ContainerID: "abc123"}).Validate(); err == nil {
		t.Error("a suspend with a short id was accepted")
	}
	if err := (SuspendRestartRequest{}).Validate(); err == nil {
		t.Error("a suspend with no id was accepted")
	}

	valid := RestoreRestartRequest{ContainerID: testContainerID, Policy: domain.RestartPolicy{Name: "always"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a restore naming a full id and a legal policy was refused: %v", err)
	}
	for name, request := range map[string]RestoreRestartRequest{
		"short id":              {ContainerID: "abc123", Policy: domain.RestartPolicy{Name: "always"}},
		"unknown policy":        {ContainerID: testContainerID, Policy: domain.RestartPolicy{Name: "sometimes"}},
		"empty policy":          {ContainerID: testContainerID},
		"retries without cause": {ContainerID: testContainerID, Policy: domain.RestartPolicy{Name: "always", MaximumRetryCount: 3}},
		"negative retries":      {ContainerID: testContainerID, Policy: domain.RestartPolicy{Name: "on-failure", MaximumRetryCount: -1}},
	} {
		if err := request.Validate(); err == nil {
			t.Errorf("%s: the request was accepted", name)
		}
	}
}

// TestOnlyAParkedContainerMayHaveItsRestartSuspended pins the ownership check
// the adapter makes before it writes anything: the target must carry one of
// HarborMaster's own name markers.
func TestOnlyAParkedContainerMayHaveItsRestartSuspended(t *testing.T) {
	for _, name := range []string{
		rpParked,
		"web" + domain.QuarantineNameSuffix + "exec_00112233445566778899",
		"web" + domain.RollbackParkedNameSuffix + "rbk_0123456789abcdef0123",
	} {
		if !restartSuspendable(name) {
			t.Errorf("%q is a HarborMaster-derived name and was not accepted", name)
		}
		if restartRestorable(name) {
			t.Errorf("%q is a HarborMaster-derived name and was accepted for a restore", name)
		}
	}
	for _, name := range []string{"web", "api.internal", "hm-old-web", ""} {
		if restartSuspendable(name) {
			t.Errorf("%q carries no HarborMaster marker and was accepted for a suspend", name)
		}
	}
	if !restartRestorable("web") {
		t.Error("a production name was refused for a restore")
	}
	if restartRestorable("") {
		t.Error("an empty name was accepted for a restore")
	}
}

func TestTheFakeMutatorEnforcesTheSameOwnershipRule(t *testing.T) {
	fake := NewFakeMutator()
	production := &FakeContainer{ID: FakeContainerID(1), Name: "web"}
	production.Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
	parked := &FakeContainer{ID: FakeContainerID(2), Name: rpParked}
	parked.Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
	fake.AddContainer(production)
	fake.AddContainer(parked)

	err := fake.SuspendRestart(context.Background(), SuspendRestartRequest{ContainerID: production.ID})
	if !errors.Is(err, ErrMutationRefused) {
		t.Fatalf("suspending a production container gave %v, want a refusal", err)
	}
	if production.Detail.Overview.RestartPolicy.Name != "always" {
		t.Error("a refused suspend changed the policy anyway")
	}

	if err := fake.SuspendRestart(context.Background(), SuspendRestartRequest{ContainerID: parked.ID}); err != nil {
		t.Fatalf("suspending a parked container failed: %v", err)
	}
	if parked.Detail.Overview.RestartPolicy.Name != "no" {
		t.Errorf("the parked container's policy is %q, want no", parked.Detail.Overview.RestartPolicy.Name)
	}

	err = fake.RestoreRestart(context.Background(), RestoreRestartRequest{
		ContainerID: parked.ID, Policy: domain.RestartPolicy{Name: "always"},
	})
	if !errors.Is(err, ErrMutationRefused) {
		t.Fatalf("restoring a policy onto a parked container gave %v, want a refusal", err)
	}
	if err := fake.RestoreRestart(context.Background(), RestoreRestartRequest{
		ContainerID: production.ID, Policy: domain.RestartPolicy{Name: "on-failure", MaximumRetryCount: 2},
	}); err != nil {
		t.Fatalf("restoring a policy onto a production container failed: %v", err)
	}
	if got := production.Detail.Overview.RestartPolicy; got.Name != "on-failure" || got.MaximumRetryCount != 2 {
		t.Errorf("the production container's policy is %+v, want on-failure:2", got)
	}
}
