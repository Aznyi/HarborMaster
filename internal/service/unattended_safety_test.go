package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The gates that must hold when nobody is watching.
//
// Every test here is a NEGATIVE, and negatives are the easy ones to get wrong:
// a test that asserts "nothing happened" passes just as well when the world was
// never set up. So each one starts from the rig that Scenario A proves DOES
// update -- same container, same registry answer, same plan -- and changes
// exactly one thing. If the gate under test stopped working, the update would
// go through, which is what makes the assertion mean something.

// runPassesAndSettle runs several decision passes and lets the follower work.
func runPassesAndSettle(rig *unattendedRig, passes int) {
	for i := 0; i < passes; i++ {
		rig.decide()
	}
}

// assertInert requires that nothing was acquired, executed, or done to the host.
func assertInert(t *testing.T, rig *unattendedRig, why string) {
	t.Helper()

	rig.awaitNoChange(why, func() bool {
		return rig.acquisitionCount() == 0 && rig.executionCount() == 0 &&
			rig.rollbackCount() == 0 && len(rig.host.operations()) == 0
	})

	// And the workload is bit-for-bit where it started. "No execution row
	// appeared" is not the same claim as "nothing happened to the host".
	live, present := rig.host.byName(c4cName)
	if !present || live.id != c4cContainerID || !live.running ||
		live.image != c4cCurrentRef {
		t.Errorf("%s: the workload changed anyway: %+v", why, live)
	}
}

// -------------------------- Scenario E: user policy outranks the fallback --

func TestScenarioEAUserPolicyOutranksAutomatic(t *testing.T) {
	for _, testCase := range []struct {
		name string
		mode domain.AutomationMode
	}{
		{"observe", domain.ModeObserve},
		{"approvalRequired", domain.ModeApprove},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// TWO policies: a broad automatic one at low priority, standing in
			// for the Simple Automatic fallback, and the operator's own at a
			// higher priority. The operator's must win.
			fallback := c4cAutomaticPolicy()
			fallback.PolicyID = "upd_fa11bac0000000000000"
			fallback.Name = "hm-c4c fallback automatic"
			fallback.Priority = 1
			fallback.Normalise()

			operator := c4cPolicyWithMode(testCase.mode)
			operator.Priority = 50

			rig := newUnattendedRig(t, func(o *rigOptions) {
				o.policies = []domain.UpdatePolicy{fallback, operator}
			})
			defer rig.stop()

			seedDiscovery(t, rig, domain.UpdateMinor)
			rig.start()
			runPassesAndSettle(rig, 3)

			assertInert(t, rig, "a higher-priority "+testCase.name+
				" policy must outrank the automatic fallback")
		})
	}
}

// ------------------------------- Scenario F: simple automatic switched off --

func TestScenarioFWithNoGoverningPolicyNothingMutates(t *testing.T) {
	// The fallback switched off is a deployment with no policy that selects
	// this container. Detection and planning continue -- an operator still
	// wants to be told -- and nothing acts.
	rig := newUnattendedRig(t)
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()
	runPassesAndSettle(rig, 3)

	assertInert(t, rig, "no governing policy means no automatic mutation")

	// Detection still happened, which is the half that must NOT stop.
	if _, err := rig.db.Plans.Current(context.Background(), c4cContainerID); err != nil {
		t.Errorf("planning stopped when the fallback was off: %v", err)
	}
	assertOneOf(t, rig, domain.EventUpdateDiscovered)
}

// -------------------------------------- Scenario G: the external opt-out --

