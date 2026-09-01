package service_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The unattended lifecycle rig: real services, real database, one modelled host.
//
// # What this proves that nothing else does
//
// Every existing automation test uses a FAKE pipeline. They prove the engine
// SUBMITS the right requests -- which is the safety property, and is the right
// thing for them to prove. None of them proves the requests are carried out,
// because on the other side of the fake there is nothing.
//
// So the product promise -- "turn it on and walk away" -- had no test. This rig
// is the missing one. It assembles the services exactly as cmd/harbormaster
// does, over a real SQLite database with real migrations, and drives them with
// their OWN scheduler loops at an accelerated cadence.
//
// # The rule this rig exists to enforce
//
// For an automatic scenario the test may seed the world, configure policy, and
// observe. It may NOT call Request on the acquisition, execution, or rollback
// service. If it did, it would prove that a manual pipeline works, which is
// already known. Every mutation below is reached because a scheduler decided to
// reach it.
//
// # Cadence, not sleeps
//
// The intervals are milliseconds instead of minutes. That is the production
// loop running fast, not a test-only path: config.Automation is passed to the
// service as a value, and the bounds in internal/config guard what an OPERATOR
// may set, not what the type can hold. Waiting is done by polling durable rows
// against a deadline, never by sleeping for a fixed period.

const (
	c4cContainerID = "aaaaaaaabbbbccccddddeeeeffff00001111222233334444555566667777888a"
	c4cName        = "hm-c4c-web"
	c4cRegistry    = "ghcr.io"
	c4cRepository  = "acme/app"
	c4cCurrentRef  = "ghcr.io/acme/app:1.0.0"
	c4cCurrentTag  = "1.0.0"
	c4cNextTag     = "1.0.1"
)

var (
	c4cCurrentDigest = "sha256:" + strings.Repeat("a", 64)
	c4cNextDigest    = "sha256:" + strings.Repeat("b", 64)
	c4cCurrentImage  = imageIDForDigest(c4cCurrentDigest)
)

// unattendedRig is the whole application, minus the HTTP surface.
type unattendedRig struct {
	t    *testing.T
	path string

	db   *store.DB
	host *unattendedHost

	inventory    *service.InventoryService
	intel        *service.ImageIntelService
	policies     *service.PolicyService
	planner      *service.PlannerService
	acquisitions *service.AcquisitionService
	executions   *service.ExecutionService
	rollbacks    *service.RollbackService
	automation   *service.AutomationService

	notifier *recordingNotifier

	// running holds the cancel for the scheduler loops, so a test can stop the
	// world at a lifecycle boundary and start it again -- which is what a
	// restart is.
	cancel  context.CancelFunc
	stopped chan struct{}
}

// rigOptions tunes one rig before its services are built.
type rigOptions struct {
	// policies are written before the first pass. Empty means the deployment
	// has none, which is what a fresh install looks like.
	policies []domain.UpdatePolicy
	// selfIdentity is what HarborMaster believes it is running as.
	selfIdentity domain.SelfIdentity
	// rollbackEnabled mirrors the deployment switch. On by default here
	// because the failure-and-recovery scenario is the point.
	rollbackDisabled bool
	// labels are placed on the workload container, for the opt-out scenario.
	labels map[string]string
}

func newUnattendedRig(t *testing.T, tune ...func(*rigOptions)) *unattendedRig {
	t.Helper()

	options := rigOptions{}
	for _, apply := range tune {
		apply(&options)
	}

	path := filepath.Join(t.TempDir(), "harbormaster.db")
	rig := &unattendedRig{t: t, path: path, host: newUnattendedHost(), notifier: &recordingNotifier{}}

	rig.host.add(&hostContainer{
		id:      c4cContainerID,
		name:    c4cName,
		image:   c4cCurrentRef,
		imageID: c4cCurrentImage,
		running: true,
		health:  []domain.HealthState{domain.HealthHealthy},
		detail:  c4cContainerDetail(options.labels),
	})
	rig.host.addImage(c4cCurrentRef, &domain.Image{
		ID: c4cCurrentImage, RepoDigests: []string{
			c4cRegistry + "/" + c4cRepository + "@" + c4cCurrentDigest,
		},
		OS: "linux", Architecture: "amd64",
	})
	rig.host.addImage(c4cRegistry+"/"+c4cRepository+"@"+c4cCurrentDigest, &domain.Image{
		ID: c4cCurrentImage, RepoDigests: []string{
			c4cRegistry + "/" + c4cRepository + "@" + c4cCurrentDigest,
		},
		OS: "linux", Architecture: "amd64",
	})
	// The same artefact keyed by its LOCAL ID, which is how the inventory looks
	// an image up. Without this the estate looks like one built on the host: no
	// registry digest, so nothing can confirm what is deployed, and every plan
	// fails closed at unknown.
	rig.host.addImage(c4cCurrentImage, &domain.Image{
		ID: c4cCurrentImage, RepoDigests: []string{
			c4cRegistry + "/" + c4cRepository + "@" + c4cCurrentDigest,
		},
		OS: "linux", Architecture: "amd64",
	})

	rig.open(options)
	return rig
}

