package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The change planner.
//
// # What it does, in one sentence
//
// For each container it assembles what HarborMaster already knows -- the image
// it runs, the update its registry offers, its baseline snapshot and that
// snapshot's restore readiness, its open drift, and its policy compliance --
// and runs the deterministic risk model over the result.
//
// # What it deliberately does NOT do
//
// It does not touch Docker. It does not pull, recreate, restore, or roll back,
// and it does not schedule anything to. There is no call into docker.Runtime
// anywhere below and no queue behind it: the planner reads six tables and writes
// one. A plan is analysis that an operator acts on with their own tooling.
//
// It does not read live Docker even indirectly. Every input comes from the
// inventory HarborMaster has already persisted, which is what makes a plan
// reproducible from the database alone.
//
// # The properties that matter
//
// DETERMINISM. The same inputs produce the same plan, byte for byte. The risk
// model is pure, the rules are a fixed slice, and the only clock reaches it as
// an explicit field. That is what lets a stored plan be re-derived and checked.
//
// NO N+1. Containers are processed in BATCHES, and each batch costs five
// grouped queries whatever its size. A ten-thousand-container estate is a
// hundred queries, not sixty thousand.
//
// UNCHANGED WORK IS NOT REDONE. Every assessment is fingerprinted over exactly
// the values it read. A pass over an unchanged estate writes nothing at all.

// ErrPlannerDisabled reports that change planning is switched off.
var ErrPlannerDisabled = errors.New("change planning is disabled")

// PlanStore is the persistence capability the planner needs.
//
// A narrow interface rather than *store.PlanRepository, so the planner is
// testable without a database and so the surface it depends on is visible in
// one place. Note what is ABSENT: nothing that updates a plan. Plans are
// immutable, and the interface makes that structural rather than conventional.
type PlanStore interface {
	Candidates(ctx context.Context, offset, limit int) ([]store.PlanCandidate, error)
	CountCandidates(ctx context.Context) (int, error)
	GatherInputs(ctx context.Context, containerIDs, imageRefs []string) (store.PlanBatchInputs, error)
	InsertPlans(ctx context.Context, plans []domain.ChangePlan, now time.Time) (store.InsertResult, error)
	PruneSuperseded(ctx context.Context, cutoff time.Time, batch int) (int64, error)
	PruneOrphans(ctx context.Context, batch int) (int64, error)
}

// PlanPreparer establishes evidence a pass will read, immediately before it
// reads it.
//
// One method, returning nothing the planner acts on. This is deliberately not a
// general extension point: the only thing a preparer may do is make the world
// more completely described before it is assessed, and the planner neither
// inspects nor branches on the result.
//
// It exists because a change plan's snapshot evidence is IMMUTABLE, so the only
// moment a baseline can enter a plan is before the plan is written. See
// service.SnapshotPreparer.
//
// Implementations MUST be bounded: this runs inside the pass budget, on the
// pass goroutine, and a preparer that took the whole budget would starve the
// planning it exists to serve.
type PlanPreparer interface {
	PrepareForPlanning(ctx context.Context) PrepareReport
}

// PlannerOptions configures a PlannerService.
type PlannerOptions struct {
	Store PlanStore

	// Lineage supplies what each container FOLLOWS. Nil restores the
	// pre-Phase-13.1 behaviour, in which a container running an immutable
	// digest has nothing to assess and is skipped forever.
	Lineage LineageReader

	// Prepare establishes snapshot baselines before the pass assesses the
	// estate. Nil skips preparation entirely, which is exactly the behaviour
	// every build had before Phase 17.
	Prepare PlanPreparer

	Config config.Planner
	// Notify raises operator notifications. Nil sends none, which is the default:
	// notifications are off unless a deployment asks for them, and every service
	// must behave identically without one.
	Notify Notifier

	Logger *slog.Logger
	Now    func() time.Time
}

// PlannerService generates change plans.
type PlannerService struct {
	store   PlanStore
	lineage LineageReader
	prepare PlanPreparer

	cfg      config.Planner
	notifier Notifier
	logger   *slog.Logger
	now      func() time.Time

	// wake carries a one-slot signal for an out-of-band generation request.
	// Capacity 1 with a non-blocking send: a second request while one is unread
	// is redundant, and dropping it is what stops a producer ever blocking.
	wake chan struct{}

	// generating guards the pass so two cannot overlap. Two concurrent passes
	// would read the same containers, compute the same fingerprints, and race
	// to insert rows the unique index would then reject -- work for no result.
	generating sync.Mutex

	// state guards the status fields, which HTTP handlers read while the worker
	// writes them.
	state  sync.RWMutex
	status plannerState
}

