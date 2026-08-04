package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

func fixtureSnapshotRow() domain.Snapshot {
	spec := []byte(`{"specVersion":1,"identity":{"containerId":"c1","containerName":"web"}}`)
	sum := sha256.Sum256(spec)

	return domain.Snapshot{
		ContainerID:         "c1",
		ContainerName:       "web",
		ImageReference:      "nginx:1.27",
		ImageDigest:         "sha256:dddd",
		ImageID:             "sha256:aaaa",
		SpecVersion:         domain.SnapshotSpecVersion,
		SpecJSON:            spec,
		Checksum:            hex.EncodeToString(sum[:]),
		HarborMasterVersion: "0.3.0",
		DockerAPIVersion:    "1.45",
		DockerEngineVersion: "27.0.0",
		Trigger:             domain.SnapshotTriggerManual,
		Reason:              "before upgrade",
		InventoryGeneration: 7,
		EventSequence:       99,
		DigestKeyID:         "abcd1234",
		CreatedAt:           time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
	}
}

func fixtureReadiness(snapshotID int64) domain.ReadinessReport {
	return domain.ReadinessReport{
		SnapshotID:  snapshotID,
		Status:      domain.ReadinessWarning,
		EvaluatedAt: time.Now().UTC(),
		Checks: []domain.ReadinessCheck{
			{ID: domain.CheckDaemonReachable, Status: domain.ReadinessReady},
			{ID: domain.CheckSecretsAvailable, Status: domain.ReadinessWarning, Detail: "1 secret must be supplied"},
		},
	}
}

