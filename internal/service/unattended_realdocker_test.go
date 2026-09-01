package service_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The same unattended promise, against a REAL Docker daemon.
//
// # Why this exists alongside the modelled rig
//
// C4C proved the lifecycle over one coherent modelled host. That rig is
// faithful -- building it forced three fixture corrections that a sloppier fake
// would have hidden -- but it is not a daemon. It cannot be wrong in the ways a
// daemon is wrong: name collisions, a container that exits half a second after
// `start` returns success, an image id that is not the manifest digest, a
// removal the daemon refuses.
//
// So this is the same product promise, driven by the same real services and the
// same real schedulers, with every Docker capability held by the real
// docker.Client. Nothing here calls Request on the acquisition, execution or
// rollback service; the schedulers do all of it.
//
// # The disposable boundary
//
// Every container this file creates is named hm-c4c1-*. Every test asserts,
// from the daemon's OWN EVENT STREAM, that no container outside that prefix was
// mutated -- a stronger claim than counting rows, because a row only records
// what HarborMaster meant to do. The images are public and are pulled rather
// than created; see the note on the constants below.
//
// Off unless HARBORMASTER_DOCKER_INTEGRATION=1.

// The images these scenarios move between.
//
// # Why a public registry and not a throwaway local one
//
// The first attempt stood up a `registry:2` container and pushed two built
// images to `localhost:5000`. Nothing worked, and the reason is a product rule
// rather than a bug: domain.NormalizeImageRef REFUSES an address-literal or
// local registry with "image reference is not supported for registry lookup",
// and the intelligence service tracks such references as unsupported on
// purpose. A rig built on localhost:5000 would have been testing a shape
// HarborMaster declines to act on.
//
// So the images are two real, adjacent alpine tags from Docker Hub. Both are
// pulled by immutable digest during the run, which is the path under test.
//
// # The one deviation from the hm-c4c1-* naming rule
//
// Containers, and every artefact this file CREATES, are hm-c4c1-*. The images
// cannot be: they must live in a repository HarborMaster will look up, which
// means a public one. They are pulled, never built, never pushed, and never
// removed by this file -- a host that already had them is unchanged, and a host
// that did not gains two public alpine tags.
const (
	c4c1Registry   = "docker.io"
	c4c1Repository = "library/alpine"
	c4c1CurrentRef = "alpine:3.20.3"
	c4c1NextRef    = "alpine:3.21.0"
	c4c1NextTag    = "3.21.0"

	// c4c1HealthyCheck passes on both images: the file exists in every alpine.
	c4c1HealthyCheck = "test -f /etc/alpine-release"
	// c4c1VersionCheck passes on 3.20 and FAILS on 3.21.
	//
	// A real health check of the kind an application ships -- "am I the version
	// I expect" -- inherited by the replacement through the preservation
	// contract and failing honestly against the new image. Nothing writes
	// execution state directly; the daemon reports unhealthy because the
	// command really exits non-zero.
	c4c1VersionCheck = "grep -q '^3\\.20' /etc/alpine-release"
)

func skipUnlessRealDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("HARBORMASTER_DOCKER_INTEGRATION") != "1" {
		t.Skip("set HARBORMASTER_DOCKER_INTEGRATION=1 to run against a real Docker daemon")
	}
}

func c4c1DockerHost() string {
	if host := os.Getenv("HARBORMASTER_DOCKER_HOST"); host != "" {
		return host
	}
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

// dockerRun executes one docker command, failing the test on error.
func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func dockerQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

// digestOf reads the manifest digest the registry recorded for a tag.
//
// From RepoDigests, which is where the daemon keeps what the REGISTRY said --
// not the image id. C3F established those are different identifiers, and the
// whole acquisition path is built on the manifest digest.
func digestOf(t *testing.T, reference string) string {
	t.Helper()

	raw := dockerRun(t, "inspect", "--format", "{{index .RepoDigests 0}}", reference)
	_, digest, found := strings.Cut(raw, "@")
	if !found {
		t.Fatalf("%s carries no registry digest: %q", reference, raw)
	}
	return digest
}

func imageIDOf(t *testing.T, reference string) string {
	t.Helper()
	return dockerRun(t, "inspect", "--format", "{{.Id}}", reference)
}

// ------------------------------------------------------- the event stream --

// dockerEvents returns the daemon's own account of what happened since a time.
//
// The primary evidence for every claim in this file. A database row says what
// HarborMaster INTENDED; this says what the daemon DID, and the two disagreeing
// is exactly the class of defect a modelled host cannot surface.
func dockerEvents(t *testing.T, since time.Time) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "events",
		"--since", since.UTC().Format(time.RFC3339),
		"--until", time.Now().UTC().Add(time.Second).Format(time.RFC3339),
		"--filter", "type=container",
		"--format", "{{.Action}} {{.Actor.Attributes.name}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker events: %v\n%s", err, out)
	}

	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			events = append(events, strings.TrimSpace(line))
		}
	}
	return events
}

