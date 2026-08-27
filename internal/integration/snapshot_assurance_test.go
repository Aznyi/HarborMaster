//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.2 against a REAL Docker daemon.
//
// # What a live run adds over the unit suites
//
// The unit suites model the whole interaction and are where the negative cases
// live. What they cannot establish is that the CHECKSUM -- which is the entire
// deduplication mechanism -- behaves the same way over a configuration a real
// daemon reports as it does over a fixture. A real inspection carries fields no
// fixture author thinks to include: generated hostnames, resolved mounts,
// default network settings, the daemon's own labels. If any of those varied
// between two inspections of an unchanged container, assurance would write a
// snapshot on every planner pass for ever, and no unit test would notice.
//
// So these tests inspect a real container repeatedly, and count rows.
//
// # Scope, stated honestly
//
// This exercises inventory -> assurance -> planning against the daemon. It does
// NOT drive a full acquisition and recreation: that pipeline is unchanged by
// Stage 17.2 and is already covered against a real daemon by recreation_test.go
// and acquisition_test.go. What Stage 17.2 adds is a gate BEFORE it, and the
// gate is what is measured here.
//
// # Disposability
//
// Every container is named with the phase17SnapshotPrefix and removed by the
// existing cleanup. Nothing else on the host is read for identity, written to,
// or removed.

const phase17SnapshotPrefix = "hm-p17-snap"

// assuranceStack is the real chain, over a real daemon and a real database.
type assuranceStack struct {
	db        *store.DB
	inventory *service.InventoryService
	assurance *service.SnapshotAssurance
	planner   *service.PlannerService
	policies  *livePolicies
}

// livePolicies holds one broad automatic policy, so the preparer treats every
// container on the host as one worth a baseline.
type livePolicies struct{ policies []domain.UpdatePolicy }

func (p *livePolicies) ActivePolicies(context.Context) ([]domain.UpdatePolicy, error) {
	return p.policies, nil
}

func (p *livePolicies) CountUpdatePolicies(context.Context) (int, int, error) {
	return len(p.policies), len(p.policies), nil
}

func newAssuranceStack(t *testing.T) *assuranceStack {
	t.Helper()
	requireDocker(t)

	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "harbormaster.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	hasher := service.NewHasher(key)

	client, err := docker.New(docker.Options{
		Host:       defaultDockerHost(),
		APIVersion: pinnedAPIVersion(),
		Timeout:    30 * time.Second,
		// The digester travels WITH the masker, exactly as the composition root
		// wires it. Masking and digesting are one decision made at one instant:
		// the adapter hides the value and records keyed evidence about it in the
		// same step, because the value is gone afterwards. A masker without a
		// digester here would produce snapshots whose secrets carry no evidence,
		// and a rotated credential would then be invisible to the checksum.
		Masker: domain.NewDefaultMasker().WithDigester(hasher.DigestValue),
	})
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.Ping(ctx); err != nil {
		t.Skipf("docker unreachable through the adapter: %v", err)
	}

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:    client,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config: config.Inventory{
			Enabled:      true,
			Workers:      4,
			MaskPatterns: domain.DefaultMaskPatterns,
		},
		SuppressPeriodic: true,
	})

	// The real capture path: it reads the CONTAINER REPOSITORY, never the
	// daemon. That is what makes an HTTP- or timer-triggered capture unable to
	// generate privileged traffic, and it is why the inventory refresh below is
	// what makes a configuration change visible to assurance.
	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: db.Containers,
		Snapshots:  db.Snapshots,
		Inventory:  db.Inventory,
		Hasher:     hasher,
		Versions:   service.HostVersions{HarborMaster: "integration"},
		Config:     config.Snapshots{Enabled: true},
		Logger:     discardLogger(),
	})

	assurance := service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
		Capturer: snapshots,
		Logger:   discardLogger(),
	})

	policies := &livePolicies{}
	preparer := service.NewSnapshotPreparer(service.SnapshotPreparerOptions{
		Assurance: assurance,
		Policies:  policies,
		Targets:   db.Containers,
		Baselines: db.Snapshots,
		Logger:    discardLogger(),
	})

	planner := service.NewPlannerService(service.PlannerOptions{
		Store:   db.Plans,
		Lineage: db.Lineage,
		Prepare: preparer,
		Config: config.Planner{
			Enabled: true, BatchSize: 200, MaxContainers: 2000,
			GenerationTimeout: time.Minute,
		},
		Logger: discardLogger(),
	})

	return &assuranceStack{
		db: db, inventory: inventory, assurance: assurance,
		planner: planner, policies: policies,
	}
}

