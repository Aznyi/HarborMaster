package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// --------------------------------------------------------------- fixtures --

type containerOpt func(*domain.ContainerDetail)

// buildContainer produces a realistic detail record. Options keep the tests
// readable: each one states only the thing it is actually about.
func buildContainer(id, name string, opts ...containerOpt) domain.ContainerDetail {
	detail := domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			HostID:        domain.LocalHostID,
			ID:            id,
			ShortID:       domain.ShortenID(id),
			Name:          name,
			Image:         domain.ParseImageRef("nginx:1.27"),
			ImageID:       "sha256:image1",
			State:         domain.StateRunning,
			Status:        "Up 2 hours",
			Health:        domain.HealthHealthy,
			CreatedAt:     time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
			Present:       true,
			Ports: []domain.Port{
				{ContainerPort: 80, Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8080, Published: true},
			},
		},
		State:       domain.StateDetail{State: domain.StateRunning, RawState: "running", Health: domain.HealthHealthy},
		Process:     domain.Process{User: "1000:1000", WorkingDir: "/app"},
		Environment: []domain.EnvVar{},
		Labels:      []domain.Label{},
		Mounts:      []domain.Mount{},
		Networks:    []domain.NetworkAttachment{},
		Warnings:    []domain.InventoryWarning{},
	}

	for _, opt := range opts {
		opt(&detail)
	}
	return detail
}

func withState(state domain.ContainerState) containerOpt {
	return func(d *domain.ContainerDetail) {
		d.Overview.State = state
		d.State.State = state
	}
}

func withHealth(health domain.HealthState) containerOpt {
	return func(d *domain.ContainerDetail) {
		d.Overview.Health = health
		d.State.Health = health
	}
}

func withCompose(project, service string) containerOpt {
	return func(d *domain.ContainerDetail) {
		d.Overview.Compose = domain.ComposeMetadata{Managed: true, Project: project, Service: service}
		d.Compose = d.Overview.Compose
	}
}

func withImage(ref, imageID string) containerOpt {
	return func(d *domain.ContainerDetail) {
		d.Overview.Image = domain.ParseImageRef(ref)
		d.Overview.ImageID = imageID
	}
}

func withLabels(labels map[string]string) containerOpt {
	return func(d *domain.ContainerDetail) {
		for key, value := range labels {
			d.Labels = append(d.Labels, domain.Label{
				Key: key, Value: value, Source: domain.ClassifyLabel(key),
			})
		}
	}
}

func withEnvironment(vars ...domain.EnvVar) containerOpt {
	return func(d *domain.ContainerDetail) { d.Environment = append(d.Environment, vars...) }
}

func withNetwork(name, ipv4 string) containerOpt {
	return func(d *domain.ContainerDetail) {
		d.Networks = append(d.Networks, domain.NetworkAttachment{
			NetworkName: name, NetworkID: "net-" + name, IPv4Address: ipv4,
			Aliases: []string{name + "-alias"},
		})
	}
}

func withMount(destination, source string, readOnly bool) containerOpt {
	return func(d *domain.ContainerDetail) {
		d.Mounts = append(d.Mounts, domain.Mount{
			Type: domain.MountTypeBind, Source: source,
			Destination: destination, ReadOnly: readOnly,
		})
	}
}

func records(details ...domain.ContainerDetail) []store.ContainerRecord {
	out := make([]store.ContainerRecord, 0, len(details))
	for _, detail := range details {
		out = append(out, store.ContainerRecord{
			Detail:  detail,
			RawJSON: []byte(`{"Id":"` + detail.Overview.ID + `","Config":{"Env":["SAFE=value"]}}`),
		})
	}
	return out
}

