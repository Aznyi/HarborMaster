package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The automation engine.
//
// # What this is, stated plainly
//
// HarborMaster's premise through Phase 10 was that nothing changes the host
// unless a person asked. Phase 11 breaks that premise on purpose, because "a
// safer replacement for Watchtower" is not a thing you can be without it.
//
// What it does NOT break is everything the premise was protecting.
//
// # This service is a CALLER, not a capability
//
// It holds no Docker interface. Look at AutomationOptions: none of the
// capability interfaces from internal/docker appears there -- not the read-only
// runtime, not the image acquirer, not the container mutator, not the
// rollbacker. (The architecture tests check this by name; this comment cannot
// spell them, because a mention would be indistinguishable from a use.)
//
// The only way this code can reach a socket is by asking a service that already
// had one, through the same request type an HTTP handler builds:
//
//	AcquisitionRequest{PlanID, RequestKey, RequestedBy}
//	ExecutionRequest{AcquisitionID, RequestKey, RequestedBy}
//	RollbackRequest{ExecutionID, RequestKey, RequestedBy}
//
// Each of those re-runs its own full preflight against the LIVE host at the
// moment it acts. Automation cannot skip the planner, the digest verification,
// the preservation comparison, the health proof, or the checkpoint discipline,
// because it does not implement any of them -- it asks the components that do.
// TestAutomationHoldsNoDockerCapability pins this by reflection.
//
// # Two loops, and why
//
// A DECISION PASS asks "what should change" and submits acquisitions. A
// FOLLOWER asks "what happened to what I already asked for" and advances it:
// acquisition succeeded, so request the recreation; recreation failed
// verification, so roll it back.
//
// They are separate because the questions have different deadlines. A pass runs
// on a fifteen-minute schedule and must not have to stay alive for a
// multi-gigabyte transfer. The follower runs every thirty seconds, holds no
// state of its own, and reads everything it needs from the database -- so a
// restart in the middle of an update resumes the follow-through rather than
// abandoning it.
//
// # Nothing here takes a target from a caller
//
// A pass reads containers from the inventory, policies from the database, and
// plans from the planner's own records. The follower reads acquisition and
// execution records HarborMaster wrote. There is no field on any type in this
// file that a caller could put a container id, an image, or a digest into.

// Automation service errors.
var (
	// ErrAutomationDisabled reports that the engine is not switched on.
	ErrAutomationDisabled = errors.New("the automation engine is disabled in this deployment")
	// ErrAutomationBusy reports that a pass is already running.
	ErrAutomationBusy = errors.New("an automation pass is already running")
)

// AutomationStore is the persistence the engine needs.
//
// An interface rather than the repository, so a test can substitute it and so
// the compile-time surface says exactly which writes the engine performs.
type AutomationStore interface {
	StartRun(ctx context.Context, run domain.AutomationRun) (domain.AutomationRun, error)
	FinishRun(ctx context.Context, runID string, state domain.AutomationRunState,
		counts domain.AutomationRun, completedAt time.Time) error
	InterruptRuns(ctx context.Context, at time.Time) (int, error)
	RecordDecisions(ctx context.Context, decisions []domain.AutomationDecision) (int, error)
	AttachExecution(ctx context.Context, runID, acquisitionID, executionID string) error
	PromoteApproved(ctx context.Context, runID, containerName, acquisitionID string) error
	AttachRollback(ctx context.Context, runID, executionID, rollbackID string) error
	LatestRun(ctx context.Context) (domain.AutomationRun, error)
	RunByID(ctx context.Context, runID string) (domain.AutomationRun, error)
	ListRuns(ctx context.Context, filter store.AutomationRunFilter) ([]domain.AutomationRun, int, error)
	ListDecisions(ctx context.Context, filter store.AutomationDecisionFilter) ([]domain.AutomationDecision, int, error)
	// PendingDecisions is the follower's one query: the work automation started
	// that has not reached a terminal state.
	PendingDecisions(ctx context.Context, limit int) ([]domain.AutomationDecision, error)
	// SettleDecision marks a decision terminal, whatever its ending. Without it
	// a successful update stays outstanding forever.
	SettleDecision(ctx context.Context, runID, containerName string, at time.Time) error
	RunSummary(ctx context.Context) (domain.AutomationRunSummary, error)
	CountAwaitingApproval(ctx context.Context) (int, error)
	PruneRuns(ctx context.Context, before time.Time, limit int) (int, error)

	Pause(ctx context.Context, pause domain.PausedContainer) (domain.PausedContainer, error)
	Resume(ctx context.Context, containerName string, by domain.Requester, at time.Time) error
	ActivePauses(ctx context.Context) ([]domain.PausedContainer, error)
	// ContainerPreferences is every per-container behaviour an operator chose,
	// by container NAME. One read for the whole pass: asking per container
	// would be the N+1 pattern on the path that runs on a timer.
	ContainerPreferences(ctx context.Context) (map[string]domain.UpdateBehavior, error)
	PauseFor(ctx context.Context, containerName string) (domain.PausedContainer, error)
	ListPauses(ctx context.Context, activeOnly bool, page store.Page) ([]domain.PausedContainer, int, error)
	CountActivePauses(ctx context.Context) (int, error)

	RecordFailure(ctx context.Context, containerName, detail string, windowHours int,
		at time.Time) (store.AutomationFailureCount, error)
	RecordSuccess(ctx context.Context, containerName string, at time.Time) error
	FailureCount(ctx context.Context, containerName string) (store.AutomationFailureCount, error)
}

