package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The first-run state contract, across the language boundary.
//
// # Why this test exists
//
// `internal/domain/first_run.go` and `web/src/api/firstRun.ts` implement the
// same state machine, because the facts it reads come from four different read
// endpoints and composing them on the server would mean a route whose only job
// is to concatenate responses the client already has.
//
// Two implementations is a drift risk, and this is the mitigation. It reads the
// Go source for the vocabulary and the precedence, reads the TypeScript for the
// same, and fails when they disagree.
//
// # What drift would actually cost
//
// A state the TypeScript does not know about would fall through its switch and
// render nothing, or -- far worse -- fall into whichever branch happens to be
// last. A new Go state called something like `assessmentFailed` silently
// rendering as "ready" is exactly the class of failure Phase 17 keeps finding,
// so the vocabulary is checked rather than trusted.

var (
	goStatePattern = regexp.MustCompile(`FirstRun[A-Za-z]+\s+FirstRunState\s+=\s+"([a-zA-Z]+)"`)
	tsStatePattern = regexp.MustCompile(`\|\s+"([a-zA-Z]+)"`)
)

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{moduleRoot(t)}, parts...)...)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(source)
}

// goFirstRunStates extracts the vocabulary from the Go source.
func goFirstRunStates(t *testing.T) []string {
	t.Helper()

	source := readFile(t, "internal", "domain", "first_run.go")
	matches := goStatePattern.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		t.Fatal("found no first-run states in the Go source; the pattern no " +
			"longer matches and this test would pass having checked nothing")
	}

	states := make([]string, 0, len(matches))
	for _, match := range matches {
		states = append(states, match[1])
	}
	return states
}

// tsFirstRunStates extracts the union members from the TypeScript source.
func tsFirstRunStates(t *testing.T) []string {
	t.Helper()

	source := readFile(t, "web", "src", "api", "firstRun.ts")
	start := strings.Index(source, "export type FirstRunState =")
	if start < 0 {
		t.Fatal("the FirstRunState union is gone from firstRun.ts")
	}
	block := source[start:]
	if end := strings.Index(block, ";"); end > 0 {
		block = block[:end]
	}

	matches := tsStatePattern.FindAllStringSubmatch(block, -1)
	if len(matches) == 0 {
		t.Fatal("found no states in the TypeScript union")
	}

	states := make([]string, 0, len(matches))
	for _, match := range matches {
		states = append(states, match[1])
	}
	return states
}

// TestEveryGoFirstRunStateExistsInTypeScript is the vocabulary half.
func TestEveryGoFirstRunStateExistsInTypeScript(t *testing.T) {
	goStates := goFirstRunStates(t)
	tsStates := tsFirstRunStates(t)

	// Non-vacuity: the Go model has ten states. A pattern that silently matched
	// two would pass while nine were unrepresented.
	if len(goStates) < 10 {
		t.Fatalf("found %d Go first-run states; the vocabulary is not where this "+
			"test thinks it is", len(goStates))
	}

	inTS := make(map[string]bool, len(tsStates))
	for _, state := range tsStates {
		inTS[state] = true
	}
	for _, state := range goStates {
		if !inTS[state] {
			t.Errorf("the Go state %q has no TypeScript representation\n"+
				"\tadd it to the FirstRunState union, FIRST_RUN_HEADINGS and "+
				"firstRunExplanation in web/src/api/firstRun.ts -- an unrepresented "+
				"state renders as nothing, or as whichever branch happens to be last",
				state)
		}
	}

	// And the reverse: a TypeScript state the Go model does not produce is
	// dead code that will never be reached, which is its own kind of lie.
	inGo := make(map[string]bool, len(goStates))
	for _, state := range goStates {
		inGo[state] = true
	}
	for _, state := range tsStates {
		if !inGo[state] {
			t.Errorf("the TypeScript state %q is not produced by the Go model", state)
		}
	}
}

// TestEveryStateHasAHeadingAndAnExplanation stops a state being added to the
// union and nowhere else.
func TestEveryStateHasAHeadingAndAnExplanation(t *testing.T) {
	source := readFile(t, "web", "src", "api", "firstRun.ts")

	for _, state := range goFirstRunStates(t) {
		// The heading record and the explanation switch both key on the literal.
		if strings.Count(source, `"`+state+`"`) < 2 &&
			strings.Count(source, state+":") < 1 {
			t.Errorf("the state %q appears in firstRun.ts too few times to have "+
				"both a heading and an explanation", state)
		}
	}
}

// TestTheProjectionPrecedenceMatches is the ordering half.
//
// The vocabulary agreeing is not enough: the ORDER of the checks is the
// semantics. If TypeScript asked about eligibility before assessment, an
// unassessed estate would report "nothing eligible" -- the exact failure Stage
// 17.8's invariant exists to prevent, with a matching vocabulary.
//
// Both are read as the sequence of state names each implementation decides in.
func TestTheProjectionPrecedenceMatches(t *testing.T) {
	goSource := readFile(t, "internal", "domain", "first_run.go")
	tsSource := readFile(t, "web", "src", "api", "firstRun.ts")

	goOrder := decisionOrder(t, goSource,
		"func DescribeFirstRun(", goFirstRunStates(t), "FirstRun")
	tsOrder := decisionOrder(t, tsSource,
		"export function describeFirstRun(", goFirstRunStates(t), "")

	// The client may begin with one extra `unknown` guard, and only that.
	//
	// It handles a capability report the browser could not fetch -- a case the
	// Go function cannot have, because its caller always supplies a value. The
	// guard is strictly CONSERVATIVE: returning `unknown` earlier can never
	// report a readier state than the Go model would, only a less ready one.
	//
	// Everything after it must match exactly. Anything else reordering is the
	// failure this test exists for.
	if len(tsOrder) > 0 && tsOrder[0] == "unknown" {
		tsOrder = tsOrder[1:]
	}

	if len(goOrder) < 8 {
		t.Fatalf("read %d decisions from DescribeFirstRun; the function is not "+
			"where this test thinks it is", len(goOrder))
	}
	if strings.Join(goOrder, ",") != strings.Join(tsOrder, ",") {
		t.Errorf("the two projections decide in different orders\n"+
			"\tGo: %s\n\tTS: %s\n"+
			"\tthe order IS the semantics: asking about eligibility before "+
			"assessment reports an unassessed estate as settled",
			strings.Join(goOrder, " -> "), strings.Join(tsOrder, " -> "))
	}
}

// decisionOrder reads the states a function returns, in source order.
func decisionOrder(t *testing.T, source, marker string, states []string, prefix string) []string {
	t.Helper()

	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("could not find %q", marker)
	}
	body := source[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}

	known := make(map[string]bool, len(states))
	for _, state := range states {
		known[state] = true
	}

	var order []string
	for _, line := range strings.Split(body, "\n") {
		for _, state := range states {
			needle := `"` + state + `"`
			if prefix != "" {
				// The Go source returns the CONSTANT, not the literal.
				needle = prefix + strings.ToUpper(state[:1]) + state[1:]
			}
			if strings.Contains(line, needle) {
				order = append(order, state)
				break
			}
		}
	}
	return order
}