func commitOf(t *testing.T, db *store.DB, containers []store.ContainerRecord, opts ...func(*store.RefreshCommit)) domain.RefreshRecord {
	t.Helper()

	commit := store.RefreshCommit{
		Host:       domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: containers,
		Images: []domain.Image{
			{ID: "sha256:image1", ShortID: "image1", RepoTags: []string{"nginx:1.27"}, Size: 100},
		},
		Networks: []domain.Network{{ID: "net-bridge", Name: "bridge", Driver: "bridge"}},
		Volumes:  []domain.Volume{{Name: "data", Driver: "local"}},
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        time.Now().UTC(),
			ContainersListed: len(containers),
			Checksum:         "checksum-" + fmt.Sprint(len(containers)),
		},
		Now: time.Now().UTC(),
	}
	for _, opt := range opts {
		opt(&commit)
	}

	record, err := db.Inventory.CommitRefresh(context.Background(), commit)
	if err != nil {
		t.Fatalf("commit refresh: %v", err)
	}
	return record
}

// ------------------------------------------------------------- migrations --

func TestInventoryMigrationsCreateEveryTable(t *testing.T) {
	db := openTestDB(t)

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("migration names: %v", err)
	}
	if len(names) < 2 {
		t.Fatalf("expected at least two migrations, got %v", names)
	}
	// Applied in lexical order, which is why they are zero-padded.
	if names[0] >= names[1] {
		t.Errorf("migrations are not ordered: %v", names)
	}

	for _, table := range []string{
		"hosts", "inventory_refreshes", "images", "networks", "volumes",
		"containers", "container_config", "container_labels",
		"container_networks", "container_mounts", "inventory_warnings",
	} {
		var name string
		err := db.SQL().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db := openTestDB(t)

	var enabled int
	if err := db.SQL().QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if enabled != 1 {
		t.Fatal("foreign keys are not enabled")
	}

	// A child row for a container that does not exist must be rejected.
	_, err := db.SQL().Exec(
		`INSERT INTO container_labels (container_id, key, value) VALUES ('ghost', 'k', 'v')`)
	if err == nil {
		t.Error("expected the foreign key to reject an orphan label")
	}
}

// Child rows must disappear with their container, or a purge would leave
// orphans that later reads would surface as real data.
func TestDeletingAContainerCascades(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	detail := buildContainer("c1", "web",
		withLabels(map[string]string{"app": "web"}),
		withNetwork("bridge", "172.17.0.2"),
		withMount("/data", "/srv/data", false))
	commitOf(t, db, records(detail))

	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM containers WHERE id = 'c1'`); err != nil {
		t.Fatalf("delete container: %v", err)
	}

	for _, table := range []string{"container_config", "container_labels", "container_networks", "container_mounts"} {
		var count int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE container_id = 'c1'`).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s still holds %d orphaned rows", table, count)
		}
	}
}

// --------------------------------------------------------------- refresh --

func TestCommitRefreshAdvancesGeneration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := commitOf(t, db, records(buildContainer("c1", "web")))
	if first.Generation != 1 {
		t.Errorf("first generation = %d, want 1", first.Generation)
	}

	second := commitOf(t, db, records(buildContainer("c1", "web")))
	if second.Generation != 2 {
		t.Errorf("second generation = %d, want 2", second.Generation)
	}

	generation, checksum, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("current generation: %v", err)
	}
	if generation != 2 || checksum == "" {
		t.Errorf("current = (%d, %q)", generation, checksum)
	}
}

