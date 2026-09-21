//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The P1 reliability pass, against a REAL daemon.
//
// Three things the unit suites model and only a daemon can prove:
//
//   - Scenario A: a create refused after the park, followed by the two
//     rollback moves that put the original back under its name and running.
//   - Scenario B: the ownership labels a replacement is created with are
//     really on the container, so an unrecorded replacement can be identified
//     by them.
//   - Scenario C: the restart-policy writes really land, really refuse a
//     production name, and a parked `always` container really reads "no".
//
// Scenarios D (a create or start that takes longer than the old ten-second
// bound) and E (a healthcheck whose start period exceeds the configured
// startup timeout) exercise the service pipeline rather than the adapter and
// live with the env-gated real-daemon rig in internal/service; see the audit's
// P1 section for the exact commands.

const p1ExecutionID = "exec_0123456789abcdef0123"

// TestScenarioAACreateRefusedAfterTheParkIsRestoredByTheRollbackMoves.
func TestScenarioAACreateRefusedAfterTheParkIsRestoredByTheRollbackMoves(t *testing.T) {
	client := recreationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := "hm-p1-create-" + hexSuffix(t)
	original := startFixture(t, name)
	parked, _ := domain.ParkedContainerName(name, p1ExecutionID)
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", parked) })

	captured, err := client.CaptureConfig(ctx, original)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := client.StopContainer(ctx, docker.StopRequest{ContainerID: original, Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := client.RenameContainer(ctx, docker.RenameRequest{ContainerID: original, NewName: parked}); err != nil {
		t.Fatalf("park: %v", err)
	}

	// Something takes the production name between the park and the create --
	// the daemon refuses the create, exactly as a late collision would.
	squatter := lastLine(dockerCLI(t, "create", "--name", name, recreationImage, "true"))
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", squatter) })

	_, err = client.CreateContainer(ctx, docker.CreateRequest{
		Captured: captured, Image: digestTargetFor(t, recreationImage),
		Name: name, ExecutionID: p1ExecutionID,
	})
	if !errors.Is(err, docker.ErrNameConflict) {
		t.Fatalf("create with the name taken gave %v, want ErrNameConflict", err)
	}
	if strings.Contains(err.Error(), squatter) || strings.Contains(err.Error(), "Conflict. The container name") {
		t.Errorf("daemon text reached the error: %v", err)
	}

	// The operator clears the collision; the rollback's two moves restore
	// service on the original, which was never removed.
	dockerCLI(t, "rm", "-f", squatter)
	if err := client.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{OriginalID: original, Name: name}); err != nil {
		t.Fatalf("restore name: %v", err)
	}
	if err := client.StartOriginal(ctx, docker.RollbackStartRequest{OriginalID: original}); err != nil {
		t.Fatalf("start original: %v", err)
	}

	inspection, err := client.InspectContainer(ctx, original)
	if err != nil || inspection == nil {
		t.Fatalf("inspect: %v", err)
	}
	if got := domain.NormaliseContainerName(inspection.Detail.Overview.Name); got != name {
		t.Errorf("the original answers to %q, want %q", got, name)
	}
	if !inspection.Detail.State.Running {
		t.Error("the original is not running after the restore")
	}
}

// TestScenarioBAReplacementCarriesItsOwnershipLabelsOnTheDaemon.
func TestScenarioBAReplacementCarriesItsOwnershipLabelsOnTheDaemon(t *testing.T) {
	client := recreationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := "hm-p1-labels-" + hexSuffix(t)
	// A forged claim on the source, which the create must overwrite.
	original := startFixture(t, name,
		"--label", domain.LabelExecutionOwner+"=exec_ffffffffffffffffffff",
		"--label", "app=web")
	parked, _ := domain.ParkedContainerName(name, p1ExecutionID)
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", parked) })

	captured, err := client.CaptureConfig(ctx, original)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := client.StopContainer(ctx, docker.StopRequest{ContainerID: original, Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := client.RenameContainer(ctx, docker.RenameRequest{ContainerID: original, NewName: parked}); err != nil {
		t.Fatalf("park: %v", err)
	}
	replacement, err := client.CreateContainer(ctx, docker.CreateRequest{
		Captured: captured, Image: digestTargetFor(t, recreationImage),
		Name: name, ExecutionID: p1ExecutionID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", replacement) })

	owner := strings.TrimSpace(dockerCLI(t, "inspect", "--format",
		`{{index .Config.Labels "`+domain.LabelExecutionOwner+`"}}`, replacement))
	if owner != p1ExecutionID {
		t.Errorf("execution label on the daemon = %q, want %q (the forged value was %q)",
			owner, p1ExecutionID, "exec_ffffffffffffffffffff")
	}
	of := strings.TrimSpace(dockerCLI(t, "inspect", "--format",
		`{{index .Config.Labels "`+domain.LabelReplacementOf+`"}}`, replacement))
	if of != original {
		t.Errorf("original label on the daemon = %q, want %q", of, original)
	}
	app := strings.TrimSpace(dockerCLI(t, "inspect", "--format", `{{index .Config.Labels "app"}}`, replacement))
	if app != "web" {
		t.Errorf("an operator label was lost: app=%q", app)
	}
}

// TestScenarioCAParkedAlwaysContainerReadsRestartNoAndIsRestoredExactly.
func TestScenarioCAParkedAlwaysContainerReadsRestartNoAndIsRestoredExactly(t *testing.T) {
	client := recreationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := "hm-p1-restart-" + hexSuffix(t)
	original := startFixture(t, name, "--restart", "always")
	parked, _ := domain.ParkedContainerName(name, p1ExecutionID)
	t.Cleanup(func() { dockerCLIQuiet("rm", "-f", parked) })

	policyOf := func() string {
		return strings.TrimSpace(dockerCLI(t, "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", original))
	}

	// Under its production name the write is refused, whatever the policy.
	err := client.SuspendRestart(ctx, docker.SuspendRestartRequest{ContainerID: original})
	if !errors.Is(err, docker.ErrMutationRefused) {
		t.Fatalf("suspend on a production name gave %v, want a refusal", err)
	}
	if got := policyOf(); got != "always" {
		t.Fatalf("a refused suspend changed the policy to %q", got)
	}

	if err := client.StopContainer(ctx, docker.StopRequest{ContainerID: original, Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := client.RenameContainer(ctx, docker.RenameRequest{ContainerID: original, NewName: parked}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := client.SuspendRestart(ctx, docker.SuspendRestartRequest{ContainerID: original}); err != nil {
		t.Fatalf("suspend on the parked name: %v", err)
	}
	if got := policyOf(); got != "no" {
		t.Fatalf("the parked container's policy is %q, want no", got)
	}

	// A restore onto the parked name is refused; onto the production name it
	// lands exactly.
	err = client.RestoreRestart(ctx, docker.RestoreRestartRequest{
		ContainerID: original, Policy: domain.RestartPolicy{Name: "always"},
	})
	if !errors.Is(err, docker.ErrMutationRefused) {
		t.Fatalf("restore onto a parked name gave %v, want a refusal", err)
	}
	if err := client.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{OriginalID: original, Name: name}); err != nil {
		t.Fatalf("restore name: %v", err)
	}
	if err := client.RestoreRestart(ctx, docker.RestoreRestartRequest{
		ContainerID: original, Policy: domain.RestartPolicy{Name: "always"},
	}); err != nil {
		t.Fatalf("restore restart: %v", err)
	}
	if got := policyOf(); got != "always" {
		t.Fatalf("the restored policy is %q, want always", got)
	}
	if err := client.StartOriginal(ctx, docker.RollbackStartRequest{OriginalID: original}); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// hexSuffix returns a short per-test suffix so parallel runs do not collide.
func hexSuffix(t *testing.T) string {
	t.Helper()
	return strings.ToLower(strings.TrimPrefix(domain.NewExecutionID(), domain.ExecutionIDPrefix))[:8]
}
