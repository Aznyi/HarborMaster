package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Removing the images HarborMaster's own updates superseded.
//
// # What this service is for
//
// Watchtower-shaped convenience without Watchtower-shaped consequences. An
// operator who lets HarborMaster update containers should not have to garbage
// collect the images those updates left behind by hand -- but the images left
// behind are precisely the ones a recovery needs, so the removal has to be far
// more careful than a prune.
//
// # The shape of the safety argument
//
// Four things stand between a settled update and a removed image, and they are
// deliberately independent:
//
//  1. SELECTION. Candidates come from HarborMaster's own execution records: an
//     image is a candidate only when a successful, settled update moved a
//     workload off it. An image HarborMaster never introduced is never a
//     candidate, whatever else is true of it.
//  2. DECISION. domain.DecideImageRetention is a pure function over gathered
//     evidence with a fixed order and a closed vocabulary of reasons. It holds
//     no Docker interface, reads no configuration flag that could disable a
//     check, and returns "retain" when it cannot establish otherwise.
//  3. RE-CHECK. The evidence behind an eligible verdict is gathered AGAIN,
//     from the store and from the live host, immediately before the removal.
//     A container that appeared between the pass starting and this image's turn
//     is seen.
//  4. THE DAEMON. The removal is never forced. Docker refusing because
//     something still references the image is the last net under the three
//     above, and reaching it means one of them was wrong -- so it is recorded,
//     and never retried.
//
// # Why the pass is small and slow
//
// A cleanup pass is bounded to MaxPerPass removals and runs on a long interval.
// Neither bound makes cleanup more correct; both make a DEFECT in cleanup
// survivable. An operator with a bad build finds a handful of images missing
// and a clear audit trail, not an empty image store.

// imageCleanupWriteGrace bounds a detached audit write.
//
// Short: an audit record of a removal must survive the shutdown that
// interrupted the pass, but a removal that has already happened cannot be
// un-happened by waiting longer to write about it.
const imageCleanupWriteGrace = 5 * time.Second

// imageCleanupPassBudget bounds one whole pass.
//
// Generous relative to the work -- a pass is a handful of queries, one
// container listing, and at most MaxPerPass removals -- but present, because
// every wait in HarborMaster is bounded.
const imageCleanupPassBudget = 10 * time.Minute

// ImageRetentionStore is the evidence a cleanup pass reads.
//
// Reads only. There is no method here that changes anything, and nothing in
// this interface names an image to remove -- the candidate set is derived from
// lifecycle records, so a compromised or buggy store can cause a WRONG image to
// be considered, but the four checks above still have to agree before it goes.
type ImageRetentionStore interface {
	ImageCleanupCandidates(ctx context.Context) ([]store.ImageCleanupCandidate, error)
	ImageReferencesFor(ctx context.Context, imageIDs []string) (map[string]store.ImageReferences, error)
	PlanTargetImages(ctx context.Context) (map[string]struct{}, error)
}

// ImageCleanupOptions builds an ImageCleanupService.
type ImageCleanupOptions struct {
	// Store reads the retention evidence.
	Store ImageRetentionStore

	// Runtime is the READ-ONLY Docker view, used to re-verify against the live
	// host immediately before a removal. Seven methods, none of which mutate.
	Runtime docker.Runtime

	// Pruner is the removal capability. Nil unless the deployment opted in:
	// with a nil pruner this service still decides, still logs what it would
	// have removed, and removes nothing.
	Pruner docker.ImagePruner

	// Self reports HarborMaster's own container, so its image is never removed.
	Self SelfReporter

	// Audit records what was removed. Nil-safe.
	Audit *AuditRecorder

	Config config.ImageCleanup
	Logger *slog.Logger
	Now    func() time.Time
}

// ImageCleanupService removes images HarborMaster's own updates superseded.
type ImageCleanupService struct {
	store   ImageRetentionStore
	runtime docker.Runtime
	pruner  docker.ImagePruner
	self    SelfReporter
	audit   *AuditRecorder

	cfg    config.ImageCleanup
	logger *slog.Logger
	now    func() time.Time

	// mu guards the last-pass summary, which the status endpoint reads.
	mu   sync.Mutex
	last ImageCleanupPass
}

