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

// Manual container recreation.
//
// # The only thing in HarborMaster that changes something running
//
// This service holds the whole of HarborMaster's ability to stop, create,
// start, rename, and remove a container. Nothing else receives
// docker.ContainerMutator, and an architecture test fails the build if any
// other package so much as names it.
//
// # The safety model, in order
//
//	 1. An operator asks, naming a SUCCEEDED ACQUISITION. There is no free-form
//	    target, no image parameter, and no container parameter.
//	 2. Every prerequisite is re-read and re-checked immediately before
//	    execution -- acquisition, plan, container, inventory, snapshot,
//	    readiness, policy, registry intelligence, and the local image.
//	 3. The container's live configuration is captured, opaquely.
//	 4. The original is stopped and PARKED under a derived name. It is not
//	    removed.
//	 5. A replacement is created under the original's name, from the approved
//	    digest, with the captured configuration.
//	 6. It is started and then PROVED: health or stability, image digest,
//	    configuration preservation, network attachment. All four.
//	 7. The success is recorded durably.
//	 8. Only then is the parked original removed.
//
// Steps 4 and 8 are the design. The original survives every intermediate
// failure, which is what makes an unsuccessful recreation recoverable by hand
// rather than an outage with no way back.
//
// # There is no rollback, deliberately
//
// A failure after step 4 stops, quarantines the replacement, leaves both
// containers on the host, and records a manual recovery plan. HarborMaster does
// not undo its own work: an automatic undo is another unattended mutation,
// performed at exactly the moment HarborMaster has demonstrated that its model
// of the host is wrong.
//
// # The checkpoint, not the state, is what survives a crash
//
// Every mutation is followed by a durable checkpoint before the next is
// attempted. If a checkpoint cannot be written, the pipeline STOPS: repeating a
// stop, a rename, or a remove against a host whose recorded state is uncertain
// is how a recoverable situation becomes an unrecoverable one.

// ErrExecutionDisabled reports that container recreation is switched off.
var ErrExecutionDisabled = errors.New("container recreation is disabled")

// ErrExecutionRefused reports that the preflight refused a request.
//
// Carries the specific refusal, so the API can report which check said no
// rather than a generic failure.
type ErrExecutionRefused struct {
	Refusal domain.ExecutionRefusal
}

func (e ErrExecutionRefused) Error() string { return e.Refusal.Explain() }

// ExecutionStore is the persistence capability the service needs.
//
// A narrow interface rather than *store.ExecutionRepository, so the service is
// testable without a database and so the surface it depends on is visible in
// one place.
type ExecutionStore interface {
	Create(ctx context.Context, execution domain.Execution, now time.Time) (domain.Execution, error)
	Get(ctx context.Context, executionID string) (domain.Execution, error)
	List(ctx context.Context, filter store.ExecutionFilter) ([]domain.Execution, int, error)
	Events(ctx context.Context, executionID string, limit int) ([]domain.ExecutionEvent, error)
	Summary(ctx context.Context) (domain.ExecutionSummary, error)
	Advance(ctx context.Context, change store.ExecutionChange, now time.Time) (bool, error)
	Checkpoint(ctx context.Context, write store.ExecutionCheckpointWrite, now time.Time) error
	ActiveCount(ctx context.Context) (int, error)
	ActiveForContainer(ctx context.Context, containerID string) (bool, error)
	ByAcquisition(ctx context.Context, acquisitionID string) (domain.Execution, bool, error)
	ByRequestKey(ctx context.Context, key string) (domain.Execution, bool, error)
	Claimable(ctx context.Context, limit int) ([]domain.Execution, error)
	Interrupted(ctx context.Context, limit int) ([]domain.Execution, error)
	ExpireStale(ctx context.Context, now time.Time, batch int) (int64, error)
	Prune(ctx context.Context, cutoff time.Time, batch int) (int64, error)
}

