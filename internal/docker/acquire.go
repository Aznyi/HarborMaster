package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Image acquisition: HarborMaster's ONLY Docker mutation.
//
// # What changed, and what did not
//
// Until Phase 8 this adapter was strictly read-only. It now has exactly one
// method that changes the host, and the change it makes is the narrowest one
// available: it downloads image layers into the daemon's local store.
//
// It cannot start, stop, restart, create, remove, rename, exec into, or
// otherwise touch a CONTAINER. It cannot delete or prune an image. Pulling an
// image does not affect any running container -- a container keeps running the
// image it was created from, identified by image ID, and a newly pulled image
// is simply another entry in the local store.
//
// # Why this is a separate interface
//
// Runtime stays read-only, and an architecture test pins its method set. The
// mutation lives on its own interface with ONE method, pinned by its own
// architecture test, so a second mutation cannot be added without editing a
// test whose entire subject is "this must stay at one".
//
// A service that only needs to observe therefore cannot pull, because it never
// receives this interface. Capability is granted by what a constructor is
// handed rather than by convention.
//
// # The target is not caller text
//
// PullTarget carries validated COMPONENTS, not a reference string. The
// reference sent to the daemon is assembled here from those components and is
// always digest-pinned. There is no field an operator, an API request, or a
// registry response could fill with an arbitrary pull argument, and there is
// no way to express a tag-only pull.

// ErrPullRefused reports that a pull request did not describe a legal target.
//
// Raised BEFORE the daemon is contacted. A refusal here means HarborMaster
// would not have known what it was downloading.
var ErrPullRefused = errors.New("pull target refused")

// ErrPullFailed reports that the daemon could not complete the pull.
//
// Deliberately carries no daemon text: an Engine error can embed socket paths,
// registry URLs, and daemon internals, and this error reaches an API response.
var ErrPullFailed = errors.New("image pull failed")

// PullTarget names exactly one immutable image.
//
// Every field is validated before use, and together they can only express
// "this repository, at this digest, for this platform". There is no options
// field, no auth field, and no free-form reference.
type PullTarget struct {
	// Registry is the host the image lives on, e.g. "docker.io". Used to build
	// the reference; it never becomes a destination HarborMaster contacts
	// itself, because the DAEMON performs the transfer.
	Registry string
	// Repository is the full path within the registry, e.g. "library/nginx".
	Repository string
	// Digest is the manifest digest to pull, e.g. "sha256:...". Required.
	// Without it there is no such thing as a safe pull: a tag can move between
	// the moment a change was approved and the moment it is fetched.
	Digest string
	// Platform is the OS/architecture to fetch. Empty means the daemon's own,
	// which is the correct default for a single-platform host.
	Platform domain.Platform
}

// Reference renders the digest-pinned reference for this target.
//
// Always `registry/repository@sha256:...`. There is no branch that produces a
// tag, which is what makes "no mutable-tag-only execution" structural rather
// than a rule someone has to remember.
func (t PullTarget) Reference() string {
	return t.Registry + "/" + t.Repository + "@" + t.Digest
}

// Validate reports whether the target is safe to send to the daemon.
//
// Checked here as well as at every layer above, because this is the last point
// before a privileged socket and the only one that cannot be bypassed by a new
// caller.
func (t PullTarget) Validate() error {
	if !domain.ContactableRegistryHost(t.Registry) {
		return fmt.Errorf("%w: registry host is not acceptable", ErrPullRefused)
	}
	if t.Repository == "" || len(t.Repository) > domain.MaxRepositoryBytes {
		return fmt.Errorf("%w: repository path is not acceptable", ErrPullRefused)
	}
	if !validRepositoryPath(t.Repository) {
		return fmt.Errorf("%w: repository path is not acceptable", ErrPullRefused)
	}
	// The whole safety model rests on this. A target without a digest is a
	// target whose content can change after approval.
	if !domain.ValidImageDigest(t.Digest) {
		return fmt.Errorf("%w: a digest is required and must be well formed", ErrPullRefused)
	}
	if len(t.Reference()) > domain.MaxReferenceBytes {
		return fmt.Errorf("%w: reference is too long", ErrPullRefused)
	}
	return nil
}

// validRepositoryPath reports whether a repository path is safe to place in a
// reference.
//
// An allowlist of the distribution specification's own character set. Anything
// else -- a space, a query character, a path traversal segment, a second `@` --
// is refused rather than escaped, because the value is about to be parsed by
// someone else's reference parser and then placed in a URL.
func validRepositoryPath(path string) bool {
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") ||
		strings.Contains(path, "//") || strings.Contains(path, "..") {
		return false
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '/':
		default:
			// Upper case included: the distribution specification requires
			// lower-case repository paths, and a daemon would reject it anyway.
			return false
		}
	}
	return true
}

