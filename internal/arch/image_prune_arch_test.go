package arch_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
)

// The fourth Docker capability, bounded like the other three.
//
// HarborMaster's mutation surface is separate interfaces, each held by exactly
// one service and each absent unless the deployment opts in: ImageAcquirer
// pulls, ContainerMutator recreates, ContainerRollbacker reverts, and now
// ImagePruner removes. The acquirer's own comment says what it may never become
// -- "no image removal, no prune" -- which is why removal is a fourth interface
// rather than a fifth acquirer method.
//
// These tests are the price of that fourth capability: a count, a container
// prohibition, a force prohibition, and an ownership rule. They are modelled on
// the tests that already guard the other three, so relaxing this one looks
// exactly as deliberate as relaxing those would.

func TestTheImagePruneSurfaceIsExactlyOneMethod(t *testing.T) {
	pruner := reflect.TypeOf((*docker.ImagePruner)(nil)).Elem()

	if got := pruner.NumMethod(); got != 1 {
		t.Fatalf("docker.ImagePruner has %d methods, want exactly 1\n"+
			"\tthis interface is the WHOLE of HarborMaster's ability to destroy an "+
			"image. A second method is a second capability and needs its own "+
			"review, its own threat model entry, and its own tests", got)
	}
	if name := pruner.Method(0).Name; name != "RemoveImage" {
		t.Errorf("the prune method is %q, want RemoveImage\n"+
			"\tthe name states the scope: ONE image, by id. A prune-all or a "+
			"dangling sweep is a different operation with a different blast "+
			"radius", name)
	}
}

func TestTheImagePrunerCannotTouchAContainer(t *testing.T) {
	// Removing an image changes the image store and nothing else. These verbs
	// would cross from "clean up" into "apply", which is a capability this
	// interface must never acquire.
	forbidden := []string{
		"attach", "commit", "connect", "copy", "create", "exec", "kill",
		"pause", "recreate", "rename", "restart", "restore", "rollback", "run",
		"start", "stop", "unpause", "update", "wait",
	}

	pruner := reflect.TypeOf((*docker.ImagePruner)(nil)).Elem()
	for i := 0; i < pruner.NumMethod(); i++ {
		name := strings.ToLower(pruner.Method(i).Name)
		for _, verb := range forbidden {
			if strings.Contains(name, verb) {
				t.Errorf("docker.ImagePruner has %q, which names the container verb %q\n"+
					"\tcleanup removes artefacts from the image store; it does not "+
					"touch anything that is running",
					pruner.Method(i).Name, verb)
			}
		}
	}
}

func TestTheImagePrunerCannotPruneEverything(t *testing.T) {
	// A sweep is not a removal. "Prune all dangling images" is one call that
	// destroys an unbounded set HarborMaster never assessed, which is precisely
	// the Watchtower-shaped behaviour this design refuses.
	pruner := reflect.TypeOf((*docker.ImagePruner)(nil)).Elem()
	for i := 0; i < pruner.NumMethod(); i++ {
		name := strings.ToLower(pruner.Method(i).Name)
		for _, sweep := range []string{"prune", "all", "dangling", "sweep", "clear"} {
			if strings.Contains(name, sweep) {
				t.Errorf("docker.ImagePruner has %q, which names the sweep verb %q\n"+
					"\tevery removal must be one image HarborMaster assessed by id",
					pruner.Method(i).Name, sweep)
			}
		}
	}
}

// forceInRemoval matches an attempt to force a removal.
//
// Narrow on purpose: `Force: true` or a force query parameter in the removal
// path. The literal `Force: false` the client passes is what the rule requires,
// so it must not match.
var forceInRemoval = regexp.MustCompile(`(?i)Force:\s*true|force\s*=\s*true|"force"\s*,\s*"1"`)