// ExecutionEvidence is the read-only data the preflight re-checks.
//
// Every method is a READ, and every one reads HARBORMASTER'S OWN DATABASE
// rather than Docker. The preflight must not turn one operator action into a
// sweep against a privileged socket, and the freshness gates are what make
// reading a cached view safe: a view too old to act on is a refusal.
type ExecutionEvidence interface {
	// Acquisition returns the download this recreation is founded on.
	Acquisition(ctx context.Context, acquisitionID string) (domain.Acquisition, error)
	// Plan returns the change plan by its immutable id, and CurrentPlan the
	// newest plan for a container -- which is how a superseded approval is
	// detected.
	Plan(ctx context.Context, planID string) (domain.ChangePlan, error)
	CurrentPlan(ctx context.Context, containerID string) (domain.ChangePlan, error)
	// Container returns the inventory's view of the container.
	Container(ctx context.Context, containerID string) (*domain.ContainerDetail, error)
	// Baseline returns the container's most recent snapshot.
	Baseline(ctx context.Context, containerID string) (domain.Snapshot, error)
	// PolicyEvaluation returns the container's most recent compliance pass.
	PolicyEvaluation(ctx context.Context, containerID string) (domain.PolicyEvaluation, error)
	// LastRefresh returns the most recent SUCCESSFUL inventory refresh.
	LastRefresh(ctx context.Context) (*domain.RefreshRecord, error)
	// Intel returns what the registry most recently reported for a reference.
	Intel(ctx context.Context, reference string) (domain.ImageIntel, error)
}

// ExecutionOptions configures an ExecutionService.
type ExecutionOptions struct {
	Store    ExecutionStore
	Evidence ExecutionEvidence

	// Runtime is the READ-ONLY capability, used to confirm the daemon is
	// reachable, to verify the local image, to check that a derived name is
	// free, and to inspect the replacement during verification.
	Runtime docker.Runtime
	// Capturer reads the container's live configuration. Nil disables
	// recreation: there would be nothing to reproduce.
	Capturer docker.ConfigCapturer
	// Mutator is the container mutation capability. Nil disables recreation
	// entirely, which is the correct behaviour for a deployment that has not
	// opted in.
	Mutator docker.ContainerMutator

	// Hasher digests sensitive values for the preservation comparison. Nil
	// makes preservation UNVERIFIABLE, which fails closed rather than passing.
	Hasher *Hasher

	// Self reports which container HarborMaster is running in. The LAST line of
	// the self-update defence, and the one that matters most: this is the layer
	// that actually stops a container.
	Self SelfReporter

	// Notify raises operator notifications. Nil sends none, which is the default:
	// notifications are off unless a deployment asks for them, and every service
	// must behave identically without one.
	Notify Notifier

	// Audit records what a finished recreation did to the host, attributed to
	// the account that asked for it. Nil records nothing, which is correct for
	// a test but not for a deployment.
	Audit *AuditRecorder

	Config config.Execution
	Logger *slog.Logger
	Now    func() time.Time
}

// ExecutionService owns the recreation lifecycle.
type ExecutionService struct {
	store    ExecutionStore
	evidence ExecutionEvidence
	runtime  docker.Runtime
	capturer docker.ConfigCapturer
	mutator  docker.ContainerMutator
	hasher   *Hasher
	// self reports HarborMaster's own container. Read on every preflight.
	notifier Notifier
	self     SelfReporter
	// audit records the OUTCOME of a recreation in the security log. Nil in a
	// test that is not about attribution; reportOutcome is nil-safe.
	audit *AuditRecorder

	cfg    config.Execution
	logger *slog.Logger
	now    func() time.Time

	// wake carries a one-slot signal that queued work exists.
	wake chan struct{}

	// mu guards the in-flight bookkeeping below.
	mu sync.Mutex
	// cancels reaches the context of an execution that has not yet mutated
	// anything. An execution past the mutation point is REMOVED from this map,
	// which is what makes "cancellation is impossible after the mutation point"
	// structural rather than a check someone has to remember.
	cancels map[string]context.CancelFunc
	// containers tracks which containers are being recreated in this process.
	// The database index is authoritative across restarts; this is what makes
	// the limit hold between the check and the claim.
	containers map[string]struct{}
	inFlight   int
}

