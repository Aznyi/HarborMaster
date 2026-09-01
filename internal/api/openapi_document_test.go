package api_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// `api/openapi.yaml` is the published contract, and until this test existed
// nothing checked that it was a loadable document.
//
// # The failures this exists to prevent
//
// The parity test next door reads the spec with a regular expression and
// compares its paths against the router. That answers "are the same endpoints
// named in both places" and nothing else — in particular it answers nothing
// about whether the file can be PARSED, because it never parses it. Two
// defects lived behind that gap through every green build:
//
//  1. **The document was not valid YAML.** A `description:` written as a plain
//     unquoted scalar continued onto a second line, and that line contained
//     "`present: false`". In block context a plain scalar may not contain ": ",
//     so a conforming parser stops there and rejects the WHOLE file. Swagger
//     UI, Redoc, Prism and every code generator load this document by parsing
//     it; not one of them could.
//
//  2. **Sixteen `$ref`s pointed at components that do not exist.** Three
//     endpoint groups referenced `responses/Unauthorized`, `responses/Disabled`,
//     `responses/BadRequest` and `schemas/Error` — plausible names, none of them
//     declared. The document's own vocabulary is `Unauthenticated`,
//     `AutomationUnavailable`, `InvalidRequest` and `ErrorResponse`. A dangling
//     reference is not a cosmetic problem: a generator either fails outright or
//     emits a client whose error handling for those statuses is missing.
//
// Both are the same underlying mistake — trusting that a document nothing loads
// is a document that works. So this test loads it, in the only two senses that
// matter to a consumer: it must be parseable, and every reference in it must
// resolve.
//
// It is written against the file's text rather than a YAML library on purpose.
// The module has no YAML dependency, and taking one on to check a static asset
// would put a parser in the supply chain of a container manager to satisfy a
// test. The two rules below are narrow enough to state exactly.

var (
	// A `key: value` whose value is a plain scalar — not a block scalar (`|`,
	// `>`), not quoted, not an alias or anchor, and not the start of a flow
	// collection. Only these continue onto following lines as plain text.
	plainScalarKey = regexp.MustCompile(`^(\s*)([A-Za-z0-9_.$-]+|"[^"]*"):[ \t]+([^|>&*"'\[{#].*)$`)

	// Every local reference in the document.
	localRef = regexp.MustCompile(`\$ref:\s*"(#/[^"]+)"`)

	// A component declaration: `    Name:` directly under a `  <kind>:` section
	// of the top-level `components:` mapping.
	componentSection = regexp.MustCompile(`^  ([a-zA-Z]+):\s*$`)
	componentName    = regexp.MustCompile(`^    ([A-Za-z0-9_.-]+):\s*$`)
)

func openapiLines(t *testing.T) []string {
	t.Helper()

	source, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	return strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// TestOpenAPIHasNoPlainScalarThatBreaksParsing fails on the construct that made
// the document unparseable.
func TestOpenAPIHasNoPlainScalarThatBreaksParsing(t *testing.T) {
	t.Parallel()

	lines := openapiLines(t)

	for i, line := range lines {
		match := plainScalarKey.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1])

		// Walk the continuation lines of this plain scalar: more-indented,
		// non-blank, and not themselves a new key or list item.
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.TrimSpace(next) == "" || indentOf(next) <= indent {
				break
			}
			trimmed := strings.TrimSpace(next)
			if strings.HasPrefix(trimmed, "- ") {
				break
			}

			if strings.Contains(trimmed, ": ") {
				t.Errorf("api/openapi.yaml:%d continues the plain scalar opened at "+
					"line %d and contains \": \":\n\n\t%s\n\n"+
					"A plain (unquoted) scalar may not contain \": \" in block "+
					"context, so a conforming YAML parser rejects the entire "+
					"document at this line — every generator, validator and "+
					"documentation renderer along with it. Write the value as a "+
					"block scalar (`description: >-` on its own line, text "+
					"indented beneath) or quote it.",
					j+1, i+1, trimmed)
			}
		}
	}
}

// TestEveryOpenAPIReferenceResolves fails on a `$ref` naming a component the
// document does not declare.
func TestEveryOpenAPIReferenceResolves(t *testing.T) {
	t.Parallel()

	lines := openapiLines(t)

	// Collect declared components, keyed as `components/<kind>/<Name>`.
	declared := map[string]bool{}
	inComponents := false
	section := ""
	for _, line := range lines {
		if line == "components:" {
			inComponents = true
			continue
		}
		if !inComponents {
			continue
		}
		// A new top-level key ends the components block.
		if line != "" && indentOf(line) == 0 && !strings.HasPrefix(line, " ") {
			break
		}
		if match := componentSection.FindStringSubmatch(line); match != nil {
			section = match[1]
			continue
		}
		if match := componentName.FindStringSubmatch(line); match != nil && section != "" {
			declared["components/"+section+"/"+match[1]] = true
		}
	}

	if len(declared) == 0 {
		t.Fatal("found no component declarations in api/openapi.yaml; the document's " +
			"shape has changed and this test is checking nothing")
	}

	var broken []string
	seen := map[string]bool{}
	for _, line := range lines {
		for _, match := range localRef.FindAllStringSubmatch(line, -1) {
			ref := strings.TrimPrefix(match[1], "#/")
			if seen[ref] {
				continue
			}
			seen[ref] = true
			if !declared[ref] {
				broken = append(broken, ref)
			}
		}
	}

	sort.Strings(broken)
	for _, ref := range broken {
		t.Errorf("api/openapi.yaml references #/%s, which the document does not "+
			"declare. A dangling reference is not cosmetic: a generator either "+
			"fails on the document or emits a client missing the behaviour this "+
			"reference described.", ref)
	}
}
