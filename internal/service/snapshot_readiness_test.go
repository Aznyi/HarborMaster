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

// fakeInventory is a readiness fixture with no database.
type fakeInventory struct {
	generation  int64
	lastRefresh *domain.RefreshRecord
	images      map[string]struct{}
	networks    map[string]struct{}
	volumes     map[string]struct{}

	imageErr   error
	networkErr error
	volumeErr  error

	// Call counters, to prove readiness never drives a refresh.
	imageLookups int
}

func (f *fakeInventory) CurrentGeneration(context.Context) (int64, string, error) {
	return f.generation, "", nil
}

func (f *fakeInventory) LastSuccessfulRefresh(context.Context) (*domain.RefreshRecord, error) {
	return f.lastRefresh, nil
}

func (f *fakeInventory) ImageExists(_ context.Context, imageID, reference string) (bool, error) {
	f.imageLookups++
	if f.imageErr != nil {
		return false, f.imageErr
	}
	if _, ok := f.images[imageID]; ok {
		return true, nil
	}
	_, ok := f.images[reference]
	return ok, nil
}

func (f *fakeInventory) NetworkNames(context.Context) (map[string]struct{}, error) {
	if f.networkErr != nil {
		return nil, f.networkErr
	}
	return f.networks, nil
}

func (f *fakeInventory) VolumeNames(context.Context) (map[string]struct{}, error) {
	if f.volumeErr != nil {
		return nil, f.volumeErr
	}
	return f.volumes, nil
}

type fakePinger struct {
	err   error
	pings int
}

func (p *fakePinger) CheckRuntime(context.Context) error {
	p.pings++
	return p.err
}

// evalFixture assembles a healthy baseline that every check passes.
type evalFixture struct {
	inventory *fakeInventory
	pinger    *fakePinger
	snapshot  domain.Snapshot
	env       []domain.SnapshotEnvEntry
	mounts    []domain.SnapshotMountRow
	networks  []domain.SnapshotNetworkRow
	now       time.Time
	maxAge    time.Duration
}

func newEvalFixture(t *testing.T) *evalFixture {
	t.Helper()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-time.Minute)

	spec := domain.SnapshotSpec{
		SpecVersion: domain.SnapshotSpecVersion,
		Identity:    domain.SpecIdentity{ContainerID: "c1", ContainerName: "web"},
		Image: domain.SpecImage{
			Reference: "nginx:1.27", ImageID: "sha256:aaaa", Digest: "sha256:dddd",
		},
		RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
	}
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	return &evalFixture{
		inventory: &fakeInventory{
			generation:  7,
			lastRefresh: &domain.RefreshRecord{Generation: 7, FinishedAt: &finished},
			images:      map[string]struct{}{"sha256:aaaa": {}},
			networks:    map[string]struct{}{"shop_default": {}},
			volumes:     map[string]struct{}{"web-data": {}},
		},
		pinger: &fakePinger{},
		snapshot: domain.Snapshot{
			ID: 1, ContainerID: "c1", ContainerName: "web",
			ImageReference: "nginx:1.27", ImageID: "sha256:aaaa",
			SpecVersion: domain.SnapshotSpecVersion, SpecJSON: blob,
		},
		env:      []domain.SnapshotEnvEntry{{Key: "PATH", Classification: domain.SensitivityNormal, Present: true, Value: "/usr/bin"}},
		mounts:   []domain.SnapshotMountRow{{Destination: "/data", Type: domain.MountTypeVolume, VolumeName: "web-data"}},
		networks: []domain.SnapshotNetworkRow{{NetworkName: "shop_default"}},
		now:      now,
		maxAge:   15 * time.Minute,
	}
}

// withSpec re-encodes the snapshot document after a mutation.
func (f *evalFixture) withSpec(t *testing.T, mutate func(*domain.SnapshotSpec)) *evalFixture {
	t.Helper()
	var spec domain.SnapshotSpec
	if err := json.Unmarshal(f.snapshot.SpecJSON, &spec); err != nil {
		t.Fatal(err)
	}
	mutate(&spec)
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot.SpecJSON = blob
	return f
}

func (f *evalFixture) evaluate(t *testing.T) domain.ReadinessReport {
	t.Helper()
	engine := NewReadinessEngine(ReadinessOptions{
		Inventory: f.inventory,
		Pinger:    f.pinger,
		Config:    config.Snapshots{MaxInventoryAge: f.maxAge},
		Now:       func() time.Time { return f.now },
	})
	report, err := engine.Evaluate(context.Background(), f.snapshot, f.env, f.mounts, f.networks)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return report
}

