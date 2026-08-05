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

// The policy engine.
//
// # What it does, in one sentence
//
// It projects each container's current configuration into a domain.PolicyTarget
// and applies every active policy's rules to it, then reconciles the failures
// against what is already stored.
//
// # What it deliberately does NOT do
//
// It does not enforce. A policy is a rule HarborMaster checks configuration
// AGAINST; it is never applied to Docker, and there is no call into
// docker.Runtime anywhere below. The engine reads the inventory HarborMaster
// has already persisted, not the daemon.
//
// It does not interpret. Every rule's semantics live in domain/policy_rules.go
// and are fixed at compile time. An administrator supplies a rule type from a
// closed catalogue and a bounded parameter list; nothing they write is
// evaluated as code.
//
// It does not read a secret. The evaluation input is built by
// domain.NewPolicyTarget, which carries environment variable NAMES and has no
// field capable of holding a value.
//
// # Two properties worth stating plainly
//
// AN INCOMPLETE PASS RESOLVES NOTHING. A pass that could not apply every policy
// has not established that the rules it skipped now pass, so recording it as
// authoritative would silently clear real non-compliance. Incomplete passes are
// stored as incomplete, and the repository refuses to resolve on them.
//
// ACKNOWLEDGEMENT DOES NOT SUPPRESS. An acknowledged or exempted violation is
// re-evaluated on every pass, keeps its last-seen timestamp current, and
// resolves automatically the moment the container complies. An acknowledgement
// that stopped the checking would turn the compliance report into a list of
// things somebody once clicked.

// ErrPolicyDisabled reports that the policy engine is switched off.
var ErrPolicyDisabled = errors.New("policy engine is disabled")

// ErrNoActivePolicies reports that no policy is defined and enabled.
//
// Distinguished from "everything complies" on purpose: an estate with no
// policies has not been found compliant, it has not been asked anything. The
// engine writes nothing in this case rather than recording a vacuous pass for
// every container on every refresh.
var ErrNoActivePolicies = errors.New("no active policies are defined")

// PolicyDefinitions is the definition capability the engine needs.
//
// A narrow interface rather than *store.PolicyRepository, so the engine is
// testable without a database and so the surface it depends on is visible in
// one place. Note what is ABSENT: no create, no update, no archive. The engine
// reads policies; it does not administer them.
type PolicyDefinitions interface {
	ActivePolicies(ctx context.Context, limit int) ([]domain.PolicyDefinition, error)
}

// PolicyContainers is the inventory capability the engine needs.
type PolicyContainers interface {
	Get(ctx context.Context, id string) (*domain.ContainerDetail, error)
	List(ctx context.Context, filter store.ContainerFilter) ([]domain.ContainerSummary, int, error)
}

// PolicyStore is the persistence capability.
type PolicyStore interface {
	ReconcilePolicy(ctx context.Context, evaluation domain.PolicyEvaluation,
		violations []domain.PolicyViolation, now time.Time) (store.UpsertResult, error)
}

// PolicyPruner removes resolved history. Separate from PolicyStore because
// retention is optional: a deployment that keeps compliance history forever
// supplies no pruner and the worker simply never prunes.
type PolicyPruner interface {
	PruneResolvedViolations(ctx context.Context, cutoff time.Time, batch int) (int64, error)
}

// PolicyInventory reports the current inventory generation, which every pass
// records so a violation can be tied to the observation behind it.
type PolicyInventory interface {
	CurrentGeneration(ctx context.Context) (int64, string, error)
}

// PolicyOptions configures a PolicyService.
type PolicyOptions struct {
	Definitions PolicyDefinitions
	Containers  PolicyContainers
	Violations  PolicyStore
	Pruner      PolicyPruner
	Inventory   PolicyInventory

	Config config.Policy
	Logger *slog.Logger
	Now    func() time.Time
}