// PullProgress is one bounded progress observation.
//
// A projection of the daemon's stream, never the stream itself. The daemon
// relays layer status strings that ORIGINATE AT THE REGISTRY, so every string
// here is truncated and stripped of control characters before it leaves this
// package -- it reaches a log, a database column, and eventually a browser.
type PullProgress struct {
	// Status is a short, sanitised description, e.g. "Downloading".
	Status string
	// Current and Total are bytes for the layer this message describes. Zero
	// when the daemon did not report them.
	Current int64
	Total   int64
	// Layers is how many distinct layer ids have been seen so far -- a bounded
	// counter rather than a growing set of registry-supplied identifiers.
	Layers int
}

// PullResult is what the daemon reported once the transfer finished.
//
// Deliberately thin, and deliberately NOT treated as proof. The caller
// re-inspects the image through the read-only Runtime and compares digests
// itself; see the package comment on Client.PullByDigest.
type PullResult struct {
	// Reference is the digest-pinned reference that was requested.
	Reference string
	// Messages is how many progress messages were processed.
	Messages int
	// BytesReported is the largest cumulative byte count observed. An estimate
	// for display: the daemon reports per-layer progress and layers already
	// present are never counted.
	BytesReported int64
	// Layers is how many distinct layers the transfer touched.
	Layers int
}

// ImageAcquirer is HarborMaster's ENTIRE Docker mutation surface.
//
// One method. An architecture test asserts that it stays one method, and a
// second asserts that Runtime -- which every other service receives -- has none
// at all.
//
// Note what is absent and cannot be added without amending both tests: no
// container lifecycle, no image removal, no prune, no tag, no build, no
// registry login.
type ImageAcquirer interface {
	// PullByDigest downloads one immutable image into the local store.
	//
	// It changes no container. Progress is delivered through the callback,
	// which is called at a bounded rate with bounded values.
	PullByDigest(ctx context.Context, target PullTarget, progress func(PullProgress)) (PullResult, error)
}

// Progress bounds.
//
// The daemon's stream is unbounded in length and carries registry-influenced
// text, so every dimension of it is capped: how much of a message is kept, how
// many messages are forwarded, how often, and how many layer ids are tracked.
const (
	// maxProgressStatusBytes truncates one status string. Long enough for the
	// daemon's own vocabulary ("Downloading", "Extracting", "Pull complete"),
	// short enough that a hostile one cannot fill a column.
	maxProgressStatusBytes = 64
	// maxProgressMessages bounds how many messages are FORWARDED. The stream is
	// still drained after this, because abandoning it early would leave the
	// daemon writing into a closed pipe.
	maxProgressMessages = 2000
	// minProgressInterval rate-limits the callback. A large image emits
	// thousands of messages a second; persisting each would make a pull an
	// unbounded write amplifier.
	minProgressInterval = 500 * time.Millisecond
	// maxTrackedLayers bounds the set of distinct layer ids held in memory.
	// The ids come from the registry, and an unbounded set of them is a memory
	// exhaustion vector on a hostile response.
	maxTrackedLayers = 512
)

// PullByDigest downloads one immutable image into the daemon's local store.
//
// # What it does not do
//
// It touches no container. Nothing here starts, stops, recreates, or modifies
// anything that is running, and the daemon endpoint used cannot: pulling adds
// an entry to the image store and nothing else.
//
// # The transfer is not the proof
//
// A successful stream means the daemon accepted the request and reported no
// error. It does NOT establish that the local store now holds the image
// HarborMaster asked for. The caller re-reads the image through the read-only
// inspection path and compares the digest and platform itself, and fails closed
// on any mismatch. This function returning nil is a necessary condition for
// success, never a sufficient one.
//
// # No credentials, ever
//
// RegistryAuth and PrivilegeFunc are left unset. HarborMaster holds no registry
// credentials by design, so a private repository fails as unauthorized -- an
// honest outcome rather than a reason to start handling secrets. Because the
// options struct is built here and never accepts caller input, there is no path
// by which one could be supplied.
func (c *Client) PullByDigest(
	ctx context.Context,
	target PullTarget,
	progress func(PullProgress),
) (PullResult, error) {
	if err := target.Validate(); err != nil {
		return PullResult{}, err
	}

	reference := target.Reference()
	result := PullResult{Reference: reference}

	// Deliberately the UNTIMED client. A pull of a multi-gigabyte image is a
	// long-lived response body, and the shared timeout would tear it down
	// mid-transfer exactly as it would the event stream. The operation is
	// bounded by the caller's context instead, which is where a pull-specific
	// deadline belongs -- one timeout cannot be right for both an inspect and a
	// layer download.
	options := client.ImagePullOptions{}
	if platform, ok := ociPlatform(target.Platform); ok {
		options.Platforms = []ocispec.Platform{platform}
	}

	stream, err := c.streamAPI.ImagePull(ctx, reference, options)
	if err != nil {
		return result, classifyPullError(ctx, err)
	}
	defer func() { _ = stream.Close() }()

	tracker := newProgressTracker(progress)
	for message, msgErr := range stream.JSONMessages(ctx) {
		if msgErr != nil {
			return result, classifyPullError(ctx, msgErr)
		}
		// An error INSIDE the stream is how the daemon reports a failure that
		// happened after the request was accepted -- a missing manifest, a
		// registry refusal. It is a failure even though the HTTP exchange
		// succeeded, and treating the stream as successful because it ended
		// would be the classic mistake here.
		if message.Error != nil {
			return result, fmt.Errorf("%w: the registry or daemon rejected the transfer", ErrPullFailed)
		}
		var current, total int64
		if message.Progress != nil {
			current, total = message.Progress.Current, message.Progress.Total
		}
		tracker.observe(message.ID, message.Status, current, total)
	}

	// Wait reports a stream that ended early. Checked separately because
	// ranging to completion is not the same as the transfer having finished.
	if err := stream.Wait(ctx); err != nil {
		return result, classifyPullError(ctx, err)
	}

	result.Messages = tracker.messages
	result.BytesReported = tracker.bytes
	result.Layers = tracker.layers()
	return result, nil
}