func TestImageRemovalNeverForces(t *testing.T) {
	// THE HARD INVARIANT. Docker refusing a removal because something still
	// uses the image is the daemon independently confirming HarborMaster's
	// decision was wrong. Forcing past it would delete an artefact a running
	// container depends on, which is the one outcome cleanup exists to avoid.
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "docker", "prune.go")

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prune.go: %v", err)
	}

	for index, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if forceInRemoval.MatchString(line) {
			t.Errorf("internal/docker/prune.go:%d forces an image removal:\n\t%s\n\n"+
				"Automatic cleanup must never force. If the daemon says an image "+
				"is in use, that is the answer and the answer is keep.",
				index+1, trimmed)
		}
	}

	// Non-vacuity: the file really does pass Force, explicitly false. A
	// refactor that dropped the option entirely would let the daemon default
	// apply, and this test would otherwise pass on a file it never checked.
	if !strings.Contains(string(source), "Force:         false") {
		t.Error("prune.go no longer passes an explicit Force: false.\n\n" +
			"The explicit literal is the point: it states the rule at the call " +
			"site rather than relying on a zero value that a future SDK could " +
			"reinterpret.")
	}
}

// TestTheImagePruneCapabilityIsNotReferencedOutsideItsOwners fails if a package
// that has no business removing images names the capability.
//
// The same technique, and the same honest limit, as the tests guarding the
// acquisition and recreation capabilities: it checks the SOURCE for the
// identifier rather than the import list, because internal/api legitimately
// imports internal/docker for its read-only error types.
func TestTheImagePruneCapabilityIsNotReferencedOutsideItsOwners(t *testing.T) {
	// Where the capability may legitimately appear: the package that defines
	// it, the service that holds it, and the composition root that grants it.
	allowed := map[string]bool{
		"internal/docker":  true,
		"internal/service": true,
		"cmd/harbormaster": true,
	}

	root := moduleRoot(t)
	var offenders []string

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
		if !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if allowed[dir] {
			return nil
		}

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(source), "ImagePruner") ||
			strings.Contains(string(source), "RemoveImage(") {
			offenders = append(offenders, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("the image-removal capability is named outside its owners:\n\t%s\n\n"+
			"Image removal is held by ONE service. A package that can reach it "+
			"is a package that can destroy the artefact a recovery depends on.",
			strings.Join(offenders, "\n\t"))
	}
}

// TestThePruneCapabilityIsGrantedOnlyWhenCleanupIsEnabled reads the composition
// root.
//
// The same lesson the self-identity guard records: a guarantee that depends on
// a few lines in main is a guarantee that needs a test on main. Every service
// test supplies its own pruner, so a main.go that handed the real client over
// unconditionally would pass the whole suite while granting the ability to
// destroy images to a deployment that never asked for it.
func TestThePruneCapabilityIsGrantedOnlyWhenCleanupIsEnabled(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "cmd", "harbormaster", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)

	for _, required := range []struct {
		fragment string
		why      string
	}{
		{"var pruner docker.ImagePruner",
			"the capability is not declared as a nil-able variable, so it cannot be withheld"},
		{"if cfg.ImageCleanup.Enabled {",
			"the grant is not gated on the deployment having asked for it"},
		{"Pruner:  pruner,",
			"the service is not given the gated variable, which means it is given something else"},
		{"Self:    self,",
			"cleanup cannot recognise HarborMaster's own image, so it could remove the image it is running"},
		{"Runtime: dockerClient,",
			"cleanup cannot re-verify against the live host before it removes"},
	} {
		if !strings.Contains(text, required.fragment) {
			t.Errorf("cmd/harbormaster/main.go does not contain %q\n\t%s\n\n"+
				"Image removal is the one capability whose mistakes HarborMaster "+
				"cannot undo. It is granted deliberately or not at all.",
				required.fragment, required.why)
		}
	}

	// The grant must appear ONCE. A second `pruner = dockerClient` outside the
	// conditional would restore the capability after it was withheld.
	if got := strings.Count(text, "pruner = dockerClient"); got != 1 {
		t.Errorf("main.go assigns the prune capability %d times, want exactly 1\n\n"+
			"One assignment, inside one conditional. A second one is how a "+
			"capability that looks gated stops being gated.", got)
	}
}
