// Package docker adapts the official Docker Engine SDK to HarborMaster's
// needs.
//
// The Docker socket is a privileged interface: anything able to talk to it can
// control every container on the host, and in most configurations that is
// equivalent to root. Two constraints follow, and this package exists to
// enforce them in one place:
//
//   - This adapter is strictly read-only. No method here creates, updates, removes,
//     starts, stops, or pulls anything, and none executes a command in a
//     container. Extending this package is the only way to gain write access,
//     which makes that change reviewable.
//   - Engine errors are never returned verbatim to callers that render them.
//     They can embed socket paths and daemon internals, so Ping reports a
//     sanitised error and logs the detail instead.
package docker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ErrUnreachable reports that the Docker Engine did not answer. It is the only
// Docker failure the API surfaces, deliberately carrying no daemon detail.
var ErrUnreachable = errors.New("docker engine unreachable")

// Info is the subset of the Engine's ping response HarborMaster reports.
type Info struct {
	APIVersion string
	OSType     string
}

// Pinger is the read-only capability the application layer depends on.
//
// Services accept this interface rather than *Client so they can be tested
// without a Docker daemon.
type Pinger interface {
	Ping(ctx context.Context) (Info, error)
}

// Client is a Runtime backed by the official Engine SDK.
type Client struct {
	api *client.Client
	// mutateAPI is a third SDK client built WITHOUT a transport timeout, used
	// by every operation that CHANGES a container.
	//
	// # Why the timeout has to come off
	//
	// A container stop asks the daemon to send SIGTERM, wait out a grace
	// period, and only then SIGKILL. With a 30-second grace -- the default,
	// matching Docker's own -- the HTTP request is held open for 30 seconds by
	// design. The main client's 10-second transport timeout cut that off at
	// ten, and no amount of care with contexts at the call site could help:
	// the transport deadline is below them all.
	//
	// The visible symptom was severe. Recreating any container that does not
	// exit promptly on SIGTERM -- an extremely common shape -- failed with
	// `timeout` after ten seconds, having already issued the stop. The
	// container ended up stopped, the checkpoint empty, and the record saying
	// HarborMaster did not know what it had done.
	//
	// Every mutation still has a deadline; it just comes from the CONTEXT the
	// call site computes from the configured timeouts, which is where a bound
	// that has to know about grace periods belongs.
	mutateAPI *client.Client

	// streamAPI is a second SDK client built WITHOUT a request timeout, used
	// only for the event stream.
	//
	// The SDK's WithTimeout sets http.Client.Timeout, which bounds the entire
	// exchange including reading the response body. The event stream is a body
	// that never ends, so sharing the timed client would tear it down every
	// DOCKER_TIMEOUT seconds and present as a daemon that will not stay up.
	// Every other call keeps its timeout: an unbounded inspect is its own
	// hazard.
	streamAPI *client.Client
	timeout   time.Duration
	// masker classifies environment variables and log options during
	// normalization, so values are masked at the adapter boundary rather than
	// somewhere further out where one missed call site would leak them.
	masker *domain.Masker
}

// Options configures a Client.
type Options struct {
	// Host is the Engine endpoint, e.g. unix:///var/run/docker.sock.
	Host string
	// Timeout bounds a single Engine call.
	Timeout time.Duration
	// Masker classifies secret-bearing names. Defaults to the built-in
	// patterns; a nil masker would mask nothing, which must never be the
	// accidental outcome of forgetting to set this.
	Masker *domain.Masker

	// APIVersion pins the Engine API version instead of negotiating one.
	//
	// EMPTY IS THE NORMAL CASE, and negotiation is what almost every
	// deployment should use: the client asks the daemon what it speaks and
	// settles on the highest both understand.
	//
	// Two reasons to pin. An operator on a daemon whose negotiation
	// misbehaves can force a version they know works, and the compatibility
	// matrix in CI runs the integration suite against each supported version
	// in turn -- which is the only way to notice a call this build makes that
	// Engine 25 does not have.
	//
	// A pinned version DISABLES negotiation. Pinning one the daemon does not
	// speak makes every call fail, which is loud and immediate rather than
	// subtle.
	APIVersion string
}

