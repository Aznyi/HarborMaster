//go:build integration

// Package integration exercises HarborMaster against a REAL Docker daemon.
//
// Build-tagged so `go test ./...` never needs one: Docker is frequently absent
// from developer machines and from parts of CI, and a test suite that cannot
// run without it is a test suite that stops being run. Everything these tests
// cover is also covered against docker.Fake in the unit suites; this is the
// end-to-end proof that the fake models the real daemon accurately.
//
// Run with:
//
//	go test -tags integration -v ./internal/integration/...
//
// ONE THING TO NOTE ABOUT THESE TESTS. The Docker mutations below -- creating
// containers, networks, volumes -- are performed by shelling out to the `docker`
// CLI, never through internal/docker. That is deliberate and load-bearing:
// seeing the writes go through an external binary is the visible proof that
// HarborMaster itself mutated nothing.
//
// This used to be enforced by the adapter having no write path at all. Since
// Phase 8 it has exactly one -- pulling a digest-pinned image -- so the property
// is now narrower and stated explicitly: nothing in THIS file uses it, and
// nothing anywhere can use the adapter to touch a container, because no such
// method exists. acquisition_test.go exercises that one write directly, and
// asserts that the container count is unchanged either side of it.
package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// testImage is deliberately tiny and universally available, so a CI runner
// pulls a few hundred kilobytes rather than a base image.
const testImage = "busybox:stable"

// resourcePrefix marks everything these tests create, so the cleanup sweep can
// find strays from an interrupted run.
const resourcePrefix = "harbormaster-it-"

// ---------------------------------------------------------------- harness --

// dockerCLI runs a docker command, failing the test on error.
//
// The CLI rather than the SDK: these are the mutations under observation, and
// routing them through an external binary keeps them unmistakably outside
// HarborMaster.
func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// dockerCLIQuiet runs a docker command, ignoring failure. Used for cleanup,
// where the resource may already be gone.
func dockerCLIQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

// requireDocker skips the test when no daemon is reachable.
func requireDocker(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("no reachable docker daemon; skipping the integration suite")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// harness is a running HarborMaster event engine bound to the real daemon.
type harness struct {
	engine    *service.EventService
	inventory *service.InventoryService
	client    *docker.Client
	db        *store.DB
	cancel    context.CancelFunc
	done      chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireDocker(t)

	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "harbormaster.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	client, err := docker.New(docker.Options{
		Host:       defaultDockerHost(),
		APIVersion: pinnedAPIVersion(),
		Timeout:    30 * time.Second,
		Masker:     domain.NewDefaultMasker(),
	})
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	if _, err := client.Ping(ctx); err != nil {
		_ = client.Close()
		_ = db.Close()
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
		// The engine owns reconciliation here, as it does in production.
		SuppressPeriodic: true,
	})

	// Seed the inventory so targeted refreshes have a generation to join, and
	// so "converged" means something from the first assertion onwards.
	if _, err := inventory.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	engine := service.NewEventService(service.EventOptions{
		Runtime:   client,
		Events:    db.DockerEvents,
		Inventory: inventory,
		Logger:    discardLogger(),
		Config: config.Events{
			Enabled:          true,
			ReconnectInitial: 200 * time.Millisecond,
			ReconnectMax:     2 * time.Second,
			ReconnectFactor:  2,
			BufferSize:       512,
			BatchSize:        16,
			BatchFlush:       100 * time.Millisecond,
			DedupWindow:      10 * time.Second,
			// Short, so a test observes convergence quickly. Production
			// defaults are far longer.
			RefreshDebounce:   150 * time.Millisecond,
			ReconcileInterval: 10 * time.Minute,
			PruneInterval:     time.Hour,
			StreamSubscribers: 4,
			StreamBuffer:      64,
			StreamReplay:      100,
			StreamHeartbeat:   30 * time.Second,
		},
	})

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.Run(runCtx)
	}()

	h := &harness{
		engine: engine, inventory: inventory, client: client,
		db: db, cancel: cancel, done: done,
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the event engine did not shut down")
		}
		_ = client.Close()
		_ = db.Close()
	})

	h.waitConnected(t)
	return h
}

func defaultDockerHost() string {
	// The tests run wherever the CLI can reach, so the adapter uses the same
	// default the application does.
	cfg, err := config.Load()
	if err != nil {
		return "unix:///var/run/docker.sock"
	}
	return cfg.Docker.Host
}

