package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Snapshot assurance tests.
//
// # These run against the REAL repository, deliberately
//
// Deduplication is a property of a transactional ON CONFLICT clause, not of the
// service that calls it. A fake writer that returned Deduplicated on demand
// would let every test below pass while the real thing accumulated a row per
// pass -- which is the exact shape of "tests pass only because a fake is more
// permissive than production". So the writer here is store.SnapshotRepository
// against a migrated database, and the assertions count ROWS.

// assuranceHarness is a real snapshot service over a real database.
type assuranceHarness struct {
	assurance  *SnapshotAssurance
	snapshots  *SnapshotService
	containers *fakeContainers
	db         *store.DB
}

func newAssuranceHarness(t *testing.T, detail domain.ContainerDetail) *assuranceHarness {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	containers := &fakeContainers{detail: &detail}
	snapshots := NewSnapshotService(SnapshotOptions{
		Containers: containers,
		Snapshots:  db.Snapshots,
		Hasher:     newTestHasher(t),
		Versions:   HostVersions{HarborMaster: "0.9.0", DockerAPI: "1.51"},
		Config:     config.Snapshots{Enabled: true},
	})

	return &assuranceHarness{
		assurance:  NewSnapshotAssurance(SnapshotAssuranceOptions{Capturer: snapshots}),
		snapshots:  snapshots,
		containers: containers,
		db:         db,
	}
}

// rows counts snapshot rows for one container, which is the assertion that
// cannot be satisfied by a permissive fake.
func (h *assuranceHarness) rows(t *testing.T, containerID string) int {
	t.Helper()
	var count int
	if err := h.db.SQL().QueryRow(
		`SELECT COUNT(*) FROM snapshots WHERE container_id = ?`, containerID).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return count
}

// ------------------------------------------------------------ dedup --

// TestUnchangedConfigurationProducesExactlyOneSnapshot is the bound on database
// growth that makes assurance safe to run on every planner pass.
func TestUnchangedConfigurationProducesExactlyOneSnapshot(t *testing.T) {
	harness := newAssuranceHarness(t, fixtureDetail())
	containerID := fixtureDetail().Overview.ID
	ctx := context.Background()

	first := harness.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)
	if first.Outcome != AssuranceCaptured {
		t.Fatalf("first outcome = %q, want %q", first.Outcome, AssuranceCaptured)
	}
	if first.SnapshotID == 0 {
		t.Fatal("the first capture reported no snapshot id")
	}

	for attempt := 0; attempt < 9; attempt++ {
		again := harness.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)
		if again.Outcome != AssuranceCurrent {
			t.Errorf("attempt %d outcome = %q, want %q", attempt, again.Outcome, AssuranceCurrent)
		}
		if again.SnapshotID != first.SnapshotID {
			t.Errorf("attempt %d returned snapshot %d, want the existing %d",
				attempt, again.SnapshotID, first.SnapshotID)
		}
		if !again.Usable() {
			t.Errorf("attempt %d is not usable; a deduplicated baseline is a baseline", attempt)
		}
	}

	// Ten calls. One row.
	if got := harness.rows(t, containerID); got != 1 {
		t.Errorf("%d snapshot rows after ten assurance calls, want 1", got)
	}
}

// ------------------------------------------------- changed configuration --

// TestAnOrdinaryConfigurationChangeProducesNewEvidence.
func TestAnOrdinaryConfigurationChangeProducesNewEvidence(t *testing.T) {
	detail := fixtureDetail()
	harness := newAssuranceHarness(t, detail)
	containerID := detail.Overview.ID
	ctx := context.Background()

	first := harness.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)
	if first.Outcome != AssuranceCaptured {
		t.Fatalf("first outcome = %q, want captured", first.Outcome)
	}

	// A plain, non-sensitive field moves.
	changed := fixtureDetail()
	changed.Environment[1].Value = "9090"
	changed.Environment[1].RawValue = "9090"
	harness.containers.detail = &changed

	second := harness.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)
	if second.Outcome != AssuranceCaptured {
		t.Errorf("outcome after a configuration change = %q, want %q",
			second.Outcome, AssuranceCaptured)
	}
	if second.SnapshotID == first.SnapshotID {
		t.Error("a changed configuration reused the previous snapshot")
	}
	if got := harness.rows(t, containerID); got != 2 {
		t.Errorf("%d snapshot rows, want 2", got)
	}
}