// Re-observing the same container updates one row; it does not accumulate.
func TestCommitRefreshUpsertsRatherThanDuplicating(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(buildContainer("c1", "web")))
	commitOf(t, db, records(buildContainer("c1", "web-renamed", withState(domain.StatePaused))))

	var count int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM containers`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("container rows = %d, want 1", count)
	}

	detail, err := db.Containers.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if detail.Overview.Name != "web-renamed" || detail.Overview.State != domain.StatePaused {
		t.Errorf("row not updated: %+v", detail.Overview)
	}
}

func TestCommitRefreshDoesNotDuplicateImagesNetworksOrVolumes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		commitOf(t, db, records(buildContainer("c1", "web")))
	}

	for _, table := range []string{"images", "networks", "volumes"} {
		var count int
		if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("%s rows = %d, want 1 after three refreshes", table, count)
		}
	}
}

// A container that disappears is retained and marked absent, so history and
// warnings survive it being removed out from under HarborMaster.
func TestContainersNoLongerPresentAreMarkedAbsent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(buildContainer("c1", "web"), buildContainer("c2", "db")))
	commitOf(t, db, records(buildContainer("c1", "web")))

	present, total, err := db.Containers.List(ctx, store.ContainerFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(present) != 1 || present[0].ID != "c1" {
		t.Errorf("present containers = %d (%+v)", total, present)
	}

	// Still retrievable when explicitly asked for.
	all, allTotal, err := db.Containers.List(ctx, store.ContainerFilter{IncludeAbsent: true})
	if err != nil {
		t.Fatalf("list including absent: %v", err)
	}
	if allTotal != 2 {
		t.Fatalf("total including absent = %d, want 2", allTotal)
	}

	var absent domain.ContainerSummary
	for _, summary := range all {
		if summary.ID == "c2" {
			absent = summary
		}
	}
	if absent.Present {
		t.Error("c2 should be marked absent")
	}
}

func TestAbsentContainersArePurgedAfterRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Seed a container far enough in the past that the retention window has
	// already passed by the second refresh.
	past := time.Now().UTC().Add(-48 * time.Hour)
	commitOf(t, db, records(buildContainer("old", "gone")), func(c *store.RefreshCommit) {
		c.Now = past
	})
	commitOf(t, db, records(buildContainer("c1", "web")), func(c *store.RefreshCommit) {
		c.AbsentRetention = time.Hour
	})

	var count int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM containers WHERE id = 'old'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Error("an absent container past its retention window should be purged")
	}
}

// The whole point of the single transaction: a failure leaves the previous
// inventory exactly as it was.
func TestFailedCommitRollsBackAndPreservesPriorInventory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(buildContainer("good", "keeper")))

	// An invalid state violates the CHECK constraint mid-transaction.
	broken := buildContainer("bad", "breaker")
	broken.Overview.State = domain.ContainerState("not-a-state")

	_, err := db.Inventory.CommitRefresh(ctx, store.RefreshCommit{
		Host:       domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: records(buildContainer("also-good", "second"), broken),
		Record:     domain.RefreshRecord{Trigger: domain.TriggerManual, StartedAt: time.Now().UTC()},
		Now:        time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected the constraint violation to fail the commit")
	}

	// Generation did not advance...
	generation, _, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("current generation: %v", err)
	}
	if generation != 1 {
		t.Errorf("generation = %d, want 1 (a failed refresh must not advance it)", generation)
	}

	// ...and nothing from the failed refresh was written.
	summaries, total, err := db.Containers.List(ctx, store.ContainerFilter{IncludeAbsent: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || summaries[0].ID != "good" {
		t.Errorf("prior inventory was disturbed: %+v", summaries)
	}
}

func TestRecordFailureDoesNotAdvanceGeneration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(buildContainer("c1", "web")))

	failed, err := db.Inventory.RecordFailure(ctx, domain.LocalHostID, domain.RefreshRecord{
		Trigger:   domain.TriggerPeriodic,
		StartedAt: time.Now().UTC(),
		Error:     "docker engine unreachable",
	})
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if failed.State != domain.RefreshFailed {
		t.Errorf("state = %q", failed.State)
	}

	generation, _, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("current generation: %v", err)
	}
	if generation != 1 {
		t.Errorf("generation = %d, want 1", generation)
	}

	// The failure is the newest attempt; the success is still the newest
	// success. Both are needed to render "last attempt / last success".
	attempt, err := db.Inventory.LastRefresh(ctx, false)
	if err != nil || attempt == nil || attempt.State != domain.RefreshFailed {
		t.Errorf("last attempt = %+v (%v)", attempt, err)
	}
	success, err := db.Inventory.LastRefresh(ctx, true)
	if err != nil || success == nil || success.State != domain.RefreshSucceeded {
		t.Errorf("last success = %+v (%v)", success, err)
	}
}

// A refresh can fail before a host row was ever written.
func TestRecordFailureOnAnEmptyDatabase(t *testing.T) {
	db := openTestDB(t)

	record, err := db.Inventory.RecordFailure(context.Background(), domain.LocalHostID,
		domain.RefreshRecord{Trigger: domain.TriggerStartup, StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("record failure with no host row: %v", err)
	}
	if record.Generation != 0 {
		t.Errorf("generation = %d, want 0 when nothing has succeeded yet", record.Generation)
	}
}

// ------------------------------------------------------------ persistence --

func TestContainerDetailRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	detail := buildContainer("c1", "web",
		withCompose("shop", "web"),
		withLabels(map[string]string{"app": "web", "com.docker.compose.project": "shop"}),
		withNetwork("frontend", "172.20.0.2"),
		withNetwork("backend", "172.21.0.2"),
		withMount("/data", "/srv/data", false),
		withMount("/etc/conf", "/srv/conf", true),
		withEnvironment(
			domain.EnvVar{Name: "SAFE", Value: "value", Sensitivity: domain.SensitivityNormal, RawValue: "value"},
			domain.EnvVar{Name: "DB_PASSWORD", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2"},
		))
	commitOf(t, db, records(detail))

	loaded, err := db.Containers.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if len(loaded.Networks) != 2 {
		t.Errorf("networks = %d", len(loaded.Networks))
	}
	if loaded.Networks[0].NetworkName != "backend" {
		t.Errorf("networks not ordered: %+v", loaded.Networks)
	}
	if len(loaded.Mounts) != 2 || !loaded.Mounts[1].ReadOnly {
		t.Errorf("mounts = %+v", loaded.Mounts)
	}
	if len(loaded.Labels) != 2 {
		t.Errorf("labels = %+v", loaded.Labels)
	}
	if len(loaded.Ports) != 1 || loaded.Ports[0].HostPort != 8080 {
		t.Errorf("ports = %+v", loaded.Ports)
	}
	if loaded.Process.User != "1000:1000" {
		t.Errorf("process not persisted: %+v", loaded.Process)
	}
}

// The masking design's storage guarantee: raw secret values never reach disk.
func TestRawEnvironmentValuesAreNotPersisted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	detail := buildContainer("c1", "web", withEnvironment(
		domain.EnvVar{
			Name: "DB_PASSWORD", Value: domain.MaskedValue,
			Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2",
		},
	))
	commitOf(t, db, records(detail))

	// Scan the entire config column, not just the decoded struct: the
	// guarantee is about bytes on disk.
	var stored string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT config_json FROM container_config WHERE container_id = 'c1'`).Scan(&stored); err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(stored, "hunter2") {
		t.Fatal("a raw secret value was written to the database")
	}
	if !strings.Contains(stored, "DB_PASSWORD") {
		t.Error("the variable name should still be recorded")
	}

	loaded, err := db.Containers.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Environment[0].Value != domain.MaskedValue {
		t.Errorf("value = %q, want masked", loaded.Environment[0].Value)
	}
	if loaded.Environment[0].RawValue != "" {
		t.Error("raw value must not survive a round trip through storage")
	}
}