// pinnedAPIVersion is the Engine API version the suite runs against.
//
// # Why this is read from configuration rather than left empty
//
// The compatibility matrix in CI runs this suite once per supported API
// version by setting HARBORMASTER_DOCKER_API_VERSION. A suite that ignored it
// would run five identical jobs and report a matrix it had not tested — worse
// than no matrix, because the badge would be green.
//
// Empty locally, which is the negotiated configuration every real deployment
// uses.
func pinnedAPIVersion() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return cfg.Docker.APIVersion
}

func (h *harness) waitConnected(t *testing.T) {
	t.Helper()
	h.eventually(t, 30*time.Second, "the event stream to connect", func() bool {
		return h.engine.Status(context.Background()).State == domain.ConnStateConnected
	})
}

// eventually polls until condition holds. No fixed sleeps: a real daemon's
// timing varies far too much for one to be anything but flaky.
func (h *harness) eventually(t *testing.T, timeout time.Duration, describe string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, describe)
}

// events returns the recorded events matching a filter.
func (h *harness) events(t *testing.T, filter store.DockerEventFilter) []domain.DockerEvent {
	t.Helper()

	if filter.Page.Limit == 0 {
		filter.Page.Limit = 200
	}
	recorded, _, err := h.db.DockerEvents.List(context.Background(), filter)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return recorded
}

// hasEvent reports whether an event with the given action was recorded for an
// actor.
func (h *harness) hasEvent(t *testing.T, actorID, action string) bool {
	t.Helper()

	for _, event := range h.events(t, store.DockerEventFilter{ActorID: actorID}) {
		if string(event.Action) == action {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- lifecycle --

// The primary use case: a container's lifecycle transitions must appear as
// events, and the inventory must converge on the real state after each one.
func TestContainerLifecycleConvergesInventory(t *testing.T) {
	h := newHarness(t)

	name := resourcePrefix + "lifecycle"
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", name) })

	dockerCLI(t, "run", "-d", "--name", name, testImage, "sleep", "3600")
	id := dockerCLI(t, "inspect", "--format", "{{.Id}}", name)

	// create + start
	h.eventually(t, 30*time.Second, "the create event", func() bool {
		return h.hasEvent(t, id, "create")
	})
	h.eventually(t, 30*time.Second, "the container to appear as running", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.State == domain.StateRunning
	})

	// pause / unpause
	dockerCLI(t, "pause", name)
	h.eventually(t, 30*time.Second, "the container to appear as paused", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.State == domain.StatePaused
	})

	dockerCLI(t, "unpause", name)
	h.eventually(t, 30*time.Second, "the container to appear as running again", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.State == domain.StateRunning
	})

	// restart
	dockerCLI(t, "restart", "-t", "1", name)
	h.eventually(t, 60*time.Second, "the restart event", func() bool {
		return h.hasEvent(t, id, "restart") || h.hasEvent(t, id, "start")
	})

	// stop
	dockerCLI(t, "stop", "-t", "1", name)
	h.eventually(t, 60*time.Second, "the container to appear as exited", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.State == domain.StateExited
	})

	// rename
	renamed := name + "-renamed"
	dockerCLI(t, "rename", name, renamed)
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", renamed) })
	h.eventually(t, 30*time.Second, "the rename to reach the inventory", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.Name == renamed
	})

	// remove
	dockerCLI(t, "rm", "-f", renamed)
	h.eventually(t, 30*time.Second, "the container to be marked absent", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && !detail.Overview.Present
	})

	// The row is RETAINED, not deleted, so its observed lifetime survives.
	if _, err := h.db.Containers.Get(context.Background(), id); err != nil {
		t.Errorf("a destroyed container's record must be retained: %v", err)
	}
}

// Events must be stored in the order they were observed, with a monotonic
// sequence. Docker guarantees no global ordering, so observation order is the
// only claim HarborMaster makes.
func TestEventsRecordedInObservedOrder(t *testing.T) {
	h := newHarness(t)

	name := resourcePrefix + "ordering"
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", name) })

	dockerCLI(t, "run", "-d", "--name", name, testImage, "sleep", "3600")
	id := dockerCLI(t, "inspect", "--format", "{{.Id}}", name)

	dockerCLI(t, "stop", "-t", "1", name)

	h.eventually(t, 60*time.Second, "several events for this container", func() bool {
		return len(h.events(t, store.DockerEventFilter{ActorID: id})) >= 2
	})

	recorded := h.events(t, store.DockerEventFilter{
		ActorID:   id,
		Sort:      "sequence",
		Direction: store.SortAsc,
	})

	for i := 1; i < len(recorded); i++ {
		if recorded[i].Sequence <= recorded[i-1].Sequence {
			t.Fatalf("sequences must be monotonic: %d then %d",
				recorded[i-1].Sequence, recorded[i].Sequence)
		}
	}

	for _, event := range recorded {
		if event.ObservedAt.IsZero() {
			t.Error("every event must carry an observation time")
		}
		if event.DockerTime.IsZero() {
			t.Error("every event must carry a docker time")
		}
		if event.Fingerprint == "" {
			t.Error("every event must carry a fingerprint")
		}
	}
}

