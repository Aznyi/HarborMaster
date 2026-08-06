package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Manual rollback: HarborMaster's third Docker mutation capability, and the
// most narrowly scoped of the three.
//
// # Why this is a separate interface rather than more ContainerMutator methods
//
// ContainerMutator can CREATE a container from a captured configuration and
// REMOVE one. Those are the two powers a rollback must not have:
//
//   - It must not create. A rollback moves containers that already exist back
//     to an arrangement HarborMaster recorded. A rollback that could create one
//     would be a restore, which is a different feature with different evidence
//     requirements and is not this phase.
//   - It must not remove. The replacement that a rollback backs out of is the
//     evidence of why the recreation failed. Deleting it destroys the reason
//     the rollback was needed.
//
// Broadening ContainerMutator to serve both would mean the rollback service
// held create and remove as well, and "it does not call them" is a weaker
// guarantee than "it cannot". Capability is granted by what a constructor is
// handed, so the rollback service is handed exactly this.
//
// # Four methods, and each names its role
//
// The verbs are the same primitives the recreation uses -- stop, rename, start
// -- but each method here is typed to ONE role in the rollback. StopReplacement
// and StartOriginal are both "start/stop a container by id" at the Engine
// level, and they are separate methods so a misuse is a compile error rather
// than an argument mistake. There is no general-purpose rename: the two rename
// operations are ParkReplacement and RestoreOriginalName, and each validates
// the shape of the name it is allowed to produce.
//
// # Every method targets a full container id
//
// Validated here, in this package, before the daemon is contacted. Nothing can
// be aimed by name, so there is no window in which a name resolves to a
// container other than the one the preflight checked. That is the TOCTOU
// defence, and it is the reason none of these requests has a name field for the
// target.
//
// # The SDK stays inside this package
//
// The requests below are HarborMaster's own types. There is no Engine option
// struct reachable from outside, and therefore no field an operator, an API
// request, or a stored record could fill with an arbitrary Docker parameter.

// ContainerRollbacker is the whole of HarborMaster's rollback capability.
//
// Four methods. It cannot create, remove, exec, attach, copy, commit, kill,
// pause, or touch an image, a volume, or a network -- not because it chooses
// not to, but because there is no method that could.
//
// TestTheRollbackSurfaceIsExactlyFourMethods and its companions in
// internal/arch fail the build if this grows, if a method name suggests a verb
// outside the set, or if any package other than the rollback service names the
// interface.
type ContainerRollbacker interface {
	// StopReplacement stops the container a recreation created, by exact id.
	StopReplacement(ctx context.Context, request RollbackStopRequest) error
	// ParkReplacement renames the replacement out of the production name, by
	// exact id, so the original can take it back.
	ParkReplacement(ctx context.Context, request RollbackParkRequest) error
	// RestoreOriginalName renames the preserved original back to the production
	// name, by exact id.
	RestoreOriginalName(ctx context.Context, request RollbackRestoreRequest) error
	// StartOriginal starts the preserved original, by exact id.
	StartOriginal(ctx context.Context, request RollbackStartRequest) error
}

// -------------------------------------------------------------- requests --

// RollbackStopRequest asks for the replacement container to be stopped.
type RollbackStopRequest struct {
	// ReplacementID is the container to stop, full length. There is no name
	// field: a rollback stops the container the preflight inspected, not
	// whatever currently answers to a name.
	ReplacementID string
	// Timeout is the grace period before the daemon escalates to SIGKILL.
	// Clamped to the same floor and default the recreation uses.
	Timeout time.Duration
}

// Validate reports whether the request names one container.
func (r RollbackStopRequest) Validate() error {
	if !validContainerID(r.ReplacementID) {
		return fmt.Errorf("%w: a full container id is required to stop the replacement",
			ErrMutationRefused)
	}
	return nil
}

// RollbackParkRequest asks for the replacement to be renamed aside.
type RollbackParkRequest struct {
	// ReplacementID is the container to rename, full length.
	ReplacementID string
	// ParkedName is the name it moves to. Derived by HarborMaster from the
	// container's own name and the rollback id; validated here as well, because
	// derivation makes it safe by construction and validation makes it safe by
	// evidence.
	ParkedName string
}

// Validate reports whether the request describes a legal rename.
//
// The parked name must carry HarborMaster's own rollback marker. That is the
// check that stops this method being a general-purpose rename: it can move a
// container OUT of the way and nowhere else, so it cannot be used to give a
// container some third name that means something to another system.
func (r RollbackParkRequest) Validate() error {
	if !validContainerID(r.ReplacementID) {
		return fmt.Errorf("%w: a full container id is required to park the replacement",
			ErrMutationRefused)
	}
	if !domain.ValidContainerName(r.ParkedName) ||
		len(r.ParkedName) > domain.MaxContainerNameBytes {
		return fmt.Errorf("%w: the parked name is not acceptable", ErrMutationRefused)
	}
	if !containsRollbackMarker(r.ParkedName) {
		return fmt.Errorf("%w: a rollback may only rename a replacement to a parked name",
			ErrMutationRefused)
	}
	return nil
}