// open builds every service over the database at rig.path.
//
// Separated from construction so a restart test can close the rig and call this
// again: a restart is a new process over the same file, and that is exactly
// what this models.
func (r *unattendedRig) open(options rigOptions) {
	r.t.Helper()

	db, err := store.Open(context.Background(), r.path)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.db = db

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(filepath.Dir(r.path), "secret.key"),
	})
	if err != nil {
		r.t.Fatalf("load secret key: %v", err)
	}
	hasher := service.NewHasher(key)
	quiet := rigLogger()
	self := selfReporter{identity: options.selfIdentity}

	r.inventory = service.NewInventoryService(service.InventoryOptions{
		Runtime:    r.host,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     quiet,
		Config:     config.Inventory{Enabled: true, RefreshInterval: time.Hour, Workers: 2},
	})

	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: db.Containers,
		Snapshots:  db.Snapshots,
		Inventory:  db.Inventory,
		Hasher:     hasher,
		Config:     config.Snapshots{Enabled: true},
		Logger:     quiet,
	})
	assurance := service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
		Capturer: snapshots,
		Logger:   quiet,
	})
	preparer := service.NewSnapshotPreparer(service.SnapshotPreparerOptions{
		Assurance: assurance,
		Policies:  db.UpdatePolicies,
		Targets:   db.Containers,
		Baselines: db.Snapshots,
		Self:      self,
		Logger:    quiet,
	})

	// The real intelligence service, for its PROJECTION only. The registry
	// lookup itself is the one thing a test cannot perform, so the answer is
	// seeded through RecordCheck below; everything that turns an inventory into
	// a set of references to ask about is the production code path.
	r.intel = service.NewImageIntelService(service.ImageIntelOptions{
		Lineage:    db.Lineage,
		Reconciler: service.NewLineageReconciler(db.Lineage, quiet, nil),
		Store:      db.ImageIntel,
		// A real registry client, never used: the rig seeds the ANSWER through
		// RecordCheck rather than letting anything reach the network. It is here
		// because the service refuses to project references without one, and
		// satisfying that guard is more honest than routing around it.
		Registry: registry.New(registry.Options{
			Version: "c4c-test", RequestTimeout: time.Second, MaxAttempts: 1,
		}),
		Config: config.ImageIntel{
			Enabled: true, RefreshInterval: time.Hour,
			MaxTrackedReferences: 500, MaxReferencesPerPass: 50,
			MaxConcurrentRequests: 1, RequestTimeout: time.Second, MaxAttempts: 1,
		},
		Notify: r.notifier,
		Logger: quiet,
	})

	// Compliance. The execution preflight refuses without a recent evaluation
	// for the container -- "the policy evaluation is missing or too old to
	// establish compliance" -- which is the correct fail-closed behaviour and
	// one of the gates that silently blocks an unattended update if the engine
	// is not running. A rig without it would be testing a deployment nobody
	// has.
	r.policies = service.NewPolicyService(service.PolicyOptions{
		Definitions: db.Policies,
		Containers:  db.Containers,
		Violations:  db.Policies,
		Pruner:      db.Policies,
		Inventory:   db.Inventory,
		Config:      config.Policy{Enabled: true, SweepInterval: time.Hour},
		Notify:      r.notifier,
		Logger:      quiet,
	})

	r.planner = service.NewPlannerService(service.PlannerOptions{
		Store:   db.Plans,
		Lineage: db.Lineage,
		Prepare: preparer,
		Config: config.Planner{Enabled: true, Interval: time.Hour, BatchSize: 50,
			MaxContainers: 200, GenerationTimeout: 30 * time.Second},
		Notify: r.notifier,
		Logger: quiet,
	})

	r.acquisitions = service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    db.Acquisitions,
		Evidence: service.NewPlanEvidence(db.Plans, db.ImageIntel, db.Containers),
		Runtime:  r.host,
		Acquirer: r.host,
		Self:     self,
		Notify:   r.notifier,
		Config: config.Acquisition{
			Enabled: true, RequireSnapshot: false,
			MaxConcurrent: 2, MaxPerRegistry: 2,
			PullTimeout: 10 * time.Second, RequestTTL: time.Hour,
			RegistryFreshness:       24 * time.Hour,
			SweepInterval:           2 * time.Millisecond,
			MaxEventsPerAcquisition: 50,
		},
		Logger: quiet,
	})

	dependencies := service.NewDependencyService(service.DependencyOptions{
		Store:      db.Dependencies,
		Lineage:    service.NewDependencyLineage(db.Containers),
		Operations: db.DependencyOperations,
		Executions: service.NewDependencyExecutions(db.Executions),
		Logger:     quiet,
	})

	planApprovals := service.NewPlanApprovalService(service.PlanApprovalOptions{
		Store:  db.PlanApprovals,
		Plans:  db.Plans,
		Logger: quiet,
	})

	r.executions = service.NewExecutionService(service.ExecutionOptions{
		Lineage: db.Lineage,
		Store:   db.Executions,
		Evidence: service.NewExecutionEvidence(
			db.Acquisitions, db.Plans, db.Containers,
			db.Snapshots, db.Policies, db.Inventory, db.ImageIntel),
		Runtime:      r.host,
		Capturer:     r.host,
		Mutator:      r.host,
		Assurance:    assurance,
		Approvals:    planApprovals,
		Self:         self,
		Dependencies: dependencies,
		Hasher:       hasher,
		Notify:       r.notifier,
		Config: config.Execution{
			Enabled: true, RequireSnapshot: true,
			StartupTimeout: 2 * time.Second, StabilityPeriod: time.Millisecond,
			HealthPollInterval: time.Millisecond, StopTimeout: time.Second,
			MaxConcurrent: 1, RequestTTL: time.Hour,
			AcquisitionFreshness: time.Hour, InventoryFreshness: time.Hour,
			SweepInterval: 2 * time.Millisecond, MaxEventsPerExecution: 100,
		},
		Logger: quiet,
	})

	if !options.rollbackDisabled {
		r.rollbacks = service.NewRollbackService(service.RollbackOptions{
			Lineage:    db.Lineage,
			Store:      db.Rollbacks,
			Evidence:   service.NewRollbackEvidence(db.Executions, db.Inventory),
			Runtime:    r.host,
			Rollbacker: r.host,
			Hasher:     hasher,
			Notify:     r.notifier,
			Config: config.Rollback{
				Enabled: true, MaxConcurrent: 1, RequestTTL: time.Hour,
				StartupTimeout: 2 * time.Second, StabilityPeriod: time.Millisecond,
				HealthPollInterval: time.Millisecond, StopTimeout: time.Second,
				InventoryFreshness: time.Hour,
				SweepInterval:      2 * time.Millisecond, MaxEventsPerRollback: 100,
			},
			Logger: quiet,
		})
	}

	r.automation = service.NewAutomationService(service.AutomationOptions{
		Store:    db.Automation,
		Policies: db.UpdatePolicies,
		Evidence: service.NewAutomationEvidence(
			db.Containers, db.Plans, db.Acquisitions, db.Executions,
			db.Lineage, db.ImageIntel),
		Pipeline:     service.NewAutomationPipeline(r.acquisitions, r.executions, r.rollbacks),
		Dependencies: dependencies,
		Notify:       r.notifier,
		Self:         self,
		Config: config.Automation{
			Enabled: true,
			// A minute, so the SCHEDULED pass never fires during a test: the
			// decision passes are driven explicitly by RunNow, which is the
			// same method the loop calls, so which one ran is deterministic.
			Interval: time.Minute,
			// Milliseconds, so the follower advances work promptly. This is the
			// production follower, running fast.
			FollowInterval: 2 * time.Millisecond,
			StartupDelay:   time.Hour,
			PruneInterval:  time.Hour,
			PassTimeout:    30 * time.Second,
			MaxConcurrent:  2,
			MaxPerRun:      10,
		},
		Logger: quiet,
	})

	// A compliance policy, so the sweep has something to evaluate and the
	// execution preflight can establish compliance. Deliberately one the
	// workload PASSES: the point of the rig is the update lifecycle, and a
	// violating container is Scenario K's business.
	if _, err := db.Policies.CreatePolicy(context.Background(), domain.PolicyDefinition{
		PolicyID: domain.NewPolicyID(),
		Name:     "hm-c4c baseline",
		Severity: domain.PolicySeverityMedium,
		Enabled:  true,
		Rules:    []domain.PolicyRule{{Type: domain.RuleUserNotRoot}},
	}, r.now()); err != nil && !strings.Contains(err.Error(), "already") {
		r.t.Fatalf("write compliance policy: %v", err)
	}

	for _, policy := range options.policies {
		if _, err := db.UpdatePolicies.CreateUpdatePolicy(context.Background(), policy, r.now()); err != nil {
			r.t.Fatalf("write policy: %v", err)
		}
	}
}