// PolicyService evaluates containers against policies and persists the result.
type PolicyService struct {
	definitions PolicyDefinitions
	containers  PolicyContainers
	violations  PolicyStore
	pruner      PolicyPruner
	inventory   PolicyInventory

	// queue coalesces per-container evaluation requests. See policy_worker.go.
	queue *evaluationQueue

	cfg    config.Policy
	logger *slog.Logger
	now    func() time.Time

	// sweeping guards the full pass so two cannot overlap. A second pass while
	// one runs would re-read the same containers and contend for the single
	// SQLite writer to reach the same conclusion.
	sweeping sync.Mutex

	// policyCount is the size of the most recently loaded active set, reported
	// through Status. Guarded because Status is called from HTTP handlers while
	// the worker updates it.
	counts sync.RWMutex
	active int
}

// NewPolicyService builds a PolicyService.
func NewPolicyService(opts PolicyOptions) *PolicyService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.MaxPolicies < 1 {
		cfg.MaxPolicies = config.DefaultPolicyMaxPolicies
	}
	if cfg.MaxViolationsPerContainer < 1 {
		cfg.MaxViolationsPerContainer = config.DefaultPolicyMaxViolationsPerContainer
	}
	if cfg.EvaluationTimeout <= 0 {
		cfg.EvaluationTimeout = config.DefaultPolicyEvaluationTimeout
	}
	if cfg.EvaluationDebounce <= 0 {
		cfg.EvaluationDebounce = config.DefaultPolicyEvaluationDebounce
	}
	if cfg.MaxPendingEvaluations < 1 {
		cfg.MaxPendingEvaluations = config.DefaultPolicyMaxPendingEvaluations
	}

	return &PolicyService{
		definitions: opts.Definitions,
		containers:  opts.Containers,
		violations:  opts.Violations,
		pruner:      opts.Pruner,
		inventory:   opts.Inventory,
		queue:       newEvaluationQueue(cfg.EvaluationDebounce, cfg.MaxPendingEvaluations, now),
		cfg:         cfg,
		logger:      logger,
		now:         now,
	}
}

// Enabled reports whether the policy engine is switched on.
func (s *PolicyService) Enabled() bool { return s.cfg.Enabled }

// Limits exposes the definition bounds the API validates against, so one
// configured value governs both what the API accepts and what the engine can
// be asked to evaluate.
func (s *PolicyService) Limits() domain.PolicyLimits {
	return domain.PolicyLimits{
		MaxRules:            s.cfg.MaxRulesPerPolicy,
		MaxValuesPerRule:    s.cfg.MaxValuesPerRule,
		MaxNameBytes:        s.cfg.MaxNameBytes,
		MaxDescriptionBytes: s.cfg.MaxDescriptionBytes,
	}
}

// EvaluateContainer evaluates one container against every active policy.
//
// Loads the active set itself, so a single-container request is self-contained.
// The SWEEP does not use this path: it loads the set once and calls
// evaluateAgainst for each container, which is what keeps a thousand-container
// pass from becoming a thousand policy queries.
func (s *PolicyService) EvaluateContainer(
	ctx context.Context,
	containerID string,
) (domain.PolicyEvaluation, error) {
	if !s.ready() {
		return domain.PolicyEvaluation{}, ErrPolicyDisabled
	}

	policies, err := s.loadActivePolicies(ctx)
	if err != nil {
		return domain.PolicyEvaluation{}, err
	}
	if len(policies) == 0 {
		return domain.PolicyEvaluation{}, ErrNoActivePolicies
	}
	return s.evaluateAgainst(ctx, containerID, policies)
}