// TestASensitiveValueChangeAloneProducesNewEvidence.
//
// The Phase 16.1 property, re-proved through the assurance entry point. The
// snapshot stores a KEYED DIGEST of a secret rather than the secret, so the
// question "would a changed password be noticed" is a question about whether
// the digest reaches the checksum. It does, and this is what says so.
func TestASensitiveValueChangeAloneProducesNewEvidence(t *testing.T) {
	detail := fixtureDetail()
	harness := newAssuranceHarness(t, detail)
	containerID := detail.Overview.ID
	ctx := context.Background()

	first := harness.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)
	if first.Outcome != AssuranceCaptured {
		t.Fatalf("first outcome = %q, want captured", first.Outcome)
	}

	// ONLY the secret moves. Everything else about the container is identical,
	// so a checksum that ignored sensitive values would deduplicate here.
	//
	// Re-CLASSIFIED rather than having RawValue overwritten, because that is how
	// a rotated credential actually reaches HarborMaster: the digest is computed
	// by the masker when the value is read from the daemon and is then carried on
	// the variable. BuildSpec deliberately does not recompute it -- the inventory
	// cannot hold a raw value, so hashing RawValue there would hash the empty
	// string and give every secret on the host one identical digest.
	changed := fixtureDetail()
	setFixtureSecret(changed.Environment, "DB_PASSWORD", "a-completely-different-password")
	harness.containers.detail = &changed

	second := harness.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)
	if second.Outcome != AssuranceCaptured {
		t.Fatalf("a changed secret did not produce new evidence: outcome = %q", second.Outcome)
	}
	if second.SnapshotID == first.SnapshotID {
		t.Fatal("a changed secret deduplicated onto the previous snapshot; " +
			"a rotated credential would be invisible to the baseline")
	}
	if got := harness.rows(t, containerID); got != 2 {
		t.Errorf("%d snapshot rows, want 2", got)
	}
}

