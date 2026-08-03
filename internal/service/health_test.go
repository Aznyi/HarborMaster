package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// fakeDB is a DatabasePinger double.
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCheckHealthyWhenEverythingIsUp(t *testing.T) {
	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{},
		Docker: &docker.Fake{Info: docker.Info{APIVersion: "1.45", OSType: "linux"}},
		Logger: discardLogger(),
	})

	report := svc.Check(context.Background())

	if report.Status != domain.StatusHealthy {
		t.Errorf("status = %q, want %q", report.Status, domain.StatusHealthy)
	}
	if report.Database.Status != domain.StatusUp {
		t.Errorf("database = %q, want %q", report.Database.Status, domain.StatusUp)
	}
	if report.Docker.Status != domain.StatusUp {
		t.Errorf("docker = %q, want %q", report.Docker.Status, domain.StatusUp)
	}
	if report.Docker.Version != "1.45" {
		t.Errorf("docker version = %q, want %q", report.Docker.Version, "1.45")
	}
}

// Docker being unreachable is a supported operating mode, not an outage: the
// API keeps serving and the UI renders a disconnected state.
func TestCheckDegradedWhenDockerIsDown(t *testing.T) {
	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{},
		Docker: &docker.Fake{Err: docker.ErrUnreachable},
		Logger: discardLogger(),
	})

	report := svc.Check(context.Background())

	if report.Status != domain.StatusDegraded {
		t.Errorf("status = %q, want %q", report.Status, domain.StatusDegraded)
	}
	if report.Database.Status != domain.StatusUp {
		t.Errorf("database = %q, want %q", report.Database.Status, domain.StatusUp)
	}
	if report.Docker.Status != domain.StatusDown {
		t.Errorf("docker = %q, want %q", report.Docker.Status, domain.StatusDown)
	}
}

// Without a database nothing else can work, so this is a hard unhealthy.
func TestCheckUnhealthyWhenDatabaseIsDown(t *testing.T) {
	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{err: errors.New("disk failure")},
		Docker: &docker.Fake{},
		Logger: discardLogger(),
	})

	report := svc.Check(context.Background())

	if report.Status != domain.StatusUnhealthy {
		t.Errorf("status = %q, want %q", report.Status, domain.StatusUnhealthy)
	}
}

// A database failure outranks a Docker failure in the summary.
func TestCheckUnhealthyDominatesDegraded(t *testing.T) {
	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{err: errors.New("disk failure")},
		Docker: &docker.Fake{Err: docker.ErrUnreachable},
		Logger: discardLogger(),
	})

	if got := svc.Check(context.Background()).Status; got != domain.StatusUnhealthy {
		t.Errorf("status = %q, want %q", got, domain.StatusUnhealthy)
	}
}

// The probe detail reaches the API response, so it must never carry the
// underlying error chain.
func TestCheckDetailDoesNotLeakInternalErrors(t *testing.T) {
	const internal = "dial unix /var/run/docker.sock: permission denied"

	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{err: errors.New("attach database /srv/secret/hm.db: locked")},
		Docker: &docker.Fake{Err: errors.New(internal)},
		Logger: discardLogger(),
	})

	report := svc.Check(context.Background())

	if strings.Contains(report.Docker.Detail, internal) {
		t.Errorf("docker detail leaked the engine error: %q", report.Docker.Detail)
	}
	if strings.Contains(report.Database.Detail, "/srv/secret/hm.db") {
		t.Errorf("database detail leaked the database path: %q", report.Database.Detail)
	}
}

func TestCheckReportsUptime(t *testing.T) {
	started := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	now := started.Add(90 * time.Second)

	svc := service.NewHealthService(service.HealthOptions{
		DB:        fakeDB{},
		Docker:    &docker.Fake{},
		Logger:    discardLogger(),
		StartedAt: started,
		Now:       func() time.Time { return now },
	})

	report := svc.Check(context.Background())

	if report.UptimeSec != 90 {
		t.Errorf("UptimeSec = %d, want 90", report.UptimeSec)
	}
	if !report.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt = %v, want %v", report.CheckedAt, now)
	}
}

// A nil adapter must degrade rather than panic.
func TestCheckHandlesMissingDependencies(t *testing.T) {
	svc := service.NewHealthService(service.HealthOptions{Logger: discardLogger()})

	report := svc.Check(context.Background())

	if report.Status != domain.StatusUnhealthy {
		t.Errorf("status = %q, want %q", report.Status, domain.StatusUnhealthy)
	}
	if report.Docker.Status != domain.StatusDown {
		t.Errorf("docker = %q, want %q", report.Docker.Status, domain.StatusDown)
	}
}

func TestCheckProbesDockerEveryCall(t *testing.T) {
	fake := &docker.Fake{}
	svc := service.NewHealthService(service.HealthOptions{
		DB:     fakeDB{},
		Docker: fake,
		Logger: discardLogger(),
	})

	svc.Check(context.Background())
	svc.Check(context.Background())

	if fake.Calls != 2 {
		t.Errorf("Ping calls = %d, want 2 (the report must not be cached)", fake.Calls)
	}
}
