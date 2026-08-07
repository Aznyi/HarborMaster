package arch_test

import (
	"go/ast"
	"go/token"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Dependency reachability.
//
// # Why a test rather than an entry in a triage document
//
// `docs/security-triage.md` records advisories that were assessed and not
// remediated, and every entry rests on a claim about what HarborMaster actually
// uses. A claim like "we import argon2 from this module and nothing else" is
// true on the day it is written and silently untrue the day somebody adds an
// import.
//
// An ignored advisory whose justification has quietly expired is worse than an
// unignored one: the scanner has been told to stop asking, and nobody is
// looking. So each ignore that rests on non-use gets a test that the non-use
// still holds.

// bannedFromTheWholeBuild must not appear anywhere in the package graph,
// first-party or transitive.
//
// One entry, and the bar for adding another is high: this is a claim about code
// nobody here wrote. It is the right bar for THIS package because the scanner
// that reports the advisory works at module granularity — Trivy would report
// the module whatever our own imports said, so "not imported by us" would not
// answer it.
var bannedFromTheWholeBuild = map[string]string{
	// GO-2026-5932. The package is unmaintained, unsafe by design, and has no
	// fixed version and never will: the advisory is that the package exists,
	// not that a release broke it.
	//
	// HarborMaster requires golang.org/x/crypto for argon2 (password
	// verifiers) and blake2b. Trivy reports the advisory against the MODULE,
	// because a manifest or binary scan cannot tell which packages a build
	// uses, so the alert is ignored in .trivyignore.yaml. This is the check
	// that keeps that ignore honest.
	"golang.org/x/crypto/openpgp": "unmaintained and unsafe by design; see GO-2026-5932",
}

// bannedFromShippedCode must not be imported by HarborMaster's own non-test
// files.
//
// A weaker claim than the one above, deliberately. Some of these ARE in the
// package graph via a dependency — `os/exec` arrives through modernc.org/libc,
// the pure-Go SQLite driver's C runtime shim — and a transitive package that
// nothing calls is not something this repository controls or claims anything
// about. What it can claim is that no line of HarborMaster reaches for one.
var bannedFromShippedCode = map[string]string{
	"os/exec":       "HarborMaster runs no subprocess; see docs/security/threat-model.md",
	"os/user":       "no identity is read from the host's account database",
	"text/template": "no template execution; a template engine is an execution surface",
	"html/template": "the frontend is a static bundle; the server renders no HTML",
	"plugin":        "no plugin loading; the channel and rule vocabularies are closed",
	"net/http/cgi":  "no CGI",
}

// TestNothingBannedIsAnywhereInTheBuild resolves the real transitive package
// set.
//
// `go list -deps` rather than parsing imports, because the question is about the
// WHOLE build: a first-party import scan would miss a dependency that pulls one
// of these in, and that is exactly the case an advisory is filed about.
func TestNothingBannedIsAnywhereInTheBuild(t *testing.T) {
	t.Parallel()

	present := buildPackageSet(t)

	for path, why := range bannedFromTheWholeBuild {
		if !present[path] {
			continue
		}
		t.Errorf("%s is in the build: %s\n\n"+
			"If this import is deliberate, remove the entry AND revisit what "+
			"rested on its absence -- for golang.org/x/crypto/openpgp that is "+
			"the .trivyignore.yaml entry for GO-2026-5932, which exists only "+
			"because the package is unused.", path, why)
	}
}

// TestNoShippedFileImportsABannedPackage reads HarborMaster's own imports.
//
// # Why test files are excluded, and why that is not a loophole
//
// The claim being defended is about the SHIPPED BINARY: HarborMaster runs no
// subprocess, executes no template, loads no plugin. A test harness that drives
// the `docker` CLI to build a fixture, or this file's own `go list`, is not the
// product doing any of those — and neither is compiled into the binary.
//
// The claim is not weakened by the exclusion, because it is independently
// covered from the other direction: `go list -deps ./...` in the test above
// resolves the real package set WITHOUT test-only imports, so anything that
// reached the binary through a test file would still have to appear there.
func TestNoShippedFileImportsABannedPackage(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	var offenders []string

	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if strings.HasSuffix(relative, "_test.go") {
			return
		}

		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if why, banned := bannedFromShippedCode[path]; banned {
				offenders = append(offenders,
					relative+":"+strconv.Itoa(fset.Position(spec.Pos()).Line)+
						" imports "+path+" -- "+why)
			}
		}
	})

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("first-party code imports a banned package:\n\t%s",
			strings.Join(offenders, "\n\t"))
	}
}

// The module that carries the ignored advisory is used for what the triage says
// it is used for.
//
// Not "is x/crypto present" -- it is, deliberately -- but "is it present for the
// reason recorded". A build that started using a third package from it would not
// be wrong, but it would make the triage entry's reasoning stale, and a stale
// justification is how an ignore outlives the argument for it.
func TestTheCryptoModuleIsUsedOnlyForWhatTheTriageSays(t *testing.T) {
	t.Parallel()

	// What docs/security-triage.md claims HarborMaster imports from the module.
	expected := map[string]bool{
		"golang.org/x/crypto/argon2":  true,
		"golang.org/x/crypto/blake2b": true,
	}

	var unexpected []string
	for path := range buildPackageSet(t) {
		// The `vendor/golang.org/x/crypto/...` paths are the standard library's
		// own vendored copy, which is a different thing from the module this
		// repository requires.
		if !strings.HasPrefix(path, "golang.org/x/crypto/") {
			continue
		}
		if !expected[path] {
			unexpected = append(unexpected, path)
		}
	}

	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Errorf("the build imports golang.org/x/crypto packages the triage does "+
			"not account for:\n\t%s\n\n"+
			"docs/security-triage.md justifies ignoring GO-2026-5932 on the "+
			"grounds of which packages this module is used for. Update it, and "+
			"update `expected` here, so the reasoning matches the build.",
			strings.Join(unexpected, "\n\t"))
	}
}

// buildPackageSet resolves every package in the build.
func buildPackageSet(t *testing.T) map[string]bool {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		// Cannot happen under `go test`, which is how this always runs. Skipped
		// rather than failed so a vendored or sandboxed invocation reports
		// honestly instead of inventing a verdict.
		t.Skip("the go tool is not on PATH; cannot resolve the package set")
	}

	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = moduleRoot(t)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	// Matched as whole lines: `go list` prints one import path per line, and a
	// substring sweep would find this file's own mention of them.
	present := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		present[strings.TrimSpace(line)] = true
	}
	if len(present) < 100 {
		t.Fatalf("go list -deps returned %d packages; that is not the whole build",
			len(present))
	}
	return present
}