// AutomationPolicyStore reads the rules.
type AutomationPolicyStore interface {
	ActivePolicies(ctx context.Context) ([]domain.UpdatePolicy, error)
	CountUpdatePolicies(ctx context.Context) (total, enabled int, err error)
}

// AutomationEvidence is everything a pass reads about the world.
//
// Deliberately READ-ONLY and deliberately narrow. Nothing on this interface can
// change anything, which is what makes "a decision pass cannot mutate" a
// property of the type rather than of the code that uses it.
type AutomationEvidence interface {
	// Targets returns every present container with its labels, and whether the
	// estate exceeded the bound.
	Targets(ctx context.Context) ([]store.AutomationTarget, bool, error)
	// CurrentPlan returns the container's newest change plan.
	CurrentPlan(ctx context.Context, containerID string) (domain.ChangePlan, error)
	// AcquisitionActive and ExecutionActive report outstanding work.
	AcquisitionActive(ctx context.Context, containerID string) (bool, error)
	ExecutionActive(ctx context.Context, containerID string) (bool, error)
	// InFlightTotal is how much work is outstanding across the whole host.
	InFlightTotal(ctx context.Context) (int, error)

	// CurrentAssessments returns HarborMaster's own positive findings that a
	// container needs no update, keyed by container NAME.
	//
	// # Why this is not derivable from the plan table
	//
	// The planner writes NO row for a container it assessed and found current,
	// so "no plan" cannot distinguish "looked, and it is fine" from "did not
	// look". Only the first may release a dependent. See domain.AssessCurrent.
	//
	// A container absent from the map has no assessment, which is the zero
	// value and means UNKNOWN. An error, an unwired repository, or an estate
	// nothing is tracked in all produce an empty map -- and therefore hold
	// every dependent rather than releasing them.
	//
	// Keyed by name because lineage is: a recreation replaces the container and
	// its id with it, so an id-keyed fact would stop describing the container
	// at the exact moment it did its job.
	CurrentAssessments(ctx context.Context, now time.Time) (map[string]domain.CurrentAssessment, error)
}

// SelfReporter tells the engine which container HarborMaster is running in.
//
// One method, and it returns a value rather than accepting one: the engine may
// ASK what HarborMaster is and may never assert it.
type SelfReporter interface {
	Identity() domain.SelfIdentity
}

// selfIdentity reads the identity, tolerating an unwired reporter.
//
// The zero identity matches nothing, so a nil reporter degrades to "the engine
// does not know" rather than to "the engine excludes everything" or to a panic.
func (s *AutomationService) selfIdentity() domain.SelfIdentity {
	if s.self == nil {
		return domain.SelfIdentity{}
	}
	return s.self.Identity()
}

