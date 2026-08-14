package docker

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// A container sharing another's NETWORK namespace has no hostname of its own.
//
// # The defect this pins
//
// Docker refuses `--hostname` together with `--network container:<id>`:
//
//	Error response from daemon: conflicting options: hostname and the network mode
//
// It refuses ALWAYS, not only when the two disagree -- so a container in that
// mode can never have had an operator-set hostname. What `docker inspect`
// reports for one is the value the DAEMON assigned: the short id of the
// container whose namespace it joined.
//
// copyConfigForCreate already cleared a daemon-derived hostname, but only
// recognised one shape of it -- the container's OWN short id, which is what the
// daemon uses for an ordinary container. For a namespace-sharing container the
// derived hostname is the PROVIDER'S short id, which matched nothing, so it was
// copied into the create and the daemon refused.
//
// # What that cost
//
// Every recreation of every `network_mode: container:<x>` workload, which is
// the entire population Phase 16 exists for. Found live in Stage 5a, at the
// worst possible moment: the create is attempted AFTER the original has been
// stopped and parked, so the failure landed at
//
//	checkpoint=originalParked failure=create originalPreserved=true
//
// -- the workload down, its original recoverable, and nothing on the host
// silently wrong. The fail-safe behaved; the recreation could never have
// succeeded.

const (
	hostnameSelfID     = "1111111111111111111111111111111111111111111111111111111111111111"
	hostnameProviderID = "2222222222222222222222222222222222222222222222222222222222222222"
)

// captureWith builds an inspect response for a container carrying a hostname.
func captureWith(selfID, hostname, networkMode string) container.InspectResponse {
	return container.InspectResponse{
		ID:         selfID,
		Config:     &container.Config{Hostname: hostname, Image: "alpine:3.22.1"},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(networkMode)},
	}
}

func TestANamespaceSharingContainerCarriesNoHostnameIntoACreate(t *testing.T) {
	t.Parallel()

	// Exactly what the daemon reports: the hostname is the PROVIDER's short id.
	response := captureWith(hostnameSelfID, hostnameProviderID[:12],
		"container:"+hostnameProviderID)

	config := copyConfigForCreate(response)

	if config.Hostname != "" {
		t.Fatalf("hostname = %q, want empty.\n\n"+
			"Docker refuses `--hostname` with `--network container:<id>` "+
			"unconditionally, so this value was assigned by the daemon and is "+
			"not configuration. Copying it into the create refuses every "+
			"recreation of every namespace-sharing workload -- after the "+
			"original has already been stopped and parked.", config.Hostname)
	}
}

// The same for IPC and PID sharing, which carry the same daemon behaviour for
// the network case they are usually combined with.
//
// A container may share IPC or PID while having its own network, and then a
// hostname IS its own configuration. So these assert the opposite: only the
// NETWORK namespace suppresses the hostname.
func TestSharingIPCOrPIDDoesNotSuppressAnOwnHostname(t *testing.T) {
	t.Parallel()

	response := captureWith(hostnameSelfID, "chosen-by-an-operator", "bridge")
	response.HostConfig.IpcMode = container.IpcMode("container:" + hostnameProviderID)
	response.HostConfig.PidMode = container.PidMode("container:" + hostnameProviderID)

	config := copyConfigForCreate(response)

	if config.Hostname != "chosen-by-an-operator" {
		t.Fatalf("hostname = %q, want it preserved; this container has its own "+
			"network namespace, so its hostname is its own configuration",
			config.Hostname)
	}
}

// An ordinary container's own derived hostname is still cleared.
//
// The behaviour that already existed, pinned so the widening above cannot be
// mistaken for a replacement of it.
func TestAnOrdinaryContainersDerivedHostnameIsStillCleared(t *testing.T) {
	t.Parallel()

	response := captureWith(hostnameSelfID, hostnameSelfID[:12], "bridge")

	if config := copyConfigForCreate(response); config.Hostname != "" {
		t.Fatalf("hostname = %q, want empty", config.Hostname)
	}
}

// An operator's chosen hostname on an ordinary container survives.
//
// The non-vacuity guard on the whole file: if the hostname were simply always
// cleared, every test above would pass and a real configuration field would be
// silently dropped on every recreation.
func TestAnOperatorsHostnameSurvivesARecreation(t *testing.T) {
	t.Parallel()

	response := captureWith(hostnameSelfID, "db-primary", "bridge")

	if config := copyConfigForCreate(response); config.Hostname != "db-primary" {
		t.Fatalf("hostname = %q, want %q; an operator's choice was discarded",
			config.Hostname, "db-primary")
	}
}

// A hostname that merely LOOKS like a short id on a namespace-sharing container
// is still cleared, and the reason is not pattern matching.
//
// The rule is about the MODE, not the value: in this mode the daemon assigns the
// hostname and refuses any other, so whatever is there came from the daemon.
func TestTheNetworkModeDecidesRatherThanTheHostnameShape(t *testing.T) {
	t.Parallel()

	response := captureWith(hostnameSelfID, "looks-nothing-like-an-id",
		"container:"+hostnameProviderID)

	if config := copyConfigForCreate(response); config.Hostname != "" {
		t.Fatalf("hostname = %q, want empty; the daemon would refuse the create "+
			"whatever this value is", config.Hostname)
	}
}

// The clearing does not touch the capture the caller still holds.
func TestClearingTheHostnameDoesNotMutateTheInspection(t *testing.T) {
	t.Parallel()

	response := captureWith(hostnameSelfID, hostnameProviderID[:12],
		"container:"+hostnameProviderID)

	_ = copyConfigForCreate(response)

	if response.Config.Hostname != hostnameProviderID[:12] {
		t.Fatalf("the inspection was mutated: hostname = %q", response.Config.Hostname)
	}
	if !strings.HasPrefix(string(response.HostConfig.NetworkMode), "container:") {
		t.Fatal("the inspection's network mode was mutated")
	}
}