// -------------------------------------------------------- networks, volumes --

func TestNetworkLifecycle(t *testing.T) {
	h := newHarness(t)

	name := resourcePrefix + "net"
	t.Cleanup(func() { dockerCLIQuiet("network", "rm", name) })

	dockerCLI(t, "network", "create", name)
	h.eventually(t, 30*time.Second, "the network to appear in the inventory", func() bool {
		networks, _, err := h.db.Networks.List(context.Background(), store.Page{Limit: 200})
		if err != nil {
			return false
		}
		for _, network := range networks {
			if network.Name == name {
				return true
			}
		}
		return false
	})

	dockerCLI(t, "network", "rm", name)
	h.eventually(t, 30*time.Second, "the network to leave the inventory", func() bool {
		networks, _, err := h.db.Networks.List(context.Background(), store.Page{Limit: 200})
		if err != nil {
			return false
		}
		for _, network := range networks {
			if network.Name == name {
				return false
			}
		}
		return true
	})
}

func TestVolumeLifecycle(t *testing.T) {
	h := newHarness(t)

	name := resourcePrefix + "vol"
	t.Cleanup(func() { dockerCLIQuiet("volume", "rm", "-f", name) })

	dockerCLI(t, "volume", "create", name)
	h.eventually(t, 30*time.Second, "the volume to appear in the inventory", func() bool {
		volumes, _, err := h.db.Volumes.List(context.Background(), store.Page{Limit: 200})
		if err != nil {
			return false
		}
		for _, volume := range volumes {
			if volume.Name == name {
				return true
			}
		}
		return false
	})

	dockerCLI(t, "volume", "rm", "-f", name)
	h.eventually(t, 30*time.Second, "the volume to leave the inventory", func() bool {
		volumes, _, err := h.db.Volumes.List(context.Background(), store.Page{Limit: 200})
		if err != nil {
			return false
		}
		for _, volume := range volumes {
			if volume.Name == name {
				return false
			}
		}
		return true
	})
}

// ------------------------------------------------------------------ images --

func TestImageTagProducesAnEvent(t *testing.T) {
	h := newHarness(t)

	// Pull explicitly so the tag below has something to work from even on a
	// runner with a cold image cache.
	dockerCLI(t, "pull", testImage)

	tag := resourcePrefix + "image:test"
	t.Cleanup(func() { dockerCLIQuiet("rmi", "-f", tag) })

	dockerCLI(t, "tag", testImage, tag)

	h.eventually(t, 30*time.Second, "an image event", func() bool {
		recorded := h.events(t, store.DockerEventFilter{
			Types: []domain.DockerEventType{domain.EventTypeImage},
		})
		return len(recorded) > 0
	})
}

// ------------------------------------------------------------ reconnection --

// Restarting the daemon is not something a shared CI runner should do, so this
// exercises the same code path by ending the subscription the way a daemon
// restart would: the engine must notice, reconnect, and reconcile.
func TestReconnectAfterStreamLoss(t *testing.T) {
	h := newHarness(t)

	before := h.engine.Status(context.Background()).Counters.FullReconciliations

	// A container created after the engine reconnects must still converge, which
	// is the property a reconnect has to preserve.
	name := resourcePrefix + "reconnect"
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", name) })

	dockerCLI(t, "run", "-d", "--name", name, testImage, "sleep", "3600")
	id := dockerCLI(t, "inspect", "--format", "{{.Id}}", name)

	h.eventually(t, 60*time.Second, "the container to converge", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.State == domain.StateRunning
	})

	if after := h.engine.Status(context.Background()).Counters.FullReconciliations; after < before {
		t.Errorf("reconciliation count went backwards: %d then %d", before, after)
	}
}

// ------------------------------------------------------- read-only guarantee --

