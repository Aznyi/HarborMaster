package docker_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
)

// Image removal against a REAL daemon.
//
// # Why this exists and why it is opt-in
//
// Everything else about cleanup is provable against fakes: the decision is a
// pure function, the pass is a service over interfaces, the evidence is real
// SQLite. One thing is not, and it is the last safety net under all of them --
// whether the daemon REFUSES to remove an image a container still references,
// and whether HarborMaster reads that refusal correctly.
//
// A fake cannot answer that. The refusal is the daemon's, its error
// classification differs across daemon versions, and misreading it would turn
// a correct safety stop into a reported failure -- or, far worse, a retry.
//
// Off unless HARBORMASTER_DOCKER_INTEGRATION=1, because it needs a Docker
// daemon, pulls a small public image, and creates a container. CI runs the rest
// of the suite; a developer runs this against a host they are willing to have
// touched.
//
// # What it touches
//
// Only artefacts it created, all named hm-c4a-*. It removes nothing it did not
// make, and every cleanup step is best-effort so a failure leaves diagnosable
// state rather than cascading.

const integrationImage = "alpine:3.20.3"

func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("HARBORMASTER_DOCKER_INTEGRATION") != "1" {
		t.Skip("set HARBORMASTER_DOCKER_INTEGRATION=1 to run against a real Docker daemon")
	}
}

// dockerCLI runs one docker command, failing the test on error.
func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// dockerCLIQuiet runs one docker command and ignores the outcome. For cleanup.
func dockerCLIQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

// integrationHost is the endpoint to test against.
//
// Taken from HARBORMASTER_DOCKER_HOST when set, so the same test runs against a
// remote or rootless daemon; otherwise the platform default, which is what a
// developer's Docker Desktop or systemd socket actually is.
func integrationHost() string {
	if host := os.Getenv("HARBORMASTER_DOCKER_HOST"); host != "" {
		return host
	}
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func integrationClient(t *testing.T) *docker.Client {
	t.Helper()
	client, err := docker.New(docker.Options{
		Host:    integrationHost(),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestARealDaemonRefusesToRemoveAnImageAContainerUses is the scenario that
// cannot be faked.
//
// A container exists, built from a tag of the image. The removal is asked for
// by IMAGE ID -- the identifier the artefact actually has -- and the daemon
// must refuse. HarborMaster must read that as "still in use" and keep it.
func TestARealDaemonRefusesToRemoveAnImageAContainerUses(t *testing.T) {
	skipUnlessIntegration(t)

	const tag = "hm-c4a-inuse:1"
	const name = "hm-c4a-inuse"

	dockerCLI(t, "pull", integrationImage)
	dockerCLI(t, "tag", integrationImage, tag)
	imageID := dockerCLI(t, "image", "inspect", tag, "--format", "{{.Id}}")

	dockerCLIQuiet("rm", "-f", name)
	dockerCLI(t, "create", "--name", name, tag, "sleep", "3600")
	t.Cleanup(func() {
		dockerCLIQuiet("rm", "-f", name)
		dockerCLIQuiet("rmi", tag)
	})

	client := integrationClient(t)

	outcome, err := client.RemoveImage(context.Background(),
		docker.ImageRemoveRequest{ImageID: imageID})
	if err != nil {
		t.Fatalf("RemoveImage returned an error rather than a verdict: %v\n\n"+
			"A daemon that refuses because something still uses the image has "+
			"ANSWERED. Reporting that as a fault trains an operator to ignore "+
			"the log, which is where a real failure would appear.", err)
	}
	if outcome != docker.ImageStillInUse {
		t.Fatalf("outcome = %q, want %q\n\n"+
			"This is the last safety net under every check HarborMaster makes "+
			"itself. If the refusal is misclassified, a defect upstream reaches "+
			"the image store unopposed.", outcome, docker.ImageStillInUse)
	}

	// The artefact is still there, and so is the container.
	if got := dockerCLI(t, "image", "inspect", imageID, "--format", "{{.Id}}"); got != imageID {
		t.Errorf("the image is gone after a refusal: %q", got)
	}
	if got := dockerCLI(t, "inspect", name, "--format", "{{.Name}}"); !strings.Contains(got, name) {
		t.Errorf("the container is gone after a refused image removal: %q", got)
	}
}

// TestARealDaemonRemovesAnUnreferencedImage is the non-vacuity control.
//
// Without it, a RemoveImage that returned "still in use" for everything would
// pass the test above and prove nothing.
func TestARealDaemonRemovesAnUnreferencedImage(t *testing.T) {
	skipUnlessIntegration(t)

	// A DISTINCT artefact, made by committing a throwaway container, rather
	// than a second tag on an existing image: removing by id must destroy the
	// artefact, and it cannot do that while another repository still names it.
	// Committing also guarantees this test never removes anything the host had
	// before it ran.
	const tag = "hm-c4a-removable:1"
	const scratch = "hm-c4a-scratch"

	dockerCLI(t, "pull", integrationImage)
	dockerCLIQuiet("rm", "-f", scratch)
	dockerCLI(t, "create", "--name", scratch, integrationImage, "true")
	dockerCLI(t, "commit", scratch, tag)
	dockerCLI(t, "rm", "-f", scratch)
	t.Cleanup(func() {
		dockerCLIQuiet("rm", "-f", scratch)
		dockerCLIQuiet("rmi", "-f", tag)
	})

	imageID := dockerCLI(t, "image", "inspect", tag, "--format", "{{.Id}}")

	client := integrationClient(t)

	outcome, err := client.RemoveImage(context.Background(),
		docker.ImageRemoveRequest{ImageID: imageID})
	if err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if outcome != docker.ImageRemoved {
		t.Fatalf("outcome = %q, want %q: an unreferenced image was not removed, "+
			"so every refusal test above proves nothing", outcome, docker.ImageRemoved)
	}

	// Asking again settles rather than failing. Cleanup runs forever on a
	// schedule over a historical candidate set; a second pass MUST be free.
	second, err := client.RemoveImage(context.Background(),
		docker.ImageRemoveRequest{ImageID: imageID})
	if err != nil {
		t.Fatalf("the second removal errored: %v", err)
	}
	if second != docker.ImageAlreadyGone {
		t.Errorf("the second removal reported %q, want %q\n\n"+
			"An image that is already gone is the desired end state, not a "+
			"fault to report every twelve hours forever.", second, docker.ImageAlreadyGone)
	}
}

// TestARealRemovalIsRefusedForAnIdentifierThatIsNotAnImageID checks the guard
// in front of the daemon.
//
// Nothing reaches the daemon: a tag, a reference, or a short id is refused
// before the call. Removing "nginx:1.27" asks the daemon to untag, which can
// leave the artefact behind or -- if the tag moved -- act on a different one
// than the one that was assessed.
func TestARealRemovalIsRefusedForAnIdentifierThatIsNotAnImageID(t *testing.T) {
	skipUnlessIntegration(t)

	client := integrationClient(t)

	for _, identifier := range []string{
		"", "alpine:3.20.3", "alpine", "sha256:abc", "../../etc/passwd",
		"sha256:" + strings.Repeat("Z", 64),
	} {
		outcome, err := client.RemoveImage(context.Background(),
			docker.ImageRemoveRequest{ImageID: identifier})
		if err == nil {
			t.Errorf("RemoveImage(%q) was accepted and returned %q\n\n"+
				"Only a full digest-form local image id names one artefact that "+
				"cannot float. Everything else is refused rather than normalised.",
				identifier, outcome)
		}
	}
}