// rigLogger discards output. A rig that logged would bury the assertion that
// failed under a hundred lines of scheduler chatter.
func rigLogger() *slog.Logger {
	if os.Getenv("C4C_VERBOSE") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (r *unattendedRig) now() time.Time { return time.Now().UTC() }

// start runs every scheduler loop, exactly as the composition root does.
func (r *unattendedRig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.stopped = make(chan struct{})

	go func() {
		defer close(r.stopped)
		done := make(chan struct{}, 4)
		go func() { r.acquisitions.Run(ctx); done <- struct{}{} }()
		go func() { r.executions.Run(ctx); done <- struct{}{} }()
		go func() { r.automation.Run(ctx); done <- struct{}{} }()
		if r.rollbacks != nil {
			go func() { r.rollbacks.Run(ctx); done <- struct{}{} }()
		} else {
			done <- struct{}{}
		}
		for i := 0; i < 4; i++ {
			<-done
		}
	}()
}

// startAcquisitionsOnly runs the acquisition worker and nothing else.
//
// The world stops with an image downloaded and no recreation, which is the
// boundary a restart is most likely to land on: the pull is the long part.
func (r *unattendedRig) startAcquisitionsOnly() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.stopped = make(chan struct{})
	go func() { defer close(r.stopped); r.acquisitions.Run(ctx) }()
}