// NewExecutionService builds an ExecutionService.
func NewExecutionService(opts ExecutionOptions) *ExecutionService {
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
		cfg.MaxConcurrent = config.DefaultExecutionMaxConcurrent
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = config.DefaultExecutionStartupTimeout
	}
	if cfg.StabilityPeriod <= 0 {
		cfg.StabilityPeriod = config.DefaultExecutionStabilityPeriod
	}
	if cfg.HealthPollInterval <= 0 {
		cfg.HealthPollInterval = config.DefaultExecutionHealthPollInterval
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = config.DefaultExecutionStopTimeout
	}
	if cfg.RequestTTL <= 0 {
		cfg.RequestTTL = config.DefaultExecutionRequestTTL
	}
	if cfg.AcquisitionFreshness <= 0 {
		cfg.AcquisitionFreshness = config.DefaultExecutionAcquisitionFreshness
	}
	if cfg.InventoryFreshness <= 0 {
		cfg.InventoryFreshness = config.DefaultExecutionInventoryFreshness
	}
	if cfg.PolicyFreshness <= 0 {
		cfg.PolicyFreshness = config.DefaultExecutionPolicyFreshness
	}
	if cfg.MaxEventsPerExecution < 1 {
		cfg.MaxEventsPerExecution = config.DefaultExecutionMaxEvents
	}

	return &ExecutionService{
		store:      opts.Store,
		evidence:   opts.Evidence,
		runtime:    opts.Runtime,
		capturer:   opts.Capturer,
		mutator:    opts.Mutator,
		hasher:     opts.Hasher,
		notifier:   opts.Notify,
		self:       opts.Self,
		audit:      opts.Audit,
		cfg:        cfg,
		logger:     logger,
		now:        now,
		wake:       make(chan struct{}, 1),
		cancels:    make(map[string]context.CancelFunc),
		containers: make(map[string]struct{}),
	}
}

// Enabled reports whether recreation is available in this deployment.
//
// Requires the configuration flag AND every capability. A deployment that
// enabled the feature but wired no mutator must report disabled rather than
// accept requests it can never run.
func (s *ExecutionService) Enabled() bool {
	return s.cfg.Enabled &&
		s.mutator != nil && s.capturer != nil &&
		s.store != nil && s.runtime != nil && s.evidence != nil
}

// digester returns the keyed digest function for preservation comparison.
//
// Nil when no hasher is wired, which makes the comparison UNVERIFIABLE rather
// than trivially equal -- see domain.ComparePreservation. Fails closed.
func (s *ExecutionService) digester() domain.SecretDigester {
	if s.hasher == nil {
		return nil
	}
	return func(value string) string { return s.hasher.Digest(value).Digest }
}

// digestKeyID reports which key the projections are built under.
func (s *ExecutionService) digestKeyID() string {
	if s.hasher == nil {
		return ""
	}
	return s.hasher.KeyID()
}

// ------------------------------------------------------------- requesting --

// ExecutionRequest is an operator's ask.
//
// ONE meaningful field. It does not carry a container, an image, a digest, a
// command, a mount, or any other Docker parameter -- every one of those is
// derived from the acquisition, which derived it from a plan. There is no field
// here an attacker could aim.
type ExecutionRequest struct {
	// AcquisitionID names the succeeded download. Single use: an acquisition
	// that has already been executed is refused, with no override.
	AcquisitionID string
	// RequestKey is an optional idempotency key, so a retried request -- or a
	// double-clicked button -- does not start a second recreation.
	RequestKey string

	// RequestedBy is the account asking. Stored on the record so the OUTCOME
	// can be attributed minutes later by a worker that has no request and no
	// session; see reportOutcome.
	RequestedBy domain.Requester
}

