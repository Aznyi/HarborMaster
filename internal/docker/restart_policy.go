package docker

import (
	"context"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Restart-policy control over containers HarborMaster has parked.
//
// # The hazard this closes
//
// Docker's restart manager restarts a stopped `always` container when the
// daemon restarts, whatever stopped it, and an `unless-stopped` one unless an
// explicit stop was issued -- a fact the daemon never reports. A parked
// original or a quarantined replacement carries the workload's own policy and
// its own port bindings, so after a host reboot it comes back and races the
// serving container for them. Whichever wins serves under the wrong name.
//
// # Two typed operations, not ContainerUpdate
//
// The Engine's update endpoint can change resource limits and the restart
// policy of any container. HarborMaster exposes neither of those things. What
// it exposes is:
//
//   - SuspendRestart: write the policy "no" onto a container whose CURRENT NAME
//     carries one of HarborMaster's own markers. The marker is read from the
//     daemon by id immediately before the write, so a container that was
//     renamed back to production between the check and the act is refused.
//   - RestoreRestart: write a policy from the closed vocabulary onto a
//     container whose current name carries NO marker. Used by a rollback after
//     it has given the original its name back and before it starts it.
//
// Both target a full container id. Neither takes a name for the target, and
// neither can write any other field of the container. The resource half of the
// Engine's update request is never populated.

// SuspendRestartRequest asks for a parked or quarantined container's restart
// policy to be set to "no".
type SuspendRestartRequest struct {
	ContainerID string
}

// Validate reports whether the request names one container.
func (r SuspendRestartRequest) Validate() error {
	if !validContainerID(r.ContainerID) {
		return fmt.Errorf("%w: a full container id is required to suspend a restart policy",
			ErrMutationRefused)
	}
	return nil
}

// RestoreRestartRequest asks for a container's configured restart policy to be
// written back.
type RestoreRestartRequest struct {
	ContainerID string
	// Policy is the policy to restore. Read from HarborMaster's own execution
	// record, which read it from the original before anything moved.
	Policy domain.RestartPolicy
}

// Validate reports whether the request names one container and a legal policy.
func (r RestoreRestartRequest) Validate() error {
	if !validContainerID(r.ContainerID) {
		return fmt.Errorf("%w: a full container id is required to restore a restart policy",
			ErrMutationRefused)
	}
	// An empty policy is refused even though the daemon would read it as "no":
	// a restore writes back something that was RECORDED, and a record with
	// nothing in it is a restore with nothing to restore.
	if r.Policy.Name == "" || !r.Policy.Valid() {
		return fmt.Errorf("%w: the restart policy is not one HarborMaster can write",
			ErrMutationRefused)
	}
	return nil
}

// restartSuspendable reports whether a container name is one HarborMaster
// parked or quarantined.
//
// The recreation's parked and quarantine markers, and the rollback's parked
// marker. A name without one belongs to a container HarborMaster did not put
// aside, and its restart policy is not HarborMaster's to change.
func restartSuspendable(name string) bool {
	normalised := domain.NormaliseContainerName(name)
	if normalised == "" {
		return false
	}
	return domain.IsHarborMasterDerivedName(normalised) ||
		strings.Contains(normalised, domain.RollbackParkedNameSuffix)
}

// restartRestorable reports whether a container name is a production name.
func restartRestorable(name string) bool {
	normalised := domain.NormaliseContainerName(name)
	if normalised == "" {
		return false
	}
	return !restartSuspendable(normalised)
}

// SuspendRestart sets a parked or quarantined container's restart policy to
// "no".
//
// A container whose policy is already "no" is left untouched: the write would
// be a no-op, and a mutation that changes nothing is still a mutation against a
// privileged socket.
func (c *Client) SuspendRestart(ctx context.Context, request SuspendRestartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.mutationTimeout())
	defer cancel()

	inspected, err := c.mutateAPI.ContainerInspect(ctx, request.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return classifyMutationError(ctx, err)
	}
	if !restartSuspendable(inspected.Container.Name) {
		return fmt.Errorf("%w: only a container HarborMaster parked or quarantined may have its restart suspended",
			ErrMutationRefused)
	}
	if inspected.Container.HostConfig != nil && inspected.Container.HostConfig.RestartPolicy.IsNone() {
		return nil
	}

	return c.writeRestartPolicy(ctx, request.ContainerID, container.RestartPolicy{
		Name: container.RestartPolicyDisabled,
	})
}

// RestoreRestart writes a container's configured restart policy back.
func (c *Client) RestoreRestart(ctx context.Context, request RestoreRestartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.mutationTimeout())
	defer cancel()

	inspected, err := c.mutateAPI.ContainerInspect(ctx, request.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return classifyMutationError(ctx, err)
	}
	if !restartRestorable(inspected.Container.Name) {
		return fmt.Errorf("%w: a restart policy may only be restored onto a container under its own name",
			ErrMutationRefused)
	}

	return c.writeRestartPolicy(ctx, request.ContainerID, container.RestartPolicy{
		Name:              container.RestartPolicyMode(request.Policy.Name),
		MaximumRetryCount: request.Policy.MaximumRetryCount,
	})
}

// writeRestartPolicy is the one place the Engine's update endpoint is reached,
// and the request it sends carries the restart policy and nothing else.
func (c *Client) writeRestartPolicy(ctx context.Context, containerID string, policy container.RestartPolicy) error {
	_, err := c.mutateAPI.ContainerUpdate(ctx, containerID, client.ContainerUpdateOptions{
		RestartPolicy: &policy,
	})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("%w: the container is no longer present", ErrContainerVanished)
		}
		return classifyMutationError(ctx, err)
	}
	return nil
}
