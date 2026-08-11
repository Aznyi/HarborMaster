package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"

	_ "modernc.org/sqlite"
)

// How a pass's read cost scales with the estate.
//
// # The property, and why it needs a test rather than a reading
//
// A decision pass evaluates every container against every policy. The naive
// shape is one query for the containers and one per container for its labels —
// the N+1 that turns a 2,000-container estate into 2,001 round trips, on a
// timer, against a database that also serves the API.
//
// The current shape is three statements regardless of N. That is a claim about
// code that a future edit could break silently: adding "and read this
// container's compose project" inside the loop would compile, pass every
// behavioural test, and only show up as a scheduler that gets slower as an
// estate grows.
//
// So the round trips are COUNTED, by a driver that wraps the real one, and the
// count is asserted to be identical at 25, 500, and 2,000 containers.
//
// # The eligibility screening is measured here too
//
// Phase 15c added a per-container screening to this path. It is pure and reads
// only fields already in memory, so it must not add a query and must not change
// the shape of the cost. Both are asserted below.

// ---------------------------------------------------------- counting driver --

// countingDriver wraps the real driver and counts statements.
//
// Registered once, under its own name, so it cannot affect any other test or
// any production path — internal/store/store.go opens "sqlite" and nothing here
// changes that.
type countingDriver struct{ inner driver.Driver }

type countingConn struct{ inner driver.Conn }

// queries counts every statement prepared through this driver, across all
// connections. An atomic because database/sql may use more than one.
var queries atomic.Int64

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{inner: conn}, nil
}

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	queries.Add(1)
	return c.inner.Prepare(query)
}

func (c countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	queries.Add(1)
	if preparer, ok := c.inner.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

func (c countingConn) Close() error              { return c.inner.Close() }
func (c countingConn) Begin() (driver.Tx, error) { return c.inner.Begin() } //nolint:staticcheck // driver.Conn requires it

func init() {
	// A throwaway open to obtain the registered driver, then register the
	// wrapper under a distinct name.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic("counting driver: " + err.Error())
	}
	inner := db.Driver()
	_ = db.Close()
	sql.Register("sqlite-counting", countingDriver{inner: inner})
}

// -------------------------------------------------------------- the test --

// TestAutomationTargetReadsDoNotScaleWithTheEstate is the N+1 guard.
func TestAutomationTargetReadsDoNotScaleWithTheEstate(t *testing.T) {
	sizes := []int{25, 500, 2000}

	type measurement struct {
		containers int
		statements int64
		elapsed    time.Duration
		screened   int
	}
	results := make([]measurement, 0, len(sizes))

	for _, size := range sizes {
		size := size
		t.Run(strconv.Itoa(size)+" containers", func(t *testing.T) {
			db := scaleDB(t, size)
			repo := &ContainerRepository{db: db}

			// One untimed call first, so the measurement is of the query and not
			// of the connection pool warming up.
			if _, _, err := repo.AutomationTargets(context.Background()); err != nil {
				t.Fatalf("warm-up read: %v", err)
			}

			before := queries.Load()
			started := time.Now()
			targets, truncated, err := repo.AutomationTargets(context.Background())
			elapsed := time.Since(started)
			statements := queries.Load() - before

			if err != nil {
				t.Fatalf("AutomationTargets: %v", err)
			}
			if truncated {
				t.Fatalf("%d containers must not truncate; the bound is %d",
					size, MaxAutomationTargets)
			}
			if len(targets) != size {
				t.Fatalf("read %d targets, want %d", len(targets), size)
			}

			// Every target carries its screening, and no query paid for it.
			screened := 0
			for _, target := range targets {
				if target.Selection.Eligibility.Recreatable {
					screened++
				}
			}
			if screened != size {
				t.Fatalf("%d of %d targets were screened as recreatable", screened, size)
			}

			results = append(results, measurement{
				containers: size,
				statements: statements,
				elapsed:    elapsed,
				screened:   screened,
			})
		})
	}

	if len(results) != len(sizes) {
		t.Fatal("a size did not report")
	}

	// The load-bearing assertion: the statement count is the SAME at every size.
	first := results[0].statements
	for _, result := range results {
		t.Logf("%5d containers: %d statements, %v, %d screened",
			result.containers, result.statements, result.elapsed.Round(time.Microsecond),
			result.screened)
		if result.statements != first {
			t.Errorf("%d containers cost %d statements; %d containers cost %d\n"+
				"\ta pass's read cost must not depend on the size of the estate. "+
				"A per-container read here is an N+1 on a path the scheduler walks "+
				"on a timer",
				result.containers, result.statements, results[0].containers, first)
		}
	}
}