// Request validates and records a recreation request.
//
// Runs the FULL preflight synchronously, so an operator gets a real answer
// rather than a queued job that fails a moment later. The recreation itself is
// asynchronous, and the preflight runs AGAIN immediately before the first
// mutation.
func (s *ExecutionService) Request(
	ctx context.Context,
	request ExecutionRequest,
) (domain.Execution, error) {
	if !s.Enabled() {
		return domain.Execution{}, ErrExecutionRefused{Refusal: domain.ExecutionRefusalDisabled}
	}

	// Idempotency is answered FIRST, before any check that could refuse.
	//
	// A retried request must return the record it already created, even when
	// the situation has since changed: the caller is asking "what happened to
	// my request", not "may I make a new one".
	if request.RequestKey != "" {
		existing, found, err := s.store.ByRequestKey(ctx, request.RequestKey)
		if err != nil {
			return domain.Execution{}, err
		}
		if found {
			return existing, nil
		}
	}

	decision, err := s.preflight(ctx, request.AcquisitionID)
	if err != nil {
		return domain.Execution{}, err
	}
	if decision.Refusal != domain.ExecutionRefusalNone {
		return domain.Execution{}, ErrExecutionRefused{Refusal: decision.Refusal}
	}

	// Admission control. Deliberately NOT part of the preflight: the preflight
	// runs again inside the worker, at which point this execution already holds
	// a slot and would refuse itself.
	if refusal, err := s.admit(ctx, decision); err != nil {
		return domain.Execution{}, err
	} else if refusal != domain.ExecutionRefusalNone {
		return domain.Execution{}, ErrExecutionRefused{Refusal: refusal}
	}

	now := s.now().UTC()
	execution := domain.Execution{
		ExecutionID:    domain.NewExecutionID(),
		AcquisitionID:  decision.Acquisition.AcquisitionID,
		PlanID:         decision.Plan.PlanID,
		SnapshotID:     decision.SnapshotID,
		ContainerID:    decision.ContainerID,
		ContainerName:  decision.ContainerName,
		OldImage:       decision.OldImage,
		OldImageID:     decision.OldImageID,
		OldImageDigest: decision.OldImageDigest,
		Target:         decision.Target,
		State:          domain.ExecutionQueued,
		RequestedAt:    now,
		ExpiresAt:      now.Add(s.cfg.RequestTTL),
		RequestKey:     request.RequestKey,
		PlanDigest:     decision.Plan.InputDigest,
		RequestedBy:    request.RequestedBy,
	}

	created, err := s.store.Create(ctx, execution, now)
	if err != nil {
		// The unique indexes refused. Both are races the checks above cannot
		// close, and both are refusals rather than faults.
		switch {
		case errors.Is(err, store.ErrExecutionActive):
			return domain.Execution{}, ErrExecutionRefused{Refusal: domain.ExecutionRefusalConflict}
		case errors.Is(err, store.ErrAcquisitionConsumed):
			return domain.Execution{}, ErrExecutionRefused{Refusal: domain.ExecutionRefusalAcquisitionConsumed}
		}
		return domain.Execution{}, err
	}

	// Logged at WARN rather than INFO, alone among HarborMaster's request
	// paths. An operator has just asked for a running container to be stopped
	// and replaced, and that belongs at a level a default log configuration
	// shows.
	s.logger.WarnContext(ctx, "container recreation requested",
		slog.String("executionId", created.ExecutionID),
		slog.String("acquisitionId", created.AcquisitionID),
		slog.String("containerId", domain.ShortenID(created.ContainerID)),
		slog.String("containerName", created.ContainerName),
		slog.String("fromImage", created.OldImage),
		slog.String("toDigest", created.Target.Digest))

	s.signal()
	return created, nil
}

