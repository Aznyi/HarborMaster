package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

const (
	depProviderID  = "07d62ee08974aceac5ff1fe8a366e9187da8270855c3c5d6d02abbec6ae56c0e"
	depDependentID = "eb68ac597e61f1fae5928f1d0ea94cabcbde0c2585496a0009e5ced064291ae3"
)

// namespaceModes sets a container's three namespace declarations.
func namespaceModes(network, ipc, pid string) containerOpt {
	return func(detail *domain.ContainerDetail) {
		detail.Security.NetworkMode = network
		detail.Security.IPCMode = ipc
		detail.Security.PIDMode = pid
	}
}

// An inventory refresh writes the namespace projection, and reading it back
// gives discovery exactly what it needs.
func TestRefreshWritesTheNamespaceProjection(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(
		buildContainer(depProviderID, "gluetun", namespaceModes("bridge", "private", "")),
		buildContainer(depDependentID, "sonarr",
			namespaceModes("container:"+depProviderID, "container:"+depProviderID, "")),
	))

	rows, err := db.Dependencies.NamespaceRows(ctx)
	if err != nil {
		t.Fatalf("namespace rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	byName := make(map[string]domain.ContainerNamespaceRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
		// The refresh inspected the container, which is the whole of what the
		// flag asserts.
		if !row.Modes.Observed {
			t.Errorf("%s came back unobserved after a refresh", row.Name)
		}
	}

	if got := byName["sonarr"].Modes.Network; got != "container:"+depProviderID {
		t.Fatalf("network mode = %q", got)
	}
	if got := byName["gluetun"].Modes.Network; got != "bridge" {
		t.Fatalf("provider network mode = %q", got)
	}

	// End to end: the projection feeds discovery, which produces the edge.
	edges, problems := domain.DiscoverDependencies(rows)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %v, want two (network and IPC)", edges)
	}
	for _, e := range edges {
		if e.Dependent != "sonarr" || e.Dependency != "gluetun" {
			t.Fatalf("edge = %s -> %s", e.Dependent, e.Dependency)
		}
	}
}