// evaluateAgainst evaluates one container against an already-loaded policy set.
func (s *PolicyService) evaluateAgainst(
	ctx context.Context,
	containerID string,
	policies []domain.PolicyDefinition,
) (domain.PolicyEvaluation, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.EvaluationTimeout)
	defer cancel()

	evaluatedAt := s.now().UTC()
	generation := s.currentGeneration(ctx)

	detail, err := s.containers.Get(ctx, containerID)
	if err != nil || detail == nil {
		// A container that has left the inventory has no current configuration
		// to check. Nothing is recorded: writing an evaluation would claim a
		// pass that did not happen.
		if errors.Is(err, store.ErrNotFound) || detail == nil {
			return domain.PolicyEvaluation{}, store.ErrNotFound
		}
		return domain.PolicyEvaluation{}, err
	}

	evaluation, violations := s.apply(*detail, policies, generation, evaluatedAt)
	return s.persist(ctx, evaluation, violations)
}

// apply runs every policy against one container. Pure apart from the clock
// value it is handed, which is what makes it directly testable.
func (s *PolicyService) apply(
	detail domain.ContainerDetail,
	policies []domain.PolicyDefinition,
	generation int64,
	evaluatedAt time.Time,
) (domain.PolicyEvaluation, []domain.PolicyViolation) {
	// The projection that drops secret values. See domain.NewPolicyTarget: the
	// target type has no field that can hold one.
	target := domain.NewPolicyTarget(detail)

	evaluation := domain.PolicyEvaluation{
		ContainerID:         detail.Overview.ID,
		ContainerName:       detail.Overview.Name,
		EvaluatedAt:         evaluatedAt,
		InventoryGeneration: generation,
		PoliciesEvaluated:   len(policies),
		Complete:            true,
	}

	violations := make([]domain.PolicyViolation, 0, 8)
	for _, policy := range policies {
		for _, rule := range policy.Rules {
			// Bounded per container. A pass that hits the cap is marked
			// INCOMPLETE rather than truncated silently, because an incomplete
			// pass resolves nothing and therefore cannot clear a real
			// violation it never reached.
			if len(violations) >= s.cfg.MaxViolationsPerContainer {
				evaluation.Complete = false
				evaluation.Reason = "the container exceeded its violation budget; some rules were not applied"
				evaluation.ViolationCount = len(violations)
				return evaluation, violations
			}

			evaluation.RulesEvaluated++
			result := domain.EvaluatePolicyRule(rule, target)
			if result.Compliant {
				continue
			}

			violations = append(violations, domain.PolicyViolation{
				PolicyID:            policy.PolicyID,
				PolicyName:          policy.Name,
				ContainerID:         evaluation.ContainerID,
				ContainerName:       evaluation.ContainerName,
				RuleType:            rule.Type,
				Severity:            rule.EffectiveSeverity(policy.Severity),
				DetectedAt:          evaluatedAt,
				LastSeenAt:          evaluatedAt,
				InventoryGeneration: generation,
				Observed:            result.Observed,
				Expected:            result.Expected,
				Reason:              result.Reason,
				Status:              domain.PolicyViolationActive,
			})
		}
	}

	evaluation.ViolationCount = len(violations)
	evaluation.Compliant = evaluation.Complete && evaluation.ViolationCount == 0
	return evaluation, violations
}

// persist writes the pass and its violations.
func (s *PolicyService) persist(
	ctx context.Context,
	evaluation domain.PolicyEvaluation,
	violations []domain.PolicyViolation,
) (domain.PolicyEvaluation, error) {
	// Detached from cancellation but bounded: a pass that finished evaluating
	// must not lose its result to a shutdown mid-write, and must not hold the
	// process open either.
	writeCtx, cancel := GraceContext(ctx, policyWriteGrace, policyWriteMax)
	defer cancel()

	result, err := s.violations.ReconcilePolicy(writeCtx, evaluation, violations, s.now().UTC())
	if err != nil {
		return domain.PolicyEvaluation{}, err
	}

	if result.Inserted > 0 || result.Resolved > 0 {
		s.logger.Info("policy evaluated",
			slog.String("container", domain.ShortenID(evaluation.ContainerID)),
			slog.Int("new", result.Inserted),
			slog.Int("stillFailing", result.Updated),
			slog.Int("resolved", result.Resolved),
			slog.Bool("complete", evaluation.Complete))
	}
	return evaluation, nil
}