// countEvents counts events whose action begins with a prefix.
func countEvents(events []string, action string) int {
	count := 0
	for _, event := range events {
		if strings.HasPrefix(event, action+" ") {
			count++
		}
	}
	return count
}

// assertOnlyDisposableContainersTouched is the safety boundary, proved.
//
// Every container event in the window must name something this file created.
// A row-count assertion cannot make this claim: it would pass just as well if
// HarborMaster had stopped an unrelated workload, because there would be no row.
func assertOnlyDisposableContainersTouched(t *testing.T, events []string) {
	t.Helper()

	// The actions that CHANGE a container. `create` and `destroy` included;
	// `die` and `health_status` are consequences rather than commands.
	mutating := map[string]bool{
		"create": true, "start": true, "stop": true, "kill": true,
		"rename": true, "destroy": true, "pause": true, "unpause": true,
		"restart": true, "update": true,
	}

	for _, event := range events {
		action, name, _ := strings.Cut(event, " ")
		if !mutating[action] {
			continue
		}
		if !strings.HasPrefix(name, "hm-c4c1-") {
			t.Errorf("the daemon reports %q on %q, which is not a disposable "+
				"workload.\n\nEvery event in the window:\n\t%s",
				action, name, strings.Join(events, "\n\t"))
		}
	}
}

// ---------------------------------------------------------------- the rig --

// realRig is the application over a real daemon.
type realRig struct {
	t    *testing.T
	path string

	db     *store.DB
	client *docker.Client

	// name is the disposable workload this rig is about.
	name string
	// currentRef, nextRef and their digests are the real images, in the
	// throwaway registry this file started.
	currentRef, nextRef       string
	currentDigest, nextDigest string
	currentImageID            string

	inventory    *service.InventoryService
	intel        *service.ImageIntelService
	policies     *service.PolicyService
	planner      *service.PlannerService
	acquisitions *service.AcquisitionService
	executions   *service.ExecutionService
	rollbacks    *service.RollbackService
	automation   *service.AutomationService
	cleanup      *service.ImageCleanupService

	notifier *recordingNotifier

	cancel  context.CancelFunc
	stopped chan struct{}

	// startedAt bounds the event window.
	startedAt time.Time

	// clockSkew is added to wall time by the rig's clock. The
	// maintenance-window scenario moves it; nothing else touches it. A skew
	// rather than a frozen clock, because every timeout in the pipeline is
	// computed from now() and a clock that never advances is a wait that never
	// ends.
	clockSkew atomic.Int64
}

// clock is what every service in this rig reads as the current time.
func (r *realRig) clock() time.Time {
	return time.Now().UTC().Add(time.Duration(r.clockSkew.Load()))
}

// advance moves the rig's clock forward.
func (r *realRig) advance(by time.Duration) { r.clockSkew.Add(int64(by)) }

// realRigOptions tunes one real-Docker rig.
type realRigOptions struct {
	name string
	// healthCheck is the command the disposable workload declares. The
	// failure scenario uses one that passes on the current image and fails on
	// the target; everything else uses one that passes on both.
	healthCheck string
	policies    []domain.UpdatePolicy
	labels      map[string]string
	// dbPath, when set, puts this rig's database at a fixed location instead of
	// a temporary one. Used only by the UI seeding run, so several scenarios
	// can accumulate their states into ONE database a running server can serve.
	dbPath string
	// keepWorkload leaves the disposable container in place at the end. Only
	// the UI seeding run wants this: the browser has to have something to look
	// at after the Go test that made it has finished.
	keepWorkload bool
}

