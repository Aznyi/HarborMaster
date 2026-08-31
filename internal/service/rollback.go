package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Manual rollback.
//
// # The whole capability, stated as limits
//
// An operator names an EXECUTION. HarborMaster reads its own record of that
// recreation, re-verifies both container identities against the live host,
// stops the replacement, parks it, renames the preserved original back, starts
// it, and proves it. Then it stops.
//
// It never creates a container, never removes one, never acts on more than one
// execution, never runs on a timer, and never decides for itself that a
// rollback is warranted.
//
// # Nothing a caller supplies reaches the host
//
// The request carries an execution id and an optional idempotency key. Every
// container id, every name, and the image identity come from the execution
// record HarborMaster wrote itself. There is no field on any type in this file
// that could carry a container id from a caller, which is what makes "rollback
// to an attacker-selected target" structurally impossible rather than merely
// checked for.
//
// # Two preflights, for the same reason a recreation has two
//
// The first runs synchronously inside Request, so an operator gets a real
// refusal rather than a queued job that fails a moment later. The second runs
// inside the worker, immediately before the first mutation, because minutes may
// have passed and containers may have moved. Only the second one's verdict
// decides whether anything is touched.

// Rollback service errors.
var (
	// ErrRollbackDisabled reports that the capability is off.
	ErrRollbackDisabled = errors.New("manual rollback is not enabled")
	// ErrRollbackRefused reports that a preflight check said no. The specific
	// check is carried on the record and on RollbackRefusedError.
	ErrRollbackRefused = errors.New("rollback refused by preflight")
	// ErrRollbackNotCancellable reports a cancel arriving after the mutation
	// point.
	ErrRollbackNotCancellable = errors.New("this rollback has already begun changing the host")
)

// RollbackRefusedError carries which check refused.
type RollbackRefusedError struct {
	Refusal domain.RollbackRefusal
}

func (e RollbackRefusedError) Error() string { return e.Refusal.Explain() }

// Unwrap lets callers match on ErrRollbackRefused.
func (e RollbackRefusedError) Unwrap() error { return ErrRollbackRefused }

// RollbackStore is the persistence the service needs.
//
// A narrow interface rather than *store.RollbackRepository, so the service is
// testable and the surface it can reach is visible in one place. Note what is
// ABSENT: nothing that edits a finished record, and nothing that deletes one
// outside retention.
type RollbackStore interface {
	Create(ctx context.Context, rollback domain.Rollback, now time.Time) (domain.Rollback, error)
	Advance(ctx context.Context, change store.RollbackChange, now time.Time) (bool, error)
	Checkpoint(ctx context.Context, write store.RollbackCheckpointWrite, now time.Time) (bool, error)
	Get(ctx context.Context, rollbackID string) (domain.Rollback, error)
	List(ctx context.Context, filter store.RollbackFilter) ([]domain.Rollback, int, error)
	Events(ctx context.Context, rollbackID string, limit int) ([]domain.RollbackEvent, error)
	// ActiveCount and ActiveForContainer both take a rollback id to EXCLUDE.
	// The preflight runs a second time from inside the pipeline, where the
	// rollback being assessed is itself active; without the exclusion it would
	// refuse every rollback for conflicting with itself.
	ActiveCount(ctx context.Context, excluding string) (int, error)
	ActiveForContainer(ctx context.Context, containerName, excluding string) (bool, error)
	SucceededForExecution(ctx context.Context, executionID string) (bool, error)
	ByRequestKey(ctx context.Context, key string) (domain.Rollback, bool, error)
	Claimable(ctx context.Context, limit int) ([]domain.Rollback, error)
	Interrupted(ctx context.Context, limit int) ([]domain.Rollback, error)
	Summary(ctx context.Context) (domain.RollbackSummary, error)
	ExpireStale(ctx context.Context, now time.Time, batch int) (int64, error)
	Prune(ctx context.Context, cutoff time.Time, batch int) (int64, error)
}

// RollbackEvidence is what the service reads to decide eligibility.
//
// The execution record is the whole of the approval chain: a rollback does not
// consult a plan, an acquisition, a policy, or a registry, because it is not
// proposing a change -- it is undoing one HarborMaster already made and already
// recorded.
type RollbackEvidence interface {
	// Execution returns the recreation being undone.
	Execution(ctx context.Context, executionID string) (domain.Execution, error)
	// ExecutionActiveForContainer reports whether a recreation is in flight for
	// a container, so a rollback cannot race one.
	ExecutionActiveForContainer(ctx context.Context, containerID string) (bool, error)
	// InventoryAge reports how stale HarborMaster's view of the host is.
	InventoryAge(ctx context.Context, now time.Time) (time.Duration, bool, error)
}

