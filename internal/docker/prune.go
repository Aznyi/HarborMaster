package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Removing an image HarborMaster superseded.
//
// # The fourth capability, and why it is its own interface
//
// HarborMaster's Docker mutation surface is three separate interfaces, each
// held by exactly one service and each absent unless the deployment opts in:
// ImageAcquirer pulls, ContainerMutator recreates, ContainerRollbacker reverts.
// The acquirer's own comment states what it may never grow into -- "no image
// removal, no prune" -- so removal arrives as a fourth interface rather than a
// fifth acquirer method.
//
// That is not ceremony. A service able to pull an image is not thereby able to
// destroy one, and the two abilities have opposite failure modes: a bad pull
// wastes bandwidth, a bad removal destroys the only copy of the artefact that
// would put a broken deployment back.
//
// # Force is not a parameter that defaults to false
//
// It is not a parameter at all. There is nowhere on ImageRemoveRequest to put
// it, and the call site below passes a literal false that no caller can reach.
// Docker refusing a removal because something still uses the image is not an
// obstacle to work around -- it is the daemon independently confirming the
// decision was wrong, and it is the last safety net under a chain of
// HarborMaster's own checks.
//
// # Removal is by image ID
//
// Not by tag, and not by digest reference. Removing "nginx:1.27" asks the
// daemon to untag, which can leave the artefact behind or -- if the tag has
// since moved -- act on a different artefact than the one that was assessed. An
// image ID names one artefact and cannot float. C3F established that ImageID
// and manifest digest are different identifiers; the decision upstream is made
// about a manifest digest, and the caller resolves that to the local ID it
// belongs to before asking for a removal.

// ErrImageRemovalRefused is a request this capability will not act on.
//
// Refused before the daemon is contacted: an identifier that is not a local
// image id names something this interface has no business removing.
var ErrImageRemovalRefused = errors.New("image removal refused")

// ImageRemoveRequest names one image to remove.
//
// Deliberately one field. Every option the daemon offers -- force, prune
// children, noprune -- is either absent or fixed at the call site, so a caller
// cannot ask for a more destructive removal than this type can express.
type ImageRemoveRequest struct {
	// ImageID is the local image identifier, "sha256:" followed by 64 hex
	// characters. Not a tag and not a reference.
	ImageID string
}

// Validate refuses anything that is not a local image id.
//
// The value reaches here from HarborMaster's own inventory, never from a
// request body -- but it is checked anyway, because a value that has been
// through a database is still a value, and this one is about to be handed to a
// daemon endpoint that destroys things.
func (r ImageRemoveRequest) Validate() error {
	id := strings.TrimSpace(r.ImageID)
	if id == "" {
		return fmt.Errorf("%w: no image id", ErrImageRemovalRefused)
	}
	if !domain.ValidImageDigest(id) {
		// ValidImageDigest is the same check the acquisition target uses: the
		// algorithm prefix and a fixed-length lower-case hex body. A local
		// image id has that shape, and anything else -- a tag, a reference, a
		// prefix, a path -- is refused rather than normalised.
		return fmt.Errorf("%w: image id is not a full digest-form identifier", ErrImageRemovalRefused)
	}
	return nil
}

// ImageRemoveOutcome is what the daemon did.
//
// A removal that was REFUSED because the image is in use is not an error from
// HarborMaster's point of view: it is the answer, and the answer is keep. It is
// reported as its own outcome so a caller can record "the daemon still needed
// it" rather than "cleanup failed".
type ImageRemoveOutcome string

const (
	// ImageRemoved means the daemon removed it.
	ImageRemoved ImageRemoveOutcome = "removed"
	// ImageAlreadyGone means there was nothing to remove. Idempotent: a second
	// cleanup pass over the same candidate settles rather than failing.
	ImageAlreadyGone ImageRemoveOutcome = "alreadyGone"
	// ImageStillInUse means the daemon refused because something references it.
	// A successful safety stop, never retried with force.
	ImageStillInUse ImageRemoveOutcome = "stillInUse"
)

// ImagePruner is HarborMaster's ability to remove an image, entire.
//
// One method. An architecture test asserts it stays one method, a second
// asserts it can never touch a container, and a third fails the build if the
// word "force" appears in this file's removal path.
//
// Note what is absent and cannot be added without amending those tests: no
// prune-all, no dangling sweep, no tag manipulation, no volume or network
// removal, no container lifecycle.
type ImagePruner interface {
	// RemoveImage removes one local image by id, never forcibly.
	//
	// Reports how the daemon answered rather than returning an error for a
	// refusal: "something still uses it" is a verdict, not a fault.
	RemoveImage(ctx context.Context, request ImageRemoveRequest) (ImageRemoveOutcome, error)
}

// RemoveImage removes one image from the local store.
//
// # What it does not do
//
// It touches no container. It does not force, does not cascade to parents, and
// does not untag: the id names one artefact and only that artefact is asked
// about. A running container keeps running whatever it was created from,
// because the daemon refuses to remove an image a container references -- which
// is the outcome this reports rather than defeats.
func (c *Client) RemoveImage(
	ctx context.Context,
	request ImageRemoveRequest,
) (ImageRemoveOutcome, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Force is a literal false with no path from any caller. PruneChildren is
	// false too: removing an image must not cascade into parent layers that
	// another image may be built on.
	_, err := c.mutateAPI.ImageRemove(ctx, request.ImageID, client.ImageRemoveOptions{
		Force:         false,
		PruneChildren: false,
	})
	if err == nil {
		return ImageRemoved, nil
	}

	// Already gone. A cleanup pass that runs twice, or races an operator doing
	// the same thing by hand, settles instead of failing.
	if cerrdefs.IsNotFound(err) {
		return ImageAlreadyGone, nil
	}

	// The daemon still needs it. This is the last safety net under every check
	// HarborMaster made itself, and reaching it means one of those checks was
	// wrong -- so it is reported, and never retried with force.
	if isImageInUse(err) {
		return ImageStillInUse, nil
	}

	// Anything else is a genuine failure. The daemon's own text is deliberately
	// not propagated: see the package comment on error handling.
	return "", fmt.Errorf("%w: the image could not be removed", ErrMutationFailed)
}

// isImageInUse recognises the daemon's refusal-because-referenced.
//
// Matched on the SDK's typed conflict error first. The string fallback exists
// because daemon versions differ in how they classify this, and misreading a
// refusal as a fault would turn a correct safety stop into a reported cleanup
// failure -- noise that trains an operator to ignore the log.
//
// It never matches loosely enough to turn a real failure into a silent skip:
// both branches require the daemon to have said the image is referenced.
func isImageInUse(err error) bool {
	if err == nil {
		return false
	}
	if cerrdefs.IsConflict(err) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "image is being used") ||
		strings.Contains(text, "image is referenced") ||
		strings.Contains(text, "conflict: unable to delete")
}

// Ensure the client satisfies the capability it declares.
var _ ImagePruner = (*Client)(nil)