func newRealRig(t *testing.T, tune func(*realRigOptions)) *realRig {
	t.Helper()
	skipUnlessRealDocker(t)

	var options realRigOptions
	tune(&options)
	if options.name == "" {
		t.Fatal("a real rig needs a disposable workload name")
	}
	if !strings.HasPrefix(options.name, "hm-c4c1-") {
		t.Fatalf("%q is not a disposable name", options.name)
	}

	path := options.dbPath
	if path == "" {
		path = filepath.Join(t.TempDir(), "harbormaster.db")
	}

	rig := &realRig{
		t:          t,
		path:       path,
		name:       options.name,
		notifier:   &recordingNotifier{},
		currentRef: c4c1CurrentRef,
		nextRef:    c4c1NextRef,
		startedAt:  time.Now().UTC().Add(-2 * time.Second),
	}
	rig.currentDigest = digestOf(t, rig.currentRef)
	rig.nextDigest = digestOf(t, rig.nextRef)
	rig.currentImageID = imageIDOf(t, rig.currentRef)

	health := options.healthCheck
	if health == "" {
		health = c4c1HealthyCheck
	}

	// The disposable workload. Created through the CLI, so nothing about
	// HarborMaster is involved in putting it there.
	//
	// Non-root, because the compliance policy this rig writes requires it and
	// the execution preflight refuses without a passing evaluation -- which is
	// correct, and is a gate the modelled rig could not exercise.
	dockerQuiet("rm", "-f", options.name)
	create := []string{
		"run", "-d", "--name", options.name,
		"--user", "65532:65532",
		"--health-cmd", health,
		"--health-interval", "1s",
		"--health-retries", "2",
		"--health-timeout", "2s",
		"--health-start-period", "1s",
	}
	for key, value := range options.labels {
		create = append(create, "--label", key+"="+value)
	}
	create = append(create, rig.currentRef, "sleep", "3600")
	dockerRun(t, create...)

	// Wait for the workload to be healthy before HarborMaster looks at it. A
	// container still in `starting` is not a baseline anything can be compared
	// against.
	deadlineAt := time.Now().Add(60 * time.Second)
	for healthOf(options.name) != "healthy" {
		if time.Now().After(deadlineAt) {
			t.Fatalf("%s never became healthy; status %q",
				options.name, healthOf(options.name))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The event window opens HERE, once the world is set up and before
	// HarborMaster has seen anything.
	//
	// Set at the end of construction rather than the start, because everything
	// before this line is the RIG's own doing: the `docker rm -f` that clears a
	// previous run, the `docker run` that creates the workload, and any cleanup
	// a preceding test was still finishing. Counting those as HarborMaster's
	// makes "exactly one create" mean two and makes "zero mutation" impossible
	// to state.
	rig.startedAt = time.Now().UTC()

	t.Cleanup(func() {
		rig.stop()
		if !options.keepWorkload {
			rig.cleanupDisposables()
		}
	})

	rig.open(options)
	return rig
}

// cleanupDisposables removes everything this rig made.
func (r *realRig) cleanupDisposables() {
	dockerQuiet("rm", "-f", r.name)
	// The parked original and the quarantined replacement, whose names are
	// derived from an execution id this rig does not hold onto. Matched by
	// prefix, which is safe because the prefix is this batch's own.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "name=hm-c4c1-", "--format", "{{.Names}}").Output()
	if err != nil {
		return
	}
	for _, name := range strings.Fields(string(out)) {
		if name == "hm-c4c1-registry" {
			continue
		}
		dockerQuiet("rm", "-f", name)
	}
}

func (r *realRig) now() time.Time { return time.Now().UTC() }

// open builds every service over the real daemon.
func (r *realRig) open(options realRigOptions) {
	r.t.Helper()

	db, err := store.Open(context.Background(), r.path)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.db = db

	client, err := docker.New(docker.Options{
		Host:    c4c1DockerHost(),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		r.t.Fatalf("connect to docker: %v", err)
	}
	r.client = client

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(filepath.Dir(r.path), "secret.key"),
	})
	if err != nil {
		r.t.Fatalf("load secret key: %v", err)
	}
	hasher := service.NewHasher(key)
	quiet := rigLogger()
	self := selfReporter{}

	clock := r.clock

	r.inventory = service.NewInventoryService(service.InventoryOptions{
		Runtime: client, Inventory: db.Inventory, Containers: db.Containers,
		Logger: quiet, Now: clock,
		Config: config.Inventory{Enabled: true, RefreshInterval: time.Hour, Workers: 4},
	})

	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: db.Containers, Snapshots: db.Snapshots, Inventory: db.Inventory,
		Hasher: hasher, Config: config.Snapshots{Enabled: true}, Logger: quiet,
	})
	assurance := service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
		Capturer: snapshots, Logger: quiet,
	})
	preparer := service.NewSnapshotPreparer(service.SnapshotPreparerOptions{
		Assurance: assurance, Policies: db.UpdatePolicies, Targets: db.Containers,
		Baselines: db.Snapshots, Self: self, Logger: quiet,
	})

	r.intel = service.NewImageIntelService(service.ImageIntelOptions{
		Lineage: db.Lineage, Reconciler: service.NewLineageReconciler(db.Lineage, quiet, nil),
		Store: db.ImageIntel,
		Registry: registry.New(registry.Options{
			Version: "c4c1-test", RequestTimeout: time.Second, MaxAttempts: 1,
		}),
		Config: config.ImageIntel{
			Enabled: true, RefreshInterval: time.Hour,
			MaxTrackedReferences: 500, MaxReferencesPerPass: 50,
			MaxConcurrentRequests: 1, RequestTimeout: time.Second, MaxAttempts: 1,
		},
		Notify: r.notifier, Logger: quiet,
	})

	r.policies = service.NewPolicyService(service.PolicyOptions{
		Definitions: db.Policies, Containers: db.Containers, Violations: db.Policies,
		Pruner: db.Policies, Inventory: db.Inventory,
		Config: config.Policy{Enabled: true, SweepInterval: time.Hour},
		Notify: r.notifier, Logger: quiet, Now: clock,
	})

	r.planner = service.NewPlannerService(service.PlannerOptions{
		Store: db.Plans, Lineage: db.Lineage, Prepare: preparer,
		Config: config.Planner{
			Enabled: true, Interval: time.Hour, BatchSize: 50,
			MaxContainers: 500, GenerationTimeout: time.Minute,
		},
		Notify: r.notifier, Logger: quiet, Now: clock,
	})

	r.acquisitions = service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    db.Acquisitions,
		Evidence: service.NewPlanEvidence(db.Plans, db.ImageIntel, db.Containers),
		Runtime:  client,
		// The REAL acquirer. Pulls happen over the wire, from the throwaway
		// registry, by immutable digest.
		Acquirer: client,
		Self:     self, Notify: r.notifier,
		Config: config.Acquisition{
			Enabled: true, RequireSnapshot: false,
			MaxConcurrent: 2, MaxPerRegistry: 2,
			PullTimeout: 2 * time.Minute, RequestTTL: time.Hour,
			RegistryFreshness: 24 * time.Hour,
			SweepInterval:     5 * time.Millisecond, MaxEventsPerAcquisition: 100,
		},
		Logger: quiet, Now: clock,
	})

	dependencies := service.NewDependencyService(service.DependencyOptions{
		Store: db.Dependencies, Lineage: service.NewDependencyLineage(db.Containers),
		Operations: db.DependencyOperations,
		Executions: service.NewDependencyExecutions(db.Executions),
		Logger:     quiet,
	})
	planApprovals := service.NewPlanApprovalService(service.PlanApprovalOptions{
		Store: db.PlanApprovals, Plans: db.Plans, Logger: quiet,
	})

	r.executions = service.NewExecutionService(service.ExecutionOptions{
		Lineage: db.Lineage, Store: db.Executions,
		Evidence: service.NewExecutionEvidence(
			db.Acquisitions, db.Plans, db.Containers,
			db.Snapshots, db.Policies, db.Inventory, db.ImageIntel),
		Runtime: client, Capturer: client, Mutator: client,
		Assurance: assurance, Approvals: planApprovals, Self: self,
		Dependencies: dependencies, Hasher: hasher, Notify: r.notifier,
		Config: config.Execution{
			Enabled: true, RequireSnapshot: true,
			StartupTimeout: 30 * time.Second, StabilityPeriod: 2 * time.Second,
			HealthPollInterval: 200 * time.Millisecond, StopTimeout: 10 * time.Second,
			MaxConcurrent: 1, RequestTTL: time.Hour,
			AcquisitionFreshness: time.Hour, InventoryFreshness: time.Hour,
			SweepInterval: 5 * time.Millisecond, MaxEventsPerExecution: 200,
		},
		Logger: quiet, Now: clock,
	})

	r.rollbacks = service.NewRollbackService(service.RollbackOptions{
		Lineage: db.Lineage, Store: db.Rollbacks,
		Evidence:   service.NewRollbackEvidence(db.Executions, db.Inventory),
		Runtime:    client,
		Rollbacker: client,
		Hasher:     hasher, Notify: r.notifier,
		Config: config.Rollback{
			Enabled: true, MaxConcurrent: 1, RequestTTL: time.Hour,
			StartupTimeout: 30 * time.Second, StabilityPeriod: 2 * time.Second,
			HealthPollInterval: 200 * time.Millisecond, StopTimeout: 10 * time.Second,
			InventoryFreshness: time.Hour,
			SweepInterval:      5 * time.Millisecond, MaxEventsPerRollback: 200,
		},
		Logger: quiet, Now: clock,
	})

	r.automation = service.NewAutomationService(service.AutomationOptions{
		Store: db.Automation, Policies: db.UpdatePolicies,
		Evidence: service.NewAutomationEvidence(
			db.Containers, db.Plans, db.Acquisitions, db.Executions,
			db.Lineage, db.ImageIntel),
		Pipeline:     service.NewAutomationPipeline(r.acquisitions, r.executions, r.rollbacks),
		Dependencies: dependencies, Notify: r.notifier, Self: self,
		Config: config.Automation{
			Enabled: true, Interval: time.Hour,
			FollowInterval: 10 * time.Millisecond,
			StartupDelay:   time.Hour, PruneInterval: time.Hour,
			PassTimeout: time.Minute, MaxConcurrent: 2, MaxPerRun: 10,
		},
		Logger: quiet, Now: clock,
	})

	// C4A cleanup, for Part 10. The capability is granted, so a pass that
	// decided wrongly would really remove an image.
	r.cleanup = service.NewImageCleanupService(service.ImageCleanupOptions{
		Store: db.ImageRetention, Runtime: client, Pruner: client,
		Self: self,
		Config: config.ImageCleanup{
			Enabled: true, MinAge: 14 * 24 * time.Hour,
			KeepGenerations: 1, Interval: time.Hour, MaxPerPass: 5,
		},
		Logger: quiet, Now: clock,
	})

	// A compliance policy the workload passes, so the execution preflight can
	// establish compliance. Without one it refuses -- correctly.
	if _, err := db.Policies.CreatePolicy(context.Background(), domain.PolicyDefinition{
		PolicyID: domain.NewPolicyID(), Name: "hm-c4c1 baseline",
		Severity: domain.PolicySeverityMedium, Enabled: true,
		Rules: []domain.PolicyRule{{Type: domain.RuleUserNotRoot}},
	}, r.now()); err != nil && !strings.Contains(err.Error(), "already") &&
		!strings.Contains(err.Error(), "taken") {
		r.t.Fatalf("write compliance policy: %v", err)
	}

	for _, policy := range options.policies {
		if _, err := db.UpdatePolicies.CreateUpdatePolicy(
			context.Background(), policy, r.now()); err != nil {
			r.t.Fatalf("write update policy: %v", err)
		}
	}
}

