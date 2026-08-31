package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Drift engine tests.
//
// These drive the REAL diff engine through the real service, against real
// repositories on a real SQLite file. The point of the phase is that drift
// reuses the Phase 3 comparison rather than reimplementing it, so a test with
// a fake comparer would verify the wrong thing.

// driftHarness bundles a drift service with the database behind it.
type driftHarness struct {
	service *service.DriftService
	db      *store.DB
	// current is the spec the builder returns, so a test can move the
	// container's configuration without a Docker daemon.
	current   domain.SnapshotSpec
	mu        sync.Mutex
	container domain.ContainerDetail
}

// baseSpec is a realistic starting configuration.
func baseSpec() domain.SnapshotSpec {
	return domain.SnapshotSpec{
		SpecVersion: domain.SnapshotSpecVersion,
		Identity: domain.SpecIdentity{
			ContainerID: "container-a", ContainerName: "web",
		},
		Image: domain.SpecImage{
			Reference: "nginx:1.27", Digest: "sha256:aaa", ImageID: "sha256:aaa",
		},
		Process: domain.Process{Command: []string{"nginx", "-g", "daemon off;"}},
		Environment: []domain.SpecEnvVar{
			{Name: "LOG_LEVEL", Value: "info", Sensitivity: domain.SensitivityNormal, Present: true},
		},
		Labels:        []domain.Label{{Key: "com.example.owner", Value: "platform"}},
		Security:      domain.Security{Privileged: false, ReadonlyRootfs: true},
		RestartPolicy: domain.RestartPolicy{Name: "always"},
	}
}

func newDriftHarness(t *testing.T, tweak ...func(*config.Drift)) *driftHarness {
	t.Helper()

	db := openDB(t)
	cfg := config.Drift{
		Enabled:                true,
		EvaluateOnEvents:       true,
		EvaluationDebounce:     time.Millisecond,
		MaxPendingEvaluations:  64,
		EvaluationTimeout:      10 * time.Second,
		MaxRecordsPerContainer: 500,
		PruneInterval:          time.Hour,
		MaxNoteBytes:           500,
	}
	for _, apply := range tweak {
		apply(&cfg)
	}

	harness := &driftHarness{db: db, current: baseSpec()}
	harness.container = domain.ContainerDetail{
		Overview: domain.ContainerSummary{ID: "container-a", Name: "web", Present: true},
	}

	harness.service = service.NewDriftService(service.DriftOptions{
		Snapshots:  db.Snapshots,
		Containers: driftContainers{harness: harness, repo: db.Containers},
		Records:    db.Drift,
		Pruner:     db.Drift,
		Inventory:  db.Inventory,
		SpecBuilder: func(domain.ContainerDetail) domain.SnapshotSpec {
			harness.mu.Lock()
			defer harness.mu.Unlock()
			return harness.current
		},
		Config: cfg,
		Logger: discardLogger(),
	})
	return harness
}

// setCurrent moves the container's live configuration.
func (h *driftHarness) setCurrent(spec domain.SnapshotSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.current = spec
}

