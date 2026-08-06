//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Manual rollback against a REAL Docker daemon.
//
// The unit suite models the pipeline exhaustively over a fake host. What it
// cannot establish is whether the four operations behave, against an actual
// daemon, the way the safety argument assumes:
//
//   - Stopping the replacement and renaming it aside really does free the
//     production name, so the preserved original can take it back.
//   - The restored original really does come back with the configuration it had
//     before the rollback moved it -- proved through the same projection the
//     service compares.
//   - The rollback capability really cannot destroy anything. There is no
//     remove method, and the replacement is still on the host afterwards.
//   - The daemon really does refuse a rename onto a taken name, and no daemon
//     text reaches the error.
//   - Every request really does require a full 64-character container id, so a
//     prefix cannot be made to match a different container.
//
// Setup uses the `docker` CLI, as in the rest of this package, so the thing
// under test is never also the thing establishing the fixture.

// rollbackClient builds an adapter bound to the real daemon.
func rollbackClient(t *testing.T) *docker.Client {
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

// TestARealRollbackRestoresTheOriginalAndKeepsTheReplacement is the end-to-end
// proof.
//
// It first performs a recreation by hand -- stop, park, create, start -- so the
// host is in exactly the arrangement a rollback exists to undo, then runs the
// rollback's four operations through the rollback capability and checks the
// result against the daemon.
func TestARealRollbackRestoresTheOriginalAndKeepsTheReplacement(t *testing.T) {
	client := rollbackClient(t)
	ctx := context.Background()

	name := "hm-rollback-" + strings.ToLower(t.Name()[:8])
	dockerCLIQuiet("rm", "-f", name)

	volume := name + "-data"
	dockerCLI(t, "volume", "create", volume)
	t.Cleanup(func() { dockerCLIQuiet("volume", "rm", "-f", volume) })

	// A container with configuration worth preserving, including a
	// secret-shaped environment variable that must never appear in a
	// projection.
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

	digest := testDigester()

	// ---- the arrangement a recreation leaves behind ------------------------

	captured, err := client.CaptureConfig(ctx, original)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	if err := client.StopContainer(ctx, docker.StopRequest{
		ContainerID: original, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("stop the original: %v", err)
	}

	parked := name + domain.ParkedNameSuffix + "exec_00112233445566778899"
	if err := client.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: original, NewName: parked,
	}); err != nil {
		t.Fatalf("park the original: %v", err)
	}
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", parked) })

	replacement, err := client.CreateContainer(ctx, docker.CreateRequest{
		Captured: captured,
		Image:    digestTargetFor(t, recreationImage),
		Name:     name,
	})
	if err != nil {
		t.Fatalf("create the replacement: %v", err)
	}
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", replacement) })

	if err := client.StartContainer(ctx, docker.StartRequest{
		ContainerID: replacement,
	}); err != nil {
		t.Fatalf("start the replacement: %v", err)
	}

	// ---- the baseline, taken before the rollback moves anything ------------
	//
	// The same projection the service builds during its preflight. The
	// post-restore comparison is made against it, which is what proves the
	// rollback did not change the container it restored.
	before, err := client.InspectContainer(ctx, original)
	if err != nil {
		t.Fatalf("inspect the preserved original: %v", err)
	}
	baseline := domain.BuildPreservationSummary(before.Detail, digest)
	if len(baseline.Fields) == 0 {
		t.Fatal("the baseline projection is empty")
	}
	for _, field := range baseline.Fields {
		if strings.Contains(field.Value, "hunter2") {
			t.Fatalf("the baseline projection carried the raw secret in field %q", field.Name)
		}
	}

	// ---- the rollback ------------------------------------------------------

	rollbackID := "rbk_00112233445566778899"
	replacementParked := name + domain.RollbackParkedNameSuffix + rollbackID
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", replacementParked) })

	if err := client.StopReplacement(ctx, docker.RollbackStopRequest{
		ReplacementID: replacement, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("stop the replacement: %v", err)
	}
	if err := client.ParkReplacement(ctx, docker.RollbackParkRequest{
		ReplacementID: replacement, ParkedName: replacementParked,
	}); err != nil {
		t.Fatalf("park the replacement: %v", err)
	}
	if err := client.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{
		OriginalID: original, Name: name,
	}); err != nil {
		t.Fatalf("restore the original's name: %v", err)
	}
	if err := client.StartOriginal(ctx, docker.RollbackStartRequest{
		OriginalID: original,
	}); err != nil {
		t.Fatalf("start the original: %v", err)
	}

	// ---- what the daemon says now ------------------------------------------

	after, err := client.InspectContainer(ctx, original)
	if err != nil {
		t.Fatalf("inspect the restored original: %v", err)
	}
	detail := after.Detail

	if got := domain.NormaliseContainerName(detail.Overview.Name); got != name {
		t.Errorf("the restored original is named %q, want %q", got, name)
	}
	if !detail.State.Running {
		t.Errorf("the restored original is %q, not running", detail.State.State)
	}

	// The preservation proof, made exactly as the service makes it.
	actual := domain.BuildPreservationSummary(detail, digest)
	report := domain.ComparePreservation(baseline, actual)
	if report.Status != domain.VerificationPassed {
		t.Errorf("a real rollback did not preserve the original: %s", report.Reason)
		for _, difference := range report.Differences {
			t.Errorf("  %s: expected %q, got %q",
				difference.Field, difference.Expected, difference.Actual)
		}
	}

	// The things the projection abstracts over, checked against the daemon.
	if !detail.Security.ReadonlyRootfs {
		t.Error("the restored original lost its read-only root filesystem")
	}
	if len(detail.Security.CapDrop) == 0 {
		t.Error("the restored original lost its capability restriction")
	}
	if detail.Overview.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy = %q", detail.Overview.RestartPolicy.Name)
	}
	found := false
	for _, mount := range detail.Mounts {
		if mount.Destination == "/data" {
			found = true
			if mount.VolumeName != volume {
				t.Errorf("the restored original is on volume %q, want %q",
					mount.VolumeName, volume)
			}
		}
	}
	if !found {
		t.Error("the restored original lost its /data mount")
	}

	// ---- the replacement is preserved --------------------------------------
	//
	// It is the evidence of why the recreation was backed out, and the rollback
	// capability has no remove method to destroy it with.
	kept, err := client.InspectContainer(ctx, replacement)
	if err != nil || kept == nil {
		t.Fatalf("the replacement is gone; a rollback must preserve it: %v", err)
	}
	if got := domain.NormaliseContainerName(kept.Detail.Overview.Name); got != replacementParked {
		t.Errorf("the replacement is named %q, want %q", got, replacementParked)
	}
	if kept.Detail.State.Running {
		t.Error("the replacement is still running after the rollback")
	}
}