// start runs every scheduler loop.
func (r *realRig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.stopped = make(chan struct{})

	go func() {
		defer close(r.stopped)
		done := make(chan struct{}, 4)
		go func() { r.acquisitions.Run(ctx); done <- struct{}{} }()
		go func() { r.executions.Run(ctx); done <- struct{}{} }()
		go func() { r.rollbacks.Run(ctx); done <- struct{}{} }()
		go func() { r.automation.Run(ctx); done <- struct{}{} }()
		for i := 0; i < 4; i++ {
			<-done
		}
	}()
}

// startAcquisitionsOnly runs the acquisition worker and nothing else.
func (r *realRig) startAcquisitionsOnly() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.stopped = make(chan struct{})
	go func() { defer close(r.stopped); r.acquisitions.Run(ctx) }()
}

// startWithoutRollback runs everything except the rollback worker.
func (r *realRig) startWithoutRollback() {
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

func (r *realRig) stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.stopped
		r.cancel = nil
	}
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

// restart stops everything and rebuilds it over the same database file.
func (r *realRig) restart() {
	r.t.Helper()
	r.stop()
	r.open(realRigOptions{name: r.name})
	r.start()
}

// ------------------------------------------------------------- the world --

func (r *realRig) refreshInventory() {
	r.t.Helper()
	if _, err := r.inventory.Refresh(context.Background(), domain.TriggerManual); err != nil {
		r.t.Fatalf("inventory refresh: %v", err)
	}
}