type plannerState struct {
	running   bool
	pending   bool
	lastRunAt *time.Time
	generated int
	unchanged int
	skipped   int
}

// NewPlannerService builds a PlannerService.
func NewPlannerService(opts PlannerOptions) *PlannerService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.BatchSize < 1 {
		cfg.BatchSize = config.DefaultPlannerBatchSize
	}
	if cfg.MaxContainers < 1 {
		cfg.MaxContainers = config.DefaultPlannerMaxContainers
	}
	if cfg.GenerationTimeout <= 0 {
		cfg.GenerationTimeout = config.DefaultPlannerGenerationTimeout
	}

	return &PlannerService{
		store:    opts.Store,
		lineage:  opts.Lineage,
		prepare:  opts.Prepare,
		cfg:      cfg,
		notifier: opts.Notify,
		logger:   logger,
		now:      now,
		wake:     make(chan struct{}, 1),
	}
}

// Enabled reports whether change planning is switched on.
func (s *PlannerService) Enabled() bool { return s.cfg.Enabled }

// ready reports whether the planner is configured well enough to run.
func (s *PlannerService) ready() bool { return s.cfg.Enabled && s.store != nil }

// GenerateResult reports what one pass did.
type GenerateResult struct {
	// Generated counts plans written.
	Generated int
	// Unchanged counts containers whose assessment had not moved, so nothing
	// was written. On a settled estate this is nearly everything.
	Unchanged int
	// Skipped counts containers with no change to assess.
	Skipped int
}

// Generate performs one bounded generation pass over the estate.
//
// # How the work is bounded
//
//   - Containers are read in PAGES of BatchSize, so peak memory is one batch
//     rather than the estate.
//   - MaxContainers caps the whole pass, so a pathologically large inventory
//     produces a bounded amount of work rather than an unbounded one.
//   - Each batch costs five grouped queries, whatever its size.
//
// Two passes never overlap: the second is refused rather than queued, because
// duplicating the work would produce identical fingerprints and write nothing.
func (s *PlannerService) Generate(ctx context.Context) (GenerateResult, error) {
	var result GenerateResult
	if !s.ready() {
		return result, ErrPlannerDisabled
	}

	if !s.generating.TryLock() {
		return result, nil
	}
	defer s.generating.Unlock()

	s.setRunning(true)
	defer s.setRunning(false)

	// BASELINES FIRST, and inside the pass rather than beside it.
	//
	// A plan records the snapshot evidence it was assessed against, immutably.
	// So a container that gains a baseline after its plan is written has a plan
	// that does not know about it -- and no later capture may be read into that
	// plan, because then the assessment an operator reviewed would not be the
	// assessment that authorised the change.
	//
	// Running preparation here, immediately before Candidates and GatherInputs,
	// is what closes that gap: a baseline captured on this line is read by the
	// grouped baseline query a few lines below, so the plan CONTAINS it.
	//
	// Bounded by the preparer itself and by this pass's context. A preparer that
	// fails prepares nothing and the pass continues: a container without a
	// baseline is planned as having none, which the acquisition preflight then
	// refuses exactly as it always has.
	if s.prepare != nil {
		s.prepare.PrepareForPlanning(ctx)
	}

	// The clock is read ONCE for the whole pass and threaded through every
	// assessment. Reading it per container would make two containers assessed a
	// millisecond apart use different "now" values, which for the image-age
	// rule means two identical estates could produce different plans.
	//
	// Read AFTER preparation, so a plan's evaluation time is not older than the
	// baseline it was assessed against.
	evaluatedAt := s.now().UTC()

	for offset := 0; offset < s.cfg.MaxContainers; offset += s.cfg.BatchSize {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		limit := s.cfg.BatchSize
		if remaining := s.cfg.MaxContainers - offset; remaining < limit {
			limit = remaining
		}

		candidates, err := s.store.Candidates(ctx, offset, limit)
		if err != nil {
			return result, err
		}
		if len(candidates) == 0 {
			return result, nil
		}

		batch, err := s.planBatch(ctx, candidates, evaluatedAt)
		if err != nil {
			return result, err
		}
		result.Skipped += batch.skipped

		if len(batch.plans) > 0 {
			written, err := s.store.InsertPlans(ctx, batch.plans, s.now().UTC())
			if err != nil {
				return result, err
			}
			result.Generated += written.Inserted
			result.Unchanged += written.Unchanged
		}
		result.Unchanged += batch.unchanged

		if len(candidates) < limit {
			return result, nil
		}
	}
	return result, nil
}

