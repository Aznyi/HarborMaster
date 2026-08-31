package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Readiness: answering "what could this policy do right now" WITHOUT building a
// second automation engine.
//
// # Why there is no ReadinessService in this file
//
// Stage 17.3 observed that no ReadinessService existed and Stage 17.4 was
// expected to create one. Tracing the pipeline first showed that would have
// been a mistake. The authoritative answer per container is an
// AutomationDecision, and the code that produces one already exists, is already
// pure, and is already read-only:
//
//	phase 1   DecideAutomation        eleven ordered checks, one verdict
//	phase 2   applyDependencyGate     holds what the graph says to hold
//
// Both are what a real pass runs. Anything that re-derived eligibility from
// them would be a second implementation of the rules, free to drift from the
// first -- and an operator would be reading a number no pass will honour.
//
// So readiness is a QUERY over those two phases, and the only new code is
// composition, attribution, and counting:
//
//	evaluateEstate   phases 1 and 2, shared with the pass itself
//	Readiness        the same, for one policy, summarised
//
// # What this fixed on the way
//
// Extracting phases 1 and 2 exposed that `Upcoming` ran only phase 1, so it
// reported a container the pass would hold for its dependencies as eligible,
// and that it discarded the estate-truncation flag. Both were live defects in
// an existing readiness surface, and both are gone because there is now one
// implementation rather than two. See automation_readiness_gap_test.go.

// estateEvaluation is what phases 1 and 2 produce over the whole estate.
type estateEvaluation struct {
	// Outcomes is one per container, in target order.
	Outcomes []AutomationOutcome
	// Order is the index order phase 3 must walk, from the dependency gate.
	Order []int
	// Truncated reports the estate being cut at the target bound. Carried
	// rather than discarded: every count derived from a truncated estate
	// describes a prefix, and a reader has to be told.
	Truncated bool
	// Now is the single clock reading the whole evaluation used.
	Now time.Time
}

// Decisions projects the evaluation onto the records a caller reads.
func (e estateEvaluation) Decisions() []domain.AutomationDecision {
	return collectDecisions(e.Outcomes)
}

// evaluateEstate runs phase 1 and phase 2 over every target, for one policy set.
//
// # Read-only, and structurally so
//
// It reads policies, targets, pauses, plans, in-flight state and the dependency
// view. It writes nothing: no run row, no decision row, no request to any
// service, and no Docker call -- AutomationOptions has nowhere to put a Docker
// interface, which is what eight architecture tests hold.
//
// `policies` is a parameter rather than a read so a caller may evaluate a
// candidate policy that is not saved. Nothing about the estate changes with it;
// only which rule each container is measured against.
func (s *AutomationService) evaluateEstate(
	ctx context.Context,
	policies []domain.UpdatePolicy,
	now time.Time,
) (estateEvaluation, error) {
	targets, truncated, err := s.evidence.Targets(ctx)
	if err != nil {
		return estateEvaluation{}, fmt.Errorf("load automation targets: %w", err)
	}

	pauses, err := s.store.ActivePauses(ctx)
	if err != nil {
		return estateEvaluation{}, fmt.Errorf("load automation pauses: %w", err)
	}
	pausedBy := make(map[string]domain.PausedContainer, len(pauses))
	for _, pause := range pauses {
		pausedBy[pause.ContainerName] = pause
	}

	// Every per-container behaviour an operator chose, read ONCE for the pass
	// like the pauses above. A preference may only narrow what the governing
	// policy permits, so a failure to read them is not a reason to refuse the
	// pass -- it degrades to the policies alone, which is the SAFER direction
	// only for the containers whose preference widened nothing. A preference
	// that narrows is a restriction, so losing it could permit more than the
	// operator asked for: the pass therefore fails rather than guessing.
	preferences, err := s.store.ContainerPreferences(ctx)
	if err != nil {
		return estateEvaluation{}, fmt.Errorf("load container update preferences: %w", err)
	}

	// ONE identity reading, for the reason the pass reads one: a refresh
	// landing mid-evaluation must not make two containers see different
	// answers to "is this HarborMaster".
	self := s.selfIdentity()

	outcomes := make([]AutomationOutcome, 0, len(targets))
	for index, target := range targets {
		if ctx.Err() != nil {
			return estateEvaluation{}, ctx.Err()
		}

		input := AutomationInput{
			Target:                  target.Selection,
			ContainerID:             target.ContainerID,
			Policies:                policies,
			Now:                     now,
			RequireApprovalForMajor: s.cfg.RequireApprovalForMajor,
			Self:                    self,
		}
		if pause, paused := pausedBy[target.Selection.Name]; paused {
			input.Pause, input.IsPaused = pause, true
		}
		// By NAME, which is what survives the recreation this may authorise.
		input.Preference = preferences[target.Selection.Name]

		// The only per-container reads, and every one an indexed point lookup.
		// Skipped entirely for a container a cheaper check already declined --
		// see loadContainerEvidence.
		s.loadContainerEvidence(ctx, &input, pausedBy, policies)

		outcome := DecideAutomation(input)
		outcome.Decision.Position = index
		// The lifecycle state the evaluation saw, so a dependent can tell
		// "needs no update" from "needs no update AND is up".
		outcome.Decision.ContainerState = target.State
		outcomes = append(outcomes, outcome)
	}

	// Phase 2. Read-only: it reads the graph and HarborMaster's own
	// "needs no update" findings, and the only thing it may do to a decision is
	// turn an eligible one into a held or blocked one.
	order := s.applyDependencyGate(ctx, outcomes)

	return estateEvaluation{
		Outcomes:  outcomes,
		Order:     order,
		Truncated: truncated,
		Now:       now,
	}, nil
}