// TestNoPlaintextSecretReachesAnAssuredSnapshot.
//
// Reads every persisted column and every environment row for the literal secret
// value. The assurance path adds no way around BuildSpec, and this is the
// assertion that says so rather than the comment.
func TestNoPlaintextSecretReachesAnAssuredSnapshot(t *testing.T) {
	const secret = "s3cr3t-assurance-value"

	detail := fixtureDetail()
	setFixtureSecret(detail.Environment, "DB_PASSWORD", secret)
	harness := newAssuranceHarness(t, detail)
	ctx := context.Background()

	result := harness.assurance.EnsureCurrent(ctx, detail.Overview.ID, domain.SnapshotTriggerPreUpdate)
	if result.Outcome != AssuranceCaptured {
		t.Fatalf("outcome = %q, want captured", result.Outcome)
	}

	// The spec document itself.
	var spec []byte
	if err := harness.db.SQL().QueryRow(
		`SELECT spec_json FROM snapshots WHERE id = ?`, result.SnapshotID).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if containsSubstring(string(spec), secret) {
		t.Error("the persisted snapshot document contains a plaintext secret")
	}

	// And every environment row, including the value column the schema is
	// supposed to keep blank for a sensitive entry.
	rows, err := harness.db.SQL().Query(
		`SELECT key, value, digest FROM snapshot_environment WHERE snapshot_id = ?`, result.SnapshotID)
	if err != nil {
		t.Fatalf("read environment: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var sawDigest bool
	for rows.Next() {
		var key, value, digest string
		if err := rows.Scan(&key, &value, &digest); err != nil {
			t.Fatalf("scan environment: %v", err)
		}
		if value == secret {
			t.Errorf("environment row %q persisted the plaintext secret", key)
		}
		if digest == secret {
			t.Errorf("environment row %q stored the secret where its digest belongs", key)
		}
		if digest != "" {
			sawDigest = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("environment rows: %v", err)
	}
	// Non-vacuity: if nothing was digested, the loop above proved nothing.
	if !sawDigest {
		t.Fatal("no environment row carried a digest; this test would pass vacuously")
	}
}

// ------------------------------------------------------- failing closed --

// failingCapturer refuses every capture.
type failingCapturer struct {
	err     error
	enabled bool
	calls   int
	mu      sync.Mutex
}

func (f *failingCapturer) Capture(context.Context, CaptureRequest) (domain.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return domain.Snapshot{}, f.err
}

func (f *failingCapturer) Enabled() bool { return f.enabled }

// TestEveryCaptureFailureIsUnavailable.
//
// Four different failures, one outcome. A caller that must fail closed should
// not have to enumerate the ways a capture can go wrong.
func TestEveryCaptureFailureIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"snapshots disabled", ErrSnapshotsDisabled},
		{"a capture already running", ErrCaptureInProgress},
		{"the container could not be read", store.ErrNotFound},
		{"an unexpected error", errors.New("the database is on fire")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			capturer := &failingCapturer{err: testCase.err, enabled: true}
			assurance := NewSnapshotAssurance(SnapshotAssuranceOptions{Capturer: capturer})

			result := assurance.EnsureCurrent(context.Background(), "c1", domain.SnapshotTriggerPreUpdate)
			if result.Outcome != AssuranceUnavailable {
				t.Errorf("outcome = %q, want %q", result.Outcome, AssuranceUnavailable)
			}
			if result.Usable() {
				t.Error("an unavailable result reported itself usable")
			}
			if result.SnapshotID != 0 {
				t.Errorf("snapshotId = %d, want 0", result.SnapshotID)
			}
			if result.Detail == "" {
				t.Error("a refusal with no sentence gives an operator nothing to act on")
			}
		})
	}
}

// TestAssuranceIsUnavailableWhenSnapshotsAreOff.
//
// And crucially: it does not even ATTEMPT a capture. An operator who switched
// snapshots off should not have HarborMaster calling into the capture path on
// every pass to be told so.
func TestAssuranceIsUnavailableWhenSnapshotsAreOff(t *testing.T) {
	capturer := &failingCapturer{enabled: false}
	assurance := NewSnapshotAssurance(SnapshotAssuranceOptions{Capturer: capturer})

	if assurance.Available() {
		t.Error("assurance reports itself available with snapshots switched off")
	}
	result := assurance.EnsureCurrent(context.Background(), "c1", domain.SnapshotTriggerScheduled)
	if result.Outcome != AssuranceUnavailable {
		t.Errorf("outcome = %q, want %q", result.Outcome, AssuranceUnavailable)
	}
	if capturer.calls != 0 {
		t.Errorf("%d capture attempts with snapshots off, want 0", capturer.calls)
	}
}

// TestANilAssuranceIsUnavailableRatherThanAPanic.
//
// The execution preflight guards on this, but a nil receiver that panicked
// would make that guard the only thing between a misconfiguration and a crash
// on the mutation path.
func TestANilAssuranceIsUnavailableRatherThanAPanic(t *testing.T) {
	var assurance *SnapshotAssurance
	if assurance.Available() {
		t.Error("a nil assurance reported itself available")
	}
}

// ------------------------------------------------------------ outcomes --

// TestOnlyPositiveOutcomesAreUsable pins the allowlist.
//
// Written as an exhaustive table including a value this build does not define,
// so an outcome added later is unusable until somebody says otherwise.
func TestOnlyPositiveOutcomesAreUsable(t *testing.T) {
	usable := map[AssuranceOutcome]bool{
		AssuranceCurrent:     true,
		AssuranceCaptured:    true,
		AssuranceNotReady:    false,
		AssuranceUnavailable: false,
		AssuranceOutcome(""): false,
		AssuranceOutcome("somethingAFutureBuildAdded"): false,
	}
	for outcome, want := range usable {
		if got := outcome.Usable(); got != want {
			t.Errorf("%q.Usable() = %v, want %v", outcome, got, want)
		}
	}
}