func TestRawInspectionIsStoredSeparately(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(buildContainer("c1", "web")))

	raw, err := db.Containers.RawInspection(ctx, "c1")
	if err != nil {
		t.Fatalf("raw inspection: %v", err)
	}
	if !strings.Contains(string(raw), "SAFE=value") {
		t.Errorf("raw payload = %s", raw)
	}

	// The default detail response must not carry it.
	detail, err := db.Containers.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rendered := fmt.Sprintf("%+v", detail)
	if strings.Contains(rendered, "SAFE=value") {
		t.Error("raw inspection leaked into the normalized detail")
	}
}

// ------------------------------------------------- filtering and ordering --

func seedForQueries(t *testing.T, db *store.DB) {
	t.Helper()

	commitOf(t, db, records(
		buildContainer("id-alpha", "alpha", withState(domain.StateRunning), withHealth(domain.HealthHealthy),
			withCompose("shop", "web"), withImage("nginx:1.27", "sha256:image1"),
			withLabels(map[string]string{"tier": "front"})),
		buildContainer("id-bravo", "bravo", withState(domain.StateExited), withHealth(domain.HealthNone),
			withCompose("shop", "worker"), withImage("redis:7", "sha256:image2"),
			withLabels(map[string]string{"tier": "back"})),
		buildContainer("id-charlie", "charlie", withState(domain.StateRunning), withHealth(domain.HealthUnhealthy),
			withCompose("blog", "web"), withImage("nginx:1.27", "sha256:image1")),
		buildContainer("id-delta", "delta", withState(domain.StatePaused), withHealth(domain.HealthNone),
			withImage("postgres:16", "sha256:image3")),
	))
}

