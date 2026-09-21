package service_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// What the log says when a recreation fails after the mutation point.
//
// An operator reading one line must be able to find both containers, know
// which image was running and which was being applied, know where the
// pipeline stopped, and know whether the service is down and whether a
// rollback can put it back -- without opening HarborMaster. And the line must
// carry nothing that is not an identifier: no environment value, no label
// value, no command line.

func TestAFailureAfterTheMutationPointLogsEveryIdentity(t *testing.T) {
	var logs bytes.Buffer
	harness := newExecHarness(t, func(h *execHarness) {
		h.logSink = &logs
		h.mutator.CreateErr = docker.ErrMutationFailed
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)
	if final.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("failure %q, want create", final.Failure)
	}

	var line string
	for _, candidate := range strings.Split(logs.String(), "\n") {
		if strings.Contains(candidate, "container recreation failed after changing the host") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("no post-mutation failure line was logged:\n%s", logs.String())
	}

	for _, want := range []string{
		"executionId=" + execution.ExecutionID,
		"containerId=" + domain.ShortenID(execContainerID),
		"replacementId=",
		"checkpoint=originalParked",
		"fromImage=",
		"toDigest=" + execDigest,
		"failure=create",
		"serviceInterrupted=true",
		"rollbackEligible=true",
		"parkedName=",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the failure line lacks %q:\n%s", want, line)
		}
	}
	// Nothing sensitive. The original carries DB_PASSWORD=hunter2 in its
	// environment; the line must not.
	if strings.Contains(logs.String(), "hunter2") {
		t.Error("a secret value reached the log")
	}
}
