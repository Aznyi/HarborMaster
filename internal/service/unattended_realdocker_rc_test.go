package service_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Release qualification of the recreation transaction against a REAL daemon.
//
// The happy path proves little. Every scenario below breaks one destructive
// transition on purpose -- a create that fails, a start that fails, a health
// check that never passes, a restart-policy write the daemon refuses, a process
// that dies between two checkpoints -- and then asks the daemon, not the
// database, which container is serving afterwards.
//
// # Fault injection
//
// Every Docker call reaches the daemon through the real docker.Client. The
// wrappers below forward everything and intercept ONE named operation per
// scenario: the create returns an error the daemon never saw, the start is
// never issued, a restart-policy update is refused. What the daemon then holds
// is exactly what it would hold if it had refused the call itself, which is the
// point: the recovery paths are exercised against real containers in real
// states.
//
// Off unless HARBORMASTER_DOCKER_INTEGRATION=1, like the rest of the rig.

// ------------------------------------------------------------- faults --

// hold stalls the pipeline at one step so the test can kill HarborMaster there.
type hold struct {
	step    string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHold(step string) *hold {
	return &hold{step: step, reached: make(chan struct{}), release: make(chan struct{})}
}

var errCrashed = errors.New("harbormaster died here")

// faults is shared by the wrappers a rig builds, across restarts.
type faults struct {
	mu sync.Mutex
	// createErr is returned instead of creating anything.
	createErr error
	// createLate makes the daemon "finish" the create this long after the
	// error was returned -- the client gave up, the daemon did not.
	createLate time.Duration
	// squatOnCreate puts a stranger on the production name instead of a
	// replacement, and reports a name conflict.
	squatOnCreate bool
	// startErr is returned instead of starting the replacement.
	startErr error
	// suspendErrMarker makes the recreation's restart-policy suspension fail
	// for a container whose current name carries this marker.
	suspendErrMarker string
	// rollbackSuspendErr makes the rollback's suspension of the parked
	// replacement fail.
	rollbackSuspendErr error
	// hold stalls one step until released; releasing returns errCrashed.
	hold *hold
}

func (f *faults) pause(step string) error {
	f.mu.Lock()
	h := f.hold
	f.mu.Unlock()
	if h == nil || h.step != step {
		return nil
	}
	h.once.Do(func() { close(h.reached) })
	<-h.release
	return errCrashed
}

type faultMutator struct {
	*docker.Client
	f *faults
}

func (m *faultMutator) RenameContainer(ctx context.Context, request docker.RenameRequest) error {
	if err := m.f.pause("rename"); err != nil {
		return err
	}
	return m.Client.RenameContainer(ctx, request)
}

func (m *faultMutator) CreateContainer(ctx context.Context, request docker.CreateRequest) (string, error) {
	if err := m.f.pause("create"); err != nil {
		return "", err
	}
	m.f.mu.Lock()
	createErr, late, squat := m.f.createErr, m.f.createLate, m.f.squatOnCreate
	m.f.mu.Unlock()
	if squat {
		dockerQuiet("run", "-d", "--name", request.Name, "--restart", "always",
			"--label", "io.hm-c4c1.stranger=true", c4c1CurrentRef, "sleep", "3600")
		return "", docker.ErrNameConflict
	}
	if createErr != nil {
		if late > 0 {
			go func() {
				time.Sleep(late)
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_, _ = m.Client.CreateContainer(ctx, request)
			}()
		}
		return "", createErr
	}
	id, err := m.Client.CreateContainer(ctx, request)
	if err != nil {
		return "", err
	}
	if err := m.f.pause("createDone"); err != nil {
		return "", err
	}
	return id, nil
}

func (m *faultMutator) StartContainer(ctx context.Context, request docker.StartRequest) error {
	if err := m.f.pause("start"); err != nil {
		return err
	}
	m.f.mu.Lock()
	startErr := m.f.startErr
	m.f.mu.Unlock()
	if startErr != nil {
		return startErr
	}
	return m.Client.StartContainer(ctx, request)
}

func (m *faultMutator) RemoveContainer(ctx context.Context, request docker.RemoveRequest) error {
	if err := m.f.pause("remove"); err != nil {
		return err
	}
	return m.Client.RemoveContainer(ctx, request)
}

func (m *faultMutator) SuspendRestart(ctx context.Context, request docker.SuspendRestartRequest) error {
	m.f.mu.Lock()
	marker := m.f.suspendErrMarker
	m.f.mu.Unlock()
	if marker != "" {
		inspection, err := m.InspectContainer(ctx, request.ContainerID)
		if err == nil && inspection != nil && strings.Contains(inspection.Detail.Overview.Name, marker) {
			return errors.New("the daemon refused the restart policy update")
		}
	}
	return m.Client.SuspendRestart(ctx, request)
}

type faultRollbacker struct {
	*docker.Client
	f *faults
}

func (r *faultRollbacker) SuspendRestart(ctx context.Context, request docker.SuspendRestartRequest) error {
	r.f.mu.Lock()
	suspendErr := r.f.rollbackSuspendErr
	r.f.mu.Unlock()
	if suspendErr != nil {
		return suspendErr
	}
	return r.Client.SuspendRestart(ctx, request)
}

func withFaults(f *faults) func(*realRigOptions) {
	return func(o *realRigOptions) {
		o.mutator = func(c *docker.Client) docker.ContainerMutator { return &faultMutator{Client: c, f: f} }
		o.rollbacker = func(c *docker.Client) docker.ContainerRollbacker { return &faultRollbacker{Client: c, f: f} }
	}
}

// ------------------------------------------------------ the manual path --

var rcOperator = domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"}

// requestManualUpdate walks the path an operator's clicks take: a snapshot,
// a plan, an acquisition request, an execution request. The schedulers do the
// rest. Returns the refusal, if the execution preflight said no.
func requestManualUpdate(t *testing.T, rig *realRig) (domain.Execution, error) {
	t.Helper()
	ctx := context.Background()

	rig.refreshInventory()
	originalID := containerID(rig.name)
	if originalID == "" {
		t.Fatal("the disposable workload is not on the host")
	}
	if result := rig.assurance.EnsureCurrent(ctx, originalID, domain.SnapshotTriggerManual); !result.Usable() {
		t.Fatalf("snapshot assurance: %+v", result)
	}
	rig.seed(domain.UpdateMinor, domain.CheckOK)

	plan, err := rig.db.Plans.Current(ctx, originalID)
	if err != nil {
		t.Fatalf("no current plan: %v", err)
	}
	acquisition, err := rig.acquisitions.Request(ctx, service.AcquisitionRequest{
		PlanID: plan.PlanID, RequestedBy: rcOperator,
	})
	if err != nil {
		t.Fatalf("acquisition refused: %v\n\n  plan: %s", err, rig.planFactors(originalID))
	}
	rig.await("the acquisition to succeed", func() bool {
		current, err := rig.db.Acquisitions.Get(ctx, acquisition.AcquisitionID)
		return err == nil && current.State == domain.AcquisitionSucceeded
	})
	rig.refreshInventory()
	rig.evaluateCompliance()

	return rig.executions.Request(ctx, service.ExecutionRequest{
		AcquisitionID: acquisition.AcquisitionID, RequestedBy: rcOperator,
	})
}

func mustRequestManualUpdate(t *testing.T, rig *realRig) domain.Execution {
	t.Helper()
	execution, err := requestManualUpdate(t, rig)
	if err != nil {
		t.Fatalf("execution refused: %v", err)
	}
	return execution
}

// --------------------------------------------------------- observing --

func inspectFormat(name, format string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func restartPolicyOf(name string) string {
	return inspectFormat(name, "{{.HostConfig.RestartPolicy.Name}}")
}

// disposablesContaining lists hm-c4c1 containers whose name carries a marker.
func disposablesContaining(t *testing.T, marker string) []string {
	t.Helper()
	var out []string
	for _, name := range disposableContainers(t) {
		if strings.Contains(name, marker) {
			out = append(out, name)
		}
	}
	return out
}

func awaitTerminalExecution(rig *realRig) domain.Execution {
	rig.t.Helper()
	rig.await("the recreation to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	return rig.terminalExecution()
}

func awaitRealRestoreSettled(rig *realRig) domain.Execution {
	rig.t.Helper()
	rig.await("the restore to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 &&
			executions[0].State.Terminal() && executions[0].Restore.State.Settled()
	})
	return rig.terminalExecution()
}

func awaitTerminalRollback(rig *realRig) domain.Rollback {
	rig.t.Helper()
	var rollback domain.Rollback
	rig.await("the rollback to settle", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 10}})
		if err != nil || len(rollbacks) != 1 || !rollbacks[0].State.Terminal() {
			return false
		}
		rollback = rollbacks[0]
		return true
	})
	return rollback
}

