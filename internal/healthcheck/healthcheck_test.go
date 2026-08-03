package healthcheck_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/healthcheck"
)

// jsonServer answers every request with the given status code and body.
func jsonServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func run(t *testing.T, url string, timeout time.Duration) (int, string) {
	t.Helper()

	var out bytes.Buffer
	code := healthcheck.Run(context.Background(), &out, healthcheck.Options{
		URL:     url,
		Timeout: timeout,
	})
	return code, out.String()
}

const (
	healthyBody = `{"status":"healthy","database":{"status":"up"},` +
		`"docker":{"status":"up","version":"1.51"},"checkedAt":"2026-08-03T09:20:11Z","uptimeSeconds":42}`
	degradedBody = `{"status":"degraded","database":{"status":"up"},` +
		`"docker":{"status":"down","detail":"docker engine unreachable"},"checkedAt":"2026-08-03T09:20:11Z","uptimeSeconds":42}`
	unhealthyBody = `{"status":"unhealthy","database":{"status":"down","detail":"database unreachable"},` +
		`"docker":{"status":"down"},"checkedAt":"2026-08-03T09:20:11Z","uptimeSeconds":42}`
)

func TestHealthyExitsZero(t *testing.T) {
	server := jsonServer(t, http.StatusOK, healthyBody)

	code, out := run(t, server.URL, time.Second)

	if code != healthcheck.ExitOK {
		t.Errorf("exit = %d, want %d", code, healthcheck.ExitOK)
	}
	if !strings.Contains(out, "healthy") {
		t.Errorf("output = %q, want it to mention the status", out)
	}
}

// Degraded means Docker is unreachable while HarborMaster still serves. The
// runtime must not restart the container over it.
func TestDegradedExitsZero(t *testing.T) {
	server := jsonServer(t, http.StatusOK, degradedBody)

	code, out := run(t, server.URL, time.Second)

	if code != healthcheck.ExitOK {
		t.Errorf("exit = %d, want %d for a degraded application", code, healthcheck.ExitOK)
	}
	if !strings.Contains(out, "degraded") {
		t.Errorf("output = %q, want it to mention degraded", out)
	}
	if !strings.Contains(out, "docker=down") {
		t.Errorf("output = %q, want the per-component verdict", out)
	}
}

func TestUnhealthyExitsNonZero(t *testing.T) {
	server := jsonServer(t, http.StatusOK, unhealthyBody)

	code, out := run(t, server.URL, time.Second)

	if code != healthcheck.ExitUnhealthy {
		t.Errorf("exit = %d, want %d", code, healthcheck.ExitUnhealthy)
	}
	if code == healthcheck.ExitOK {
		t.Error("an unhealthy application must not report success")
	}
	if !strings.Contains(out, "unhealthy") {
		t.Errorf("output = %q", out)
	}
}

func TestTimeoutExitsNonZero(t *testing.T) {
	// Blocks until the client gives up.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	start := time.Now()
	code, out := run(t, server.URL, 150*time.Millisecond)
	elapsed := time.Since(start)

	if code == healthcheck.ExitOK {
		t.Error("a timed-out probe must not report success")
	}
	if code != healthcheck.ExitUnreachable {
		t.Errorf("exit = %d, want %d", code, healthcheck.ExitUnreachable)
	}
	// The probe must honour its own deadline, not the server's patience.
	if elapsed > 3*time.Second {
		t.Errorf("probe took %v, want it bounded by the configured timeout", elapsed)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("output = %q, want it to say it timed out", out)
	}
}

func TestMalformedResponseExitsNonZero(t *testing.T) {
	tests := map[string]struct {
		body string
		want int
	}{
		"not json":       {"this is not json", healthcheck.ExitMalformed},
		"truncated json": {`{"status":`, healthcheck.ExitMalformed},
		"html":           {"<html><body>502</body></html>", healthcheck.ExitMalformed},
		"empty body":     {"", healthcheck.ExitMalformed},
		"no status":      {`{"database":{"status":"up"}}`, healthcheck.ExitMalformed},
		"unknown status": {`{"status":"banana"}`, healthcheck.ExitMalformed},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			server := jsonServer(t, http.StatusOK, tc.body)

			code, out := run(t, server.URL, time.Second)

			if code != tc.want {
				t.Errorf("exit = %d, want %d (output %q)", code, tc.want, out)
			}
			if code == healthcheck.ExitOK {
				t.Error("a malformed report must not report success")
			}
		})
	}
}