// batchResult is what planning one batch produced.
type batchResult struct {
	plans []domain.ChangePlan
	// unchanged counts containers whose fingerprint matched the stored one, so
	// no plan was built at all.
	unchanged int
	// skipped counts containers with nothing to assess.
	skipped int
}

// planBatch assesses one batch of containers.
func (s *PlannerService) planBatch(
	ctx context.Context,
	candidates []store.PlanCandidate,
	evaluatedAt time.Time,
) (batchResult, error) {
	var result batchResult

	containerIDs := make([]string, 0, len(candidates))
	// A set, because a hundred containers on one image must produce ONE
	// reference in the query rather than a hundred duplicates.
	referenceSet := make(map[string]struct{}, len(candidates))
	declared := make(map[string]declaredImage, len(candidates))

	// Lineage, by container name.
	//
	// Read once for the batch. A container HarborMaster has updated declares an
	// immutable digest, so the reference it DECLARES answers the wrong
	// question; what has to be looked up is the tag it FOLLOWS.
	lineages := s.lineageFor(ctx, candidates)

	for _, candidate := range candidates {
		containerIDs = append(containerIDs, candidate.ContainerID)

		// The tracking reference goes into the lookup set whether or not the
		// declared reference does, because it is the one whose registry answer
		// this container is assessed against.
		if lineage, tracked := lineages[candidate.ContainerName]; tracked && lineage.Tracked() {
			referenceSet[lineage.TrackingReference] = struct{}{}
		}

		// A container that declares no image at all. There is no reference to
		// look up and no record to read back, so there is nothing to say about
		// it beyond its presence, which the inventory already reports.
		if candidate.ImageRef == "" {
			continue
		}
		reference, err := domain.NormalizeImageRef(candidate.ImageRef)
		if err != nil {
			// A reference that cannot be normalised has no image intelligence
			// to look up. It is still planned, with the registry evidence
			// absent -- which the model reports as unknown rather than fine.
			//
			// That evidence EXISTS: image intelligence tracks a refused
			// reference under its raw form with status unsupported, precisely
			// so the gap is visible. Reading it back is what this branch does;
			// omitting the key was the whole defect, and it made every such
			// container silently absent from planning.
			//
			// No NormalizedRef is recorded, deliberately. Nothing downstream
			// may derive a host, a repository, a tag or a target from a string
			// this package refused to parse, so the entry carries a lookup key
			// and nothing else.
			key := domain.UnsupportedReferenceKey(candidate.ImageRef)
			if key == "" {
				continue
			}
			declared[candidate.ContainerID] = declaredImage{IntelKey: key}
			referenceSet[key] = struct{}{}
			continue
		}
		declared[candidate.ContainerID] = declaredImage{
			Ref:      reference,
			IntelKey: reference.Canonical,
		}
		referenceSet[reference.Canonical] = struct{}{}
	}

	references := make([]string, 0, len(referenceSet))
	for reference := range referenceSet {
		references = append(references, reference)
	}

	inputs, err := s.store.GatherInputs(ctx, containerIDs, references)
	if err != nil {
		return result, err
	}

	result.plans = make([]domain.ChangePlan, 0, len(candidates))
	for _, candidate := range candidates {
		plan, state := s.planOne(candidate, declared[candidate.ContainerID],
			lineages[candidate.ContainerName], inputs, evaluatedAt)
		switch state {
		case planNew:
			result.plans = append(result.plans, plan)
			// Raised here rather than from automation, so a deployment that
			// wants to be TOLD about updates without letting anything act on
			// them gets told. Automation is off by default; discovery is not.
			//
			// planNew is the whole suppression: a plan whose fingerprint matches
			// the stored one is planUnchanged and never reaches this branch, so
			// a planner running every hour says a thing once rather than
			// twenty-four times.
			if plan.ProposedImage != "" && plan.ProposedImage != plan.CurrentImage {
				NotifyUpdateDiscovered(s.notifier, plan.ContainerName,
					plan.CurrentImage, plan.ProposedImage, plan.UpdateType)
			}
		case planUnchanged:
			result.unchanged++
		case planSkipped:
			result.skipped++
		}
	}
	return result, nil
}