func awaitHealthy(t *testing.T, name string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for healthOf(name) != "healthy" {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become healthy within %s; status %q", name, within, healthOf(name))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertOriginalServing is the question every failure scenario ends on.
func assertOriginalServing(t *testing.T, rig *realRig, originalID, policy string) {
	t.Helper()
	if got := containerID(rig.name); got != originalID {
		t.Fatalf("%q is held by %s, want the original %s\n\nhost: %v",
			rig.name, domain.ShortenID(got), domain.ShortenID(originalID), disposableContainers(t))
	}
	if !isRunning(rig.name) {
		t.Fatalf("the original is not running under %q", rig.name)
	}
	awaitHealthy(t, rig.name, 2*time.Minute)
	if got := restartPolicyOf(rig.name); got != policy {
		t.Errorf("the restored original has restart policy %q, want %q", got, policy)
	}
}

// planMentionsRestart reports whether a recovery plan tells the operator
// about a container that could start by itself.
func planMentionsRestart(plan *domain.RecoveryPlan) bool {
	if plan == nil {
		return false
	}
	for _, step := range plan.Steps {
		text := strings.ToLower(step.Description + " " + step.Command)
		if strings.Contains(text, "--restart=no") || strings.Contains(text, "restart") {
			return true
		}
	}
	return strings.Contains(strings.ToLower(plan.Situation), "restart")
}

// dockerEventsSince is dockerEvents with a nanosecond window. The rig's helper
// formats its bound to the second, which is right for a window that opens
// before HarborMaster does anything and wrong for one that opens the instant
// after the test's own `docker run`: the same second would carry both.
func dockerEventsSince(t *testing.T, since time.Time) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "events",
		"--since", since.UTC().Format(time.RFC3339Nano),
		"--until", time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
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

func mutatingEventsOn(events []string, name string) []string {
	var out []string
	for _, event := range events {
		action, actor, _ := strings.Cut(event, " ")
		if actor != name {
			continue
		}
		switch action {
		case "create", "start", "stop", "kill", "rename", "destroy", "pause", "unpause", "restart", "update":
			out = append(out, event)
		}
	}
	return out
}

// ------------------------------------ Scenario 1: successful replacement --

func TestRealDockerRCASuccessfulReplacementPreservesEverything(t *testing.T) {
	skipUnlessRealDocker(t)
	const (
		name    = "hm-c4c1-rc-success"
		network = "hm-c4c1-rc-net"
		volume  = "hm-c4c1-rc-vol"
	)
	bind := t.TempDir()
	dockerQuiet("network", "rm", network)
	dockerQuiet("volume", "rm", volume)
	dockerRun(t, "network", "create", network)
	dockerRun(t, "volume", "create", volume)
	// Registered before the rig so it runs after the rig's own cleanup.
	t.Cleanup(func() {
		dockerQuiet("network", "rm", network)
		dockerQuiet("volume", "rm", volume)
	})

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{
			"--restart", "always",
			"-p", "127.0.0.1:18631:8080",
			"-v", volume + ":/data",
			"-v", bind + ":/bind",
			"-e", "HM_RC=yes",
			"--label", "io.hm-c4c1.rc=1",
			"--network", network,
		}
	})
	originalID := containerID(name)
	rig.start()
	execution := mustRequestManualUpdate(t, rig)

	rig.await("the recreation to settle and the original to be removed", func() bool {
		current, err := rig.db.Executions.Get(context.Background(), execution.ExecutionID)
		return err == nil && current.State.Terminal() && (current.State != domain.ExecutionSucceeded || current.OriginalRemoved)
	})
	final := rig.terminalExecution()
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("the recreation ended %q (failure %q, checkpoint %q, refusal %q): %s",
			final.State, final.Failure, final.Checkpoint, final.Refusal, final.Message)
	}

	// ---- the daemon's account ----
	assertOnlyDisposableContainersTouched(t, dockerEvents(t, rig.startedAt))

	// ---- the host afterwards ----
	replacementID := containerID(name)
	if replacementID == "" || replacementID == originalID || replacementID != final.ReplacementID {
		t.Fatalf("%q is held by %s; record names %s; original was %s",
			name, domain.ShortenID(replacementID), domain.ShortenID(final.ReplacementID), domain.ShortenID(originalID))
	}
	if !isRunning(name) {
		t.Error("the replacement is not running")
	}
	awaitHealthy(t, name, time.Minute)
	if got := runningImage(name); !strings.Contains(got, rig.nextDigest) {
		t.Errorf("the replacement runs %q, want the approved digest %q", got, rig.nextDigest)
	}
	if got := restartPolicyOf(name); got != "always" {
		t.Errorf("restart policy %q, want always", got)
	}
	mounts := inspectFormat(name, `{{range .Mounts}}{{.Type}}:{{.Name}}:{{.Source}}->{{.Destination}};{{end}}`)
	if !strings.Contains(mounts, "volume:"+volume+":") || !strings.Contains(mounts, "->/data;") {
		t.Errorf("the named volume was not preserved: %s", mounts)
	}
	if !strings.Contains(mounts, "->/bind;") || !strings.Contains(mounts, "bind::") {
		t.Errorf("the bind mount was not preserved: %s", mounts)
	}
	if ports := inspectFormat(name, `{{json .HostConfig.PortBindings}}`); !strings.Contains(ports, "18631") {
		t.Errorf("the published port was not preserved: %s", ports)
	}
	if networks := inspectFormat(name, `{{json .NetworkSettings.Networks}}`); !strings.Contains(networks, `"`+network+`"`) {
		t.Errorf("the user-defined network was not preserved: %s", networks)
	}
	if env := inspectFormat(name, `{{json .Config.Env}}`); !strings.Contains(env, "HM_RC=yes") {
		t.Errorf("the environment was not preserved: %s", env)
	}
	if label := inspectFormat(name, `{{index .Config.Labels "io.hm-c4c1.rc"}}`); label != "1" {
		t.Errorf("the custom label was not preserved: %q", label)
	}
	if containerID(originalID) != "" {
		t.Error("the parked original is still on the host after a settled update")
	}
	if leftovers := disposablesContaining(t, domain.ParkedNameSuffix); len(leftovers) != 0 {
		t.Errorf("parked originals left behind: %v", leftovers)
	}
	if leftovers := disposablesContaining(t, domain.QuarantineNameSuffix); len(leftovers) != 0 {
		t.Errorf("quarantines left behind: %v", leftovers)
	}

	// ---- HarborMaster's own identity records ----
	rig.refreshInventory()
	ctx := context.Background()
	current, err := rig.db.Containers.Get(ctx, replacementID)
	if err != nil || current == nil || !current.Overview.Present {
		t.Fatalf("the container repository does not report the replacement as present: %v", err)
	}
	lineage, err := rig.db.Lineage.Get(ctx, name)
	if err != nil || lineage.ContainerID != replacementID {
		t.Errorf("lineage observes %s (err %v), want the replacement %s",
			domain.ShortenID(lineage.ContainerID), err, domain.ShortenID(replacementID))
	}
	if resolved, err := rig.db.Executions.ReplacementFor(ctx, originalID); err != nil || resolved != replacementID {
		t.Errorf("ReplacementFor(original) = %q, %v; want %q", resolved, err, replacementID)
	}
	if got := rig.count("rollbacks"); got != 0 {
		t.Errorf("rollbacks = %d, want 0", got)
	}
}