func (r *realRig) syncIntel() {
	r.t.Helper()
	if _, err := r.intel.SyncInventory(context.Background()); err != nil {
		r.t.Fatalf("sync intel: %v", err)
	}
}

// publishUpdate seeds the registry ANSWER.
//
// The one thing about the outside world this rig fakes, and it has to: the
// alternative is HarborMaster's registry client talking to the throwaway
// registry, which is a different subsystem's test. The digest is REAL -- it is
// what the registry actually recorded for the pushed image -- so everything
// downstream, including the pull, is genuine.
func (r *realRig) publishUpdate(update domain.UpdateType, status domain.CheckStatus) {
	r.t.Helper()

	// The reference the PROJECTION recorded, which is the normalised form --
	// `docker.io/library/alpine:3.20.3`, not the familiar `alpine:3.20.3` a
	// person types. Seeding the familiar form writes a row nothing reads.
	reference := r.trackedReference()

	outcome := store.CheckOutcome{
		Reference: reference, Status: status,
		RemoteDigest: r.currentDigest, Update: update,
		LatestTag: c4c1NextTag, LatestDigest: r.nextDigest,
		Platform: domain.Platform{OS: "linux", Architecture: "amd64"},
	}
	if status != domain.CheckOK {
		outcome.RemoteDigest, outcome.LatestDigest = "", ""
		outcome.Update = domain.UpdateUnknown
		outcome.Detail = "the registry did not answer"
	}
	if err := r.db.ImageIntel.RecordCheck(context.Background(), outcome, r.now()); err != nil {
		r.t.Fatalf("record registry check: %v", err)
	}
}

