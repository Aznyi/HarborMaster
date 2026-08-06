//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Container recreation against a REAL Docker daemon.
//
// This is the largest thing HarborMaster does, and the part where a fake
// proving the behaviour is least satisfying. The unit suites model the pipeline
// exhaustively; these tests exercise the actual adapter against an actual
// daemon and assert the properties a fake cannot:
//
//   - A container created from a captured configuration really does come back
//     with that configuration, as the daemon reports it.
//   - The preservation projection built from a real inspection of the original
//     really does compare equal against a real inspection of the replacement.
//   - An anonymous volume really is carried forward rather than replaced with
//     an empty one, which is the difference between an update and data loss.
//   - RemoveContainer really cannot remove a RUNNING container, because the
//     adapter cannot force.
//   - A rename to a taken name really is refused by the daemon, with no daemon
//     text reaching the error.
//
// Setup uses the `docker` CLI, as in the rest of this package, so the thing
// under test is never also the thing establishing the fixture.

// recreationImage is deliberately tiny and universally available.
const recreationImage = "busybox:stable"

// recreationClient builds an adapter bound to the real daemon.
func recreationClient(t *testing.T) *docker.Client {
	t.Helper()
	requireDocker(t)

	client, err := docker.New(docker.Options{
		Host:    defaultDockerHost(),
		Timeout: 30 * time.Second,
		Masker:  domain.NewDefaultMasker(),
	})
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// startFixture creates and starts a container with the CLI and returns its id.
//
// Registered for cleanup by NAME PREFIX rather than by id, because the tests
// below rename containers and a cleanup keyed on the original name would miss
// the parked one.
func startFixture(t *testing.T, name string, args ...string) string {
	t.Helper()

	// Pulled separately, so the run below prints an id and nothing else. With
	// CombinedOutput a first-time run interleaves the transfer's progress lines
	// with the id, and the adapter -- correctly -- refuses anything that is not
	// exactly 64 hex characters.
	dockerCLI(t, "pull", "--quiet", recreationImage)

	full := append([]string{"run", "-d", "--name", name}, args...)
	full = append(full, recreationImage, "sh", "-c", "while true; do sleep 1; done")

	id := lastLine(dockerCLI(t, full...))
	if len(id) != 64 {
		t.Fatalf("docker run returned %q, which is not a full container id", id)
	}

	t.Cleanup(func() {
		for _, candidate := range []string{name, name + ".parked", name + ".replacement"} {
			dockerCLIQuiet("rm", "-f", candidate)
		}
		dockerCLIQuiet("rm", "-f", id)
	})
	return id
}

// TestARealRecreationPreservesTheConfiguration is the end-to-end proof.
//
// It runs the pipeline's mutation sequence by hand against a live daemon --
// capture, stop, park, create, start -- and then compares the two containers
// through the same projection the service uses. The service's own orchestration
// is covered exhaustively by the unit suite; what cannot be faked is whether
// Docker actually gives back what it was given.
func TestARealRecreationPreservesTheConfiguration(t *testing.T) {
	client := recreationClient(t)
	ctx := context.Background()

	name := "hm-recreate-" + strings.ToLower(t.Name()[:8])
	dockerCLIQuiet("rm", "-f", name)

	// A container with configuration worth preserving: an environment variable
	// (one of them secret-shaped), a label, a capability restriction, a
	// read-only root, and a named volume.
	volume := name + "-data"
	dockerCLI(t, "volume", "create", volume)
	t.Cleanup(func() { dockerCLIQuiet("volume", "rm", "-f", volume) })

	original := startFixture(t, name,
		"--env", "PORT=8080",
		"--env", "DB_PASSWORD=hunter2",
		"--label", "app=hm-integration",
		"--cap-drop", "ALL",
		"--read-only",
		"--tmpfs", "/tmp",
		"--restart", "unless-stopped",
		"--volume", volume+":/data",
	)

	// ---- capture -----------------------------------------------------------

	captured, err := client.CaptureConfig(ctx, original)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !captured.Valid() {
		t.Fatal("the capture is incomplete")
	}
	if captured.ContainerName != name {
		t.Fatalf("captured name = %q, want %q", captured.ContainerName, name)
	}

	// A stand-in for the installation's keyed hasher. It must be
	// NON-INVERTIBLE: a digester that echoed its input would make the
	// disclosure assertion below fail for a reason that says nothing about the
	// product.
	digest := testDigester()

	expected := captured.Summary(digest)
	if len(expected.Fields) == 0 {
		t.Fatal("the projection is empty")
	}

	// The projection must not carry the secret, even built from a real
	// inspection of a real container.
	for _, field := range expected.Fields {
		if strings.Contains(field.Value, "hunter2") {
			t.Fatalf("the projection carried the raw secret in field %q", field.Name)
		}
	}

	// ---- the mutation sequence ---------------------------------------------

	if err := client.StopContainer(ctx, docker.StopRequest{
		ContainerID: original, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	parked := name + ".parked"
	if err := client.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: original, NewName: parked,
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// The same image the container already runs. This test is about
	// CONFIGURATION fidelity, so holding the image constant isolates it.
	target := digestTargetFor(t, recreationImage)

	replacement, err := client.CreateContainer(ctx, docker.CreateRequest{
		Captured: captured,
		Image:    target,
		Name:     name,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", replacement) })

	if err := client.StartContainer(ctx, docker.StartRequest{
		ContainerID: replacement,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// ---- compare -----------------------------------------------------------

	inspection, err := client.InspectContainer(ctx, replacement)
	if err != nil {
		t.Fatalf("inspect replacement: %v", err)
	}

	actual := domain.BuildPreservationSummary(inspection.Detail, digest)
	report := domain.ComparePreservation(expected, actual)

	if report.Status != domain.VerificationPassed {
		t.Errorf("a real recreation did not preserve the configuration: %s", report.Reason)
		for _, difference := range report.Differences {
			t.Errorf("  %s: expected %q, got %q",
				difference.Field, difference.Expected, difference.Actual)
		}
	}

	// And the things the projection abstracts over, checked directly against
	// the daemon's own words.
	detail := inspection.Detail
	if !detail.Security.ReadonlyRootfs {
		t.Error("the replacement lost its read-only root filesystem")
	}
	if len(detail.Security.CapDrop) == 0 {
		t.Error("the replacement lost its capability restriction")
	}
	if detail.Overview.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy = %q", detail.Overview.RestartPolicy.Name)
	}

	// The named volume is attached to the SAME volume, not a new one.
	found := false
	for _, mount := range detail.Mounts {
		if mount.Destination == "/data" {
			found = true
			if mount.VolumeName != volume {
				t.Errorf("the replacement is on volume %q, want %q", mount.VolumeName, volume)
			}
		}
	}
	if !found {
		t.Error("the replacement lost its /data mount")
	}
}

// TestAnAnonymousVolumeIsCarriedForward is the data-loss test, against a real
// daemon.
//
// A volume the daemon created is not named in the container's own
// configuration. Recreating naively gives the replacement a brand new EMPTY
// volume and orphans the original's data.
func TestAnAnonymousVolumeIsCarriedForward(t *testing.T) {
	client := recreationClient(t)
	ctx := context.Background()

	name := "hm-anonvol-" + strings.ToLower(t.Name()[:6])
	dockerCLIQuiet("rm", "-f", name)

	// `-v /data` with no source: the daemon creates an anonymous volume.
	original := startFixture(t, name, "--volume", "/data")

	// Put something in it, so "the same volume" is checkable rather than
	// assumed.
	dockerCLI(t, "exec", original, "sh", "-c", "echo marker > /data/witness")

	originalVolume := volumeNameAt(t, original, "/data")
	if originalVolume == "" {
		t.Fatal("the fixture has no anonymous volume at /data")
	}

	captured, err := client.CaptureConfig(ctx, original)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	if err := client.StopContainer(ctx, docker.StopRequest{
		ContainerID: original, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := client.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: original, NewName: name + ".parked",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	replacement, err := client.CreateContainer(ctx, docker.CreateRequest{
		Captured: captured,
		Image:    digestTargetFor(t, recreationImage),
		Name:     name,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", replacement) })

	if err := client.StartContainer(ctx, docker.StartRequest{
		ContainerID: replacement,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if got := volumeNameAt(t, replacement, "/data"); got != originalVolume {
		t.Fatalf("the replacement is on volume %q, want the original's %q\n"+
			"\tan anonymous volume that is not carried forward is data loss dressed up "+
			"as an update", got, originalVolume)
	}

	// And the data is really there.
	if witness := dockerCLI(t, "exec", replacement, "cat", "/data/witness"); witness != "marker" {
		t.Errorf("the replacement's /data does not carry the original's contents: %q", witness)
	}
}

// TestTheAdapterCannotRemoveARunningContainer.
//
// There is no force field on RemoveRequest, so the daemon refuses. This is the
// property that makes "the original is preserved" hold even if a caller got the
// ordering wrong.
func TestTheAdapterCannotRemoveARunningContainer(t *testing.T) {
	client := recreationClient(t)
	ctx := context.Background()

	name := "hm-noforce-" + strings.ToLower(t.Name()[:6])
	dockerCLIQuiet("rm", "-f", name)
	id := startFixture(t, name)

	err := client.RemoveContainer(ctx, docker.RemoveRequest{ContainerID: id})
	if err == nil {
		t.Fatal("a running container was removed; the adapter must not be able to force")
	}

	// No daemon text in the error, as everywhere else.
	if strings.Contains(err.Error(), "/var/run") || strings.Contains(err.Error(), "endpoint") {
		t.Errorf("the error carries daemon detail: %v", err)
	}

	// And it really is still there.
	if state := dockerCLI(t, "inspect", "--format", "{{.State.Running}}", id); state != "true" {
		t.Errorf("the container is %q, want running", state)
	}
}

// TestARenameToATakenNameIsRefused.
//
// The daemon enforces name uniqueness, and the adapter classifies the conflict
// distinctly so an operator is told the name is taken rather than given a
// generic failure.
func TestARenameToATakenNameIsRefused(t *testing.T) {
	client := recreationClient(t)
	ctx := context.Background()

	first := "hm-taken-a"
	second := "hm-taken-b"
	dockerCLIQuiet("rm", "-f", first)
	dockerCLIQuiet("rm", "-f", second)

	startFixture(t, first)
	secondID := startFixture(t, second)

	err := client.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: secondID, NewName: first,
	})
	if err == nil {
		t.Fatal("a rename onto a taken name succeeded")
	}
	if !strings.Contains(err.Error(), "conflict") && !strings.Contains(err.Error(), "in use") {
		t.Errorf("the conflict was not classified as one: %v", err)
	}
}

// TestTheAdapterRefusesAShortContainerID.
//
// Against a real daemon, because a short id is a value Docker itself accepts
// everywhere -- which is precisely why the adapter has to refuse it. A mutation
// aimed by short id could resolve to a different container than the one the
// preflight checked.
func TestTheAdapterRefusesAShortContainerID(t *testing.T) {
	client := recreationClient(t)
	ctx := context.Background()

	name := "hm-shortid-" + strings.ToLower(t.Name()[:6])
	dockerCLIQuiet("rm", "-f", name)
	id := startFixture(t, name)

	short := id[:12]
	for label, err := range map[string]error{
		"stop":    client.StopContainer(ctx, docker.StopRequest{ContainerID: short, Timeout: time.Second}),
		"start":   client.StartContainer(ctx, docker.StartRequest{ContainerID: short}),
		"rename":  client.RenameContainer(ctx, docker.RenameRequest{ContainerID: short, NewName: name + "-x"}),
		"remove":  client.RemoveContainer(ctx, docker.RemoveRequest{ContainerID: short}),
		"capture": captureError(ctx, client, short),
	} {
		if err == nil {
			t.Errorf("%s accepted a short container id", label)
		}
	}

	// Nothing happened to it.
	if state := dockerCLI(t, "inspect", "--format", "{{.State.Running}}", id); state != "true" {
		t.Errorf("the container is %q, want running", state)
	}
}

// captureError returns the error from a capture, discarding the value.
func captureError(ctx context.Context, client *docker.Client, id string) error {
	_, err := client.CaptureConfig(ctx, id)
	return err
}

// digestTargetFor resolves an image to a pinned execution target.
func digestTargetFor(t *testing.T, reference string) domain.ExecutionTarget {
	t.Helper()

	digest := resolveDigest(t, reference)
	normalised, err := domain.NormalizeImageRef(reference)
	if err != nil {
		t.Fatalf("normalise %q: %v", reference, err)
	}

	return domain.ExecutionTarget{
		Registry:   normalised.Host,
		Repository: normalised.Path,
		Digest:     digest,
		Reference:  reference,
	}
}

// testDigester returns a non-invertible stand-in for the installation's keyed
// hasher.
//
// SHA-256 rather than HMAC because these tests compare two projections against
// each other rather than against a stored one, so no key needs to persist. The
// property that matters is the same: the same value produces the same token,
// and the token does not reveal the value.
func testDigester() domain.SecretDigester {
	return func(value string) string {
		sum := sha256.Sum256([]byte("harbormaster-integration:" + value))
		return hex.EncodeToString(sum[:16])
	}
}

// lastLine returns the final non-empty line of a command's output.
//
// The CLI helper captures stdout and stderr together, so a warning the daemon
// or the CLI emits arrives ahead of the value being read.
func lastLine(output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// volumeNameAt reports which volume a container has mounted at a destination.
func volumeNameAt(t *testing.T, containerID, destination string) string {
	t.Helper()

	format := `{{range .Mounts}}{{if eq .Destination "` + destination + `"}}{{.Name}}{{end}}{{end}}`
	return dockerCLI(t, "inspect", "--format", format, containerID)
}