// ---------------------------- Scenario 2: create fails after the park --

func TestRealDockerRCACreateFailureIsRestoredWithoutAClick(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-create"
	f := &faults{createErr: docker.ErrMutationFailed}
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--restart", "always"}
		withFaults(f)(o)
	})
	originalID := containerID(name)
	rig.start()
	sinceRequest := time.Now().UTC()
	mustRequestManualUpdate(t, rig)

	execution := awaitRealRestoreSettled(rig)
	if execution.State != domain.ExecutionFailed || execution.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("the update ended %q/%q, want failed/create", execution.State, execution.Failure)
	}
	if execution.ReplacementID != "" {
		t.Errorf("a replacement %q is recorded for a create that failed", execution.ReplacementID)
	}
	if execution.Restore.State != domain.RestoreRestored {
		t.Fatalf("restore = %+v, want restored\n\nplan: %+v", execution.Restore, execution.Recovery)
	}
	if execution.Recovery == nil || execution.Recovery.ServiceInterrupted {
		t.Errorf("the record still says the service is down: %+v", execution.Recovery)
	}
	rollback := awaitTerminalRollback(rig)
	if rollback.State != domain.RollbackSucceeded || !rollback.ManualRestore() || rollback.RollbackID != execution.Restore.RollbackID {
		t.Errorf("rollback %s ended %q manualRestore=%v; the update names %s",
			rollback.RollbackID, rollback.State, rollback.ManualRestore(), execution.Restore.RollbackID)
	}
	if got := countEvents(dockerEvents(t, sinceRequest), "create"); got != 0 {
		t.Errorf("the daemon created %d containers; the create was supposed to fail", got)
	}
	assertOnlyDisposableContainersTouched(t, dockerEvents(t, rig.startedAt))
	assertOriginalServing(t, rig, originalID, "always")
	if leftovers := disposablesContaining(t, domain.ParkedNameSuffix); len(leftovers) != 0 {
		t.Errorf("the original is still parked somewhere: %v", leftovers)
	}
}

// ---------------------------------------------- Scenario 3: start fails --

func TestRealDockerRCAStartFailureQuarantinesAndRestores(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-start"
	f := &faults{startErr: docker.ErrMutationFailed}
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--restart", "always"}
		withFaults(f)(o)
	})
	originalID := containerID(name)
	rig.start()
	mustRequestManualUpdate(t, rig)

	execution := awaitRealRestoreSettled(rig)
	if execution.State != domain.ExecutionFailed || execution.Failure != domain.ExecutionFailureStart {
		t.Fatalf("the update ended %q/%q, want failed/start", execution.State, execution.Failure)
	}
	if execution.Checkpoint != domain.CheckpointReplacementQuarantined || execution.QuarantineName == "" {
		t.Errorf("checkpoint %q quarantine %q, want replacementQuarantined with a name",
			execution.Checkpoint, execution.QuarantineName)
	}
	if execution.Restore.State != domain.RestoreRestored {
		t.Fatalf("restore = %+v, want restored", execution.Restore)
	}
	rollback := awaitTerminalRollback(rig)
	if rollback.State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q/%q", rollback.State, rollback.Failure)
	}
	// The failed replacement is kept, stopped, unable to restart by itself.
	if rollback.ReplacementID != execution.ReplacementID || rollback.ReplacementParkedName == "" {
		t.Fatalf("the rollback moved %s to %q; the update created %s",
			domain.ShortenID(rollback.ReplacementID), rollback.ReplacementParkedName, domain.ShortenID(execution.ReplacementID))
	}
	if isRunning(rollback.ReplacementParkedName) {
		t.Error("the failed replacement is running")
	}
	if got := restartPolicyOf(rollback.ReplacementParkedName); got != "no" {
		t.Errorf("the failed replacement has restart policy %q, want no", got)
	}
	assertOriginalServing(t, rig, originalID, "always")
	assertOnlyDisposableContainersTouched(t, dockerEvents(t, rig.startedAt))
}

