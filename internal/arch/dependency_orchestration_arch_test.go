package arch_test

import (
	"go/ast"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The architectural guards for dependency orchestration.
//
// # What these defend
//
// Phase 16 added a subsystem that decides whether a container may be recreated
// and, in one case, that a container MUST be. That is close enough to a mutation
// capability to be worth proving it is not one.
//
// The claims, each with a test below:
//
//	 1. dependency code holds no Docker mutation capability
//	 2. no dependency file so much as NAMES one
//	 3. no dependency API handler reaches acquisition or execution
//	 4. the gate can only subtract from eligibility
//	 5. membership cannot enrol an unselected workload
//	 6. only coordinator-produced evidence can create an UpdateRebind plan
//	 7. ordinary UpdateNone remains non-executable
//	 8. namespace resolution failure always refuses
//	 9. a provider cannot mutate before its dependents are proven rebindable
//	10. persistence failure before mutation produces zero Docker calls
//	11. operator relationships cannot override Docker-derived ones
//	12. removing an operator edge cannot remove a Docker-derived edge
//	13. a rebind respects every existing policy and safety gate
//	14. no caller-supplied Docker value enters rebind construction

// dependencyFiles are the files this phase added or that carry its logic.
//
// Named explicitly rather than matched by a prefix, so a new file has to be
// added here deliberately and the person adding it has to read what the list is
// for.
var dependencyFiles = []string{
	"internal/domain/workload_dependency.go",
	"internal/domain/dependency_graph.go",
	"internal/domain/dependency_discovery.go",
	"internal/domain/dependency_validate.go",
	"internal/domain/dependency_state.go",
	"internal/domain/dependency_rebind.go",
	"internal/domain/dependency_operation.go",
	"internal/service/dependency.go",
	"internal/service/dependency_decide.go",
	"internal/service/dependency_recovery.go",
	"internal/store/dependency_repository.go",
	"internal/store/dependency_operation_repository.go",
}

// mutationCapabilities are the interfaces that can change the Docker host.
//
// Spelled here because this test's whole job is to find them by name. Every
// other file in the dependency subsystem is forbidden from mentioning them.
var mutationCapabilities = []string{
	"ContainerMutator",
	"ImageAcquirer",
	"ContainerRollbacker",
	"ConfigCapturer",
}

// 1. The dependency service holds no Docker capability, by reflection.
//
// Checked on the OPTIONS struct as well as the service, because the options are
// where a capability would be wired: a field that does not exist cannot be
// populated by a composition root, however well-intentioned.
func TestDependencyServiceHoldsNoDockerCapability(t *testing.T) {
	t.Parallel()

	for _, subject := range []reflect.Type{
		reflect.TypeOf(service.DependencyOptions{}),
		reflect.TypeOf(service.DependencyService{}),
	} {
		for i := range subject.NumField() {
			field := subject.Field(i)
			typeName := field.Type.String()
			for _, capability := range mutationCapabilities {
				if strings.Contains(typeName, capability) {
					t.Errorf("%s.%s is a %s\n"+
						"\tthe dependency subsystem is a READER. It produces evidence and "+
						"hands it to services that own their own capability and re-run their "+
						"own preflight. A capability here would make it a second mutation path.",
						subject.Name(), field.Name, typeName)
				}
			}
			// The pipeline is not a capability, but holding it would let this
			// service submit work of its own, which is the same problem.
			if strings.Contains(typeName, "AutomationPipeline") {
				t.Errorf("%s.%s holds the automation pipeline; the dependency subsystem "+
					"must not be able to submit work", subject.Name(), field.Name)
			}
		}
	}
}

// 2. No dependency file names a mutation capability at all.
//
// Stronger than the reflection check above and for a different reason: a
// capability could be reached through a local variable, a parameter, or a type
// assertion without ever appearing as a struct field. A mention is the thing
// that is forbidden.
func TestNoDependencyFileNamesAMutationCapability(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	var offenders []string

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if !isDependencyFile(relative) {
			return
		}

		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || ident.Name != "docker" {
				return true
			}
			for _, capability := range mutationCapabilities {
				if selector.Sel.Name == capability {
					offenders = append(offenders, relative+":"+
						fmtLine(fset, selector.Pos())+" names docker."+capability)
				}
			}
			return true
		})
	})

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("a dependency file names a Docker mutation capability:\n\t%s\n\n"+
			"The dependency subsystem decides ORDER and produces EVIDENCE. It must "+
			"not be able to act on either.", strings.Join(offenders, "\n\t"))
	}
}

// dependencyHandlerFile is the API surface the guards below inspect.
const dependencyHandlerFile = "internal/api/dependency_handlers.go"