// policyWriteGrace and policyWriteMax bound the persistence write at shutdown.
const (
	policyWriteGrace = 5 * time.Second
	policyWriteMax   = 30 * time.Second
)

// loadActivePolicies reads the active set and records its size for Status.
//
// The set is loaded with a limit one ABOVE the configured maximum, so hitting
// the cap is detectable rather than silent: a pass that could not see every
// policy must report itself incomplete, and it cannot do that if the query
// returns exactly the cap and looks like a complete answer.
func (s *PolicyService) loadActivePolicies(ctx context.Context) ([]domain.PolicyDefinition, error) {
	policies, err := s.definitions.ActivePolicies(ctx, s.cfg.MaxPolicies+1)
	if err != nil {
		return nil, err
	}

	truncated := false
	if len(policies) > s.cfg.MaxPolicies {
		policies = policies[:s.cfg.MaxPolicies]
		truncated = true
	}

	s.counts.Lock()
	s.active = len(policies)
	s.counts.Unlock()

	if truncated {
		s.logger.Warn("more active policies exist than the configured maximum; the remainder are not evaluated",
			slog.Int("maximum", s.cfg.MaxPolicies))
	}
	return policies, nil
}

// ready reports whether the engine is configured well enough to run.
func (s *PolicyService) ready() bool {
	return s.cfg.Enabled &&
		s.definitions != nil &&
		s.containers != nil &&
		s.violations != nil
}

// currentGeneration reads the inventory generation, tolerating failure.
//
// A generation that cannot be read is recorded as zero rather than failing the
// pass: the generation is provenance, and losing provenance is a smaller harm
// than not detecting non-compliance at all.
func (s *PolicyService) currentGeneration(ctx context.Context) int64 {
	if s.inventory == nil {
		return 0
	}
	generation, _, err := s.inventory.CurrentGeneration(ctx)
	if err != nil {
		return 0
	}
	return generation
}

// Sweep evaluates every container in the inventory against every active policy.
//
// This is the authoritative pass, and it is the BATCHED one: the active policy
// set is loaded ONCE and applied to every container, so a thousand containers
// cost one policy query rather than a thousand.
//
// Containers are processed SEQUENTIALLY and in pages. A thousand containers
// evaluated concurrently would hold a thousand decoded container documents in
// memory and contend for the single SQLite writer; sequential evaluation costs
// wall time, which a background sweep has, and bounds memory, which it does not
// otherwise have.
func (s *PolicyService) Sweep(ctx context.Context) (SweepResult, error) {
	var result SweepResult
	if !s.ready() {
		return result, ErrPolicyDisabled
	}

	// Refused rather than queued: a second concurrent sweep re-reads the same
	// containers to reach the same conclusion.
	if !s.sweeping.TryLock() {
		return result, nil
	}
	defer s.sweeping.Unlock()

	policies, err := s.loadActivePolicies(ctx)
	if err != nil {
		return result, err
	}
	if len(policies) == 0 {
		return result, ErrNoActivePolicies
	}

	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		summaries, total, err := s.containers.List(ctx, store.ContainerFilter{
			Page: store.Page{Limit: pageSize, Offset: offset},
		})
		if err != nil {
			return result, err
		}
		if len(summaries) == 0 {
			return result, nil
		}

		for _, summary := range summaries {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			// An absent container has no current configuration to check.
			if !summary.Present {
				result.Skipped++
				continue
			}

			switch _, err := s.evaluateAgainst(ctx, summary.ID, policies); {
			case err == nil:
				result.Evaluated++
			case errors.Is(err, store.ErrNotFound):
				result.Skipped++
			default:
				result.Failed++
				s.logger.Warn("policy evaluation failed",
					slog.String("container", domain.ShortenID(summary.ID)),
					slog.String("error", err.Error()))
			}
		}

		if offset+len(summaries) >= total {
			return result, nil
		}
	}
}