// AutomationPipeline is the three services a pass and the follower submit to.
//
// The whole mutation surface of this subsystem, in one interface, so a reader
// can see it is three request submissions and nothing else. Every method here
// is implemented by a service that owns its own Docker capability, runs its own
// preflight, and can refuse.
type AutomationPipeline interface {
	AcquisitionEnabled() bool
	ExecutionEnabled() bool
	RollbackEnabled() bool

	RequestAcquisition(ctx context.Context, request AcquisitionRequest) (domain.Acquisition, error)
	RequestExecution(ctx context.Context, request ExecutionRequest) (domain.Execution, error)
	RequestRollback(ctx context.Context, request RollbackRequest) (domain.Rollback, error)

	Acquisition(ctx context.Context, acquisitionID string) (domain.Acquisition, error)
	Execution(ctx context.Context, executionID string) (domain.Execution, error)
}

// AutomationOptions configures an AutomationService.
//
// Note what is absent: every Docker capability interface. There is nowhere to
// wire one, which is the strongest form the "automation is a caller" rule can
// take.
type AutomationOptions struct {
	Store    AutomationStore
	Policies AutomationPolicyStore
	Evidence AutomationEvidence
	Pipeline AutomationPipeline

	// Dependencies is the ordering evidence a pass reads and the coordinator the
	// follower advances.
	//
	// NOT a capability. It holds no Docker interface -- an architecture test
	// pins that by reflection -- and the engine's whole ability to affect the
	// host remains AutomationPipeline. Nil leaves the pass exactly as it was
	// before Phase 16.
	Dependencies AutomationDependencies

	// Self reports which container HarborMaster is running in, so a pass can
	// refuse to update it. Nil means the zero identity, which matches nothing --
	// and the execution service refuses independently, so a build without this
	// wired is still protected.
	Self SelfReporter

	// Audit records policy administration, passes, pauses, and approvals. The
	// mutations themselves are audited by the services that perform them.
	Audit *AuditRecorder

	// Notify raises operator notifications. Nil sends none, which is the default:
	// notifications are off unless a deployment asks for them, and every service
	// must behave identically without one.
	//
	// A Notifier is NOT a capability. It is one method that puts a message on a
	// queue, and there is nothing on it that can reach Docker -- the engine's
	// whole ability to affect the host remains AutomationPipeline.
	Notify Notifier

	Config config.Automation
	Logger *slog.Logger
	Now    func() time.Time
}

// AutomationService owns the scheduler and the follower.
type AutomationService struct {
	store        AutomationStore
	policies     AutomationPolicyStore
	evidence     AutomationEvidence
	pipeline     AutomationPipeline
	self         SelfReporter
	dependencies AutomationDependencies
	audit        *AuditRecorder
	notifier     Notifier

	cfg    config.Automation
	logger *slog.Logger
	now    func() time.Time

	// running guards the in-process single-pass rule. The DATABASE is
	// authoritative across processes and across a restart; this is what makes
	// the answer immediate for a caller rather than a constraint violation.
	mu      sync.Mutex
	running bool
}

// passRequest is one pass, with the account that asked for it.
type passRequest struct {
	trigger     domain.AutomationTrigger
	dryRun      bool
	requestedBy domain.Requester
}

// NewAutomationService builds an AutomationService.
func NewAutomationService(opts AutomationOptions) *AutomationService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.Interval <= 0 {
		cfg.Interval = config.DefaultAutomationInterval
	}
	if cfg.FollowInterval <= 0 {
		cfg.FollowInterval = config.DefaultAutomationFollowInterval
	}
	if cfg.PassTimeout <= 0 {
		cfg.PassTimeout = config.DefaultAutomationPassTimeout
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = config.DefaultAutomationMaxConcurrent
	}
	if cfg.MaxPerRun < 1 {
		cfg.MaxPerRun = config.DefaultAutomationMaxPerRun
	}
	if cfg.PruneInterval <= 0 {
		cfg.PruneInterval = config.DefaultAutomationPruneInterval
	}

	return &AutomationService{
		store:        opts.Store,
		policies:     opts.Policies,
		evidence:     opts.Evidence,
		pipeline:     opts.Pipeline,
		dependencies: opts.Dependencies,
		self:         opts.Self,
		audit:        opts.Audit,
		notifier:     opts.Notify,
		cfg:          cfg,
		logger:       logger,
		now:          now,
	}
}

// Enabled reports whether the engine may act in this deployment.
//
// Requires the configuration flag AND a pipeline that can actually acquire and
// recreate. A deployment that enabled automation but wired no execution service
// must report disabled rather than run passes that can only ever decide.
func (s *AutomationService) Enabled() bool {
	return s.cfg.Enabled &&
		s.store != nil && s.policies != nil && s.evidence != nil && s.pipeline != nil &&
		s.pipeline.AcquisitionEnabled() && s.pipeline.ExecutionEnabled()
}