// The guards on the handler file must not pass because the file is absent.
//
// # Why this test exists
//
// In Stage 3b the handler guards were written before the handlers, and they
// passed -- vacuously, because an AST walk over a file that does not exist
// inspects nothing. A guard that passes for the wrong reason is worse than no
// guard: it appears in the suite, it is green, and it is defending nothing.
//
// So the file's EXISTENCE is asserted separately. If the handlers are ever
// removed or renamed, this fails loudly rather than quietly turning three
// security guards into no-ops.
func TestTheDependencyHandlerGuardsAreNotVacuous(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	found := false
	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		if relativeSlash(t, root, fset.Position(file.Pos()).Filename) == dependencyHandlerFile {
			found = true
		}
	})

	if !found {
		t.Fatalf("%s does not exist\n"+
			"\tthree architecture guards walk this file: that it does not import "+
			"internal/docker, that it names no mutation capability, and that it "+
			"calls no mutation request. All three pass vacuously when the file is "+
			"absent. If the handlers moved, move the guards with them.",
			dependencyHandlerFile)
	}
}

// 1 (API half). The dependency handlers do not import internal/docker.
//
// The strongest form of "this file cannot reach the daemon": not that it does
// not call a mutation, but that the package holding every Docker type is not in
// scope at all.
func TestDependencyHandlersDoNotImportDocker(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	var offenders []string

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		if relativeSlash(t, root, fset.Position(file.Pos()).Filename) != dependencyHandlerFile {
			return
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasSuffix(path, "/internal/docker") {
				offenders = append(offenders, dependencyHandlerFile+":"+
					fmtLine(fset, spec.Pos())+" imports "+path)
			}
		}
	})

	if len(offenders) > 0 {
		t.Errorf("the dependency handlers import the Docker package:\n\t%s\n\n"+
			"These endpoints read a projection and write one small table. Nothing "+
			"they do requires a Docker type to be in scope.",
			strings.Join(offenders, "\n\t"))
	}
}

// 5. No API route creates an UpdateRebind plan.
//
// Belt and braces over the structural guarantee. RebindEvidence cannot be
// constructed outside internal/domain, so BuildRebindPlan is unreachable from a
// handler -- but a future refactor that exported a field would make it
// reachable, and this catches the attempt at the call site as well.
func TestNoAPIFileConstructsARebind(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	banned := map[string]string{
		"BuildRebindPlan":    "an API route must not construct a rebind plan",
		"RebindEvidenceFrom": "an API route must not construct rebind evidence",
		"NewRebindTarget":    "an API route must not construct a rebind target",
		"UpdateRebind":       "an API route must not name the rebind update type",
	}

	var offenders []string
	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if !strings.HasPrefix(relative, "internal/api/") ||
			strings.HasSuffix(relative, "_test.go") {
			return
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || ident.Name != "domain" {
				return true
			}
			if why, forbidden := banned[selector.Sel.Name]; forbidden {
				offenders = append(offenders,
					relative+":"+fmtLine(fset, selector.Pos())+" names domain."+
						selector.Sel.Name+" -- "+why)
			}
			return true
		})
	})

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("an API file reaches rebind construction:\n\t%s\n\n"+
			"A rebind plan exists only because the coordinator observed a stale "+
			"namespace reference in HarborMaster's own inventory. A caller acts on "+
			"one by naming the plan id it generated, through the existing "+
			"acquisition and execution endpoints.", strings.Join(offenders, "\n\t"))
	}
}

// 1 (production wiring). BuildRebindPlan has exactly ONE caller.
//
// # Why a call-site census rather than a behavioural test
//
// The structural guarantee is that RebindEvidence cannot be constructed outside
// internal/domain. This is the complement: even inside the service layer, the
// number of places that can turn evidence into a plan is one, and it is the
// coordinator.
//
// A second caller would not be a bug on its own -- but it would be a second
// place where the TOCTOU re-checks have to be remembered, and those are the
// checks that stop a container being recreated on evidence that expired minutes
// ago. Keeping it to one is what makes "the re-checks always run" true by
// construction.
func TestBuildRebindPlanHasExactlyOneCaller(t *testing.T) {
	t.Parallel()

	const allowed = "internal/service/dependency_coordinator.go"

	root := moduleRoot(t)
	var callers []string

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if strings.HasSuffix(relative, "_test.go") {
			// Tests exercise the builder directly and deliberately: the
			// safety-gate table in this file is one of them.
			return
		}
		// The definition lives in domain; this is about CALLERS.
		if strings.HasPrefix(relative, "internal/domain/") {
			return
		}

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "BuildRebindPlan" {
				return true
			}
			callers = append(callers, relative+":"+fmtLine(fset, call.Pos()))
			return true
		})
	})

	sort.Strings(callers)
	if len(callers) != 1 {
		t.Fatalf("domain.BuildRebindPlan has %d callers, want exactly 1:\n\t%s\n\n"+
			"The coordinator re-reads every condition immediately before building a "+
			"plan -- whether the dependent still exists, still points at the expected "+
			"provider, is still recreatable, is still pinnable. A second caller is a "+
			"second place those re-checks have to be remembered.",
			len(callers), strings.Join(callers, "\n\t"))
	}
	if !strings.HasPrefix(callers[0], allowed+":") {
		t.Fatalf("domain.BuildRebindPlan is called from %s, want %s", callers[0], allowed)
	}
}