// TestARealRestoreOntoATakenNameIsRefused.
//
// The preflight checks the production name is free before it stops anything,
// but a third party can take it in the window between. The daemon must refuse,
// and the error must not carry the daemon's words.
func TestARealRestoreOntoATakenNameIsRefused(t *testing.T) {
	client := rollbackClient(t)
	ctx := context.Background()

	name := "hm-rbname-" + strings.ToLower(t.Name()[:6])
	interloper := name + "-other"
	dockerCLIQuiet("rm", "-f", name)
	dockerCLIQuiet("rm", "-f", interloper)

	original := startFixture(t, name)
	_ = startFixture(t, interloper)

	// The original is parked, as a recreation would have left it.
	if err := client.StopContainer(ctx, docker.StopRequest{
		ContainerID: original, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	parked := name + domain.ParkedNameSuffix + "exec_00112233445566778899"
	if err := client.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: original, NewName: parked,
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", parked) })

	// Restoring it onto a name another container holds must fail.
	err := client.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{
		OriginalID: original, Name: interloper,
	})
	if err == nil {
		t.Fatal("a rename onto a taken name succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "name") {
		t.Errorf("the error does not describe a name conflict: %v", err)
	}
	for _, fragment := range []string{
		"Error response from daemon", "/var/run/docker.sock", "conflict. The container name",
	} {
		if strings.Contains(err.Error(), fragment) {
			t.Errorf("the error carries the daemon's words: %q", fragment)
		}
	}

	// And the original is still where it was: a refused rename changes nothing.
	inspection, err := client.InspectContainer(ctx, original)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got := domain.NormaliseContainerName(inspection.Detail.Overview.Name); got != parked {
		t.Errorf("the original is named %q, want %q", got, parked)
	}
}

// TestEveryRealRollbackOperationRequiresAFullContainerID.
//
// A prefix is enough for the daemon and for the CLI, and that is exactly the
// problem: a short id can match a container the record does not name. Every
// request validates before the socket is touched.
func TestEveryRealRollbackOperationRequiresAFullContainerID(t *testing.T) {
	client := rollbackClient(t)
	ctx := context.Background()

	name := "hm-rbid-" + strings.ToLower(t.Name()[:6])
	dockerCLIQuiet("rm", "-f", name)
	full := startFixture(t, name)
	prefix := full[:12]

	// The prefix really does resolve for the daemon, so the refusals below are
	// HarborMaster's and not the daemon's.
	if resolved := lastLine(dockerCLI(t, "inspect", "--format", "{{.Id}}", prefix)); resolved != full {
		t.Fatalf("the daemon did not resolve the prefix %q; the premise of this test is wrong", prefix)
	}

	cases := []struct {
		name string
		call func(id string) error
	}{
		{
			name: "stop",
			call: func(id string) error {
				return client.StopReplacement(ctx, docker.RollbackStopRequest{
					ReplacementID: id, Timeout: time.Second,
				})
			},
		},
		{
			name: "park",
			call: func(id string) error {
				return client.ParkReplacement(ctx, docker.RollbackParkRequest{
					ReplacementID: id,
					ParkedName:    name + domain.RollbackParkedNameSuffix + "rbk_00112233445566778899",
				})
			},
		},
		{
			name: "restore",
			call: func(id string) error {
				return client.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{
					OriginalID: id, Name: name,
				})
			},
		},
		{
			name: "start",
			call: func(id string) error {
				return client.StartOriginal(ctx, docker.RollbackStartRequest{OriginalID: id})
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, id := range []string{"", prefix, name, full[:63], full + "0"} {
				if err := testCase.call(id); err == nil {
					t.Errorf("%s accepted the id %q", testCase.name, id)
				}
			}
		})
	}

	// The container is untouched by all of that.
	inspection, err := client.InspectContainer(ctx, full)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !inspection.Detail.State.Running {
		t.Error("a container was stopped by a refused request")
	}
	if got := domain.NormaliseContainerName(inspection.Detail.Overview.Name); got != name {
		t.Errorf("a container was renamed by a refused request: %q", got)
	}
}

// TestTheRollbackCapabilityRefusesNamesItDidNotDerive.
//
// Park takes a name that MUST carry the rollback marker, and restore takes one
// that must NOT carry any HarborMaster marker. Between them those two rules
// make the rename operations one-way: nothing here can move a container into a
// name that says something untrue about how it got there.
func TestTheRollbackCapabilityRefusesNamesItDidNotDerive(t *testing.T) {
	client := rollbackClient(t)
	ctx := context.Background()

	name := "hm-rbnm-" + strings.ToLower(t.Name()[:6])
	dockerCLIQuiet("rm", "-f", name)
	id := startFixture(t, name)

	// Park refuses a name with no rollback marker.
	for _, parkedName := range []string{
		name,
		name + ".parked",
		name + domain.ParkedNameSuffix + "exec_00112233445566778899",
		"",
	} {
		if err := client.ParkReplacement(ctx, docker.RollbackParkRequest{
			ReplacementID: id, ParkedName: parkedName,
		}); err == nil {
			t.Errorf("park accepted the name %q", parkedName)
		}
	}

	// Restore refuses a name that carries any derived marker: restoring onto
	// one would tell an operator the container is parked when it is serving.
	for _, restoreName := range []string{
		name + domain.RollbackParkedNameSuffix + "rbk_00112233445566778899",
		name + domain.ParkedNameSuffix + "exec_00112233445566778899",
		name + domain.QuarantineNameSuffix + "exec_00112233445566778899",
		"",
	} {
		if err := client.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{
			OriginalID: id, Name: restoreName,
		}); err == nil {
			t.Errorf("restore accepted the name %q", restoreName)
		}
	}

	// Nothing moved.
	inspection, err := client.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got := domain.NormaliseContainerName(inspection.Detail.Overview.Name); got != name {
		t.Errorf("the container is named %q, want %q", got, name)
	}
}
