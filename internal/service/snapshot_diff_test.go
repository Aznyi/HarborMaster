package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

func testDiffEngine(t *testing.T) *DiffEngine {
	t.Helper()
	return NewDiffEngine(config.Snapshots{
		MaxConcurrentDiffs: 4,
		DiffTimeout:        5 * time.Second,
		MaxDiffEntries:     1000,
		MaxGroupEntries:    5000,
	})
}

func diffInput(t *testing.T, detail domain.ContainerDetail) DiffInput {
	t.Helper()
	spec := buildTestSpec(t, detail)

	env := make([]domain.SnapshotEnvEntry, 0, len(spec.Environment))
	for i, entry := range spec.Environment {
		env = append(env, domain.SnapshotEnvEntry{
			Position: i, Key: entry.Name, Classification: entry.Sensitivity,
			Present: entry.Present, Value: entry.Value, Length: entry.Length,
			Digest: entry.Digest, DigestAlgorithm: entry.DigestAlgorithm,
			DigestKeyID: entry.DigestKeyID,
		})
	}
	return DiffInput{SnapshotID: 1, Spec: spec, Env: env}
}

func mustDiff(t *testing.T, from, to DiffInput, opts DiffOptions) domain.SnapshotDiff {
	t.Helper()
	diff, err := testDiffEngine(t).Diff(context.Background(), from, to, opts)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return diff
}

func findGroup(t *testing.T, diff domain.SnapshotDiff, name domain.DiffGroupName) domain.DiffGroup {
	t.Helper()
	for _, group := range diff.Groups {
		if group.Name == name {
			return group
		}
	}
	t.Fatalf("group %q not present", name)
	return domain.DiffGroup{}
}

func findEntry(t *testing.T, diff domain.SnapshotDiff, group domain.DiffGroupName, key string) domain.DiffEntry {
	t.Helper()
	for _, entry := range findGroup(t, diff, group).Entries {
		if entry.Key == key {
			return entry
		}
	}
	t.Fatalf("entry %q not found in group %q", key, group)
	return domain.DiffEntry{}
}

func TestIdenticalConfigurationsProduceNoChanges(t *testing.T) {
	from := diffInput(t, fixtureDetail())
	to := diffInput(t, fixtureDetail())

	diff := mustDiff(t, from, to, DiffOptions{})
	if !diff.Identical {
		t.Errorf("Identical = false; changed = %d", diff.ChangedCount)
	}
	if diff.ChangedCount != 0 {
		t.Errorf("ChangedCount = %d, want 0", diff.ChangedCount)
	}
}

// Reordering set-like fields is not a change.
func TestDiffIgnoresOrderingNoise(t *testing.T) {
	a := fixtureDetail()
	b := fixtureDetail()
	reverseSlice(b.Mounts)
	reverseSlice(b.Ports)
	reverseSlice(b.Networks)
	reverseSlice(b.Labels)
	reverseSlice(b.Security.CapDrop)

	diff := mustDiff(t, diffInput(t, a), diffInput(t, b), DiffOptions{})
	if diff.ChangedCount != 0 {
		t.Errorf("reordering produced %d changes, want 0", diff.ChangedCount)
	}
}