// RollbackOptions configures a RollbackService.
type RollbackOptions struct {
	Store    RollbackStore
	Evidence RollbackEvidence

	// Runtime is the READ-ONLY capability, used to confirm the daemon is
	// reachable, to re-verify both container identities, and to inspect the
	// original during verification.
	Runtime docker.Runtime
	// Rollbacker is the mutation capability. Nil disables rollback entirely,
	// which is the correct behaviour for a deployment that has not opted in --
	// the capability is ABSENT rather than merely unused.
	Rollbacker docker.ContainerRollbacker

	// Hasher digests sensitive values for the preservation comparison. Nil
	// makes preservation UNVERIFIABLE, which fails closed rather than passing.
	Hasher *Hasher

	// Audit records what a finished rollback did to the host, attributed to the
	// account that asked for it.
	Audit *AuditRecorder

	// Lineage returns what the container follows to the artefact that is
	// running again. Nil leaves lineage untouched.
	Lineage LineageStore

	// Notify raises operator notifications. Nil sends none, which is the default:
	// notifications are off unless a deployment asks for them, and every service
	// must behave identically without one.
	Notify Notifier

	Config config.Rollback
	Logger *slog.Logger
	Now    func() time.Time
}

// RollbackService owns the rollback lifecycle.
type RollbackService struct {
	store      RollbackStore
	evidence   RollbackEvidence
	runtime    docker.Runtime
	rollbacker docker.ContainerRollbacker
	hasher     *Hasher
	audit      *AuditRecorder
	// lineage returns the tracking record to the restored digest.
	lineage  LineageStore
	notifier Notifier

	cfg    config.Rollback
	logger *slog.Logger
	now    func() time.Time

	// wake carries a one-slot signal that queued work exists.
	wake chan struct{}

	mu sync.Mutex
	// inFlight and containers bound concurrency in THIS process. The database
	// index is authoritative across processes and restarts; these close the
	// window between reading the queue and claiming a row.
	inFlight   int
	containers map[string]struct{}
	// cancels holds the pre-mutation cancel function for each running
	// rollback, so an operator's cancel can reach one that has not yet begun.
	cancels map[string]context.CancelFunc
}

// NewRollbackService builds a RollbackService.
func NewRollbackService(opts RollbackOptions) *RollbackService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = config.DefaultRollbackMaxConcurrent
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = config.DefaultRollbackStartupTimeout
	}
	if cfg.StabilityPeriod <= 0 {
		cfg.StabilityPeriod = config.DefaultRollbackStabilityPeriod
	}
	if cfg.HealthPollInterval <= 0 {
		cfg.HealthPollInterval = config.DefaultRollbackHealthPollInterval
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = config.DefaultRollbackStopTimeout
	}
	if cfg.RequestTTL <= 0 {
		cfg.RequestTTL = config.DefaultRollbackRequestTTL
	}
	if cfg.InventoryFreshness <= 0 {
		cfg.InventoryFreshness = config.DefaultRollbackInventoryFreshness
	}
	if cfg.MaxEventsPerRollback < 1 {
		cfg.MaxEventsPerRollback = config.DefaultRollbackMaxEvents
	}

	return &RollbackService{
		store:      opts.Store,
		evidence:   opts.Evidence,
		runtime:    opts.Runtime,
		rollbacker: opts.Rollbacker,
		hasher:     opts.Hasher,
		audit:      opts.Audit,
		lineage:    opts.Lineage,
		notifier:   opts.Notify,
		cfg:        cfg,
		logger:     logger,
		now:        now,
		wake:       make(chan struct{}, 1),
		containers: make(map[string]struct{}),
		cancels:    make(map[string]context.CancelFunc),
	}
}

// Enabled reports whether rollback is configured AND the capability is held.
//
// Both. A deployment that set the flag but was built without the interface has
// not got the capability, and reporting otherwise would offer an operator a
// button that cannot work.
func (s *RollbackService) Enabled() bool {
	return s.cfg.Enabled && s.rollbacker != nil && s.store != nil
}