// 6. Operation creation is invoked from the execution service before mutation.
//
// An AST check that the call EXISTS in the pipeline, complementing the
// behavioural tests that assert zero mutations when it fails. Without this, the
// call could be deleted and the behavioural tests would still pass -- they would
// simply never reach the failure they are testing.
func TestTheExecutionPipelineRecordsTheOperationBeforeMutating(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	var recordLine, mutateLine int

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		if relativeSlash(t, root, fset.Position(file.Pos()).Filename) !=
			"internal/service/execution_pipeline.go" {
			return
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "recordDependencyOperation":
				recordLine = fset.Position(call.Pos()).Line
			case "mutate":
				// The call that begins changing the host.
				if mutateLine == 0 {
					mutateLine = fset.Position(call.Pos()).Line
				}
			}
			return true
		})
	})

	if recordLine == 0 {
		t.Fatal("execution_pipeline.go does not record the dependency operation\n" +
			"\tthe set of containers that must be reattached has to be durable BEFORE " +
			"the provider is stopped: stopping is the instant they lose their network.")
	}
	if mutateLine == 0 {
		t.Fatal("execution_pipeline.go no longer calls mutate; this guard needs updating")
	}
	if recordLine >= mutateLine {
		t.Fatalf("the dependency operation is recorded at line %d, at or after the "+
			"mutation begins at line %d\n"+
			"\tA record written after the stop describes containers that are already "+
			"broken.", recordLine, mutateLine)
	}
}

// automationDependencyFile is the pass/follower wiring these guards inspect.
const automationDependencyFile = "internal/service/automation_dependency.go"

// The automation dependency wiring owns no Docker capability.
//
// # Why this file specifically
//
// It is the one place where the dependency subsystem touches the engine that
// changes the host. It reorders submissions and it advances coordinated
// operations, and both are a short step from "and then it calls Docker".
//
// It does not, and these assertions are what keep it that way: the package is
// not imported, the capability interfaces are not named, and the mutation
// methods are not referenced. The follower reaches the host only through
// RequestAcquisition, exactly as the decision pass always has.
func TestTheAutomationDependencyWiringHoldsNoDockerCapability(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	found := false
	var offenders []string

	// The mutation methods, spelled here because finding them by name is this
	// test's whole job.
	mutationMethods := map[string]struct{}{
		"StopContainer": {}, "StartContainer": {}, "CreateContainer": {},
		"RenameContainer": {}, "RemoveContainer": {}, "PullByDigest": {},
		"CaptureConfig": {}, "RollbackContainer": {},
	}

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		if relativeSlash(t, root, fset.Position(file.Pos()).Filename) != automationDependencyFile {
			return
		}
		found = true

		// The package itself must not be in scope.
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasSuffix(path, "/internal/docker") {
				offenders = append(offenders, automationDependencyFile+":"+
					fmtLine(fset, spec.Pos())+" imports "+path)
			}
		}

		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// A capability named through the docker package.
			if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == "docker" {
				for _, capability := range mutationCapabilities {
					if selector.Sel.Name == capability {
						offenders = append(offenders, automationDependencyFile+":"+
							fmtLine(fset, selector.Pos())+" names docker."+capability)
					}
				}
			}
			// A mutation method called on anything at all.
			if _, banned := mutationMethods[selector.Sel.Name]; banned {
				offenders = append(offenders, automationDependencyFile+":"+
					fmtLine(fset, selector.Pos())+" references "+selector.Sel.Name)
			}
			return true
		})
	})

	// NON-VACUOUS: the file must exist, or these three checks inspect nothing.
	if !found {
		t.Fatalf("%s does not exist\n"+
			"\tThree guards walk this file. All of them pass vacuously when it is "+
			"absent. If the wiring moved, move the guards with it.",
			automationDependencyFile)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("the automation dependency wiring reaches Docker:\n\t%s\n\n"+
			"This code decides ORDER and advances bookkeeping. It reaches the host "+
			"only by asking a service that owns its own capability and re-runs its "+
			"own preflight -- the same RequestAcquisition the decision pass uses.",
			strings.Join(offenders, "\n\t"))
	}
}