// Cancel stops a recreation an operator no longer wants.
//
// # Only before the mutation point
//
// A recreation that has stopped the original and created a replacement cannot
// be abandoned: doing so would leave the host in a state nobody recorded and
// nobody chose. Past that point the only safe direction is forwards, through
// verification, to a recorded conclusion -- successful or failed, but recorded.
//
// The in-process cancel function is REMOVED from the map at the mutation point,
// so this cannot reach a pipeline that has begun changing things even if the
// state check below were somehow wrong.
func (s *ExecutionService) Cancel(ctx context.Context, executionID string) (domain.Execution, error) {
	if s.store == nil {
		return domain.Execution{}, ErrExecutionDisabled
	}

	existing, err := s.store.Get(ctx, executionID)
	if err != nil {
		return domain.Execution{}, err
	}
	if !existing.State.Cancellable() {
		return existing, ErrExecutionRefused{Refusal: domain.ExecutionRefusalNone}
	}

	s.mu.Lock()
	cancel, running := s.cancels[executionID]
	s.mu.Unlock()
	if running {
		cancel()
	}

	moved, err := s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID: executionID,
		// Explicitly the three pre-mutation states. Passing an empty From would
		// mean "any active state", which here would include the mutating ones.
		From: []domain.ExecutionState{
			domain.ExecutionQueued, domain.ExecutionValidating, domain.ExecutionCapturing,
		},
		To:      domain.ExecutionCancelled,
		Message: "cancelled by an operator before anything on this host was changed",
		Detail:  "cancelled by an operator",
	}, s.now().UTC())
	if err != nil {
		return domain.Execution{}, err
	}
	if !moved {
		// It moved past the cancellable states between the read and the write.
		// Not an error and not a success: the caller is told what the record
		// actually says.
		return s.store.Get(ctx, executionID)
	}

	s.logger.InfoContext(ctx, "container recreation cancelled",
		slog.String("executionId", executionID))

	return s.store.Get(ctx, executionID)
}

// admit decides whether a NEW request fits within the configured limits.
//
// Separate from the preflight, and the separation matters: the preflight runs a
// second time inside the worker, by which point this execution is itself
// active. A limit check there would count the execution against its own budget
// and refuse every single request.
// The SINGLE-USE check lives here for the same reason. It is checked first,
// because it is the most specific and most useful answer: "that download has
// already been applied" tells an operator to make a new plan, while "another
// recreation is running" would leave them retrying something that will never
// be different.
func (s *ExecutionService) admit(
	ctx context.Context,
	decision executionDecision,
) (domain.ExecutionRefusal, error) {
	if _, consumed, err := s.store.ByAcquisition(ctx, decision.Acquisition.AcquisitionID); err != nil {
		return domain.ExecutionRefusalNone, err
	} else if consumed {
		return domain.ExecutionRefusalAcquisitionConsumed, nil
	}

	active, err := s.store.ActiveForContainer(ctx, decision.ContainerID)
	if err != nil {
		return domain.ExecutionRefusalNone, err
	}
	if active {
		return domain.ExecutionRefusalConflict, nil
	}

	total, err := s.store.ActiveCount(ctx)
	if err != nil {
		return domain.ExecutionRefusalNone, err
	}
	if total >= s.cfg.MaxConcurrent {
		return domain.ExecutionRefusalLimit, nil
	}

	return domain.ExecutionRefusalNone, nil
}