// trackedReference is the CANONICAL reference the intelligence table keys on.
//
// Not the familiar `alpine:3.20.3` a person types and the inventory records --
// the projection normalises it, and a check recorded against the familiar form
// writes a row nothing ever reads. Established by inspecting the table, not by
// assumption: it holds `docker.io/library/alpine:3.20.3`.
func (r *realRig) trackedReference() string {
	return c4c1Registry + "/" + c4c1Repository + ":" + strings.TrimPrefix(c4c1CurrentRef, "alpine:")
}

// publishUpdateTo seeds a check naming a specific tag and digest.
//
// The superseded-plan scenario needs a FIRST plan pointing somewhere other than
// where the second one will point, so the two are genuinely different
// assessments rather than the same one written twice.
func (r *realRig) publishUpdateTo(
	update domain.UpdateType, status domain.CheckStatus, tag, digest string,
) {
	r.t.Helper()

	if err := r.db.ImageIntel.RecordCheck(context.Background(), store.CheckOutcome{
		Reference: r.trackedReference(), Status: status,
		RemoteDigest: r.currentDigest, Update: update,
		LatestTag: tag, LatestDigest: digest,
		Platform: domain.Platform{OS: "linux", Architecture: "amd64"},
	}, r.now()); err != nil {
		r.t.Fatalf("record registry check: %v", err)
	}
}

