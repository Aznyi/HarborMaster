// Package arch holds architecture tests: rules about the SHAPE of the
// codebase that a reviewer would otherwise have to enforce by memory.
//
// These are guardrails for two invariants that are easy to break by accident
// and expensive to notice late:
//
//   - HarborMaster talks to a privileged Docker socket through exactly one
//     adapter package, so the SDK cannot spread into the domain, services,
//     repositories, or API handlers.
//   - That adapter's OBSERVATION surface is read-only, and its MUTATION surface
//     is exactly one method: pulling an approved, digest-pinned image. Gaining
//     the ability to start, stop, remove, or exec into a container must require
//     editing an interface and a test whose subject is that limit, which is what
//     makes it visible in review.
//
// The import rules are checked by parsing every Go file's import declarations
// with go/parser rather than by grepping. Grep would match the module paths
// written in this file's own error messages and in documentation; the AST sees
// only real imports, so the check has no false positives and cannot be evaded
// by an import alias.
package arch_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
)

// deprecatedModule is the retired root Docker module. Docker Engine v29 stopped
// publishing it, and its last release carries unpatched advisories with no
// fixed version, so it must never come back.
const deprecatedModule = "github.com/docker/docker"

// sdkModule is the supported Moby SDK. It is allowed in exactly one package.
const sdkModule = "github.com/moby/moby"

// engineModule is the Moby engine monolith. It is not intended to be consumed
// as an application library, and pulling it in would drag daemon code into a
// read-only observer.
const engineModule = "github.com/moby/moby/v2"

// adapterPackage is the only directory permitted to import the SDK.
const adapterPackage = "internal/docker"

// adapterModule is that package's import path.
const adapterModule = "github.com/Aznyi/HarborMaster/internal/docker"

// TestDeprecatedDockerModuleIsNotImported fails if the retired root module
// reappears anywhere, including in tests.
func TestDeprecatedDockerModuleIsNotImported(t *testing.T) {
	for _, imp := range allImports(t) {
		if imp.path == deprecatedModule || strings.HasPrefix(imp.path, deprecatedModule+"/") {
			t.Errorf("%s imports %s\n\tthe root Docker module was retired at Engine v29 and has "+
				"unpatched advisories with no fixed version; use %s/client and %s/api instead",
				imp.file, imp.path, sdkModule, sdkModule)
		}
	}
}

// TestEngineMonolithIsNotImported fails if the Moby engine module is imported.
func TestEngineMonolithIsNotImported(t *testing.T) {
	for _, imp := range allImports(t) {
		if imp.path == engineModule || strings.HasPrefix(imp.path, engineModule+"/") {
			t.Errorf("%s imports %s\n\tthe engine monolith is not an application library; "+
				"HarborMaster consumes %s/client and %s/api only",
				imp.file, imp.path, sdkModule, sdkModule)
		}
	}
}

// TestMobySDKIsConfinedToTheAdapter fails if any package outside
// internal/docker imports the SDK.
//
// This is the rule that keeps SDK types out of the domain models, services,
// repositories, API handlers, and the OpenAPI-facing layer. A leak there would
// couple the whole application to one runtime's wire format.
func TestMobySDKIsConfinedToTheAdapter(t *testing.T) {
	for _, imp := range allImports(t) {
		if imp.path != sdkModule && !strings.HasPrefix(imp.path, sdkModule+"/") {
			continue
		}
		if filepath.ToSlash(filepath.Dir(imp.rel)) == adapterPackage {
			continue
		}
		t.Errorf("%s imports %s\n\tthe Moby SDK may only be imported from %s; "+
			"convert to a domain type at that boundary instead",
			imp.file, imp.path, adapterPackage)
	}
}

// mutationMethods are capabilities HarborMaster must not have.
//
// The list is deliberately broader than the SDK's method names: it also catches
// the obvious hand-rolled spellings (Start, Stop, Remove) so that a method added
// under a different name than the SDK's still trips the test.
var mutationMethods = []string{
	"attach", "archive", "build", "commit", "connect", "copy", "create",
	"delete", "disconnect", "exec", "export", "import", "kill", "load",
	"login", "logout", "logs", "pause", "plugin", "prune", "pull", "push",
	"put", "remove", "rename", "resize", "restart", "rm", "run", "save",
	"start", "stop", "tag", "unpause", "update", "wait", "write",
}

