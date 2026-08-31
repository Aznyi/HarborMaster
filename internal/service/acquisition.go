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

// Safe image acquisition.
//
// # The one thing HarborMaster can change
//
// This service holds HarborMaster's entire ability to mutate the Docker host:
// downloading an approved image into the local store. Nothing here starts,
// stops, recreates, or reconfigures a container, and nothing here can --
// docker.ImageAcquirer has exactly one method, pinned by an architecture test.
//
// **No container is updated by any of this.** A container keeps running the
// image it was created from. An acquired image is another entry in the local
// store beside it, ready for an operator to use with their own tooling.
//
// # The safety model, in order
//
//  1. An operator asks, naming a CHANGE PLAN. There is no free-form target.
//  2. The plan and all its supporting evidence are re-read and re-checked
//     immediately before the pull -- not trusted from when the plan was written.
//  3. The target is resolved to an immutable digest, and refused without one.
//  4. Only that registry, repository, and digest are pulled.
//  5. The result is VERIFIED by read-only inspection: digest and platform.
//  6. An immutable audit record is written either way.
//
// Step 2 is the important one. The gap between "a plan said this was
// reasonable" and "we are downloading it" is a time-of-check/time-of-use
// window, and every refusal in domain.AcquisitionRefusal names a way that
// window can be abused or can simply pass.
//
// # It fails closed, everywhere
//
// Any check that cannot be COMPLETED refuses, and refusing is cheap: an
// operator asks again. The converse -- proceeding because a check was
// inconclusive -- would mean pulling on the strength of evidence nobody has.

// ErrAcquisitionDisabled reports that image acquisition is switched off.
var ErrAcquisitionDisabled = errors.New("image acquisition is disabled")

// ErrAcquisitionRefused reports that the preflight refused a request.
//
// Carries the specific refusal, so the API can report which check said no
// rather than a generic failure.
type ErrAcquisitionRefused struct {
	Refusal domain.AcquisitionRefusal
}

func (e ErrAcquisitionRefused) Error() string { return e.Refusal.Explain() }

// AcquisitionStore is the persistence capability the service needs.
//
// A narrow interface rather than *store.AcquisitionRepository, so the service
// is testable without a database and so the surface it depends on is visible in
// one place.
type AcquisitionStore interface {
	Create(ctx context.Context, acquisition domain.Acquisition, now time.Time) (domain.Acquisition, error)
	Get(ctx context.Context, acquisitionID string) (domain.Acquisition, error)
	List(ctx context.Context, filter store.AcquisitionFilter) ([]domain.Acquisition, int, error)
	Events(ctx context.Context, acquisitionID string, limit int) ([]domain.AcquisitionEvent, error)
	Summary(ctx context.Context) (domain.AcquisitionSummary, error)
	Advance(ctx context.Context, change store.StateChange, now time.Time) (bool, error)
	RecordProgress(ctx context.Context, acquisitionID, progress string, bytes int64, layers int, now time.Time, maxEvents int) error
	ActiveCount(ctx context.Context, registry string) (total, perRegistry int, err error)
	ActiveForTarget(ctx context.Context, containerID, digest string) (bool, error)
	ByRequestKey(ctx context.Context, key string) (domain.Acquisition, bool, error)
	Claimable(ctx context.Context, limit int) ([]domain.Acquisition, error)
	RecoverInterrupted(ctx context.Context, now time.Time) (int64, error)
	ExpireStale(ctx context.Context, now time.Time, batch int) (int64, error)
	Prune(ctx context.Context, cutoff time.Time, batch int) (int64, error)
}

// AcquisitionEvidence is the read-only data the preflight re-checks.
//
// Every method is a READ. The preflight's whole job is to establish that the
// world still matches what was approved, and it must not be able to change
// anything while doing so.
type AcquisitionEvidence interface {
	// Plan returns the change plan by its immutable id.
	Plan(ctx context.Context, planID string) (domain.ChangePlan, error)
	// CurrentPlan returns the newest plan for a container, which is how a
	// superseded approval is detected.
	CurrentPlan(ctx context.Context, containerID string) (domain.ChangePlan, error)
	// Intel returns what the registry most recently reported for a reference.
	Intel(ctx context.Context, reference string) (domain.ImageIntel, error)
	// ContainerPresent reports whether the container still exists.
	ContainerPresent(ctx context.Context, containerID string) (bool, error)
}

// AcquisitionOptions configures an AcquisitionService.
type AcquisitionOptions struct {
	Store    AcquisitionStore
	Evidence AcquisitionEvidence
	// Runtime is the READ-ONLY capability, used to confirm the daemon is
	// reachable and to verify the acquired image afterwards.
	Runtime docker.Runtime
	// Acquirer is the mutation. Nil disables acquisition entirely, which is the
	// correct behaviour for a deployment that has not opted in.
	Acquirer docker.ImageAcquirer

	// Self reports which container HarborMaster is running in, so the preflight
	// can refuse a plan that names it. Nil means the zero identity, which
	// matches nothing -- and the execution service refuses independently.
	Self SelfReporter

	// Notify raises operator notifications. Nil sends none, which is the default:
	// notifications are off unless a deployment asks for them, and every service
	// must behave identically without one.
	Notify Notifier

	// Audit records what a finished download did to the host, attributed to the
	// account that asked for it. Nil records nothing, which is correct for a
	// test but not for a deployment.
	Audit *AuditRecorder

	Config config.Acquisition
	Logger *slog.Logger
	Now    func() time.Time
}

// AcquisitionService owns the acquisition lifecycle.
type AcquisitionService struct {
	store    AcquisitionStore
	evidence AcquisitionEvidence
	runtime  docker.Runtime
	acquirer docker.ImageAcquirer
	// self reports HarborMaster's own container. Read on every preflight.
	notifier Notifier
	self     SelfReporter
	// audit records the OUTCOME of a download in the security log. Nil in a
	// test that is not about attribution; reportOutcome is nil-safe.
	audit *AuditRecorder

	cfg    config.Acquisition
	logger *slog.Logger
	now    func() time.Time

	// wake carries a one-slot signal that queued work exists. Capacity 1 with a
	// non-blocking send: a second signal while one is unread is redundant.
	wake chan struct{}

	// running tracks in-flight acquisitions so cancellation can reach the
	// context driving a pull. The map is the ONLY way to stop a transfer that
	// has already started.
	mu      sync.Mutex
	running map[string]context.CancelFunc
	// perRegistry counts in-flight work per registry, in this process. The
	// database count is authoritative across restarts; this is what makes the
	// limit hold between the check and the claim.
	perRegistry map[string]int
	inFlight    int
}

