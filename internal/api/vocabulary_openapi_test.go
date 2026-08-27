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

// TestExecutionRefusalsMatchThePublishedSchema closes the gap that let three
// refusals ship unpublished.
//
// # This drift was already shipping, for the second time
//
// Migration 0028 found `selfUpdate`, `namespaceProviderMissing`, and
// `dependentsNotRebindable` in the Go vocabulary and REJECTED by the database
// CHECK, and wrote a store-level test so it could not recur there. The same
// three values were also absent from this schema and from the TypeScript union,
// and nothing checked that -- so a client mapping each refusal to a sentence had
// no entry for the three most safety-critical ones and would have rendered the
// raw identifier to an operator.
//
// The store now has TestEveryExecutionRefusalIsAcceptedByTheSchema. This is the
// same guard for the published contract, and adding it is what makes Phase
// 17.2's new `snapshotChanged` value the last one that can be added without
// updating both.
func TestExecutionRefusalsMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.ExecutionRefusals))
	for _, refusal := range domain.ExecutionRefusals {
		values = append(values, string(refusal))
	}
	sameRefusalVocabulary(t, "ExecutionRefusal", values)
}

// sameRefusalVocabulary compares a refusal vocabulary against its schema enum,
// ignoring the "no refusal" sentinel.
//
// The sentinel is a ZERO VALUE rather than a refusal: it is absent from the Go
// lists by design, and the two published enums disagree about whether to carry
// it -- ExecutionRefusal declares `- ""` and AcquisitionRefusal does not. That
// disagreement is cosmetic and not what these tests are for, so it is filtered
// on both sides rather than being allowed to mask the real comparison.
func sameRefusalVocabulary(t *testing.T, schema string, goValues []string) {
	t.Helper()

	published := enumValues(t, schema)
	real := make([]string, 0, len(published))
	for _, value := range published {
		// YAML's `- ""` reaches the extractor with its quotes intact.
		if value == "" || value == `""` {
			continue
		}
		real = append(real, value)
	}
	if len(real) == 0 {
		t.Fatalf("no refusal values were extracted for %q; the check would pass "+
			"vacuously", schema)
	}

	inSchema := make(map[string]bool, len(real))
	for _, value := range real {
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
	for _, value := range real {
		if !inGo[value] {
			t.Errorf("%s: the schema publishes %q and Go does not define it\n"+
				"\ta value no server can produce is a promise to clients that "+
				"cannot be kept", schema, value)
		}
	}
}

// TestAcquisitionRefusalsMatchThePublishedSchema is the same guard for the
// download path, where `selfUpdate` was unpublished for the same reason.
func TestAcquisitionRefusalsMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.AcquisitionRefusals))
	for _, refusal := range domain.AcquisitionRefusals {
		values = append(values, string(refusal))
	}
	sameRefusalVocabulary(t, "AcquisitionRefusal", values)
}

// TestEveryExecutionRefusalExplainsItself.
//
// Explain has a default arm that returns the raw identifier, so a refusal added
// without a sentence degrades silently into an operator reading `snapshotChanged`
// in a browser. This is the same check TestEveryAutomationReasonExplainsItself
// makes for the automation vocabulary.
func TestEveryExecutionRefusalExplainsItself(t *testing.T) {
	t.Parallel()

	if len(domain.ExecutionRefusals) < 25 {
		t.Fatalf("found %d execution refusals; the vocabulary is not where this "+
			"test thinks it is", len(domain.ExecutionRefusals))
	}
	for _, refusal := range domain.ExecutionRefusals {
		explanation := refusal.Explain()
		if explanation == "" || explanation == string(refusal) {
			t.Errorf("%q has no sentence of its own; an operator would read the "+
				"raw identifier", refusal)
		}
	}
}

// TestUpdateTypesMatchThePublishedSchema closes the gap that let `rebind` ship
// unpublished.
//
// # The third occurrence of this defect's shape
//
// Migration 0027 taught the change_plans CHECK about `rebind` when Phase 16
// introduced it. Migration 0028 did the same for three execution refusals.
// Phase 17.2 found those three refusals also missing from this schema and from
// the TypeScript unions, and added guards.
//
// `rebind` was the one nobody had checked. It is in domain.UpdateTypes, it is
// accepted by the database, it is permitted by every update strategy, and the
// planner produces it -- but the published enum did not list it and the
// TypeScript union did not admit it. A client mapping each update type to a
// label had no entry, so a reattachment plan rendered as a bare identifier.
//
// One guard per vocabulary is the only thing that stops this recurring, so this
// is that guard for update types.
func TestUpdateTypesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.UpdateTypes))
	for _, update := range domain.UpdateTypes {
		values = append(values, string(update))
	}
	samePublishedVocabulary(t, "UpdateType", values)
}

// TestUpdateStrategiesMatchThePublishedSchema and the two below cover the rest
// of the policy vocabulary, for the same reason.
func TestUpdateStrategiesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.UpdateStrategies))
	for _, strategy := range domain.UpdateStrategies {
		values = append(values, string(strategy))
	}
	samePublishedVocabulary(t, "UpdateStrategy", values)
}

func TestAutomationModesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.AutomationModes))
	for _, mode := range domain.AutomationModes {
		values = append(values, string(mode))
	}
	samePublishedVocabulary(t, "AutomationMode", values)
}

func TestUpdateScopesMatchThePublishedSchema(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, len(domain.UpdateScopes))
	for _, scope := range domain.UpdateScopes {
		values = append(values, string(scope))
	}
	samePublishedVocabulary(t, "UpdateScope", values)
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
