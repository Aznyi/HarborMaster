package arch_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/service"
)

// Architecture tests for snapshot assurance.
//
// # The invariant these hold
//
//	The snapshot/readiness evidence used to authorise an update must be part of
//	the immutable evidence of the ChangePlan that is executed.
//
// Phase 17.2 gave HarborMaster the ability to capture a container's
// configuration snapshot by itself, so an operator does not have to snapshot
// every container by hand before automatic updates can work. The obvious
// implementation -- capture a snapshot at the moment the pipeline discovers it
// needs one, and let the plan already in flight proceed -- is the one that must
// stay impossible: a plan whose risk assessment was computed against "no
// baseline exists" would then be executed as though it had been computed
// against a baseline that did exist.
//
// Six rules, each a separate test:
//
//  1. Assurance holds no Docker capability. It reaches one capture method,
//     which reads HarborMaster's own container repository.
//  2. Its source names no Docker capability, SDK, or operation verb.
//  3. It cannot submit an acquisition, an execution, or a rollback.
//  4. It cannot construct or write a ChangePlan.
//  5. Change plans remain insert-only: the planner's own store interface has no
//     method that updates one.
//  6. The composition root still supplies assurance to the execution service,
//     so the staleness check cannot be silently unwired.

// assuranceSourcePrefixes are the files this subsystem owns.
var assuranceSourcePrefixes = []string{"snapshot_assurance"}