// NewAcquisitionService builds an AcquisitionService.
func NewAcquisitionService(opts AcquisitionOptions) *AcquisitionService {
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
		cfg.MaxConcurrent = config.DefaultAcquisitionMaxConcurrent
	}
	if cfg.MaxPerRegistry < 1 {
		cfg.MaxPerRegistry = config.DefaultAcquisitionMaxPerRegistry
	}
	if cfg.PullTimeout <= 0 {
		cfg.PullTimeout = config.DefaultAcquisitionPullTimeout
	}
	if cfg.RequestTTL <= 0 {
		cfg.RequestTTL = config.DefaultAcquisitionRequestTTL
	}
	if cfg.MaxEventsPerAcquisition < 1 {
		cfg.MaxEventsPerAcquisition = config.DefaultAcquisitionMaxEvents
	}
	if cfg.RegistryFreshness <= 0 {
		cfg.RegistryFreshness = config.DefaultAcquisitionRegistryFreshness
	}

	return &AcquisitionService{
		store:       opts.Store,
		evidence:    opts.Evidence,
		runtime:     opts.Runtime,
		acquirer:    opts.Acquirer,
		notifier:    opts.Notify,
		self:        opts.Self,
		audit:       opts.Audit,
		cfg:         cfg,
		logger:      logger,
		now:         now,
		wake:        make(chan struct{}, 1),
		running:     make(map[string]context.CancelFunc),
		perRegistry: make(map[string]int),
	}
}

// Enabled reports whether acquisition is available in this deployment.
//
// Requires BOTH the configuration flag and an actual mutation capability. A
// deployment that enabled the feature but wired no acquirer must report
// disabled rather than accept requests it can never run.
func (s *AcquisitionService) Enabled() bool {
	return s.cfg.Enabled && s.acquirer != nil && s.store != nil && s.runtime != nil
}

// ------------------------------------------------------------- requesting --

// AcquisitionRequest is an operator's ask.
//
// Note what it does NOT carry: no registry, no repository, no digest, no tag,
// no pull option, no platform. Every one of those is derived from the plan.
// There is no field here an attacker could aim.
type AcquisitionRequest struct {
	// PlanID names the approved change plan.
	PlanID string
	// RequestKey is an optional idempotency key, so a retried request does not
	// start a second pull.
	RequestKey string

	// RequestedBy is the account asking. Stored on the record so the OUTCOME
	// can be attributed minutes later by a worker that has no request and no
	// session; see reportOutcome.
	RequestedBy domain.Requester
}

// Request validates and records an acquisition request.
//
// Runs the FULL preflight synchronously, so an operator gets a real answer
// rather than a queued job that fails a moment later. The pull itself is
// asynchronous, and the preflight runs AGAIN immediately before it -- this call
// can be minutes ahead of a worker slot, and the world can move in between.
func (s *AcquisitionService) Request(
	ctx context.Context,
	request AcquisitionRequest,
) (domain.Acquisition, error) {
	if !s.Enabled() {
		return domain.Acquisition{}, ErrAcquisitionRefused{Refusal: domain.AcquisitionRefusalDisabled}
	}

	// Idempotency is answered FIRST, before any check that could refuse.
	//
	// A retried request must return the record it already created, and it must
	// do so even when the situation has since changed -- the caller is asking
	// "what happened to my request", not "may I make a new one". Running the
	// preflight first would refuse the retry as a duplicate of itself.
	if request.RequestKey != "" {
		existing, found, err := s.store.ByRequestKey(ctx, request.RequestKey)
		if err != nil {
			return domain.Acquisition{}, err
		}
		if found {
			return existing, nil
		}
	}

	decision, err := s.preflight(ctx, request.PlanID, "")
	if err != nil {
		return domain.Acquisition{}, err
	}
	if decision.Refusal != domain.AcquisitionRefusalNone {
		return domain.Acquisition{}, ErrAcquisitionRefused{Refusal: decision.Refusal}
	}

	// Admission control. Deliberately NOT part of the preflight: the preflight
	// runs again inside the worker, at which point this acquisition already
	// holds a slot and would refuse itself.
	if refusal, err := s.admit(ctx, decision); err != nil {
		return domain.Acquisition{}, err
	} else if refusal != domain.AcquisitionRefusalNone {
		return domain.Acquisition{}, ErrAcquisitionRefused{Refusal: refusal}
	}

	now := s.now().UTC()
	acquisition := domain.Acquisition{
		AcquisitionID: domain.NewAcquisitionID(),
		PlanID:        decision.Plan.PlanID,
		ContainerID:   decision.Plan.ContainerID,
		ContainerName: decision.Plan.ContainerName,
		Target:        decision.Target,
		State:         domain.AcquisitionQueued,
		RequestedAt:   now,
		ExpiresAt:     now.Add(s.cfg.RequestTTL),
		RequestKey:    request.RequestKey,
		PlanDigest:    decision.Plan.InputDigest,
		RequestedBy:   request.RequestedBy,
	}

	created, err := s.store.Create(ctx, acquisition, now)
	if err != nil {
		// The unique index refused: another acquisition for this target is
		// already in flight. That is the race the check above cannot close, and
		// it is a refusal rather than a fault.
		if errors.Is(err, store.ErrAcquisitionActive) {
			return domain.Acquisition{}, ErrAcquisitionRefused{Refusal: domain.AcquisitionRefusalDuplicate}
		}
		return domain.Acquisition{}, err
	}

	s.logger.InfoContext(ctx, "image acquisition requested",
		slog.String("acquisitionId", created.AcquisitionID),
		slog.String("containerId", domain.ShortenID(created.ContainerID)),
		slog.String("registry", created.Target.Registry),
		slog.String("repository", created.Target.Repository),
		// The digest is the whole point of the record, and it is not a secret.
		slog.String("digest", created.Target.Digest))

	s.signal()
	return created, nil
}

