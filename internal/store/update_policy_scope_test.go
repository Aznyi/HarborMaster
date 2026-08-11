package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Persistence of the policy scope, and the screening a pass reads.
//
// Two properties, and the first is the one an upgrade depends on: a policy
// written before the scope existed must come out of the upgrade governing
// exactly what it governed before. The second is that the facts the broad scope
// requires are established by the repository, once, for the whole estate.

// ----------------------------------------------- backward compatibility --

// A row written without a scope reads back as the narrow one, and governs
// exactly what it always did.
//
// Written through raw SQL on purpose: the repository always supplies a scope
// now, so inserting through it would prove nothing about a row that predates
// the column. This is the shape a real upgraded deployment carries.
func TestUpgradePreservesUpdatePolicyBreadth(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// The column exists after the migration, so a "pre-scope" row is one that
	// simply does not name it and takes the default.
	_, err := db.SQL().ExecContext(ctx, `
		INSERT INTO update_policies
			(policy_id, name, enabled, priority, strategy, mode,
			 minimum_recommendation, selector_json, window_json, limits_json,
			 failure_json, created_at, updated_at)
		VALUES ('upd_00112233445566778899', 'written before scopes', 1, 10,
		        'patch', 'observe', 'proceed',
		        '{"include":["legacy-web"]}', '{"alwaysOpen":true}', '{}', '{}',
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert a pre-scope policy: %v", err)
	}

	policy, err := db.UpdatePolicies.UpdatePolicyByID(ctx, "upd_00112233445566778899")
	if err != nil {
		t.Fatalf("read the pre-scope policy: %v", err)
	}

	if policy.Scope != domain.ScopeSelector {
		t.Fatalf("scope %q, want %q\n"+
			"\ta policy stored before the column existed must come out narrow; any "+
			"other answer means the upgrade changed what somebody's rule governs",
			policy.Scope, domain.ScopeSelector)
	}

	// And behaviourally: it still names one container.
	named := screenedStoreTarget("legacy-web", "nginx:1.27")
	other := screenedStoreTarget("everything-else", "nginx:1.27")
	if !policy.Governs(named, domain.SelfIdentity{}) {
		t.Fatal("the upgraded policy no longer governs the container it named")
	}
	if policy.Governs(other, domain.SelfIdentity{}) {
		t.Fatal("the upgraded policy governs a container it never named")
	}
}

// An archived policy is untouched by the upgrade, scope included.
func TestUpgradeLeavesArchivedPoliciesAlone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.SQL().ExecContext(ctx, `
		INSERT INTO update_policies
			(policy_id, name, enabled, archived, archived_at, priority, strategy,
			 mode, minimum_recommendation, selector_json, window_json,
			 limits_json, failure_json, created_at, updated_at)
		VALUES ('upd_00112233445566778890', 'withdrawn', 0, 1, '2026-01-01T00:00:00Z',
		        0, 'patch', 'observe', 'proceed', '{"include":["gone"]}',
		        '{"alwaysOpen":true}', '{}', '{}',
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert an archived policy: %v", err)
	}

	policy, err := db.UpdatePolicies.UpdatePolicyByID(ctx, "upd_00112233445566778890")
	if err != nil {
		t.Fatalf("read the archived policy: %v", err)
	}
	if !policy.Archived || policy.Scope != domain.ScopeSelector {
		t.Fatalf("archived=%v scope=%q; a withdrawn policy must survive unchanged",
			policy.Archived, policy.Scope)
	}
	// And it governs nothing, as it did before.
	if policy.Governs(screenedStoreTarget("gone", "nginx:1.27"), domain.SelfIdentity{}) {
		t.Fatal("an archived policy must govern nothing")
	}
}

// ------------------------------------------------------------ round trip --

func TestBroadScopeRoundTrips(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "keep everything current",
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Selector:              domain.UpdateSelector{Exclude: []string{"database"}},
		Strategy:              domain.StrategyPatch,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  domain.ModeObserve,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()

	created := createUpdatePolicy(t, db, policy)
	if created.Scope != domain.ScopeAllEligible {
		t.Fatalf("create returned scope %q", created.Scope)
	}

	read, err := db.UpdatePolicies.UpdatePolicyByID(ctx, created.PolicyID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if read.Scope != domain.ScopeAllEligible {
		t.Fatalf("stored scope %q, want allEligible", read.Scope)
	}
	if len(read.Selector.Exclude) != 1 || read.Selector.Exclude[0] != "database" {
		t.Fatalf("the exclusion did not survive: %v", read.Selector.Exclude)
	}

	// The scheduler's own query returns it with the scope intact. A pass that
	// read the policy without its breadth would evaluate a different rule from
	// the one an administrator saved.
	active, err := db.UpdatePolicies.ActivePolicies(ctx)
	if err != nil {
		t.Fatalf("load active policies: %v", err)
	}
	found := false
	for _, candidate := range active {
		if candidate.PolicyID == created.PolicyID {
			found = true
			if candidate.Scope != domain.ScopeAllEligible {
				t.Fatalf("the scheduler reads scope %q", candidate.Scope)
			}
		}
	}
	if !found {
		t.Fatal("the policy is missing from the scheduler's query")
	}
}

// An edit that does not mention the scope must not change it. A partial edit
// is the most common way a breadth would move by accident.
func TestEditWithoutAScopeLeavesBreadthAlone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "broad",
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Strategy:              domain.StrategyPatch,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  domain.ModeObserve,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	created := createUpdatePolicy(t, db, policy)

	renamed := "renamed"
	updated, err := db.UpdatePolicies.ApplyUpdatePolicy(ctx, created.PolicyID,
		store.UpdatePolicyChange{Name: &renamed}, time.Now().UTC())
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.Scope != domain.ScopeAllEligible {
		t.Fatalf("scope moved to %q on a rename", updated.Scope)
	}
}

// ------------------------------------------------------ the schema check --

// The database refuses a scope the domain would refuse. Two layers, because a
// policy row is reachable by anything that can open the file.
func TestTheDatabaseRefusesAnUnknownScope(t *testing.T) {
	db := openTestDB(t)

	_, err := db.SQL().ExecContext(context.Background(), `
		INSERT INTO update_policies
			(policy_id, name, enabled, priority, scope, strategy, mode,
			 minimum_recommendation, selector_json, window_json, limits_json,
			 failure_json, created_at, updated_at)
		VALUES ('upd_00112233445566778891', 'bogus', 1, 0, 'everything',
		        'patch', 'observe', 'proceed', '{}', '{}', '{}', '{}',
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("the schema accepted a scope outside the vocabulary")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Fatalf("want a constraint violation, got %v", err)
	}
}

// ---------------------------------------------------------- screening --

// The repository establishes the eligibility facts, for the whole estate, in
// the queries it already ran.
func TestAutomationTargetsAreScreened(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	seedScreeningHost(t, db.SQL())
	seedScreeningContainer(t, db.SQL(), "c1", "web", "nginx:1.27", nil)
	seedScreeningContainer(t, db.SQL(), "c2",
		"web.hm-old-exec_0123456789abcdef0123", "nginx:1.26", nil)
	seedScreeningContainer(t, db.SQL(), "c3", "legacy", "legacy:1",
		map[string]string{domain.LabelHarborMasterEnabled: "false"})
	seedScreeningContainer(t, db.SQL(), "c4", "ghost", "", nil)

	targets, truncated, err := db.Containers.AutomationTargets(ctx)
	if err != nil {
		t.Fatalf("load automation targets: %v", err)
	}
	if truncated {
		t.Fatal("four containers is not a truncated estate")
	}

	byName := make(map[string]domain.TargetEligibility, len(targets))
	for _, target := range targets {
		byName[target.Selection.Name] = target.Selection.Eligibility
	}

	if got := byName["web"]; !got.Recreatable || got.Derived || got.OptedOut {
		t.Fatalf("an ordinary workload screened as %+v", got)
	}
	if got := byName["web.hm-old-exec_0123456789abcdef0123"]; !got.Derived {
		t.Fatalf("a parked original screened as %+v", got)
	}
	if got := byName["legacy"]; !got.OptedOut {
		t.Fatalf("an opted-out container screened as %+v", got)
	}
	if got := byName["ghost"]; got.Recreatable {
		t.Fatalf("a container with no image screened as recreatable: %+v", got)
	}

	// And the broad scope agrees with the screening.
	broad := domain.UpdatePolicy{
		PolicyID: domain.NewUpdatePolicyID(),
		Enabled:  true,
		Scope:    domain.ScopeAllEligible,
	}
	selected := make([]string, 0, len(targets))
	for _, target := range targets {
		if broad.Governs(target.Selection, domain.SelfIdentity{}) {
			selected = append(selected, target.Selection.Name)
		}
	}
	if len(selected) != 1 || selected[0] != "web" {
		t.Fatalf("the broad scope selected %v, want only the ordinary workload", selected)
	}
}

// ------------------------------------------------------------- helpers --

func screenedStoreTarget(name, image string) domain.SelectionTarget {
	return domain.SelectionTarget{
		Name:        name,
		Image:       image,
		Eligibility: domain.ScreenTarget(name, image, nil),
	}
}

// seedScreeningHost inserts the host row containers reference.
func seedScreeningHost(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO hosts (id, name, runtime, api_version, os_type, created_at, updated_at)
		VALUES ('local', 'test', 'docker', '1.45', 'linux',
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
}

// seedScreeningContainer inserts a present container and its labels.
func seedScreeningContainer(
	t *testing.T,
	db *sql.DB,
	id, name, image string,
	labels map[string]string,
) {
	t.Helper()
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO containers
			(id, host_id, short_id, name, image_ref, state, created_at,
			 present, first_seen_at, last_seen_at, generation)
		VALUES (?, 'local', ?, ?, ?, 'running', '2026-01-01T00:00:00Z',
		        1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 1)`,
		id, id, name, image)
	if err != nil {
		t.Fatalf("seed container %s: %v", name, err)
	}
	for key, value := range labels {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO container_labels (container_id, key, value, source)
			VALUES (?, ?, ?, 'harbormaster')`, id, key, value); err != nil {
			t.Fatalf("seed label %s on %s: %v", key, name, err)
		}
	}
}