func TestConnectionFailureExitsNonZero(t *testing.T) {
	// Bind and immediately close, so the port is almost certainly free.
	server := jsonServer(t, http.StatusOK, healthyBody)
	url := server.URL
	server.Close()

	code, out := run(t, url, time.Second)

	if code != healthcheck.ExitUnreachable {
		t.Errorf("exit = %d, want %d", code, healthcheck.ExitUnreachable)
	}
	if !strings.Contains(out, "connection failed") {
		t.Errorf("output = %q", out)
	}
}

func TestNonOKStatusExitsNonZero(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		server := jsonServer(t, status, healthyBody)

		code, out := run(t, server.URL, time.Second)

		if code != healthcheck.ExitUnreachable {
			t.Errorf("status %d: exit = %d, want %d", status, code, healthcheck.ExitUnreachable)
		}
		if !strings.Contains(out, "unexpected status") {
			t.Errorf("status %d: output = %q", status, out)
		}
	}
}

func TestInvalidURLExitsNonZero(t *testing.T) {
	code, out := run(t, "://not-a-url", time.Second)

	if code == healthcheck.ExitOK {
		t.Error("an unusable URL must not report success")
	}
	if strings.Contains(out, "not-a-url") {
		t.Errorf("diagnostics must not echo the configured URL: %q", out)
	}
}

// Container health output is widely visible, so it must stay free of the
// endpoint, proxy hosts, and any detail the server attached.
func TestOutputDoesNotLeakConfigurationOrDetail(t *testing.T) {
	const secretDetail = "dial unix /var/run/docker.sock: permission denied"
	server := jsonServer(t, http.StatusOK,
		`{"status":"unhealthy","database":{"status":"down","detail":"`+secretDetail+`"},"docker":{"status":"down"}}`)

	_, out := run(t, server.URL, time.Second)

	for _, leak := range []string{secretDetail, "/var/run/docker.sock", server.URL} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaked %q: %s", leak, out)
		}
	}
}

func TestOutputIsASingleLine(t *testing.T) {
	server := jsonServer(t, http.StatusOK, degradedBody)

	_, out := run(t, server.URL, time.Second)

	if lines := strings.Count(strings.TrimSpace(out), "\n"); lines != 0 {
		t.Errorf("output spans %d extra lines, want one concise line: %q", lines, out)
	}
	if !strings.HasPrefix(out, "healthcheck: ") {
		t.Errorf("output = %q, want a recognisable prefix", out)
	}
}

// An oversized body must be rejected rather than buffered, so a compromised or
// wrong endpoint cannot make the check allocate without bound.
func TestOversizedResponseIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"healthy","padding":"`))
		padding := bytes.Repeat([]byte("a"), 1<<20) // 1 MiB, well past the cap
		_, _ = w.Write(padding)
		_, _ = w.Write([]byte(`"}`))
	}))
	t.Cleanup(server.Close)

	code, _ := run(t, server.URL, 5*time.Second)

	if code == healthcheck.ExitOK {
		t.Error("a response beyond the size cap must not report success")
	}
}

func TestZeroTimeoutUsesADefault(t *testing.T) {
	server := jsonServer(t, http.StatusOK, healthyBody)

	var out bytes.Buffer
	code := healthcheck.Run(context.Background(), &out, healthcheck.Options{URL: server.URL})

	if code != healthcheck.ExitOK {
		t.Errorf("exit = %d, want %d with an unset timeout", code, healthcheck.ExitOK)
	}
}

func TestCancelledContextExitsNonZero(t *testing.T) {
	server := jsonServer(t, http.StatusOK, healthyBody)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	code := healthcheck.Run(ctx, &out, healthcheck.Options{URL: server.URL, Timeout: time.Second})

	if code == healthcheck.ExitOK {
		t.Error("a cancelled probe must not report success")
	}
}