// Cancel stops an acquisition an operator no longer wants.
//
// Cancels the in-process context first, so a transfer already running stops,
// then records the terminal state. Ordering matters: recording first would
// leave a pull running against a row that says it finished.
func (s *AcquisitionService) Cancel(ctx context.Context, acquisitionID string) (domain.Acquisition, error) {
	if s.store == nil {
		return domain.Acquisition{}, ErrAcquisitionDisabled
	}

	existing, err := s.store.Get(ctx, acquisitionID)
	if err != nil {
		return domain.Acquisition{}, err
	}
	if !existing.State.Cancellable() {
		// Verifying is deliberately not cancellable: the bytes are already on
		// the host, and stopping the confirmation would leave an unverified
		// image with no record saying so.
		return existing, ErrAcquisitionRefused{Refusal: domain.AcquisitionRefusalNone}
	}

	s.mu.Lock()
	cancel, running := s.running[acquisitionID]
	s.mu.Unlock()
	if running {
		cancel()
	}

	moved, err := s.store.Advance(ctx, store.StateChange{
		AcquisitionID: acquisitionID,
		From: []domain.AcquisitionState{
			domain.AcquisitionQueued, domain.AcquisitionValidating, domain.AcquisitionPulling,
		},
		To:      domain.AcquisitionCancelled,
		Message: "cancelled by an operator",
		Detail:  "cancelled by an operator",
	}, s.now().UTC())
	if err != nil {
		return domain.Acquisition{}, err
	}
	if !moved {
		// It finished between the read and the write. Not an error: the
		// operator's intent was that it stop, and it has.
		return s.store.Get(ctx, acquisitionID)
	}

	s.logger.InfoContext(ctx, "image acquisition cancelled",
		slog.String("acquisitionId", acquisitionID))

	return s.store.Get(ctx, acquisitionID)
}

// ---------------------------------------------------------- the preflight --

// preflightDecision is what revalidation concluded.
type preflightDecision struct {
	Plan   domain.ChangePlan
	Target domain.AcquisitionTarget
	// Refusal is AcquisitionRefusalNone when the request may proceed.
	Refusal domain.AcquisitionRefusal
}