// startWithoutRollback runs everything except the rollback worker.
//
// Models a process that failed an update and died before recovering it -- and,
// equally, a deployment that had rollback switched off at the time.
func (r *unattendedRig) startWithoutRollback() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.stopped = make(chan struct{})

	go func() {
		defer close(r.stopped)
		done := make(chan struct{}, 3)
		go func() { r.acquisitions.Run(ctx); done <- struct{}{} }()
		go func() { r.executions.Run(ctx); done <- struct{}{} }()
		go func() { r.automation.Run(ctx); done <- struct{}{} }()
		for i := 0; i < 3; i++ {
			<-done
		}
	}()
}

// stop halts the schedulers and closes the database. The other half of a
// restart.
func (r *unattendedRig) stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.stopped
		r.cancel = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

// restart stops everything and builds it again over the same database file.
//
// The point of the exercise: every durable row survives, every in-memory queue
// does not, and the lifecycle has to resume from what was written down.
func (r *unattendedRig) restart(options rigOptions) {
	r.t.Helper()
	r.stop()
	// Policies are already in the database; writing them again would conflict.
	options.policies = nil
	r.open(options)
	r.start()
}

// ------------------------------------------------------------- seeding --

// refreshInventory runs the real inventory pass against the modelled host.
func (r *unattendedRig) refreshInventory() {
	r.t.Helper()
	if _, err := r.inventory.Refresh(context.Background(), domain.TriggerManual); err != nil {
		r.t.Fatalf("inventory refresh: %v", err)
	}
}

// publishUpdate seeds the registry evidence that a newer image exists.
//
// This is the ONE thing the rig fakes about the outside world, and it has to
// be: the alternative is a real registry. It writes through the repository's
// own RecordCheck, so the row is shaped exactly as a real collection would
// leave it -- a settled, successful check with a remote digest.
func (r *unattendedRig) publishUpdate(update domain.UpdateType, status domain.CheckStatus) {
	r.t.Helper()

	outcome := store.CheckOutcome{
		Reference:    c4cCurrentRef,
		Status:       status,
		RemoteDigest: c4cCurrentDigest,
		Update:       update,
		LatestTag:    c4cNextTag,
		LatestDigest: c4cNextDigest,
		Platform:     domain.Platform{OS: "linux", Architecture: "amd64"},
	}
	if status != domain.CheckOK {
		outcome.RemoteDigest = ""
		outcome.LatestDigest = ""
		outcome.Update = domain.UpdateUnknown
		outcome.Detail = "the registry did not answer"
	}
	if err := r.db.ImageIntel.RecordCheck(context.Background(), outcome, r.now()); err != nil {
		r.t.Fatalf("record registry check: %v", err)
	}
}

