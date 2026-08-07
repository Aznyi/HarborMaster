package arch_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/service"
)

// Architecture tests for the automation engine.
//
// # What Phase 11 changed, and what it must not have changed
//
// Phase 11 is the first subsystem that can cause the host to change with nobody
// watching. It introduced NO new Docker capability to do so: the engine submits
// the same three request types an operator's HTTP request produces, to the same
// three services, each of which owns its capability and re-runs its own
// preflight against the live host.
//
// These tests are what make that a property the build checks rather than a
// claim the comments make. Four rules:
//
//  1. The automation service holds no Docker capability interface. Not a
//     Runtime, not an ImageAcquirer, not a ContainerMutator, not a
//     ContainerRollbacker.
//  2. The automation source names no Docker capability, no SDK, and no Docker
//     operation verb.
//  3. Its whole mutation surface is one interface with exactly five methods:
//     three request submissions and two record reads.
//  4. No type it accepts from a caller has anywhere to put a container id, an
//     image, a tag, a digest, or a registry.
//
// Rule 4 is the one that matters most. Every other rule could be satisfied by
// an engine that faithfully forwarded an attacker's chosen image; this one says
// there is no field to forward.

// automationSourcePrefixes are the files this subsystem owns.
var automationSourcePrefixes = []string{"automation", "update_policy"}

// automationSources reads the engine's non-test source.
func automationSources(t *testing.T) map[string]string {
	t.Helper()

	root := moduleRoot(t)
	serviceDir := filepath.Join(root, "internal", "service")
	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		t.Fatalf("read internal/service: %v", err)
	}

	sources := make(map[string]string, 8)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		owned := false
		for _, prefix := range automationSourcePrefixes {
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
	if len(sources) == 0 {
		t.Fatal("no automation sources found; this test is not looking at the subsystem it names")
	}
	return sources
}

// TestAutomationOptionsHoldNoDockerCapability is the central rule.
//
// If somebody adds a docker.Runtime to AutomationOptions "just to check the
// daemon is reachable", this fails. That is the point: the engine's inability
// to reach a socket except through a service is the whole safety argument, and
// a field is how it would be lost.
func TestAutomationOptionsHoldNoDockerCapability(t *testing.T) {
	optionsType := reflect.TypeOf(service.AutomationOptions{})

	// Every Docker capability interface, by name. Compared against the STRING
	// of each field's type so a nested or aliased one is still caught.
	forbidden := []string{
		"docker.Runtime",
		"docker.ImageAcquirer",
		"docker.ContainerMutator",
		"docker.ContainerRollbacker",
		"docker.ConfigCapturer",
		"docker.Client",
	}

	for i := 0; i < optionsType.NumField(); i++ {
		field := optionsType.Field(i)
		rendered := field.Type.String()
		for _, name := range forbidden {
			if !strings.Contains(rendered, name) {
				continue
			}
			t.Errorf("service.AutomationOptions.%s is a %s\n"+
				"\tthe automation engine must reach Docker ONLY by submitting a request to a "+
				"service that owns a capability and runs its own preflight; a capability here "+
				"would let a scheduled pass act without one",
				field.Name, rendered)
		}
	}
}

// TestAutomationSourceNamesNoDockerCapability is the same rule at the file
// level.
//
// Deliberately text rather than reflection, and deliberately separate from the
// test above. A capability reached through a local variable, a type assertion,
// or a helper would satisfy the struct check and fail this one.
func TestAutomationSourceNamesNoDockerCapability(t *testing.T) {
	forbidden := []string{
		"docker.Runtime", "docker.ImageAcquirer", "docker.ContainerMutator",
		"docker.ContainerRollbacker", "docker.ConfigCapturer",
		"docker.PullRequest", "docker.RecreateRequest", "docker.RollbackRequest",
		"github.com/moby/moby",
	}

	for name, source := range automationSources(t) {
		for _, symbol := range forbidden {
			if strings.Contains(source, symbol) {
				t.Errorf("internal/service/%s names %s\n"+
					"\tthe automation engine holds no Docker capability; it submits requests to "+
					"the acquisition, execution, and rollback services, which own theirs",
					name, symbol)
			}
		}
	}
}

