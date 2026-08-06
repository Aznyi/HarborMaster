package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The audit recorder.
//
// # Why a thin wrapper rather than calling the repository directly
//
// Three reasons, and the third is the important one:
//
//  1. Every caller wants the same failure behaviour -- log and continue -- and
//     writing that at thirty call sites invites one of them to get it wrong.
//  2. The clock is injected once here rather than threaded everywhere.
//  3. It is the single place a "never record a secret" test can point at. An
//     architecture test asserts that no package outside internal/service and
//     internal/store constructs a domain.AuditEvent, so every audit row in the
//     system passes through this file and the repository's bounding.
//
// # A failed audit write never fails the action
//
// Deliberate, and worth defending because the opposite reads as more rigorous.
// If an audit write could fail an action then filling the disk would become a
// way to disable HarborMaster, and a failed write during logout would leave the
// operator logged IN. The failure is logged at ERROR -- it is a real problem --
// and the action proceeds.

// AuditStore is the persistence the recorder needs.
type AuditStore interface {
	Record(ctx context.Context, event domain.AuditEvent, now time.Time) error
	List(ctx context.Context, filter store.AuditFilter) ([]domain.AuditEvent, int, error)
	Summary(ctx context.Context, window time.Duration, now time.Time) (domain.AuditSummary, error)
	Prune(ctx context.Context, operationalCutoff, securityCutoff time.Time, batch int) (int64, error)
}

// AuditRecorder appends immutable security records.
type AuditRecorder struct {
	store  AuditStore
	cfg    config.Auth
	logger *slog.Logger
	now    func() time.Time
}

// NewAuditRecorder builds a recorder.
func NewAuditRecorder(
	auditStore AuditStore,
	cfg config.Auth,
	logger *slog.Logger,
	now func() time.Time,
) *AuditRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AuditRecorder{store: auditStore, cfg: cfg, logger: logger, now: now}
}

// Record appends one event.
//
// Never returns an error: see the file header. A nil recorder is a no-op so a
// partially wired service in a test does not panic.
func (r *AuditRecorder) Record(ctx context.Context, event domain.AuditEvent) {
	if r == nil || r.store == nil {
		return
	}

	// Detached and bounded. Many audit rows are written on a path whose context
	// is about to be cancelled -- a logout, a failed request, a shutdown -- and
	// the record must still land.
	writeCtx, cancel := GraceContext(ctx, auditWriteGrace, auditWriteGrace)
	defer cancel()

	if err := r.store.Record(writeCtx, event, r.now().UTC()); err != nil {
		// ERROR rather than WARN. A missing audit row is a hole in the record
		// an administrator relies on, and the log line is the only remaining
		// trace of what happened.
		r.logger.ErrorContext(ctx, "could not record a security audit event",
			slog.String("action", string(event.Action)),
			slog.String("outcome", string(event.Outcome)),
			slog.String("error", err.Error()))
	}

	// Privileged actions are ALSO logged, at a level a default configuration
	// shows. An operator watching logs should not have to open a page to notice
	// that a container was replaced.
	//
	// The two free-form fields are sanitised HERE rather than relied upon to
	// have been sanitised by the caller. The store does its own sanitising on
	// the way to a column, but that happens after this line, so a log record
	// would otherwise depend on a guarantee established downstream of it.
	//
	// In practice every value reaching this point is a server-generated
	// identifier or a username the account service already constrained. That is
	// exactly why it is worth bounding here: the property is currently true by
	// the good behaviour of every call site, and this makes it true by
	// construction at the point where a log line is written.
	if event.Action.Privileged() && event.Outcome == domain.AuditSucceeded {
		r.logger.WarnContext(ctx, "privileged action performed",
			slog.String("action", string(event.Action)),
			slog.String("actor",
				domain.SanitiseDisplayText(event.ActorUsername, domain.MaxAuditActorBytes)),
			slog.String("targetType", string(event.TargetType)),
			slog.String("targetId",
				domain.SanitiseDisplayText(event.TargetID, domain.MaxAuditTargetIDBytes)))
	}
}