// Readable reports whether the automation records can be served.
//
// Separate from Enabled: an operator must be able to write policies, read past
// runs, and review pauses on a deployment where the engine is switched off.
// Refusing to show yesterday's decisions because today's scheduler is disabled
// would hide exactly the history somebody turned it off to examine.
func (s *AutomationService) Readable() bool {
	return s.store != nil && s.policies != nil
}

// ------------------------------------------------------------------- run --

// Run drives the scheduler and the follower until ctx is cancelled.
//
// Both loops in one goroutine, deliberately. They must not overlap: a follower
// submitting a recreation while a pass is deciding whether to submit another
// for the same container would be two components racing on the same rule, and
// the in-flight check that prevents it is read at the start of a pass.
func (s *AutomationService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("automation engine disabled by configuration")
		return
	}
	if !s.Enabled() {
		// Configured on, but the capability behind it is missing. Loud, because
		// an operator who set AUTOMATION_ENABLED believes their estate is being
		// kept current and it is not.
		s.logger.Error("automation is enabled but the recreation pipeline is not; no pass will run",
			slog.Bool("acquisitionEnabled", s.pipeline != nil && s.pipeline.AcquisitionEnabled()),
			slog.Bool("executionEnabled", s.pipeline != nil && s.pipeline.ExecutionEnabled()))
		return
	}

	// A pass that a restart cut short is bookkeeping, not a host in an unknown
	// state: a pass submits work to services that checkpoint their own. Marked
	// interrupted so the single-run rule does not block every future pass.
	if count, err := s.store.InterruptRuns(ctx, s.now()); err != nil {
		s.logger.Error("could not close out interrupted automation passes", slog.Any("error", err))
	} else if count > 0 {
		s.logger.Warn("closed out automation passes a restart interrupted",
			slog.Int("count", count))
	}

	s.logger.Info("automation engine started",
		slog.Duration("interval", s.cfg.Interval),
		slog.Duration("followInterval", s.cfg.FollowInterval),
		slog.Duration("startupDelay", s.cfg.StartupDelay),
		slog.Int("maxConcurrent", s.cfg.MaxConcurrent),
		slog.Int("maxPerRun", s.cfg.MaxPerRun),
		slog.Bool("requireApprovalForMajor", s.cfg.RequireApprovalForMajor))

	// The first pass waits for the inventory, image intelligence, and planner
	// to have run. Deciding against a stale inventory is deciding against a
	// host nobody has looked at.
	startup := time.NewTimer(s.cfg.StartupDelay)
	defer startup.Stop()

	schedule := time.NewTicker(s.cfg.Interval)
	defer schedule.Stop()
	follow := time.NewTicker(s.cfg.FollowInterval)
	defer follow.Stop()
	prune := time.NewTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("automation engine stopped")
			return

		case <-startup.C:
			s.runScheduledPass(ctx, domain.AutoTriggerStartup)

		case <-schedule.C:
			s.runScheduledPass(ctx, domain.AutoTriggerSchedule)

		case <-follow.C:
			s.follow(ctx)

		case <-prune.C:
			s.prune(ctx)
		}
	}
}

// RunNow runs a pass immediately, on the caller's goroutine.
//
// # Why this does not hand the work to the scheduler loop
//
// An earlier shape queued the request and waited for the loop to pick it up.
// That made a synchronous API call depend on a background goroutine being
// alive: if the loop had exited -- because the engine was disabled, because Run
// was never called, because a test built the service directly -- the caller
// blocked until its own context expired, and the failure looked like a hang
// rather than a refusal.
//
// Running it here removes that dependency entirely. Overlap is still
// impossible: the in-process mutex in runPass refuses a second pass
// immediately, and the database's single-run rule refuses one across processes
// and across a restart.
//
// # Why the pass is detached from the request context
//
// The pass is bounded by AUTOMATION_PASS_TIMEOUT either way, but a browser tab
// closing must not abandon a pass that has already submitted work: the record
// of what it did is what makes the history complete. GraceContext gives the
// pass its full budget whether or not the caller is still waiting.
//
// The requester is the AUTHENTICATED account from the request context. It is
// recorded on the run and carried into every acquisition the pass submits, so a
// host change automation made on somebody's instruction is attributed to them
// rather than to "the scheduler".
func (s *AutomationService) RunNow(
	ctx context.Context,
	dryRun bool,
	requestedBy domain.Requester,
) (domain.AutomationRun, []domain.AutomationDecision, error) {
	if !s.Enabled() {
		return domain.AutomationRun{}, nil, ErrAutomationDisabled
	}

	trigger := domain.AutoTriggerManual
	if dryRun {
		trigger = domain.AutoTriggerDryRun
	}

	passCtx, cancel := GraceContext(ctx, s.cfg.PassTimeout, s.cfg.PassTimeout)
	defer cancel()

	return s.runPass(passCtx, passRequest{
		trigger:     trigger,
		dryRun:      dryRun,
		requestedBy: requestedBy,
	})
}