// declaredImage is the DECLARED reference of one candidate, resolved as far as
// it can be resolved.
//
// Two fields rather than one NormalizedRef, because a reference that cannot be
// normalised still has a record to read and must still be assessable. Keeping
// the lookup key separate is what lets the unsupported case carry a key WITHOUT
// carrying a parsed reference: there is nowhere here for a host, a repository,
// a tag or a digest that NormalizeImageRef refused to produce.
type declaredImage struct {
	// Ref is the normalised reference, and is the ZERO VALUE when the declared
	// reference could not be normalised. Never partially filled.
	Ref domain.NormalizedRef

	// IntelKey is the key the image intelligence record is stored under: the
	// canonical reference for a supported one, the bounded raw reference for an
	// unsupported one, and "" for a candidate that declares no image at all.
	//
	// "" is not a lookup. An empty key is checked for explicitly before the map
	// is read, so several containers with no reference cannot share one entry
	// and cannot read another container's evidence.
	//
	// The two families cannot collide: a canonical reference always normalises
	// (it is the fixed point of NormalizeImageRef), so no string that FAILED to
	// normalise can equal one.
	IntelKey string
}

// planState reports what happened to one container.
type planState int

const (
	planNew planState = iota
	planUnchanged
	planSkipped
)

// planOne assesses a single container.
//
// Returns planSkipped when there is nothing to assess, and planUnchanged when
// the assessment's fingerprint matches the stored one.
func (s *PlannerService) planOne(
	candidate store.PlanCandidate,
	declared declaredImage,
	lineage domain.ImageLineage,
	batch store.PlanBatchInputs,
	evaluatedAt time.Time,
) (domain.ChangePlan, planState) {
	// The normalised reference, which is the ZERO VALUE when the declared one
	// could not be normalised. Every read of it below is a read of an empty
	// field in that case, which is the honest answer rather than a guess.
	reference := declared.Ref

	// THE LINEAGE PATH.
	//
	// A container HarborMaster has updated runs `repo@sha256:...`. Asking
	// whether that reference has an update is asking whether a digest can move,
	// and the honest answer -- no -- is what used to remove every updated
	// container from automation for good.
	//
	// When lineage says what the container FOLLOWS, the question becomes the
	// right one: does the tag resolve to something other than what is running?
	// Everything downstream of this branch is unchanged, so a lineage plan goes
	// through the same risk model, the same policy gates, and the same
	// acquisition and execution verification as any other.
	if lineage.Tracked() {
		if plan, state, handled := s.planTracked(candidate, reference, lineage, batch, evaluatedAt); handled {
			return plan, state
		}
	}

	// The record for the reference the container DECLARES, read under the key
	// that record is stored beneath. An empty key names no record and is never
	// looked up: reading batch.Intel[""] would let every container with no
	// reference share one entry.
	var (
		intel    domain.ImageIntel
		hasIntel bool
	)
	if declared.IntelKey != "" {
		intel, hasIntel = batch.Intel[declared.IntelKey]
	}

	// A plan describes a PROPOSED CHANGE. A container whose image has no update
	// on offer has nothing to plan, and generating a row saying "no change
	// proposed" for every settled container would fill the table with
	// non-events.
	//
	// The exception is a container whose registry evidence is missing or
	// stale: that is not "no update", it is "we do not know", and an operator
	// should be able to see that a container is unassessable rather than have
	// it silently absent.
	if hasIntel && intel.Update == domain.UpdateNone && intel.Status == domain.CheckOK {
		return domain.ChangePlan{}, planSkipped
	}
	// A container with no tracked intelligence at all has not been looked at
	// yet. Nothing to plan and nothing to say beyond that, which the image
	// intelligence dashboard already reports.
	//
	// This is NOT the unsupported case. An unsupported reference HAS a record
	// -- written by the inventory sync, status unsupported, never queued for a
	// lookup -- so it falls through to the assessment below and is reported as
	// unassessable rather than omitted.
	if !hasIntel {
		return domain.ChangePlan{}, planSkipped
	}

	// The reference and digest to move onto, resolved together. An invalid
	// target means nothing is proposed, and the plan says so rather than
	// pairing a reference with a digest that was resolved for another one.
	target := proposedChange(intel)

	return s.buildPlan(candidate, imageEvidence{
		Intel:          intel,
		CurrentImage:   intel.Familiar,
		CurrentDigest:  intel.LocalDigest,
		CurrentDetail:  intel.LocalDigestDetail,
		CurrentTag:     reference.Tag,
		ProposedImage:  target.Reference(),
		ProposedDigest: target.Digest(),
		UpdateType:     observedUpdateType(intel),
	}, batch, evaluatedAt)
}