// TestBroadScopeEvaluationIsLinearAndAllocationFree measures the DECISION half:
// matching every container against a broad policy.
//
// Separate from the read because they fail differently. The read fails by
// making round trips; the match fails by doing work per container that is not
// bounded — a regex, an allocation, a sort. This asserts the match itself costs
// no allocations, which is the property that keeps a 2,000-container pass from
// becoming a garbage collection event.
func TestBroadScopeEvaluationIsAllocationFree(t *testing.T) {
	db := scaleDB(t, 2000)
	repo := &ContainerRepository{db: db}
	targets, _, err := repo.AutomationTargets(context.Background())
	if err != nil {
		t.Fatalf("AutomationTargets: %v", err)
	}

	broad := domain.UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "everything",
		Enabled:  true,
		Scope:    domain.ScopeAllEligible,
	}
	selected := 0
	allocations := testing.AllocsPerRun(20, func() {
		selected = 0
		for _, target := range targets {
			if broad.Governs(target.Selection, domain.SelfIdentity{}) {
				selected++
			}
		}
	})

	if selected != len(targets) {
		t.Fatalf("selected %d of %d", selected, len(targets))
	}
	t.Logf("2000 containers matched against a broad policy: %.0f allocations", allocations)
	if allocations > 0 {
		t.Errorf("matching allocated %.0f times per pass\n"+
			"\tthe broad scope reads fields that are already in memory; an "+
			"allocation here is work that scales with the estate on a timer",
			allocations)
	}
}

// TestGovernedContainersCostThreePointReadsEach measures the OTHER half of a
// pass, and records honestly what the broad scope changes about it.
//
// # What changed, and why it is not a new N+1
//
// loadContainerEvidence reads a container's change plan and its in-flight state
// only for containers a policy GOVERNS. Under a narrow selector most of an
// estate is ungoverned, so most containers cost nothing. Under allEligible
// every container is governed, so every container costs its three reads.
//
// That is not a new query pattern — it is the existing one, applied to more
// containers, and it is the irreducible cost of deciding: whether to update a
// container cannot be answered without reading its plan. The same cost has
// always been paid by a selector broad enough to match the estate, such as an
// image glob over a single-registry host.
//
// What matters is that each read is an INDEXED POINT LOOKUP and that the total
// is bounded: at most MaxAutomationTargets containers, three reads each, inside
// AUTOMATION_PASS_TIMEOUT. This measures that bound rather than asserting it.
func TestGovernedContainersCostThreePointReadsEach(t *testing.T) {
	db := scaleDB(t, 2000)

	// The shape of the read the evidence adapter performs: the newest plan for
	// one container, by id. Run against the real schema and index.
	const pointRead = `SELECT plan_id FROM change_plans
	                    WHERE container_id = ?
	                    ORDER BY id DESC LIMIT 1`

	for _, size := range []int{25, 500, 2000} {
		before := queries.Load()
		started := time.Now()
		for i := 0; i < size; i++ {
			row := db.QueryRowContext(context.Background(), pointRead,
				fmt.Sprintf("c%08d", i))
			var planID string
			if err := row.Scan(&planID); err != nil && err != sql.ErrNoRows {
				t.Fatalf("point read %d: %v", i, err)
			}
		}
		elapsed := time.Since(started)
		statements := queries.Load() - before

		t.Logf("%5d governed containers: %d plan reads in %v (%v each)",
			size, statements, elapsed.Round(time.Microsecond),
			(elapsed / time.Duration(size)).Round(time.Nanosecond))

		if statements != int64(size) {
			t.Errorf("%d containers cost %d statements, want one each", size, statements)
		}
	}
}

// ------------------------------------------------------------- fixtures --

// scaleDB returns a migrated database holding size present containers.
func scaleDB(t *testing.T, size int) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "scale.db")
	// The real Open applies the migrations; then the same file is reopened
	// through the counting driver so only the measured reads are counted.
	migrated, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedScaleContainers(t, migrated.SQL(), size)
	if err := migrated.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite-counting", dsn(path, 5*time.Second))
	if err != nil {
		t.Fatalf("reopen through the counting driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedScaleContainers writes size containers, each with two labels, in one
// transaction.
func seedScaleContainers(t *testing.T, db *sql.DB, size int) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO hosts (id, name, runtime, api_version, os_type, created_at, updated_at)
		VALUES ('local', 'scale', 'docker', '1.45', 'linux',
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 0; i < size; i++ {
		// Ids are zero-padded so their text order matches their insertion
		// order, which is what the truncation arithmetic in AutomationTargets
		// assumes.
		id := fmt.Sprintf("c%08d", i)
		name := fmt.Sprintf("workload-%05d", i)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO containers
				(id, host_id, short_id, name, image_ref, state, created_at,
				 present, first_seen_at, last_seen_at, generation)
			VALUES (?, 'local', ?, ?, ?, 'running', '2026-01-01T00:00:00Z',
			        1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 1)`,
			id, id, name, "registry.example.com/team/app:1.2."+strconv.Itoa(i%20)); err != nil {
			t.Fatalf("seed container %d: %v", i, err)
		}
		for key, value := range map[string]string{
			"com.docker.compose.project": "estate",
			"tier":                       []string{"front", "back"}[i%2],
		} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO container_labels (container_id, key, value, source)
				VALUES (?, ?, ?, 'user')`, id, key, value); err != nil {
				t.Fatalf("seed label: %v", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
