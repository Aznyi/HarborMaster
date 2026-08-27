package service

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Preparing the estate's baselines, ahead of the planner.
//
// # Why this runs before planning rather than before acquisition
//
// A change plan records the snapshot evidence it was assessed against, and that
// record is immutable. So the only moment a baseline can enter a plan is BEFORE
// the plan is written. Capturing one later produces evidence that the plan does
// not contain, which is exactly the thing snapshot assurance must not do -- see
// the invariant at the top of snapshot_assurance.go.
//
// This runs at the start of a planner pass, on the planner's own goroutine,
// inside the planner's own budget. There is NO new scheduler, no new worker, and
// no new goroutine that outlives the call: the planner already runs after every
// committed inventory refresh, on a periodic tick, and on request, and all three
// converge here first.
//
// # Why the work is bounded, and what happens past the bound
//
// A first pass over an estate of two thousand containers governed by a broad
// policy would otherwise want two thousand captures at once. Instead it takes
// maxAssurancePerPass of them and leaves the rest; the next pass takes the next
// batch, and the estate converges over a handful of passes rather than in one
// spike against a database with a single writer. Progress is LOGGED, because a
// bound that silently truncates reads as "everything is prepared".
//
// On a settled estate the whole thing costs four queries and captures nothing:
// every governed container already has a baseline, so the candidate list is
// empty before any work begins.

const (
	// maxAssurancePerPass bounds how many baselines one planner pass captures.
	//
	// Deliberately small. Capture reads a container's full detail, builds the
	// spec document, and hashes every sensitive value, so the cost is real even
	// though it never touches Docker. Twenty-five per pass converges a
	// two-thousand-container estate in eighty passes while leaving the planner's
	// own budget almost entirely to planning.
	maxAssurancePerPass = 25

	// assuranceConcurrency bounds simultaneous captures.
	//
	// Four rather than more: the expensive part of a capture parallelises (a
	// repository read, the spec build, the hashing) but the write does not --
	// SQLite has one writer -- so raising this trades contention for no
	// throughput.
	assuranceConcurrency = 4
)

// AssuranceTargets supplies the containers a pass may consider.
//
// Deliberately the SAME method the automation engine's evidence reads, so the
// preparer sees exactly the estate a pass would see -- including the eligibility
// screening and the label bound the repository applies. A separate query here
// would be a second answer to "what is on this host", and the two would drift.
//
// It is satisfied directly by *store.ContainerRepository. Nothing on it can
// write.
type AssuranceTargets interface {
	AutomationTargets(ctx context.Context) ([]store.AutomationTarget, bool, error)
}

// AssuranceBaselines reports which containers already have a snapshot.
//
// One method returning the whole estate in one query. A per-container lookup
// here would be the N+1 that the bound above exists to avoid re-introducing.
type AssuranceBaselines interface {
	BaselineIDs(ctx context.Context) (map[string]int64, error)
}

// SnapshotPreparerOptions configures a SnapshotPreparer.
//
// Note what is absent: every Docker capability, every request type, and every
// plan writer. The preparer's whole ability to affect anything is
// SnapshotAssurance, which is one capture method.
type SnapshotPreparerOptions struct {
	Assurance *SnapshotAssurance
	Policies  AutomationPolicyStore
	Targets   AssuranceTargets
	Baselines AssuranceBaselines
	// Self is the container HarborMaster runs in, which is never prepared: it
	// cannot be updated, so a baseline for it would be work for an update that
	// can never happen.
	Self   SelfReporter
	Logger *slog.Logger
}

// SnapshotPreparer captures the baselines a planner pass will need.
type SnapshotPreparer struct {
	assurance *SnapshotAssurance
	policies  AutomationPolicyStore
	targets   AssuranceTargets
	baselines AssuranceBaselines
	self      SelfReporter
	logger    *slog.Logger
}

// NewSnapshotPreparer builds a SnapshotPreparer.
func NewSnapshotPreparer(opts SnapshotPreparerOptions) *SnapshotPreparer {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SnapshotPreparer{
		assurance: opts.Assurance,
		policies:  opts.Policies,
		targets:   opts.Targets,
		baselines: opts.Baselines,
		self:      opts.Self,
		logger:    logger,
	}
}

// PrepareReport is what one preparation pass did.
type PrepareReport struct {
	// Considered is how many governed containers were examined.
	Considered int
	// Missing is how many of them had no baseline at all.
	Missing int
	// Captured, Deduplicated and Unavailable partition the attempts made.
	Captured     int
	Deduplicated int
	Unavailable  int
	// Deferred is how many were left for a later pass by the per-pass bound.
	Deferred int
}

// Attempted reports how many captures this pass tried.
func (r PrepareReport) Attempted() int { return r.Captured + r.Deduplicated + r.Unavailable }