// RollbackRestoreRequest asks for the original to take the production name
// back.
type RollbackRestoreRequest struct {
	// OriginalID is the preserved original, full length.
	OriginalID string
	// Name is the production name it returns to. Read from the execution
	// record, which read it from the daemon.
	Name string
}

// Validate reports whether the request describes a legal rename.
//
// The target name must NOT carry a HarborMaster marker. This method exists to
// return a container to a name a human chose; letting it write a parked or
// quarantine name would make the two rename methods interchangeable and lose
// the property that each can only move a container in one direction.
func (r RollbackRestoreRequest) Validate() error {
	if !validContainerID(r.OriginalID) {
		return fmt.Errorf("%w: a full container id is required to restore the original",
			ErrMutationRefused)
	}
	if !domain.ValidContainerName(r.Name) ||
		len(r.Name) > domain.MaxContainerNameBytes {
		return fmt.Errorf("%w: the restored name is not acceptable", ErrMutationRefused)
	}
	if containsRollbackMarker(r.Name) || containsRecreationMarker(r.Name) {
		return fmt.Errorf("%w: a rollback may only restore a container to its own name",
			ErrMutationRefused)
	}
	return nil
}

// RollbackStartRequest asks for the original to be started.
type RollbackStartRequest struct {
	// OriginalID is the preserved original, full length.
	OriginalID string
}

// Validate reports whether the request names one container.
func (r RollbackStartRequest) Validate() error {
	if !validContainerID(r.OriginalID) {
		return fmt.Errorf("%w: a full container id is required to start the original",
			ErrMutationRefused)
	}
	return nil
}

// containsRollbackMarker reports whether a name carries the rollback suffix.
func containsRollbackMarker(name string) bool {
	return containsSuffixMarker(name, domain.RollbackParkedNameSuffix)
}

// containsRecreationMarker reports whether a name carries either recreation
// suffix.
func containsRecreationMarker(name string) bool {
	return containsSuffixMarker(name, domain.ParkedNameSuffix) ||
		containsSuffixMarker(name, domain.QuarantineNameSuffix)
}

// containsSuffixMarker reports whether a marker appears anywhere in a name.
//
// Anywhere rather than at the end, because the derived names carry an id after
// the marker. A name that contains the marker at all is one HarborMaster
// derived, and that is the property being tested.
func containsSuffixMarker(name, marker string) bool {
	return len(name) >= len(marker) && indexOf(name, marker) >= 0
}

// indexOf is strings.Index, spelled locally so this file's validation has no
// dependency that a future edit could swap for something case-insensitive.
func indexOf(haystack, needle string) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// -------------------------------------------------------- implementation --

// StopReplacement stops the container a recreation created.
//
// The same bounded stop the recreation uses: a grace period the daemon
// observes, and an adapter timeout that outlives it so the context cannot
// cancel the stop it just asked for.
func (c *Client) StopReplacement(ctx context.Context, request RollbackStopRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	grace := request.Timeout
	if grace <= 0 {
		grace = DefaultStopTimeout
	}
	if grace < MinStopTimeout {
		grace = MinStopTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, grace+c.timeout)
	defer cancel()

	seconds := int(grace.Round(time.Second) / time.Second)
	if _, err := c.mutateAPI.ContainerStop(ctx, request.ReplacementID, client.ContainerStopOptions{
		Timeout: &seconds,
	}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}

// ParkReplacement renames the replacement out of the production name.
func (c *Client) ParkReplacement(ctx context.Context, request RollbackParkRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if _, err := c.mutateAPI.ContainerRename(ctx, request.ReplacementID, client.ContainerRenameOptions{
		NewName: request.ParkedName,
	}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}

// RestoreOriginalName renames the preserved original back to its own name.
func (c *Client) RestoreOriginalName(ctx context.Context, request RollbackRestoreRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if _, err := c.mutateAPI.ContainerRename(ctx, request.OriginalID, client.ContainerRenameOptions{
		NewName: request.Name,
	}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}

// StartOriginal starts the preserved original.
func (c *Client) StartOriginal(ctx context.Context, request RollbackStartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if _, err := c.mutateAPI.ContainerStart(ctx, request.OriginalID,
		client.ContainerStartOptions{}); err != nil {
		return classifyMutationError(ctx, err)
	}
	return nil
}
