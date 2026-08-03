package docker_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
)

// A missing socket must not stop the process from starting: HarborMaster runs
// degraded and reports the condition.
func TestNewSucceedsWithoutADaemon(t *testing.T) {
	client, err := docker.New(docker.Options{
		Host:    "unix:///nonexistent/harbormaster-test.sock",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New must not contact the daemon: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
}

func TestNewRejectsInvalidHost(t *testing.T) {
	// No scheme separator: the SDK cannot derive a transport from this.
	if _, err := docker.New(docker.Options{Host: "not-a-url"}); err == nil {
		t.Fatal("expected an error for a malformed engine endpoint")
	}
}

// The endpoint comes from configuration, so a construction failure must not
// echo it back into logs or stderr.
func TestNewErrorDoesNotLeakTheEndpoint(t *testing.T) {
	const host = "secret-internal-host-name"

	_, err := docker.New(docker.Options{Host: host})
	if err == nil {
		t.Fatal("expected an error for a malformed engine endpoint")
	}
	if strings.Contains(err.Error(), host) {
		t.Errorf("error leaked the configured endpoint: %q", err.Error())
	}
}

func TestPingReportsUnreachable(t *testing.T) {
	client, err := docker.New(docker.Options{
		Host:    "tcp://127.0.0.1:1", // nothing listens here
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.Ping(context.Background()); !errors.Is(err, docker.ErrUnreachable) {
		t.Errorf("err = %v, want it to wrap docker.ErrUnreachable", err)
	}
}

func TestSanitizeError(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"nil":      {nil, ""},
		"deadline": {context.DeadlineExceeded, "docker engine did not respond in time"},
		"cancel":   {context.Canceled, "docker probe cancelled"},
		"other":    {errors.New("boom"), docker.ErrUnreachable.Error()},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := docker.SanitizeError(tc.err); got != tc.want {
				t.Errorf("SanitizeError = %q, want %q", got, tc.want)
			}
		})
	}
}

// Sanitised text is what the API renders, so it must not echo daemon internals
// such as socket paths or permission detail.
func TestSanitizeErrorDropsEngineDetail(t *testing.T) {
	engineErr := errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock: permission denied")

	got := docker.SanitizeError(engineErr)

	for _, leak := range []string{"/var/run/docker.sock", "permission denied"} {
		if strings.Contains(got, leak) {
			t.Errorf("sanitised message leaked %q: %s", leak, got)
		}
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	fake := &docker.Fake{Info: docker.Info{APIVersion: "1.45"}}

	info, err := fake.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if info.APIVersion != "1.45" {
		t.Errorf("APIVersion = %q", info.APIVersion)
	}
	if fake.PingCalls != 1 {
		t.Errorf("PingCalls = %d, want 1", fake.PingCalls)
	}
}
