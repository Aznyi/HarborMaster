package arch_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
)

// Architecture tests for the rollback capability.
//
// Phase 10 gave HarborMaster a THIRD Docker mutation interface. These tests are
// what keep it the narrowest of the three, and what make each of its limits a
// property the build checks rather than a claim a comment makes.
//
// The limits, stated once:
//
//  1. Four methods. Not five, and specifically not create or remove.
//  2. Every method targets a FULL container id. Nothing can be aimed by name.
//  3. Exactly one package may name the interface.
//  4. The two rename operations can only move a container in one direction
//     each, so neither is a general-purpose rename.

// TestTheRollbackSurfaceIsExactlyFourMethods pins the whole capability.
//
// If this test needs editing, the change under review is HarborMaster gaining a
// new power over a privileged socket. That is the point: the diff cannot be
// quiet.
func TestTheRollbackSurfaceIsExactlyFourMethods(t *testing.T) {
	rollbackerType := reflect.TypeOf((*docker.ContainerRollbacker)(nil)).Elem()

	want := map[string]bool{
		"StopReplacement":     true,
		"ParkReplacement":     true,
		"RestoreOriginalName": true,
		"StartOriginal":       true,
	}

	if got := rollbackerType.NumMethod(); got != len(want) {
		t.Fatalf("docker.ContainerRollbacker has %d methods, want exactly %d\n"+
			"\tthis interface is the WHOLE of HarborMaster's ability to undo a recreation; "+
			"a fifth method is a fifth capability and needs its own review, its own threat "+
			"model entry, and its own tests", got, len(want))
	}

	got := make(map[string]bool, rollbackerType.NumMethod())
	for i := 0; i < rollbackerType.NumMethod(); i++ {
		got[rollbackerType.Method(i).Name] = true
	}
	for name := range got {
		if !want[name] {
			t.Errorf("docker.ContainerRollbacker gained method %q\n"+
				"\tevery method here is a capability against a privileged socket, and this "+
				"set is exactly what the rollback pipeline needs and nothing more", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("docker.ContainerRollbacker no longer has method %q; update this test "+
				"if the removal is intended", name)
		}
	}
}

// forbiddenRollbackVerbs are capabilities the rollback interface must never
// gain.
//
// CREATE and REMOVE lead the list, and they are the reason this is a separate
// interface rather than more methods on ContainerMutator:
//
//   - A rollback that could CREATE would be a restore. Restoring a container
//     from a snapshot needs evidence a rollback does not have -- that the
//     captured configuration is still reproducible, that every referenced
//     volume and network still exists -- and is a different feature.
//   - A rollback that could REMOVE would destroy the failed replacement, which
//     is the evidence of why the rollback was needed in the first place.
var forbiddenRollbackVerbs = []string{
	"Create", "Remove", "Delete", "Prune",
	"Exec", "Attach", "Copy", "Commit", "Kill", "Pause", "Unpause",
	"Update", "Wait", "Export", "Import", "Push", "Pull", "Build",
	"Image", "Volume", "Network", "Secret", "Config", "Swarm", "Service", "Node",
}

// TestTheRollbackInterfaceCannotCreateOrDestroy fails if a forbidden verb
// appears.
//
// Deliberately separate from the count above. A reviewer relaxing the count
// still has to get past this, and the two failures say different things.
func TestTheRollbackInterfaceCannotCreateOrDestroy(t *testing.T) {
	rollbackerType := reflect.TypeOf((*docker.ContainerRollbacker)(nil)).Elem()

	for i := 0; i < rollbackerType.NumMethod(); i++ {
		name := rollbackerType.Method(i).Name
		for _, verb := range forbiddenRollbackVerbs {
			if !strings.Contains(name, verb) {
				continue
			}
			t.Errorf("docker.ContainerRollbacker has method %q, which contains %q\n"+
				"\ta rollback moves containers that already exist back to an arrangement "+
				"HarborMaster recorded; it may not create one, destroy one, or reach any "+
				"other kind of Docker resource", name, verb)
		}
	}
}

// TestEveryRollbackRequestTargetsAFullContainerID is the TOCTOU defence.
//
// Between the preflight that checks a container and the mutation that moves it,
// a NAME can come to mean a different container. An id cannot. So every request
// type carries an id field for its target and no name field for it, and every
// Validate refuses anything that is not a full 64-character id.
//
// The rename requests do carry a name -- the name being written -- which is a
// different thing from the name of the target, and is checked separately below.
func TestEveryRollbackRequestTargetsAFullContainerID(t *testing.T) {
	const short = "abc123"
	const full = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := map[string]struct {
		valid   func() error
		invalid []func() error
	}{
		"StopReplacement": {
			valid: func() error {
				return docker.RollbackStopRequest{ReplacementID: full}.Validate()
			},
			invalid: []func() error{
				func() error { return docker.RollbackStopRequest{}.Validate() },
				func() error { return docker.RollbackStopRequest{ReplacementID: short}.Validate() },
				func() error { return docker.RollbackStopRequest{ReplacementID: "web"}.Validate() },
			},
		},
		"StartOriginal": {
			valid: func() error {
				return docker.RollbackStartRequest{OriginalID: full}.Validate()
			},
			invalid: []func() error{
				func() error { return docker.RollbackStartRequest{}.Validate() },
				func() error { return docker.RollbackStartRequest{OriginalID: short}.Validate() },
			},
		},
		"ParkReplacement": {
			valid: func() error {
				return docker.RollbackParkRequest{
					ReplacementID: full,
					ParkedName:    "web.hm-rolledback-rbk_0123456789abcdef0123",
				}.Validate()
			},
			invalid: []func() error{
				func() error {
					return docker.RollbackParkRequest{
						ParkedName: "web.hm-rolledback-rbk_0123456789abcdef0123",
					}.Validate()
				},
				func() error {
					return docker.RollbackParkRequest{
						ReplacementID: short,
						ParkedName:    "web.hm-rolledback-rbk_0123456789abcdef0123",
					}.Validate()
				},
			},
		},
		"RestoreOriginalName": {
			valid: func() error {
				return docker.RollbackRestoreRequest{OriginalID: full, Name: "web"}.Validate()
			},
			invalid: []func() error{
				func() error {
					return docker.RollbackRestoreRequest{Name: "web"}.Validate()
				},
				func() error {
					return docker.RollbackRestoreRequest{OriginalID: short, Name: "web"}.Validate()
				},
			},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if err := testCase.valid(); err != nil {
				t.Fatalf("a request naming a full container id was refused: %v", err)
			}
			for i, build := range testCase.invalid {
				if err := build(); err == nil {
					t.Errorf("case %d: a request WITHOUT a full container id was accepted\n"+
						"\tevery rollback mutation must target an exact id; a name can come "+
						"to mean a different container between the check and the act", i)
				}
			}
		})
	}
}