// ------------------------------------------------------------- requesting --

// RollbackRequest is one operator asking for one rollback.
//
// TWO FIELDS, and neither names a container. Everything about what will happen
// is derived from the execution record, which HarborMaster wrote itself.
type RollbackRequest struct {
	// ExecutionID names the recreation to undo.
	ExecutionID string
	// RequestKey is an optional idempotency key, so a retried request -- or a
	// double-clicked button -- does not start a second rollback.
	RequestKey string

	// RequestedBy is the account asking. Stored on the record so the OUTCOME
	// can be attributed minutes later by a worker with no request and no
	// session.
	RequestedBy domain.Requester
}

// Request validates and records a rollback request.
//
// Runs the FULL preflight synchronously, so an operator gets a real answer
// rather than a queued job that fails a moment later. The rollback itself is
// asynchronous, and the preflight runs AGAIN immediately before the first
// mutation.
func (s *RollbackService) Request(
	ctx context.Context,
	request RollbackRequest,
) (domain.Rollback, error) {
	if !s.Enabled() {
		return domain.Rollback{}, RollbackRefusedError{Refusal: domain.RollbackRefusalDisabled}
	}
	if !domain.ValidExecutionID(request.ExecutionID) {
		return domain.Rollback{}, RollbackRefusedError{
			Refusal: domain.RollbackRefusalExecutionMissing,
		}
	}

	// An idempotency key is answered BEFORE the preflight.
	//
	// A retried request -- or a double-clicked button -- must get back the
	// rollback it already made. Running the preflight first would refuse it for
	// conflicting with the very rollback the key names, which would turn a
	// harmless retry into an error an operator has to interpret.
	if request.RequestKey != "" {
		existing, found, err := s.store.ByRequestKey(ctx, request.RequestKey)
		if err != nil {
			return domain.Rollback{}, fmt.Errorf("read rollback by request key: %w", err)
		}
		if found {
			return existing, nil
		}
	}

	now := s.now().UTC()

	decision, refusal, err := s.assess(ctx, request.ExecutionID, "", now)
	if err != nil {
		return domain.Rollback{}, err
	}
	if refusal != domain.RollbackRefusalNone {
		return domain.Rollback{}, RollbackRefusedError{Refusal: refusal}
	}

	rollback := domain.Rollback{
		RollbackID:       domain.NewRollbackID(),
		ExecutionID:      decision.ExecutionID,
		ContainerName:    decision.ContainerName,
		OriginalID:       decision.OriginalID,
		ParkedName:       decision.ParkedName,
		ReplacementID:    decision.ReplacementID,
		OriginalImage:    decision.OriginalImage,
		OriginalImageID:  decision.OriginalImageID,
		ReplacementImage: decision.ReplacementImage,
		State:            domain.RollbackQueued,
		RequestedAt:      now,
		ExpiresAt:        now.Add(s.cfg.RequestTTL),
		RequestKey:       request.RequestKey,
		RequestedBy:      request.RequestedBy,
	}

	created, err := s.store.Create(ctx, rollback, now)
	if err != nil {
		// The unique indexes refused. Both are races the checks above cannot
		// close, and both are refusals rather than faults.
		switch {
		case errors.Is(err, store.ErrRollbackActive):
			return domain.Rollback{}, RollbackRefusedError{Refusal: domain.RollbackRefusalConflict}
		case errors.Is(err, store.ErrRollbackAlreadySucceeded):
			return domain.Rollback{}, RollbackRefusedError{
				Refusal: domain.RollbackRefusalAlreadyRolledBack,
			}
		}
		return domain.Rollback{}, fmt.Errorf("record rollback: %w", err)
	}

	s.logger.InfoContext(ctx, "rollback requested",
		slog.String("rollbackId", created.RollbackID),
		slog.String("executionId", created.ExecutionID),
		slog.String("containerName", created.ContainerName))

	// Raised at REQUEST rather than at completion, unlike every other
	// notification in the pipeline. A rollback takes as long as a container
	// takes to stop and start, and an operator who is about to be paged wants to
	// know it has begun rather than reading about it afterwards.
	//
	// MANUAL rollbacks only. An automatic one is the middle of a sequence whose
	// ends already speak -- "could not be updated", then "was restored
	// automatically" -- and a third message between them says nothing and
	// arrives looking like a third problem.
	if !created.Automatic() {
		NotifyRollbackStarted(s.notifier, created.ContainerName, created.RollbackID)
	}

	s.signal()
	return created, nil
}