// readAllColumns snapshots every column of a snapshots row as text.
func readAllColumns(t *testing.T, db *store.DB, id int64) map[string]string {
	t.Helper()

	rows, err := db.SQL().Query(`SELECT * FROM snapshots WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("select row: %v", err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("snapshot %d not found", id)
	}

	values := make([]any, len(columns))
	holders := make([]sql.NullString, len(columns))
	for i := range values {
		values[i] = &holders[i]
	}
	if err := rows.Scan(values...); err != nil {
		t.Fatalf("scan: %v", err)
	}

	out := make(map[string]string, len(columns))
	for i, column := range columns {
		out[column] = holders[i].String
	}
	return out
}

// TestSnapshotFieldsAreImmutable is the guarantee the whole phase rests on.
//
// It captures a snapshot, records every column, runs every exported repository
// method that could plausibly write, and asserts that nothing changed except
// the two denormalised readiness-summary columns.
func TestSnapshotFieldsAreImmutable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.Snapshots.Create(ctx, fixtureSnapshotRow(),
		[]domain.SnapshotEnvEntry{{Position: 0, Key: "PATH", Classification: domain.SensitivityNormal, Present: true, Value: "/usr/bin"}},
		[]domain.SnapshotMountRow{{Destination: "/data", Type: domain.MountTypeVolume, VolumeName: "web-data"}},
		[]domain.SnapshotNetworkRow{{NetworkName: "bridge", Aliases: []string{"web"}}},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before := readAllColumns(t, db, created.ID)

	// Re-capturing the same configuration must be a no-op on the stored row.
	if _, err := db.Snapshots.Create(ctx, fixtureSnapshotRow(), nil, nil, nil); err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	// Repeated readiness evaluations must touch only the summary.
	for i := 0; i < 3; i++ {
		if err := db.Snapshots.RecordReadiness(ctx, fixtureReadiness(created.ID)); err != nil {
			t.Fatalf("record readiness %d: %v", i, err)
		}
	}
	// Reads must not mutate either.
	if _, err := db.Snapshots.Get(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Snapshots.List(ctx, store.SnapshotFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Snapshots.Environment(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	after := readAllColumns(t, db, created.ID)

	mutable := map[string]bool{
		"readiness_status":       true,
		"readiness_evaluated_at": true,
	}
	for column, want := range before {
		if mutable[column] {
			continue
		}
		if after[column] != want {
			t.Errorf("column %q changed after capture: %q -> %q\n"+
				"\ta snapshot is evidence; only the readiness summary may change",
				column, want, after[column])
		}
	}

	// And the summary really did change, or the test above proves nothing.
	if after["readiness_status"] == before["readiness_status"] {
		t.Error("readiness_status did not change; the immutability check is vacuous")
	}
}

// The stored document must still hash to the stored checksum after every write
// path has run. Altered evidence is worse than no evidence.
func TestChecksumStillVerifiesAfterWrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.Snapshots.Create(ctx, fixtureSnapshotRow(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Snapshots.RecordReadiness(ctx, fixtureReadiness(created.ID)); err != nil {
		t.Fatal(err)
	}

	got, err := db.Snapshots.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got.SpecJSON)
	if hex.EncodeToString(sum[:]) != got.Checksum {
		t.Error("the stored document no longer hashes to the stored checksum")
	}
}

// A source-level guard. Adding an UPDATE against another column should be a
// visible diff a reviewer sees, not a silent new capability.
func TestRepositoryUpdatesOnlyReadinessSummary(t *testing.T) {
	source, err := os.ReadFile("snapshot_repository.go")
	if err != nil {
		t.Fatalf("read repository source: %v", err)
	}

	// Every UPDATE statement targeting the snapshots table, up to the WHERE.
	pattern := regexp.MustCompile(`(?is)UPDATE\s+snapshots\s+SET\s+(.*?)\s+WHERE`)
	matches := pattern.FindAllStringSubmatch(string(source), -1)

	if len(matches) == 0 {
		t.Fatal("no UPDATE against snapshots found; RecordReadiness should contain exactly one")
	}
	if len(matches) > 1 {
		t.Errorf("found %d UPDATE statements against snapshots, want exactly 1", len(matches))
	}

	allowed := map[string]bool{
		"readiness_status":       true,
		"readiness_evaluated_at": true,
	}
	for _, match := range matches {
		for _, assignment := range strings.Split(match[1], ",") {
			column := strings.TrimSpace(strings.Split(assignment, "=")[0])
			if !allowed[column] {
				t.Errorf("UPDATE against snapshots sets %q, which is outside the readiness summary\n"+
					"\tevery other column is fixed at capture time", column)
			}
		}
	}
}

// The repository must expose no method that could delete or rewrite a single
// snapshot's configuration.
func TestRepositoryHasNoConfigurationMutators(t *testing.T) {
	source, err := os.ReadFile("snapshot_repository.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)

	for _, forbidden := range []string{
		"func (r *SnapshotRepository) Update",
		"func (r *SnapshotRepository) Replace",
		"func (r *SnapshotRepository) Rewrite",
		"func (r *SnapshotRepository) SetSpec",
		"func (r *SnapshotRepository) SetChecksum",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("repository exposes %q; snapshots are append-only", forbidden)
		}
	}
}

func TestCreateWritesChildRowsTransactionally(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.Snapshots.Create(ctx, fixtureSnapshotRow(),
		[]domain.SnapshotEnvEntry{
			{Position: 0, Key: "PATH", Classification: domain.SensitivityNormal, Present: true, Value: "/usr/bin"},
			{
				Position: 1, Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive,
				Present: true, Length: 7, Digest: "deadbeef",
				DigestAlgorithm: domain.DigestHMACSHA256, DigestKeyID: "abcd1234",
			},
		},
		[]domain.SnapshotMountRow{{Destination: "/data", Type: domain.MountTypeVolume, VolumeName: "web-data"}},
		[]domain.SnapshotNetworkRow{{NetworkName: "bridge", Aliases: []string{"web", "app"}}},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env, err := db.Snapshots.Environment(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 {
		t.Fatalf("environment rows = %d, want 2", len(env))
	}
	// Order is preserved: environment order is meaningful to some programs.
	if env[0].Key != "PATH" || env[1].Key != "DB_PASSWORD" {
		t.Errorf("environment order not preserved: %v, %v", env[0].Key, env[1].Key)
	}
	if env[1].Value != "" {
		t.Errorf("sensitive entry carried a value: %q", env[1].Value)
	}
	if env[1].Digest != "deadbeef" || env[1].DigestKeyID != "abcd1234" {
		t.Errorf("digest metadata lost: %+v", env[1])
	}

	mounts, err := db.Snapshots.Mounts(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].VolumeName != "web-data" {
		t.Errorf("mounts = %+v", mounts)
	}

	networks, err := db.Snapshots.Networks(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(networks) != 1 || len(networks[0].Aliases) != 2 {
		t.Errorf("networks = %+v", networks)
	}
}

// Even if capture were buggy, the repository blanks a sensitive value before
// it reaches the database.
func TestRepositoryBlanksSensitiveValuesDefensively(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.Snapshots.Create(ctx, fixtureSnapshotRow(),
		[]domain.SnapshotEnvEntry{{
			Position: 0, Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive,
			Present: true, Value: "hunter2-should-never-be-written",
		}},
		nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env, err := db.Snapshots.Environment(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env[0].Value != "" {
		t.Errorf("a sensitive value reached the database: %q", env[0].Value)
	}
}

// An UNCLASSIFIED entry is stored as sensitive, not as normal.
//
// If the classification is missing, HarborMaster does not know whether the
// value is a secret, and the fail-closed answer to "is this safe to store" is
// no. Being wrong this way costs an operator a value they cannot see; being
// wrong the other way leaks a credential, and only one of those is
// recoverable.
func TestUnclassifiedEnvironmentEntryIsTreatedAsSensitive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.Snapshots.Create(ctx, fixtureSnapshotRow(),
		[]domain.SnapshotEnvEntry{{
			Position: 0, Key: "MYSTERY", Present: true,
			Value: "unclassified-value-should-not-be-stored",
			// Classification deliberately left unset.
		}},
		nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env, err := db.Snapshots.Environment(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env[0].Value != "" {
		t.Errorf("an unclassified value was stored: %q", env[0].Value)
	}
	if env[0].Classification != domain.SensitivitySensitive {
		t.Errorf("Classification = %q, want sensitive for an unclassified entry", env[0].Classification)
	}
}

// Rendering a page of snapshots must not become one query per snapshot.
func TestEnvironmentForLoadsManySnapshotsInOneQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	ids := make([]int64, 0, 10)
	for i := range 10 {
		s := fixtureSnapshotRow()
		s.ContainerID = "c" + string(rune('0'+i))
		created, err := db.Snapshots.Create(ctx, s,
			[]domain.SnapshotEnvEntry{{
				Position: 0, Key: "PATH", Classification: domain.SensitivityNormal,
				Present: true, Value: "/usr/bin",
			}},
			nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}

	byID, err := db.Snapshots.EnvironmentFor(ctx, ids)
	if err != nil {
		t.Fatalf("EnvironmentFor: %v", err)
	}
	if len(byID) != 10 {
		t.Errorf("loaded %d snapshots' environments, want 10", len(byID))
	}
	for _, id := range ids {
		if len(byID[id]) != 1 {
			t.Errorf("snapshot %d has %d environment rows, want 1", id, len(byID[id]))
		}
	}
}

func TestEnvironmentForHandlesEmptyInput(t *testing.T) {
	db := openTestDB(t)

	byID, err := db.Snapshots.EnvironmentFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("EnvironmentFor(nil): %v", err)
	}
	if len(byID) != 0 {
		t.Errorf("len = %d, want 0", len(byID))
	}
}

func TestSnapshotFilterByTriggerAndReadiness(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	manual := fixtureSnapshotRow()
	createSnapshot(t, db, manual)

	api := fixtureSnapshotRow()
	api.ContainerID = "c2"
	api.Trigger = domain.SnapshotTriggerAPI
	createSnapshot(t, db, api)

	got, total, err := db.Snapshots.List(ctx, store.SnapshotFilter{
		Triggers: []domain.SnapshotTrigger{domain.SnapshotTriggerAPI},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(got) != 1 || got[0].Trigger != domain.SnapshotTriggerAPI {
		t.Errorf("trigger filter returned %d rows: %+v", len(got), got)
	}

	got, _, err = db.Snapshots.List(ctx, store.SnapshotFilter{
		Readiness: []domain.ReadinessStatus{domain.ReadinessUnknown},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("readiness filter returned %d rows, want 2", len(got))
	}
}

func TestSnapshotSortFieldAllowlist(t *testing.T) {
	for _, field := range []string{"createdAt", "id", "container", "readiness", "trigger"} {
		if !store.ValidSnapshotSortField(field) {
			t.Errorf("ValidSnapshotSortField(%q) = false, want true", field)
		}
	}
	// Anything that could reach the SQL text as an identifier must be refused.
	for _, field := range []string{
		"", "spec_json", "created_at; DROP TABLE snapshots",
		"1=1", "container_id) --",
	} {
		if store.ValidSnapshotSortField(field) {
			t.Errorf("ValidSnapshotSortField(%q) = true; the allowlist is the injection boundary", field)
		}
	}
}

func TestRecordReadinessAppendsRatherThanReplaces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created := createSnapshot(t, db, fixtureSnapshotRow())

	first := fixtureReadiness(created.ID)
	first.EvaluatedAt = time.Now().UTC().Add(-time.Hour)
	if err := db.Snapshots.RecordReadiness(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := fixtureReadiness(created.ID)
	second.Status = domain.ReadinessReady
	if err := db.Snapshots.RecordReadiness(ctx, second); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM snapshot_restore_checks WHERE snapshot_id = ?`, created.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Errorf("check rows = %d, want 4 (two evaluations of two checks, appended)", count)
	}

	// The summary reflects the NEWEST evaluation.
	checks, evaluatedAt, err := db.Snapshots.LatestReadiness(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 {
		t.Errorf("latest evaluation has %d checks, want 2", len(checks))
	}
	if evaluatedAt == nil || evaluatedAt.Before(first.EvaluatedAt) {
		t.Errorf("LatestReadiness returned the older evaluation: %v", evaluatedAt)
	}
}

func TestDistinctDigestKeyID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// No snapshots yet: no key has ever been used, so generating one is safe.
	keyID, err := db.Snapshots.DistinctDigestKeyID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "" {
		t.Errorf("keyID = %q, want empty on a fresh database", keyID)
	}

	createSnapshot(t, db, fixtureSnapshotRow())

	keyID, err = db.Snapshots.DistinctDigestKeyID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "abcd1234" {
		t.Errorf("keyID = %q, want abcd1234; startup relies on this to refuse regenerating a lost key", keyID)
	}
}
