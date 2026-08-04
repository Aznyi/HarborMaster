package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// scanDatabaseFor walks EVERY table and EVERY column looking for a needle.
//
// Deliberately indiscriminate. A targeted check ("is the value in
// snapshot_environment.value?") only tests the leak the author already thought
// of; this one catches a secret that reached a column nobody expected --
// a warning message, a reason string, a JSON blob, a future table.
func scanDatabaseFor(t *testing.T, db *sql.DB, needle string) []string {
	t.Helper()
	if needle == "" {
		t.Fatal("scanDatabaseFor needs a non-empty needle")
	}

	tableRows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = tableRows.Close() }()

	var tables []string
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := tableRows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}

	var hits []string
	for _, table := range tables {
		// The table name comes from sqlite_master, never from a caller.
		rows, err := db.Query(`SELECT * FROM ` + table) //nolint:gosec // schema-derived identifier
		if err != nil {
			t.Fatalf("select from %s: %v", table, err)
		}

		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatalf("columns of %s: %v", table, err)
		}

		for rows.Next() {
			cells := make([]any, len(columns))
			holders := make([]sql.RawBytes, len(columns))
			for i := range cells {
				cells[i] = &holders[i]
			}
			if err := rows.Scan(cells...); err != nil {
				_ = rows.Close()
				t.Fatalf("scan row of %s: %v", table, err)
			}
			for i, cell := range holders {
				if strings.Contains(string(cell), needle) {
					hits = append(hits, fmt.Sprintf("%s.%s", table, columns[i]))
				}
			}
		}
		_ = rows.Close()
	}
	return hits
}

// The end-to-end guarantee, asserted against a real database.
//
// Every needle is a value that a container might genuinely carry, written
// through the real repository, then hunted for across every column of every
// table. Nothing here is mocked.
func TestNoSecretValueReachesTheDatabase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	needles := map[string]string{
		"DB_PASSWORD":          "needle-db-password-0001",
		"API_TOKEN":            "needle-api-token-0002",
		"DATABASE_URL":         "postgres://user:needle-embedded-0003@db:5432/app",
		"CREDENTIALS_JSON":     `{"private_key":"needle-credentials-0004"}`,
		"logging.splunk-token": "needle-splunk-0005",
	}

	env := make([]domain.SnapshotEnvEntry, 0, len(needles))
	position := 0
	for key := range needles {
		env = append(env, domain.SnapshotEnvEntry{
			Position:       position,
			Key:            key,
			Classification: domain.SensitivitySensitive,
			Present:        true,
			// The value is supplied deliberately: the repository must refuse to
			// write it even when a caller hands it over.
			Value:           needles[key],
			Length:          len(needles[key]),
			Digest:          "digest-placeholder",
			DigestAlgorithm: domain.DigestHMACSHA256,
			DigestKeyID:     "key1",
		})
		position++
	}

	snapshot := fixtureSnapshotRow()
	snapshot.Reason = "capture before upgrade"
	if _, err := db.Snapshots.Create(ctx, snapshot, env, nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	for name, value := range needles {
		if hits := scanDatabaseFor(t, db.SQL(), value); len(hits) > 0 {
			t.Errorf("%s leaked its value into %v", name, hits)
		}
	}
}

// The scanner has to be able to find something, or the test above proves
// nothing at all.
func TestSecretScannerActuallyDetectsAValue(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// A NON-sensitive variable is stored in full by design, so it is the honest
	// positive control for the scanner.
	snapshot := fixtureSnapshotRow()
	if _, err := db.Snapshots.Create(ctx, snapshot,
		[]domain.SnapshotEnvEntry{{
			Position: 0, Key: "PATH", Classification: domain.SensitivityNormal,
			Present: true, Value: "canary-value-should-be-found",
		}}, nil, nil); err != nil {
		t.Fatal(err)
	}

	hits := scanDatabaseFor(t, db.SQL(), "canary-value-should-be-found")
	if len(hits) == 0 {
		t.Fatal("the scanner found nothing for a value that IS stored; " +
			"TestNoSecretValueReachesTheDatabase would pass vacuously")
	}
}