func (r *realRig) evaluateCompliance() {
	r.t.Helper()
	if _, err := r.policies.Sweep(context.Background()); err != nil {
		r.t.Fatalf("policy sweep: %v", err)
	}
}

func (r *realRig) plan() {
	r.t.Helper()
	if _, err := r.planner.Generate(context.Background()); err != nil {
		r.t.Fatalf("planner: %v", err)
	}
}

// seed walks the whole read-only half of the pipeline.
func (r *realRig) seed(update domain.UpdateType, status domain.CheckStatus) {
	r.t.Helper()
	r.refreshInventory()
	r.syncIntel()
	r.publishUpdate(update, status)
	r.evaluateCompliance()
	r.plan()
}

func (r *realRig) decide() (domain.AutomationRun, []domain.AutomationDecision) {
	r.t.Helper()
	run, decisions, err := r.automation.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		r.t.Fatalf("automation pass: %v", err)
	}
	return run, decisions
}

// mine returns the decision about THIS rig's workload.
//
// A real host carries every container the developer happens to have running, so
// a pass returns dozens of decisions and printing them all buries the one that
// matters. Every failure message in this file names this one.
func (r *realRig) mine(decisions []domain.AutomationDecision) string {
	for _, decision := range decisions {
		if decision.ContainerName == r.name {
			return "verdict=" + string(decision.Verdict) +
				" reason=" + string(decision.Reason) +
				" recommendation=" + string(decision.Recommendation) +
				"\n    detail=" + decision.Detail
		}
	}
	return "no decision was recorded for " + r.name
}

// planFactors renders why the planner reached the recommendation it did.
func (r *realRig) planFactors(containerID string) string {
	plan, err := r.db.Plans.Current(context.Background(), containerID)
	if err != nil {
		return "no current plan: " + err.Error()
	}
	out := "recommendation=" + string(plan.Risk.Recommendation) +
		" band=" + string(plan.Risk.Band) +
		" currentDigest=" + plan.CurrentDigest +
		" proposedDigest=" + plan.ProposedDigest
	for _, factor := range plan.Risk.Factors {
		out += "\n    " + string(factor.Rule) + " severity=" + string(factor.Severity) +
			" detail=" + factor.Detail
	}
	return out
}

// ------------------------------------------------------------- observing --

func (r *realRig) await(what string, condition func() bool) {
	r.t.Helper()

	deadline := time.After(3 * time.Minute)
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			r.t.Fatalf("timed out waiting for %s\n\nnotifications: %v\nevents:\n\t%s",
				what, eventsOf(r.notifier.all()),
				strings.Join(dockerEvents(r.t, r.startedAt), "\n\t"))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (r *realRig) awaitNoChange(what string, condition func() bool) {
	r.t.Helper()

	settle := time.After(3 * time.Second)
	for {
		if !condition() {
			r.t.Fatalf("%s stopped being true\n\nevents:\n\t%s",
				what, strings.Join(dockerEvents(r.t, r.startedAt), "\n\t"))
		}
		select {
		case <-settle:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (r *realRig) count(table string) int {
	r.t.Helper()
	var n int
	if err := r.db.SQL().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		r.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (r *realRig) terminalExecution() domain.Execution {
	r.t.Helper()
	executions, _, err := r.db.Executions.List(context.Background(),
		store.ExecutionFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		r.t.Fatalf("list executions: %v", err)
	}
	if len(executions) == 0 {
		r.t.Fatal("no execution was created")
	}
	return executions[0]
}

// containerID returns the id currently answering to a name, or "".
func containerID(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.Id}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runningImage returns the image reference a live container was created from.
func runningImage(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.Config.Image}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// healthOf reports the daemon's health verdict for a container.
func healthOf(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Health.Status}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isRunning(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Running}}", name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// c4c1Policy is the operator's automatic policy for one disposable workload.
func c4c1Policy(name string, mode domain.AutomationMode) domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "hm-c4c1 " + name,
		Enabled:               true,
		Priority:              10,
		Selector:              domain.UpdateSelector{Include: []string{name}},
		Strategy:              domain.StrategyMinor,
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  mode,
		Window:                domain.MaintenanceWindow{Start: "00:00", End: "23:59"},
		Failure:               domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()
	return policy
}