// signal asks the worker to look for queued work.
func (s *ExecutionService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// --------------------------------------------------------------- reading --

// Get returns one execution with its audit trail.
func (s *ExecutionService) Get(
	ctx context.Context,
	executionID string,
) (domain.Execution, []domain.ExecutionEvent, error) {
	execution, err := s.store.Get(ctx, executionID)
	if err != nil {
		return domain.Execution{}, nil, err
	}
	events, err := s.store.Events(ctx, executionID, s.cfg.MaxEventsPerExecution)
	if err != nil {
		return execution, nil, err
	}
	return execution, events, nil
}

// List returns a page of executions.
func (s *ExecutionService) List(
	ctx context.Context,
	filter store.ExecutionFilter,
) ([]domain.Execution, int, error) {
	return s.store.List(ctx, filter)
}

// Summary returns the dashboard aggregate, with whether the feature is on.
func (s *ExecutionService) Summary(ctx context.Context) (domain.ExecutionSummary, error) {
	summary, err := s.store.Summary(ctx)
	if err != nil {
		return summary, err
	}
	summary.Enabled = s.Enabled()
	return summary, nil
}

// Eligible reports whether an acquisition could be executed right now, without
// recording anything.
//
// Used by the UI to decide whether to offer the action. Runs the SAME preflight
// as the request path, so the button and the outcome cannot disagree -- and
// because it records nothing, being wrong costs an operator a refusal rather
// than a failed job.
func (s *ExecutionService) Eligible(
	ctx context.Context,
	acquisitionID string,
) (domain.ExecutionTarget, domain.ExecutionRefusal, error) {
	if !s.Enabled() {
		return domain.ExecutionTarget{}, domain.ExecutionRefusalDisabled, nil
	}

	decision, err := s.preflight(ctx, acquisitionID)
	if err != nil {
		return domain.ExecutionTarget{}, domain.ExecutionRefusalNone, err
	}
	return decision.Target, decision.Refusal, nil
}

// ------------------------------------------------------------- evidence --

// executionEvidence adapts the repositories to ExecutionEvidence.
//
// A small adapter rather than passing seven repositories into the service, so
// the service's dependency is one narrow READ-ONLY interface and a test can
// substitute it wholesale.
type executionEvidence struct {
	acquisitions *store.AcquisitionRepository
	plans        *store.PlanRepository
	containers   *store.ContainerRepository
	snapshots    *store.SnapshotRepository
	policies     *store.PolicyRepository
	inventory    *store.InventoryRepository
	intel        *store.ImageIntelRepository
}

// NewExecutionEvidence builds the evidence source from the repositories.
func NewExecutionEvidence(
	acquisitions *store.AcquisitionRepository,
	plans *store.PlanRepository,
	containers *store.ContainerRepository,
	snapshots *store.SnapshotRepository,
	policies *store.PolicyRepository,
	inventory *store.InventoryRepository,
	intel *store.ImageIntelRepository,
) ExecutionEvidence {
	return &executionEvidence{
		acquisitions: acquisitions,
		plans:        plans,
		containers:   containers,
		snapshots:    snapshots,
		policies:     policies,
		inventory:    inventory,
		intel:        intel,
	}
}

func (e *executionEvidence) Acquisition(ctx context.Context, id string) (domain.Acquisition, error) {
	return e.acquisitions.Get(ctx, id)
}

func (e *executionEvidence) Plan(ctx context.Context, planID string) (domain.ChangePlan, error) {
	return e.plans.Get(ctx, planID)
}

func (e *executionEvidence) CurrentPlan(ctx context.Context, containerID string) (domain.ChangePlan, error) {
	return e.plans.Current(ctx, containerID)
}

func (e *executionEvidence) Container(ctx context.Context, containerID string) (*domain.ContainerDetail, error) {
	return e.containers.Get(ctx, containerID)
}

func (e *executionEvidence) Baseline(ctx context.Context, containerID string) (domain.Snapshot, error) {
	return e.snapshots.Baseline(ctx, containerID)
}

func (e *executionEvidence) PolicyEvaluation(ctx context.Context, containerID string) (domain.PolicyEvaluation, error) {
	return e.policies.PolicyEvaluation(ctx, containerID)
}

func (e *executionEvidence) LastRefresh(ctx context.Context) (*domain.RefreshRecord, error) {
	record, err := e.inventory.LastRefresh(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("read last inventory refresh: %w", err)
	}
	return record, nil
}

func (e *executionEvidence) Intel(ctx context.Context, reference string) (domain.ImageIntel, error) {
	return e.intel.Get(ctx, reference)
}