// Cancel stops a rollback that has not yet changed anything.
//
// Refused once the replacement has been stopped. A rollback that has begun
// moving containers must reach a recorded conclusion: abandoning it partway
// would leave the container an operator depends on neither running nor recorded
// as down.
func (s *RollbackService) Cancel(ctx context.Context, rollbackID string) (domain.Rollback, error) {
	if !domain.ValidRollbackID(rollbackID) {
		return domain.Rollback{}, store.ErrNotFound
	}

	existing, err := s.store.Get(ctx, rollbackID)
	if err != nil {
		return domain.Rollback{}, err
	}
	if existing.State.Terminal() {
		return existing, nil
	}
	if !existing.State.Cancellable() {
		return domain.Rollback{}, ErrRollbackNotCancellable
	}

	moved, err := s.store.Advance(ctx, store.RollbackChange{
		RollbackID: rollbackID,
		From:       []domain.RollbackState{domain.RollbackQueued, domain.RollbackValidating},
		To:         domain.RollbackCancelled,
		Message:    "cancelled by an operator before anything on this host was changed",
		Detail:     "cancelled before the mutation point",
	}, s.now().UTC())
	if err != nil {
		return domain.Rollback{}, err
	}
	if !moved {
		// It moved past the cancellable window between the read and the write.
		// Re-read so the caller is told what is actually true.
		current, readErr := s.store.Get(ctx, rollbackID)
		if readErr != nil {
			return domain.Rollback{}, readErr
		}
		if current.State.Terminal() {
			return current, nil
		}
		return domain.Rollback{}, ErrRollbackNotCancellable
	}

	// Interrupt the worker if one has already picked it up. Best effort: the
	// database transition above is what makes the cancellation authoritative,
	// and the pipeline re-reads state at every step boundary.
	s.mu.Lock()
	if cancel, running := s.cancels[rollbackID]; running {
		cancel()
	}
	s.mu.Unlock()

	return s.store.Get(ctx, rollbackID)
}

// ---------------------------------------------------------------- reading --

// Get returns one rollback with its bounded audit trail.
func (s *RollbackService) Get(
	ctx context.Context,
	rollbackID string,
) (domain.Rollback, []domain.RollbackEvent, error) {
	if !domain.ValidRollbackID(rollbackID) {
		return domain.Rollback{}, nil, store.ErrNotFound
	}

	rollback, err := s.store.Get(ctx, rollbackID)
	if err != nil {
		return domain.Rollback{}, nil, err
	}
	events, err := s.store.Events(ctx, rollbackID, s.cfg.MaxEventsPerRollback)
	if err != nil {
		return domain.Rollback{}, nil, err
	}
	return rollback, events, nil
}

// List returns a page of rollbacks.
func (s *RollbackService) List(
	ctx context.Context,
	filter store.RollbackFilter,
) ([]domain.Rollback, int, error) {
	return s.store.List(ctx, filter)
}

// Summary returns the estate aggregate.
func (s *RollbackService) Summary(ctx context.Context) (domain.RollbackSummary, error) {
	summary, err := s.store.Summary(ctx)
	if err != nil {
		return summary, err
	}
	// Set here rather than in the repository: whether the capability is held
	// is a property of THIS PROCESS, not of the rows. A database restored onto
	// a deployment that has not opted in must read as switched off.
	summary.Enabled = s.Enabled()
	return summary, nil
}