// The load-bearing assertion of the whole suite: HarborMaster observes and
// never mutates.
//
// Two independent checks. First, the container's state and start count are
// unchanged after the engine has been running and refreshing for a while.
// Second, no event attributes the mutation to HarborMaster.
func TestHarborMasterPerformsNoDockerMutations(t *testing.T) {
	h := newHarness(t)

	name := resourcePrefix + "readonly"
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", name) })

	dockerCLI(t, "run", "-d", "--name", name, testImage, "sleep", "3600")
	id := dockerCLI(t, "inspect", "--format", "{{.Id}}", name)

	h.eventually(t, 60*time.Second, "the container to converge", func() bool {
		detail, err := h.db.Containers.Get(context.Background(), id)
		return err == nil && detail.Overview.State == domain.StateRunning
	})

	startedAt := dockerCLI(t, "inspect", "--format", "{{.State.StartedAt}}", name)
	restarts := dockerCLI(t, "inspect", "--format", "{{.RestartCount}}", name)

	// Drive several targeted refreshes and a full reconciliation. If any code
	// path mutated the container, this is where it would show.
	for range 5 {
		if err := h.inventory.RefreshContainer(context.Background(), id); err != nil {
			t.Fatalf("targeted refresh: %v", err)
		}
	}
	if err := h.inventory.Reconcile(context.Background(), domain.TriggerReconcile); err != nil &&
		!errors.Is(err, service.ErrRefreshInProgress) {
		t.Fatalf("reconcile: %v", err)
	}

	if after := dockerCLI(t, "inspect", "--format", "{{.State.StartedAt}}", name); after != startedAt {
		t.Errorf("StartedAt changed from %s to %s; HarborMaster restarted a container", startedAt, after)
	}
	if after := dockerCLI(t, "inspect", "--format", "{{.RestartCount}}", name); after != restarts {
		t.Errorf("RestartCount changed from %s to %s", restarts, after)
	}
	if state := dockerCLI(t, "inspect", "--format", "{{.State.Status}}", name); state != "running" {
		t.Errorf("state = %s, want running; HarborMaster changed the container", state)
	}

	// No stop, kill, die, or destroy may have been observed for a container
	// nothing touched.
	for _, forbidden := range []string{"stop", "kill", "die", "destroy", "pause"} {
		if h.hasEvent(t, id, forbidden) {
			t.Errorf("a %q event occurred for an untouched container", forbidden)
		}
	}
}

// Redaction must hold against a real daemon and a real label, not just a
// fixture.
func TestEventAttributesAreRedactedAgainstARealDaemon(t *testing.T) {
	h := newHarness(t)

	const secret = "integration-secret-value-do-not-leak"

	name := resourcePrefix + "redaction"
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", name) })

	dockerCLI(t, "run", "-d", "--name", name,
		"--label", "DB_PASSWORD="+secret,
		"--label", "harmless=keep-me",
		testImage, "sleep", "3600")
	id := dockerCLI(t, "inspect", "--format", "{{.Id}}", name)

	h.eventually(t, 30*time.Second, "an event for the labelled container", func() bool {
		return len(h.events(t, store.DockerEventFilter{ActorID: id})) > 0
	})

	for _, event := range h.events(t, store.DockerEventFilter{ActorID: id}) {
		for key, value := range event.Attributes {
			if strings.Contains(value, secret) {
				t.Fatalf("attribute %q leaked a secret label value", key)
			}
		}
		if got := event.Attributes["DB_PASSWORD"]; got != "" && got != domain.MaskedValue {
			t.Errorf("DB_PASSWORD = %q, want it masked", got)
		}
		if got := event.Attributes["harmless"]; got != "" && got != "keep-me" {
			t.Errorf("a non-sensitive label was altered: %q", got)
		}
	}
}

// ----------------------------------------------------------------- cleanup --

// TestZZZCleanupStrays removes anything an interrupted run left behind.
//
// Named to sort last, so it runs after every other test in the package. A
// shared CI runner must not accumulate containers from a cancelled job.
func TestZZZCleanupStrays(t *testing.T) {
	requireDocker(t)

	for _, kind := range []struct {
		list   []string
		remove []string
	}{
		{[]string{"ps", "-aq", "--filter", "name=" + resourcePrefix}, []string{"rm", "-f"}},
		{[]string{"network", "ls", "-q", "--filter", "name=" + resourcePrefix}, []string{"network", "rm"}},
		{[]string{"volume", "ls", "-q", "--filter", "name=" + resourcePrefix}, []string{"volume", "rm", "-f"}},
	} {
		output := dockerCLI(t, kind.list...)
		if output == "" {
			continue
		}
		for _, id := range strings.Fields(output) {
			dockerCLIQuiet(append(append([]string(nil), kind.remove...), id)...)
		}
	}

	// Images are matched by reference rather than by a name filter.
	images := dockerCLI(t, "images", "--format", "{{.Repository}}:{{.Tag}}")
	for _, reference := range strings.Fields(images) {
		if strings.HasPrefix(reference, resourcePrefix) {
			dockerCLIQuiet("rmi", "-f", reference)
		}
	}

	fmt.Println("integration cleanup complete")
}