// AutomationOptions gained no Docker capability field.
//
// The automation engine's own guard covers the fields it had before Phase 16;
// this covers the one Phase 16 added. `Dependencies` is a READER, and a
// capability reaching the engine through it would be the same hole by a
// different door.
func TestAutomationOptionsGainedNoCapability(t *testing.T) {
	t.Parallel()

	options := reflect.TypeOf(service.AutomationOptions{})
	for i := range options.NumField() {
		field := options.Field(i)
		typeName := field.Type.String()
		for _, capability := range mutationCapabilities {
			if strings.Contains(typeName, capability) {
				t.Errorf("service.AutomationOptions.%s is a %s\n"+
					"\tThe engine's whole ability to affect the host is "+
					"AutomationPipeline: three request submissions to services that "+
					"own their capabilities. A capability here would bypass all of it.",
					field.Name, typeName)
			}
		}
	}

	// And the dependency field, specifically, is the narrow read interface.
	dependencies, ok := options.FieldByName("Dependencies")
	if !ok {
		t.Fatal("service.AutomationOptions has no Dependencies field; this guard needs updating")
	}
	if dependencies.Type.Kind() != reflect.Interface {
		t.Fatalf("AutomationOptions.Dependencies is %s, want an interface\n"+
			"\tA concrete type here would let the engine reach whatever else that "+
			"type can do.", dependencies.Type.Kind())
	}
	if dependencies.Type.NumMethod() != 1 {
		t.Errorf("AutomationOptions.Dependencies has %d methods, want exactly 1 (View)\n"+
			"\tThe pass asks what the estate's ordering is. Every additional method "+
			"is something else it could do instead.", dependencies.Type.NumMethod())
	}
}

// Pass purity: the normal decision cannot see dependency state.
//
// # The layering, and why a structural guard beats a behavioural one
//
// The three-phase pass depends on phase 1 being genuinely independent of phase
// 2. A behavioural test can show that two graphs produce the same verdict for
// the cases it tries; it cannot show that no graph ever could.
//
// This can. If DecideAutomation has no way to READ dependency state, then no
// arrangement of dependency state can change what it decides -- and the way to
// establish that is to prove the types it reads have no dependency field and
// the file it lives in never mentions the graph.
//
// That is three-phase invariant A, proved once for every possible input.
func TestTheNormalDecisionCannotReadDependencyState(t *testing.T) {
	t.Parallel()

	// The two types DecideAutomation reads. A dependency field on either would
	// be a channel from phase 2 back into phase 1.
	for _, subject := range []reflect.Type{
		reflect.TypeOf(service.AutomationInput{}),
		reflect.TypeOf(domain.SelectionTarget{}),
	} {
		for i := range subject.NumField() {
			field := subject.Field(i)
			lowered := strings.ToLower(field.Name)
			typeName := strings.ToLower(field.Type.String())

			for _, banned := range []string{"depend", "graph", "stage", "rebind"} {
				if strings.Contains(lowered, banned) || strings.Contains(typeName, banned) {
					t.Errorf("%s.%s (%s) exposes dependency state to the normal decision\n"+
						"\tPhase 1 must be independent of phase 2. A field here would let a "+
						"dependency relationship change WHETHER a container is eligible, "+
						"which is the broadening this phase must never permit.",
						subject.Name(), field.Name, field.Type)
				}
			}
		}
	}
}

// The file holding DecideAutomation never mentions the dependency vocabulary.
//
// Complements the reflection check above: a field is one way in, and a package
// level lookup is another.
func TestTheDecisionFileNeverMentionsDependencies(t *testing.T) {
	t.Parallel()

	const decisionFile = "internal/service/automation_decide.go"

	root := moduleRoot(t)
	found := false
	var offenders []string

	// Types and functions that only the dependency subsystem owns.
	banned := map[string]struct{}{
		"DependencyGraph": {}, "WorkloadDependency": {}, "DependencyState": {},
		"DecideDependency": {}, "DependencyInput": {}, "DependencyFact": {},
		"BuildDependencyGraph": {}, "RebindEvidence": {}, "BuildRebindPlan": {},
		"DiscoverDependencies": {},
	}

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		if relativeSlash(t, root, fset.Position(file.Pos()).Filename) != decisionFile {
			return
		}
		found = true

		ast.Inspect(file, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if _, forbidden := banned[ident.Name]; forbidden {
				offenders = append(offenders,
					decisionFile+":"+fmtLine(fset, ident.Pos())+" mentions "+ident.Name)
			}
			return true
		})
	})

	// NON-VACUOUS.
	if !found {
		t.Fatalf("%s does not exist; this guard inspects nothing", decisionFile)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("the normal automation decision names the dependency vocabulary:\n\t%s\n\n"+
			"DecideAutomation decides WHETHER a container may be updated. The "+
			"dependency gate decides whether that may happen YET. Merging them "+
			"would make a relationship able to affect eligibility.",
			strings.Join(offenders, "\n\t"))
	}
}