// Eligible answers whether one execution LOOKS like one that can be rolled
// back.
//
// # It reads records only, and that is deliberate
//
// Advice for a UI, not permission. It runs the half of the preflight that reads
// HarborMaster's own records and stops there: it issues no Docker call, so it
// cannot say whether the two containers are still where the record puts them.
//
// The reason is that this answer is served from `GET /executions/{id}`, a page
// the UI polls. Letting a read endpoint turn one HTTP request into a ping, two
// inspections, and a container listing would make it a denial-of-service
// amplifier pointed at a privileged socket. See assessRecords.
//
// Nothing is lost. The request path runs the FULL assessment before it accepts
// anything, and the pipeline runs it again against the live host immediately
// before the first mutation -- so a caller that skipped this and posted anyway
// gets a complete answer, just later.
func (s *RollbackService) Eligible(
	ctx context.Context,
	executionID string,
) (domain.RollbackEligibility, error) {
	if !domain.ValidExecutionID(executionID) {
		return refusedEligibility(domain.RollbackRefusalExecutionMissing), nil
	}
	if !s.Enabled() {
		return refusedEligibility(domain.RollbackRefusalDisabled), nil
	}

	decision, refusal, err := s.assessRecords(ctx, executionID, "")
	if err != nil {
		return domain.RollbackEligibility{}, err
	}
	if refusal != domain.RollbackRefusalNone {
		eligibility := refusedEligibility(refusal)
		// The identities are still filled in when they are known, so a refusal
		// page can show WHICH containers it is talking about.
		eligibility.ContainerName = decision.ContainerName
		eligibility.OriginalID = decision.OriginalID
		eligibility.ParkedName = decision.ParkedName
		eligibility.ReplacementID = decision.ReplacementID
		return eligibility, nil
	}

	return domain.RollbackEligibility{
		Eligible:       true,
		ContainerName:  decision.ContainerName,
		OriginalID:     decision.OriginalID,
		ParkedName:     decision.ParkedName,
		ReplacementID:  decision.ReplacementID,
		OriginalRef:    decision.OriginalImage,
		ReplacementRef: decision.ReplacementImage,
	}, nil
}

// refusedEligibility renders a refusal.
func refusedEligibility(refusal domain.RollbackRefusal) domain.RollbackEligibility {
	return domain.RollbackEligibility{
		Eligible: false,
		Refusal:  refusal,
		Reason:   refusal.Explain(),
	}
}

// signal wakes the worker without blocking.
func (s *RollbackService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// digester returns the keyed digester for preservation comparison.
//
// Nil when no hasher was supplied, which makes every comparison UNVERIFIABLE
// rather than passing. A rollback with no hasher therefore cannot succeed, and
// that is the correct fail-closed behaviour: without one, the projection could
// not distinguish two different secrets.
func (s *RollbackService) digester() domain.SecretDigester {
	if s.hasher == nil {
		return nil
	}
	return func(value string) string { return s.hasher.Digest(value).Digest }
}

// digestKeyID identifies the key the projections were digested under.
func (s *RollbackService) digestKeyID() string {
	if s.hasher == nil {
		return ""
	}
	return s.hasher.KeyID()
}

// ---------------------------------------------------------------- evidence --

// rollbackEvidence adapts the repositories to RollbackEvidence.
//
// Two repositories, and that is the whole of it. A rollback does not consult a
// plan, an acquisition, a policy, or a registry: it is not proposing a change,
// it is undoing one HarborMaster already made and already recorded. Taking
// those dependencies would make the service's dependency list a description of
// the storage layout rather than of what it needs.
type rollbackEvidence struct {
	executions *store.ExecutionRepository
	inventory  *store.InventoryRepository
}

// NewRollbackEvidence builds the evidence adapter the rollback service needs.
func NewRollbackEvidence(
	executions *store.ExecutionRepository,
	inventory *store.InventoryRepository,
) RollbackEvidence {
	return &rollbackEvidence{executions: executions, inventory: inventory}
}

func (e *rollbackEvidence) Execution(
	ctx context.Context,
	executionID string,
) (domain.Execution, error) {
	return e.executions.Get(ctx, executionID)
}

func (e *rollbackEvidence) ExecutionActiveForContainer(
	ctx context.Context,
	containerID string,
) (bool, error) {
	return e.executions.ActiveForContainer(ctx, containerID)
}

// InventoryAge reports how stale HarborMaster's view of the host is.
//
// The second return value distinguishes "the inventory is old" from "there has
// never been a successful refresh". Both refuse a rollback, and a caller that
// could not tell them apart would report the wrong one to an operator.
func (e *rollbackEvidence) InventoryAge(
	ctx context.Context,
	now time.Time,
) (time.Duration, bool, error) {
	record, err := e.inventory.LastRefresh(ctx, true)
	if err != nil {
		return 0, false, fmt.Errorf("read last inventory refresh: %w", err)
	}
	if record == nil {
		return 0, false, nil
	}
	return now.UTC().Sub(record.StartedAt.UTC()), true, nil
}