// observedUpdateType is what the registry record actually ESTABLISHED about
// this reference, as distinct from what its column happens to hold.
//
// This is the ONE place that decision is made, and every plan built from a
// declared reference goes through it.
//
// # The contract it defends
//
// domain.UpdateNone is a POSITIVE claim -- "the image is current: the tag has
// not moved and no newer comparable tag exists" -- and the attention model
// turns it into AttentionUpToDate, "HarborMaster looked and found nothing to
// do". internal/domain/attention.go states the rule it must not break:
//
//	Absent evidence produces AttentionNotChecked, never AttentionUpToDate.
//
// The attention model cannot uphold that by itself. It is a pure function of
// the evidence handed to it, so a planner that hands it a `none` which is
// really a column default makes it say "up to date" about a comparison that
// never happened -- and no amount of correctness in assessState can recover
// from an input that is already a lie. Closing that gap is this function's
// whole job, which is why the check lives here rather than being repeated as a
// status switch in the attention model.
//
// # Telling a real verdict from the column default
//
// `image_intel.update_type` is `NOT NULL DEFAULT 'none'`, and it is written by
// exactly one statement: the CheckOK arm of ImageIntelRepository.RecordCheck.
// The failure arm deliberately leaves it alone, "because blanking them would
// turn 'we could not reach the registry' into 'no update is available', which
// is a different and false claim". The inventory upsert does not touch it
// either.
//
// That same CheckOK statement is the only writer of `last_success_at`. So
// LastSuccessAt == nil means NO comparison has ever succeeded for this
// reference, and whatever its update column says is the DEFAULT rather than an
// observation. This is a marker the model already defines -- "the most recent
// [lookup] that answered" -- and already relies on: both the acquisition and
// the execution preflight refuse on `intel.LastSuccessAt == nil`. It is not a
// timestamp being read as a proxy for a semantic state.
//
// Without this clause a container reports "Up to date" whenever its FIRST
// lookup did not succeed -- pending because nothing has run yet, notFound for
// a locally built image that was never published, unauthorized for a private
// repository HarborMaster holds no credentials for by design. None of those
// compared anything.
//
// # What is deliberately preserved
//
// A reference that HAS succeeded keeps intel.Update exactly as stored, however
// the most recent lookup went. A transient failure, a rate limit, or a
// repository that has just become private must not erase a real `major`,
// `patch` or `none` that a real comparison established: that verdict remains
// the best knowledge available, which is precisely why the failure arm
// preserves it in the first place.
//
// # Why unsupported is judged separately
//
// An unsupported reference is never queued, so it will never be compared
// AGAIN. Where a failure is a gap that the next pass may close, this one never
// closes, and any verdict such a row happens to carry -- from before a
// normalisation rule tightened around its reference -- is frozen with no
// possibility of refresh or contradiction. Reported as unknown whatever its
// history, which is also what Batch B1 established and pinned.
//
// # Nothing becomes actionable
//
// UpdateUnknown is the model's own word for this state: "an update may exist
// but its size could not be determined". It is non-actionable in exactly the
// places UpdateNone is -- UpdateType.Available() is false for both, and
// UpdateStrategy.Permits refuses both -- so moving between them changes what an
// operator is TOLD and never what may be DONE.
func observedUpdateType(intel domain.ImageIntel) domain.UpdateType {
	// Never comparable again, so a stored verdict can never be refreshed.
	if intel.Status == domain.CheckUnsupported {
		return domain.UpdateUnknown
	}
	// Never compared at all, so a stored verdict is the column default.
	if intel.LastSuccessAt == nil {
		return domain.UpdateUnknown
	}
	return intel.Update
}

// lineageFor reads the lineage of every container in the batch, by name.
//
// One read for the batch rather than one per container. Never fails the pass:
// without lineage the planner behaves exactly as it did before Phase 13.1 --
// tag-referenced containers are still planned, and digest-pinned ones are still
// skipped -- so an unreadable lineage table degrades to the old behaviour rather
// than stopping planning for the estate.
func (s *PlannerService) lineageFor(
	ctx context.Context,
	candidates []store.PlanCandidate,
) map[string]domain.ImageLineage {
	if s.lineage == nil || len(candidates) == 0 {
		return nil
	}
	all, err := s.lineage.All(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "image lineage could not be read; containers already moved onto "+
			"a digest will not be assessed this pass",
			slog.String("error", err.Error()))
		return nil
	}

	wanted := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		wanted[candidate.ContainerName] = struct{}{}
	}
	lineages := make(map[string]domain.ImageLineage, len(candidates))
	for _, lineage := range all {
		if _, ok := wanted[lineage.ContainerName]; ok {
			lineages[lineage.ContainerName] = lineage
		}
	}
	return lineages
}