// The stage sorter cannot reach a change plan.
//
// A sorter that could read ChangePlan fields could also write them, and the
// field it would be writing is the digest a container is recreated on.
func TestTheStageSorterCannotReachAChangePlan(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	found := false
	var offenders []string

	// The plan fields a sorter has no business naming.
	banned := map[string]struct{}{
		"ProposedImage": {}, "ProposedDigest": {}, "CurrentImage": {},
		"CurrentDigest": {}, "UpdateType": {}, "InputDigest": {},
	}

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		if relativeSlash(t, root, fset.Position(file.Pos()).Filename) != automationDependencyFile {
			return
		}
		found = true

		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, forbidden := banned[selector.Sel.Name]; forbidden {
				offenders = append(offenders, automationDependencyFile+":"+
					fmtLine(fset, selector.Pos())+" names "+selector.Sel.Name)
			}
			return true
		})
	})

	if !found {
		t.Fatalf("%s does not exist; this guard inspects nothing", automationDependencyFile)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("the dependency wiring reaches into a change plan:\n\t%s\n\n"+
			"Dependency ordering is a scheduling projection. It decides WHEN work "+
			"is submitted and never what that work is.",
			strings.Join(offenders, "\n\t"))
	}
}

// The dependency wiring starts no goroutine.
//
// # Why this is a source check rather than a measurement
//
// The scale tests originally asserted on runtime.NumGoroutine() before and
// after a pass. That number is process-global, and the tests run in parallel, so
// the delta included other tests' goroutines -- it passed in isolation and
// failed in the full suite, which is the worst of both.
//
// The property is structural anyway. "No concurrency was added" is a fact about
// the source: the pass runs on the caller's goroutine, the gate is a loop, and
// the stage sorter is a sort. A `go` statement in this file would be a new,
// unbounded fan-out over containers -- so the check is that there isn't one.
func TestTheDependencyWiringStartsNoGoroutine(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	var offenders []string
	inspected := 0

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if !isDependencyFile(relative) && relative != automationDependencyFile {
			return
		}
		inspected++

		ast.Inspect(file, func(node ast.Node) bool {
			if statement, ok := node.(*ast.GoStmt); ok {
				offenders = append(offenders,
					relative+":"+fmtLine(fset, statement.Pos())+" starts a goroutine")
			}
			return true
		})
	})

	// NON-VACUOUS: the files must have been found.
	if inspected == 0 {
		t.Fatal("no dependency files were inspected; this guard checks nothing")
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("the dependency subsystem starts goroutines:\n\t%s\n\n"+
			"Concurrency in this subsystem would be per-container fan-out over an "+
			"estate HarborMaster does not bound. The existing acquisition, "+
			"execution, and automation limits remain the only concurrency "+
			"controls.", strings.Join(offenders, "\n\t"))
	}
}

// 3. No dependency API handler reaches acquisition, execution, or rollback.
//
// The handlers may READ dependency records and WRITE operator relationships.
// They may not request a pull, a recreation, or a rollback -- a caller acts on
// those through the existing endpoints, naming a record HarborMaster generated.
func TestDependencyHandlersCannotRequestMutation(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	forbidden := map[string]string{
		"RequestAcquisition": "a dependency handler must not start an image download",
		"RequestExecution":   "a dependency handler must not start a recreation",
		"RequestRollback":    "a dependency handler must not start a rollback",
		"StopContainer":      "a dependency handler must not touch a container",
		"CreateContainer":    "a dependency handler must not create a container",
		"RemoveContainer":    "a dependency handler must not remove a container",
		"RenameContainer":    "a dependency handler must not rename a container",
		"StartContainer":     "a dependency handler must not start a container",
	}

	var offenders []string
	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if relative != "internal/api/dependency_handlers.go" {
			return
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, banned := forbidden[selector.Sel.Name]; banned {
				offenders = append(offenders,
					relative+":"+fmtLine(fset, call.Pos())+" calls "+selector.Sel.Name+" -- "+why)
			}
			return true
		})
	})

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("a dependency handler reaches a mutation:\n\t%s", strings.Join(offenders, "\n\t"))
	}
}