// New builds a Client for the configured endpoint.
//
// Constructing a client does not contact the daemon; an unreachable socket is
// a runtime condition reported by Ping, not a startup failure. HarborMaster
// must stay up and report "disconnected" rather than refusing to boot.
func New(opts Options) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Masker == nil {
		opts.Masker = domain.NewDefaultMasker()
	}

	// client.New, not the deprecated NewClientWithOpts: the SDK marks the latter
	// "will be removed in the next release", and a constructor that vanishes on
	// a routine dependency bump is not something the one package holding the
	// privileged socket should depend on.
	//
	// WithAPIVersionNegotiation is likewise gone: negotiation is the default in
	// this SDK version and the option is now a no-op. HarborMaster still talks
	// to older daemons; it simply no longer asks for behaviour it already gets.
	// The pinned version, when one was configured. Applied to all three
	// clients: two of them talking a different version from the third would be
	// a difference nobody would think to look for.
	pinned := []client.Opt{}
	if opts.APIVersion != "" {
		pinned = append(pinned, client.WithAPIVersion(opts.APIVersion))
	}

	api, err := client.New(append([]client.Opt{
		client.WithHost(opts.Host),
		client.WithTimeout(opts.Timeout),
	}, pinned...)...)
	if err != nil {
		// The endpoint is configuration, so it is kept out of the message.
		return nil, errors.New("create docker client: invalid engine endpoint")
	}

	// See Client.streamAPI: the event stream needs a client with no request
	// timeout. Its lifetime is bounded by the caller's context instead.
	streamAPI, err := client.New(append([]client.Opt{
		client.WithHost(opts.Host),
	}, pinned...)...)
	if err != nil {
		_ = api.Close()
		return nil, errors.New("create docker client: invalid engine endpoint")
	}

	// See Client.mutateAPI.
	mutateAPI, err := client.New(append([]client.Opt{
		client.WithHost(opts.Host),
	}, pinned...)...)
	if err != nil {
		_ = api.Close()
		_ = streamAPI.Close()
		return nil, errors.New("create docker client: invalid engine endpoint")
	}

	return &Client{
		api:       api,
		streamAPI: streamAPI,
		mutateAPI: mutateAPI,
		timeout:   opts.Timeout,
		masker:    opts.Masker,
	}, nil
}

// Ping verifies the Engine is reachable.
//
// On failure it returns an error wrapping ErrUnreachable with the daemon's
// detail attached for logging. Callers that render to an API response must use
// SanitizeError rather than the error's own message.
func (c *Client) Ping(ctx context.Context) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// PingOptions is left zero: version negotiation is already requested once,
	// at construction, via client.WithAPIVersionNegotiation. Asking for it again
	// per Ping would re-negotiate on a liveness probe that runs on a timer.
	ping, err := c.api.Ping(ctx, client.PingOptions{})
	if err != nil {
		// BOTH errors are wrapped with %w, deliberately.
		//
		// With `%v` on the second the chain stopped at ErrUnreachable, which
		// silently broke SanitizeError: its context.DeadlineExceeded and
		// context.Canceled branches could never match, so every timeout was
		// reported as a flat "unreachable". Wrapping restores those branches.
		//
		// This does not widen disclosure. The rendered text is identical either
		// way, and SanitizeError -- not the error's own message -- is what
		// callers put in a response.
		return Info{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return Info{APIVersion: ping.APIVersion, OSType: ping.OSType}, nil
}

// Close releases both underlying HTTP transports.
//
// Closing the stream client does not by itself stop an in-flight subscription:
// cancelling the context the subscription was created with is what does that.
// Close is the last step, after the event engine has stopped.
func (c *Client) Close() error {
	var firstErr error
	for _, api := range []*client.Client{c.api, c.streamAPI, c.mutateAPI} {
		if api == nil {
			continue
		}
		if err := api.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SanitizeError maps any Docker failure onto a short, operator-safe phrase
// suitable for an API response. The underlying error stays in the logs.
func SanitizeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "docker engine did not respond in time"
	case errors.Is(err, context.Canceled):
		return "docker probe cancelled"
	default:
		return ErrUnreachable.Error()
	}
}

// Compile-time check that Client satisfies the interface services depend on.
var _ Pinger = (*Client)(nil)