// ------------------------------------------------------------------ pass --

// runPass executes one decision pass under the single-pass guard.
func (s *AutomationService) runPass(
	ctx context.Context,
	request passRequest,
) (domain.AutomationRun, []domain.AutomationDecision, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return domain.AutomationRun{}, nil, ErrAutomationBusy
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	passCtx, cancel := context.WithTimeout(ctx, s.cfg.PassTimeout)
	defer cancel()

	return s.pass(passCtx, request)
}

// runScheduledPass runs a pass the timer asked for, logging any refusal.
//
// A scheduled pass has nobody to return an error to, so the one thing it must
// not do is fail silently.
func (s *AutomationService) runScheduledPass(ctx context.Context, trigger domain.AutomationTrigger) {
	if _, _, err := s.runPass(ctx, passRequest{trigger: trigger}); err != nil {
		if errors.Is(err, ErrAutomationBusy) {
			s.logger.WarnContext(ctx, "skipped a scheduled automation pass; one is already running",
				slog.String("trigger", string(trigger)))
			return
		}
		s.logger.ErrorContext(ctx, "a scheduled automation pass failed",
			slog.String("trigger", string(trigger)), slog.Any("error", err))
	}
}

// pass is the whole decision pass: start a run, decide, submit, finish.
func (s *AutomationService) pass(
	ctx context.Context,
	request passRequest,
) (domain.AutomationRun, []domain.AutomationDecision, error) {
	started := s.now().UTC()

	run, err := s.store.StartRun(ctx, domain.AutomationRun{
		RunID:       domain.NewAutomationRunID(),
		Trigger:     request.trigger,
		DryRun:      request.dryRun,
		RequestedBy: request.requestedBy,
		StartedAt:   started,
	})
	if err != nil {
		if errors.Is(err, store.ErrAutomationRunActive) {
			return domain.AutomationRun{}, nil, ErrAutomationBusy
		}
		s.logger.ErrorContext(ctx, "could not start an automation pass", slog.Any("error", err))
		return domain.AutomationRun{}, nil, err
	}

	if request.requestedBy.Known() {
		s.recordAudit(ctx, domain.AuditAutomationRunStarted, domain.AuditSucceeded,
			run.RunID, requesterActor(request.requestedBy),
			"an automation pass was started by hand")
	}

	decisions, counts, passErr := s.decide(ctx, run, request)

	finished := s.now().UTC()
	counts.DurationMs = finished.Sub(started).Milliseconds()

	state := domain.RunCompleted
	if passErr != nil {
		state = domain.RunFailed
		// HarborMaster's own sentence. Never the underlying error text, which
		// can carry a daemon or registry string.
		counts.Message = "the pass could not be completed"
	}

	// The decisions are written BEFORE the run is finished, so a reader that
	// sees a completed run sees the reasoning behind it. Written on the failure
	// path too: the containers the pass did reach are the evidence for what it
	// was doing when it stopped.
	if len(decisions) > 0 {
		if _, err := s.store.RecordDecisions(ctx, decisions); err != nil {
			s.logger.ErrorContext(ctx, "could not record automation decisions",
				slog.String("runId", run.RunID), slog.Any("error", err))
		}
		s.notifyDecisions(decisions)
	}

	if passErr != nil {
		// HarborMaster's own sentence, the same one the run record carries.
		// Never the underlying error, which can carry a daemon or registry
		// string.
		NotifySchedulerError(s.notifier, "pass", counts.Message)
	}

	if err := s.store.FinishRun(ctx, run.RunID, state, counts, finished); err != nil {
		s.logger.ErrorContext(ctx, "could not close an automation pass",
			slog.String("runId", run.RunID), slog.Any("error", err))
	}

	run.State = state
	run.Considered = counts.Considered
	run.Eligible = counts.Eligible
	run.Submitted = counts.Submitted
	run.Skipped = counts.Skipped
	run.Failed = counts.Failed
	run.Message = counts.Message
	run.CompletedAt = &finished
	run.DurationMs = counts.DurationMs

	s.auditRunOutcome(ctx, run, request)

	s.logger.InfoContext(ctx, "automation pass finished",
		slog.String("runId", run.RunID),
		slog.String("trigger", string(run.Trigger)),
		slog.String("state", string(state)),
		slog.Bool("dryRun", run.DryRun),
		slog.Int("considered", run.Considered),
		slog.Int("eligible", run.Eligible),
		slog.Int("submitted", run.Submitted),
		slog.Int("failed", run.Failed),
		slog.Int64("durationMs", run.DurationMs))

	return run, decisions, passErr
}