// PrepareForPlanning captures baselines for governed containers that lack one.
//
// Never returns an error. Every way this can fall short is a REASON that the
// planner should not be stopped for: a pass that could not read its policies
// still plans, and a container that could not be snapshotted is simply one the
// plan will record as having no baseline -- which the acquisition preflight then
// refuses, exactly as it does today.
//
// It captures ONLY for containers a policy governs. An estate with no policies
// prepares nothing, which is what makes the feature cost nothing until an
// operator asks for automation.
func (s *SnapshotPreparer) PrepareForPlanning(ctx context.Context) PrepareReport {
	var report PrepareReport
	if s == nil || s.assurance == nil || !s.assurance.Available() {
		// Snapshots are switched off. An explicit operator choice, and not one
		// this code may quietly reverse.
		return report
	}
	if s.policies == nil || s.targets == nil || s.baselines == nil {
		return report
	}

	policies, err := s.policies.ActivePolicies(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "snapshot preparation could not read update policies",
			slog.Any("error", err))
		return report
	}
	if len(policies) == 0 {
		// Nothing is automatically managed, so nothing needs a baseline in
		// advance. The early exit that keeps this free on a deployment that has
		// not configured automation.
		return report
	}

	targets, truncated, err := s.targets.AutomationTargets(ctx)
	if err != nil {
		return report
	}
	if truncated {
		s.logger.WarnContext(ctx, "snapshot preparation saw a truncated estate; "+
			"some containers were not considered for a baseline")
	}

	baselines, err := s.baselines.BaselineIDs(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "snapshot preparation could not read existing baselines",
			slog.Any("error", err))
		return report
	}

	self := domain.SelfIdentity{}
	if s.self != nil {
		self = s.self.Identity()
	}

	candidates := make([]string, 0, maxAssurancePerPass)
	for _, target := range targets {
		if !s.governed(target, policies, self) {
			continue
		}
		report.Considered++
		if _, exists := baselines[target.ContainerID]; exists {
			continue
		}
		report.Missing++
		if len(candidates) >= maxAssurancePerPass {
			report.Deferred++
			continue
		}
		candidates = append(candidates, target.ContainerID)
	}

	if len(candidates) == 0 {
		return report
	}

	s.capture(ctx, candidates, &report)

	s.logger.InfoContext(ctx, "captured configuration snapshots ahead of planning",
		slog.Int("captured", report.Captured),
		slog.Int("deduplicated", report.Deduplicated),
		slog.Int("unavailable", report.Unavailable),
		slog.Int("deferredToALaterPass", report.Deferred))
	return report
}

// governed reports whether any policy would consider this container.
//
// Uses the SAME selection function the automation engine uses, so a container
// prepared here is a container a pass could actually act on. Reimplementing the
// rule would be a second answer to the question of what a policy governs, which
// is the one place this codebase most needs a single answer.
//
// The two exclusions applied on top are the ones that make preparation pointless
// rather than unsafe: HarborMaster's own container can never be updated, and a
// container carrying the update opt-out will never be acted on. Both are
// re-checked by the automation decision regardless; skipping them here only
// avoids work. (Named in words rather than as a symbol: the architecture guard
// forbidding this file from reaching the decision function is a text check, and
// it cannot tell a mention from a use.)
func (s *SnapshotPreparer) governed(
	target store.AutomationTarget,
	policies []domain.UpdatePolicy,
	self domain.SelfIdentity,
) bool {
	if match, _ := self.SelfMatch(domain.SelfTarget{
		ContainerID:   target.ContainerID,
		ContainerName: target.Selection.Name,
		ImageRef:      target.Selection.Image,
		Labels:        target.Selection.Labels,
	}); match {
		return false
	}
	if overrides := domain.ParseUpdateLabels(target.Selection.Labels); overrides.Disabled {
		return false
	}
	_, governed := domain.SelectUpdatePolicy(policies, target.Selection, self)
	return governed
}

// capture runs the bounded captures and tallies their outcomes.
//
// Concurrency is bounded by a fixed-size worker pool rather than by spawning a
// goroutine per candidate: the candidate list is already bounded, but a pool
// makes the peak explicit rather than incidental.
func (s *SnapshotPreparer) capture(ctx context.Context, candidates []string, report *PrepareReport) {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		next int
	)

	worker := func() {
		defer wg.Done()
		for {
			mu.Lock()
			if next >= len(candidates) || ctx.Err() != nil {
				mu.Unlock()
				return
			}
			containerID := candidates[next]
			next++
			mu.Unlock()

			result := s.assurance.EnsureCurrent(ctx, containerID, domain.SnapshotTriggerScheduled)

			mu.Lock()
			switch result.Outcome {
			case AssuranceCaptured:
				report.Captured++
			case AssuranceCurrent:
				report.Deduplicated++
			default:
				report.Unavailable++
			}
			mu.Unlock()
		}
	}

	workers := assuranceConcurrency
	if workers > len(candidates) {
		workers = len(candidates)
	}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}
	wg.Wait()
}