func findCheck(t *testing.T, report domain.ReadinessReport, id domain.ReadinessCheckID) domain.ReadinessCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("check %q not present in the report", id)
	return domain.ReadinessCheck{}
}

func TestHealthyBaselineIsReady(t *testing.T) {
	report := newEvalFixture(t).evaluate(t)

	if report.Status != domain.ReadinessReady {
		t.Errorf("Status = %q, want ready\nchecks: %+v", report.Status, report.Checks)
	}
	if len(report.Checks) != len(domain.ReadinessCheckIDs) {
		t.Errorf("produced %d checks, want %d", len(report.Checks), len(domain.ReadinessCheckIDs))
	}
}

func TestEveryCheckIsPresentInTheReport(t *testing.T) {
	report := newEvalFixture(t).evaluate(t)

	seen := make(map[domain.ReadinessCheckID]bool, len(report.Checks))
	for _, check := range report.Checks {
		seen[check.ID] = true
	}
	for _, id := range domain.ReadinessCheckIDs {
		if !seen[id] {
			t.Errorf("check %q missing from the report", id)
		}
	}
}

func TestUnreachableDaemonIsNotReady(t *testing.T) {
	f := newEvalFixture(t)
	f.pinger.err = errors.New("dial unix /var/run/docker.sock: connect: permission denied")

	report := f.evaluate(t)
	if report.Status != domain.ReadinessNotReady {
		t.Errorf("Status = %q, want not_ready", report.Status)
	}
	// The raw error names the socket path and must not reach the report.
	check := findCheck(t, report, domain.CheckDaemonReachable)
	if strings.Contains(check.Detail, "docker.sock") || strings.Contains(check.Detail, "permission denied") {
		t.Errorf("check detail leaked the raw error: %q", check.Detail)
	}
}

// A bind mount can never be verified in Phase 3, and an unverifiable check must
// stop the report claiming "ready".
func TestUnverifiableDegradesOverallToWarning(t *testing.T) {
	f := newEvalFixture(t)
	f.mounts = append(f.mounts, domain.SnapshotMountRow{
		Destination: "/etc/nginx/conf.d", Type: domain.MountTypeBind, Source: "/srv/conf",
	})

	report := f.evaluate(t)
	if report.Status == domain.ReadinessReady {
		t.Error("Status = ready despite an unverifiable check; readiness must fail closed")
	}
	if report.Status != domain.ReadinessWarning {
		t.Errorf("Status = %q, want warning", report.Status)
	}
	if findCheck(t, report, domain.CheckMountSources).Status != domain.ReadinessUnverifiable {
		t.Error("bind mounts should report unverifiable")
	}
}

func TestStaleInventoryDegradesOverall(t *testing.T) {
	f := newEvalFixture(t)
	finished := f.now.Add(-2 * time.Hour)
	f.inventory.lastRefresh = &domain.RefreshRecord{Generation: 7, FinishedAt: &finished}

	report := f.evaluate(t)
	if report.Status == domain.ReadinessReady {
		t.Error("Status = ready on a two-hour-old inventory; it must degrade")
	}
	if !report.InventoryStale {
		t.Error("InventoryStale not set")
	}
	if report.InventoryAgeSeconds != int64((2 * time.Hour).Seconds()) {
		t.Errorf("InventoryAgeSeconds = %d, want %d", report.InventoryAgeSeconds, int64((2 * time.Hour).Seconds()))
	}
}

func TestNoSuccessfulRefreshIsNotReady(t *testing.T) {
	f := newEvalFixture(t)
	f.inventory.lastRefresh = nil

	report := f.evaluate(t)
	if report.Status != domain.ReadinessNotReady {
		t.Errorf("Status = %q, want not_ready with no inventory basis at all", report.Status)
	}
}

// Age is measured from the last SUCCESS. A failed attempt does not make the
// data any fresher, and measuring from it would hide staleness.
func TestAgeMeasuredFromLastSuccessNotLastAttempt(t *testing.T) {
	f := newEvalFixture(t)
	// The last SUCCESS was three hours ago; a failed attempt a minute ago is
	// not represented here at all, which is the point: the engine asks for the
	// last successful refresh specifically.
	finished := f.now.Add(-3 * time.Hour)
	f.inventory.lastRefresh = &domain.RefreshRecord{Generation: 7, FinishedAt: &finished}

	report := f.evaluate(t)
	if !report.InventoryStale {
		t.Error("a three-hour-old successful refresh should read as stale")
	}
	if report.InventoryAgeSeconds < int64((3 * time.Hour).Seconds()) {
		t.Errorf("InventoryAgeSeconds = %d; age must come from the last success", report.InventoryAgeSeconds)
	}
}