// notifyDecisions raises the notifications a pass's reasoning implies.
//
// Only ONE kind: an update that will not happen until a person approves it.
// Every other verdict is either something that already produced its own
// notification -- a submitted update raises acquisition and execution events of
// its own -- or something nobody wants a message about, and a pass over two
// hundred containers that raised two hundred messages would be a subsystem an
// operator switched off within a day.
//
// Bounded, because a pass's decision count is bounded by the estate rather than
// by anything HarborMaster chose. Past the bound the rest are counted into one
// message instead of dropped silently.
func (s *AutomationService) notifyDecisions(decisions []domain.AutomationDecision) {
	if s.notifier == nil {
		return
	}

	raised := 0
	pending := 0
	for _, decision := range decisions {
		if decision.Verdict != domain.VerdictAwaitingApproval {
			continue
		}
		pending++
		if raised >= maxApprovalNotificationsPerPass {
			continue
		}
		raised++
		NotifyApprovalRequired(s.notifier, decision.ContainerName,
			decision.PlanID, decision.Detail)
	}

	if pending > raised {
		s.logger.Info("more updates await approval than were notified individually",
			slog.Int("awaitingApproval", pending),
			slog.Int("notified", raised))
		NotifySchedulerError(s.notifier, "approvals",
			"More updates are waiting for approval than HarborMaster sends "+
				"individual messages for. See the automation page for the full list.")
	}
}

// maxApprovalNotificationsPerPass bounds how many approval messages one pass
// sends.
//
// A first run against an estate that has never been updated can produce a
// decision per container. Twenty messages is a prompt; two hundred is a reason
// to turn notifications off.
const maxApprovalNotificationsPerPass = 20