// syncIntelReferences projects the inventory into the intelligence table, which
// is what the real collector does before it looks anything up.
func (r *unattendedRig) syncIntelReferences() {
	r.t.Helper()
	if _, err := r.intel.SyncInventory(context.Background()); err != nil {
		r.t.Fatalf("sync intel references: %v", err)
	}
}

// evaluateCompliance runs the real policy engine over the estate.
//
// The execution preflight requires a RECENT evaluation, so this is part of the
// world an unattended deployment has, not a convenience.
func (r *unattendedRig) evaluateCompliance() {
	r.t.Helper()
	if _, err := r.policies.Sweep(context.Background()); err != nil {
		r.t.Fatalf("policy sweep: %v", err)
	}
}

// plan runs the real planner once.
func (r *unattendedRig) plan() service.GenerateResult {
	r.t.Helper()
	result, err := r.planner.Generate(context.Background())
	if err != nil {
		r.t.Fatalf("planner: %v", err)
	}
	return result
}

// decide runs one real decision pass -- the same method the scheduler's own
// ticker calls.
func (r *unattendedRig) decide() (domain.AutomationRun, []domain.AutomationDecision) {
	r.t.Helper()
	run, decisions, err := r.automation.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		r.t.Fatalf("automation pass: %v", err)
	}
	return run, decisions
}

// ------------------------------------------------------------- waiting --