// Readiness answers the policy-preview question for one policy.
//
// `candidate` may be:
//
//   - nil, meaning "the saved policies as they stand". Every decision is
//     attributed to whichever saved policy made it.
//   - a saved policy, meaning "this stored rule". It REPLACES its saved self in
//     the set, so an operator previewing an edit sees the edit rather than what
//     is stored.
//   - an unsaved candidate carrying domain.AutomationReadinessCandidatePolicyID, meaning
//     "this configuration, if I saved it". It joins the other enabled policies
//     rather than being evaluated alone, because SelectUpdatePolicy gives each
//     container to exactly one policy and a preview that ignored the others
//     would credit this one with containers it will never govern.
//
// Nothing is persisted in any case. A candidate is never written, never given a
// real identifier, and never visible to a pass.
func (s *AutomationService) Readiness(
	ctx context.Context,
	candidate *domain.UpdatePolicy,
) (domain.AutomationReadinessReport, []domain.AutomationDecision, error) {
	if !s.Readable() {
		return domain.AutomationReadinessReport{}, nil, ErrAutomationDisabled
	}

	saved, err := s.policies.ActivePolicies(ctx)
	if err != nil {
		return domain.AutomationReadinessReport{}, nil, fmt.Errorf("load update policies: %w", err)
	}

	policies := saved
	attributeTo := ""
	if candidate != nil {
		policies = withCandidate(saved, *candidate)
		attributeTo = candidate.PolicyID
	}

	evaluation, err := s.evaluateEstate(ctx, policies, s.now().UTC())
	if err != nil {
		return domain.AutomationReadinessReport{}, nil, err
	}
	decisions := evaluation.Decisions()

	report := domain.SummariseAutomationReadiness(
		decisions, attributeTo, evaluation.Now, evaluation.Truncated)

	// With no candidate there is no single policy to attribute to, so the
	// summary's per-policy counts are meaningless and only the estate-wide
	// facts are returned. The decisions carry the rest.
	if candidate == nil {
		report = domain.AutomationReadinessReport{
			EvaluatedAt: evaluation.Now.UTC(),
			Truncated:   evaluation.Truncated,
			Considered:  len(decisions),
		}
	}

	return report, decisions, nil
}

// withCandidate substitutes a candidate policy into the saved set.
//
// A policy with the same identifier is REPLACED rather than added, so
// previewing an edit measures the edit. Everything else is kept, because the
// question is what this policy would do on an estate the other policies are
// also governing.
func withCandidate(
	saved []domain.UpdatePolicy,
	candidate domain.UpdatePolicy,
) []domain.UpdatePolicy {
	policies := make([]domain.UpdatePolicy, 0, len(saved)+1)
	for _, policy := range saved {
		if policy.PolicyID == candidate.PolicyID {
			continue
		}
		policies = append(policies, policy)
	}
	return append(policies, candidate)
}