// 6 and 14. Only coordinator-produced evidence can create an UpdateRebind plan,
// and no caller-supplied value enters its construction.
//
// # Why this is a type-surface test rather than a behavioural one
//
// The guarantee is STRUCTURAL: RebindEvidence's fields are unexported, so no
// package outside internal/domain can write one as a literal. This test proves
// the two halves that make that matter -- that the zero value is refused, and
// that the fields really are unexported.
func TestOnlyCoordinatorEvidenceProducesARebindPlan(t *testing.T) {
	t.Parallel()

	// Every field unexported. If one were exported, an API handler could build
	// evidence naming any container and any provider id it liked.
	evidenceType := reflect.TypeOf(domain.RebindEvidence{})
	for i := range evidenceType.NumField() {
		if evidenceType.Field(i).IsExported() {
			t.Fatalf("domain.RebindEvidence.%s is exported\n"+
				"\tthe unexported fields are what stop a caller constructing evidence. "+
				"An exported one would make 'please rebind this container' a request "+
				"body, which is a caller-supplied container id reaching a mutation.",
				evidenceType.Field(i).Name)
		}
	}

	// The zero value -- the only one an outside package can produce -- is
	// refused.
	_, refusal := domain.BuildRebindPlan(
		domain.RebindEvidence{},
		domain.RebindCandidate{
			Name: "sonarr", Provider: "gluetun",
			Present: true, NamespacesObserved: true, Recreatable: true,
			RunningReference: "alpine:3.22",
			RunningDigest:    "sha256:" + strings.Repeat("a", 64),
		},
		domain.SelfIdentity{},
		domain.PlanInputs{},
		"plan_test",
		time.Now().UTC(),
	)
	if refusal != domain.RebindRefusalNoEvidence {
		t.Fatalf("unestablished evidence produced %q, want noEvidence", refusal)
	}

	// And the only constructor refuses everything that is not the specific
	// stale-binding signal from a HARD namespace source.
	for _, refusalKind := range domain.DiscoveryRefusals {
		problem := domain.DependencyProblem{
			Container:    "sonarr",
			Source:       domain.DependencyNetworkNamespace,
			ReferencedID: strings.Repeat("a", 64),
			Refusal:      refusalKind,
		}
		_, ok := domain.RebindEvidenceFrom(problem, "gluetun", time.Now().UTC())
		if ok != refusalKind.RebindSignal() {
			t.Errorf("discovery refusal %q produced evidence = %v", refusalKind, ok)
		}
	}

	// An OPERATOR relationship can never require a rebind. Only Docker enforces
	// a namespace.
	if _, ok := domain.RebindEvidenceFrom(domain.DependencyProblem{
		Container:    "api",
		Source:       domain.DependencyOperator,
		ReferencedID: strings.Repeat("a", 64),
		Refusal:      domain.DiscoveryUnknownContainer,
	}, "postgres", time.Now().UTC()); ok {
		t.Fatal("an operator relationship produced rebind evidence")
	}
}

// 7. Ordinary UpdateNone remains non-executable, and adding UpdateRebind did
// not change that.
func TestUpdateNoneRemainsNonExecutable(t *testing.T) {
	t.Parallel()

	for _, strategy := range domain.UpdateStrategies {
		if strategy.Permits(domain.UpdateNone) {
			t.Errorf("strategy %q permits updateNone", strategy)
		}
		if strategy.Permits(domain.UpdateUnknown) {
			t.Errorf("strategy %q permits an unsized update", strategy)
		}
		// The rebind allowance is deliberate and universal; see the note on
		// Permits. Asserted so its absence would be noticed too.
		if !strategy.Permits(domain.UpdateRebind) {
			t.Errorf("strategy %q refuses a rebind; a container it broke would stay broken", strategy)
		}
	}

	// A plan proposing nothing is structurally valid but proposes nothing, which
	// is what makes it inert.
	empty := domain.ChangePlan{UpdateType: domain.UpdateNone}
	if !empty.ValidTarget() {
		t.Fatal("a plan proposing nothing should be structurally valid")
	}
	if empty.ProposedImage != "" || empty.ProposedDigest != "" {
		t.Fatal("a plan proposing nothing carries a proposal")
	}
}