// automationForbiddenVerbs are Docker operations the engine must never perform
// itself.
//
// Every one of these is something a service already does behind its own
// preflight. The engine asking for one directly would be the engine having
// found a second path to the host.
var automationForbiddenVerbs = []string{
	"PullImage", "CreateContainer", "StartContainer", "StopContainer",
	"RemoveContainer", "RenameContainer", "InspectContainer",
	"ContainerCreate", "ContainerStart", "ContainerStop", "ContainerRemove",
	"ContainerRename", "ContainerExecCreate", "ImagePull", "ImageRemove",
}

// TestAutomationPerformsNoDockerOperation fails if the engine names one.
func TestAutomationPerformsNoDockerOperation(t *testing.T) {
	for name, source := range automationSources(t) {
		for _, verb := range automationForbiddenVerbs {
			if strings.Contains(source, verb) {
				t.Errorf("internal/service/%s names %s\n"+
					"\tautomation orchestrates the existing pipeline; it does not operate on "+
					"Docker itself, and a call like this would bypass a preflight", name, verb)
			}
		}
	}
}

// TestTheAutomationPipelineIsExactlyFiveMethods pins the mutation surface.
//
// Three request submissions and two record reads, plus the three Enabled
// reports that are pure predicates. If this needs editing, the change under
// review is automation gaining a new way to affect the host.
func TestTheAutomationPipelineIsExactlyFiveMethods(t *testing.T) {
	pipelineType := reflect.TypeOf((*service.AutomationPipeline)(nil)).Elem()

	want := map[string]bool{
		// Predicates. Cannot cause anything.
		"AcquisitionEnabled": true,
		"ExecutionEnabled":   true,
		"RollbackEnabled":    true,
		// The three submissions. Each takes a request whose fields are an
		// identifier HarborMaster generated and an idempotency key.
		"RequestAcquisition": true,
		"RequestExecution":   true,
		"RequestRollback":    true,
		// The two reads the follower needs to know what happened.
		"Acquisition": true,
		"Execution":   true,
	}

	got := make(map[string]bool, pipelineType.NumMethod())
	for i := 0; i < pipelineType.NumMethod(); i++ {
		got[pipelineType.Method(i).Name] = true
	}

	for name := range got {
		if !want[name] {
			t.Errorf("service.AutomationPipeline gained method %q\n"+
				"\tthis interface is the WHOLE of automation's ability to affect the host; "+
				"a new method needs its own review and its own threat model entry", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("service.AutomationPipeline no longer has method %q; update this test "+
				"if the removal is intended", name)
		}
	}
}

// TestAutomationEvidenceCannotWrite pins the read side.
//
// A decision pass reads the world through this interface. Every method returns
// data; none accepts a change. An engine that could write through its evidence
// source would be an engine whose "read the world" step could modify it.
func TestAutomationEvidenceCannotWrite(t *testing.T) {
	evidenceType := reflect.TypeOf((*service.AutomationEvidence)(nil)).Elem()

	writeVerbs := []string{
		"Create", "Update", "Delete", "Remove", "Set", "Write", "Insert",
		"Record", "Save", "Store", "Pause", "Resume", "Request", "Submit",
	}
	for i := 0; i < evidenceType.NumMethod(); i++ {
		name := evidenceType.Method(i).Name
		for _, verb := range writeVerbs {
			if strings.HasPrefix(name, verb) {
				t.Errorf("service.AutomationEvidence has method %q\n"+
					"\tthe evidence source is how a pass READS the world; a write here would "+
					"let the decision step change what it is deciding about", name)
			}
		}
	}
}

// TestNoAutomationInputCarriesACallerChosenTarget is the rule that matters
// most.
//
// Invariant 10: nothing that changes the host takes its target from a caller.
// Automation is the newest way to change the host, so every type it accepts is
// checked for a field that could hold one.
//
// The three request types it submits are checked too, even though they predate
// this phase, because the whole safety argument is that automation adds no new
// field to them.
func TestNoAutomationInputCarriesACallerChosenTarget(t *testing.T) {
	// Field names that would let a caller aim a mutation. Compared
	// case-insensitively against the whole field name, so `TargetImage`,
	// `imageRef`, and `Digest` are all caught.
	aimable := []string{
		"image", "digest", "tag", "registry", "repository", "reference",
		"containerid", "target",
	}

	// The types a caller's input reaches. AutomationInput is the decision
	// function's parameter and DOES carry identities -- but every one of them
	// is filled from the inventory and the planner by the engine itself, never
	// from a request, which is why it is not in this list. What is in this list
	// is everything that crosses the request boundary.
	subjects := []struct {
		name  string
		value any
	}{
		{"service.AcquisitionRequest", service.AcquisitionRequest{}},
		{"service.ExecutionRequest", service.ExecutionRequest{}},
		{"service.RollbackRequest", service.RollbackRequest{}},
	}

	for _, subject := range subjects {
		subjectType := reflect.TypeOf(subject.value)
		for i := 0; i < subjectType.NumField(); i++ {
			field := strings.ToLower(subjectType.Field(i).Name)
			for _, forbidden := range aimable {
				if !strings.Contains(field, forbidden) {
					continue
				}
				t.Errorf("%s has field %q\n"+
					"\tno request that can change the host may carry its own target; every "+
					"identity must be derived from a record HarborMaster wrote and re-verified "+
					"against the live host before acting",
					subject.name, subjectType.Field(i).Name)
			}
		}
	}
}

// TestAutomationSubmitsOnlyIdentifiersItGenerated checks the request shapes.
//
// An acquisition request names a PLAN. An execution request names an
// ACQUISITION. A rollback request names an EXECUTION. Each of those is an
// identifier HarborMaster generated from its own entropy source, and each of
// those records was written by HarborMaster from evidence it gathered.
//
// The chain is what makes automation safe, and this test says the chain has no
// link a caller could insert themselves into.
func TestAutomationSubmitsOnlyIdentifiersItGenerated(t *testing.T) {
	expected := map[string][]string{
		"service.AcquisitionRequest": {"PlanID", "RequestKey", "RequestedBy"},
		"service.ExecutionRequest":   {"AcquisitionID", "RequestKey", "RequestedBy"},
		"service.RollbackRequest":    {"ExecutionID", "RequestKey", "RequestedBy"},
	}
	subjects := map[string]any{
		"service.AcquisitionRequest": service.AcquisitionRequest{},
		"service.ExecutionRequest":   service.ExecutionRequest{},
		"service.RollbackRequest":    service.RollbackRequest{},
	}

	for name, want := range expected {
		subjectType := reflect.TypeOf(subjects[name])
		if got := subjectType.NumField(); got != len(want) {
			t.Errorf("%s has %d fields, want exactly %d (%s)\n"+
				"\ta fourth field on a request that changes the host is a fourth thing a "+
				"caller can influence", name, got, len(want), strings.Join(want, ", "))
			continue
		}
		for index, fieldName := range want {
			if got := subjectType.Field(index).Name; got != fieldName {
				t.Errorf("%s field %d is %q, want %q", name, index, got, fieldName)
			}
		}
	}
}

// TestTheAutomationEngineIsTheOnlyCallerOfItsPipeline keeps the adapter
// contained.
//
// If a handler could build an AutomationPipeline and submit through it, the
// engine's checks -- the policy, the window, the pause, the strategy ceiling --
// would be optional. They are not: the pipeline exists so the ENGINE can reach
// the services, and the API reaches those services directly, through their own
// audited, authorized handlers.
func TestTheAutomationEngineIsTheOnlyCallerOfItsPipeline(t *testing.T) {
	for _, file := range goFiles(t) {
		if !strings.Contains(file.text, "AutomationPipeline") {
			continue
		}
		switch filepath.ToSlash(filepath.Dir(file.rel)) {
		case "internal/service":
			// The engine and its adapter.
		case "internal/arch":
			// This test.
		case "cmd/harbormaster":
			// Composition root: builds it and hands it to the engine.
		default:
			t.Errorf("%s names AutomationPipeline\n"+
				"\tonly the engine, its adapter, and the composition root may; anywhere else "+
				"is a second caller that would not have run the policy, window, and pause "+
				"checks the engine applies before it submits", file.rel)
		}
	}
}