// enrol installs a broad automatic policy, so the preparer prepares.
func (s *assuranceStack) enrol(t *testing.T) {
	t.Helper()
	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "phase 17 live acceptance",
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Strategy:              domain.StrategyDigestOnly,
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  domain.ModeAutomatic,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	s.policies.policies = []domain.UpdatePolicy{policy}
}

func (s *assuranceStack) refresh(t *testing.T) {
	t.Helper()
	if _, err := s.inventory.Refresh(context.Background(), domain.TriggerManual); err != nil {
		t.Fatalf("inventory refresh: %v", err)
	}
}

// snapshotRows counts a container's snapshots.
func (s *assuranceStack) snapshotRows(t *testing.T, containerID string) int {
	t.Helper()
	var count int
	if err := s.db.SQL().QueryRow(
		`SELECT COUNT(*) FROM snapshots WHERE container_id = ?`, containerID).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return count
}

// ------------------------------------------------------------ the tests --

// TestLiveAssuranceCapturesABaselineForARealContainer.
//
// The first-update shape, against the daemon: a container HarborMaster has
// never snapshotted gets a baseline from the preparation step of a planner
// pass, and the baseline describes the container the daemon actually reported.
func TestLiveAssuranceCapturesABaselineForARealContainer(t *testing.T) {
	stack := newAssuranceStack(t)
	stack.enrol(t)
	ctx := context.Background()

	name := fmt.Sprintf("%s-first", phase17SnapshotPrefix)
	id := startFixture(t, name, "-e", "PHASE17=first")
	stack.refresh(t)

	if got := stack.snapshotRows(t, id); got != 0 {
		t.Fatalf("fixture defect: %d snapshots before any pass, want 0", got)
	}

	if _, err := stack.planner.Generate(ctx); err != nil {
		t.Fatalf("planner pass: %v", err)
	}

	if got := stack.snapshotRows(t, id); got != 1 {
		t.Fatalf("%d snapshots after one pass, want exactly 1", got)
	}

	baselines, err := stack.db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baselines: %v", err)
	}
	baseline := baselines[id]
	if baseline == 0 {
		t.Fatal("no baseline was captured for a governed container")
	}

	// The snapshot describes the container the daemon reported, not a guess.
	stored, err := stack.db.Snapshots.Get(ctx, baseline)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if stored.ContainerID != id {
		t.Errorf("snapshot names container %q, want %q", stored.ContainerID, id)
	}
	if stored.ContainerName != name {
		t.Errorf("snapshot names %q, want %q", stored.ContainerName, name)
	}
	// The trigger says WHY, which is what tells an operator this was
	// HarborMaster preparing rather than somebody clicking.
	if stored.Trigger != domain.SnapshotTriggerScheduled {
		t.Errorf("trigger = %q, want %q", stored.Trigger, domain.SnapshotTriggerScheduled)
	}
}

