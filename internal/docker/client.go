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
	api, err := client.New(
		client.WithHost(opts.Host),
		client.WithTimeout(opts.Timeout),
	)
	if err != nil {
		// The endpoint is configuration, so it is kept out of the message.
		return nil, errors.New("create docker client: invalid engine endpoint")
	}

	// See Client.streamAPI: the event stream needs a client with no request
	// timeout. Its lifetime is bounded by the caller's context instead.
	streamAPI, err := client.New(
		client.WithHost(opts.Host),
	)
	if err != nil {
		_ = api.Close()
		return nil, errors.New("create docker client: invalid engine endpoint")
	}

	return &Client{api: api, streamAPI: streamAPI, timeout: opts.Timeout, masker: opts.Masker}, nil
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
	for _, api := range []*client.Client{c.api, c.streamAPI} {
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