// planTracked assesses a container against the reference it FOLLOWS.
//
// Returns handled=false when lineage cannot carry the assessment, which sends
// the container back to the declared-reference path -- the pre-Phase-13.1
// behaviour, and the right fallback for a container whose lineage exists but
// whose registry evidence does not.
//
// # The comparison
//
//	tracking reference -> registry digest   VS   digest actually running
//
// The digest actually running is taken from LINEAGE, not from the inventory.
// The two disagreeing means somebody changed the host outside HarborMaster, and
// this path refuses rather than reconciles: proposing a change from a state
// HarborMaster did not establish is how an automated updater acts on a
// misunderstanding.
func (s *PlannerService) planTracked(
	candidate store.PlanCandidate,
	reference domain.NormalizedRef,
	lineage domain.ImageLineage,
	batch store.PlanBatchInputs,
	evaluatedAt time.Time,
) (domain.ChangePlan, planState, bool) {
	intel, hasIntel := batch.Intel[lineage.TrackingReference]
	if !hasIntel {
		// The tracking reference has not been checked yet. Nothing to say --
		// and importantly NOT "no update", which is what the old behaviour
		// amounted to.
		return domain.ChangePlan{}, planSkipped, true
	}

	// What the container is OBSERVED to run, compared against what lineage says
	// HarborMaster approved.
	//
	// Resolved through the one shared definition: the declared reference when it
	// is pinned, otherwise the local image's RepoDigests matched to this exact
	// repository. Deriving it from the reference alone left this empty for every
	// tag-created container, which silently disabled the external-change guard
	// below on precisely the containers it was written to protect.
	observed, _ := domain.RunningDigestFor(reference, candidate.RepoDigests)
	if observed != "" && lineage.RunningDigest != "" &&
		!strings.EqualFold(observed, lineage.RunningDigest) {
		// EXTERNAL CHANGE. Somebody moved this container while HarborMaster was
		// not looking. Refused rather than reconciled here: the planner's job is
		// to assess, and it cannot assess a container whose starting point it
		// does not know. Reconciliation owns this case and will re-establish
		// lineage from what is actually running.
		s.logger.Info("a managed container is running a digest HarborMaster did not put there; "+
			"it is not being planned until its lineage is reconciled",
			slog.String("containerName", candidate.ContainerName))
		return domain.ChangePlan{}, planSkipped, true
	}

	running := lineage.RunningDigest
	if running == "" {
		running = observed
	}

	proposal := domain.EvaluateLineage(lineage, intel, running)
	if !proposal.Usable {
		// Not established. Fall through to the declared-reference path, which
		// reports an unassessable container honestly rather than as settled.
		return domain.ChangePlan{}, planSkipped, false
	}
	if proposal.Update == domain.UpdateNone {
		// Settled. Nothing to propose, and a row saying so for every current
		// container is the noise the planner already declines to write.
		return domain.ChangePlan{}, planSkipped, true
	}

	plan, state := s.buildPlan(candidate, imageEvidence{
		Intel: intel,
		// The container is running an artefact, and what it is running is the
		// digest -- so that is what the plan reports as current. The tracking
		// tag is carried separately in CurrentTag, so a reader can see both
		// without either being mistaken for the other.
		CurrentImage:   currentImageFor(reference, lineage),
		CurrentDigest:  running,
		CurrentDetail:  intel.LocalDigestDetail,
		CurrentTag:     lineage.TrackingReferenceTag(),
		ProposedImage:  proposal.Familiar,
		ProposedDigest: proposal.Digest,
		UpdateType:     proposal.Update,
	}, batch, evaluatedAt)
	return plan, state, true
}

// currentImageFor renders what the container is running, for a reader.
//
// The DECLARED reference when there is one -- that is literally what the
// container runs -- falling back to the tracking reference's familiar form when
// the declared one could not be parsed.
func currentImageFor(reference domain.NormalizedRef, lineage domain.ImageLineage) string {
	if reference.Familiar != "" {
		return reference.Familiar
	}
	return lineage.TrackingFamiliar
}

// imageEvidence is the image-side input to a plan.
//
// Extracted so the DECLARED-reference path and the TRACKING-reference path
// produce a plan through exactly the same code. Everything after this struct --
// the risk model, the fingerprint, the duplicate suppression -- is shared, which
// is what makes a lineage plan pass the same gates as any other rather than
// becoming a second, weaker planner.
type imageEvidence struct {
	// Intel is the registry record the plan's status fields come from. For a
	// tracked container it is the record for the TRACKING reference.
	Intel domain.ImageIntel

	CurrentImage  string
	CurrentDigest string
	CurrentDetail string
	CurrentTag    string

	ProposedImage  string
	ProposedDigest string
	UpdateType     domain.UpdateType
}

