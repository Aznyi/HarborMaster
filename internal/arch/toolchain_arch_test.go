package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The Go toolchain version is declared in four places and they must agree.
//
// # The failure this exists to prevent
//
// Three workflows pin `GO_VERSION` and the Dockerfile pins `GO_IMAGE`. They had
// all named the LINE, "1.26", on the reasoning that a range resolves to the
// newest patch and a `toolchain` directive in go.mod would settle any doubt.
//
// Neither held. A runner resolved "1.26" to go1.26.5 while the release image was
// built from golang:1.26.6, and govulncheck reported five standard library
// advisories -- GO-2026-6218, GO-2026-6090, GO-2026-6089, GO-2026-5972 and
// GO-2026-5026 -- every one already fixed in 1.26.6. The go.mod `toolchain`
// floor did not rescue it: the scanners analyse with whatever toolchain the
// setup action put on the runner.
//
// The consequence is worse than a red build. A security gate that scans a
// DIFFERENT standard library than the artefact ships is not measuring the
// artefact, and it can fail in the reassuring direction just as easily as it
// failed in this one: had the runner resolved a NEWER patch than the image, the
// advisories present in the shipped binary would have gone unreported.
//
// So the agreement is asserted here rather than left to a comment asking people
// to remember. Raising the Go version means changing four lines, and this test
// names the ones that were missed.

// goVersionPattern matches a full three-part Go version and nothing shorter.
var goVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

var (
	workflowGoVersion = regexp.MustCompile(`(?m)^\s*GO_VERSION:\s*"([^"]+)"`)
	dockerfileGoImage = regexp.MustCompile(`(?m)^ARG GO_IMAGE=golang:([0-9][^-\s]*)`)
)

func TestEveryToolchainPinNamesTheSameGoPatch(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)

	declared := map[string]string{}

	for _, workflow := range []string{"ci.yml", "security.yml", "codeql.yml"} {
		path := filepath.Join(root, ".github", "workflows", workflow)
		source, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
		if err != nil {
			t.Fatalf("read %s: %v", workflow, err)
		}
		match := workflowGoVersion.FindSubmatch(source)
		if match == nil {
			t.Fatalf("%s declares no GO_VERSION; a workflow that does not name its "+
				"Go version builds and scans with whatever the runner defaults to",
				workflow)
		}
		declared[".github/workflows/"+workflow] = string(match[1])
	}

	source, err := os.ReadFile(filepath.Join(root, "Dockerfile")) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	match := dockerfileGoImage.FindSubmatch(source)
	if match == nil {
		t.Fatal("the Dockerfile declares no GO_IMAGE=golang:<version>; the image the " +
			"release is compiled in is then unpinned")
	}
	declared["Dockerfile"] = string(match[1])

	// Every pin names a full patch. A range is what caused the skew.
	for where, version := range declared {
		if !goVersionPattern.MatchString(version) {
			t.Errorf("%s pins Go %q, which is a range rather than a patch; it can "+
				"resolve to a different standard library than the release ships",
				where, version)
		}
	}

	// And they all name the SAME patch.
	var reference, referenceFrom string
	for _, where := range []string{
		".github/workflows/ci.yml",
		".github/workflows/security.yml",
		".github/workflows/codeql.yml",
		"Dockerfile",
	} {
		version := declared[where]
		if reference == "" {
			reference, referenceFrom = version, where
			continue
		}
		if version != reference {
			t.Errorf("%s pins Go %s but %s pins Go %s; the gates and the artefact "+
				"would disagree about which standard library was compiled in",
				where, version, referenceFrom, reference)
		}
	}
}
