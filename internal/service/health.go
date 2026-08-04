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

// EventEngineReporter is the event-engine capability health depends on.
//
// A narrow interface rather than *EventService, so health stays testable
// without a Docker daemon and without starting the engine.
type EventEngineReporter interface {
	// Enabled reports whether the engine is switched on.
	Enabled() bool
	// Degraded reports whether the engine's state should degrade health, with
	// an operator-safe reason.
	Degraded() (bool, string)
}

// HealthService reports whether HarborMaster's dependencies are reachable.
type HealthService struct {
	db        DatabasePinger
	docker    docker.Pinger
	events    EventEngineReporter
	logger    *slog.Logger
	startedAt time.Time
	// now is injectable so tests can assert on uptime deterministically.
	now func() time.Time
}

// HealthOptions configures a HealthService.
type HealthOptions struct {
	DB     DatabasePinger
	Docker docker.Pinger
	// Events is optional. A nil reporter omits the event component entirely,
	// which is what an API-only or pre-Phase-2.5 deployment looks like.
	Events    EventEngineReporter
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
		events:    opts.Events,
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
//
// The event engine follows the same reasoning and never escalates past
// degraded. A disconnected event stream means the inventory falls back to
// periodic reconciliation, which is a slower but correct mode -- and a
// transient reconnect must NOT make the container health check fail, or a
// daemon restart would put HarborMaster into a restart loop.
func (s *HealthService) Check(ctx context.Context) domain.HealthReport {
	now := s.now().UTC()
	report := domain.HealthReport{
		Database:  s.checkDatabase(ctx),
		Docker:    s.checkDocker(ctx),
		Events:    s.checkEvents(),
		CheckedAt: now,
		UptimeSec: int64(now.Sub(s.startedAt).Seconds()),
	}

	switch {
	case report.Database.Status != domain.StatusUp:
		report.Status = domain.StatusUnhealthy
	case report.Docker.Status != domain.StatusUp:
		report.Status = domain.StatusDegraded
	case report.Events != nil && report.Events.Status != domain.StatusUp:
		report.Status = domain.StatusDegraded
	default:
		report.Status = domain.StatusHealthy
	}
	return report
}

// checkEvents reports the event engine's contribution to health.
//
// Returns nil when no engine is configured, which omits the component from the
// response rather than inventing a status for something that does not exist.
//
// A DISABLED engine reports up, not down. Running on periodic reconciliation
// alone is a supported configuration, and reporting it as degraded forever
// would train an operator to ignore the field.
func (s *HealthService) checkEvents() *domain.Component {
	if s.events == nil {
		return nil
	}

	if !s.events.Enabled() {
		return &domain.Component{
			Status: domain.StatusUp,
			Detail: "event engine disabled by configuration; inventory uses periodic reconciliation",
		}
	}

	degraded, reason := s.events.Degraded()
	if !degraded {
		return &domain.Component{Status: domain.StatusUp}
	}
	// The reason comes from the engine already sanitised; it is prose about
	// state, never a Docker error.
	return &domain.Component{Status: domain.StatusDown, Detail: reason}
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