// preflight re-reads every piece of evidence and decides whether to proceed.
//
// # Why every check is here rather than spread out
//
// A refusal must be attributable to a named check, and the set of checks must
// be readable in one place. A reviewer asking "what would stop HarborMaster
// downloading the wrong thing" should have exactly one function to read.
//
// # It runs twice
//
// Once when the operator asks, so they get an immediate answer, and again
// immediately before the pull. The second run is the one that matters: it is
// what closes the time-of-check/time-of-use window.
//
// approvedDigest is empty on the first run and carries the digest the operator
// approved on the second, which is how a digest that moved in between is
// detected as a CHANGE rather than merely as a different value.
func (s *AcquisitionService) preflight(
	ctx context.Context,
	planID string,
	approvedDigest string,
) (preflightDecision, error) {
	var decision preflightDecision

	if !s.cfg.Enabled || s.acquirer == nil {
		decision.Refusal = domain.AcquisitionRefusalDisabled
		return decision, nil
	}

	// ---- the plan itself -------------------------------------------------

	plan, err := s.evidence.Plan(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		decision.Refusal = domain.AcquisitionRefusalPlanMissing
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	decision.Plan = plan

	// HARBORMASTER ITSELF.
	//
	// Checked here, at the top of the preflight, and independently of the
	// automation engine's own check. The engine is not the only caller: an
	// operator can POST /acquisitions with any plan id, and this is the layer
	// that has to hold when the one above is bypassed or wrong.
	//
	// Refused at the DOWNLOAD, though a download changes no container, because
	// the download is the first half of an operation whose second half cannot
	// complete. A succeeded acquisition that can never be spent reads as a
	// working feature right up to the moment it is not.
	if s.self != nil {
		identity := s.self.Identity()
		if self, _ := identity.SelfMatch(domain.SelfTarget{
			ContainerID:   plan.ContainerID,
			ContainerName: plan.ContainerName,
			ImageRef:      plan.CurrentImage,
		}); self {
			decision.Refusal = domain.AcquisitionRefusalSelfUpdate
			return decision, nil
		}
	}

	// ---- the container ---------------------------------------------------
	//
	// Checked BEFORE currency, and the order is about accuracy rather than
	// safety: both refuse, and neither permits anything the other would not.
	//
	// Since C3D a plan is current only while the container it assessed still
	// exists, so a departed container makes CurrentPlan answer "none" -- and
	// refusing there would tell the operator "the change plan no longer
	// exists", which is false. The plan is intact and readable; its CONTAINER
	// is gone. Asking the more specific question first means the refusal names
	// the real reason.
	present, err := s.evidence.ContainerPresent(ctx, plan.ContainerID)
	if err != nil {
		return decision, err
	}
	if !present {
		decision.Refusal = domain.AcquisitionRefusalContainerMissing
		return decision, nil
	}

	// A superseded plan means the operator approved an assessment that has
	// since been replaced. The newer one may say something different, and
	// acting on the older is acting on an opinion nobody currently holds.
	current, err := s.evidence.CurrentPlan(ctx, plan.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		decision.Refusal = domain.AcquisitionRefusalPlanMissing
		return decision, nil
	case err != nil:
		return decision, err
	case current.PlanID != plan.PlanID:
		decision.Refusal = domain.AcquisitionRefusalPlanSuperseded
		return decision, nil
	}

	// The recommendation gate. "unknown" refuses alongside "not recommended",
	// and that is the point: a gap in evidence is not permission. A model that
	// could not judge a change has not approved it.
	switch plan.Risk.Recommendation {
	case domain.RecommendProceed, domain.RecommendCaution, domain.RecommendManualReview:
	default:
		decision.Refusal = domain.AcquisitionRefusalRecommendation
		return decision, nil
	}

	// ---- policy and readiness --------------------------------------------

	// A critical policy violation means the organisation has already said this
	// container should not be running as it is. Acquiring an image for it is
	// not the moment to overrule that.
	if plan.PolicyOpen > 0 && plan.PolicyMaxSeverity == domain.PolicySeverityCritical {
		decision.Refusal = domain.AcquisitionRefusalPolicyViolation
		return decision, nil
	}

	// The documented restore-readiness policy: a snapshot must exist and must
	// not have FAILED its readiness check. A warning is accepted, because a
	// warning means the reference point is imperfect rather than unusable, and
	// refusing on one would make the feature unusable on a realistic estate.
	//
	// Downloading an image does not put a container at risk -- nothing is
	// applied -- so this gate is about the operator having a reference point
	// for what they do NEXT, with their own tooling.
	if s.cfg.RequireSnapshot {
		if !plan.SnapshotAvailable || plan.RestoreReadiness == domain.ReadinessNotReady {
			decision.Refusal = domain.AcquisitionRefusalRestoreReadiness
			return decision, nil
		}
	}

	// ---- the target ------------------------------------------------------

	if plan.ProposedDigest == "" || !domain.ValidImageDigest(plan.ProposedDigest) {
		decision.Refusal = domain.AcquisitionRefusalDigestUnavailable
		return decision, nil
	}

	// Registry intelligence, re-read. This is where the digest actually comes
	// from: the plan records what was on offer when it was written, and the
	// intel record says what is on offer now.
	reference, normalised := normalisedReference(plan.CurrentImage)
	if !normalised {
		decision.Refusal = domain.AcquisitionRefusalTargetRefused
		return decision, nil
	}

	intel, err := s.evidence.Intel(ctx, reference.Canonical)
	if errors.Is(err, store.ErrNotFound) {
		decision.Refusal = domain.AcquisitionRefusalRegistryStale
		return decision, nil
	}
	if err != nil {
		return decision, err
	}

	// Stale or unsuccessful registry evidence refuses. An old lookup does not
	// establish that the digest below is still the one being served, and the
	// digest is the entire safety property.
	if intel.Status != domain.CheckOK || intel.LastSuccessAt == nil {
		decision.Refusal = domain.AcquisitionRefusalRegistryStale
		return decision, nil
	}
	if s.now().UTC().Sub(intel.LastSuccessAt.UTC()) > s.cfg.RegistryFreshness {
		decision.Refusal = domain.AcquisitionRefusalRegistryStale
		return decision, nil
	}

	// THE TOCTOU CHECK. What is on offer NOW must be exactly what the plan
	// recorded, and -- on the second run -- what the operator approved.
	//
	// The comparison is over the REFERENCE AND DIGEST TOGETHER, rebuilt by the
	// same function the planner used. That matters for two reasons:
	//
	//   - The pair is what acquisition acts on. Checking the digest alone would
	//     pass a plan whose reference had since been re-pointed at a different
	//     tag, which is the crossing this phase exists to make impossible.
	//   - One definition of "what is on offer" means the planner and this check
	//     cannot drift apart. They used to: the planner paired a newer tag with
	//     the CURRENT tag's digest, and this check compared that same current
	//     digest against itself and passed vacuously.
	// A REATTACHMENT is checked against itself, not against the registry.
	//
	// # Why the registry comparison cannot apply here
	//
	// `proposedChange(intel)` answers "what would this image be UPGRADED to" --
	// the newest tag the registry offers. A rebind proposes the opposite: the
	// digest the container IS ALREADY RUNNING, so that it can be recreated
	// unchanged and reattached to a replaced provider. The two are different by
	// construction, so the comparison refused every reattachment with "the
	// digest on offer has changed" -- while the provider it was meant to
	// reattach to had already been replaced.
	//
	// Found live in Stage 5 against Docker 29.7.2.
	//
	// # What is checked instead, and why it is the same guarantee
	//
	// The property the TOCTOU check exists for is "the image that gets pulled is
	// the image the plan named, and nothing moved underneath". For a rebind the
	// plan's target came from HarborMaster's own inventory of what the container
	// is running, re-read immediately before the plan was built, and it is
	// already on this host. So the equivalent statement is that the plan is
	// self-consistent: it proposes the reference and digest it currently has.
	//
	// A rebind that fails this is one whose image fields were crossed, which is
	// the same crossing the ordinary path refuses -- and every other gate above
	// and below still applies unchanged, including the execution preflight's own
	// re-verification against the live container.
	// `targetReference` and `targetDigest` are what will actually be pulled.
	var targetReference, targetDigest string

	if plan.UpdateType == domain.UpdateRebind {
		if plan.ProposedImage != plan.CurrentImage ||
			plan.ProposedDigest != plan.CurrentDigest {
			decision.Refusal = domain.AcquisitionRefusalDigestChanged
			return decision, nil
		}
		if approvedDigest != "" && approvedDigest != plan.ProposedDigest {
			decision.Refusal = domain.AcquisitionRefusalDigestChanged
			return decision, nil
		}
		targetReference, targetDigest = plan.ProposedImage, plan.ProposedDigest
	} else {
		// THE TOCTOU CHECK, for a plan that moves an image.
		//
		// The plan's pair must be one the registry is serving RIGHT NOW. Both
		// candidates come from the same freshly-checked evidence, so a plan that
		// matches either names an image that has not moved underneath it; a plan
		// that matches neither is assessed against a registry that has since
		// changed, and is refused.
		offered := offeredTargets(intel)
		if len(offered) == 0 {
			decision.Refusal = domain.AcquisitionRefusalDigestUnavailable
			return decision, nil
		}
		var onOffer domain.ProposedTarget
		for _, candidate := range offered {
			if candidate.Reference() == plan.ProposedImage &&
				candidate.Digest() == plan.ProposedDigest {
				onOffer = candidate
				break
			}
		}
		if !onOffer.Valid() {
			decision.Refusal = domain.AcquisitionRefusalDigestChanged
			return decision, nil
		}
		if approvedDigest != "" && approvedDigest != onOffer.Digest() {
			decision.Refusal = domain.AcquisitionRefusalDigestChanged
			return decision, nil
		}
		targetReference, targetDigest = onOffer.Reference(), onOffer.Digest()
	}

	// The plan's own fingerprint. Recomputing it would mean re-gathering every
	// input, which the planner already does on a schedule; what is checked here
	// is that the plan HarborMaster is acting on is the one whose fingerprint
	// was approved, and that the planner has not since produced a different
	// assessment (the superseded check above).
	if plan.InputDigest == "" {
		decision.Refusal = domain.AcquisitionRefusalPlanStale
		return decision, nil
	}

	// The platform must be one the image actually publishes. PlatformMissing is
	// what the last successful lookup established.
	if intel.PlatformMissing {
		decision.Refusal = domain.AcquisitionRefusalPlatformUnavailable
		return decision, nil
	}

	// The registry and repository come from the PROPOSED reference, not the
	// current one. They are then required to match the current reference's,
	// because a proposal is a new tag in the same repository and never a move
	// to a different one -- a repository change here would be a pull from
	// somewhere the plan never assessed.
	proposedRef, proposedNormalised := normalisedReference(targetReference)
	if !proposedNormalised {
		decision.Refusal = domain.AcquisitionRefusalTargetRefused
		return decision, nil
	}
	if proposedRef.Host != reference.Host || proposedRef.Path != reference.Path {
		decision.Refusal = domain.AcquisitionRefusalTargetRefused
		return decision, nil
	}

	decision.Target = domain.AcquisitionTarget{
		// Docker Hub's canonical host is what a pull reference must name; the
		// API host is where the distribution API lives and is not always the
		// same string. Using the wrong one produces a reference the daemon
		// cannot resolve, so the reference host is taken from the normalised
		// reference.
		Registry:   proposedRef.Host,
		Repository: proposedRef.Path,
		Digest:     targetDigest,
		Reference:  targetReference,
		Platform:   intel.Platform,
	}

	if !decision.Target.Valid() {
		decision.Refusal = domain.AcquisitionRefusalTargetRefused
		return decision, nil
	}

	// ---- the host --------------------------------------------------------

	if !s.dockerReachable(ctx) {
		// Nothing can be established about the host, so nothing may be done to
		// it. Fails closed.
		decision.Refusal = domain.AcquisitionRefusalDockerUnavailable
		return decision, nil
	}

	return decision, nil
}

// normalisedReference resolves a reference, reporting whether it could be.
//
// A boolean rather than an error, because at this call site a reference that
// cannot be normalised is a REFUSAL rather than a fault: nothing is wrong with
// HarborMaster and there is nothing to retry. Returning an error would make the
// caller's "refuse" path indistinguishable from its "something broke" path.
func normalisedReference(raw string) (domain.NormalizedRef, bool) {
	reference, err := domain.NormalizeImageRef(raw)
	if err != nil {
		return domain.NormalizedRef{}, false
	}
	return reference, true
}

// dockerReachable reports whether the daemon answered.
//
// A boolean for the same reason as above: a daemon that is down means the
// preflight cannot establish anything about the host, which is a refusal. The
// underlying error carries daemon detail and is deliberately discarded rather
// than propagated toward a response.
func (s *AcquisitionService) dockerReachable(ctx context.Context) bool {
	_, err := s.runtime.Ping(ctx)
	return err == nil
}

// admit decides whether a NEW request fits within the configured limits.
//
// Separate from the preflight, and the separation matters: the preflight runs a
// second time inside the worker, by which point this acquisition is itself
// active. A limit check there would count the acquisition against its own
// budget and refuse every single request. Admission is a property of ACCEPTING
// work; the preflight is a property of the work still being safe.
//
// Duplicate detection is checked BEFORE the limits, because it is the more
// specific and more useful answer: "this image is already downloading" tells an
// operator something, while "too many downloads" would leave them retrying a
// request that will never be different.
func (s *AcquisitionService) admit(
	ctx context.Context,
	decision preflightDecision,
) (domain.AcquisitionRefusal, error) {
	active, err := s.store.ActiveForTarget(ctx,
		decision.Plan.ContainerID, decision.Target.Digest)
	if err != nil {
		return domain.AcquisitionRefusalNone, err
	}
	if active {
		return domain.AcquisitionRefusalDuplicate, nil
	}

	total, perRegistry, err := s.store.ActiveCount(ctx, decision.Target.Registry)
	if err != nil {
		return domain.AcquisitionRefusalNone, err
	}
	if total >= s.cfg.MaxConcurrent || perRegistry >= s.cfg.MaxPerRegistry {
		return domain.AcquisitionRefusalLimit, nil
	}

	return domain.AcquisitionRefusalNone, nil
}

// ------------------------------------------------------------- executing --

// execute runs one acquisition through validation, transfer, and verification.
//
// Every failure path writes a terminal record. An acquisition that stopped
// without saying why would be worse than one that failed loudly: the operator
// would not know whether an image had arrived.
func (s *AcquisitionService) execute(ctx context.Context, acquisition domain.Acquisition) {
	id := acquisition.AcquisitionID

	// The cancellable context for this acquisition, registered so Cancel can
	// reach it. Bounded by the configured pull timeout on top.
	pullCtx, cancel := context.WithTimeout(ctx, s.cfg.PullTimeout)
	defer cancel()

	// The concurrency slot was taken by reserve before this goroutine started;
	// it is RELEASED here, whatever happens below. Registering the cancel
	// function is what makes an in-flight transfer stoppable at all.
	s.mu.Lock()
	s.running[id] = cancel
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.inFlight--
		s.perRegistry[acquisition.Target.Registry]--
		if s.perRegistry[acquisition.Target.Registry] <= 0 {
			delete(s.perRegistry, acquisition.Target.Registry)
		}
		s.mu.Unlock()

		// The OUTCOME reaches the security audit log from exactly one place --
		// see ExecutionService.reportOutcome for why a deferred read-back beats
		// an audit call on each of the terminal paths.
		s.reportOutcome(ctx, acquisition)
	}()

	// ---- claim -----------------------------------------------------------

	claimed, err := s.store.Advance(ctx, store.StateChange{
		AcquisitionID: id,
		From:          []domain.AcquisitionState{domain.AcquisitionQueued},
		To:            domain.AcquisitionValidating,
		Detail:        "rechecking the plan and its supporting evidence",
		MarkStarted:   true,
	}, s.now().UTC())
	if err != nil {
		s.logger.WarnContext(ctx, "could not claim acquisition",
			slog.String("acquisitionId", id), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		// Cancelled or expired between being listed and being claimed. Another
		// path owns it now.
		return
	}

	// ---- revalidate ------------------------------------------------------

	// The second preflight, and the one that matters. The first ran when the
	// operator asked, which may have been minutes ago.
	decision, err := s.preflight(ctx, acquisition.PlanID, acquisition.Target.Digest)
	if err != nil {
		s.fail(ctx, id, domain.AcquisitionFailureInternal, domain.AcquisitionRefusalNone,
			"HarborMaster could not complete the safety checks")
		return
	}
	if decision.Refusal != domain.AcquisitionRefusalNone {
		s.refuse(ctx, id, decision.Refusal)
		return
	}
	// The target is taken from the REVALIDATION, not from the stored request.
	// They agree -- the digest check above proved it -- and taking the fresh one
	// means a future field added to the target cannot be acted on stale.
	target := decision.Target

	// ---- pull ------------------------------------------------------------

	moved, err := s.store.Advance(ctx, store.StateChange{
		AcquisitionID: id,
		From:          []domain.AcquisitionState{domain.AcquisitionValidating},
		To:            domain.AcquisitionPulling,
		Detail:        "downloading " + target.PinnedReference(),
	}, s.now().UTC())
	if err != nil || !moved {
		return
	}

	result, err := s.acquirer.PullByDigest(pullCtx, docker.PullTarget{
		Registry:   target.Registry,
		Repository: target.Repository,
		Digest:     target.Digest,
		Platform:   target.Platform,
	}, s.progressSink(ctx, id))

	if err != nil {
		failure, message := classifyAcquisitionFailure(ctx, pullCtx, err)
		if failure == domain.AcquisitionFailureNone {
			// Cancelled. The terminal record was written by Cancel, or the
			// process is shutting down and the recovery pass will mark it.
			return
		}
		s.fail(ctx, id, failure, domain.AcquisitionRefusalNone, message)
		return
	}

	// ---- verify ----------------------------------------------------------

	moved, err = s.store.Advance(ctx, store.StateChange{
		AcquisitionID:    id,
		From:             []domain.AcquisitionState{domain.AcquisitionPulling},
		To:               domain.AcquisitionVerifying,
		Detail:           "confirming the image that arrived",
		Layers:           result.Layers,
		BytesTransferred: result.BytesReported,
	}, s.now().UTC())
	if err != nil || !moved {
		return
	}

	s.verify(ctx, id, target, result)
}

// verify confirms that the image now on the host is the one that was approved.
//
// # Why the pull's own success is not enough
//
// A completed transfer means the daemon accepted the request and reported no
// error. It does not establish what is in the local store: the daemon resolves
// the reference itself, a registry can serve different content than expected,
// and a multi-platform image resolves differently per platform.
//
// So the image is re-read through the READ-ONLY inspection path and compared.
// This is the only step that can conclude "succeeded", and it fails closed:
// anything it cannot establish is a failure.
func (s *AcquisitionService) verify(
	ctx context.Context,
	id string,
	target domain.AcquisitionTarget,
	result docker.PullResult,
) {
	// Inspected by the digest-pinned reference, so the daemon resolves exactly
	// what was asked for rather than whatever a tag currently points at.
	image, err := s.runtime.InspectImage(ctx, target.PinnedReference())
	if err != nil {
		// The check could not be PERFORMED, which establishes nothing. That is
		// not the same as a mismatch, and it is recorded as its own
		// classification so an operator can tell the two apart.
		s.fail(ctx, id, domain.AcquisitionFailureVerification, domain.AcquisitionRefusalNone,
			domain.AcquisitionFailureVerification.Explain())
		return
	}

	acquiredPlatform := domain.Platform{
		OS:           image.OS,
		Architecture: image.Architecture,
		Variant:      image.Variant,
	}

	// The digest comparison. The local image must carry a repo digest matching
	// the approved one; anything else means the store holds something other
	// than what was approved.
	if !imageCarriesDigest(*image, target.Digest) {
		s.mismatch(ctx, id, domain.AcquisitionFailureDigestMismatch, *image, acquiredPlatform, result)
		return
	}

	// The platform comparison, when the plan established one. An image that
	// does not target this host would not run here, and reporting it as
	// acquired would be a promise HarborMaster cannot keep.
	if !target.Platform.Empty() &&
		(acquiredPlatform.OS != target.Platform.OS ||
			acquiredPlatform.Architecture != target.Platform.Architecture) {
		s.mismatch(ctx, id, domain.AcquisitionFailurePlatformMismatch, *image, acquiredPlatform, result)
		return
	}

	if _, err := s.store.Advance(ctx, store.StateChange{
		AcquisitionID:    id,
		From:             []domain.AcquisitionState{domain.AcquisitionVerifying},
		To:               domain.AcquisitionSucceeded,
		Message:          "the image is present locally and its digest was confirmed; no container has been changed",
		Detail:           "verified",
		AcquiredImageID:  image.ID,
		AcquiredDigest:   target.Digest,
		AcquiredPlatform: acquiredPlatform,
		SizeBytes:        image.Size,
		Layers:           result.Layers,
		BytesTransferred: result.BytesReported,
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record acquisition success",
			slog.String("acquisitionId", id), slog.String("error", err.Error()))
		return
	}

	s.logger.InfoContext(ctx, "image acquired",
		slog.String("acquisitionId", id),
		slog.String("registry", target.Registry),
		slog.String("repository", target.Repository),
		slog.String("digest", target.Digest),
		slog.Int64("sizeBytes", image.Size),
		// Stated on every success, because it is the fact most easily assumed
		// otherwise.
		slog.Bool("containerChanged", false))
}

// mismatch records a verification that found the wrong image.
//
// The acquired values are stored even though they disagree with the target:
// they are the EVIDENCE, and an operator investigating a digest mismatch needs
// to know what actually arrived.
func (s *AcquisitionService) mismatch(
	ctx context.Context,
	id string,
	failure domain.AcquisitionFailure,
	image domain.Image,
	platform domain.Platform,
	result docker.PullResult,
) {
	if _, err := s.store.Advance(ctx, store.StateChange{
		AcquisitionID:    id,
		From:             []domain.AcquisitionState{domain.AcquisitionVerifying},
		To:               domain.AcquisitionFailed,
		Failure:          failure,
		Message:          failure.Explain(),
		Detail:           "verification failed",
		AcquiredImageID:  image.ID,
		AcquiredPlatform: platform,
		SizeBytes:        image.Size,
		Layers:           result.Layers,
		BytesTransferred: result.BytesReported,
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record acquisition mismatch",
			slog.String("acquisitionId", id), slog.String("error", err.Error()))
	}

	// Logged at ERROR. A digest mismatch is not routine: it means the host now
	// holds content that was never approved, and the two ways that happens --
	// a daemon resolving differently, a registry serving differently -- both
	// warrant a person looking.
	s.logger.ErrorContext(ctx, "acquired image did not match what was approved",
		slog.String("acquisitionId", id),
		slog.String("failure", string(failure)),
		slog.String("acquiredImageId", domain.ShortenID(image.ID)))
}

// imageCarriesDigest reports whether a local image carries the given manifest
// digest.
//
// Checks the repo digests, which is where the manifest digest a pull resolved
// is recorded. The image ID is a DIFFERENT hash -- of the config, not the
// manifest -- and comparing against it would never match.
func imageCarriesDigest(image domain.Image, digest string) bool {
	if digest == "" {
		return false
	}
	for _, repoDigest := range image.RepoDigests {
		// A repo digest is "repository@sha256:...". The suffix is compared
		// rather than the whole string, because the repository spelling the
		// daemon records can differ from the one requested.
		if _, found, ok := cutLast(repoDigest, "@"); ok && found == digest {
			return true
		}
	}
	return false
}

// cutLast splits s at the last occurrence of sep.
func cutLast(s, sep string) (before, after string, found bool) {
	index := -1
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			index = i
		}
	}
	if index < 0 {
		return s, "", false
	}
	return s[:index], s[index+len(sep):], true
}