// TestEveryOutcomeExplainsItself.
func TestEveryOutcomeExplainsItself(t *testing.T) {
	for _, outcome := range []AssuranceOutcome{
		AssuranceCurrent, AssuranceCaptured, AssuranceNotReady, AssuranceUnavailable,
	} {
		if explanation := outcome.Explain(); explanation == "" || explanation == string(outcome) {
			t.Errorf("%q has no sentence of its own", outcome)
		}
	}
}

// ----------------------------------------------------------- concurrency --

// TestConcurrentAssuranceProducesOneRow.
//
// Run under -race. The existing capture path refuses a concurrent capture of
// the same container rather than queueing it, so the expected shape is one
// success and the rest reporting unavailable -- and, whatever the split, one
// row. Never two, and never a result that reports a baseline that is not there.
func TestConcurrentAssuranceProducesOneRow(t *testing.T) {
	harness := newAssuranceHarness(t, fixtureDetail())
	containerID := fixtureDetail().Overview.ID
	ctx := context.Background()

	const callers = 8
	results := make([]AssuranceResult, callers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(callers)

	for i := 0; i < callers; i++ {
		go func(index int) {
			defer done.Done()
			start.Wait()
			results[index] = harness.assurance.EnsureCurrent(
				ctx, containerID, domain.SnapshotTriggerScheduled)
		}(i)
	}
	start.Done()
	done.Wait()

	if got := harness.rows(t, containerID); got != 1 {
		t.Errorf("%d snapshot rows after %d concurrent callers, want 1", got, callers)
	}

	var usable int
	for index, result := range results {
		switch result.Outcome {
		case AssuranceCaptured, AssuranceCurrent:
			usable++
			if result.SnapshotID == 0 {
				t.Errorf("caller %d reported a usable baseline with no id", index)
			}
		case AssuranceUnavailable:
			// The documented concurrency contract: refused rather than queued.
			if result.SnapshotID != 0 {
				t.Errorf("caller %d reported unavailable with a snapshot id", index)
			}
		default:
			t.Errorf("caller %d got outcome %q", index, result.Outcome)
		}
	}
	// Non-vacuity: if every caller were refused, the row count above would be
	// zero and this test would be asserting nothing useful.
	if usable == 0 {
		t.Error("no caller established a baseline; the contention guard is refusing everything")
	}
}

// A second assurance over a SECOND container must not be blocked by the first:
// the capture guard is per container, and a global one would serialise the
// whole preparation pass.
func TestAssuranceForDifferentContainersDoesNotSerialise(t *testing.T) {
	first := fixtureDetail()
	second := fixtureDetail()
	second.Overview.ID = "d0cked0000000000"
	second.Overview.Name = "api"

	harness := newAssuranceHarness(t, first)
	ctx := context.Background()

	if got := harness.assurance.EnsureCurrent(ctx, first.Overview.ID,
		domain.SnapshotTriggerScheduled); got.Outcome != AssuranceCaptured {
		t.Fatalf("first outcome = %q", got.Outcome)
	}
	harness.containers.detail = &second
	if got := harness.assurance.EnsureCurrent(ctx, second.Overview.ID,
		domain.SnapshotTriggerScheduled); got.Outcome != AssuranceCaptured {
		t.Fatalf("second outcome = %q", got.Outcome)
	}

	if got := harness.rows(t, first.Overview.ID); got != 1 {
		t.Errorf("%d rows for the first container, want 1", got)
	}
	if got := harness.rows(t, second.Overview.ID); got != 1 {
		t.Errorf("%d rows for the second container, want 1", got)
	}
}