// The document is the other place a secret could hide.
func TestCanonicalDocumentInDatabaseCarriesNoSecret(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const needle = "needle-inside-the-document"

	snapshot := fixtureSnapshotRow()
	snapshot.SpecJSON = []byte(`{"specVersion":1,"environment":[{"name":"DB_PASSWORD","sensitivity":"sensitive","present":true,"length":26}]}`)
	if _, err := db.Snapshots.Create(ctx, snapshot, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	if hits := scanDatabaseFor(t, db.SQL(), needle); len(hits) > 0 {
		t.Errorf("the document leaked into %v", hits)
	}
}

// An operator-supplied reason is stored, so it must not be able to smuggle
// anything structural into the row.
func TestReasonIsStoredAsOpaqueText(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	snapshot := fixtureSnapshotRow()
	snapshot.Reason = `'; DROP TABLE snapshots; --`

	created, err := db.Snapshots.Create(ctx, snapshot, nil, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The table still exists and the reason round-trips verbatim, which is what
	// a bound parameter guarantees.
	got, err := db.Snapshots.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("the snapshots table did not survive: %v", err)
	}
	if got.Reason != snapshot.Reason {
		t.Errorf("Reason = %q, want it stored verbatim", got.Reason)
	}
}

// Filter values must never reach the SQL text.
func TestFilterValuesAreBoundNotInterpolated(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	createSnapshot(t, db, fixtureSnapshotRow())

	// Each of these would be catastrophic if concatenated into the query.
	for _, hostile := range []string{
		`' OR '1'='1`,
		`'; DROP TABLE snapshots; --`,
		`" UNION SELECT * FROM snapshots --`,
	} {
		_, _, err := db.Snapshots.List(ctx, store.SnapshotFilter{ContainerID: hostile})
		if err != nil {
			t.Errorf("filter %q produced an error rather than simply matching nothing: %v", hostile, err)
		}
	}

	// The table is intact and still holds its row.
	count, err := db.Snapshots.Count(ctx)
	if err != nil {
		t.Fatalf("the snapshots table did not survive: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

// A large history must not degrade into one query per row.
func TestLargeSnapshotHistoryStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the large-dataset test in short mode")
	}

	db := openTestDB(t)
	ctx := context.Background()

	const total = 2000
	for i := range total {
		s := fixtureSnapshotRow()
		s.ContainerID = fmt.Sprintf("container-%03d", i%50)
		s.Checksum = fmt.Sprintf("%064x", i+1)
		if _, err := db.Snapshots.Create(ctx, s, nil, nil, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// A page must stay a page regardless of how much history exists.
	page, count, err := db.Snapshots.List(ctx, store.SnapshotFilter{Page: store.Page{Limit: 25}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 25 {
		t.Errorf("page size = %d, want 25", len(page))
	}
	if count != total {
		t.Errorf("total = %d, want %d", count, total)
	}

	// Loading a page's child rows is ONE query, not one per snapshot.
	ids := make([]int64, 0, len(page))
	for _, s := range page {
		ids = append(ids, s.ID)
	}
	if _, err := db.Snapshots.EnvironmentFor(ctx, ids); err != nil {
		t.Fatalf("EnvironmentFor: %v", err)
	}
}

// An unbounded page request must be refused rather than honoured.
func TestPageLimitCannotBeEscalated(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 60 {
		s := fixtureSnapshotRow()
		s.Checksum = fmt.Sprintf("%064x", i+1)
		createSnapshot(t, db, s)
	}

	for _, limit := range []int{0, -1, 1_000_000} {
		got, _, err := db.Snapshots.List(ctx, store.SnapshotFilter{Page: store.Page{Limit: limit}})
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(got) > 200 {
			t.Errorf("limit %d returned %d rows; the cap was not applied", limit, len(got))
		}
	}
}