// ImageCleanupPass is what one pass did.
//
// Counts and a timestamp. No image identifiers: this is a status summary an
// operator reads, and the per-image account is the audit log.
type ImageCleanupPass struct {
	// RanAt is when the pass finished. Zero means no pass has run.
	RanAt time.Time
	// Considered is how many candidates the pass looked at.
	Considered int
	// Removed is how many images the daemon actually removed.
	Removed int
	// Retained is how many were kept, for any reason.
	Retained int
	// Refused is how many the daemon declined because something still used
	// them. Never zero-and-forgotten: a non-zero value means a check upstream
	// disagreed with the daemon, which is worth an operator's attention.
	Refused int
	// Failed is how many removals errored for a reason that was not a refusal.
	Failed int
}

// NewImageCleanupService builds the service.
func NewImageCleanupService(opts ImageCleanupOptions) *ImageCleanupService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &ImageCleanupService{
		store:   opts.Store,
		runtime: opts.Runtime,
		pruner:  opts.Pruner,
		self:    opts.Self,
		audit:   opts.Audit,
		cfg:     opts.Config,
		logger:  logger.With(slog.String("component", "imageCleanup")),
		now:     now,
	}
}

// policy converts configuration into the decision's vocabulary.
//
// The ONLY place configuration reaches the decision, and it carries exactly two
// numbers plus the on switch. There is no field here through which a setting
// could disable a safety check, because there is no such field on the policy.
func (s *ImageCleanupService) policy() domain.ImageRetentionPolicy {
	return domain.ImageRetentionPolicy{
		Enabled:         s.cfg.Enabled,
		MinAge:          s.cfg.MinAge,
		KeepGenerations: s.cfg.KeepGenerations,
	}
}

// LastPass returns the most recent pass summary.
func (s *ImageCleanupService) LastPass() ImageCleanupPass {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Run drives cleanup until ctx is cancelled.
//
// No startup pass. A pass immediately after a restart would run against an
// inventory nobody has refreshed yet, and cleanup gains nothing from being
// prompt -- so the first pass waits a full interval, by which time the
// inventory, the planner, and any interrupted work have all settled.
func (s *ImageCleanupService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("image cleanup disabled by configuration")
		return
	}
	if s.pruner == nil {
		// Enabled without the capability. Report it rather than pretending:
		// an operator who set the flag and sees nothing removed needs to know
		// the deployment did not grant the ability.
		s.logger.Warn("image cleanup is enabled but no removal capability was granted; " +
			"no image will be removed")
		return
	}

	s.logger.Info("image cleanup starting",
		slog.Duration("interval", s.cfg.Interval),
		slog.Duration("minAge", s.cfg.MinAge),
		slog.Int("keepGenerations", s.cfg.KeepGenerations),
		slog.Int("maxPerPass", s.cfg.MaxPerPass))

	tick := newOptionalTicker(s.cfg.Interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			s.RunPass(ctx)
		}
	}
}

// RunPass performs one cleanup pass.
//
// Exported so a test drives a pass directly rather than waiting on a ticker.
// It refuses on the same conditions the loop does, so calling it cannot bypass
// the on switch.
func (s *ImageCleanupService) RunPass(ctx context.Context) ImageCleanupPass {
	policy := s.policy()
	if !policy.Usable() {
		// Fail closed. An unusable policy is not "use the defaults", it is
		// "this configuration does not establish that removal is wanted".
		return ImageCleanupPass{RanAt: s.now()}
	}

	passCtx, cancel := context.WithTimeout(ctx, imageCleanupPassBudget)
	defer cancel()

	summary := ImageCleanupPass{RanAt: s.now()}

	candidates, err := s.store.ImageCleanupCandidates(passCtx)
	if err != nil {
		s.logger.Warn("image cleanup could not read its candidates; nothing was removed")
		s.record(summary)
		return summary
	}
	summary.Considered = len(candidates)
	if len(candidates) == 0 {
		s.record(summary)
		return summary
	}

	evidence, complete := s.gather(passCtx, candidates)

	for _, candidate := range candidates {
		if summary.Removed >= s.cfg.MaxPerPass {
			// The blast-radius bound. Whatever is left waits for the next pass,
			// which costs nothing: an image that could have gone today is no
			// harder to remove tomorrow.
			break
		}

		decision := domain.DecideImageRetention(
			s.evidenceFor(candidate, evidence, complete), policy, s.now())
		if !decision.Removable() {
			summary.Retained++
			continue
		}

		s.remove(passCtx, candidate, policy, &summary)
	}

	s.logger.Info("image cleanup pass finished",
		slog.Int("considered", summary.Considered),
		slog.Int("removed", summary.Removed),
		slog.Int("retained", summary.Retained),
		slog.Int("refused", summary.Refused),
		slog.Int("failed", summary.Failed))

	s.record(summary)
	return summary
}

