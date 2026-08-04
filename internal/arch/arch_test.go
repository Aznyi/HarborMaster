// Package arch holds architecture tests: rules about the SHAPE of the
// codebase that a reviewer would otherwise have to enforce by memory.
//
// These are guardrails for two invariants that are easy to break by accident
// and expensive to notice late:
//
//   - HarborMaster talks to a privileged Docker socket through exactly one
//     adapter package, so the SDK cannot spread into the domain, services,
//     repositories, or API handlers.
//   - That adapter is read-only. Gaining the ability to start, stop, remove, or
//     exec into a container must require editing the Runtime interface, which
//     is what makes it visible in review.
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