// progressSink builds the bounded progress callback.
//
// The adapter already rate-limits emission; this bounds the WRITES on top, and
// the repository caps the stored event count on top of that. Three independent
// bounds, because progress is the one thing here whose volume a third party
// influences.
func (s *AcquisitionService) progressSink(ctx context.Context, id string) func(docker.PullProgress) {
	var (
		mu      sync.Mutex
		written int
	)

	return func(progress docker.PullProgress) {
		mu.Lock()
		if written >= s.cfg.MaxEventsPerAcquisition {
			mu.Unlock()
			return
		}
		written++
		mu.Unlock()

		// Deliberately NOT the pull's context: a cancelled pull must still be
		// able to record why it stopped. Bounded so a wedged write cannot hold
		// the transfer open.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), progressWriteTimeout)
		defer cancel()

		if err := s.store.RecordProgress(writeCtx, id, progress.Status,
			progress.Current, progress.Layers, s.now().UTC(),
			s.cfg.MaxEventsPerAcquisition); err != nil {
			// Progress is telemetry. Losing a line is not a reason to abandon a
			// transfer that is working, so it is logged at debug and dropped.
			s.logger.DebugContext(ctx, "could not record acquisition progress",
				slog.String("acquisitionId", id), slog.String("error", err.Error()))
		}
	}
}