// passEvidence is everything one pass gathered, keyed by image id.
type passEvidence struct {
	references   map[string]store.ImageReferences
	planTargets  map[string]struct{}
	liveByImage  map[string]int
	selfIdentity domain.SelfIdentity
}

// gather collects the evidence for a whole candidate set.
//
// Returns complete=false if ANY source could not be read. A partial gather is
// not partial information -- it is the absence of information, and the decision
// treats it as such by retaining everything.
func (s *ImageCleanupService) gather(
	ctx context.Context,
	candidates []store.ImageCleanupCandidate,
) (passEvidence, bool) {
	evidence := passEvidence{
		references:  map[string]store.ImageReferences{},
		planTargets: map[string]struct{}{},
		liveByImage: map[string]int{},
	}

	ids := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate.ImageID] {
			continue
		}
		seen[candidate.ImageID] = true
		ids = append(ids, candidate.ImageID)
	}

	references, err := s.store.ImageReferencesFor(ctx, ids)
	if err != nil {
		s.logger.Warn("image cleanup could not read image references; keeping everything")
		return evidence, false
	}
	evidence.references = references

	targets, err := s.store.PlanTargetImages(ctx)
	if err != nil {
		s.logger.Warn("image cleanup could not read plan targets; keeping everything")
		return evidence, false
	}
	evidence.planTargets = targets

	live, err := s.liveImageUse(ctx)
	if err != nil {
		s.logger.Warn("image cleanup could not read the live container list; keeping everything")
		return evidence, false
	}
	evidence.liveByImage = live

	evidence.selfIdentity = s.identity()
	return evidence, true
}

// liveImageUse counts containers on the HOST by the image they were built from.
//
// The independent check. HarborMaster's own inventory is a snapshot of the last
// refresh; this is the daemon answering now. A container created by an operator
// thirty seconds ago is invisible to the first and visible to the second.
func (s *ImageCleanupService) liveImageUse(ctx context.Context) (map[string]int, error) {
	if s.runtime == nil {
		// No read-only runtime is not "no containers". It is a check that could
		// not be performed, which establishes nothing.
		return nil, docker.ErrUnreachable
	}

	containers, err := s.runtime.ListContainers(ctx)
	if err != nil {
		return nil, err
	}

	byImage := make(map[string]int, len(containers))
	for _, container := range containers {
		if container.ImageID == "" {
			continue
		}
		byImage[container.ImageID]++
	}
	return byImage, nil
}

// identity reads HarborMaster's own identity, tolerating an unwired reporter.
//
// The zero identity matches nothing, so a missing reporter degrades to "does
// not know which image is its own" -- which is caught by the composition-root
// architecture test rather than silently excluding the wrong image here.
func (s *ImageCleanupService) identity() domain.SelfIdentity {
	if s.self == nil {
		return domain.SelfIdentity{}
	}
	return s.self.Identity()
}

// evidenceFor assembles one image's evidence for the decision.
func (s *ImageCleanupService) evidenceFor(
	candidate store.ImageCleanupCandidate,
	gathered passEvidence,
	complete bool,
) domain.ImageRetentionEvidence {
	references := gathered.references[candidate.ImageID]
	_, planTarget := gathered.planTargets[candidate.ImageID]

	planTargets := references.PlanTargets
	if planTarget {
		planTargets++
	}

	settled := candidate.SettledAt

	// An empty identity image matches nothing: an image id is never empty here,
	// but the guard is what stops a failed self-detection from matching the
	// first candidate whose id also could not be read.
	isSelf := gathered.selfIdentity.ImageID != "" &&
		gathered.selfIdentity.ImageID == candidate.ImageID

	// Present containers are the SUM of what the records say and what the host
	// says. Only the "greater than zero" case matters, and summing is the
	// conservative combination: either source alone is enough to retain.
	present := references.PresentContainers + gathered.liveByImage[candidate.ImageID]

	return domain.ImageRetentionEvidence{
		ImageID:                    candidate.ImageID,
		EvidenceComplete:           complete,
		PresentContainers:          present,
		PreservedContainers:        references.PreservedContainers,
		IsSelf:                     isSelf,
		ActiveAcquisitions:         references.ActiveAcquisitions,
		ActiveExecutions:           references.ActiveExecutions,
		ActiveRollbacks:            references.ActiveRollbacks,
		UnsettledFailures:          references.UnsettledFailures,
		OutstandingRecoveries:      references.OutstandingRecoveries,
		PlanTargets:                planTargets,
		SettledAt:                  &settled,
		NewerSupersededGenerations: candidate.NewerGenerations,
	}
}

