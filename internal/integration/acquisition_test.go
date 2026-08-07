//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Image acquisition against a REAL Docker daemon.
//
// This is the one part of HarborMaster that changes the host, so it is the part
// where a fake proving the behaviour is least satisfying. These tests exercise
// the actual adapter against an actual daemon and assert the properties the
// unit suites can only model:
//
//   - A digest-pinned pull really does land the expected image.
//   - The post-transfer verification really does find it, through the read-only
//     inspection path.
//   - A pull for a digest that does not exist fails, carries no daemon text,
//     and leaves nothing behind.
//   - **No container is touched.** Asserted by counting containers either side.
//
// Every mutation these tests need for SETUP still goes through the `docker`
// CLI, as in the rest of this package. The one exception is the pull itself,
// which is the thing under test.

// acquisitionImage is deliberately tiny and universally available.
const acquisitionImage = "busybox:stable"

// resolveDigest pulls an image with the CLI and returns its manifest digest.
//
// The CLI does the resolving so the digest under test comes from somewhere
// other than the code being tested. A digest HarborMaster derived itself would
// make the comparison circular.
func resolveDigest(t *testing.T, reference string) string {
	t.Helper()

	dockerCLI(t, "pull", "--quiet", reference)

	raw := dockerCLI(t, "image", "inspect", reference, "--format", "{{json .RepoDigests}}")

	var digests []string
	if err := json.Unmarshal([]byte(raw), &digests); err != nil {
		t.Fatalf("decode repo digests %q: %v", raw, err)
	}
	if len(digests) == 0 {
		t.Skipf("%s has no repo digest locally; it was probably built rather than pulled", reference)
	}

	// "repository@sha256:..." -- the part after the last @ is the digest.
	at := strings.LastIndex(digests[0], "@")
	if at < 0 {
		t.Fatalf("malformed repo digest %q", digests[0])
	}
	return digests[0][at+1:]
}

// containerCount reports how many containers exist, in any state.
//
// The whole point of the assertion: acquiring an image must not change it.
func containerCount(t *testing.T) int {
	t.Helper()

	output := dockerCLI(t, "ps", "--all", "--quiet")
	if strings.TrimSpace(output) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(output), "\n"))
}

// acquisitionClient builds an adapter bound to the real daemon.
func acquisitionClient(t *testing.T) *docker.Client {
	t.Helper()
	requireDocker(t)

	client, err := docker.New(docker.Options{
		Host:       defaultDockerHost(),
		APIVersion: pinnedAPIVersion(),
		Timeout:    30 * time.Second,
		Masker:     domain.NewDefaultMasker(),
	})
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Ping(ctx); err != nil {
		t.Skipf("docker unreachable through the adapter: %v", err)
	}
	return client
}

// A digest-pinned pull lands the expected image, and read-only inspection
// confirms it. This is the whole happy path, end to end, against a real daemon.
func TestPullByDigestLandsTheExpectedImage(t *testing.T) {
	client := acquisitionClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	digest := resolveDigest(t, acquisitionImage)
	target := docker.PullTarget{
		Registry:   "docker.io",
		Repository: "library/busybox",
		Digest:     digest,
	}

	// Removed first, so the pull genuinely transfers rather than finding it
	// already present. Failure is ignored: another test or a previous run may
	// hold a reference to it.
	dockerCLIQuiet("image", "rm", "--force", target.Reference())

	before := containerCount(t)

	var updates []docker.PullProgress
	result, err := client.PullByDigest(ctx, target, func(progress docker.PullProgress) {
		updates = append(updates, progress)
	})
	if err != nil {
		t.Fatalf("PullByDigest: %v", err)
	}
	if result.Reference != target.Reference() {
		t.Errorf("result reference = %q, want %q", result.Reference, target.Reference())
	}

	// VERIFICATION, through the read-only path -- exactly as the service does
	// it. A completed transfer is not the proof; this is.
	image, err := client.InspectImage(ctx, target.Reference())
	if err != nil {
		t.Fatalf("InspectImage after pull: %v", err)
	}

	carries := false
	for _, repoDigest := range image.RepoDigests {
		if strings.HasSuffix(repoDigest, "@"+digest) {
			carries = true
			break
		}
	}
	if !carries {
		t.Errorf("the acquired image does not carry the requested digest: %v", image.RepoDigests)
	}
	if image.OS == "" || image.Architecture == "" {
		t.Errorf("inspection reported no platform: %+v", image)
	}

	// THE PROPERTY THAT MATTERS MOST: no container changed.
	if after := containerCount(t); after != before {
		t.Errorf("container count moved from %d to %d; acquiring an image must not touch a container",
			before, after)
	}

	// Progress, if the daemon reported any, is bounded and sanitised.
	for _, progress := range updates {
		if len(progress.Status) > 64 {
			t.Errorf("progress status is %d bytes: %q", len(progress.Status), progress.Status)
		}
		if strings.ContainsAny(progress.Status, "\r\n") {
			t.Errorf("progress status carries newlines: %q", progress.Status)
		}
	}
}