// 2 (policy half). "Every strategy permits a rebind" must never be mistaken for
// "a rebind is universally authorised."
//
// The strategy is ONE gate of many. This walks the others and asserts that a
// rebind is refused by each of them exactly as any other change would be.
func TestARebindIsNotUniversallyAuthorised(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("b", 64)
	candidate := func(tune func(*domain.RebindCandidate)) domain.RebindCandidate {
		base := domain.RebindCandidate{
			Name: "sonarr", Provider: "gluetun",
			ContainerID:        strings.Repeat("c", 64),
			ImageRef:           "alpine:3.22",
			Present:            true,
			NamespacesObserved: true,
			Recreatable:        true,
			RunningReference:   "alpine:3.22",
			RunningDigest:      digest,
		}
		if tune != nil {
			tune(&base)
		}
		return base
	}
	evidence, ok := domain.RebindEvidenceFrom(domain.DependencyProblem{
		Container:    "sonarr",
		Source:       domain.DependencyNetworkNamespace,
		ReferencedID: strings.Repeat("d", 64),
		Refusal:      domain.DiscoveryUnknownContainer,
	}, "gluetun", time.Now().UTC())
	if !ok {
		t.Fatal("could not build the evidence this test is about")
	}

	cases := []struct {
		name string
		tune func(*domain.RebindCandidate)
		self domain.SelfIdentity
		want domain.RebindRefusal
	}{
		{
			name: "the estate-wide opt-out is honoured",
			tune: func(c *domain.RebindCandidate) {
				c.Labels = map[string]string{domain.LabelHarborMasterEnabled: "false"}
			},
			want: domain.RebindRefusalDisabled,
		},
		{
			name: "the update opt-out label is honoured",
			tune: func(c *domain.RebindCandidate) {
				c.Labels = map[string]string{domain.LabelUpdateEnabled: "false"}
			},
			want: domain.RebindRefusalDisabled,
		},
		{
			name: "self-update protection holds",
			tune: func(c *domain.RebindCandidate) { c.Name = "harbormaster" },
			self: domain.SelfIdentity{ContainerName: "harbormaster"},
			want: domain.RebindRefusalHarborMaster,
		},
		{
			name: "a preserved container is refused",
			tune: func(c *domain.RebindCandidate) { c.Derived = true },
			want: domain.RebindRefusalPreserved,
		},
		{
			name: "an absent container is refused",
			tune: func(c *domain.RebindCandidate) { c.Present = false },
			want: domain.RebindRefusalNotPresent,
		},
		{
			name: "an unpinnable container is refused",
			tune: func(c *domain.RebindCandidate) { c.RunningDigest = "" },
			want: domain.RebindRefusalDigestUnestablished,
		},
		{
			name: "an unobserved namespace is refused",
			tune: func(c *domain.RebindCandidate) { c.NamespacesObserved = false },
			want: domain.RebindRefusalNamespaceStale,
		},
		{
			name: "an unrecreatable workload is refused",
			tune: func(c *domain.RebindCandidate) { c.Recreatable = false },
			want: domain.RebindRefusalNotRecreatable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			subject := candidate(testCase.tune)
			// The self-match case renames the dependent, so the evidence has to
			// name the same container or it refuses for the wrong reason.
			subjectEvidence := evidence
			if subject.Name != "sonarr" {
				built, ok := domain.RebindEvidenceFrom(domain.DependencyProblem{
					Container:    subject.Name,
					Source:       domain.DependencyNetworkNamespace,
					ReferencedID: strings.Repeat("d", 64),
					Refusal:      domain.DiscoveryUnknownContainer,
				}, "gluetun", time.Now().UTC())
				if !ok {
					t.Fatal("could not build evidence for the renamed subject")
				}
				subjectEvidence = built
			}

			_, refusal := domain.BuildRebindPlan(subjectEvidence, subject,
				testCase.self, domain.PlanInputs{}, "plan_test", time.Now().UTC())
			if refusal != testCase.want {
				t.Fatalf("refusal = %q, want %q", refusal, testCase.want)
			}
		})
	}
}