// assuranceSources reads assurance's non-test source.
//
// Fails when it finds nothing, so renaming the files turns this file into a
// build failure rather than into a suite of tests that check nothing.
func assuranceSources(t *testing.T) map[string]string {
	t.Helper()

	root := moduleRoot(t)
	serviceDir := filepath.Join(root, "internal", "service")
	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		t.Fatalf("read internal/service: %v", err)
	}

	sources := make(map[string]string, 4)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		owned := false
		for _, prefix := range assuranceSourcePrefixes {
			if strings.HasPrefix(name, prefix) {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		source, readErr := os.ReadFile(filepath.Join(serviceDir, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		sources[name] = string(source)
	}
	// Non-vacuity. Two files are expected: the assurance service and the
	// preparer. If they are renamed or removed, every text check below would
	// iterate an empty map and pass.
	if len(sources) < 2 {
		t.Fatalf("found %d snapshot assurance sources, want at least 2; "+
			"this test is not looking at the subsystem it names", len(sources))
	}
	return sources
}

// TestSnapshotAssuranceHoldsNoDockerCapability is rule 1.
//
// Both options structs, because the preparer holds the assurance and a
// capability on either would be a capability assurance could reach.
func TestSnapshotAssuranceHoldsNoDockerCapability(t *testing.T) {
	forbidden := []string{
		"docker.Runtime",
		"docker.ImageAcquirer",
		"docker.ContainerMutator",
		"docker.ContainerRollbacker",
		"docker.ConfigCapturer",
		"docker.Client",
	}

	for _, options := range []any{
		service.SnapshotAssuranceOptions{},
		service.SnapshotPreparerOptions{},
	} {
		optionsType := reflect.TypeOf(options)
		for i := 0; i < optionsType.NumField(); i++ {
			field := optionsType.Field(i)
			rendered := field.Type.String()
			for _, name := range forbidden {
				if !strings.Contains(rendered, name) {
					continue
				}
				t.Errorf("service.%s.%s is a %s\n"+
					"\tsnapshot assurance reads HarborMaster's own container repository and "+
					"never a socket. A capability here would make an HTTP-triggered or "+
					"timer-triggered capture able to generate privileged traffic.",
					optionsType.Name(), field.Name, rendered)
			}
		}
	}
}

// TestAssuranceSourceNamesNoDockerCapability is rule 2, at the file level.
//
// Text rather than reflection, and separate from the test above: a capability
// reached through a local variable, a type assertion, or a helper would satisfy
// the struct check and fail this one.
func TestAssuranceSourceNamesNoDockerCapability(t *testing.T) {
	forbidden := []string{
		"docker.Runtime", "docker.ImageAcquirer", "docker.ContainerMutator",
		"docker.ContainerRollbacker", "docker.ConfigCapturer",
		"docker.PullRequest", "docker.RecreateRequest", "docker.RollbackRequest",
		"github.com/moby/moby",
	}

	for name, source := range assuranceSources(t) {
		for _, symbol := range forbidden {
			if strings.Contains(source, symbol) {
				t.Errorf("internal/service/%s names %s\n"+
					"\tsnapshot assurance holds no Docker capability", name, symbol)
			}
		}
	}
}

// TestAssuranceCannotSubmitPipelineWork is rule 3.
//
// Assurance produces EVIDENCE. It must not be able to start a download, a
// recreation, or a rollback -- if it could, "capture a snapshot" and "change the
// host" would be one call away from each other.
func TestAssuranceCannotSubmitPipelineWork(t *testing.T) {
	forbidden := []string{
		"AcquisitionRequest", "ExecutionRequest", "RollbackRequest",
		"AutomationPipeline", "RequestAcquisition", "RequestExecution",
		"RequestRollback",
	}

	for name, source := range assuranceSources(t) {
		for _, symbol := range forbidden {
			if strings.Contains(source, symbol) {
				t.Errorf("internal/service/%s names %s\n"+
					"\tassurance creates evidence and submits no work; a path from here to "+
					"the pipeline would be a second way to reach the host", name, symbol)
			}
		}
	}
}

// TestAssuranceCannotWriteAPlan is rule 4, and the load-bearing one.
//
// Assurance may CREATE snapshot evidence. It may not put that evidence into a
// plan, because a plan is immutable and the evidence it was assessed against is
// what authorises the change. A single call to InsertPlans from here would be
// the invariant gone.
func TestAssuranceCannotWriteAPlan(t *testing.T) {
	forbidden := []string{
		"InsertPlans", "ChangePlan", "PlanInputs", "NewPlanID",
		"change_plans", "InputDigest",
	}

	for name, source := range assuranceSources(t) {
		for _, symbol := range forbidden {
			if strings.Contains(source, symbol) {
				t.Errorf("internal/service/%s names %s\n"+
					"\tsnapshot assurance must not reach a change plan. The evidence a plan "+
					"was assessed against is immutable, and a snapshot captured later must "+
					"never be read into a plan that predates it.", name, symbol)
			}
		}
	}
}

// TestChangePlansRemainInsertOnly is rule 5.
//
// The planner's own store interface is the whole surface by which a plan can be
// persisted, and it has no update. Checked by reflection over the interface
// rather than by reading SQL, because a method added here is how the capability
// would arrive.
func TestChangePlansRemainInsertOnly(t *testing.T) {
	planStore := reflect.TypeOf((*service.PlanStore)(nil)).Elem()

	// Non-vacuity: if the interface moved or was emptied, the loop below would
	// check nothing.
	if planStore.NumMethod() < 4 {
		t.Fatalf("service.PlanStore has %d methods; this test is not looking at "+
			"the interface it names", planStore.NumMethod())
	}

	forbidden := []string{"Update", "Set", "Patch", "Amend", "Modify", "Rewrite"}
	for i := 0; i < planStore.NumMethod(); i++ {
		method := planStore.Method(i)
		for _, verb := range forbidden {
			if strings.HasPrefix(method.Name, verb) {
				t.Errorf("service.PlanStore.%s would let a plan be changed after it was written\n"+
					"\tplans are insert-only. That is what makes a plan id a durable reference "+
					"to one assessment, and what lets an execution bind to the evidence an "+
					"operator actually reviewed.", method.Name)
			}
		}
	}
}

// TestTheCompositionRootStillSuppliesAssurance is rule 6.
//
// The staleness check in the execution preflight is guarded on assurance being
// non-nil, so a build that stopped wiring it would lose the check silently and
// every existing test would still pass. This is the same shape as the guard
// that pins the self-identity wiring.
func TestTheCompositionRootStillSuppliesAssurance(t *testing.T) {
	root := moduleRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "cmd", "harbormaster", "main.go"))
	if err != nil {
		t.Fatalf("read composition root: %v", err)
	}
	main := string(source)

	// Non-vacuity: the execution service must still be constructed here at all.
	if !strings.Contains(main, "service.NewExecutionService(") {
		t.Fatal("the composition root no longer constructs the execution service; " +
			"this test is not looking at what it thinks it is")
	}

	for _, required := range []string{
		// Assurance is built.
		"service.NewSnapshotAssurance(",
		// The preparer is built and handed to the planner, so baselines are
		// captured BEFORE a plan is written.
		"service.NewSnapshotPreparer(",
		"Prepare:",
		// And assurance reaches the execution service, which is where the
		// staleness check lives.
		"Assurance:",
	} {
		if !strings.Contains(main, required) {
			t.Errorf("the composition root no longer contains %q\n"+
				"\tsnapshot assurance is guarded on being non-nil at both call sites, so "+
				"un-wiring it removes the pre-plan baseline capture and the pre-recreation "+
				"staleness check without failing anything else", required)
		}
	}
}

// TestThePreparerCannotDecideWhetherTheHostChanges.
//
// The preparer reads policies to work out which containers are worth
// snapshotting. That is a SELECTION question, and it must not become an
// authorisation one: nothing here may reach a verdict, a decision, or a budget.
func TestThePreparerCannotDecideWhetherTheHostChanges(t *testing.T) {
	forbidden := []string{
		"DecideAutomation", "AutomationDecision", "AutomationBudget",
		"VerdictUpdate", "AutomationOutcome",
	}

	sources := assuranceSources(t)
	preparer, found := sources["snapshot_assurance_prepare.go"]
	if !found {
		t.Fatal("snapshot_assurance_prepare.go is not where this test expects it")
	}

	for _, symbol := range forbidden {
		if strings.Contains(preparer, symbol) {
			t.Errorf("snapshot_assurance_prepare.go names %s\n"+
				"\tthe preparer decides what to SNAPSHOT, never what to update. Reaching "+
				"the decision function from here would make snapshot preparation a second "+
				"place the automation verdict is computed.", symbol)
		}
	}
}