// seedBaseline stores baseSpec as the container's baseline snapshot.
func (h *driftHarness) seedBaseline(t *testing.T) domain.Snapshot {
	t.Helper()

	spec := baseSpec()
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	created, err := h.db.Snapshots.Create(context.Background(), domain.Snapshot{
		ContainerID:   "container-a",
		ContainerName: "web",
		Trigger:       domain.SnapshotTriggerManual,
		SpecVersion:   domain.SnapshotSpecVersion,
		SpecJSON:      document,
		Checksum:      strings.Repeat("a", 64),
		CreatedAt:     time.Now().UTC(),
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("create baseline: %v", err)
	}
	return created
}

// driftContainers satisfies service.DriftContainers, serving the harness's
// in-memory container rather than requiring an inventory refresh.
type driftContainers struct {
	harness *driftHarness
	repo    *store.ContainerRepository
}

func (c driftContainers) GetPresent(ctx context.Context, id string) (*domain.ContainerDetail, error) {
	if id != "container-a" {
		return nil, store.ErrNotFound
	}
	c.harness.mu.Lock()
	defer c.harness.mu.Unlock()
	detail := c.harness.container
	return &detail, nil
}

func (c driftContainers) List(ctx context.Context, filter store.ContainerFilter) ([]domain.ContainerSummary, int, error) {
	if filter.Page.Offset > 0 {
		return nil, 1, nil
	}
	return []domain.ContainerSummary{{ID: "container-a", Name: "web", Present: true}}, 1, nil
}

// ------------------------------------------------------------- detection --

// The headline case: a container that has gone privileged since its baseline.
func TestDetectsSecurityDrift(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)

	drifted := baseSpec()
	drifted.Security.Privileged = true
	drifted.Security.ReadonlyRootfs = false
	harness.setCurrent(drifted)

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "container-a")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}
	if !evaluation.Complete {
		t.Errorf("evaluation should be complete: %s", evaluation.Reason)
	}

	records, _, err := harness.db.Drift.List(context.Background(), store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := make(map[string]domain.DriftRecord, len(records))
	for _, record := range records {
		found[record.Field] = record
	}

	privileged, ok := found["privileged"]
	if !ok {
		t.Fatalf("privileged drift was not detected; got %v", fieldNames(records))
	}
	if privileged.Severity != domain.DriftSeverityCritical {
		t.Errorf("privileged severity = %q, want critical", privileged.Severity)
	}
	if privileged.Category != domain.DriftCategorySecurity {
		t.Errorf("category = %q, want security", privileged.Category)
	}
	if privileged.PreviousValue != "false" || privileged.CurrentValue != "true" {
		t.Errorf("values = %q -> %q, want false -> true",
			privileged.PreviousValue, privileged.CurrentValue)
	}

	rootfs, ok := found["readonlyRootfs"]
	if !ok {
		t.Fatal("readonlyRootfs drift was not detected")
	}
	if rootfs.Severity != domain.DriftSeverityCritical {
		t.Errorf("readonlyRootfs severity = %q, want critical", rootfs.Severity)
	}
}

func TestDetectsImageDrift(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)

	drifted := baseSpec()
	drifted.Image.Reference = "nginx:1.28"
	drifted.Image.Digest = "sha256:bbb"
	drifted.Image.ImageID = "sha256:bbb"
	harness.setCurrent(drifted)

	if _, err := harness.service.EvaluateContainer(context.Background(), "container-a"); err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	records, _, err := harness.db.Drift.List(context.Background(), store.DriftFilter{
		Categories: []domain.DriftCategory{domain.DriftCategoryImage},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("expected reference and digest drift, got %v", fieldNames(records))
	}

	for _, record := range records {
		switch record.Field {
		case "image.digest", "image.id":
			if record.Severity != domain.DriftSeverityCritical {
				t.Errorf("%s = %q, want critical", record.Field, record.Severity)
			}
		case "image.reference":
			if record.Severity != domain.DriftSeverityHigh {
				t.Errorf("image.reference = %q, want high", record.Severity)
			}
		}
	}
}