// A digest that does not exist fails, and the failure carries no daemon text.
func TestPullingAnUnknownDigestFailsCleanly(t *testing.T) {
	client := acquisitionClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	before := containerCount(t)

	target := docker.PullTarget{
		Registry:   "docker.io",
		Repository: "library/busybox",
		// Well formed and certainly not published.
		Digest: "sha256:" + strings.Repeat("0", 64),
	}

	_, err := client.PullByDigest(ctx, target, nil)
	if err == nil {
		t.Fatal("pulling a digest that does not exist should fail")
	}
	if !errors.Is(err, docker.ErrPullFailed) && !errors.Is(err, docker.ErrUnreachable) {
		t.Errorf("error = %v, want a classified pull failure", err)
	}

	// The message reaches an API response, so it must carry nothing from the
	// daemon or the registry.
	message := err.Error()
	for _, leak := range []string{"docker.sock", "/var/run", "registry-1.docker.io", "https://"} {
		if strings.Contains(message, leak) {
			t.Errorf("the error leaked %q: %v", leak, err)
		}
	}

	if after := containerCount(t); after != before {
		t.Errorf("a failed pull changed the container count: %d then %d", before, after)
	}
}

// A cancelled pull stops, and reports cancellation rather than a fault.
func TestACancelledPullStops(t *testing.T) {
	client := acquisitionClient(t)

	digest := resolveDigest(t, acquisitionImage)
	target := docker.PullTarget{
		Registry:   "docker.io",
		Repository: "library/busybox",
		Digest:     digest,
	}
	dockerCLIQuiet("image", "rm", "--force", target.Reference())

	// Cancelled before the transfer can complete.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.PullByDigest(ctx, target, nil)
	if err == nil {
		t.Fatal("a cancelled pull should report why it stopped")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// The adapter refuses an illegal target BEFORE contacting the daemon.
//
// Proved against a real daemon by using a registry host that would be a
// genuine SSRF target if the check were missing: nothing is dialled, and the
// failure arrives immediately.
func TestAnIllegalTargetNeverReachesTheDaemon(t *testing.T) {
	client := acquisitionClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for name, target := range map[string]docker.PullTarget{
		"loopback registry": {
			Registry: "127.0.0.1", Repository: "library/busybox",
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		"internal registry with a port": {
			Registry: "registry.internal:5000", Repository: "library/busybox",
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		"no digest": {
			Registry: "docker.io", Repository: "library/busybox",
		},
		"traversal in the repository": {
			Registry: "docker.io", Repository: "library/../../etc/passwd",
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
	} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			_, err := client.PullByDigest(ctx, target, nil)

			if !errors.Is(err, docker.ErrPullRefused) {
				t.Fatalf("error = %v, want ErrPullRefused", err)
			}
			// Refused before any network or socket work: a check that ran after
			// contacting the daemon would not be a defence.
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("the refusal took %s, which suggests the daemon was contacted first", elapsed)
			}
		})
	}
}

// The image store gains an entry and loses nothing. Acquisition adds; it has no
// capability to remove.
func TestAcquisitionOnlyAddsToTheImageStore(t *testing.T) {
	client := acquisitionClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// A second, unrelated image that must still be present afterwards.
	dockerCLI(t, "pull", "--quiet", "hello-world:latest")
	sentinel := dockerCLI(t, "image", "inspect", "hello-world:latest", "--format", "{{.Id}}")

	digest := resolveDigest(t, acquisitionImage)
	target := docker.PullTarget{
		Registry:   "docker.io",
		Repository: "library/busybox",
		Digest:     digest,
	}

	if _, err := client.PullByDigest(ctx, target, nil); err != nil {
		t.Fatalf("PullByDigest: %v", err)
	}

	// The unrelated image survived.
	if _, err := client.InspectImage(ctx, sentinel); err != nil {
		t.Errorf("an unrelated image disappeared during acquisition: %v", err)
	}
}