// progressWriteTimeout bounds one progress write.
const progressWriteTimeout = 5 * time.Second

// fail records a terminal failure.
func (s *AcquisitionService) fail(
	ctx context.Context,
	id string,
	failure domain.AcquisitionFailure,
	refusal domain.AcquisitionRefusal,
	message string,
) {
	// Detached and bounded: the failure that brought us here is frequently a
	// cancelled or expired context, and the record must still be written.
	writeCtx, cancel := GraceContext(ctx, acquisitionWriteGrace, acquisitionWriteGrace)
	defer cancel()

	if _, err := s.store.Advance(writeCtx, store.StateChange{
		AcquisitionID: id,
		To:            domain.AcquisitionFailed,
		Failure:       failure,
		Refusal:       refusal,
		Message:       message,
		Detail:        message,
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record acquisition failure",
			slog.String("acquisitionId", id), slog.String("error", err.Error()))
	}
}

// refuse records a preflight refusal.
//
// A refusal is the safety model working, so it is recorded with the SPECIFIC
// check that said no rather than as a generic failure -- an operator needs to
// know whether the world moved or whether they asked for something impossible.
func (s *AcquisitionService) refuse(ctx context.Context, id string, refusal domain.AcquisitionRefusal) {
	s.fail(ctx, id, domain.AcquisitionFailurePreflight, refusal, refusal.Explain())

	s.logger.InfoContext(ctx, "image acquisition refused by preflight",
		slog.String("acquisitionId", id),
		slog.String("refusal", string(refusal)))
}