// --------------------------------------------- Scenario 4: health fails --

func TestRealDockerRCAnUnhealthyReplacementIsRestoredAndSaidSo(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-health"
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.healthCheck = c4c1VersionCheck
		o.runArgs = []string{"--restart", "always"}
	})
	originalID := containerID(name)
	rig.start()
	mustRequestManualUpdate(t, rig)

	execution := awaitRealRestoreSettled(rig)
	if execution.State != domain.ExecutionFailed {
		t.Fatalf("the update ended %q, want failed", execution.State)
	}
	if execution.Failure != domain.ExecutionFailureUnhealthy && execution.Failure != domain.ExecutionFailureHealthTimeout {
		t.Errorf("failure %q, want unhealthy or healthTimeout", execution.Failure)
	}
	if execution.Restore.State != domain.RestoreRestored {
		t.Fatalf("restore = %+v, want restored", execution.Restore)
	}
	// The API and UI read exactly these three fields.
	if execution.Recovery == nil || execution.Recovery.ServiceInterrupted || !execution.Restore.State.ServiceRestored() {
		t.Errorf("the record does not say the original is serving again: recovery=%+v restore=%+v",
			execution.Recovery, execution.Restore)
	}
	rollback := awaitTerminalRollback(rig)
	if rollback.State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q/%q", rollback.State, rollback.Failure)
	}
	if isRunning(rollback.ReplacementParkedName) || restartPolicyOf(rollback.ReplacementParkedName) != "no" {
		t.Errorf("the unhealthy replacement %q is running=%v policy=%q", rollback.ReplacementParkedName,
			isRunning(rollback.ReplacementParkedName), restartPolicyOf(rollback.ReplacementParkedName))
	}
	assertOriginalServing(t, rig, originalID, "always")

	rig.await("the recovered notification", func() bool {
		return len(notificationsFor2(rig, domain.EventUpdateRecovered)) > 0
	})
	assertOneLogical(t, rig, domain.EventExecutionFailed)
	assertOneLogical(t, rig, domain.EventUpdateRecovered)
	assertNone(t, rig, domain.EventExecutionSucceeded, domain.EventRollbackStarted, domain.EventRollbackSucceeded)
}

// ------------------------------- Scenario 5: a long legitimate start --

// rcSlowHealth passes on the ninth probe and every one after it.
const rcSlowHealth = "n=$(cat /tmp/hm-n 2>/dev/null || echo 0); n=$((n+1)); echo $n > /tmp/hm-n; [ $n -gt 8 ]"

func TestRealDockerRCALongStartInsideTheHealthcheckBudgetSucceeds(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-slow"
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.healthCheck = rcSlowHealth
		o.healthArgs = []string{
			"--health-interval", "3s", "--health-retries", "3",
			"--health-timeout", "2s", "--health-start-period", "90s",
		}
		o.healthyWait = 3 * time.Minute
		// A startup timeout the container cannot meet: healthy needs nine
		// probes at 3s. The healthcheck-derived deadline (90s + 3 × 5s) is
		// what must carry it.
		o.startupTimeout = 10 * time.Second
		o.maxHealthWait = 5 * time.Minute
	})
	rig.start()
	started := time.Now()
	mustRequestManualUpdate(t, rig)
	execution := awaitTerminalExecution(rig)
	if execution.State != domain.ExecutionSucceeded {
		t.Fatalf("the update ended %q/%q after %s; a legitimate slow start was refused",
			execution.State, execution.Failure, time.Since(started))
	}
	if healthOf(name) != "healthy" || containerID(name) != execution.ReplacementID {
		t.Errorf("the replacement is not serving healthy: %q", healthOf(name))
	}
}

func TestRealDockerRCTheHealthWaitCapStillBoundsAPathologicalStart(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-capped"
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.healthCheck = rcSlowHealth
		o.healthArgs = []string{
			"--health-interval", "3s", "--health-retries", "3",
			"--health-timeout", "2s", "--health-start-period", "90s",
		}
		o.healthyWait = 3 * time.Minute
		o.runArgs = []string{"--restart", "always"}
		o.startupTimeout = 10 * time.Second
		// The cap: below what the healthcheck would allow, so the wait ends
		// here and the update fails rather than waiting out the start period.
		o.maxHealthWait = 15 * time.Second
	})
	originalID := containerID(name)
	rig.start()
	requested := time.Now()
	mustRequestManualUpdate(t, rig)
	execution := awaitRealRestoreSettled(rig)
	if execution.State != domain.ExecutionFailed || execution.Failure != domain.ExecutionFailureHealthTimeout {
		t.Fatalf("the update ended %q/%q, want failed/healthTimeout", execution.State, execution.Failure)
	}
	if waited := time.Since(requested); waited > 90*time.Second {
		t.Errorf("the update took %s to give up; the cap did not bound the wait", waited)
	}
	if execution.Restore.State != domain.RestoreRestored {
		t.Fatalf("restore = %+v, want restored", execution.Restore)
	}
	assertOriginalServing(t, rig, originalID, "always")
}

// ------------------- Scenario 6: the create lands but is never recorded --

