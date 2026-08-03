// Package service holds HarborMaster's application logic: the layer that
// composes adapters (Docker, persistence) into the behaviour the API exposes.
//
// Services depend on interfaces, never on concrete adapters, so every service
// is testable without a Docker daemon or a database file.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// DatabasePinger is the persistence capability the health check needs.
type DatabasePinger interface {
	Ping(ctx context.Context) error
}

// HealthService reports whether HarborMaster's dependencies are reachable.
type HealthService struct {
	db        DatabasePinger
	docker    docker.Pinger
	logger    *slog.Logger
	startedAt time.Time
	// now is injectable so tests can assert on uptime deterministically.
	now func() time.Time
}

// HealthOptions configures a HealthService.
type HealthOptions struct {
	DB        DatabasePinger
	Docker    docker.Pinger
	Logger    *slog.Logger
	StartedAt time.Time
	Now       func() time.Time
}

// NewHealthService builds a HealthService.
func NewHealthService(opts HealthOptions) *HealthService {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	startedAt := opts.StartedAt
	if startedAt.IsZero() {
		startedAt = now()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &HealthService{
		db:        opts.DB,
		docker:    opts.Docker,
		logger:    logger,
		startedAt: startedAt,
		now:       now,
	}
}

// Check probes every dependency and returns a report.
//
// It never returns an error: an unreachable dependency is information the
// caller wants rendered, not a failure of the health endpoint itself. Docker
// being down is "degraded" -- HarborMaster still serves and the UI shows a
// disconnected state -- while the database being down is "unhealthy", since
// nothing else can function without it.
func (s *HealthService) Check(ctx context.Context) domain.HealthReport {
	now := s.now().UTC()
	report := domain.HealthReport{
		Database:  s.checkDatabase(ctx),
		Docker:    s.checkDocker(ctx),
		CheckedAt: now,
		UptimeSec: int64(now.Sub(s.startedAt).Seconds()),
	}

	switch {
	case report.Database.Status != domain.StatusUp:
		report.Status = domain.StatusUnhealthy
	case report.Docker.Status != domain.StatusUp:
		report.Status = domain.StatusDegraded
	default:
		report.Status = domain.StatusHealthy
	}
	return report
}

func (s *HealthService) checkDatabase(ctx context.Context) domain.Component {
	if s.db == nil {
		return domain.Component{Status: domain.StatusDown, Detail: "database not configured"}
	}

	start := s.now()
	err := s.db.Ping(ctx)
	latency := s.now().Sub(start).Milliseconds()

	if err != nil {
		// The driver error can name the database path; it goes to the log only.
		s.logger.ErrorContext(ctx, "database health probe failed", slog.String("error", err.Error()))
		return domain.Component{Status: domain.StatusDown, Detail: "database unreachable", LatencyMS: latency}
	}
	return domain.Component{Status: domain.StatusUp, LatencyMS: latency}
}

func (s *HealthService) checkDocker(ctx context.Context) domain.Component {
	if s.docker == nil {
		return domain.Component{Status: domain.StatusDown, Detail: "docker not configured"}
	}

	start := s.now()
	info, err := s.docker.Ping(ctx)
	latency := s.now().Sub(start).Milliseconds()

	if err != nil {
		// Debug, not error: an absent Docker socket is an expected operating
		// mode, and logging it at error level on every poll is just noise.
		s.logger.DebugContext(ctx, "docker health probe failed", slog.String("error", err.Error()))
		return domain.Component{
			Status:    domain.StatusDown,
			Detail:    docker.SanitizeError(err),
			LatencyMS: latency,
		}
	}
	return domain.Component{
		Status:    domain.StatusUp,
		LatencyMS: latency,
		Version:   info.APIVersion,
	}
}