// acquisitionWriteGrace bounds a detached terminal write.
const acquisitionWriteGrace = 10 * time.Second

// classifyAcquisitionFailure maps a pull error onto a classification and a
// HarborMaster-authored message.
//
// The error is never rendered. A daemon error can embed socket paths and
// registry URLs, and the message produced here reaches an API response and a
// browser -- so the classification decides the wording, and the wording comes
// from a fixed vocabulary.
func classifyAcquisitionFailure(
	parent, pullCtx context.Context,
	err error,
) (domain.AcquisitionFailure, string) {
	// Cancellation is an operator decision, not a fault. The parent being done
	// means shutdown, which the recovery pass handles.
	if errors.Is(err, context.Canceled) && parent.Err() == nil && pullCtx.Err() != nil {
		return domain.AcquisitionFailureNone, ""
	}
	if errors.Is(err, context.Canceled) {
		return domain.AcquisitionFailureNone, ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.AcquisitionFailureTimeout, domain.AcquisitionFailureTimeout.Explain()
	}

	switch {
	case errors.Is(err, docker.ErrPullRefused):
		return domain.AcquisitionFailurePreflight, domain.AcquisitionRefusalTargetRefused.Explain()
	case errors.Is(err, docker.ErrUnreachable):
		return domain.AcquisitionFailureDockerUnavailable, domain.AcquisitionFailureDockerUnavailable.Explain()
	case errors.Is(err, docker.ErrPullFailed):
		// The adapter has already classified registry refusals into this
		// sentinel with a HarborMaster-authored suffix; the distinction between
		// "the registry said no" and "the transfer broke" is preserved by the
		// message the adapter chose, and both are recorded as registry-side.
		return domain.AcquisitionFailureRegistry, domain.AcquisitionFailureRegistry.Explain()
	}
	return domain.AcquisitionFailureTransfer, domain.AcquisitionFailureTransfer.Explain()
}