// Environment, labels, ports, mounts, networks, resources, restart policy,
// health, logging and Compose all detected through the same engine.
func TestDetectsEveryRequiredCategory(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)

	drifted := baseSpec()
	drifted.Environment = []domain.SpecEnvVar{
		{Name: "LOG_LEVEL", Value: "debug", Sensitivity: domain.SensitivityNormal, Present: true},
	}
	drifted.Labels = []domain.Label{{Key: "com.example.owner", Value: "someone-else"}}
	drifted.Ports = []domain.Port{{ContainerPort: 80, Protocol: "tcp", Published: true, HostPort: 8080, HostIP: "0.0.0.0"}}
	drifted.Mounts = []domain.SpecMount{{Type: domain.MountTypeBind, Source: "/etc", Destination: "/host-etc"}}
	drifted.Networks = []domain.SpecNetwork{{NetworkName: "backend"}}
	drifted.Resources = domain.Resources{MemoryBytes: 536870912}
	drifted.RestartPolicy = domain.RestartPolicy{Name: "no"}
	drifted.HealthCheck = &domain.HealthCheck{Test: []string{"CMD", "true"}, IntervalMS: 30000}
	drifted.Logging = domain.SpecLogging{Driver: "json-file"}
	drifted.Compose = domain.ComposeMetadata{Managed: true, Project: "stack"}
	drifted.Process.Command = []string{"nginx", "-g", "daemon on;"}
	harness.setCurrent(drifted)

	if _, err := harness.service.EvaluateContainer(context.Background(), "container-a"); err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	records, _, err := harness.db.Drift.List(context.Background(), store.DriftFilter{
		Page: store.Page{Limit: 200},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	seen := make(map[domain.DriftCategory]bool, len(records))
	for _, record := range records {
		seen[record.Category] = true
	}

	for _, required := range []domain.DriftCategory{
		domain.DriftCategoryEnvironment, domain.DriftCategoryLabels,
		domain.DriftCategoryPorts, domain.DriftCategoryMounts,
		domain.DriftCategoryNetworks, domain.DriftCategoryResources,
		domain.DriftCategoryRestart, domain.DriftCategoryHealth,
		domain.DriftCategoryLogging, domain.DriftCategoryCompose,
		domain.DriftCategoryProcess,
	} {
		if !seen[required] {
			t.Errorf("category %q was not detected", required)
		}
	}
}

// A secret-backed field reports THAT it changed and never what to.
func TestSensitiveEnvironmentDriftWithholdsValues(t *testing.T) {
	harness := newDriftHarness(t)

	const (
		oldSecret = "OLD-SECRET-VALUE-111"
		newSecret = "NEW-SECRET-VALUE-222"
	)

	// The baseline carries a sensitive variable with a digest and no value,
	// exactly as capture would have written it.
	spec := baseSpec()
	spec.Environment = append(spec.Environment, domain.SpecEnvVar{
		Name:            "DB_PASSWORD",
		Sensitivity:     domain.SensitivitySensitive,
		Present:         true,
		Length:          len(oldSecret),
		Digest:          "digest-of-old",
		DigestAlgorithm: domain.DigestHMACSHA256,
		DigestKeyID:     "key-1",
	})
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	baseline, err := harness.db.Snapshots.Create(context.Background(), domain.Snapshot{
		ContainerID: "container-a", ContainerName: "web",
		Trigger: domain.SnapshotTriggerManual, SpecVersion: domain.SnapshotSpecVersion,
		SpecJSON: document, Checksum: strings.Repeat("b", 64), CreatedAt: time.Now().UTC(),
	}, []domain.SnapshotEnvEntry{{
		Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive, Present: true,
		Length: len(oldSecret), Digest: "digest-of-old",
		DigestAlgorithm: domain.DigestHMACSHA256, DigestKeyID: "key-1",
	}}, nil, nil)
	if err != nil {
		t.Fatalf("create baseline: %v", err)
	}
	_ = baseline

	// The current configuration has a DIFFERENT secret, again value-free.
	drifted := baseSpec()
	drifted.Environment = append(drifted.Environment, domain.SpecEnvVar{
		Name:            "DB_PASSWORD",
		Sensitivity:     domain.SensitivitySensitive,
		Present:         true,
		Length:          len(newSecret),
		Digest:          "digest-of-new",
		DigestAlgorithm: domain.DigestHMACSHA256,
		DigestKeyID:     "key-1",
	})
	harness.setCurrent(drifted)

	if _, err := harness.service.EvaluateContainer(context.Background(), "container-a"); err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	records, _, err := harness.db.Drift.List(context.Background(), store.DriftFilter{
		Categories: []domain.DriftCategory{domain.DriftCategoryEnvironment},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var password *domain.DriftRecord
	for i := range records {
		if records[i].Field == "DB_PASSWORD" {
			password = &records[i]
		}
	}
	if password == nil {
		t.Fatalf("the sensitive variable's change was not detected; got %v", fieldNames(records))
	}

	if !password.Sensitive {
		t.Error("the record must be marked sensitive so a UI can explain the withheld values")
	}
	if password.PreviousValue != "" || password.CurrentValue != "" {
		t.Errorf("a sensitive record carries values: %q / %q",
			password.PreviousValue, password.CurrentValue)
	}
	if password.Kind != domain.ChangeModified {
		t.Errorf("kind = %q, want modified; the digest comparison establishes that it changed",
			password.Kind)
	}
}

// Digests produced under DIFFERENT keys cannot be compared. Reporting
// "modified" there would tell an operator every secret changed at once after a
// key rotation -- a false alarm indistinguishable from a breach.
func TestDigestsFromDifferentKeysAreUnverifiableRatherThanChanged(t *testing.T) {
	harness := newDriftHarness(t)

	spec := baseSpec()
	spec.Environment = append(spec.Environment, domain.SpecEnvVar{
		Name: "DB_PASSWORD", Sensitivity: domain.SensitivitySensitive, Present: true,
		Digest: "digest-a", DigestAlgorithm: domain.DigestHMACSHA256, DigestKeyID: "key-1",
	})
	document, _ := json.Marshal(spec)
	if _, err := harness.db.Snapshots.Create(context.Background(), domain.Snapshot{
		ContainerID: "container-a", ContainerName: "web",
		Trigger: domain.SnapshotTriggerManual, SpecVersion: domain.SnapshotSpecVersion,
		SpecJSON: document, Checksum: strings.Repeat("c", 64), CreatedAt: time.Now().UTC(),
	}, []domain.SnapshotEnvEntry{{
		Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive, Present: true,
		Digest: "digest-a", DigestAlgorithm: domain.DigestHMACSHA256, DigestKeyID: "key-1",
	}}, nil, nil); err != nil {
		t.Fatalf("create baseline: %v", err)
	}

	// Same value, but the key was rotated, so the digest differs for a reason
	// that says nothing about the secret.
	drifted := baseSpec()
	drifted.Environment = append(drifted.Environment, domain.SpecEnvVar{
		Name: "DB_PASSWORD", Sensitivity: domain.SensitivitySensitive, Present: true,
		Digest: "digest-b", DigestAlgorithm: domain.DigestHMACSHA256, DigestKeyID: "key-2",
	})
	harness.setCurrent(drifted)

	if _, err := harness.service.EvaluateContainer(context.Background(), "container-a"); err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	records, _, err := harness.db.Drift.List(context.Background(), store.DriftFilter{
		Categories: []domain.DriftCategory{domain.DriftCategoryEnvironment},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, record := range records {
		if record.Field != "DB_PASSWORD" {
			continue
		}
		if record.Kind != domain.ChangeUnverifiable {
			t.Errorf("kind = %q, want unverifiable after a key rotation", record.Kind)
		}
		if record.PreviousValue != "" || record.CurrentValue != "" {
			t.Error("an unverifiable sensitive record must still carry no values")
		}
		return
	}
	t.Fatalf("the rotated digest produced no record; got %v", fieldNames(records))
}

// An identical configuration produces no drift -- but DOES produce an
// evaluation, which is what distinguishes clean from never-checked.
func TestNoDriftStillRecordsAnEvaluation(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)
	harness.setCurrent(baseSpec())

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "container-a")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}
	if evaluation.DriftCount != 0 {
		t.Errorf("driftCount = %d, want 0", evaluation.DriftCount)
	}
	if !evaluation.Complete {
		t.Errorf("evaluation should be complete: %s", evaluation.Reason)
	}

	_, total, err := harness.db.Drift.List(context.Background(), store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want no drift records", total)
	}

	stored, err := harness.db.Drift.Evaluation(context.Background(), "container-a")
	if err != nil {
		t.Fatalf("the evaluation must be recorded even when it found nothing: %v", err)
	}
	if !stored.Complete || stored.DriftCount != 0 {
		t.Errorf("stored evaluation = %+v", stored)
	}
}

// A container with no baseline is recorded as INCOMPLETE with a reason, not as
// clean. Telling an operator their estate is drift-free when most of it was
// never comparable is the worst thing a posture dashboard can do.
func TestMissingBaselineIsIncompleteNotClean(t *testing.T) {
	harness := newDriftHarness(t)
	harness.setCurrent(baseSpec())

	evaluation, err := harness.service.EvaluateContainer(context.Background(), "container-a")
	if err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}
	if evaluation.Complete {
		t.Error("an evaluation with no baseline must not be complete")
	}
	if !strings.Contains(evaluation.Reason, "baseline") {
		t.Errorf("reason = %q, want it to explain the missing baseline", evaluation.Reason)
	}

	stored, err := harness.db.Drift.Evaluation(context.Background(), "container-a")
	if err != nil {
		t.Fatalf("Evaluation: %v", err)
	}
	if stored.Complete {
		t.Error("the stored evaluation must be marked incomplete")
	}
}

// A container that has left the inventory records nothing: there is no current
// configuration to compare, and writing an evaluation would claim a comparison
// that did not happen.
func TestVanishedContainerIsNotEvaluated(t *testing.T) {
	harness := newDriftHarness(t)

	_, err := harness.service.EvaluateContainer(context.Background(), "container-gone")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}

	if _, err := harness.db.Drift.Evaluation(context.Background(), "container-gone"); !errors.Is(err, store.ErrNotFound) {
		t.Error("no evaluation should have been recorded for a container that is gone")
	}
}