// The migration-safety property, tested directly.
//
// A row that predates migration 0024 must come back UNOBSERVED, so discovery
// blocks it rather than reading its empty network_mode as "shares nothing".
// This is the difference between a safe upgrade and an estate that looks
// entirely independent for one refresh interval.
func TestPreMigrationRowsComeBackUnobserved(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	// A refresh establishes the host and one modern row, so the assertion
	// below is that the flag is PER ROW rather than a property of the estate.
	commitOf(t, db, records(
		buildContainer(depDependentID, "modern", namespaceModes("bridge", "", "")),
	))

	// Inserted WITHOUT the namespace columns, exactly as a row written by any
	// build before 0024 looks once the ALTER TABLE applies its defaults.
	_, err := db.SQL().ExecContext(ctx, `
		INSERT INTO containers
			(id, host_id, short_id, name, image_ref, state, created_at,
			 present, first_seen_at, last_seen_at, generation, warning_count)
		VALUES (?, ?, 'old', 'legacy', 'alpine:3.22', 'running',
		        '2026-08-01T00:00:00Z', 1,
		        '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', 1, 0)`,
		depProviderID, domain.LocalHostID)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	rows, err := db.Dependencies.NamespaceRows(ctx)
	if err != nil {
		t.Fatalf("namespace rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	byName := make(map[string]domain.ContainerNamespaceRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	if byName["legacy"].Modes.Observed {
		t.Fatal("a pre-migration row must not report its namespaces as observed")
	}
	if !byName["modern"].Modes.Observed {
		t.Fatal("a refreshed row must report its namespaces as observed")
	}

	// And the consequence: discovery refuses the legacy row rather than
	// reading its empty network_mode as "shares nothing", while the refreshed
	// one is cleared normally.
	edges, problems := domain.DiscoverDependencies(rows)
	if len(edges) != 0 {
		t.Fatalf("edges = %v, want none", edges)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
	if problems[0].Container != "legacy" ||
		problems[0].Refusal != domain.DiscoveryUnobserved {
		t.Fatalf("problem = %+v, want legacy / namespacesUnobserved", problems[0])
	}
}

// Operator relationships round-trip, and carry their attribution.
func TestOperatorDependencyRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	created, err := db.Dependencies.Create(ctx, domain.WorkloadDependency{
		Dependent:  "api",
		Dependency: "postgres",
		Source:     domain.DependencyOperator,
		CreatedBy:  domain.Requester{UserID: "usr_1", Username: "ada"},
	}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !domain.ValidDependencyID(created.DependencyID) {
		t.Fatalf("generated id %q does not validate", created.DependencyID)
	}

	fetched, err := db.Dependencies.Get(ctx, created.DependencyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.Dependent != "api" || fetched.Dependency != "postgres" {
		t.Fatalf("fetched %s -> %s", fetched.Dependent, fetched.Dependency)
	}
	if fetched.Source != domain.DependencyOperator {
		t.Fatalf("source = %q", fetched.Source)
	}
	if fetched.CreatedBy.Username != "ada" || fetched.CreatedBy.UserID != "usr_1" {
		t.Fatalf("attribution = %+v", fetched.CreatedBy)
	}
	if !fetched.CreatedAt.Equal(now) {
		t.Fatalf("createdAt = %v, want %v", fetched.CreatedAt, now)
	}

	listed, err := db.Dependencies.OperatorDependencies(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d, want 1", len(listed))
	}

	count, err := db.Dependencies.OperatorDependencyCount(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	if err := db.Dependencies.Delete(ctx, created.DependencyID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Dependencies.Get(ctx, created.DependencyID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

// The unique index is what makes a duplicate a refusal rather than a second
// row expressing one constraint.
func TestDuplicateOperatorDependencyIsRefused(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	edge := domain.WorkloadDependency{
		Dependent: "api", Dependency: "postgres", Source: domain.DependencyOperator,
	}
	if _, err := db.Dependencies.Create(ctx, edge, now); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := db.Dependencies.Create(ctx, edge, now); !errors.Is(err, store.ErrDependencyExists) {
		t.Fatalf("second create = %v, want ErrDependencyExists", err)
	}
}

// A discovered source may not be stored, refused by the repository AND by the
// schema. Two independent refusals, because a row asserting a runtime
// requirement the daemon does not enforce would make HarborMaster refuse an
// update for a reason that is not true.
func TestDiscoveredSourcesCannotBeStored(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	for _, source := range domain.DiscoveredDependencySources {
		_, err := db.Dependencies.Create(ctx, domain.WorkloadDependency{
			Dependent: "sonarr", Dependency: "gluetun", Source: source,
		}, time.Now().UTC())
		if err == nil {
			t.Fatalf("%q was accepted for storage", source)
		}
	}

	// The schema refuses it too, with the repository bypassed entirely.
	_, err := db.SQL().ExecContext(ctx, `
		INSERT INTO workload_dependencies
			(dependency_id, dependent_name, dependency_name, source, created_at)
		VALUES ('dep_0000000000000000000a', 'sonarr', 'gluetun',
		        'dockerNetworkNamespace', '2026-08-12T00:00:00Z')`)
	if err == nil {
		t.Fatal("the schema accepted a discovered source")
	}
}

// The smallest possible cycle is impossible at the schema level.
func TestSchemaRefusesASelfDependency(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.SQL().ExecContext(ctx, `
		INSERT INTO workload_dependencies
			(dependency_id, dependent_name, dependency_name, source, created_at)
		VALUES ('dep_0000000000000000000b', 'api', 'api', 'operator',
		        '2026-08-12T00:00:00Z')`)
	if err == nil {
		t.Fatal("the schema accepted a self-dependency")
	}
}

// Ids are shape-validated before they reach a query, so a caller cannot probe
// the table with arbitrary strings.
func TestMalformedIDsAreNotFound(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	for _, bad := range []string{
		"", "not-an-id", "dep_", "' OR 1=1 --", strings.Repeat("a", 300),
	} {
		if _, err := db.Dependencies.Get(ctx, bad); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("get(%q) = %v, want ErrNotFound", bad, err)
		}
		if err := db.Dependencies.Delete(ctx, bad); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

// Endpoints carry the facts validation needs, including the labels the
// self-identity match reads.
func TestEndpointsCarryLabelsAndPresence(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(
		buildContainer(depProviderID, "gluetun", withLabels(map[string]string{
			"io.harbormaster.enabled": "true",
		})),
		buildContainer(depDependentID, "sonarr"),
	))

	endpoints, err := db.Dependencies.Endpoints(ctx)
	if err != nil {
		t.Fatalf("endpoints: %v", err)
	}
	index := domain.EndpointsFromNames(endpoints)
	if len(index) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(index))
	}
	gluetun, ok := index["gluetun"]
	if !ok {
		t.Fatal("gluetun is missing from the endpoint index")
	}
	if !gluetun.Present {
		t.Fatal("a container the refresh saw is not marked present")
	}
	if gluetun.Labels["io.harbormaster.enabled"] != "true" {
		t.Fatalf("labels = %v", gluetun.Labels)
	}
	if gluetun.ImageRef == "" {
		t.Fatal("the endpoint carries no image reference for the self match")
	}
}

// A parked original is recognised from its derived name, so an operator cannot
// build ordering around HarborMaster's own evidence.
func TestDerivedNamesAreMarkedOnEndpoints(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	parked := "api" + domain.ParkedNameSuffix + "exec_0123456789abcdef0123"
	commitOf(t, db, records(buildContainer(depProviderID, parked)))

	endpoints, err := db.Dependencies.Endpoints(ctx)
	if err != nil {
		t.Fatalf("endpoints: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(endpoints))
	}
	if !endpoints[0].Derived {
		t.Fatalf("%q was not recognised as a container HarborMaster derived", parked)
	}
}