// Because HarborMaster stores no secret values, a container with sensitive
// variables can never be "ready".
func TestSensitiveEnvironmentCapsAtWarning(t *testing.T) {
	f := newEvalFixture(t)
	f.env = append(f.env, domain.SnapshotEnvEntry{
		Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive,
		Present: true, Length: 12, Digest: "deadbeef",
	})

	report := f.evaluate(t)
	if report.Status == domain.ReadinessReady {
		t.Error("Status = ready despite secrets that cannot be restored")
	}

	check := findCheck(t, report, domain.CheckSecretsAvailable)
	if check.Status != domain.ReadinessWarning {
		t.Errorf("secrets check = %q, want warning", check.Status)
	}
	if !strings.Contains(check.Detail, "never stores secret values") {
		t.Errorf("the detail should state the design plainly: %q", check.Detail)
	}
}

func TestMissingVolumeIsNotReady(t *testing.T) {
	f := newEvalFixture(t)
	f.inventory.volumes = map[string]struct{}{}

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckNamedVolumes).Status != domain.ReadinessNotReady {
		t.Error("a missing named volume should be not_ready")
	}
	if report.Status != domain.ReadinessNotReady {
		t.Errorf("Status = %q, want not_ready", report.Status)
	}
}

func TestMissingNetworkIsNotReady(t *testing.T) {
	f := newEvalFixture(t)
	f.inventory.networks = map[string]struct{}{}

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckNetworksPresent).Status != domain.ReadinessNotReady {
		t.Error("a missing network should be not_ready")
	}
}

// The predefined networks always exist on a working daemon.
func TestPredefinedNetworksAreAlwaysPresent(t *testing.T) {
	f := newEvalFixture(t)
	f.inventory.networks = map[string]struct{}{}
	f.networks = []domain.SnapshotNetworkRow{{NetworkName: "bridge"}, {NetworkName: "host"}}

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckNetworksPresent).Status != domain.ReadinessReady {
		t.Error("bridge and host should not be reported missing")
	}
}

// A missing image is an obstacle, not a wall: a future phase would pull it.
func TestMissingImageIsAWarningNotAFailure(t *testing.T) {
	f := newEvalFixture(t)
	f.inventory.images = map[string]struct{}{}

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckImageAvailable).Status != domain.ReadinessWarning {
		t.Error("a missing image should be a warning; it is recoverable by pulling")
	}
}

func TestMissingImageDigestIsAWarning(t *testing.T) {
	f := newEvalFixture(t).withSpec(t, func(s *domain.SnapshotSpec) {
		s.Image.Digest = ""
		s.Image.RepoDigests = nil
	})
	f.snapshot.ImageDigest = ""

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckImageDigestKnown).Status != domain.ReadinessWarning {
		t.Error("an undigested image should warn: the restore target is a mutable tag")
	}
}

func TestInvalidRestartPolicyIsNotReady(t *testing.T) {
	f := newEvalFixture(t).withSpec(t, func(s *domain.SnapshotSpec) {
		s.RestartPolicy = domain.RestartPolicy{Name: "sometimes"}
	})

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckRestartPolicyValid).Status != domain.ReadinessNotReady {
		t.Error("an unknown restart policy should be not_ready")
	}
}

func TestIncompleteComposeMetadataWarns(t *testing.T) {
	f := newEvalFixture(t).withSpec(t, func(s *domain.SnapshotSpec) {
		s.Compose = domain.ComposeMetadata{Managed: true, Project: "shop"}
	})

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckComposeMetadata).Status != domain.ReadinessWarning {
		t.Error("incomplete Compose metadata should warn")
	}
}

func TestInconsistentConfigurationIsNotReady(t *testing.T) {
	f := newEvalFixture(t).withSpec(t, func(s *domain.SnapshotSpec) {
		s.Resources.MemoryBytes = 100
		s.Resources.MemoryReservationBytes = 500
	})

	report := f.evaluate(t)
	if findCheck(t, report, domain.CheckConfigConsistent).Status != domain.ReadinessNotReady {
		t.Error("a reservation above the limit should be not_ready")
	}
}