// TestScenarioGTheOptOutLabelsWinOverEverything covers BOTH canonical labels.
//
// They are different instructions and are read in different places, and this
// test exists partly because assuming they were one thing was wrong:
//
//   - `io.harbormaster.enabled=false` is the ESTATE opt-out. It is read by
//     domain.ScreenTarget and decides ELIGIBILITY -- whether a broad policy
//     may enrol the container at all. It is what invariant 11 means by "never
//     enrolled".
//   - `io.harbormaster.update.enabled=false` is the UPDATE opt-out. It is read
//     by domain.Resolve and disables the governing policy even when an
//     operator named the container explicitly.
//
// Both must stop every mutation, and neither may be quietly lost across a
// restart -- they live on the container, not in HarborMaster's state.
func TestScenarioGTheOptOutLabelsWinOverEverything(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		label  string
		policy func() domain.UpdatePolicy
	}{{
		name:  "the estate opt-out against a broad policy",
		label: domain.LabelHarborMasterEnabled,
		// The FALLBACK shape: a policy that enrols by eligibility rather than
		// by name, which is what Simple Automatic Updates is.
		policy: func() domain.UpdatePolicy {
			policy := c4cAutomaticPolicy()
			policy.Scope = domain.ScopeAllEligible
			policy.Selector = domain.UpdateSelector{}
			policy.Normalise()
			return policy
		},
	}, {
		name:  "the update opt-out against a policy naming the container",
		label: domain.LabelUpdateEnabled,
		// An operator who named this container deliberately. The update label
		// still wins: it is the container's own owner saying no.
		policy: c4cAutomaticPolicy,
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			rig := newUnattendedRig(t, func(o *rigOptions) {
				o.policies = []domain.UpdatePolicy{testCase.policy()}
				o.labels = map[string]string{testCase.label: "false"}
			})
			defer rig.stop()

			seedDiscovery(t, rig, domain.UpdateMinor)
			rig.start()
			runPassesAndSettle(rig, 5)

			assertInert(t, rig, testCase.label+"=false must stop every update")

			// A restart does not forget. The label lives on the container and is
			// re-read by every inventory refresh; it is not state HarborMaster
			// could lose.
			rig.restart(rigOptions{})
			rig.refreshInventory()
			runPassesAndSettle(rig, 3)
			assertInert(t, rig, testCase.label+"=false must survive a restart")
		})
	}
}

// ----------------------------------------------- Scenario H: self update --

func TestScenarioHHarborMasterNeverUpdatesItself(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
		// HarborMaster believes it IS this container. Every one of the four
		// independent signals would do; the id is the strongest.
		o.selfIdentity = domain.SelfIdentity{
			ContainerID:   c4cContainerID,
			ContainerName: c4cName,
			ImageRef:      c4cCurrentRef,
			Source:        domain.SelfSourceConfigured,
		}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()
	runPassesAndSettle(rig, 5)

	assertInert(t, rig, "HarborMaster must never recreate its own container")
}

// ------------------------------ Scenario I: stale or unknown evidence --

func TestScenarioIUnprovenRegistryEvidenceNeverMutates(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status domain.CheckStatus
	}{
		{"pending", domain.CheckPending},
		{"failed", domain.CheckFailed},
		{"unauthorized", domain.CheckUnauthorized},
		{"rateLimited", domain.CheckRateLimited},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rig := newUnattendedRig(t, func(o *rigOptions) {
				o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
			})
			defer rig.stop()

			rig.refreshInventory()
			rig.syncIntelReferences()
			// No positive evidence: the registry never answered usefully.
			rig.publishUpdate(domain.UpdateUnknown, testCase.status)
			rig.evaluateCompliance()
			rig.plan()

			rig.start()
			runPassesAndSettle(rig, 3)

			assertInert(t, rig,
				"a "+testCase.name+" registry check establishes no target")

			// B1.1, restated: the absence of positive evidence is UNKNOWN, and
			// unknown is not "up to date". A plan that reported no update
			// available here would be asserting something nothing established.
			plan, err := rig.db.Plans.Current(context.Background(), c4cContainerID)
			if err == nil && plan.UpdateType == domain.UpdateNone {
				t.Errorf("a %s check produced updateType=none.\n\n"+
					"Nothing established that the container is current. "+
					"Reporting it as up to date is the failure mode B1.1 exists "+
					"to prevent.", testCase.name)
			}
		})
	}
}

// --------------------------------------- Scenario K: the pause gate --

func TestScenarioKAPausedWorkloadIsNotTouchedUntilResumed(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)

	// Paused by an operator, through the engine's own path.
	if _, err := rig.automation.PauseContainer(context.Background(), c4cName,
		"held while we investigate",
		service.Actor{UserID: "usr_0011223344556677889a", Username: "colby"},
	); err != nil {
		t.Fatalf("pause: %v", err)
	}

	rig.start()
	runPassesAndSettle(rig, 3)
	assertInert(t, rig, "a paused workload must not be updated")

	// Cleared. The scheduler continues by itself: no manual mutation request is
	// needed to pick the work back up, which is the half of a pause that is
	// easy to get wrong.
	if err := rig.automation.Resume(context.Background(), c4cName,
		domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"},
		service.Actor{}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	rig.decide()
	rig.await("the update to run after the pause was cleared", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	if got := rig.terminalExecution().State; got != domain.ExecutionSucceeded {
		t.Errorf("after resuming, the recreation ended %q", got)
	}
}