// remove re-checks an eligible image and, if it is still eligible, removes it.
//
// # Why the evidence is gathered twice
//
// The pass-level gather can be minutes old by the time a candidate near the end
// of the list has its turn, and a container created in the meantime is exactly
// the case that matters. This re-gathers THIS image's references and re-lists
// the live containers immediately before acting, so the window between the
// check and the removal is one round trip rather than a whole pass.
//
// It is not a race-free window -- nothing short of holding the daemon still
// could be -- which is why the daemon's own refusal remains underneath it.
func (s *ImageCleanupService) remove(
	ctx context.Context,
	candidate store.ImageCleanupCandidate,
	policy domain.ImageRetentionPolicy,
	summary *ImageCleanupPass,
) {
	fresh, complete := s.gather(ctx, []store.ImageCleanupCandidate{candidate})
	decision := domain.DecideImageRetention(
		s.evidenceFor(candidate, fresh, complete), policy, s.now())
	if !decision.Removable() {
		// Something changed between the pass gathering evidence and this
		// image's turn. Keeping it is the whole point of looking again.
		summary.Retained++
		return
	}

	outcome, err := s.pruner.RemoveImage(ctx, docker.ImageRemoveRequest{
		ImageID: candidate.ImageID,
	})
	if err != nil {
		summary.Failed++
		s.logger.Warn("an image could not be removed",
			slog.String("container", candidate.ContainerName))
		s.reportRemoval(ctx, candidate, domain.AuditFailed,
			"the image could not be removed")
		return
	}

	switch outcome {
	case docker.ImageRemoved:
		summary.Removed++
		s.reportRemoval(ctx, candidate, domain.AuditSucceeded,
			"an image superseded by a settled update was removed")

	case docker.ImageAlreadyGone:
		// Idempotent. A second pass over the same candidate, or an operator who
		// removed it by hand, settles here rather than failing.
		summary.Retained++

	case docker.ImageStillInUse:
		// The daemon disagreed with every check above it. Worth saying out
		// loud: it means the evidence HarborMaster gathered was incomplete in
		// a way none of the checks noticed.
		summary.Refused++
		s.logger.Warn("the daemon refused to remove an image because something still uses it; "+
			"it was kept and will not be retried in this pass",
			slog.String("container", candidate.ContainerName))
		s.reportRemoval(ctx, candidate, domain.AuditDenied,
			"the daemon still needs the image, so it was kept")
	}
}

// reportRemoval writes the audit record for one removal attempt.
//
// # What is recorded, and what is not
//
// Recorded: the outcome, the image id, the workload name, and HarborMaster's
// own sentence about what happened. Nothing here is caller-supplied -- the
// image id came from an execution record HarborMaster wrote and the name from
// its own inventory.
//
// Not recorded: retained images. A pass that keeps ninety images and removes
// one would otherwise write ninety-one records a day, and the ninety would bury
// the one. What was kept, and why, is a question the status view answers.
func (s *ImageCleanupService) reportRemoval(
	ctx context.Context,
	candidate store.ImageCleanupCandidate,
	outcome domain.AuditOutcome,
	reason string,
) {
	if s.audit == nil {
		return
	}

	// Detached and bounded: the record of a removal must survive the shutdown
	// that interrupted the pass, and must never be able to fail the pass.
	writeCtx, cancel := GraceContext(ctx, imageCleanupWriteGrace, imageCleanupWriteGrace)
	defer cancel()

	s.audit.RecordAction(writeCtx, Actor{},
		domain.AuditImageRemoved, outcome,
		domain.AuditTargetImage,
		domain.SanitiseDisplayText(candidate.ImageID, domain.MaxAuditTargetIDBytes),
		domain.SanitiseDisplayText(candidate.ContainerName, domain.MaxAuditTargetIDBytes),
		reason)
}

// record stores the pass summary for the status view.
func (s *ImageCleanupService) record(pass ImageCleanupPass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = pass
}