// signal asks the worker to look for queued work.
func (s *AcquisitionService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// -------------------------------------------------------------- reading --

// Get returns one acquisition with its audit trail.
func (s *AcquisitionService) Get(
	ctx context.Context,
	acquisitionID string,
) (domain.Acquisition, []domain.AcquisitionEvent, error) {
	acquisition, err := s.store.Get(ctx, acquisitionID)
	if err != nil {
		return domain.Acquisition{}, nil, err
	}
	events, err := s.store.Events(ctx, acquisitionID, s.cfg.MaxEventsPerAcquisition)
	if err != nil {
		return acquisition, nil, err
	}
	return acquisition, events, nil
}

// List returns a page of acquisitions.
func (s *AcquisitionService) List(
	ctx context.Context,
	filter store.AcquisitionFilter,
) ([]domain.Acquisition, int, error) {
	return s.store.List(ctx, filter)
}

// Summary returns the dashboard aggregate, with whether the feature is on.
func (s *AcquisitionService) Summary(ctx context.Context) (domain.AcquisitionSummary, error) {
	summary, err := s.store.Summary(ctx)
	if err != nil {
		return summary, err
	}
	// Reported so an empty list is not read as "nothing has ever been
	// acquired" in a deployment where acquiring is switched off.
	summary.Enabled = s.Enabled()
	return summary, nil
}

// Eligible reports whether a plan could be acquired right now, without
// recording anything.
//
// Used by the UI to decide whether to offer the action. Runs the SAME preflight
// as the request path, so the button and the outcome cannot disagree -- and
// because it records nothing, being wrong costs an operator a refusal rather
// than a failed job.
func (s *AcquisitionService) Eligible(
	ctx context.Context,
	planID string,
) (domain.AcquisitionTarget, domain.AcquisitionRefusal, error) {
	if !s.Enabled() {
		return domain.AcquisitionTarget{}, domain.AcquisitionRefusalDisabled, nil
	}

	decision, err := s.preflight(ctx, planID, "")
	if err != nil {
		return domain.AcquisitionTarget{}, domain.AcquisitionRefusalNone, err
	}
	return decision.Target, decision.Refusal, nil
}

// planEvidence adapts the repositories to AcquisitionEvidence.
//
// A small adapter rather than passing four repositories into the service, so
// the service's dependency is one narrow READ-ONLY interface and a test can
// substitute it wholesale.
type planEvidence struct {
	plans      *store.PlanRepository
	intel      *store.ImageIntelRepository
	containers *store.ContainerRepository
}

// NewPlanEvidence builds the evidence source from the repositories.
func NewPlanEvidence(
	plans *store.PlanRepository,
	intel *store.ImageIntelRepository,
	containers *store.ContainerRepository,
) AcquisitionEvidence {
	return &planEvidence{plans: plans, intel: intel, containers: containers}
}

func (e *planEvidence) Plan(ctx context.Context, planID string) (domain.ChangePlan, error) {
	return e.plans.Get(ctx, planID)
}

func (e *planEvidence) CurrentPlan(ctx context.Context, containerID string) (domain.ChangePlan, error) {
	return e.plans.Current(ctx, containerID)
}

func (e *planEvidence) Intel(ctx context.Context, reference string) (domain.ImageIntel, error) {
	return e.intel.Get(ctx, reference)
}

func (e *planEvidence) ContainerPresent(ctx context.Context, containerID string) (bool, error) {
	detail, err := e.containers.Get(ctx, containerID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up container: %w", err)
	}
	// The ROW existing is not the container existing.
	//
	// A container removed from the host keeps its inventory row until retention
	// purges it -- that row is the history an operator reads afterwards, and
	// ContainerRepository.Get deliberately returns it so a detail page still
	// works. It carries present = 0.
	//
	// This helper answers "is the container on the host", so it has to read
	// that flag. Without it the name promised a check it never performed, and a
	// departed container passed the presence gate on the strength of its own
	// tombstone.
	return detail != nil && detail.Overview.Present, nil
}

// ------------------------------------------------------- audit attribution --

// reportOutcome records what a finished download did to the host.
//
// The image store is a smaller blast radius than a running container, but the
// event still belongs in the security log: an image on the host is content
// somebody caused to arrive, and "who downloaded this" is a question an
// administrator asks after finding one.
//
// Bounded, detached, and unable to fail the acquisition -- the same reasoning
// as ExecutionService.reportOutcome, which documents it at length.
func (s *AcquisitionService) reportOutcome(ctx context.Context, requested domain.Acquisition) {
	// Both consumers or neither: the read-back is the expensive part, and a
	// deployment with an audit recorder and no notifier -- or the reverse -- is
	// perfectly normal.
	if s.audit == nil && s.notifier == nil {
		return
	}

	writeCtx, cancel := GraceContext(ctx, acquisitionWriteGrace, acquisitionWriteGrace)
	defer cancel()

	final, err := s.store.Get(writeCtx, requested.AcquisitionID)
	if err != nil {
		s.audit.RecordAction(writeCtx, requesterActor(requested.RequestedBy),
			domain.AuditAcquisitionFailed, domain.AuditFailed,
			domain.AuditTargetAcquisition, requested.AcquisitionID, requested.ContainerName,
			"the download finished but its record could not be read back")
		return
	}
	if !final.State.Terminal() {
		// Abandoned by a shutdown. The restart recovery pass settles it.
		return
	}

	action := domain.AuditAcquisitionFailed
	outcome := domain.AuditFailed
	if final.State == domain.AcquisitionSucceeded {
		action = domain.AuditAcquisitionCompleted
		outcome = domain.AuditSucceeded
	}

	s.audit.RecordAction(writeCtx, requesterActor(final.RequestedBy),
		action, outcome, domain.AuditTargetAcquisition,
		final.AcquisitionID, final.ContainerName, acquisitionOutcomeReason(final))

	// The notification leaves from the same read-back, for the same reason: one
	// place that observes the settled record beats a call on each of the five
	// terminal paths, where the sixth would eventually be forgotten.
	//
	// Cancelled and expired raise nothing. An operator who cancelled a download
	// does not need to be told they cancelled it.
	switch final.State {
	case domain.AcquisitionSucceeded:
		NotifyAcquisitionSucceeded(s.notifier, final.ContainerName,
			final.Target.Reference, final.AcquisitionID)
	case domain.AcquisitionFailed:
		NotifyAcquisitionFailed(s.notifier, final.ContainerName,
			final.Target.Reference, final.AcquisitionID, acquisitionOutcomeReason(final))
	}
}

// acquisitionOutcomeReason renders the conclusion in HarborMaster's own words.
//
// Never the failure message, which can carry a registry's text. The digest is
// included on success because it is the one fact that identifies WHAT arrived,
// and it is a hex digest HarborMaster computed rather than a value it was told.
func acquisitionOutcomeReason(final domain.Acquisition) string {
	switch final.State {
	case domain.AcquisitionSucceeded:
		return "the image is now in this host's local store at " + final.AcquiredDigest
	case domain.AcquisitionCancelled:
		return "cancelled; the transfer was stopped and nothing is reported as acquired"
	case domain.AcquisitionExpired:
		return "expired in the queue; nothing was downloaded"
	default:
		return "the download did not complete (" + string(final.Failure) + ")"
	}
}