// Re-evaluating an unchanged difference does not duplicate it, and the drift
// that goes away resolves. The full lifecycle through the real engine.
func TestDriftLifecycleThroughTheEngine(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)
	ctx := context.Background()

	drifted := baseSpec()
	drifted.Security.Privileged = true
	harness.setCurrent(drifted)

	for range 5 {
		if _, err := harness.service.EvaluateContainer(ctx, "container-a"); err != nil {
			t.Fatalf("EvaluateContainer: %v", err)
		}
	}

	records, total, err := harness.db.Drift.List(ctx, store.DriftFilter{
		Categories: []domain.DriftCategory{domain.DriftCategorySecurity},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d after 5 evaluations, want 1; repeats must collapse", total)
	}
	recordID := records[0].ID

	// The container is put back the way the baseline describes.
	harness.setCurrent(baseSpec())
	if _, err := harness.service.EvaluateContainer(ctx, "container-a"); err != nil {
		t.Fatalf("EvaluateContainer: %v", err)
	}

	resolved, err := harness.db.Drift.Get(ctx, recordID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resolved.Status != domain.DriftStatusResolved {
		t.Errorf("status = %q, want resolved", resolved.Status)
	}
	if resolved.ResolvedAt == nil {
		t.Error("resolvedAt must be set")
	}
}

// A disabled engine evaluates nothing and says so.
func TestDisabledEngineEvaluatesNothing(t *testing.T) {
	harness := newDriftHarness(t, func(cfg *config.Drift) { cfg.Enabled = false })
	harness.seedBaseline(t)

	if harness.service.Enabled() {
		t.Error("Enabled must report false")
	}
	if _, err := harness.service.EvaluateContainer(context.Background(), "container-a"); !errors.Is(err, service.ErrDriftDisabled) {
		t.Errorf("err = %v, want ErrDriftDisabled", err)
	}
	if _, err := harness.service.Sweep(context.Background()); !errors.Is(err, service.ErrDriftDisabled) {
		t.Errorf("sweep err = %v, want ErrDriftDisabled", err)
	}
}

// The sweep evaluates the estate and is the authoritative pass.
func TestSweepEvaluatesEveryContainer(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)

	drifted := baseSpec()
	drifted.Security.Privileged = true
	harness.setCurrent(drifted)

	result, err := harness.service.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Evaluated != 1 {
		t.Errorf("evaluated = %d, want 1", result.Evaluated)
	}
	if result.Failed != 0 {
		t.Errorf("failed = %d, want 0", result.Failed)
	}

	_, total, err := harness.db.Drift.List(context.Background(), store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total == 0 {
		t.Error("the sweep detected no drift")
	}
}

// Two sweeps must not overlap: the second is refused rather than queued,
// because it would re-read the same containers to reach the same conclusion.
func TestConcurrentSweepsDoNotOverlap(t *testing.T) {
	harness := newDriftHarness(t)
	harness.seedBaseline(t)
	harness.setCurrent(baseSpec())

	var group sync.WaitGroup
	results := make(chan service.SweepResult, 4)

	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := harness.service.Sweep(context.Background())
			if err != nil {
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)

	// At least one ran; the refused ones report zero work rather than erroring.
	var evaluated int
	for result := range results {
		evaluated += result.Evaluated
	}
	if evaluated == 0 {
		t.Error("no sweep ran at all")
	}
}

// ---------------------------------------------------------------- queueing --

// An event storm for one container collapses into one pending entry.
func TestRepeatedRequestsCoalesce(t *testing.T) {
	harness := newDriftHarness(t, func(cfg *config.Drift) {
		cfg.EvaluationDebounce = time.Hour // never becomes due during the test
	})

	for range 1000 {
		harness.service.RequestEvaluation("container-a")
	}

	status := harness.service.Status()
	if status.PendingEvaluations != 1 {
		t.Errorf("pending = %d, want 1; a thousand events for one container is one entry",
			status.PendingEvaluations)
	}
	if status.Overflowed {
		t.Error("one container must not overflow the queue")
	}
}

// Past the cap the queue stops tracking containers individually and escalates
// to a sweep, which covers all of them and costs less than the tracking.
func TestQueueOverflowEscalatesToASweep(t *testing.T) {
	harness := newDriftHarness(t, func(cfg *config.Drift) {
		cfg.EvaluationDebounce = time.Hour
		cfg.MaxPendingEvaluations = 8
	})

	for i := range 100 {
		harness.service.RequestEvaluation(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}

	status := harness.service.Status()
	if !status.Overflowed {
		t.Error("the queue must report that it overflowed")
	}
	if !status.SweepPending {
		t.Error("an overflow must escalate to a sweep")
	}
	if status.PendingEvaluations > 8 {
		t.Errorf("pending = %d, want at most the cap of 8", status.PendingEvaluations)
	}
}

// A sweep request supersedes pending per-container work: the sweep evaluates
// everything they name.
func TestSweepRequestDiscardsPendingWork(t *testing.T) {
	harness := newDriftHarness(t, func(cfg *config.Drift) {
		cfg.EvaluationDebounce = time.Hour
	})

	harness.service.RequestEvaluation("container-a")
	harness.service.RequestEvaluation("container-b")
	harness.service.RequestSweep()

	status := harness.service.Status()
	if status.PendingEvaluations != 0 {
		t.Errorf("pending = %d, want 0; a sweep covers them all", status.PendingEvaluations)
	}
	if !status.SweepPending {
		t.Error("the sweep must be pending")
	}
}

// A disabled engine ignores requests entirely rather than queueing work that
// will never run.
func TestDisabledEngineIgnoresRequests(t *testing.T) {
	harness := newDriftHarness(t, func(cfg *config.Drift) { cfg.Enabled = false })

	harness.service.RequestEvaluation("container-a")
	harness.service.RequestSweep()

	if status := harness.service.Status(); status.PendingEvaluations != 0 || status.SweepPending {
		t.Errorf("a disabled engine queued work: %+v", status)
	}
}

// The worker must evaluate a requested container and then stop cleanly.
func TestWorkerEvaluatesAndShutsDown(t *testing.T) {
	harness := newDriftHarness(t, func(cfg *config.Drift) {
		cfg.SweepOnStartup = false
		cfg.SweepInterval = 0
		cfg.EvaluationDebounce = time.Millisecond
	})
	harness.seedBaseline(t)

	drifted := baseSpec()
	drifted.Security.Privileged = true
	harness.setCurrent(drifted)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	harness.service.RequestEvaluation("container-a")

	eventually(t, 5*time.Second, "the worker to record drift", func() bool {
		_, total, err := harness.db.Drift.List(context.Background(), store.DriftFilter{})
		return err == nil && total > 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drift worker did not shut down; a goroutine is stuck")
	}
}

func fieldNames(records []domain.DriftRecord) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Field)
	}
	return names
}
