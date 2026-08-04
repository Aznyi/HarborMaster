package docker_test

// Not-found classification against a stub daemon.
//
// These tests exist because of a specific migration risk. The v28 SDK exposed
// client.IsErrNotFound; the moby client does not, and the adapter now uses
// containerd's errdefs.IsNotFound instead. That is a swap of the MECHANISM by
// which a 404 is recognised, and nothing else in the suite exercises it.
//
// The failure it guards against is quiet and expensive: if a 404 stopped being
// classified as "not found", a container that vanished between being listed and
// being inspected would surface as ErrUnreachable. The refresh treats that as
// the daemon being down and fails the whole sweep, instead of recording one
// warning and carrying on -- so ordinary churn on a busy host would look like an
// outage.
//
// A stub HTTP server is used rather than a live daemon so the check runs in the
// normal unit suite, with no Docker required.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
)

// stubDaemon starts an HTTP server that answers the SDK's version negotiation
// and then serves handler for everything else. It returns a client pointed at
// it.
func stubDaemon(t *testing.T, handler http.HandlerFunc) *docker.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client pings to negotiate an API version before the first call.
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.51")
			w.Header().Set("Ostype", "linux")
			w.WriteHeader(http.StatusOK)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("split stub address: %v", err)
	}

	client, err := docker.New(docker.Options{
		Host:    "tcp://" + net.JoinHostPort(host, port),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// notFound writes the error shape the Engine uses for a missing object.
func notFound(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"` + message + `"}`))
	}
}

// A container that disappears between list and inspect is routine churn, not a
// fault, and must be reported as such.
func TestInspectContainerMapsNotFoundToVanished(t *testing.T) {
	client := stubDaemon(t, notFound("No such container: deadbeef"))

	_, err := client.InspectContainer(context.Background(), "deadbeefcafe")

	if !errors.Is(err, docker.ErrContainerVanished) {
		t.Fatalf("err = %v, want it to wrap ErrContainerVanished", err)
	}
	if errors.Is(err, docker.ErrUnreachable) {
		t.Error("a 404 must not be reported as an unreachable daemon; that would fail the whole refresh")
	}
}

// An image removed while a container still references it is a warning, not a
// refresh failure.
func TestInspectImageMapsNotFoundToUnavailable(t *testing.T) {
	client := stubDaemon(t, notFound("No such image: sha256:beef"))

	_, err := client.InspectImage(context.Background(), "sha256:beefcafe")

	if !errors.Is(err, docker.ErrImageUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrImageUnavailable", err)
	}
	if errors.Is(err, docker.ErrUnreachable) {
		t.Error("a 404 must not be reported as an unreachable daemon")
	}
}

// The counterpart: a genuine server-side failure must NOT be mistaken for a
// vanished object, or a broken daemon would be silently recorded as churn.
func TestInspectContainerKeepsServerErrorsUnreachable(t *testing.T) {
	client := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"devicemapper: no space left"}`))
	})

	_, err := client.InspectContainer(context.Background(), "deadbeefcafe")

	if errors.Is(err, docker.ErrContainerVanished) {
		t.Fatal("a 500 must not be classified as a vanished container")
	}
	if !errors.Is(err, docker.ErrUnreachable) {
		t.Fatalf("err = %v, want it to wrap ErrUnreachable", err)
	}
}

// Engine detail must not survive into what the API renders, whichever error
// path produced it.
func TestInspectErrorsAreSanitizedForTheAPI(t *testing.T) {
	client := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"cannot read /var/run/docker.sock: permission denied"}`))
	})

	_, err := client.InspectContainer(context.Background(), "deadbeefcafe")
	if err == nil {
		t.Fatal("expected an error")
	}

	sanitized := docker.SanitizeError(err)
	for _, leak := range []string{"/var/run/docker.sock", "permission denied"} {
		if strings.Contains(sanitized, leak) {
			t.Errorf("sanitised message leaked %q: %s", leak, sanitized)
		}
	}
}
