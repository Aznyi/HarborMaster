package service_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// A cleanup pass against a REAL daemon and a REAL database.
//
// # What this covers that nothing else does
//
// The unit tests prove each piece: the decision is a pure function, the store
// queries run against real SQLite, the pass is a service over interfaces, and
// the removal capability is checked against a real daemon on its own. This is
// the seam between them -- lifecycle records written by the real repositories,
// a real image on a real host, and the real pruner -- because the interesting
// failures live in the joins, not the parts.
//
// Off unless HARBORMASTER_DOCKER_INTEGRATION=1.
//
// # What it touches
//
// Only artefacts it created. Every image is committed from a throwaway
// container so it exists nowhere else on the host, every name begins hm-c4a-,
// and each scenario cleans up after itself. Nothing here can reach an image the
// host had before it ran: the candidate set comes from execution rows this test
// wrote, naming image ids this test created.

const cleanupBaseImage = "alpine:3.20.3"

func skipUnlessDockerIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("HARBORMASTER_DOCKER_INTEGRATION") != "1" {
		t.Skip("set HARBORMASTER_DOCKER_INTEGRATION=1 to run against a real Docker daemon")
	}
}

func cleanupDockerHost() string {
	if host := os.Getenv("HARBORMASTER_DOCKER_HOST"); host != "" {
		return host
	}
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func cleanupDocker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func cleanupDockerQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

// makeDisposableImage commits a throwaway container into a distinct artefact.
//
// A COMMIT rather than a tag, deliberately. A second tag on an existing image
// shares its id, so a test that removed it would be removing something the host
// already had. A committed image is one this test created and nothing else
// references, which is what makes "the image is gone" a safe assertion.
func makeDisposableImage(t *testing.T, tag string) string {
	t.Helper()

	scratch := "hm-c4a-mk-" + strings.ReplaceAll(strings.ReplaceAll(tag, ":", "-"), "/", "-")

	cleanupDocker(t, "pull", cleanupBaseImage)
	cleanupDockerQuiet("rm", "-f", scratch)
	cleanupDocker(t, "create", "--name", scratch, cleanupBaseImage, "true")
	// A unique label makes the commit produce a distinct id even when two
	// scenarios commit from the same base in the same second.
	cleanupDocker(t, "commit", "--change", "LABEL io.harbormaster.test="+tag, scratch, tag)
	cleanupDocker(t, "rm", "-f", scratch)

	t.Cleanup(func() {
		cleanupDockerQuiet("rm", "-f", scratch)
		cleanupDockerQuiet("rmi", "-f", tag)
	})

	return cleanupDocker(t, "image", "inspect", tag, "--format", "{{.Id}}")
}

func imageExists(t *testing.T, imageID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", imageID).Run() == nil
}

// cleanupSettlement is when every scenario's updates settled, and cleanupClock
// is far enough past it that the retention window has always elapsed.
//
// The PRODUCTION minimum age is used unchanged -- the window is crossed by
// moving the clock, not by configuring a shorter one. A test that lowered
// MIN_AGE would be exercising a configuration the loader refuses.
var (
	cleanupSettlement = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cleanupClock      = cleanupSettlement.Add(365 * 24 * time.Hour)
)

// integrationRig is a cleanup service over a real database and a real daemon.
type integrationRig struct {
	db      *store.DB
	client  *docker.Client
	service *service.ImageCleanupService
}

func newIntegrationRig(t *testing.T, keepGenerations int) *integrationRig {
	t.Helper()

	db := openDB(t)
	client, err := docker.New(docker.Options{
		Host:    cleanupDockerHost(),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	cfg := cleanupConfig()
	cfg.KeepGenerations = keepGenerations

	return &integrationRig{
		db:     db,
		client: client,
		service: service.NewImageCleanupService(service.ImageCleanupOptions{
			Store:   db.ImageRetention,
			Runtime: client,
			Pruner:  client,
			Config:  cfg,
			Now:     func() time.Time { return cleanupClock },
		}),
	}
}

// settle writes a SUCCEEDED, settled execution through the real repository.
//
// Through Create and Advance rather than an INSERT: the candidate query keys on
// a combination of columns the lifecycle produces, and a hand-written row could
// claim a combination it never would.
func (r *integrationRig) settle(
	t *testing.T,
	containerName, oldImageID, targetImageID string,
	settledAt time.Time,
) domain.Execution {
	t.Helper()
	return r.write(t, containerName, oldImageID, targetImageID, settledAt,
		domain.ExecutionSucceeded, true, nil)
}

// write drives one execution to a terminal state.
func (r *integrationRig) write(
	t *testing.T,
	containerName, oldImageID, targetImageID string,
	at time.Time,
	state domain.ExecutionState,
	originalRemoved bool,
	adjust func(*store.ExecutionChange),
) domain.Execution {
	t.Helper()
	ctx := context.Background()

	execution := domain.Execution{
		ExecutionID:   domain.NewExecutionID(),
		AcquisitionID: domain.NewAcquisitionID(),
		PlanID:        "plan_00112233445566778899",
		ContainerID:   strings.Repeat("d", 64),
		ContainerName: containerName,
		OldImage:      cleanupBaseImage,
		OldImageID:    oldImageID,
		Target: domain.ExecutionTarget{
			Registry:   "docker.io",
			Repository: "library/alpine",
			Digest:     "sha256:" + strings.Repeat("e", 64),
			Reference:  cleanupBaseImage,
			ImageID:    targetImageID,
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State:       domain.ExecutionQueued,
		RequestedAt: at.Add(-time.Minute),
		ExpiresAt:   at.Add(time.Hour),
		PlanDigest:  strings.Repeat("f", 64),
	}

	created, err := r.db.Executions.Create(ctx, execution, at.Add(-time.Minute))
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}

	change := store.ExecutionChange{
		ExecutionID:     created.ExecutionID,
		To:              state,
		OriginalRemoved: originalRemoved,
	}
	if state == domain.ExecutionFailed {
		change.Failure = domain.ExecutionFailureImageMismatch
	}
	if adjust != nil {
		adjust(&change)
	}

	moved, err := r.db.Executions.Advance(ctx, change, at)
	if err != nil || !moved {
		t.Fatalf("advance execution: moved=%v err=%v", moved, err)
	}
	return created
}

func (r *integrationRig) run(t *testing.T) service.ImageCleanupPass {
	t.Helper()
	return r.service.RunPass(context.Background())
}

// --------------------------------------------------- A: a settled update --

func TestLiveASettledUpdateHasItsSupersededImageRemoved(t *testing.T) {
	skipUnlessDockerIntegration(t)

	// The non-vacuity control for every scenario below. If nothing is ever
	// removed against a real daemon, "it was kept" proves nothing.
	rig := newIntegrationRig(t, 1)

	// TWO settled updates, because one generation is kept unconditionally.
	// With a single superseded image there is nothing eligible at all -- which
	// is the generation contract working, and is why this scenario needs a
	// second update before anything can go.
	old := makeDisposableImage(t, "hm-c4a-a-old:1")
	middle := makeDisposableImage(t, "hm-c4a-a-mid:1")
	current := makeDisposableImage(t, "hm-c4a-a-new:1")

	rig.settle(t, "hm-c4a-a", old, middle, cleanupSettlement)
	rig.settle(t, "hm-c4a-a", middle, current, cleanupSettlement.Add(time.Hour))

	pass := rig.run(t)

	if pass.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 (considered=%d retained=%d refused=%d failed=%d)",
			pass.Removed, pass.Considered, pass.Retained, pass.Refused, pass.Failed)
	}
	if imageExists(t, old) {
		t.Error("the superseded image is still on the host")
	}
	if !imageExists(t, middle) {
		t.Error("the KEPT generation was removed: one superseded image per " +
			"workload is kept whatever its age")
	}
	if !imageExists(t, current) {
		t.Error("THE CURRENT IMAGE WAS REMOVED.\n\n" +
			"This is the outcome cleanup exists to make impossible. The image a " +
			"workload was moved ONTO is never a candidate; only the one it was " +
			"moved off is.")
	}
}

// ------------------------------------------------ B: multiple generations --

func TestLiveOnlyTheGenerationsPastTheContractAreRemoved(t *testing.T) {
	skipUnlessDockerIntegration(t)

	rig := newIntegrationRig(t, 2)

	oldest := makeDisposableImage(t, "hm-c4a-b-1:1")
	middle := makeDisposableImage(t, "hm-c4a-b-2:1")
	newest := makeDisposableImage(t, "hm-c4a-b-3:1")
	current := makeDisposableImage(t, "hm-c4a-b-4:1")

	rig.settle(t, "hm-c4a-b", oldest, middle, cleanupSettlement)
	rig.settle(t, "hm-c4a-b", middle, newest, cleanupSettlement.Add(time.Hour))
	rig.settle(t, "hm-c4a-b", newest, current, cleanupSettlement.Add(2*time.Hour))

	rig.run(t)

	if imageExists(t, oldest) {
		t.Error("the third-oldest generation was kept with two configured; " +
			"nothing would ever be removed on a busy host")
	}
	for name, id := range map[string]string{"middle": middle, "newest": newest} {
		if !imageExists(t, id) {
			t.Errorf("the %s generation was removed with two configured.\n\n"+
				"Generations are the rollback contract: two means an operator "+
				"can go back past the update that broke things to the one "+
				"before it.", name)
		}
	}
	if !imageExists(t, current) {
		t.Error("the current image was removed")
	}
}

// -------------------------------------------------- C and F: still in use --

func TestLiveAnImageAContainerStillUsesIsKept(t *testing.T) {
	skipUnlessDockerIntegration(t)

	rig := newIntegrationRig(t, 1)

	// Superseded according to the records -- and still under a container on the
	// host. That combination is how a shared base image looks: one workload
	// moved off it, another never did.
	shared := makeDisposableImage(t, "hm-c4a-c-shared:1")
	current := makeDisposableImage(t, "hm-c4a-c-new:1")

	const holder = "hm-c4a-c-holder"
	cleanupDockerQuiet("rm", "-f", holder)
	cleanupDocker(t, "create", "--name", holder, shared, "true")
	t.Cleanup(func() { cleanupDockerQuiet("rm", "-f", holder) })

	rig.settle(t, "hm-c4a-c", shared, current, cleanupSettlement)

	pass := rig.run(t)

	if !imageExists(t, shared) {
		t.Fatal("AN IMAGE A CONTAINER IS BUILT FROM WAS REMOVED.\n\n" +
			"Two independent things had to fail for this: the live container " +
			"listing HarborMaster takes immediately before acting, and the " +
			"daemon's own refusal underneath it.")
	}
	if pass.Removed != 0 {
		t.Errorf("Removed = %d, want 0", pass.Removed)
	}
	// The live listing should have caught it, so the daemon never had to.
	if pass.Refused != 0 {
		t.Errorf("Refused = %d, want 0\n\n"+
			"A refusal means HarborMaster asked the daemon to remove an image a "+
			"container was using, and only the daemon stopped it. Correct, but "+
			"it means the check above the daemon did not see what the daemon "+
			"saw.", pass.Refused)
	}
	if got := cleanupDocker(t, "inspect", holder, "--format", "{{.Name}}"); !strings.Contains(got, holder) {
		t.Errorf("the container is gone: %q", got)
	}
}

// ---------------------------------------------------- D: a failed update --

func TestLiveAFailedUpdateKeepsBothOfItsImages(t *testing.T) {
	skipUnlessDockerIntegration(t)

	rig := newIntegrationRig(t, 1)

	original := makeDisposableImage(t, "hm-c4a-d-old:1")
	failed := makeDisposableImage(t, "hm-c4a-d-new:1")

	rig.write(t, "hm-c4a-d", original, failed, cleanupSettlement,
		domain.ExecutionFailed, false, nil)

	rig.run(t)

	if !imageExists(t, original) {
		t.Error("the ORIGINAL image of a failed update was removed.\n\n" +
			"It is what puts the workload back. It is the last thing cleanup " +
			"may touch after an update did not work.")
	}
	if !imageExists(t, failed) {
		t.Error("the FAILED TARGET image was removed.\n\n" +
			"It is the only artefact that can be inspected to find out why the " +
			"update did not work.")
	}
}

// ------------------------------------------- E: an outstanding recovery --

func TestLiveAnOutstandingRecoveryKeepsItsImages(t *testing.T) {
	skipUnlessDockerIntegration(t)

	rig := newIntegrationRig(t, 1)

	original := makeDisposableImage(t, "hm-c4a-e-old:1")
	replacement := makeDisposableImage(t, "hm-c4a-e-new:1")

	// A settled successful update -- which alone would make `original` a
	// candidate -- and a SECOND, failed one that left a recovery note naming
	// the same images. The note wins.
	rig.settle(t, "hm-c4a-e", original, replacement, cleanupSettlement)

	rig.write(t, "hm-c4a-e2", original, replacement, cleanupSettlement.Add(time.Hour),
		domain.ExecutionFailed, false, func(change *store.ExecutionChange) {
			change.MarkMutated = true
			change.ParkedName = "hm-c4a-e2" + domain.ParkedNameSuffix + change.ExecutionID
			change.Recovery = domain.BuildRecoveryPlan(domain.RecoveryContext{
				ExecutionID:   change.ExecutionID,
				ContainerName: "hm-c4a-e2",
				OriginalID:    strings.Repeat("a", 64),
				ParkedName:    "hm-c4a-e2" + domain.ParkedNameSuffix + change.ExecutionID,
				Checkpoint:    domain.CheckpointReplacementStarted,
			})
		})

	rig.run(t)

	if !imageExists(t, original) {
		t.Error("an image named by an OUTSTANDING RECOVERY PLAN was removed.\n\n" +
			"HarborMaster told an operator the host was in a state it could not " +
			"resolve. Whatever that note says will be carried out against this " +
			"image.")
	}
	if !imageExists(t, replacement) {
		t.Error("the replacement image named by an outstanding recovery was removed")
	}
}

// ------------------------------------------------------ G: repeatability --

func TestLiveRepeatingAPassIsFree(t *testing.T) {
	skipUnlessDockerIntegration(t)

	// The restart property. Cleanup keeps no durable "already removed" state --
	// its candidate set is historical evidence that does not change when an
	// image goes -- so a restarted process re-examines the same candidates and
	// must settle rather than fail.
	rig := newIntegrationRig(t, 1)

	old := makeDisposableImage(t, "hm-c4a-g-old:1")
	middle := makeDisposableImage(t, "hm-c4a-g-mid:1")
	current := makeDisposableImage(t, "hm-c4a-g-new:1")

	rig.settle(t, "hm-c4a-g", old, middle, cleanupSettlement)
	rig.settle(t, "hm-c4a-g", middle, current, cleanupSettlement.Add(time.Hour))

	first := rig.run(t)
	second := rig.run(t)

	if first.Removed != 1 {
		t.Fatalf("the first pass removed %d, want 1", first.Removed)
	}
	if second.Failed != 0 {
		t.Errorf("the second pass reported %d failures, want 0\n\n"+
			"An image that is already gone is the desired end state, not a "+
			"fault to report every twelve hours forever.", second.Failed)
	}
	if second.Removed != 0 {
		t.Errorf("the second pass removed %d, want 0", second.Removed)
	}
	if !imageExists(t, current) {
		t.Error("the current image was removed by the repeated pass")
	}
}