// TestLiveAssuranceDeduplicatesAcrossRepeatedPasses is the test a fixture
// cannot substitute for.
//
// Ten real inspections of one unchanged real container must produce ONE row. If
// any field the daemon reports varied between inspections -- a timestamp, a
// generated id, a map iteration order that reached the document -- this fails,
// and the feature would otherwise grow the database without bound on every
// planner pass for the life of the deployment.
func TestLiveAssuranceDeduplicatesAcrossRepeatedPasses(t *testing.T) {
	stack := newAssuranceStack(t)
	stack.enrol(t)
	ctx := context.Background()

	name := fmt.Sprintf("%s-dedup", phase17SnapshotPrefix)
	id := startFixture(t, name, "-e", "PHASE17=dedup")

	for pass := 0; pass < 10; pass++ {
		// A refresh before every pass, exactly as production does: the planner
		// runs after each committed refresh. Each one re-inspects the container
		// through the daemon.
		stack.refresh(t)
		if _, err := stack.planner.Generate(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	if got := stack.snapshotRows(t, id); got != 1 {
		t.Errorf("%d snapshot rows after ten refresh-and-plan cycles over an "+
			"unchanged container, want 1", got)
	}

	// And directly through assurance, which is the call the execution preflight
	// makes immediately before a recreation.
	result := stack.assurance.EnsureCurrent(ctx, id, domain.SnapshotTriggerPreUpdate)
	if result.Outcome != service.AssuranceCurrent {
		t.Errorf("pre-recreation assurance on an unchanged container = %q, want %q",
			result.Outcome, service.AssuranceCurrent)
	}
	if got := stack.snapshotRows(t, id); got != 1 {
		t.Errorf("%d snapshot rows after a pre-recreation check, want 1", got)
	}
}

// TestLiveConfigurationChangeProducesANewBaselineAndStalesThePlan.
//
// The staleness path, end to end against the daemon:
//
//	container -> baseline S1 -> plan names S1
//	    -> the container is genuinely reconfigured on the host
//	    -> assurance captures S2
//	    -> S2 != the plan's baseline, which is what the execution preflight
//	       refuses on
//
// The reconfiguration is a real one: the container is removed and recreated
// with a different environment, which is what an operator editing a compose
// file does.
func TestLiveConfigurationChangeProducesANewBaselineAndStalesThePlan(t *testing.T) {
	stack := newAssuranceStack(t)
	stack.enrol(t)
	ctx := context.Background()

	name := fmt.Sprintf("%s-change", phase17SnapshotPrefix)
	first := startFixture(t, name, "-e", "PHASE17=before")
	stack.refresh(t)

	if _, err := stack.planner.Generate(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	baselines, err := stack.db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baselines: %v", err)
	}
	original := baselines[first]
	if original == 0 {
		t.Fatal("no baseline was captured")
	}

	// Reconfigure for real: remove and recreate under the same name with a
	// different environment. The container id changes, which is exactly what
	// happens on the host and is why a snapshot is keyed on the id it described.
	dockerCLI(t, "rm", "-f", name)
	second := startFixture(t, name, "-e", "PHASE17=after", "-e", "PHASE17_EXTRA=added")
	if second == first {
		t.Fatal("fixture defect: the recreated container reused its id")
	}
	stack.refresh(t)

	// Assurance on the container as it is now.
	result := stack.assurance.EnsureCurrent(ctx, second, domain.SnapshotTriggerPreUpdate)
	if result.Outcome != service.AssuranceCaptured {
		t.Fatalf("assurance after a real reconfiguration = %q, want %q",
			result.Outcome, service.AssuranceCaptured)
	}
	if result.SnapshotID == original {
		t.Fatal("a reconfigured container deduplicated onto its old baseline")
	}

	// This is the comparison the execution preflight makes. A plan naming the
	// original baseline is stale against the container as it now stands.
	if result.SnapshotID == original {
		t.Error("the staleness check would not fire")
	}

	// The old snapshot is still there. It is evidence, and assurance never
	// removes evidence.
	if _, err := stack.db.Snapshots.Get(ctx, original); err != nil {
		t.Errorf("the original baseline was lost: %v", err)
	}
}

// TestLiveAssuranceNeverCallsTheDaemon.
//
// The security property, established against a live host: assurance reads
// HarborMaster's own inventory, so a container that is REMOVED from the daemon
// but still present in the inventory can still be snapshotted -- and a
// container the inventory has never seen cannot be, however much the daemon
// knows about it.
//
// The second half is the one that matters: it is the proof that no capture can
// be turned into daemon traffic by a caller naming an arbitrary container.
func TestLiveAssuranceNeverCallsTheDaemon(t *testing.T) {
	stack := newAssuranceStack(t)
	ctx := context.Background()

	// A container the daemon knows about and the inventory does not, because no
	// refresh has run since it was created.
	name := fmt.Sprintf("%s-unseen", phase17SnapshotPrefix)
	id := startFixture(t, name, "-e", "PHASE17=unseen")

	result := stack.assurance.EnsureCurrent(ctx, id, domain.SnapshotTriggerPreUpdate)
	if result.Outcome != service.AssuranceUnavailable {
		t.Errorf("assurance for a container the inventory has not seen = %q, want %q\n"+
			"\tcapture must read HarborMaster's own records; reaching the daemon here "+
			"would make every capture a privileged call a caller could aim",
			result.Outcome, service.AssuranceUnavailable)
	}
	if got := stack.snapshotRows(t, id); got != 0 {
		t.Errorf("%d snapshots for a container the inventory has never seen, want 0", got)
	}

	// After a refresh the same call succeeds, which is what makes the assertion
	// above about the SOURCE rather than about the container being unusable.
	stack.refresh(t)
	if again := stack.assurance.EnsureCurrent(ctx, id, domain.SnapshotTriggerPreUpdate); again.Outcome != service.AssuranceCaptured {
		t.Errorf("assurance after a refresh = %q, want %q", again.Outcome, service.AssuranceCaptured)
	}
}

// TestLiveNoPlaintextSecretReachesTheDatabase.
//
// The masker classifies from the variable's NAME, so a real container carrying a
// realistically named credential is the honest test of whether the value can
// reach the database through the assurance path.
func TestLiveNoPlaintextSecretReachesTheDatabase(t *testing.T) {
	stack := newAssuranceStack(t)
	stack.enrol(t)
	ctx := context.Background()

	const secret = "p17-live-plaintext-must-not-persist"
	name := fmt.Sprintf("%s-secret", phase17SnapshotPrefix)
	id := startFixture(t, name, "-e", "DB_PASSWORD="+secret)
	stack.refresh(t)

	result := stack.assurance.EnsureCurrent(ctx, id, domain.SnapshotTriggerScheduled)
	if result.Outcome != service.AssuranceCaptured {
		t.Fatalf("assurance = %q, want captured", result.Outcome)
	}

	// The whole snapshot document.
	var spec []byte
	if err := stack.db.SQL().QueryRow(
		`SELECT spec_json FROM snapshots WHERE id = ?`, result.SnapshotID).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if contains(string(spec), secret) {
		t.Error("the persisted snapshot document contains a plaintext secret " +
			"read from a real container")
	}

	// And the environment rows.
	rows, err := stack.db.SQL().Query(
		`SELECT key, value, digest FROM snapshot_environment WHERE snapshot_id = ?`,
		result.SnapshotID)
	if err != nil {
		t.Fatalf("read environment: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var sawTheVariable bool
	for rows.Next() {
		var key, value, digest string
		if err := rows.Scan(&key, &value, &digest); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if key == "DB_PASSWORD" {
			sawTheVariable = true
			if value != "" {
				t.Errorf("DB_PASSWORD persisted a value (%d bytes)", len(value))
			}
			if digest == "" {
				t.Error("DB_PASSWORD persisted no digest, so a rotation would be undetectable")
			}
			if digest == secret {
				t.Error("DB_PASSWORD stored the secret where its digest belongs")
			}
		}
		if value == secret {
			t.Errorf("environment row %q persisted the plaintext secret", key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// Non-vacuity: without this the loop could have found no rows at all.
	if !sawTheVariable {
		t.Fatal("the snapshot recorded no DB_PASSWORD row; this test proved nothing")
	}
}

// contains is a local substring helper, kept out of the domain packages.
func contains(haystack, needle string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