// buildPlan turns resolved image evidence into a change plan.
func (s *PlannerService) buildPlan(
	candidate store.PlanCandidate,
	evidence imageEvidence,
	batch store.PlanBatchInputs,
	evaluatedAt time.Time,
) (domain.ChangePlan, planState) {
	intel := evidence.Intel

	baseline := batch.Baselines[candidate.ContainerID]
	drift := batch.Drift[candidate.ContainerID]
	policy := batch.Policy[candidate.ContainerID]

	inputs := domain.PlanInputs{
		ContainerID:   candidate.ContainerID,
		ContainerName: candidate.ContainerName,

		CurrentImage:        evidence.CurrentImage,
		ProposedImage:       evidence.ProposedImage,
		CurrentDigest:       evidence.CurrentDigest,
		CurrentDigestDetail: evidence.CurrentDetail,
		ProposedDigest:      evidence.ProposedDigest,
		CurrentTag:          evidence.CurrentTag,
		UpdateType:          evidence.UpdateType,

		SnapshotID:       baseline.SnapshotID,
		RestoreReadiness: readinessOrUnknown(baseline),

		DriftOpen:        drift.Open,
		DriftMaxSeverity: domain.DriftSeverity(drift.MaxSeverity),

		PolicyOpen:        policy.Open,
		PolicyMaxSeverity: domain.PolicySeverity(policy.MaxSeverity),

		RegistryStatus:          intel.Status,
		RegistryDetail:          intel.StatusDetail,
		LocalPlatform:           intel.Platform,
		ProposedPlatformMissing: intel.PlatformMissing,

		ProposedPublishedAt: intel.PublishedAt,
		ContainerCount:      intel.ContainerCount,

		EvaluatedAt: evaluatedAt,
	}

	fingerprint := inputs.Fingerprint()
	// The fast path, and the reason a settled estate costs nothing to re-plan.
	// The unique index is the guarantee under concurrency; this is the check
	// that avoids the write in the first place.
	if stored, ok := batch.Fingerprints[candidate.ContainerID]; ok && stored == fingerprint {
		return domain.ChangePlan{}, planUnchanged
	}

	assessment := domain.AssessRisk(inputs)

	return domain.ChangePlan{
		PlanID:        domain.NewPlanID(),
		ContainerID:   candidate.ContainerID,
		ContainerName: candidate.ContainerName,

		CurrentImage:   inputs.CurrentImage,
		ProposedImage:  inputs.ProposedImage,
		CurrentDigest:  inputs.CurrentDigest,
		ProposedDigest: inputs.ProposedDigest,
		UpdateType:     inputs.UpdateType,

		SnapshotID:        inputs.SnapshotID,
		SnapshotAvailable: inputs.SnapshotID > 0,
		RestoreReadiness:  inputs.RestoreReadiness,

		DriftOpen:        inputs.DriftOpen,
		DriftMaxSeverity: inputs.DriftMaxSeverity,

		PolicyOpen:        inputs.PolicyOpen,
		PolicyMaxSeverity: inputs.PolicyMaxSeverity,

		RegistryStatus:      inputs.RegistryStatus,
		RegistryDetail:      inputs.RegistryDetail,
		ProposedPublishedAt: inputs.ProposedPublishedAt,

		Risk: assessment,

		PlanVersion:    domain.PlanSchemaVersion,
		PlannerVersion: domain.PlannerVersion,
		InputDigest:    fingerprint,
		GeneratedAt:    evaluatedAt,
	}, planNew
}

