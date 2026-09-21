package service_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// TestAReplacementStillInItsStartPeriodIsNotFailedEarly: the container's
// healthcheck declares a start period longer than HarborMaster's configured
// startup timeout. Docker still calls it starting; HarborMaster must wait for
// the healthcheck's own budget rather than roll back a container that is
// behaving exactly as configured.
func TestAReplacementStillInItsStartPeriodIsNotFailedEarly(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		// The configured timeout is two seconds (see newExecHarness). The
		// healthcheck's start period is longer, and the container reports
		// starting until the end of it.
		check := &domain.HealthCheck{
			Test:          []string{"CMD", "curl", "-f", "http://localhost/"},
			StartPeriodMS: 3000, IntervalMS: 100, TimeoutMS: 100, Retries: 1,
		}
		h.mutator.Containers[execContainerID].Detail.HealthCheck = check
		h.runtime.Inspections[execContainerID].Detail.HealthCheck = check
		h.evidence.container.HealthCheck = check

		replacement := execReplacementDetail()
		replacement.HealthCheck = check
		replacement.State.Health = domain.HealthStarting
		replacement.Overview.Health = domain.HealthStarting
		h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: replacement}
	})

	execution := harness.request(t)

	// Healthy just after the configured timeout would have expired, and well
	// inside the healthcheck's own start period.
	go func() {
		time.Sleep(2500 * time.Millisecond)
		harness.runtime.WithInspection(execReplacementID, func(detail *domain.ContainerDetail) {
			detail.State.Health = domain.HealthHealthy
			detail.Overview.Health = domain.HealthHealthy
		})
	}()

	final := harness.runOnce(t, execution)
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state %q/%q, want succeeded: the container was inside its start period",
			final.State, final.Failure)
	}
	if final.Verification.Health != domain.VerificationPassed {
		t.Errorf("health verdict %q, want passed", final.Verification.Health)
	}
}