func TestRealDockerRCALateCreateIsAdoptedOnEvidenceAndRolledBackByID(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-adopt"
	f := &faults{createErr: docker.ErrMutationFailed, createLate: 2 * time.Second}
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--restart", "always"}
		o.restoreOff = true
		withFaults(f)(o)
	})
	originalID := containerID(name)
	rig.start()
	mustRequestManualUpdate(t, rig)
	execution := awaitTerminalExecution(rig)
	if execution.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("failure %q, want create", execution.Failure)
	}

	// The daemon finishes the create the client gave up on.
	var lateID string
	rig.await("the late create to land", func() bool {
		lateID = containerID(name)
		return lateID != "" && lateID != originalID
	})
	ctx := context.Background()
	for label, want := range map[string]string{
		"io.harbormaster.execution": execution.ExecutionID,
		"io.harbormaster.original":  originalID,
	} {
		if got := inspectFormat(name, `{{index .Config.Labels "`+label+`"}}`); got != want {
			t.Errorf("the late replacement carries %s=%q, want %q", label, got, want)
		}
	}

	rig.executions.Reconcile(ctx)
	adopted, err := rig.db.Executions.Get(ctx, execution.ExecutionID)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if adopted.ReplacementID != lateID || adopted.Checkpoint != domain.CheckpointReplacementCreated {
		t.Fatalf("after reconciliation replacement=%s checkpoint=%q; the late create is %s",
			domain.ShortenID(adopted.ReplacementID), adopted.Checkpoint, domain.ShortenID(lateID))
	}
	if adopted.State != domain.ExecutionFailed || adopted.Failure != domain.ExecutionFailureCreate {
		t.Errorf("adoption changed the settled outcome to %q/%q", adopted.State, adopted.Failure)
	}
	events := rig.count("execution_events")
	rig.executions.Reconcile(ctx)
	rig.executions.Reconcile(ctx)
	again, _ := rig.db.Executions.Get(ctx, execution.ExecutionID)
	if again.ReplacementID != adopted.ReplacementID || again.Checkpoint != adopted.Checkpoint ||
		rig.count("execution_events") != events {
		t.Errorf("repeated reconciliation kept writing: %+v vs %+v (%d events, had %d)",
			again, adopted, rig.count("execution_events"), events)
	}

	// The rollback moves the container the record now names, by id.
	if _, err := rig.rollbacks.Request(ctx, service.RollbackRequest{
		ExecutionID: execution.ExecutionID, RequestedBy: rcOperator,
	}); err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	rollback := awaitTerminalRollback(rig)
	if rollback.State != domain.RollbackSucceeded || rollback.ReplacementID != lateID {
		t.Fatalf("the rollback ended %q/%q on replacement %s, want succeeded on %s",
			rollback.State, rollback.Failure, domain.ShortenID(rollback.ReplacementID), domain.ShortenID(lateID))
	}
	if isRunning(rollback.ReplacementParkedName) || restartPolicyOf(rollback.ReplacementParkedName) != "no" {
		t.Errorf("the adopted replacement is running=%v policy=%q after the rollback",
			isRunning(rollback.ReplacementParkedName), restartPolicyOf(rollback.ReplacementParkedName))
	}
	assertOriginalServing(t, rig, originalID, "always")
	assertOnlyDisposableContainersTouched(t, dockerEvents(t, rig.startedAt))
}

// ---------------------- Scenario 7: a stranger holds the production name --

func TestRealDockerRCAStrangerOnTheProductionNameIsNeverTouched(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-stranger"
	f := &faults{createErr: docker.ErrMutationFailed}
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--restart", "always"}
		o.restoreOff = true
		withFaults(f)(o)
	})
	originalID := containerID(name)
	rig.start()
	mustRequestManualUpdate(t, rig)
	execution := awaitTerminalExecution(rig)
	if execution.Failure != domain.ExecutionFailureCreate || execution.Checkpoint != domain.CheckpointOriginalParked {
		t.Fatalf("failure %q checkpoint %q, want create/originalParked", execution.Failure, execution.Checkpoint)
	}
	ctx := context.Background()

	variants := []struct {
		label string
		args  []string
	}{
		{"name only", nil},
		{"forged execution label only", []string{"--label", "io.harbormaster.execution=" + execution.ExecutionID}},
		{"wrong original lineage", []string{
			"--label", "io.harbormaster.execution=" + execution.ExecutionID,
			"--label", "io.harbormaster.original=" + strings.Repeat("d", 64)}},
		{"wrong image", []string{
			"--label", "io.harbormaster.execution=" + execution.ExecutionID,
			"--label", "io.harbormaster.original=" + originalID}},
		{"stale execution id", []string{
			"--label", "io.harbormaster.execution=exec_0123456789abcdef0123",
			"--label", "io.harbormaster.original=" + originalID}},
	}
	for _, variant := range variants {
		t.Run(variant.label, func(t *testing.T) {
			args := append([]string{"run", "-d", "--name", name, "--restart", "always"}, variant.args...)
			// Every variant runs the CURRENT image: even "wrong image" carries
			// both labels correctly and fails on the image alone.
			args = append(args, c4c1CurrentRef, "sleep", "3600")
			strangerID := dockerRun(t, args...)
			t.Cleanup(func() { dockerQuiet("rm", "-f", strangerID) })
			mark := time.Now().UTC()

			rig.executions.Reconcile(ctx)
			rig.executions.AdvanceRestores(ctx)
			if _, err := rig.rollbacks.Request(ctx, service.RollbackRequest{
				ExecutionID: execution.ExecutionID, RequestedBy: rcOperator,
				RequestKey: "stranger-" + strings.ReplaceAll(variant.label, " ", "-"),
			}); err == nil {
				t.Error("a rollback was accepted with a stranger on the production name")
			} else {
				var refused service.RollbackRefusedError
				if !errors.As(err, &refused) {
					t.Errorf("rollback failed with %v, want a refusal", err)
				}
			}
			time.Sleep(time.Second)

			record, _ := rig.db.Executions.Get(ctx, execution.ExecutionID)
			if record.ReplacementID != "" {
				t.Fatalf("the stranger %s was adopted", domain.ShortenID(record.ReplacementID))
			}
			if got := containerID(name); got != strangerID {
				t.Fatalf("%q is held by %s, the stranger was %s", name, domain.ShortenID(got), domain.ShortenID(strangerID))
			}
			if !isRunning(name) || restartPolicyOf(name) != "always" {
				t.Errorf("the stranger was touched: running=%v policy=%q", isRunning(name), restartPolicyOf(name))
			}
			if touched := mutatingEventsOn(dockerEventsSince(t, mark), name); len(touched) != 0 {
				t.Errorf("the daemon reports HarborMaster acting on the stranger: %v", touched)
			}
			dockerRun(t, "rm", "-f", strangerID)
		})
	}
	// The original is exactly where the failure left it.
	if got := containerID(execution.ParkedName); got != originalID || isRunning(execution.ParkedName) {
		t.Errorf("the parked original is %s running=%v", domain.ShortenID(got), isRunning(execution.ParkedName))
	}
}

// The automatic restore, refused: a stranger on the name is exactly the host
// arrangement the rollback preflight must not undo by guessing.
func TestRealDockerRCAStrangerOnTheNameRefusesTheAutomaticRestore(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-squat"
	f := &faults{squatOnCreate: true}
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--restart", "always"}
		withFaults(f)(o)
	})
	originalID := containerID(name)
	rig.start()
	mustRequestManualUpdate(t, rig)
	execution := awaitRealRestoreSettled(rig)
	if execution.Restore.State != domain.RestoreRefused {
		t.Fatalf("restore = %+v, want refused", execution.Restore)
	}
	if execution.Recovery == nil || !execution.Recovery.ServiceInterrupted {
		t.Errorf("the record does not say the service is down: %+v", execution.Recovery)
	}
	if execution.ReplacementID != "" {
		t.Errorf("the stranger was adopted as %s", domain.ShortenID(execution.ReplacementID))
	}
	if !isRunning(name) || restartPolicyOf(name) != "always" ||
		inspectFormat(name, `{{index .Config.Labels "io.hm-c4c1.stranger"}}`) != "true" {
		t.Errorf("the stranger was touched: running=%v policy=%q", isRunning(name), restartPolicyOf(name))
	}
	if got := containerID(execution.ParkedName); got != originalID || isRunning(execution.ParkedName) {
		t.Errorf("the parked original is %s running=%v", domain.ShortenID(got), isRunning(execution.ParkedName))
	}
	if got := rig.count("rollbacks"); got != 0 {
		t.Errorf("rollbacks = %d; a refused preflight must write no rollback record", got)
	}
}