// await polls a condition against a deadline.
//
// Never a sleep-then-assert: a fixed sleep is either too short and flaky or too
// long and slow, and it hides how long the thing actually took.
func (r *unattendedRig) await(what string, condition func() bool) {
	r.t.Helper()

	deadline := time.After(20 * time.Second)
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			r.t.Fatalf("timed out waiting for %s decisions:%s\n\nhost operations: %v\nnotifications: %v",
				what, r.host.operations(), eventsOf(r.notifier.all()), r.decisionDetails())
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitNoChange asserts a condition STAYS true over many scheduler ticks.
//
// The shape every "zero mutation" scenario needs: proving nothing happened once
// proves only that nothing had happened yet.
func (r *unattendedRig) awaitNoChange(what string, condition func() bool) {
	r.t.Helper()

	settle := time.After(150 * time.Millisecond)
	for {
		if !condition() {
			r.t.Fatalf("%s stopped being true\n\nhost operations: %v", what, r.host.operations())
		}
		select {
		case <-settle:
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// ------------------------------------------------------------ reading --

func (r *unattendedRig) acquisitionCount() int {
	r.t.Helper()
	var count int
	if err := r.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM acquisitions`).Scan(&count); err != nil {
		r.t.Fatalf("count acquisitions: %v", err)
	}
	return count
}

func (r *unattendedRig) executionCount() int {
	r.t.Helper()
	var count int
	if err := r.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM executions`).Scan(&count); err != nil {
		r.t.Fatalf("count executions: %v", err)
	}
	return count
}

func (r *unattendedRig) rollbackCount() int {
	r.t.Helper()
	var count int
	if err := r.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM rollbacks`).Scan(&count); err != nil {
		r.t.Fatalf("count rollbacks: %v", err)
	}
	return count
}

// terminalExecution returns the one execution, once it has settled.
func (r *unattendedRig) terminalExecution() domain.Execution {
	r.t.Helper()

	executions, _, err := r.db.Executions.List(context.Background(), store.ExecutionFilter{
		Page: store.Page{Limit: 10},
	})
	if err != nil {
		r.t.Fatalf("list executions: %v", err)
	}
	if len(executions) == 0 {
		r.t.Fatal("no execution was created")
	}
	return executions[0]
}

// c4cContainerDetail is the workload as the daemon reports it.
func c4cContainerDetail(labels map[string]string) domain.ContainerDetail {
	list := make([]domain.Label, 0, len(labels))
	for key, value := range labels {
		// Classified as the adapter classifies it. A label with no source
		// is not the shape normalize.go produces.
		list = append(list, domain.Label{
			Key: key, Value: value, Source: domain.ClassifyLabel(key),
		})
	}
	// The same normalisation internal/docker/normalize.go performs. Without it
	// a modelled container carries its labels as display text only, and every
	// label-driven gate -- the opt-out, the per-container preferences -- reads
	// an empty metadata block and passes. A fake that skips the adapter's own
	// step tests a world that cannot exist.
	metadata := domain.ParseHarborMasterMetadata(labels)

	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			HostID:        domain.LocalHostID,
			ID:            c4cContainerID,
			ShortID:       domain.ShortenID(c4cContainerID),
			Name:          c4cName,
			Image:         domain.ParseImageRef(c4cCurrentRef),
			ImageID:       c4cCurrentImage,
			State:         domain.StateRunning,
			Status:        "Up 2 hours",
			Health:        domain.HealthHealthy,
			CreatedAt:     time.Now().UTC().Add(-2 * time.Hour),
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
			Present:       true,
			HarborMaster:  metadata,
		},
		State:        domain.StateDetail{State: domain.StateRunning, RawState: "running", Health: domain.HealthHealthy},
		Process:      domain.Process{User: "1000:1000", WorkingDir: "/app"},
		Environment:  []domain.EnvVar{{Name: "SAFE", Value: "value", RawValue: "value"}},
		Labels:       list,
		HarborMaster: metadata,
		Mounts:       []domain.Mount{},
		Networks:     []domain.NetworkAttachment{},
		Warnings:     []domain.InventoryWarning{},
	}
}

// c4cAutomaticPolicy is an operator's own policy: automatic, with automatic
// rollback permitted.
//
// The window spans the whole day, so a pass is never refused for being outside
// it. That is not the window gate being bypassed -- Scenario K closes the window
// and asserts the refusal -- it is the gate being satisfied.
func c4cAutomaticPolicy() domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		// Explicit, because the DECISION carries the policy id and the follower
		// re-reads the governing policy by it to answer "may this failure be
		// rolled back". An empty id resolves to no policy and automatic rollback
		// is correctly refused -- which looks exactly like the feature being
		// broken. A real policy always has one; only a fixture can forget.
		PolicyID: "upd_c4c0000000000000c4c0",
		Name:     "hm-c4c automatic",
		Enabled:  true,
		Priority: 10,
		Selector: domain.UpdateSelector{Include: []string{c4cName}},
		Strategy: domain.StrategyMinor,
		// The floor the SHIPPED presets use. Every preset in
		// web/src/api/automationPresets.ts sets proceedWithCaution, and a
		// fixture demanding "proceed" would be testing a policy the product
		// does not offer -- and would pass only on a risk profile no real
		// minor-version update has.
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  domain.ModeAutomatic,
		Window:                domain.MaintenanceWindow{Start: "00:00", End: "23:59"},
		Failure:               domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()
	return policy
}

// c4cPolicyWithMode is the same policy in a different mode.
func c4cPolicyWithMode(mode domain.AutomationMode) domain.UpdatePolicy {
	policy := c4cAutomaticPolicy()
	policy.Mode = mode
	policy.Normalise()
	return policy
}

// decisionDetails renders every recorded decision's reasoning.
//
// Included in every timeout message, because "the lifecycle did not finish" is
// unactionable and "the execution preflight refused because X" is the answer.
func (r *unattendedRig) decisionDetails() string {
	rows, err := r.db.SQL().QueryContext(context.Background(),
		`SELECT verdict, reason, detail, acquisition_id, execution_id, rollback_id, policy_id
		   FROM automation_decisions ORDER BY id`)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var verdict, reason, detail, acq, exec, rb, pol string
		if err := rows.Scan(&verdict, &reason, &detail, &acq, &exec, &rb, &pol); err != nil {
			return "unscannable: " + err.Error()
		}
		out = append(out, "\n  verdict="+verdict+" reason="+reason+
			" policy="+pol+" acq="+acq+" exec="+exec+" rb="+rb+"\n    detail="+detail)
	}
	// The refusal reason lives on the failure row, not on the decision: the
	// decision records what the PASS concluded, and the refusal happened later,
	// in the follower.
	var name, detail string
	if err := r.db.SQL().QueryRowContext(context.Background(),
		`SELECT container_name, COALESCE(last_detail, '') FROM automation_failures LIMIT 1`,
	).Scan(&name, &detail); err == nil {
		out = append(out, "\n  lastFailure["+name+"]="+detail)
	}
	return strings.Join(out, "")
}