// TestRuntimeExposesNoMutationMethods fails if the read-only runtime interface
// grows a method that could change the host.
//
// It reflects over the interface rather than reading the source, so a method
// added through an embedded interface is caught too.
func TestRuntimeExposesNoMutationMethods(t *testing.T) {
	runtimeType := reflect.TypeOf((*docker.Runtime)(nil)).Elem()

	for i := 0; i < runtimeType.NumMethod(); i++ {
		name := runtimeType.Method(i).Name
		lowered := strings.ToLower(name)

		for _, verb := range mutationMethods {
			// Prefix match: the verb is what the method DOES, so it leads the
			// name. Substring matching would reject ListContainers for
			// containing "tag" inside no word at all, and StreamEvents for
			// "run" -- neither of which is a mutation.
			if strings.HasPrefix(lowered, verb) {
				t.Errorf("docker.Runtime has method %q, which looks like a mutation (%q)\n"+
					"\tHarborMaster is read-only: it observes Docker and never changes it. "+
					"If this method really is an observation, rename it; if it is not, it does not belong here.",
					name, verb)
			}
		}
	}
}

// TestRuntimeSurfaceIsTheExpectedReadOnlySet pins the exact method set.
//
// The verb check above catches a mutation named like one. This catches
// everything else -- a method that mutates under an innocuous name still has to
// be added here consciously, in a diff a reviewer will see.
func TestRuntimeSurfaceIsTheExpectedReadOnlySet(t *testing.T) {
	want := map[string]bool{
		"Ping":             true,
		"ListContainers":   true,
		"InspectContainer": true,
		"InspectImage":     true,
		"ListNetworks":     true,
		"ListVolumes":      true,
		"StreamEvents":     true,
	}

	runtimeType := reflect.TypeOf((*docker.Runtime)(nil)).Elem()
	got := make(map[string]bool, runtimeType.NumMethod())
	for i := 0; i < runtimeType.NumMethod(); i++ {
		got[runtimeType.Method(i).Name] = true
	}

	for name := range got {
		if !want[name] {
			t.Errorf("docker.Runtime gained method %q\n\tevery method on the runtime interface is a "+
				"capability against a privileged socket; add it to this test only if it is an observation",
				name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("docker.Runtime no longer has method %q; update this test if the removal is intended", name)
		}
	}
}

// ---------------------------------------------------------- the mutation --

// Phase 8 gave HarborMaster its first Docker mutation: pulling an approved
// image. The three tests below are what keep it at exactly one.
//
// The rule is not "no mutation" any more, so it has to be stated precisely:
// there is ONE mutating method, on its OWN interface, and every service that
// does not need it receives docker.Runtime instead and therefore cannot reach
// it. Capability is granted by what a constructor is handed.

// TestTheMutationSurfaceIsExactlyOneMethod pins the entire write capability.
//
// If this test needs editing, the change under review is HarborMaster gaining a
// new power over a privileged socket. That is the point: the diff cannot be
// quiet.
func TestTheMutationSurfaceIsExactlyOneMethod(t *testing.T) {
	acquirerType := reflect.TypeOf((*docker.ImageAcquirer)(nil)).Elem()

	if got := acquirerType.NumMethod(); got != 1 {
		t.Fatalf("docker.ImageAcquirer has %d methods, want exactly 1\n"+
			"\tthis interface is the WHOLE of HarborMaster's ability to change the Docker host; "+
			"a second method is a second capability and needs its own review, its own threat "+
			"model entry, and its own tests", got)
	}
	if name := acquirerType.Method(0).Name; name != "PullByDigest" {
		t.Errorf("the mutation method is %q, want PullByDigest\n"+
			"\tthe name states the safety property: a pull that is not digest-pinned is a pull "+
			"whose content can change after approval", name)
	}
}

// containerVerbs are capabilities the IMAGE acquirer must never have.
//
// Pulling an image changes the image store and nothing else -- a running
// container keeps running the image it was created from. These verbs are the
// ones that would cross from "acquire" into "apply", which is a different
// capability with a different interface, a different owner, and its own tests
// further down this file.
var containerVerbs = []string{
	"attach", "commit", "connect", "copy", "create", "delete", "disconnect",
	"exec", "kill", "pause", "prune", "recreate", "remove", "rename", "restart",
	"restore", "rm", "rollback", "run", "start", "stop", "unpause", "update",
	"wait",
}

// TestTheMutationInterfaceCannotTouchAContainer fails if the acquirer grows a
// method that could change something that is running.
//
// Deliberately separate from the count above. A reviewer relaxing the count
// still has to get past this, and the two failures say different things.
//
// Still true after Phase 9. HarborMaster CAN now change a container, but not
// through this interface: the two capabilities are held by two services and
// granted by two constructor arguments, so a component able to pull is still
// unable to apply.
func TestTheMutationInterfaceCannotTouchAContainer(t *testing.T) {
	acquirerType := reflect.TypeOf((*docker.ImageAcquirer)(nil)).Elem()

	for i := 0; i < acquirerType.NumMethod(); i++ {
		name := acquirerType.Method(i).Name
		lowered := strings.ToLower(name)

		for _, verb := range containerVerbs {
			if strings.HasPrefix(lowered, verb) {
				t.Errorf("docker.ImageAcquirer has method %q, which looks like %q\n"+
					"\tHarborMaster acquires images through this interface and applies them "+
					"through docker.ContainerMutator. Merging the two would mean a service that "+
					"can download can also replace, which is the separation these tests keep.",
					name, verb)
			}
		}
	}
}

// ------------------------------------------------- the container mutation --

// Phase 9 gave HarborMaster its first CONTAINER mutation: replacing one
// container with a new one built from its own configuration. That is a
// materially larger privilege than pulling an image, and the tests below keep
// it at exactly the size it was reviewed at.
//
// Four things are pinned, each separately so a failure says which one changed:
//
//  1. The mutation interface has exactly five methods, with exactly these names.
//  2. It cannot reach an image, a volume, a network, or an exec.
//  3. The captured configuration exposes no field or method that could carry a
//     secret out of internal/docker.
//  4. No package outside the execution service can name any of it.

// TestTheContainerMutationSurfaceIsExactlyFiveMethods pins the whole container
// write capability.
//
// If this test needs editing, the change under review is HarborMaster gaining a
// new power over running containers on a privileged socket. That is the point:
// the diff cannot be quiet.
func TestTheContainerMutationSurfaceIsExactlyFiveMethods(t *testing.T) {
	mutatorType := reflect.TypeOf((*docker.ContainerMutator)(nil)).Elem()

	want := map[string]bool{
		"CreateContainer": true,
		"StartContainer":  true,
		"StopContainer":   true,
		"RenameContainer": true,
		"RemoveContainer": true,
	}

	if got := mutatorType.NumMethod(); got != len(want) {
		t.Fatalf("docker.ContainerMutator has %d methods, want exactly %d\n"+
			"\tthis interface is the WHOLE of HarborMaster's ability to change a running "+
			"container; a sixth method is a sixth capability and needs its own review, its "+
			"own threat model entry, and its own tests", got, len(want))
	}

	got := make(map[string]bool, mutatorType.NumMethod())
	for i := 0; i < mutatorType.NumMethod(); i++ {
		got[mutatorType.Method(i).Name] = true
	}
	for name := range got {
		if !want[name] {
			t.Errorf("docker.ContainerMutator gained method %q\n"+
				"\tevery method here is a capability against a privileged socket, and this set "+
				"is exactly what the recreation pipeline needs and nothing more", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("docker.ContainerMutator no longer has method %q; update this test if the "+
				"removal is intended", name)
		}
	}
}

// forbiddenMutatorVerbs are capabilities the container mutator must never gain.
//
// Recreating a container needs five verbs. These are the ones that would turn
// it into something else: a way to run commands inside a container, to read or
// write its filesystem, to delete data, or to touch the image store.
//
// "container" is deliberately absent even though every legal method contains
// it, and the five legal names are checked by the exact-set test above rather
// than by this one.
var forbiddenMutatorVerbs = []string{
	"attach", "build", "commit", "connect", "copy", "delete", "disconnect",
	"exec", "export", "image", "import", "kill", "load", "login", "logout",
	"logs", "network", "pause", "plugin", "prune", "pull", "push", "put",
	"resize", "restore", "rollback", "save", "tag", "unpause", "update",
	"volume", "wait", "write",
}

// TestTheContainerMutatorCannotReachAnythingElse fails if the mutator grows a
// method outside container lifecycle.
//
// Deliberately separate from the exact-set test. A reviewer relaxing that one
// still has to get past this, and the two failures say different things: one
// says "the surface grew", this says "the surface grew INTO something it was
// never meant to touch".
func TestTheContainerMutatorCannotReachAnythingElse(t *testing.T) {
	mutatorType := reflect.TypeOf((*docker.ContainerMutator)(nil)).Elem()

	for i := 0; i < mutatorType.NumMethod(); i++ {
		name := mutatorType.Method(i).Name
		lowered := strings.ToLower(name)

		for _, verb := range forbiddenMutatorVerbs {
			if strings.Contains(lowered, verb) {
				t.Errorf("docker.ContainerMutator has method %q, which contains %q\n"+
					"\tthis interface recreates containers. Executing inside one, copying files "+
					"in or out, deleting images or volumes, and reconfiguring networks are all "+
					"different capabilities and none of them belongs here.",
					name, verb)
			}
		}
	}
}

// TestTheConfigCapturerIsExactlyOneRead pins the capture surface.
//
// CaptureConfig is a READ, which is why it is not on ContainerMutator: a read
// there would inflate the pinned count above and make that test say something
// less true than it does. It gets its own interface and its own pin.
func TestTheConfigCapturerIsExactlyOneRead(t *testing.T) {
	capturerType := reflect.TypeOf((*docker.ConfigCapturer)(nil)).Elem()

	if got := capturerType.NumMethod(); got != 1 {
		t.Fatalf("docker.ConfigCapturer has %d methods, want exactly 1\n"+
			"\tit exists to hand ONE value to ONE service; anything else it grew would be "+
			"reachable by whoever holds the capture capability", got)
	}
	if name := capturerType.Method(0).Name; name != "CaptureConfig" {
		t.Errorf("the capture method is %q, want CaptureConfig", name)
	}
}

// TestTheRuntimeCannotCaptureAConfiguration fails if the read-only runtime
// grows the capture method.
//
// A capture is a read, so it would pass every mutation check in this file. It
// still must not be on Runtime: the value it returns is the CREATE PAYLOAD, and
// every service in HarborMaster receives Runtime. Putting it there would hand a
// container's real environment to the drift engine, the policy engine, the
// planner, and the API's inventory reader, none of which has any use for it.
func TestTheRuntimeCannotCaptureAConfiguration(t *testing.T) {
	runtimeType := reflect.TypeOf((*docker.Runtime)(nil)).Elem()

	for i := 0; i < runtimeType.NumMethod(); i++ {
		name := runtimeType.Method(i).Name
		if strings.Contains(strings.ToLower(name), "capture") {
			t.Errorf("docker.Runtime has method %q\n"+
				"\ta captured configuration is the create payload, and every service receives "+
				"Runtime. It belongs on docker.ConfigCapturer, which only the execution service "+
				"is given.", name)
		}
	}
}

// ---------------------------------------------- the captured configuration --

// TestCapturedConfigExposesNoSecretSurface pins what a CapturedConfig lets the
// world see.
//
// # What this protects
//
// A CapturedConfig holds a container's real environment values, its log-driver
// options, and the SDK structures that will be sent to the daemon. It is handed
// to the execution service, which must be able to pass it back into
// CreateContainer and must NOT be able to read it, log it, or serialise it.
//
// The unexported fields are what enforce that. This test is what stops the
// enforcement being undone by a well-meaning addition -- an `Env []string` for
// a diff view, a `Config` accessor for a test, a `HostConfig()` for a future
// feature. Each is a reasonable thing to want, and each would move secret
// values out of the one package allowed to hold them.
//
// # Why identifiers rather than types
//
// A type check would happily pass an `Env []string`. The point is not that the
// exported surface holds safe TYPES; it is that it holds exactly these five
// fields and these eight methods, every one of which has been looked at.
func TestCapturedConfigExposesNoSecretSurface(t *testing.T) {
	capturedType := reflect.TypeOf(docker.CapturedConfig{})

	wantFields := map[string]bool{
		"ContainerID":    true,
		"ContainerName":  true,
		"ImageReference": true,
		"ImageID":        true,
		"CapturedAt":     true,
	}

	for i := 0; i < capturedType.NumField(); i++ {
		field := capturedType.Field(i)
		if !field.IsExported() {
			continue
		}
		if !wantFields[field.Name] {
			t.Errorf("docker.CapturedConfig gained exported field %q\n"+
				"\tthis struct holds a container's real environment values and log-driver "+
				"credentials. An exported field is readable by the execution service, by "+
				"anything that logs it, and by any encoder that reaches it. Only identifiers "+
				"belong here -- add the field unexported, and expose a value-free projection "+
				"through Summary if a caller needs to know something about it.",
				field.Name)
		}
		delete(wantFields, field.Name)
	}
	for name := range wantFields {
		t.Errorf("docker.CapturedConfig no longer has exported field %q; update this test if "+
			"the removal is intended", name)
	}

	// The methods, pinned for the same reason: an accessor is an exported field
	// with extra steps.
	wantMethods := map[string]bool{
		// Reports whether the capture is complete enough to create from.
		"Valid": true,
		// The VALUE-FREE projection. The one way the service learns anything
		// about the contents.
		"Summary": true,
		// The normalised, MASKED view. Safe because domain.EnvVar.RawValue is
		// `json:"-"` and its Value field already holds the masked form.
		"Detail": true,
		// Redacted renderings, which exist precisely so the defaults cannot
		// spill the struct into a log or a response.
		"LogValue":    true,
		"String":      true,
		"MarshalJSON": true,

		// The two shared-namespace methods. Added in Phase 16, and admitted here
		// deliberately rather than reluctantly, because neither one widens what
		// this type lets out.
		//
		// NamespaceReferences returns CONTAINER IDS and a closed-vocabulary kind.
		// A container id is already an exported field on this struct and appears
		// in the inventory, in the API, and in container names on the host. It is
		// an identifier, which is exactly what the field rule above permits; it
		// is not configuration and carries no value from Config, HostConfig, or
		// the environment.
		//
		// RebindNamespaces is a WRITE, and returns nothing but an error. It takes
		// a map of id to id, validates both ends, and refuses anything it cannot
		// resolve. There is no way to read configuration through it and no way to
		// pass it a container name.
		//
		// Why they exist at all: verified against Docker 29.6.2, a capture whose
		// `container:<id>` namespace reference names a provider that has since
		// been replaced makes the daemon refuse the create -- AFTER the original
		// has been stopped and parked. The alternative to these two methods was
		// leaving that reference stale and letting the recreation fail at the
		// worst possible moment.
		"NamespaceReferences": true,
		"RebindNamespaces":    true,
	}

	pointerType := reflect.TypeOf(&docker.CapturedConfig{})
	for i := 0; i < pointerType.NumMethod(); i++ {
		name := pointerType.Method(i).Name
		if !wantMethods[name] {
			t.Errorf("docker.CapturedConfig gained exported method %q\n"+
				"\tan accessor on this type is a way for a secret to leave internal/docker. If "+
				"a caller needs to know something about the configuration, add it to the "+
				"value-free projection Summary returns.", name)
		}
		delete(wantMethods, name)
	}
	for name := range wantMethods {
		t.Errorf("docker.CapturedConfig no longer has method %q; update this test if the "+
			"removal is intended", name)
	}
}

// recreationAllowed are the packages permitted to hold the container mutation
// capability.
//
// internal/docker implements it. internal/service holds it in exactly one
// service. cmd/harbormaster wires that service. internal/arch is this test.
//
// Note who is ABSENT: internal/api. A handler that could stop a container
// directly would bypass the preflight revalidation, the checkpointing, and the
// verification -- which together are the entire safety model of this feature.
var recreationAllowed = map[string]bool{
	"internal/docker":  true,
	"internal/service": true,
	"cmd/harbormaster": true,
	"internal/arch":    true,
	// The live-Docker suite exercises the recreation against a real daemon,
	// which is the end-to-end proof that the adapter behaves as the fake models
	// it. Build-tagged, so it is never part of an ordinary build.
	"internal/integration": true,
}

// recreationIdentifiers are the names that grant, or carry, the ability to
// change a container.
//
// CapturedConfig is included even though it is inert: a package that can hold
// one is a package being handed a container's real configuration, and that
// deserves the same visibility as the mutation itself.
var recreationIdentifiers = []string{
	"ContainerMutator", "ConfigCapturer", "CapturedConfig",
	"CreateContainer", "StartContainer", "StopContainer",
	"RenameContainer", "RemoveContainer",
}

// TestTheRecreationCapabilityIsNotReferencedOutsideItsOwners fails if a package
// that has no business recreating names the mutation capability.
//
// An import-level rule would not do here: internal/api imports internal/docker
// for its read-only error types, so this checks the SOURCE for the identifiers
// rather than the import list -- the same technique, and the same honest limit,
// as the acquisition test above.
func TestTheRecreationCapabilityIsNotReferencedOutsideItsOwners(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist", "data", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if recreationAllowed[filepath.ToSlash(filepath.Dir(rel))] {
			return nil
		}

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, identifier := range recreationIdentifiers {
			if strings.Contains(string(source), identifier) {
				t.Errorf("%s references %s\n"+
					"\tthe ability to stop and replace a container belongs to the execution "+
					"service alone. A package that can reach it directly bypasses the preflight "+
					"revalidation, the checkpointing, and the verification -- which together are "+
					"the entire safety model of this feature.",
					rel, identifier)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// acquisitionAllowed are the packages permitted to hold the mutation
// capability.
//
// internal/docker implements it. internal/service holds it in exactly one
// service. cmd/harbormaster wires that service. internal/arch is this test.
//
// Note who is ABSENT: internal/api. A handler that could pull directly would
// bypass the preflight revalidation entirely, and preflight is the whole safety
// model -- the API asks the service for work, and the service decides.
var acquisitionAllowed = map[string]bool{
	"internal/docker":  true,
	"internal/service": true,
	"cmd/harbormaster": true,
	"internal/arch":    true,
	// The live-Docker suite exercises the pull directly against a real daemon,
	// which is the end-to-end proof that the adapter behaves as the fake models
	// it -- and it asserts that the container count is unchanged either side.
	// Build-tagged, so it is never part of an ordinary build.
	"internal/integration": true,
}

// TestTheMutationCapabilityIsNotReferencedOutsideItsOwners fails if a package
// that has no business pulling names the acquirer type.
//
// An import-level rule, with the honest limit that comes with one: it sees the
// package a file imports, not what it does with it. internal/api imports
// internal/docker for its read-only error types, so this checks the SOURCE for
// the identifier rather than the import list.
func TestTheMutationCapabilityIsNotReferencedOutsideItsOwners(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist", "data", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if acquisitionAllowed[filepath.ToSlash(filepath.Dir(rel))] {
			return nil
		}

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, identifier := range []string{"ImageAcquirer", "PullByDigest"} {
			if strings.Contains(string(source), identifier) {
				t.Errorf("%s references %s\n"+
					"\tthe pull capability belongs to the acquisition service alone. A package that "+
					"can pull directly bypasses the preflight revalidation, which is the entire "+
					"safety model of this feature.",
					rel, identifier)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// diagnosticsPackage is the operator-facing diagnostic surface.
const diagnosticsPackage = "github.com/Aznyi/HarborMaster/internal/diagnostics"

// apiPackage is the HTTP layer, which must never reach the diagnostics.
const apiPackage = "internal/api"

// TestDiagnosticsAreNotReachableOverHTTP fails if the API layer imports the
// diagnostics package.
//
// `harbormaster diagnose` reports filesystem paths, free space, journal mode,
// page counts, schema history, and when the Docker daemon was last reachable.
// That is what an operator needs and what an attacker wants, and HarborMaster's
// API is unauthenticated by design in this phase -- so an endpoint serving it
// would hand host layout to anything that can reach the port.
//
// Requiring shell access instead is the control. This test is what keeps it
// from being undone by a well-meaning "expose the diagnosis in the UI" change,
// which is a reasonable thing to want and an unreasonable thing to have before
// authentication exists.
func TestDiagnosticsAreNotReachableOverHTTP(t *testing.T) {
	for _, imp := range allImports(t) {
		if imp.path != diagnosticsPackage && !strings.HasPrefix(imp.path, diagnosticsPackage+"/") {
			continue
		}
		if filepath.ToSlash(filepath.Dir(imp.rel)) != apiPackage {
			continue
		}
		t.Errorf("%s imports %s\n"+
			"\tthe diagnostics report host paths, free space, and schema detail, and the API is "+
			"unauthenticated; it must stay a command that requires shell access, not an endpoint",
			imp.file, imp.path)
	}
}

// registryPackage is the only directory permitted to open an outbound network
// connection.
const registryPackage = "internal/registry"

// registryModule is that package's import path.
const registryModule = "github.com/Aznyi/HarborMaster/internal/registry"

// egressPackages are the standard-library packages a caller needs in order to
// build an HTTP client.
//
// net is deliberately ABSENT, and the omission is worth stating rather than
// leaving to be discovered: net is also the standard library's address-parsing
// package, and this check reads import declarations rather than call sites, so
// it cannot tell net.ParseIP from net.Dial. Including it would flag
// internal/domain's own SSRF host validation, and a test that cries wolf gets
// suppressed.
//
// The check therefore covers the packages that are needed to CONSTRUCT a
// client, which is the thing that would bypass the guarded transport. A raw
// net.Dial to a registry would slip past it -- that is the honest limit of an
// import-level rule, and the reason the address guard in internal/registry is
// enforced at dial time rather than relying on this.
var egressPackages = []string{"net/http", "crypto/tls"}

// egressAllowed are the directories permitted to import them.
//
// TWO of these are outbound egress, and the second was added deliberately in
// Phase 12:
//
//   - internal/registry reads registry metadata. Its destinations come from
//     image references HarborMaster computed; no caller supplies a host.
//   - internal/notify delivers notifications. Its destinations are URLs an
//     ADMINISTRATOR TYPED, which is a strictly larger risk, and its transport
//     carries the same defences plus a refusal of the cloud metadata endpoint
//     that holds even under the private-address opt-in.
//
// internal/api and internal/healthcheck serve and probe HTTP respectively --
// both INBOUND or loopback, neither a path to an arbitrary destination.
// cmd/harbormaster wires the server together.
//
// A third entry here is a third place that can reach the internet. Adding one
// means editing this list, which is the point.
var egressAllowed = map[string]bool{
	"internal/registry":    true,
	"internal/notify":      true,
	"internal/api":         true,
	"internal/healthcheck": true,
	"cmd/harbormaster":     true,
}

// egressGuarded are the two packages that actually dial arbitrary hosts, and
// the defences each must carry.
//
// Checked by source rather than by behaviour, because the failure this guards
// is a refactor that quietly drops one: a transport with no Control function
// still compiles, still works against a public host, and is no longer a
// defence.
var egressGuarded = map[string][]string{
	"internal/registry/transport.go": {
		"Control:",       // the dial-time address guard
		"Proxy:",         // explicitly nil
		"CheckRedirect:", // redirects refused
		"MinVersion",     // a TLS floor
	},
	"internal/notify/transport.go": {
		"Control:",
		"Proxy:",
		"CheckRedirect:",
	},
}

// TestEveryEgressTransportKeepsItsGuards fails if one of the two outbound
// transports loses a defence.
func TestEveryEgressTransportKeepsItsGuards(t *testing.T) {
	root := moduleRoot(t)

	for rel, required := range egressGuarded {
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v\n\tthis file builds one of HarborMaster's two "+
				"outbound transports; if it moved, move this test with it", rel, err)
			continue
		}
		text := string(source)
		for _, marker := range required {
			if !strings.Contains(text, marker) {
				t.Errorf("%s no longer sets %s\n"+
					"\tthis transport reaches arbitrary hosts; without every guard it is "+
					"a server-side request forgery primitive", rel, marker)
			}
		}
		// InsecureSkipVerify is never acceptable in either.
		//
		// Matched as an ASSIGNMENT rather than as a word. Both files mention it
		// in a comment saying it is deliberately absent, and a check that
		// flagged those would be a check somebody deletes.
		for _, assignment := range []string{
			"InsecureSkipVerify:",
			"InsecureSkipVerify =",
		} {
			if strings.Contains(text, assignment) {
				t.Errorf("%s sets InsecureSkipVerify\n"+
					"\tboth outbound transports verify certificates, and there is no "+
					"configuration that relaxes it", rel)
			}
		}
	}
}

// TestOutboundNetworkAccessIsConfinedToTheGuardedClients fails if a package
// outside the allowed set gains the ability to open a connection.
//
// Phase 6 gave HarborMaster its first outbound egress and Phase 12 its second,
// and the entire SSRF defence rests on both going through a guarded transport:
// a dialler that refuses non-public addresses, a redirect policy that refuses
// everything, no proxy, and HTTPS only.
//
// A third HTTP client built somewhere else would have none of that, and would
// be an easy thing to add without noticing what it bypassed. This test is what
// makes that visible in review.
func TestOutboundNetworkAccessIsConfinedToTheGuardedClients(t *testing.T) {
	for _, imp := range allImports(t) {
		dir := filepath.ToSlash(filepath.Dir(imp.rel))
		if egressAllowed[dir] {
			continue
		}
		// Tests may reach for a loopback server to exercise a handler.
		if strings.HasSuffix(imp.file, "_test.go") {
			continue
		}

		for _, pkg := range egressPackages {
			if imp.path != pkg {
				continue
			}
			t.Errorf("%s imports %s\n"+
				"\tHarborMaster's only outbound egress goes through %s, whose transport refuses "+
				"non-public addresses, refuses every redirect, ignores proxies, and requires HTTPS; "+
				"a client built elsewhere would have none of those defences",
				imp.file, imp.path, registryPackage)
		}
	}
}

// TestRegistryClientDoesNotImportTheDockerAdapter fails if the two boundaries
// are joined.
//
// They answer different questions from different sources -- one reads a
// privileged local socket, the other reads a public third party -- and the
// blast radius of a bug in either is contained by their staying apart. A
// registry client that could reach docker.Runtime would also be a path from a
// hostile registry response toward the Docker socket.
func TestRegistryClientDoesNotImportTheDockerAdapter(t *testing.T) {
	for _, imp := range allImports(t) {
		dir := filepath.ToSlash(filepath.Dir(imp.rel))

		if dir == registryPackage && strings.HasPrefix(imp.path, adapterModule) {
			t.Errorf("%s imports %s\n"+
				"\tthe registry client reads an untrusted third party and must not be able to "+
				"reach the privileged Docker socket",
				imp.file, imp.path)
		}
		if dir == adapterPackage && strings.HasPrefix(imp.path, registryModule) {
			t.Errorf("%s imports %s\n"+
				"\tthe Docker adapter must not gain outbound network egress",
				imp.file, imp.path)
		}
	}
}

// anImport is one import declaration found in the repository.
type anImport struct {
	// file is a repo-relative path used in failure messages.
	file string
	// rel is the same path, kept separate so the directory comparison is not
	// coupled to how file is formatted.
	rel  string
	path string
}

// allImports parses every Go file in the module and returns its imports.
//
// Build tags are ignored on purpose: parser.ImportsOnly reads the import block
// of every file regardless of constraints, so a file that only compiles under
// the integration tag is checked too.
func allImports(t *testing.T) []anImport {
	t.Helper()

	root := moduleRoot(t)
	var found []anImport

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			// Vendored, generated, and dependency trees are not ours to police.
			case ".git", "node_modules", "vendor", "bin", "dist", "data":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		fileSet := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		for _, spec := range parsed.Imports {
			unquoted, quoteErr := strconv.Unquote(spec.Path.Value)
			if quoteErr != nil {
				continue
			}
			found = append(found, anImport{file: rel, rel: rel, path: unquoted})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no imports found; the walk is not reaching the source tree")
	}
	return found
}

// moduleRoot walks up from the test's directory to the directory holding
// go.mod, so the test does not depend on where it is run from.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod above the test directory")
		}
		dir = parent
	}
}