func TestPrivilegedContainerWarnsOnRuntimeFeatures(t *testing.T) {
	f := newEvalFixture(t).withSpec(t, func(s *domain.SnapshotSpec) {
		s.Security.Privileged = true
	})

	report := f.evaluate(t)
	check := findCheck(t, report, domain.CheckRuntimeFeatures)
	if check.Status != domain.ReadinessWarning {
		t.Error("privileged mode should warn")
	}
	if !strings.Contains(check.Detail, "root-equivalent") {
		t.Errorf("the detail should say what privileged means: %q", check.Detail)
	}
}

// A read endpoint that can drive privileged socket traffic is a DoS amplifier.
func TestEvaluationPingsOnceAndNeverRefreshes(t *testing.T) {
	f := newEvalFixture(t)
	report := f.evaluate(t)

	if f.pinger.pings != 1 {
		t.Errorf("pings = %d, want exactly 1", f.pinger.pings)
	}
	if f.inventory.imageLookups != 1 {
		t.Errorf("imageLookups = %d, want exactly 1", f.inventory.imageLookups)
	}
	if report.DaemonCheckedAt == nil {
		t.Error("DaemonCheckedAt not recorded")
	}
}

func TestLookupFailuresAreUnverifiableNotReady(t *testing.T) {
	f := newEvalFixture(t)
	f.inventory.networkErr = errors.New("database is locked")
	f.inventory.volumeErr = errors.New("database is locked")
	f.inventory.imageErr = errors.New("database is locked")

	report := f.evaluate(t)
	for _, id := range []domain.ReadinessCheckID{
		domain.CheckNetworksPresent, domain.CheckNamedVolumes, domain.CheckImageAvailable,
	} {
		if got := findCheck(t, report, id).Status; got != domain.ReadinessUnverifiable {
			t.Errorf("check %q = %q, want unverifiable when the lookup fails", id, got)
		}
	}
	if report.Status == domain.ReadinessReady {
		t.Error("Status = ready when nothing could be verified")
	}
}

// No check detail may echo a secret, and none of the inputs here should ever
// reach the report.
func TestCheckDetailsNeverContainSecrets(t *testing.T) {
	f := newEvalFixture(t)
	f.env = append(f.env, domain.SnapshotEnvEntry{
		Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive,
		Present: true, Length: 7, Digest: "digest-must-not-appear",
	})

	report := f.evaluate(t)
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"digest-must-not-appear", "hunter2"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the report leaked %q", needle)
		}
	}
}

func TestWorstStatusFold(t *testing.T) {
	cases := []struct {
		name  string
		input []domain.ReadinessStatus
		want  domain.ReadinessStatus
	}{
		{"all ready", []domain.ReadinessStatus{domain.ReadinessReady, domain.ReadinessReady}, domain.ReadinessReady},
		{"warning wins over ready", []domain.ReadinessStatus{domain.ReadinessReady, domain.ReadinessWarning}, domain.ReadinessWarning},
		{"not ready dominates", []domain.ReadinessStatus{domain.ReadinessWarning, domain.ReadinessNotReady, domain.ReadinessReady}, domain.ReadinessNotReady},
		{"unverifiable caps at warning", []domain.ReadinessStatus{domain.ReadinessReady, domain.ReadinessUnverifiable}, domain.ReadinessWarning},
		{"empty is not ready", nil, domain.ReadinessNotReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.WorstStatus(tc.input); got != tc.want {
				t.Errorf("WorstStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNullHostValidationInspectsNothing(t *testing.T) {
	result, err := NewNullHostValidation().PathExists(context.Background(), "/etc/passwd")
	if err != nil {
		t.Fatalf("PathExists: %v", err)
	}
	if result.Status != domain.HostPathUnverifiable {
		t.Errorf("Status = %q, want unverifiable; Phase 3 must not touch the filesystem", result.Status)
	}
}

func TestJoinLimitedBoundsOutput(t *testing.T) {
	many := make([]string, 200)
	for i := range many {
		many[i] = "volume-" + strconv.Itoa(i)
	}
	got := joinLimited(many, 5)
	if strings.Count(got, ",") > 5 {
		t.Errorf("joinLimited emitted an unbounded list: %q", got)
	}
	if !strings.Contains(got, "and 195 more") {
		t.Errorf("truncation should be explicit: %q", got)
	}
}