// auditWriteGrace bounds one detached audit write.
const auditWriteGrace = 5 * time.Second

// List returns a page of audit events.
func (r *AuditRecorder) List(
	ctx context.Context,
	filter store.AuditFilter,
) ([]domain.AuditEvent, int, error) {
	return r.store.List(ctx, filter)
}

// Summary returns the aggregate over the configured window.
func (r *AuditRecorder) Summary(ctx context.Context) (domain.AuditSummary, error) {
	return r.store.Summary(ctx, r.cfg.AuditSummaryWindow, r.now().UTC())
}

// Prune removes audit events past their retention.
//
// Two cutoffs. Security events are kept far longer than operational ones: an
// inventory refresh from six months ago is noise, while a failed login from six
// months ago is the first entry in a story.
func (r *AuditRecorder) Prune(ctx context.Context) {
	if r == nil || r.store == nil {
		return
	}
	if r.cfg.AuditRetention <= 0 && r.cfg.SecurityAuditRetention <= 0 {
		return
	}

	now := r.now().UTC()
	operational := now.Add(-r.cfg.AuditRetention)
	security := now.Add(-r.cfg.SecurityAuditRetention)

	// A zero retention means "keep forever", expressed as a cutoff far enough
	// in the past that nothing matches. Using the zero time would be clearer
	// but would also compare unequal against the stored text format.
	if r.cfg.AuditRetention <= 0 {
		operational = time.Unix(0, 0).UTC()
	}
	if r.cfg.SecurityAuditRetention <= 0 {
		security = time.Unix(0, 0).UTC()
	}

	pruned, err := r.store.Prune(ctx, operational, security, auditPruneBatch)
	if err != nil {
		r.logger.WarnContext(ctx, "audit retention pass failed",
			slog.String("error", err.Error()))
		return
	}
	if pruned > 0 {
		r.logger.InfoContext(ctx, "pruned old audit events", slog.Int64("removed", pruned))
	}
}

// auditPruneBatch bounds one retention transaction, so a large history cannot
// hold the single SQLite writer for an unbounded time.
const auditPruneBatch = 500

// Run applies audit retention on an interval until ctx is cancelled.
//
// Retention here DELETES security history, so the loop refuses to run at all
// unless an interval was configured, and Prune itself keeps everything when a
// retention is zero. The failure mode of a misconfiguration should be "the
// table grows", never "the evidence is gone".
func (r *AuditRecorder) Run(ctx context.Context) {
	if r == nil || r.store == nil || r.cfg.AuditPruneInterval <= 0 {
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(r.cfg.AuditPruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Prune(ctx)
		}
	}
}

// ------------------------------------------------------------- convenience --

// Actor is the identity attached to an operational audit event.
//
// A small struct rather than five parameters, because every write path in
// HarborMaster now passes the same five values and a positional call with five
// strings is a call whose arguments get swapped.
type Actor struct {
	UserID    string
	Username  string
	Role      domain.Role
	SessionID string

	RequestID  string
	ClientAddr string
}

// RecordAction appends an operational audit event for an actor.
//
// The path every existing write feature uses to say who asked. Keeping it here
// rather than in each feature's service is what makes "every write is
// attributed" checkable: an architecture test can look for the call.
func (r *AuditRecorder) RecordAction(
	ctx context.Context,
	actor Actor,
	action domain.AuditAction,
	outcome domain.AuditOutcome,
	targetType domain.AuditTargetType,
	targetID, targetName, reason string,
) {
	r.Record(ctx, domain.AuditEvent{
		Action:  action,
		Outcome: outcome,

		ActorUserID:    actor.UserID,
		ActorUsername:  actor.Username,
		ActorRole:      actor.Role,
		ActorSessionID: actor.SessionID,

		TargetType: targetType,
		TargetID:   targetID,
		TargetName: targetName,

		RequestID:  actor.RequestID,
		ClientAddr: actor.ClientAddr,
		Reason:     reason,
	})
}
