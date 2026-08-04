package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// fakeEventReporter is an EventEngineReporter double.
type fakeEventReporter struct {
	enabled  bool
	degraded bool
	reason   string
}

func (f fakeEventReporter) Enabled() bool { return f.enabled }

func (f fakeEventReporter) Degraded() (bool, string) { return f.degraded, f.reason }

func healthWith(events service.EventEngineReporter) *service.HealthService {
	return service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{},
		Docker: &docker.Fake{Info: docker.Info{APIVersion: "1.51"}},
		Events: events,
		Logger: discardLogger(),
	})
}

func TestHealthWithAConnectedEventEngine(t *testing.T) {
	report := healthWith(fakeEventReporter{enabled: true}).Check(context.Background())

	if report.Status != domain.StatusHealthy {
		t.Errorf("status = %q, want healthy", report.Status)
	}
	if report.Events == nil || report.Events.Status != domain.StatusUp {
		t.Errorf("events = %+v, want up", report.Events)
	}
}

// A disconnected stream degrades, and never escalates past it: periodic
// reconciliation still keeps the inventory correct.
func TestHealthDegradedWhenTheEventStreamIsDown(t *testing.T) {
	report := healthWith(fakeEventReporter{
		enabled:  true,
		degraded: true,
		reason:   "docker event stream disconnected; relying on periodic reconciliation",
	}).Check(context.Background())

	if report.Status != domain.StatusDegraded {
		t.Errorf("status = %q, want degraded", report.Status)
	}
	if report.Events == nil || report.Events.Status != domain.StatusDown {
		t.Fatalf("events = %+v, want down", report.Events)
	}
	if report.Events.Detail == "" {
		t.Error("a degraded event engine must explain itself")
	}
	// Degraded, never unhealthy: the container health check treats degraded as
	// exit 0, so a daemon restart must not become a HarborMaster restart loop.
	if report.Status == domain.StatusUnhealthy {
		t.Error("a transient event-stream reconnect must never report unhealthy")
	}
}

// Running on periodic reconciliation alone is a supported configuration.
// Reporting it as degraded forever would train an operator to ignore the field.
func TestHealthWithADisabledEventEngineIsNotDegraded(t *testing.T) {
	report := healthWith(fakeEventReporter{enabled: false}).Check(context.Background())

	if report.Status != domain.StatusHealthy {
		t.Errorf("status = %q, want healthy; a disabled engine is a valid mode", report.Status)
	}
	if report.Events == nil || report.Events.Status != domain.StatusUp {
		t.Fatalf("events = %+v, want up", report.Events)
	}
	if report.Events.Detail == "" {
		t.Error("a disabled engine must say so, or the field reads as a fault")
	}
}

// A deployment with no engine configured must say nothing about it rather than
// inventing a status.
func TestHealthWithoutAnEventEngineOmitsTheComponent(t *testing.T) {
	report := healthWith(nil).Check(context.Background())

	if report.Events != nil {
		t.Errorf("events = %+v, want omitted when no engine is configured", report.Events)
	}
	if report.Status != domain.StatusHealthy {
		t.Errorf("status = %q, want healthy", report.Status)
	}
}

// The database is the one dependency HarborMaster cannot operate without, so it
// must outrank a degraded event engine.
func TestDatabaseFailureOutranksTheEventEngine(t *testing.T) {
	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{err: context.DeadlineExceeded},
		Docker: &docker.Fake{Info: docker.Info{APIVersion: "1.51"}},
		Events: fakeEventReporter{enabled: true, degraded: true, reason: "disconnected"},
		Logger: discardLogger(),
	})

	report := svc.Check(context.Background())
	if report.Status != domain.StatusUnhealthy {
		t.Errorf("status = %q, want unhealthy", report.Status)
	}
}

// A real engine must satisfy the interface health depends on, or the wiring in
// main.go would not compile for a reason no test would catch.
func TestEventServiceSatisfiesTheHealthReporter(t *testing.T) {
	var _ service.EventEngineReporter = (*service.EventService)(nil)
}