// proposedChange renders what the container would move to.
//
// Two shapes, because an update has two shapes: a newer TAG names a different
// reference, while a moved digest names the same reference resolving to
// different content. Reporting the second as a reference change would suggest
// editing something that does not need editing.
//
// # Each shape uses the digest resolved for ITS OWN reference
//
// This function used to return intel.RemoteDigest for both. RemoteDigest is the
// digest the registry serves for the CURRENT tag, so a newer-tag proposal was
// rendered with the old tag's digest -- and acquisition, being digest-pinned,
// would have pulled the old image and called it the new one.
//
// A newer tag is now paired with intel.LatestDigest, which the image check
// resolved from that tag's own manifest. When it is absent the tag is NOT
// proposed: an unpinnable change is not something an operator can be offered,
// and falling back to the current digest is the original defect.
//
// The returned target is a domain.ProposedTarget rather than two strings,
// because two strings are what got crossed.
// offeredTargets is every image the registry currently serves for this
// reference that a plan could legitimately name.
//
// # Why the acquisition check asks this rather than "what would I choose"
//
// The TOCTOU check has to answer "is the plan's reference-and-digest pair still
// what the registry is serving". It used to answer it by re-deriving the
// planner's CHOICE with proposedChange and demanding an exact match. That made
// the selection rule exist in two places, and Stage 17.9 paid for it: fixing
// the tracked path to follow a moved tag before any newer tag left this check
// still preferring the newer tag, so every such acquisition was refused with
// "the digest on offer has changed" -- the planner and the check disagreeing
// about a registry neither of them had misread.
//
// Selection belongs to the planner alone. This returns the candidates, and the
// check requires the plan to be one of them. That is the same guarantee --
// nothing moved underneath, and the repository cannot change -- without a
// second opinion about which candidate is best.
func offeredTargets(intel domain.ImageIntel) []domain.ProposedTarget {
	var targets []domain.ProposedTarget

	// The tracking reference at whatever it resolves to now.
	if intel.RemoteDigest != "" {
		if target, err := domain.NewProposedTarget(
			intel.Familiar, intel.RemoteDigest, intel.Familiar); err == nil {
			targets = append(targets, target)
		}
	}

	// A newer tag in the same series, when the check resolved one.
	if intel.LatestTag != "" && intel.LatestDigest != "" {
		base := intel.Familiar
		if index := lastIndexByte(base, ':'); index > lastIndexByte(base, '/') {
			base = base[:index]
		}
		reference := base + ":" + intel.LatestTag
		if target, err := domain.NewProposedTarget(
			reference, intel.LatestDigest, reference); err == nil {
			targets = append(targets, target)
		}
	}

	return targets
}

func proposedChange(intel domain.ImageIntel) domain.ProposedTarget {
	if intel.LatestTag != "" {
		if intel.LatestDigest == "" {
			// A newer tag with no digest of its own. Nothing is proposed.
			return domain.ProposedTarget{}
		}
		// The familiar form with the tag replaced, so an operator reads
		// "nginx:1.26" rather than a canonical path.
		base := intel.Familiar
		if index := lastIndexByte(base, ':'); index > lastIndexByte(base, '/') {
			base = base[:index]
		}
		reference := base + ":" + intel.LatestTag
		target, err := domain.NewProposedTarget(reference, intel.LatestDigest, reference)
		if err != nil {
			return domain.ProposedTarget{}
		}
		return target
	}

	// No newer tag. The same reference resolving to different content is a
	// real proposal -- but ONLY when the check actually established that the
	// digest moved.
	//
	// Any other verdict here means the evidence was incomplete: a tag listing
	// that exceeded its budget, a registry that did not answer, an image built
	// on this host. Proposing the current reference in those cases produced
	// plans that read "nginx:1.27.0-alpine -> nginx:1.27.0-alpine", which
	// invites an operator to act on a change that does not exist.
	if intel.Update != domain.UpdateDigest {
		return domain.ProposedTarget{}
	}

	// The digest here IS the one resolved for this reference, so the pair is
	// correct by construction.
	target, err := domain.NewProposedTarget(intel.Familiar, intel.RemoteDigest, intel.Familiar)
	if err != nil {
		return domain.ProposedTarget{}
	}
	return target
}

// lastIndexByte returns the last index of b in s, or -1.
//
// A local helper rather than strings.LastIndexByte purely to keep the tag-split
// logic above reading as one expression.
func lastIndexByte(s string, b byte) int {
	for index := len(s) - 1; index >= 0; index-- {
		if s[index] == b {
			return index
		}
	}
	return -1
}

// readinessOrUnknown resolves the readiness a baseline carries.
//
// A container with no snapshot has no readiness either. Reporting the zero
// value would be an empty string in a column with a CHECK constraint, so the
// absence is stated explicitly as unknown.
func readinessOrUnknown(baseline store.BaselineRollup) domain.ReadinessStatus {
	if baseline.SnapshotID == 0 || baseline.Readiness == "" {
		return domain.ReadinessUnknown
	}
	return baseline.Readiness
}

// setRunning records whether a pass is in flight.
func (s *PlannerService) setRunning(running bool) {
	s.state.Lock()
	defer s.state.Unlock()
	s.status.running = running
}
