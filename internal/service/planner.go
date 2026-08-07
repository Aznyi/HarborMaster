package service

import (
	"context"
	"errors"
	"log/slog"
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

// PlannerOptions configures a PlannerService.
type PlannerOptions struct {
	Store PlanStore

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
	store PlanStore

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

	// The clock is read ONCE for the whole pass and threaded through every
	// assessment. Reading it per container would make two containers assessed a
	// millisecond apart use different "now" values, which for the image-age
	// rule means two identical estates could produce different plans.
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
	normalized := make(map[string]domain.NormalizedRef, len(candidates))

	for _, candidate := range candidates {
		containerIDs = append(containerIDs, candidate.ContainerID)
		if candidate.ImageRef == "" {
			continue
		}
		reference, err := domain.NormalizeImageRef(candidate.ImageRef)
		if err != nil {
			// A reference that cannot be normalised has no image intelligence
			// to look up. It is still planned, with the registry evidence
			// absent -- which the model reports as unknown rather than fine.
			continue
		}
		normalized[candidate.ContainerID] = reference
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
		plan, state := s.planOne(candidate, normalized[candidate.ContainerID], inputs, evaluatedAt)
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
	reference domain.NormalizedRef,
	batch store.PlanBatchInputs,
	evaluatedAt time.Time,
) (domain.ChangePlan, planState) {
	intel, hasIntel := batch.Intel[reference.Canonical]

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
	if !hasIntel {
		return domain.ChangePlan{}, planSkipped
	}

	baseline := batch.Baselines[candidate.ContainerID]
	drift := batch.Drift[candidate.ContainerID]
	policy := batch.Policy[candidate.ContainerID]

	// The reference and digest to move onto, resolved together. An invalid
	// target means nothing is proposed, and the plan says so rather than
	// pairing a reference with a digest that was resolved for another one.
	target := proposedChange(intel)
	proposedImage, proposedDigest := target.Reference(), target.Digest()

	inputs := domain.PlanInputs{
		ContainerID:   candidate.ContainerID,
		ContainerName: candidate.ContainerName,

		CurrentImage:        intel.Familiar,
		ProposedImage:       proposedImage,
		CurrentDigest:       intel.LocalDigest,
		CurrentDigestDetail: intel.LocalDigestDetail,
		ProposedDigest:      proposedDigest,
		CurrentTag:          reference.Tag,
		UpdateType:          intel.Update,

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