// decide evaluates every container and submits what may be submitted.
func (s *AutomationService) decide(
	ctx context.Context,
	run domain.AutomationRun,
	request passRequest,
) ([]domain.AutomationDecision, domain.AutomationRun, error) {
	var counts domain.AutomationRun

	policies, err := s.policies.ActivePolicies(ctx)
	if err != nil {
		return nil, counts, fmt.Errorf("load update policies: %w", err)
	}
	inFlight, err := s.evidence.InFlightTotal(ctx)
	if err != nil {
		return nil, counts, fmt.Errorf("count outstanding work: %w", err)
	}

	// ONE clock reading for the whole pass. Two containers whose windows differ
	// by a millisecond must not get different answers because time moved
	// between them.
	now := s.now().UTC()
	// The identity is read once too, for the same reason -- inside
	// evaluateEstate, which is the only place that needs it.

	budget := NewAutomationBudget(s.cfg.MaxPerRun, s.cfg.MaxConcurrent, inFlight, registryOfReference)

	// ---- phases 1 and 2: decide, then gate -------------------------------
	//
	// Shared with the readiness query, and shared deliberately. Both answer the
	// same question over the same evidence, and the moment they answered it in
	// two places they disagreed: the preview reported a container as eligible
	// that the pass held for its dependencies. One implementation cannot do
	// that. See automation_readiness.go.
	//
	// Phase 1 is DecideAutomation, the same pure function reading the same
	// inputs. Phase 2 is the dependency gate, which may do exactly one thing to
	// a decision: turn an eligible one into a held or blocked one. A pass with
	// no dependency evidence, or one whose graph could not be built, leaves
	// every decision exactly as it was.
	evaluation, err := s.evaluateEstate(ctx, policies, now)
	if err != nil {
		return nil, counts, err
	}
	outcomes, order := evaluation.Outcomes, evaluation.Order
	counts.Considered = len(outcomes)

	if evaluation.Truncated {
		// Said out loud rather than silently covering a prefix of the estate.
		counts.Message = "this host has more containers than one pass may consider; " +
			"some were not examined"
		s.logger.WarnContext(ctx, "automation pass truncated the estate",
			slog.String("runId", run.RunID),
			slog.Int("limit", store.MaxAutomationTargets))
	}

	// The run each decision belongs to. Set here rather than inside the shared
	// evaluation because a readiness query has no run to belong to.
	for index := range outcomes {
		outcomes[index].Decision.RunID = run.RunID
	}

	// ---- phase 3: submit, in stage order ---------------------------------
	//
	// The budget is applied in the SAME order the graph implies, so a per-run
	// ceiling truncates the tail of the estate rather than a slice through the
	// middle of a chain. A container deferred by the budget stays waiting for a
	// later pass; it is never marked blocked.
	for _, index := range order {
		if ctx.Err() != nil {
			return collectDecisions(outcomes), counts, ctx.Err()
		}
		outcome := &outcomes[index]

		switch {
		case outcome.Eligible():
			counts.Eligible++
			// A dry run never acts, whatever the policy said. Checked here as
			// well as in the mode branch so the pass-level flag is the last
			// word: an operator who asked for a preview gets a preview.
			if request.dryRun {
				outcome.Decision.Verdict = domain.VerdictWouldUpdate
				outcome.Decision.Reason = domain.ReasonDryRunMode
				outcome.Decision.Detail = "this pass is a dry run, so nothing was changed"
				counts.Skipped++
				break
			}
			reason, admitted := budget.Admit(outcome.Decision, outcome.Effective)
			if !admitted {
				outcome.Decision.Verdict = domain.VerdictSkip
				outcome.Decision.Reason = reason
				outcome.Decision.Detail = reason.Explain()
				counts.Skipped++
				break
			}
			if s.submit(ctx, run, outcome, request.requestedBy) {
				counts.Submitted++
			} else {
				counts.Failed++
			}

		case outcome.Decision.Verdict == domain.VerdictWouldUpdate,
			outcome.Decision.Verdict == domain.VerdictAwaitingApproval:
			counts.Eligible++
			counts.Skipped++

		default:
			counts.Skipped++
		}
	}

	return collectDecisions(outcomes), counts, nil
}

// collectDecisions projects the outcomes onto the records a pass stores.
//
// In TARGET order rather than submission order, because the decision list is a
// record of what the pass thought about every container and an operator reading
// it should see the estate, not the schedule.
func collectDecisions(outcomes []AutomationOutcome) []domain.AutomationDecision {
	decisions := make([]domain.AutomationDecision, 0, len(outcomes))
	for _, outcome := range outcomes {
		decisions = append(decisions, outcome.Decision)
	}
	return decisions
}

// loadContainerEvidence fills in the per-container reads.
//
// Skipped for a container a cheaper check has already declined, so a pass over
// an estate where nothing is governed costs one policy query and one inventory
// query rather than two lookups per container. The decision function still
// re-applies every check: this is an optimisation, never an authority.
func (s *AutomationService) loadContainerEvidence(
	ctx context.Context,
	input *AutomationInput,
	pausedBy map[string]domain.PausedContainer,
	policies []domain.UpdatePolicy,
) {
	if input.IsPaused && input.Pause.Active(input.Now) {
		return
	}
	overrides := domain.ParseUpdateLabels(input.Target.Labels)
	if overrides.Disabled || overrides.Paused {
		return
	}
	if _, governed := domain.SelectUpdatePolicy(policies, input.Target, input.Self); !governed {
		return
	}

	plan, err := s.evidence.CurrentPlan(ctx, input.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		input.HasPlan = false
	case err != nil:
		// A plan that could not be read is not a plan. Fails towards "no
		// update", which is the safe direction.
		s.logger.WarnContext(ctx, "could not read a container's change plan",
			slog.String("containerId", domain.ShortenID(input.ContainerID)),
			slog.Any("error", err))
		input.HasPlan = false
	default:
		input.Plan, input.HasPlan = plan, true
	}

	acquiring, err := s.evidence.AcquisitionActive(ctx, input.ContainerID)
	if err != nil {
		// Could not establish that nothing is in flight, so it is treated as in
		// flight. Fails closed.
		input.InFlight = true
		return
	}
	if acquiring {
		input.InFlight = true
		return
	}
	recreating, err := s.evidence.ExecutionActive(ctx, input.ContainerID)
	if err != nil {
		input.InFlight = true
		return
	}
	input.InFlight = recreating
}