// ---------- Scenarios 8, 9, 13: a daemon restart with parked and quarantined --

// dockerEngineRestart restarts Docker Desktop's engine and waits for it.
func dockerEngineRestart(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "desktop", "restart").CombinedOutput(); err != nil {
		t.Fatalf("docker desktop restart: %v\n%s", err, out)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		probe, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
		err := exec.CommandContext(probe, "docker", "version", "--format", "{{.Server.Version}}").Run()
		cancelProbe()
		if err == nil {
			// One more pause for the daemon to finish restarting the
			// containers whose policy asks for it.
			time.Sleep(10 * time.Second)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine did not come back within 4 minutes")
		}
		time.Sleep(3 * time.Second)
	}
}

func TestRealDockerRCADaemonRestartBringsBackNothingHarborMasterParked(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-daemon"
	f := &faults{startErr: docker.ErrMutationFailed}
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--restart", "always"}
		o.restoreOff = true
		withFaults(f)(o)
	})
	originalID := containerID(name)
	rig.start()
	mustRequestManualUpdate(t, rig)
	execution := awaitTerminalExecution(rig)
	if execution.Failure != domain.ExecutionFailureStart || execution.Checkpoint != domain.CheckpointReplacementQuarantined {
		t.Fatalf("failure %q checkpoint %q, want start/replacementQuarantined", execution.Failure, execution.Checkpoint)
	}
	parked, quarantined := execution.ParkedName, execution.QuarantineName

	// Scenario 8 and 9: what the daemon holds.
	if got := restartPolicyOf(parked); got != "no" {
		t.Fatalf("the parked original (was always) reads restart policy %q, want no", got)
	}
	if got := restartPolicyOf(quarantined); got != "no" {
		t.Fatalf("the quarantined replacement reads restart policy %q, want no", got)
	}
	if isRunning(parked) || isRunning(quarantined) {
		t.Fatalf("parked running=%v quarantined running=%v before the restart", isRunning(parked), isRunning(quarantined))
	}
	if got := execution.OriginalRestartPolicy.Name; got != "always" {
		t.Fatalf("the record holds the original's policy as %q, want always", got)
	}

	// Scenario 13: the daemon restarts. HarborMaster is down for it.
	rig.stop()
	dockerEngineRestart(t)
	if isRunning(parked) {
		t.Error("the parked original started by itself after the daemon restart")
	}
	if isRunning(quarantined) {
		t.Error("the quarantined replacement started by itself after the daemon restart")
	}
	if got := containerID(name); got != "" {
		t.Errorf("%q is held by %s after the restart; nothing should hold it", name, domain.ShortenID(got))
	}

	// HarborMaster comes back, then a person rolls back.
	rig.open(rig.options)
	rig.start()
	if _, err := rig.rollbacks.Request(context.Background(), service.RollbackRequest{
		ExecutionID: execution.ExecutionID, RequestedBy: rcOperator,
	}); err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	rollback := awaitTerminalRollback(rig)
	if rollback.State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q/%q", rollback.State, rollback.Failure)
	}
	assertOriginalServing(t, rig, originalID, "always")
	if isRunning(rollback.ReplacementParkedName) || restartPolicyOf(rollback.ReplacementParkedName) != "no" {
		t.Errorf("the replacement after the rollback: running=%v policy=%q",
			isRunning(rollback.ReplacementParkedName), restartPolicyOf(rollback.ReplacementParkedName))
	}
}

// ------------------- Scenario 10: the original's suspension is refused --

func TestRealDockerRCARefusedSuspensionStopsBeforeTheCreate(t *testing.T) {
	skipUnlessRealDocker(t)
	t.Run("the record names the hazard", func(t *testing.T) {
		const name = "hm-c4c1-rc-suspend"
		f := &faults{suspendErrMarker: domain.ParkedNameSuffix}
		rig := newRealRig(t, func(o *realRigOptions) {
			o.name = name
			o.runArgs = []string{"--restart", "always"}
			o.restoreOff = true
			withFaults(f)(o)
		})
		originalID := containerID(name)
		rig.start()
		sinceRequest := time.Now().UTC()
		mustRequestManualUpdate(t, rig)
		execution := awaitTerminalExecution(rig)
		if execution.Failure != domain.ExecutionFailureRestartPolicy || execution.Checkpoint != domain.CheckpointOriginalParked {
			t.Fatalf("failure %q checkpoint %q, want restartPolicy/originalParked", execution.Failure, execution.Checkpoint)
		}
		if execution.ReplacementID != "" || countEvents(dockerEvents(t, sinceRequest), "create") != 0 {
			t.Fatalf("a replacement was created beside an original that can restart by itself")
		}
		if !planMentionsRestart(execution.Recovery) {
			t.Errorf("the recovery plan does not name the restart hazard: %+v", execution.Recovery)
		}
		if execution.Recovery == nil || !execution.Recovery.ServiceInterrupted {
			t.Error("the record does not say the service is down")
		}
		// The hazard is real: the parked original still carries its policy.
		if got := containerID(execution.ParkedName); got != originalID {
			t.Fatalf("the parked name holds %s, want the original", domain.ShortenID(got))
		}
		if got := restartPolicyOf(execution.ParkedName); got != "always" {
			t.Errorf("the parked original reads %q; the scenario meant the suspension to fail", got)
		}
	})
	t.Run("the automatic restore puts it back", func(t *testing.T) {
		const name = "hm-c4c1-rc-suspend2"
		f := &faults{suspendErrMarker: domain.ParkedNameSuffix}
		rig := newRealRig(t, func(o *realRigOptions) {
			o.name = name
			o.runArgs = []string{"--restart", "always"}
			withFaults(f)(o)
		})
		originalID := containerID(name)
		rig.start()
		sinceRequest := time.Now().UTC()
		mustRequestManualUpdate(t, rig)
		execution := awaitRealRestoreSettled(rig)
		if execution.Failure != domain.ExecutionFailureRestartPolicy {
			t.Fatalf("failure %q, want restartPolicy", execution.Failure)
		}
		if execution.Restore.State != domain.RestoreRestored {
			t.Fatalf("restore = %+v, want restored", execution.Restore)
		}
		if countEvents(dockerEvents(t, sinceRequest), "create") != 0 {
			t.Error("a replacement was created")
		}
		assertOriginalServing(t, rig, originalID, "always")
	})
}

