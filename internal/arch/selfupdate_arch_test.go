package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Architecture tests for self-update protection.
//
// # What must be true, and why a test rather than a comment
//
// HarborMaster must never update the container it is running in. Not because
// the result would be wrong, but because the operation cannot COMPLETE: the
// process stops its own container and dies between the stop and the
// checkpoint. Nothing verifies the replacement, nothing records what happened,
// and no rollback is possible because the thing that would perform it is gone.
//
// The defence is FOUR independent refusals:
//
//	automation_decide.go     check 0, before every other check a pass makes
//	automation_query.go      the approval path, which is a second caller
//	acquisition.go           preflight, before the download
//	execution_preflight.go   preflight, before the stop
//
// These tests fail the build if any one of them disappears. Four is not
// belt-and-braces for its own sake -- each covers a caller the others do not:
// a scheduled pass, an operator releasing a held decision, a direct
// POST /acquisitions, and a direct POST /executions.

// selfRefusalSites are the files that must each carry a refusal, and the
// symbol that proves it.
var selfRefusalSites = map[string]string{
	"internal/service/automation_decide.go":   "ReasonSelfUpdate",
	"internal/service/automation_query.go":    "SelfMatch",
	"internal/service/acquisition.go":         "AcquisitionRefusalSelfUpdate",
	"internal/service/execution_preflight.go": "ExecutionRefusalSelfUpdate",
}

// TestEveryMutationPathRefusesASelfUpdate fails if a refusal is removed.
//
// If this test needs editing, the change under review removes one of the three
// things stopping HarborMaster from killing itself mid-operation.
func TestEveryMutationPathRefusesASelfUpdate(t *testing.T) {
	root := moduleRoot(t)

	for rel, symbol := range selfRefusalSites {
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v\n"+
				"\tthis file carries one of the three self-update refusals; if it moved, "+
				"move this test with it rather than deleting the entry", rel, err)
			continue
		}
		text := string(source)

		if !strings.Contains(text, symbol) {
			t.Errorf("%s no longer names %s\n"+
				"\tHarborMaster must never update the container it is running in: the "+
				"process stops its own container and dies before it can verify, "+
				"checkpoint, or roll back", rel, symbol)
		}
		// The refusal has to be reached through SelfMatch, not through an
		// open-coded comparison that a later edit could get subtly wrong.
		if !strings.Contains(text, "SelfMatch(") {
			t.Errorf("%s names %s but does not call SelfMatch\n"+
				"\tthe comparison lives in one place on purpose: it is layered over "+
				"several independent signals, and an open-coded id check would drop "+
				"the image and label signals silently", rel, symbol)
		}
	}
}

// TestTheSelfUpdateRefusalCannotBeConfiguredAway fails if a setting appears
// that would relax it.
//
// Every other capability in HarborMaster has an off switch. This one has no ON
// switch, and that asymmetry is the design: there is no deployment in which
// stopping your own container partway through an operation is what somebody
// wanted.
func TestTheSelfUpdateRefusalCannotBeConfiguredAway(t *testing.T) {
	root := moduleRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "internal", "config", "config.go"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(source)

	for _, forbidden := range []string{
		"AllowSelfUpdate",
		"ALLOW_SELF_UPDATE",
		"SelfUpdateEnabled",
		"SELF_UPDATE_ENABLED",
		"PermitSelfUpdate",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("configuration names %q\n"+
				"\ta self-update cannot be made to work by configuring it: the process "+
				"stops its own container and dies. A setting here would offer an "+
				"operation that is guaranteed to end badly", forbidden)
		}
	}
}