// submit asks the acquisition service for the update.
//
// The ONLY place a pass causes anything to happen, and all it does is build a
// two-field request. Reports whether the submission was accepted; a refusal is
// recorded on the decision rather than raised, because a refusal is the
// preflight working.
func (s *AutomationService) submit(
	ctx context.Context,
	run domain.AutomationRun,
	outcome *AutomationOutcome,
	requestedBy domain.Requester,
) bool {
	decision := &outcome.Decision

	acquisition, err := s.pipeline.RequestAcquisition(ctx, AcquisitionRequest{
		// The plan id, and nothing else. Every identity the pull will use --
		// registry, repository, digest, platform -- is derived from the plan by
		// the acquisition service's own preflight.
		PlanID: decision.PlanID,
		// Deterministic, so a pass that is somehow retried cannot start a
		// second pull for the same decision.
		RequestKey:  automationRequestKey(run.RunID, decision.PlanID),
		RequestedBy: automationRequester(requestedBy),
	})
	if err != nil {
		var refused ErrAcquisitionRefused
		if errors.As(err, &refused) {
			decision.Verdict = domain.VerdictSkip
			decision.Reason = domain.ReasonRefused
			decision.Detail = "the acquisition preflight refused: " + refused.Refusal.Explain()
			return false
		}
		decision.Verdict = domain.VerdictSkip
		decision.Reason = domain.ReasonError
		decision.Detail = "the acquisition could not be requested"
		s.logger.ErrorContext(ctx, "automation could not request an acquisition",
			slog.String("runId", run.RunID),
			slog.String("planId", decision.PlanID),
			slog.Any("error", err))
		return false
	}

	decision.AcquisitionID = acquisition.AcquisitionID
	s.logger.InfoContext(ctx, "automation requested an image acquisition",
		slog.String("runId", run.RunID),
		slog.String("acquisitionId", acquisition.AcquisitionID),
		slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
		slog.String("policyId", decision.PolicyID))
	return true
}

// automationRequester attributes work to the account that caused it.
//
// A manual pass carries the operator who pressed the button. A SCHEDULED pass
// carries nobody, and says so: the empty requester renders as "an unrecorded
// account" everywhere, and inventing a synthetic "automation" user would put a
// name in the audit log that no account could ever be held to.
func automationRequester(requestedBy domain.Requester) domain.Requester {
	return requestedBy
}

// automationRequestKey is the idempotency key for one decision's submission.
//
// Derived from the run and the plan, both of which HarborMaster generated. Not
// from anything a caller supplies, and not random: the whole point is that the
// same decision submitted twice is one request.
func automationRequestKey(runID, planID string) string {
	return domain.AutomationRequestKeyPrefix + runID + ":" + planID
}

// registryOfReference extracts the registry host from an image reference.
//
// Best-effort and used ONLY for the per-registry concurrency bucket, never for
// a network decision: the acquisition service resolves the real registry from
// the plan and validates it against its own allowlist. A reference this cannot
// parse buckets under the empty string, which groups the unparseable together
// and still bounds them.
func registryOfReference(reference string) string {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return ""
	}
	slash := strings.Index(trimmed, "/")
	if slash < 0 {
		// No slash means an official Docker Hub image, e.g. "nginx:1.27".
		return "docker.io"
	}
	head := trimmed[:slash]
	// A registry host has a dot, a colon, or is literally localhost. Anything
	// else is a Docker Hub namespace, e.g. "library/nginx".
	if strings.ContainsAny(head, ".:") || head == "localhost" {
		return head
	}
	return "docker.io"
}