// ---------------- Scenario 11: the quarantine's suspension is refused --

func TestRealDockerRCAnUnsecuredQuarantineIsNeverCalledSafe(t *testing.T) {
	skipUnlessRealDocker(t)
	t.Run("the rollback secures it", func(t *testing.T) {
		const name = "hm-c4c1-rc-quar"
		f := &faults{startErr: docker.ErrMutationFailed, suspendErrMarker: domain.QuarantineNameSuffix}
		rig := newRealRig(t, func(o *realRigOptions) {
			o.name = name
			o.runArgs = []string{"--restart", "always"}
			withFaults(f)(o)
		})
		originalID := containerID(name)
		rig.start()
		mustRequestManualUpdate(t, rig)
		execution := awaitRealRestoreSettled(rig)
		if execution.Checkpoint == domain.CheckpointReplacementQuarantined {
			t.Error("the record calls the replacement quarantined although its restart could not be suspended")
		}
		if execution.QuarantineName == "" {
			t.Error("the record does not say where the replacement was moved")
		}
		if execution.Restore.State != domain.RestoreRestored {
			t.Fatalf("restore = %+v, want restored", execution.Restore)
		}
		rollback := awaitTerminalRollback(rig)
		if rollback.State != domain.RollbackSucceeded || rollback.Recovery != nil {
			t.Errorf("rollback %q with plan %+v; it secured the replacement and should carry no plan",
				rollback.State, rollback.Recovery)
		}
		if got := restartPolicyOf(rollback.ReplacementParkedName); got != "no" {
			t.Errorf("the replacement reads %q after the rollback, want no", got)
		}
		assertOriginalServing(t, rig, originalID, "always")
	})
	t.Run("the rollback cannot secure it and says so", func(t *testing.T) {
		const name = "hm-c4c1-rc-quar2"
		f := &faults{
			startErr:           docker.ErrMutationFailed,
			suspendErrMarker:   domain.QuarantineNameSuffix,
			rollbackSuspendErr: errors.New("the daemon refused the restart policy update"),
		}
		rig := newRealRig(t, func(o *realRigOptions) {
			o.name = name
			o.runArgs = []string{"--restart", "always"}
			withFaults(f)(o)
		})
		originalID := containerID(name)
		rig.start()
		mustRequestManualUpdate(t, rig)
		execution := awaitRealRestoreSettled(rig)
		if execution.Checkpoint == domain.CheckpointReplacementQuarantined {
			t.Error("the record calls the replacement quarantined although its restart could not be suspended")
		}
		if execution.Restore.State != domain.RestoreRestored {
			t.Fatalf("restore = %+v, want restored", execution.Restore)
		}
		rollback := awaitTerminalRollback(rig)
		if rollback.State != domain.RollbackSucceeded {
			t.Fatalf("the rollback ended %q/%q", rollback.State, rollback.Failure)
		}
		// Succeeded -- the original is serving -- but NOT green: the record
		// carries an attention plan naming the replacement and the command.
		if !planMentionsRestart(rollback.Recovery) {
			t.Errorf("the rollback carries no plan about the replacement that can restart by itself: %+v", rollback.Recovery)
		}
		if rollback.Recovery != nil && rollback.Recovery.ServiceInterrupted {
			t.Error("the plan says the service is interrupted; the original is serving")
		}
		if got := restartPolicyOf(rollback.ReplacementParkedName); got != "always" {
			t.Errorf("the replacement reads %q; the scenario meant both suspensions to fail", got)
		}
		if isRunning(rollback.ReplacementParkedName) {
			t.Error("the replacement is running")
		}
		assertOriginalServing(t, rig, originalID, "always")
	})
}

// ------------------ Scenario 12: HarborMaster dies in the middle --

// crash kills HarborMaster at a hold: the database handle is gone before the
// stalled goroutine can record anything more, then the schedulers are torn
// down. What the daemon holds and what the row says are exactly what a
// process death would have left.
func (r *realRig) crash(h *hold) domain.Execution {
	r.t.Helper()
	select {
	case <-h.reached:
	case <-time.After(3 * time.Minute):
		r.t.Fatalf("the pipeline never reached %q", h.step)
	}
	executions, _, err := r.db.Executions.List(context.Background(),
		store.ExecutionFilter{Page: store.Page{Limit: 10}})
	if err != nil || len(executions) != 1 {
		r.t.Fatalf("read the in-flight record: %v", err)
	}
	inFlight := executions[0]

	db := r.db
	r.db = nil
	_ = db.Close()
	close(h.release)
	r.cancel()
	<-r.stopped
	r.cancel = nil
	_ = r.client.Close()
	r.client = nil
	return inFlight
}

