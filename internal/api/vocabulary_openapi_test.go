package api

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The closed vocabularies, pinned against the published schema.
//
// # Why this test exists
//
// Every one of these vocabularies is rendered by a UI that maps each value to a
// sentence. A value the map does not know renders as the RAW ENUM -- and that is
// not hypothetical: it had already happened. Phase 16 added seven automation
// reasons in Go, and both the OpenAPI enum and the frontend's map were left
// behind, so an operator whose container was held by a dependency read
// `dependencyWaiting` in the Automation page.
//
// A test cannot make a frontend map itself correct from Go. What it CAN do is
// make the schema the single published statement of each vocabulary, and fail
// the build the moment Go and the schema disagree. The frontend then pins itself
// against the same schema, in its own suite, which is why the yaml is parsed as
// text on both sides rather than duplicated in two places.
//
// # Why the yaml is read as text rather than parsed
//
// The enums are flat lists of scalars under a known key. A YAML dependency to
// read them would be a supply-chain addition for one test, and the extraction
// below is checked by its own non-vacuity guard: an empty or missing enum fails
// rather than passing quietly.

// openAPIPath locates the published schema from this package.
func openAPIPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join("..", "..", "api", "openapi.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the published schema is not readable at %s: %v", path, err)
	}
	return path
}

// enumValues extracts one schema's `enum:` list.
//
// Handles both the block form the large vocabularies use and the inline
// `enum: [a, b]` form the short ones do, because the file contains both and a
// reader that understood only one would silently return nothing.
func enumValues(t *testing.T, schema string) []string {
	t.Helper()

	file, err := os.Open(openAPIPath(t))
	if err != nil {
		t.Fatalf("open schema: %v", err)
	}
	defer func() { _ = file.Close() }()

	var (
		values    []string
		inSchema  bool
		inEnum    bool
		schemaKey = "    " + schema + ":"
	)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, schemaKey) {
			inSchema = true
			continue
		}
		if !inSchema {
			continue
		}
		// A new top-level schema ends this one.
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") {
			break
		}

		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "enum:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "enum:"))
			if strings.HasPrefix(rest, "[") {
				// Inline form.
				rest = strings.Trim(rest, "[]")
				for _, value := range strings.Split(rest, ",") {
					if value = strings.TrimSpace(value); value != "" {
						values = append(values, value)
					}
				}
				break
			}
			inEnum = true
			continue
		}
		if inEnum {
			if !strings.HasPrefix(trimmed, "- ") {
				break
			}
			values = append(values, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read schema: %v", err)
	}

	// The non-vacuity guard. Every check below compares two sets, and two empty
	// sets are equal -- so a typo in the schema name would turn this file into
	// a test of nothing.
	if len(values) == 0 {
		t.Fatalf("no enum values were extracted for %q; the check would pass "+
			"vacuously", schema)
	}
	return values
}

// samePublishedVocabulary compares a Go vocabulary against the schema's enum.
func samePublishedVocabulary(t *testing.T, schema string, goValues []string) {
	t.Helper()

	published := enumValues(t, schema)

	inSchema := make(map[string]bool, len(published))
	for _, value := range published {
		inSchema[value] = true
	}
	inGo := make(map[string]bool, len(goValues))
	for _, value := range goValues {
		inGo[value] = true
	}

	for _, value := range goValues {
		if !inSchema[value] {
			t.Errorf("%s: Go defines %q and the schema does not\n"+
				"\tthe API already returns this value. A client mapping each value "+
				"to a sentence has no entry for it and will render the raw enum.",
				schema, value)
		}
	}
	for _, value := range published {
		if !inGo[value] {
			t.Errorf("%s: the schema publishes %q and Go does not define it\n"+
				"\ta value no server can produce is a promise to clients that "+
				"cannot be kept", schema, value)
		}
	}
}

func TestAutomationReasonsMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.AutomationReasons))
	for _, reason := range domain.AutomationReasons {
		values = append(values, string(reason))
	}
	samePublishedVocabulary(t, "AutomationReason", values)
}

func TestDependencyStatesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.DependencyStates))
	for _, state := range domain.DependencyStates {
		values = append(values, string(state))
	}
	samePublishedVocabulary(t, "DependencyState", values)
}

func TestAttentionStatesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.AttentionOrder))
	for _, state := range domain.AttentionOrder {
		values = append(values, string(state))
	}
	samePublishedVocabulary(t, "AttentionState", values)
}

func TestDependencyMemberStatesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.DependencyMemberStates))
	for _, state := range domain.DependencyMemberStates {
		values = append(values, string(state))
	}
	samePublishedVocabulary(t, "DependencyMemberState", values)
}

func TestNotificationEventsMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.NotificationEvents))
	for _, event := range domain.NotificationEvents {
		values = append(values, string(event))
	}
	samePublishedVocabulary(t, "NotificationEvent", values)
}

func TestDependencyOperationStatesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.DependencyOperationStates))
	for _, state := range domain.DependencyOperationStates {
		values = append(values, string(state))
	}
	samePublishedVocabulary(t, "DependencyOperationState", values)
}

func TestDependencyOperationFailuresMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.DependencyOperationFailures))
	for _, failure := range domain.DependencyOperationFailures {
		values = append(values, string(failure))
	}
	samePublishedVocabulary(t, "DependencyOperationFailure", values)
}

func TestDependencySourcesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.DependencySources))
	for _, source := range domain.DependencySources {
		values = append(values, string(source))
	}
	samePublishedVocabulary(t, "DependencySource", values)
}

// Every automation reason explains itself.
//
// The vocabulary is only useful if each value carries a sentence; a reason with
// an empty explanation is one the UI would have to invent words for.
func TestEveryAutomationReasonExplainsItself(t *testing.T) {
	t.Parallel()

	for _, reason := range domain.AutomationReasons {
		if strings.TrimSpace(reason.Explain()) == "" {
			t.Errorf("the automation reason %q has no explanation", reason)
		}
	}
}