// ociPlatform converts a HarborMaster platform, reporting whether it is usable.
//
// An incomplete platform yields false rather than a partially filled struct:
// asking the daemon for "linux/" would either be ignored or matched loosely,
// and a loose match is exactly what the verification step exists to catch.
func ociPlatform(platform domain.Platform) (ocispec.Platform, bool) {
	if platform.OS == "" || platform.Architecture == "" {
		return ocispec.Platform{}, false
	}
	return ocispec.Platform{
		OS:           platform.OS,
		Architecture: platform.Architecture,
		Variant:      platform.Variant,
	}, true
}

// progressTracker turns the daemon's stream into bounded observations.
type progressTracker struct {
	emit func(PullProgress)

	// seen holds distinct layer ids, capped. Registry-supplied strings, so the
	// set is bounded rather than allowed to grow with the response.
	seen     map[string]struct{}
	overflow int

	messages int
	bytes    int64
	lastEmit time.Time
}

func newProgressTracker(emit func(PullProgress)) *progressTracker {
	return &progressTracker{emit: emit, seen: make(map[string]struct{}, 32)}
}

// layers reports how many distinct layers were observed.
func (t *progressTracker) layers() int { return len(t.seen) + t.overflow }

// observe records one message and emits at most one bounded update.
//
// Takes plain values rather than the SDK's message type, so the whole of the
// bounding logic can be tested without constructing one.
func (t *progressTracker) observe(id, status string, current, total int64) {
	t.messages++

	if id != "" {
		if _, known := t.seen[id]; !known {
			if len(t.seen) < maxTrackedLayers {
				t.seen[id] = struct{}{}
			} else {
				// Past the cap the id is counted but not retained. The count
				// stays honest and memory stays bounded.
				t.overflow++
			}
		}
	}

	if current > t.bytes {
		t.bytes = current
	}

	if t.emit == nil || t.messages > maxProgressMessages {
		return
	}
	// Rate-limited. A pull emits thousands of messages a second, and a callback
	// that persists each one would turn one operator action into an unbounded
	// number of writes.
	now := time.Now()
	if !t.lastEmit.IsZero() && now.Sub(t.lastEmit) < minProgressInterval {
		return
	}
	t.lastEmit = now

	t.emit(PullProgress{
		Status:  domain.SanitiseDisplayText(status, maxProgressStatusBytes),
		Current: current,
		Total:   total,
		Layers:  t.layers(),
	})
}

// classifyPullError maps a daemon or transport failure onto a sanitised error.
//
// Two properties matter. First, the returned error carries NO daemon text: an
// Engine error can embed the socket path, the resolved registry URL, and
// internal state, and this value reaches an API response. Second, cancellation
// is distinguished from failure, because a cancelled pull is an operator
// decision rather than a fault.
func classifyPullError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	switch {
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%w: the image is not available at that digest", ErrPullFailed)
	case cerrdefs.IsUnauthorized(err), cerrdefs.IsPermissionDenied(err):
		// Expected for a private repository. HarborMaster holds no credentials
		// by design, so this is a permanent answer rather than a fault.
		return fmt.Errorf("%w: the registry requires credentials HarborMaster does not hold", ErrPullFailed)
	case cerrdefs.IsInvalidArgument(err):
		return fmt.Errorf("%w: the daemon refused the request", ErrPullFailed)
	case cerrdefs.IsUnavailable(err), cerrdefs.IsDeadlineExceeded(err):
		return fmt.Errorf("%w: %w", ErrUnreachable, errors.New("the daemon did not answer"))
	}
	return fmt.Errorf("%w: the transfer did not complete", ErrPullFailed)
}

// Compile-time check that Client provides the one mutation method.
var _ ImageAcquirer = (*Client)(nil)