func TestRealDockerRCARestartMidUpdateConvergesOnTheOriginal(t *testing.T) {
	skipUnlessRealDocker(t)
	cases := []struct {
		point string
		step  string
	}{
		{"after originalStopped", "rename"},
		{"after originalParked", "create"},
		{"after the create landed, before its checkpoint", "createDone"},
		{"after replacementCreated", "start"},
		{"after replacementStarted and verified", "remove"},
	}
	for i, tc := range cases {
		t.Run(tc.point, func(t *testing.T) {
			name := "hm-c4c1-rc-crash" + string(rune('a'+i))
			h := newHold(tc.step)
			f := &faults{hold: h}
			rig := newRealRig(t, func(o *realRigOptions) {
				o.name = name
				o.runArgs = []string{"--restart", "always"}
				withFaults(f)(o)
			})
			originalID := containerID(name)
			rig.start()
			execution := mustRequestManualUpdate(t, rig)

			inFlight := rig.crash(h)
			replacementBefore := containerID(name)
			if replacementBefore == originalID {
				replacementBefore = ""
			}
			t.Logf("crashed %s: state=%s checkpoint=%q parked=%q replacement=%s original running=%v replacement running=%v",
				tc.point, inFlight.State, inFlight.Checkpoint, inFlight.ParkedName,
				domain.ShortenID(replacementBefore),
				isRunning(originalID), replacementBefore != "" && isRunning(replacementBefore))
			f.mu.Lock()
			f.hold = nil
			f.mu.Unlock()

			if tc.step == "remove" {
				// The recreation settles BEFORE the parked original is removed:
				// the removal is the one step that has no undo, so it comes
				// after the record says the replacement is proved. A crash here
				// leaves a settled success, a serving replacement, and a parked
				// original that is stopped and cannot restart. Nothing rolls
				// back a success, and nothing removes the original later; the
				// record says it is still present.
				if inFlight.State != domain.ExecutionSucceeded || inFlight.OriginalRemoved {
					t.Fatalf("the row reads %q originalRemoved=%v at the removal step", inFlight.State, inFlight.OriginalRemoved)
				}
				rig.open(rig.options)
				rig.start()
				time.Sleep(3 * time.Second)
				settled, _ := rig.db.Executions.Get(context.Background(), execution.ExecutionID)
				t.Logf("recovered %s: state=%s originalRemoved=%v serving=%s", tc.point,
					settled.State, settled.OriginalRemoved, domain.ShortenID(containerID(name)))
				if settled.State != domain.ExecutionSucceeded || settled.Restore.State != "" {
					t.Errorf("the settled success was rewritten: %q restore=%+v", settled.State, settled.Restore)
				}
				if got := containerID(name); got != replacementBefore || !isRunning(name) {
					t.Errorf("%q is held by %s running=%v; want the verified replacement", name, domain.ShortenID(got), isRunning(name))
				}
				if containerID(inFlight.ParkedName) != originalID {
					t.Errorf("the parked original is gone or renamed; the record says it is present")
				} else if isRunning(inFlight.ParkedName) || restartPolicyOf(inFlight.ParkedName) != "no" {
					t.Errorf("the parked original: running=%v policy=%q", isRunning(inFlight.ParkedName), restartPolicyOf(inFlight.ParkedName))
				}
				return
			}
			if inFlight.State.Terminal() {
				t.Fatalf("the row was already settled as %q; this is not a crash", inFlight.State)
			}
			rig.open(rig.options)
			rig.start()

			settled := awaitRealRestoreSettled(rig)
			if settled.ExecutionID != execution.ExecutionID || settled.Failure != domain.ExecutionFailureInterrupted {
				t.Fatalf("settled as %q/%q, want interrupted", settled.State, settled.Failure)
			}
			t.Logf("recovered %s: checkpoint=%q replacement=%s restore=%s serving=%s",
				tc.point, settled.Checkpoint, domain.ShortenID(settled.ReplacementID),
				settled.Restore.State, domain.ShortenID(containerID(name)))
			if settled.Restore.State != domain.RestoreRestored {
				t.Fatalf("restore = %+v, want restored\n\nplan: %+v", settled.Restore, settled.Recovery)
			}
			if replacementBefore != "" && settled.ReplacementID != replacementBefore {
				t.Errorf("the record names replacement %s; the daemon held %s",
					domain.ShortenID(settled.ReplacementID), domain.ShortenID(replacementBefore))
			}
			assertOriginalServing(t, rig, originalID, "always")
			if replacementBefore != "" {
				rollback := awaitTerminalRollback(rig)
				if isRunning(rollback.ReplacementParkedName) || restartPolicyOf(rollback.ReplacementParkedName) != "no" {
					t.Errorf("the replacement after recovery: running=%v policy=%q",
						isRunning(rollback.ReplacementParkedName), restartPolicyOf(rollback.ReplacementParkedName))
				}
			}
			assertOnlyDisposableContainersTouched(t, dockerEvents(t, rig.startedAt))
		})
	}
}

// ------------------------------------------- Scenario 14: AutoRemove --

func TestRealDockerRCAnAutoRemoveContainerIsRefusedBeforeAnyMutation(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-rm"
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.runArgs = []string{"--rm"}
	})
	originalID := containerID(name)
	rig.start()
	mark := time.Now().UTC()
	execution, err := requestManualUpdate(t, rig)
	if err == nil {
		execution = awaitTerminalExecution(rig)
	} else {
		t.Logf("refused at request time: %v", err)
	}
	if err == nil {
		if execution.State == domain.ExecutionSucceeded || execution.Checkpoint != domain.CheckpointNone {
			t.Fatalf("the update ended %q/%q at checkpoint %q", execution.State, execution.Failure, execution.Checkpoint)
		}
		t.Logf("settled as %s/%s/%s: %s", execution.State, execution.Failure, execution.Refusal, execution.Message)
	}
	if got := containerID(name); got != originalID || !isRunning(name) {
		t.Fatalf("the original is %s running=%v; it must be untouched", domain.ShortenID(got), isRunning(name))
	}
	if touched := mutatingEventsOn(dockerEventsSince(t, mark), name); len(touched) != 0 {
		t.Errorf("the daemon reports HarborMaster acting on the AutoRemove container: %v", touched)
	}
	if got := countEvents(dockerEventsSince(t, mark), "create"); got != 0 {
		t.Errorf("%d containers were created", got)
	}
}

// ---------------------------------------------- Scenario 15: paused --

func TestRealDockerRCAPausedContainerIsRefusedThenUpdatesOnceUnpaused(t *testing.T) {
	skipUnlessRealDocker(t)
	const name = "hm-c4c1-rc-paused"
	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
	})
	originalID := containerID(name)
	rig.start()
	dockerRun(t, "pause", name)
	t.Cleanup(func() { dockerQuiet("unpause", name) })
	mark := time.Now().UTC()

	_, err := requestManualUpdate(t, rig)
	var refused service.ErrExecutionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a paused container was accepted for recreation (err %v)", err)
	}
	if refused.Refusal != domain.ExecutionRefusalContainerState {
		t.Errorf("refusal %q, want containerState", refused.Refusal)
	}
	if inspectFormat(name, "{{.State.Paused}}") != "true" || containerID(name) != originalID {
		t.Fatal("the paused container was touched")
	}
	if touched := mutatingEventsOn(dockerEventsSince(t, mark), name); len(touched) != 0 {
		t.Errorf("the daemon reports HarborMaster acting on the paused container: %v", touched)
	}

	dockerRun(t, "unpause", name)
	rig.refreshInventory()
	ctx := context.Background()
	acquisitions, _, err := rig.db.Acquisitions.List(ctx, store.AcquisitionFilter{Page: store.Page{Limit: 10}})
	if err != nil || len(acquisitions) != 1 {
		t.Fatalf("acquisitions: %d, %v", len(acquisitions), err)
	}
	execution, err := rig.executions.Request(ctx, service.ExecutionRequest{
		AcquisitionID: acquisitions[0].AcquisitionID, RequestedBy: rcOperator,
	})
	if err != nil {
		t.Fatalf("the update after unpausing was refused: %v", err)
	}
	rig.await("the recreation to settle", func() bool {
		current, err := rig.db.Executions.Get(ctx, execution.ExecutionID)
		return err == nil && current.State.Terminal()
	})
	final, _ := rig.db.Executions.Get(ctx, execution.ExecutionID)
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("the update after unpausing ended %q/%q: %s", final.State, final.Failure, final.Message)
	}
	if got := containerID(name); got == originalID || got != final.ReplacementID {
		t.Errorf("%q is held by %s after the update", name, domain.ShortenID(got))
	}
}