// TestSelfIdentityMatchesNothingWhenItKnowsNothing is the fail-safe direction.
//
// A zero SelfIdentity is what a build with no detection wired, and a deployment
// where every probe failed, both produce. It must exclude NOTHING: an identity
// that matched every container whose id was also blank would silently stop
// automation across the estate, and an operator would turn the protection off
// to get their updates back.
func TestSelfIdentityMatchesNothingWhenItKnowsNothing(t *testing.T) {
	var unknown domain.SelfIdentity

	for _, target := range []domain.SelfTarget{
		{},
		{ContainerID: "a"},
		{ContainerName: "web"},
		{ImageRef: "nginx:1.27"},
		{ImageID: "sha256:abc"},
		{ContainerID: "a", ContainerName: "web", ImageRef: "nginx:1.27"},
	} {
		if matched, _ := unknown.SelfMatch(target); matched {
			t.Errorf("an unknown identity matched %+v\n"+
				"\tan identity that knows nothing must exclude nothing, or a failed "+
				"probe becomes an estate-wide outage of the update engine", target)
		}
	}
}

// TestAnIdentityWithoutASignalDoesNotMatchOnThatSignal covers the empty-field
// trap one field at a time.
//
// The failure this guards is subtle: a SelfIdentity that established an id but
// not an image must not match a container whose image is also blank. Every
// comparison requires BOTH sides to be present.
func TestAnIdentityWithoutASignalDoesNotMatchOnThatSignal(t *testing.T) {
	idOnly := domain.SelfIdentity{ContainerID: strings.Repeat("a", 64)}

	// A container with no id, no name, and no image is not HarborMaster.
	if matched, _ := idOnly.SelfMatch(domain.SelfTarget{}); matched {
		t.Error("an identity with only an id matched a target with nothing")
	}
	// Nor is one whose image happens to be blank.
	if matched, _ := idOnly.SelfMatch(domain.SelfTarget{
		ContainerID: strings.Repeat("b", 64),
		ImageRef:    "",
		ImageID:     "",
	}); matched {
		t.Error("a blank image matched a blank image")
	}

	imageOnly := domain.SelfIdentity{ImageRef: "ghcr.io/aznyi/harbormaster:0.9"}
	if matched, _ := imageOnly.SelfMatch(domain.SelfTarget{
		ContainerID: strings.Repeat("c", 64),
	}); matched {
		t.Error("an identity with only an image matched on an absent image")
	}
}

// The self identity is actually WIRED, at every site that refuses.
//
// # Why this test exists
//
// The refusals were written, tested, and pinned by the sites test above — and
// for a while nothing in the composition root constructed a SelfIdentifier at
// all. Every service held a nil Self, `selfIdentity()` returned the zero
// identity, the zero identity matches nothing, and all four refusals were
// unreachable. Every test still passed, because every test supplied its own
// identity.
//
// A guarantee that depends on a constructor call in main is a guarantee that
// needs a test on main.
func TestTheSelfIdentityIsWiredIntoEveryServiceThatRefuses(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "cmd", "harbormaster", "main.go")) //nolint:gosec // a path this test computed
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)

	for _, required := range []struct {
		fragment string
		why      string
	}{
		{"service.NewSelfIdentifier(", "nothing builds the identifier, so every service holds a nil Self"},
		{"self.Resolve(", "the identity is never established, so it stays the zero value until the first inventory refresh"},
		{"inventory.AddRefreshObserver(self)", "the identity is never refreshed, so a renamed container is excluded under its old name"},
	} {
		if !strings.Contains(text, required.fragment) {
			t.Errorf("cmd/harbormaster/main.go does not contain %q\n\t%s",
				required.fragment, required.why)
		}
	}

	// Three services refuse a self-update and each needs the identity. A
	// service constructed without it holds nil, and nil matches nothing.
	// Counted by REGEXP rather than by exact spelling, because gofmt chooses
	// the alignment and a test that depended on it would break on an unrelated
	// field being added to one of these structs.
	wired := regexp.MustCompile(`
\s*Self:\s+self,`).FindAllString(text, -1)
	if got := len(wired); got < 3 {
		t.Errorf("the self identity reaches %d services in main.go, want at least 3 "+
			"(acquisition, execution, automation)\n"+
			"\ta service constructed without it holds a nil Self, and the zero "+
			"identity matches nothing -- which makes its refusal unreachable",
			got)
	}
}