// TestTheRenameOperationsCanOnlyMoveAContainerOneWay pins what stops the two
// rename methods being interchangeable.
//
// ParkReplacement may only write a name carrying HarborMaster's own rollback
// marker, so it can move a container OUT of the production name and nowhere
// else. RestoreOriginalName may only write a name carrying NO marker, so it can
// return a container to a name a human chose and cannot be used to park one.
//
// Without these, the two methods would be one general-purpose rename, and a
// rollback could give any container it holds any name it liked.
func TestTheRenameOperationsCanOnlyMoveAContainerOneWay(t *testing.T) {
	const full = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// A park may not write a plain production name.
	for _, name := range []string{"web", "api", "web.hm-old-exec_1", "web.hm-failed-exec_1"} {
		if err := (docker.RollbackParkRequest{
			ReplacementID: full, ParkedName: name,
		}).Validate(); err == nil {
			t.Errorf("ParkReplacement accepted the name %q\n"+
				"\tit may only move a container to a rollback parked name; anything else "+
				"makes it a general-purpose rename", name)
		}
	}

	// A restore may not write a parked or quarantine name.
	for _, name := range []string{
		"web.hm-rolledback-rbk_0123456789abcdef0123",
		"web.hm-old-exec_0123456789abcdef0123",
		"web.hm-failed-exec_0123456789abcdef0123",
	} {
		if err := (docker.RollbackRestoreRequest{
			OriginalID: full, Name: name,
		}).Validate(); err == nil {
			t.Errorf("RestoreOriginalName accepted the name %q\n"+
				"\tit may only return a container to its own name; writing a derived name "+
				"would make the two rename methods interchangeable", name)
		}
	}
}

// rollbackAllowed is the set of directories that may name the rollback
// capability.
//
// ONE package holds it, plus the adapter that defines it and the command that
// hands it over. Everything else -- the API, the store, the domain, every other
// service -- must not be able to reach it.
var rollbackAllowed = map[string]bool{
	"internal/docker":  true, // defines it
	"internal/service": true, // the one service that holds it
	"cmd/harbormaster": true, // wires it
	"internal/arch":    true, // this test
	// The live-Docker suite exercises the four operations against a real daemon,
	// which is the end-to-end proof that the adapter behaves as the fake models
	// it -- and it asserts that the displaced replacement is still on the host
	// afterwards. Build-tagged, so it is never part of an ordinary build.
	"internal/integration": true,
}

// TestTheRollbackCapabilityIsNotReferencedOutsideItsOwners fails if any other
// package names the interface.
//
// The API layer is the one that matters. A handler able to reach the rollbacker
// could move containers without the preflight, which is the entire safety
// model. Capability is granted by what a constructor is handed, and this test
// is what keeps the list of things that are handed it short enough to read.
func TestTheRollbackCapabilityIsNotReferencedOutsideItsOwners(t *testing.T) {
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
		if rollbackAllowed[filepath.ToSlash(filepath.Dir(rel))] {
			return nil
		}

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		for _, symbol := range []string{
			"ContainerRollbacker",
			"RollbackStopRequest", "RollbackParkRequest",
			"RollbackRestoreRequest", "RollbackStartRequest",
		} {
			if !strings.Contains(string(source), symbol) {
				continue
			}
			t.Errorf("%s names docker.%s\n"+
				"\tthe rollback capability is held by one service; a package that can name "+
				"it is a package that could move containers without the preflight",
				rel, symbol)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// TestTheRollbackServiceHoldsNoOtherMutationCapability keeps the three
// capabilities separate.
//
// A service that held both the mutator and the rollbacker could create and
// remove containers as part of a rollback, which is exactly what the separate
// interface exists to prevent. The check is on the FILES that implement the
// rollback: they may name the rollbacker and must not name the mutator or the
// acquirer.
func TestTheRollbackServiceHoldsNoOtherMutationCapability(t *testing.T) {
	root := moduleRoot(t)
	serviceDir := filepath.Join(root, "internal", "service")

	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		t.Fatalf("read internal/service: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "rollback") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		source, readErr := os.ReadFile(filepath.Join(serviceDir, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}

		for _, forbidden := range []string{
			"ContainerMutator", "ImageAcquirer", "ConfigCapturer",
			"CreateContainer", "RemoveContainer",
		} {
			if strings.Contains(string(source), forbidden) {
				t.Errorf("internal/service/%s names docker.%s\n"+
					"\tthe rollback pipeline holds ONE capability; naming another means it "+
					"could create or destroy a container as part of undoing a recreation",
					name, forbidden)
			}
		}
	}
}