// 11 and 12. An operator relationship cannot override a Docker-derived one, and
// removing one cannot remove a Docker-derived edge.
//
// # Why this is structural rather than behavioural
//
// Discovered edges are DERIVED from the inventory on every read and are never
// written to the operator table. So there is no row to delete, and an operator
// write that claimed a discovered source is refused at three independent layers:
// the domain validator, the repository, and a database CHECK.
func TestOperatorRelationshipsCannotOverrideDockerDerivedOnes(t *testing.T) {
	t.Parallel()

	// A write claiming a discovered source is refused by name.
	for _, source := range domain.DiscoveredDependencySources {
		_, refusal := domain.ValidateOperatorDependency(domain.DependencyValidationInput{
			Dependent:  "sonarr",
			Dependency: "gluetun",
			Source:     source,
			Containers: map[string]domain.DependencyEndpoint{
				"sonarr":  {Name: "sonarr", Present: true},
				"gluetun": {Name: "gluetun", Present: true},
			},
		})
		if refusal != domain.DependencyRefusalDiscoveredSource {
			t.Errorf("a write claiming %q was refused with %q, want discoveredSource",
				source, refusal)
		}
	}

	// An operator relationship duplicating a discovered one is refused as a
	// duplicate rather than stored alongside it -- so there is never a second,
	// operator-owned row an operator could delete to weaken a Docker-enforced
	// ordering.
	_, refusal := domain.ValidateOperatorDependency(domain.DependencyValidationInput{
		Dependent:  "sonarr",
		Dependency: "gluetun",
		Source:     domain.DependencyOperator,
		Containers: map[string]domain.DependencyEndpoint{
			"sonarr":  {Name: "sonarr", Present: true},
			"gluetun": {Name: "gluetun", Present: true},
		},
		Existing: []domain.WorkloadDependency{{
			Dependent: "sonarr", Dependency: "gluetun",
			Source: domain.DependencyNetworkNamespace,
		}},
	})
	if refusal != domain.DependencyRefusalDuplicate {
		t.Fatalf("duplicating a discovered relationship was refused with %q, want duplicate", refusal)
	}

	// And a discovered edge cannot carry a stored identity, so a DELETE by id
	// cannot name one.
	edges, _ := domain.DiscoverDependencies([]domain.ContainerNamespaceRow{
		{ContainerID: strings.Repeat("a", 64), Name: "gluetun",
			Modes: domain.NamespaceModes{Observed: true}},
		{ContainerID: strings.Repeat("b", 64), Name: "sonarr",
			Modes: domain.NamespaceModes{
				Network: "container:" + strings.Repeat("a", 64), Observed: true}},
	})
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
	if edges[0].DependencyID != "" {
		t.Fatal("a discovered relationship carries a stored id; it could then be deleted by id")
	}
}

// 8. A namespace resolution failure always refuses.
//
// The SharesNamespace / ParseNamespaceContainerRef distinction, asserted as the
// safety property rather than as parser behaviour: a mode this build cannot read
// must never produce "no relationship".
func TestNamespaceResolutionFailureAlwaysSubtracts(t *testing.T) {
	t.Parallel()

	unreadable := []string{
		"container:gluetun",
		"container:07d62ee08974",
		"container:",
		"container:" + strings.ToUpper(strings.Repeat("a", 64)),
	}

	for _, mode := range unreadable {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			if !domain.SharesNamespace(mode) {
				t.Fatal("a container: reference must count as sharing")
			}
			if _, ok := domain.ParseNamespaceContainerRef(mode); ok {
				t.Fatal("an unreadable reference must not parse")
			}

			// The consequence: a problem, never an absence of edges read as
			// independence.
			edges, problems := domain.DiscoverDependencies([]domain.ContainerNamespaceRow{
				{ContainerID: strings.Repeat("b", 64), Name: "sonarr",
					Modes: domain.NamespaceModes{Network: mode, Observed: true}},
			})
			if len(edges) != 0 {
				t.Fatalf("edges = %v, want none", edges)
			}
			if len(problems) == 0 {
				t.Fatal("an unreadable namespace produced no problem, so the container reads as independent")
			}
		})
	}
}

// 5. Dependency membership cannot enrol an otherwise-unselected workload.
//
// The selector is asked the same question with and without a dependency
// relationship in play, and must give the same answer: the graph is not one of
// its inputs, and there is no field on SelectionTarget for one.
func TestDependencyMembershipCannotEnrolAWorkload(t *testing.T) {
	t.Parallel()

	// SelectionTarget is what a policy selector matches against. If it gained a
	// dependency field, a relationship could start influencing selection.
	targetType := reflect.TypeOf(domain.SelectionTarget{})
	for i := range targetType.NumField() {
		name := strings.ToLower(targetType.Field(i).Name)
		if strings.Contains(name, "depend") || strings.Contains(name, "graph") {
			t.Fatalf("domain.SelectionTarget gained field %q\n"+
				"\ta policy selector must not be able to see dependency relationships. "+
				"Broad SELECTION is not broad AUTHORISATION, and a relationship must "+
				"never be a route into a policy's scope.", targetType.Field(i).Name)
		}
	}

	// And the same for the automation decision's inputs.
	inputType := reflect.TypeOf(service.AutomationInput{})
	for i := range inputType.NumField() {
		name := strings.ToLower(inputType.Field(i).Name)
		if strings.Contains(name, "depend") || strings.Contains(name, "graph") {
			t.Fatalf("service.AutomationInput gained field %q; the eligibility decision "+
				"must not read dependency state", inputType.Field(i).Name)
		}
	}
}

// isDependencyFile reports whether a repository-relative path is one of the
// files these guards cover.
func isDependencyFile(relative string) bool {
	for _, candidate := range dependencyFiles {
		if relative == candidate {
			return true
		}
	}
	return false
}

// fmtLine renders a position's line number.
func fmtLine(fset *token.FileSet, pos token.Pos) string {
	return strings.TrimPrefix(fset.Position(pos).String(), fset.Position(pos).Filename+":")
}
