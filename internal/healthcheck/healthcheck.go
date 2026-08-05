// Package healthcheck implements the `harbormaster healthcheck` command.
//
// Container runtimes need a way to ask "is this process serving?", and the
// usual answer is `curl -f http://localhost/health`. HarborMaster's runtime
// image is distroless: it has no shell, no curl, and no wget, deliberately, so
// that a compromised process has nothing to execute. This command is the
// replacement — the application binary probing its own endpoint.
//
// It speaks only to loopback, reads a bounded response, and prints a single
// line. It never echoes configuration values or response detail beyond the
// per-component up/down verdict.
package healthcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Exit codes. Anything nonzero marks the container unhealthy; the distinct
// values exist so an operator reading `docker inspect` can tell what happened.
const (
	// ExitOK means the application reported healthy or degraded.
	ExitOK = 0
	// ExitUnhealthy means the application reported unhealthy.
	ExitUnhealthy = 1
	// ExitUnreachable means the endpoint could not be reached or answered
	// with an unexpected status code.
	ExitUnreachable = 2
	// ExitMalformed means the endpoint answered with something that is not a
	// health report.
	ExitMalformed = 3
)

// maxResponseBytes bounds the body read. A health report is a few hundred
// bytes; anything larger means we are not talking to HarborMaster.
const maxResponseBytes = 64 << 10 // 64 KiB

// healthReport is the subset of the health payload this command interprets.
//
// It is decoded structurally rather than reusing domain.HealthReport so that
// the check keeps working against an older or newer server that added fields.
type healthReport struct {
	Status   string `json:"status"`
	Database struct {
		Status string `json:"status"`
	} `json:"database"`
	Docker struct {
		Status string `json:"status"`
	} `json:"docker"`
}

// Options configures a probe.
type Options struct {
	// URL is the health endpoint, normally derived from config.HealthcheckURL.
	URL string
	// Timeout bounds the entire probe. Zero selects a safe default.
	Timeout time.Duration
	// Client overrides the HTTP client. Tests use this; production leaves it
	// nil and gets the hardened client below.
	Client *http.Client
}

// defaultTimeout applies when Options.Timeout is unset.
const defaultTimeout = 3 * time.Second

// Run performs the probe and returns the process exit code.
//
// Diagnostics go to out as a single concise line. Errors are classified, not
// forwarded: the underlying message can name a socket path or a proxy host, and
// container health output is widely visible.
func Run(ctx context.Context, out io.Writer, opts Options) int {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := opts.Client
	if client == nil {
		client = newClient(timeout)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		// The URL is derived from configuration, so it stays out of the message.
		return emit(out, ExitUnreachable, "invalid health endpoint")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return emit(out, ExitUnreachable, "timed out after "+timeout.String())
		}
		return emit(out, ExitUnreachable, "connection failed")
	}
	defer func() {
		// Drain within the cap so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return emit(out, ExitUnreachable, fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}

	var parsed healthReport
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := decoder.Decode(&parsed); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return emit(out, ExitUnreachable, "timed out after "+timeout.String())
		}
		return emit(out, ExitMalformed, "unreadable health report")
	}

	switch parsed.Status {
	case "healthy", "degraded":
		// Degraded means Docker is unreachable but HarborMaster is serving.
		// That is a condition to surface in the UI, not a reason for the
		// runtime to restart or replace the container.
		return emit(out, ExitOK, parsed.Status+components(parsed))
	case "unhealthy":
		return emit(out, ExitUnhealthy, "unhealthy"+components(parsed))
	case "":
		return emit(out, ExitMalformed, "health report has no status")
	default:
		return emit(out, ExitMalformed, "unrecognised status")
	}
}

// components renders the per-dependency verdict, which is a fixed vocabulary
// ("up"/"down") and so cannot carry arbitrary server text.
func components(r healthReport) string {
	database := r.Database.Status
	docker := r.Docker.Status
	if database == "" && docker == "" {
		return ""
	}
	if database == "" {
		database = "unknown"
	}
	if docker == "" {
		docker = "unknown"
	}
	return fmt.Sprintf(" (database=%s docker=%s)", database, docker)
}

// emit writes one diagnostic line and returns the exit code.
func emit(out io.Writer, code int, message string) int {
	// The exit code is the real signal; a failed diagnostic write must not
	// change it.
	_, _ = fmt.Fprintf(out, "healthcheck: %s\n", message)
	return code
}

// newClient builds the probe's HTTP client.
//
// Proxies are disabled explicitly: this request targets loopback, and an
// inherited HTTP_PROXY would otherwise send the probe to a third party — which
// both breaks the check and leaks the fact that HarborMaster is running.
// Redirects are refused for the same reason.
func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          1,
		},
	}
}