func TestDiffDetectsChangesPerGroup(t *testing.T) {
	cases := []struct {
		name   string
		group  domain.DiffGroupName
		key    string
		kind   domain.ChangeKind
		mutate func(*domain.ContainerDetail)
	}{
		{"label modified", domain.DiffGroupLabels, "app", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Labels[1].Value = "api" }},
		{"label added", domain.DiffGroupLabels, "new", domain.ChangeAdded,
			func(d *domain.ContainerDetail) {
				d.Labels = append(d.Labels, domain.Label{Key: "new", Value: "x"})
			}},
		{"port modified", domain.DiffGroupPorts, "443/tcp", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Ports[0].HostPort = 9443 }},
		{"mount modified", domain.DiffGroupMounts, "/data", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Mounts[0].ReadOnly = true }},
		{"network added", domain.DiffGroupNetworks, "extra", domain.ChangeAdded,
			func(d *domain.ContainerDetail) {
				d.Networks = append(d.Networks, domain.NetworkAttachment{NetworkName: "extra"})
			}},
		{"resource modified", domain.DiffGroupResources, "memoryBytes", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Resources.MemoryBytes = 1 }},
		{"security modified", domain.DiffGroupSecurity, "privileged", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Security.Privileged = true }},
		{"compose modified", domain.DiffGroupCompose, "service", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Compose.Service = "api" }},
		{"metadata modified", domain.DiffGroupMetadata, "image.reference", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Overview.Image.Raw = "nginx:1.28" }},
		{"env modified", domain.DiffGroupEnvironment, "PATH", domain.ChangeModified,
			func(d *domain.ContainerDetail) { d.Environment[0].Value = "/bin" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := fixtureDetail()
			tc.mutate(&after)

			diff := mustDiff(t, diffInput(t, fixtureDetail()), diffInput(t, after), DiffOptions{})
			entry := findEntry(t, diff, tc.group, tc.key)
			if entry.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", entry.Kind, tc.kind)
			}
			if diff.Identical {
				t.Error("Identical = true despite a change")
			}
		})
	}
}

// A sensitive entry reports THAT it changed, never what to.
func TestSensitiveEntriesReportChangeWithoutValues(t *testing.T) {
	before := fixtureDetail()
	after := fixtureDetail()
	setFixtureSecret(after.Environment, "DB_PASSWORD", "a-brand-new-secret-value")

	diff := mustDiff(t, diffInput(t, before), diffInput(t, after), DiffOptions{})

	entry := findEntry(t, diff, domain.DiffGroupEnvironment, "DB_PASSWORD")
	if entry.Kind != domain.ChangeModified {
		t.Errorf("Kind = %q, want modified", entry.Kind)
	}
	if !entry.Sensitive {
		t.Error("Sensitive not set")
	}
	if entry.Old != "" || entry.New != "" {
		t.Errorf("a sensitive entry carried values: old=%q new=%q", entry.Old, entry.New)
	}

	blob, err := json.Marshal(diff)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{specSecretValue, "a-brand-new-secret-value"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the diff leaked %q", needle)
		}
	}
}

func TestUnchangedSensitiveEntryIsNotReportedAsChanged(t *testing.T) {
	diff := mustDiff(t, diffInput(t, fixtureDetail()), diffInput(t, fixtureDetail()), DiffOptions{})
	if diff.ModifiedCount != 0 {
		t.Errorf("an unchanged secret was reported as modified: %d changes", diff.ModifiedCount)
	}
}

// Digests from different keys cannot be compared. Reporting "modified" would
// tell an operator every secret changed after a key rotation.
func TestDigestsFromDifferentKeysAreUnverifiable(t *testing.T) {
	from := diffInput(t, fixtureDetail())

	to := diffInput(t, fixtureDetail())
	for i := range to.Env {
		if to.Env[i].Key == "DB_PASSWORD" {
			to.Env[i].DigestKeyID = "different-key"
		}
	}

	diff := mustDiff(t, from, to, DiffOptions{})
	entry := findEntry(t, diff, domain.DiffGroupEnvironment, "DB_PASSWORD")
	if entry.Kind != domain.ChangeUnverifiable {
		t.Errorf("Kind = %q, want unverifiable across key IDs", entry.Kind)
	}
	if entry.Note == "" {
		t.Error("an unverifiable entry should explain why")
	}
}

func TestUnchangedEntriesAreOmittedByDefault(t *testing.T) {
	from := diffInput(t, fixtureDetail())
	to := diffInput(t, fixtureDetail())

	without := mustDiff(t, from, to, DiffOptions{})
	for _, group := range without.Groups {
		if len(group.Entries) != 0 {
			t.Errorf("group %q returned %d entries with no changes", group.Name, len(group.Entries))
		}
		if group.Unchanged == 0 {
			t.Errorf("group %q did not count its unchanged entries", group.Name)
		}
	}

	with := mustDiff(t, from, to, DiffOptions{IncludeUnchanged: true})
	total := 0
	for _, group := range with.Groups {
		total += len(group.Entries)
	}
	if total == 0 {
		t.Error("IncludeUnchanged returned nothing")
	}
}