func TestContainerFiltering(t *testing.T) {
	db := openTestDB(t)
	seedForQueries(t, db)
	ctx := context.Background()

	tests := map[string]struct {
		filter store.ContainerFilter
		want   []string
	}{
		"no filter":     {store.ContainerFilter{}, []string{"alpha", "bravo", "charlie", "delta"}},
		"state running": {store.ContainerFilter{States: []domain.ContainerState{domain.StateRunning}}, []string{"alpha", "charlie"}},
		"two states": {store.ContainerFilter{States: []domain.ContainerState{domain.StateExited, domain.StatePaused}},
			[]string{"bravo", "delta"}},
		"unhealthy":       {store.ContainerFilter{Health: []domain.HealthState{domain.HealthUnhealthy}}, []string{"charlie"}},
		"compose project": {store.ContainerFilter{ComposeProject: "shop"}, []string{"alpha", "bravo"}},
		"compose service": {store.ContainerFilter{ComposeService: "web"}, []string{"alpha", "charlie"}},
		"image":           {store.ContainerFilter{Image: "nginx"}, []string{"alpha", "charlie"}},
		"search by name":  {store.ContainerFilter{Search: "brav"}, []string{"bravo"}},
		"search by image": {store.ContainerFilter{Search: "postgres"}, []string{"delta"}},
		"label key":       {store.ContainerFilter{LabelKey: "tier"}, []string{"alpha", "bravo"}},
		"label key/value": {store.ContainerFilter{LabelKey: "tier", LabelValue: "back"}, []string{"bravo"}},
		"restart policy":  {store.ContainerFilter{RestartPolicy: "unless-stopped"}, []string{"alpha", "bravo", "charlie", "delta"}},
		"no match":        {store.ContainerFilter{ComposeProject: "nonexistent"}, []string{}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			summaries, total, err := db.Containers.List(ctx, tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if total != len(tc.want) {
				t.Errorf("total = %d, want %d", total, len(tc.want))
			}

			got := make([]string, 0, len(summaries))
			for _, summary := range summaries {
				got = append(got, summary.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("names = %v, want %v", got, tc.want)
			}
		})
	}
}

// A search for "%" must match a literal percent sign, not everything.
func TestSearchEscapesLikeWildcards(t *testing.T) {
	db := openTestDB(t)
	seedForQueries(t, db)

	_, total, err := db.Containers.List(context.Background(), store.ContainerFilter{Search: "%"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 {
		t.Errorf("a literal %% matched %d containers; LIKE wildcards are not escaped", total)
	}
}

func TestContainerSorting(t *testing.T) {
	db := openTestDB(t)
	seedForQueries(t, db)
	ctx := context.Background()

	ascending, _, err := db.Containers.List(ctx, store.ContainerFilter{Sort: "name", Direction: store.SortAsc})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if ascending[0].Name != "alpha" || ascending[3].Name != "delta" {
		t.Errorf("ascending = %+v", names(ascending))
	}

	descending, _, err := db.Containers.List(ctx, store.ContainerFilter{Sort: "name", Direction: store.SortDesc})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if descending[0].Name != "delta" {
		t.Errorf("descending = %+v", names(descending))
	}

	// Every allowlisted field must actually work, not just parse.
	for _, field := range store.SortFields() {
		if _, _, err := db.Containers.List(ctx, store.ContainerFilter{Sort: field}); err != nil {
			t.Errorf("sort by %q failed: %v", field, err)
		}
	}
}

// An unknown sort field must fall back to the default, never reach SQL.
func TestUnknownSortFieldFallsBackSafely(t *testing.T) {
	db := openTestDB(t)
	seedForQueries(t, db)

	injection := "name; DROP TABLE containers;--"
	if store.ValidSortField(injection) {
		t.Fatal("the allowlist accepted an injection attempt")
	}

	summaries, _, err := db.Containers.List(context.Background(), store.ContainerFilter{Sort: injection})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(summaries) != 4 || summaries[0].Name != "alpha" {
		t.Errorf("expected the default ordering, got %v", names(summaries))
	}

	// And the table is still there.
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM containers`).Scan(&count); err != nil {
		t.Fatalf("table missing: %v", err)
	}
}

// Rows with equal sort keys must still page deterministically, or paging can
// repeat or skip a container.
func TestSortingIsDeterministicForTiedKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	tied := make([]domain.ContainerDetail, 0, 10)
	for i := 0; i < 10; i++ {
		tied = append(tied, buildContainer(fmt.Sprintf("id-%02d", i), "same-name"))
	}
	commitOf(t, db, records(tied...))

	var first []string
	for attempt := 0; attempt < 5; attempt++ {
		summaries, _, err := db.Containers.List(ctx, store.ContainerFilter{Sort: "name"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		ids := make([]string, 0, len(summaries))
		for _, summary := range summaries {
			ids = append(ids, summary.ID)
		}
		if first == nil {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("ordering changed between identical queries:\n%v\n%v", first, ids)
		}
	}
}

func TestPagination(t *testing.T) {
	db := openTestDB(t)
	seedForQueries(t, db)
	ctx := context.Background()

	page1, total, err := db.Containers.List(ctx, store.ContainerFilter{
		Sort: "name", Page: store.Page{Limit: 2, Offset: 0},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want the unpaged count of 4", total)
	}
	if len(page1) != 2 || page1[0].Name != "alpha" {
		t.Errorf("page 1 = %v", names(page1))
	}

	page2, _, err := db.Containers.List(ctx, store.ContainerFilter{
		Sort: "name", Page: store.Page{Limit: 2, Offset: 2},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page2) != 2 || page2[0].Name != "charlie" {
		t.Errorf("page 2 = %v", names(page2))
	}

	// Pages must not overlap.
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Errorf("container %s appears on both pages", a.ID)
			}
		}
	}
}

func TestResolveContainerID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(
		buildContainer("aaaa1111", "first"),
		buildContainer("aaaa2222", "second"),
		buildContainer("bbbb3333", "third"),
	))

	if id, err := db.Containers.ResolveID(ctx, "aaaa1111"); err != nil || id != "aaaa1111" {
		t.Errorf("exact id = (%q, %v)", id, err)
	}
	if id, err := db.Containers.ResolveID(ctx, "third"); err != nil || id != "bbbb3333" {
		t.Errorf("by name = (%q, %v)", id, err)
	}
	if id, err := db.Containers.ResolveID(ctx, "bbbb"); err != nil || id != "bbbb3333" {
		t.Errorf("unambiguous prefix = (%q, %v)", id, err)
	}

	// An ambiguous prefix must not resolve to an arbitrary one of the matches.
	if _, err := db.Containers.ResolveID(ctx, "aaaa"); !errors.Is(err, store.ErrAmbiguousID) {
		t.Errorf("ambiguous prefix error = %v, want ErrAmbiguousID", err)
	}
	if _, err := db.Containers.ResolveID(ctx, "zzzz"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown prefix error = %v, want ErrNotFound", err)
	}
	if _, err := db.Containers.ResolveID(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("empty reference error = %v", err)
	}
}

// A full ID that happens to prefix another must resolve to itself.
func TestExactIDBeatsPrefixAmbiguity(t *testing.T) {
	db := openTestDB(t)

	commitOf(t, db, records(buildContainer("abc", "short"), buildContainer("abcdef", "long")))

	id, err := db.Containers.ResolveID(context.Background(), "abc")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "abc" {
		t.Errorf("id = %q, want the exact match", id)
	}
}

func TestCountsAndWarnings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(
		buildContainer("c1", "a", withState(domain.StateRunning), withHealth(domain.HealthHealthy)),
		buildContainer("c2", "b", withState(domain.StateExited), withHealth(domain.HealthNone)),
		buildContainer("c3", "c", withState(domain.StateDead), withHealth(domain.HealthNone)),
		buildContainer("c4", "d", withState(domain.StatePaused), withHealth(domain.HealthUnhealthy)),
	), func(c *store.RefreshCommit) {
		c.Warnings = []domain.InventoryWarning{{
			ContainerID: "c1", ContainerName: "a",
			Code: domain.WarningInspectFailed, Message: "could not inspect",
			OccurredAt: time.Now().UTC(),
		}}
	})

	counts, err := db.Inventory.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Containers != 4 {
		t.Errorf("containers = %d", counts.Containers)
	}
	if counts.Running != 1 || counts.Paused != 1 {
		t.Errorf("running/paused = %d/%d", counts.Running, counts.Paused)
	}
	// "Stopped" means exited or dead, which is what an operator means by it.
	if counts.Stopped != 2 {
		t.Errorf("stopped = %d, want exited + dead = 2", counts.Stopped)
	}
	if counts.Healthy != 1 || counts.Unhealthy != 1 {
		t.Errorf("health counts = %d/%d", counts.Healthy, counts.Unhealthy)
	}
	if counts.ByState[domain.StateDead] != 1 {
		t.Errorf("byState = %+v", counts.ByState)
	}
	if counts.Images != 1 || counts.Networks != 1 || counts.Volumes != 1 {
		t.Errorf("catalog counts = %+v", counts)
	}
	if counts.Warnings != 1 {
		t.Errorf("warnings = %d", counts.Warnings)
	}

	warnings, err := db.Inventory.WarningsForContainer(ctx, "c1")
	if err != nil || len(warnings) != 1 {
		t.Errorf("container warnings = %+v (%v)", warnings, err)
	}
}

func TestImageAndCatalogRepositories(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(
		buildContainer("c1", "a", withImage("nginx:1.27", "sha256:image1")),
		buildContainer("c2", "b", withImage("nginx:1.27", "sha256:image1")),
	))

	usages, total, err := db.Images.List(ctx, store.Page{})
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if total != 1 {
		t.Fatalf("images = %d", total)
	}
	if usages[0].ContainerCount != 2 {
		t.Errorf("container count = %d, want 2", usages[0].ContainerCount)
	}

	single, err := db.Images.Get(ctx, "sha256:image1")
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	if single.Image.RepoTags[0] != "nginx:1.27" {
		t.Errorf("repo tags = %v", single.Image.RepoTags)
	}

	if _, err := db.Images.Get(ctx, "sha256:missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing image error = %v", err)
	}

	networks, netTotal, err := db.Networks.List(ctx, store.Page{})
	if err != nil || netTotal != 1 || networks[0].Name != "bridge" {
		t.Errorf("networks = %+v (%v)", networks, err)
	}
	volumes, volTotal, err := db.Volumes.List(ctx, store.Page{})
	if err != nil || volTotal != 1 || volumes[0].Name != "data" {
		t.Errorf("volumes = %+v (%v)", volumes, err)
	}
}

func TestDistinctFilterValues(t *testing.T) {
	db := openTestDB(t)
	seedForQueries(t, db)
	ctx := context.Background()

	projects, err := db.Containers.DistinctComposeProjects(ctx)
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if strings.Join(projects, ",") != "blog,shop" {
		t.Errorf("projects = %v, want sorted and de-duplicated", projects)
	}

	images, err := db.Containers.DistinctImages(ctx)
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	if len(images) != 3 {
		t.Errorf("images = %v", images)
	}
}

// ---------------------------------------------------------- scale --

// A thousand containers is the stated design target. This checks that
// persistence and paged queries stay practical at that size, and that the
// query cost does not grow with the result set.
func TestLargeInventoryRemainsPractical(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the large inventory test in short mode")
	}

	db := openTestDB(t)
	ctx := context.Background()

	const count = 1000
	details := make([]domain.ContainerDetail, 0, count)
	for i := 0; i < count; i++ {
		state := domain.StateRunning
		if i%3 == 0 {
			state = domain.StateExited
		}
		details = append(details, buildContainer(
			fmt.Sprintf("container-%04d", i),
			fmt.Sprintf("service-%04d", i),
			withState(state),
			withCompose(fmt.Sprintf("project-%02d", i%20), fmt.Sprintf("svc-%02d", i%7)),
			withImage(fmt.Sprintf("registry.example.com/app:%d", i%50), fmt.Sprintf("sha256:image%02d", i%50)),
			withLabels(map[string]string{"tier": fmt.Sprintf("t%d", i%5)}),
			withNetwork("bridge", fmt.Sprintf("172.17.%d.%d", i/256, i%256)),
			withMount("/data", fmt.Sprintf("/srv/%d", i), false),
			withEnvironment(
				domain.EnvVar{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
				domain.EnvVar{Name: "DB_PASSWORD", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "secret"},
			),
		))
	}

	images := make([]domain.Image, 0, 50)
	for i := 0; i < 50; i++ {
		images = append(images, domain.Image{
			ID:      fmt.Sprintf("sha256:image%02d", i),
			ShortID: fmt.Sprintf("image%02d", i),
			Size:    int64(i) * 1024,
		})
	}

	start := time.Now()
	commitOf(t, db, records(details...), func(c *store.RefreshCommit) { c.Images = images })
	commitDuration := time.Since(start)
	t.Logf("persisted %d containers in %s", count, commitDuration)

	// Generous: this is a guard against an accidental O(n^2), not a benchmark.
	if commitDuration > 90*time.Second {
		t.Errorf("persisting %d containers took %s, which suggests a scaling problem", count, commitDuration)
	}

	start = time.Now()
	page, total, err := db.Containers.List(ctx, store.ContainerFilter{
		Sort: "name", Page: store.Page{Limit: 50, Offset: 500},
	})
	pageDuration := time.Since(start)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	t.Logf("paged query over %d containers in %s", total, pageDuration)

	if total != count {
		t.Errorf("total = %d, want %d", total, count)
	}
	if len(page) != 50 {
		t.Errorf("page size = %d, want 50", len(page))
	}
	if pageDuration > 5*time.Second {
		t.Errorf("a paged query took %s, which suggests a missing index", pageDuration)
	}

	// Filtered queries must stay bounded too.
	start = time.Now()
	filtered, filteredTotal, err := db.Containers.List(ctx, store.ContainerFilter{
		States:         []domain.ContainerState{domain.StateRunning},
		ComposeProject: "project-03",
		Page:           store.Page{Limit: 25},
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	t.Logf("filtered query returned %d of %d in %s", len(filtered), filteredTotal, time.Since(start))
	if filteredTotal == 0 {
		t.Error("expected the filtered query to match something")
	}

	counts, err := db.Inventory.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Containers != count {
		t.Errorf("counts.Containers = %d, want %d", counts.Containers, count)
	}

	// A single detail fetch must not degrade with inventory size.
	start = time.Now()
	if _, err := db.Containers.Get(ctx, "container-0500"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a single container fetch took %s", elapsed)
	}
}

func names(summaries []domain.ContainerSummary) []string {
	out := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, summary.Name)
	}
	return out
}