// Silent truncation would read as "these configurations are identical", which
// is exactly the wrong conclusion to hand an operator preparing a restore.
func TestDiffTruncatesExplicitly(t *testing.T) {
	engine := NewDiffEngine(config.Snapshots{
		MaxConcurrentDiffs: 4,
		DiffTimeout:        5 * time.Second,
		MaxDiffEntries:     10,
		MaxGroupEntries:    5000,
	})

	before := fixtureDetail()
	after := fixtureDetail()
	for i := range 200 {
		after.Labels = append(after.Labels, domain.Label{
			Key: "generated-" + strconv.Itoa(i), Value: "value",
		})
	}

	diff, err := engine.Diff(context.Background(), diffInput(t, before), diffInput(t, after), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !diff.Truncated {
		t.Fatal("Truncated not set; silent truncation reads as 'no more changes'")
	}
	if diff.TruncationReason == "" {
		t.Error("TruncationReason is empty")
	}
	if diff.Identical {
		t.Error("a truncated diff must never report Identical")
	}

	returned := 0
	for _, group := range diff.Groups {
		returned += len(group.Entries)
	}
	if returned > 10 {
		t.Errorf("returned %d entries, want at most 10", returned)
	}

	labels := findGroup(t, diff, domain.DiffGroupLabels)
	if labels.Total <= labels.Returned {
		t.Error("the group should report a total above what it returned")
	}
}

func TestGroupComparisonIsBounded(t *testing.T) {
	engine := NewDiffEngine(config.Snapshots{
		MaxConcurrentDiffs: 4,
		DiffTimeout:        5 * time.Second,
		MaxDiffEntries:     100000,
		MaxGroupEntries:    50,
	})

	after := fixtureDetail()
	for i := range 500 {
		after.Labels = append(after.Labels, domain.Label{Key: "k" + strconv.Itoa(i), Value: "v"})
	}

	diff, err := engine.Diff(context.Background(), diffInput(t, fixtureDetail()), diffInput(t, after), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !findGroup(t, diff, domain.DiffGroupLabels).Truncated {
		t.Error("the labels group exceeded MaxGroupEntries without reporting truncation")
	}
}

// Refused, not queued: a queue turns a load spike into unbounded memory.
func TestConcurrencyCeilingRefusesRatherThanQueues(t *testing.T) {
	engine := NewDiffEngine(config.Snapshots{
		MaxConcurrentDiffs: 1,
		DiffTimeout:        5 * time.Second,
		MaxDiffEntries:     1000,
		MaxGroupEntries:    5000,
	})

	// Occupy the only slot.
	engine.slots <- struct{}{}
	defer func() { <-engine.slots }()

	_, err := engine.Diff(context.Background(), DiffInput{}, DiffInput{}, DiffOptions{})
	if !errors.Is(err, ErrDiffBusy) {
		t.Errorf("err = %v, want ErrDiffBusy", err)
	}
}

func TestSlotIsReleasedAfterEachDiff(t *testing.T) {
	engine := NewDiffEngine(config.Snapshots{
		MaxConcurrentDiffs: 1,
		DiffTimeout:        5 * time.Second,
		MaxDiffEntries:     1000,
		MaxGroupEntries:    5000,
	})

	for i := range 5 {
		if _, err := engine.Diff(context.Background(), DiffInput{}, DiffInput{}, DiffOptions{}); err != nil {
			t.Fatalf("diff %d: %v", i, err)
		}
	}
}

func TestCancelledContextTruncatesRatherThanHangs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	diff, err := testDiffEngine(t).Diff(ctx, diffInput(t, fixtureDetail()), diffInput(t, fixtureDetail()), DiffOptions{})
	if err != nil {
		t.Fatalf("a cancelled diff should return a truncated result, not an error: %v", err)
	}
	if !diff.Truncated {
		t.Error("a cancelled diff should report truncation")
	}
}

func TestGroupSelectionNarrowsOutput(t *testing.T) {
	diff := mustDiff(t, diffInput(t, fixtureDetail()), diffInput(t, fixtureDetail()), DiffOptions{
		Groups: []domain.DiffGroupName{domain.DiffGroupLabels},
	})
	if len(diff.Groups) != 1 || diff.Groups[0].Name != domain.DiffGroupLabels {
		t.Errorf("group selection ignored: %+v", diff.Groups)
	}
}

func TestValidDiffGroupIsAnAllowlist(t *testing.T) {
	for _, name := range domain.DiffGroupNames {
		if !domain.ValidDiffGroup(string(name)) {
			t.Errorf("ValidDiffGroup(%q) = false", name)
		}
	}
	for _, name := range []string{"", "spec_json", "$.environment", "labels; DROP TABLE", "*"} {
		if domain.ValidDiffGroup(name) {
			t.Errorf("ValidDiffGroup(%q) = true; the vocabulary must stay closed", name)
		}
	}
}

func TestLongValuesAreTruncatedExplicitly(t *testing.T) {
	after := fixtureDetail()
	after.Environment[0].Value = strings.Repeat("x", 100_000)
	after.Environment[0].RawValue = after.Environment[0].Value

	diff := mustDiff(t, diffInput(t, fixtureDetail()), diffInput(t, after), DiffOptions{})
	entry := findEntry(t, diff, domain.DiffGroupEnvironment, "PATH")

	if len(entry.New) > maxDiffValueBytes+64 {
		t.Errorf("a value of %d bytes was returned unbounded", len(entry.New))
	}
	if !strings.Contains(entry.New, "truncated") {
		t.Error("value truncation should be explicit")
	}
}

// Adding and removing a variable must be reported as such, not as a
// modification of an unrelated key.
func TestAddedAndRemovedEnvironmentEntries(t *testing.T) {
	before := fixtureDetail()
	after := fixtureDetail()
	after.Environment = append(after.Environment, domain.EnvVar{
		Name: "NEW_VAR", Value: "new", Sensitivity: domain.SensitivityNormal, RawValue: "new",
	})
	// Drop NGINX_PORT.
	after.Environment = append(after.Environment[:1], after.Environment[2:]...)

	diff := mustDiff(t, diffInput(t, before), diffInput(t, after), DiffOptions{})

	if got := findEntry(t, diff, domain.DiffGroupEnvironment, "NEW_VAR").Kind; got != domain.ChangeAdded {
		t.Errorf("NEW_VAR = %q, want added", got)
	}
	if got := findEntry(t, diff, domain.DiffGroupEnvironment, "NGINX_PORT").Kind; got != domain.ChangeRemoved {
		t.Errorf("NGINX_PORT = %q, want removed", got)
	}
}

// A removed sensitive variable must not leak its old value either.
func TestRemovedSensitiveEntryWithholdsItsValue(t *testing.T) {
	before := fixtureDetail()
	after := fixtureDetail()
	after.Environment = after.Environment[:2] // drop DB_PASSWORD

	diff := mustDiff(t, diffInput(t, before), diffInput(t, after), DiffOptions{})
	entry := findEntry(t, diff, domain.DiffGroupEnvironment, "DB_PASSWORD")

	if entry.Kind != domain.ChangeRemoved {
		t.Errorf("Kind = %q, want removed", entry.Kind)
	}
	if entry.Old != "" {
		t.Errorf("a removed sensitive entry leaked its value: %q", entry.Old)
	}
}

// A sensitive LOG OPTION must be withheld from the metadata group too.
func TestSensitiveLogOptionsAreNotDiffed(t *testing.T) {
	before := fixtureDetail()
	after := fixtureDetail()
	setFixtureSecret(after.Logging.Options, "splunk-token", "tok_something-else")

	diff := mustDiff(t, diffInput(t, before), diffInput(t, after), DiffOptions{})
	blob, err := json.Marshal(diff)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "tok_") {
		t.Errorf("a sensitive log option value reached the diff: %s", blob)
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	before := fixtureDetail()
	after := fixtureDetail()
	after.Labels[1].Value = "api"

	first, err := json.Marshal(mustDiff(t, diffInput(t, before), diffInput(t, after), DiffOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		got, err := json.Marshal(mustDiff(t, diffInput(t, before), diffInput(t, after), DiffOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("diff output is not deterministic on iteration %d", i)
		}
	}
}
