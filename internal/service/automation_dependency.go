package service

import (
	"context"
	"log/slog"
	"sort"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Wiring the dependency graph into the decision pass.
//
// # One scheduler, one submission path
//
// Nothing here submits anything. It reorders the indices phase 3 walks and, for
// containers whose dependencies are not satisfied, replaces an eligible verdict
// with a held or blocked one. The submission itself is the same `s.submit` call
// it always was, going to the same acquisition service.
//
// # The gate can only subtract
//
// DecideDependency is a pure function that returns either the verdict it was
// given or a refusal. This file adds no second opinion: it assembles the inputs,
// calls it once per container, and copies the answer onto the decision.
//
// TestTheDependencyWiringCannotSubmitADeclinedDecision walks the whole verdict
// vocabulary through the REAL pass and proves no arrangement of dependency state
// turns a declined decision into a submitted one.
//
// # Stage ordering is a sort, not a queue
//
// Phase 3 walks the returned index order. A container in stage 2 is admitted
// after every stage-1 container has been offered to the budget, so a per-run
// ceiling truncates the TAIL of the estate rather than cutting through the
// middle of a chain -- and a container the ceiling defers stays WAITING for a
// later pass rather than being marked blocked, which is a different and much
// worse answer.

// AutomationDependencies is the dependency evidence a pass reads.
//
// One method, a READ. Nothing on it submits, mutates, or schedules -- the pass
// asks what the estate's ordering is and decides nothing else from it.
type AutomationDependencies interface {
	View(ctx context.Context) (DependencyView, error)
}

// applyDependencyGate holds back decisions whose dependencies are not satisfied,
// and returns the order phase 3 should walk.
//
// # What happens when there is no dependency evidence
//
// Two cases, and they are deliberately different:
//
//   - NOT WIRED. The deployment has no dependency subsystem, so there are no
//     relationships to honour and the pass proceeds exactly as it did before
//     Phase 16. Nothing is lost, because nothing was ever recorded.
//
//   - WIRED BUT UNAVAILABLE. The graph could not be built -- the estate is past
//     a bound, the inventory could not be read. That is "HarborMaster cannot
//     establish what constrains these containers", and per
//     ErrDependencyGraphUnavailable every caller treats it as a refusal. Eligible
//     decisions are held rather than submitted.
//
// The second is the fail-closed direction and it costs an operator a pass. The
// alternative -- submitting unordered updates because the ordering could not be
// read -- is how a provider gets recreated while HarborMaster has no idea what
// shares its namespace.
func (s *AutomationService) applyDependencyGate(
	ctx context.Context,
	outcomes []AutomationOutcome,
) []int {
	order := naturalOrder(len(outcomes))

	if s.dependencies == nil {
		return order
	}

	view, err := s.dependencies.View(ctx)
	if err != nil {
		// Cannot establish. Every decision that would have acted is held.
		s.logger.WarnContext(ctx,
			"the dependency graph could not be built; holding this pass's updates",
			slog.Any("error", err))
		for index := range outcomes {
			outcome := &outcomes[index]
			if !outcome.Eligible() {
				continue
			}
			outcome.Decision.Verdict = domain.VerdictSkip
			outcome.Decision.Reason = domain.ReasonDependencyBlocked
			outcome.Decision.Detail = "HarborMaster could not establish what these " +
				"containers depend on, so it did not change any of them"
			outcome.Decision.DependencyState = domain.DependencyBlocked
		}
		return order
	}

	// HarborMaster's own positive findings that a container needs no update.
	//
	// Read once for the pass, like the graph. A failure here is NOT a refusal:
	// an empty map establishes nothing, and establishing nothing holds every
	// dependent -- so the degraded behaviour is the conservative one and there
	// is nothing to fail closed about.
	assessments, err := s.evidence.CurrentAssessments(ctx, s.now().UTC())
	if err != nil {
		s.logger.WarnContext(ctx,
			"HarborMaster could not read which containers are up to date; "+
				"dependents of a container with no change plan will be held this pass",
			slog.Any("error", err))
		assessments = nil
	}

	facts := dependencyFactsFor(outcomes, assessments)

	for index := range outcomes {
		outcome := &outcomes[index]
		name := domain.NormaliseContainerName(outcome.Decision.ContainerName)

		verdict := DecideDependency(DependencyInput{
			Container: name,
			Verdict:   outcome.Decision.Verdict,
			Reason:    outcome.Decision.Reason,
			Graph:     view.Graph,
			Problems:  view.Problems[name],
			Facts:     facts,
			// INVARIANT A IS NOT EVALUATED HERE, deliberately.
			//
			// The first version of this passed HasHardDependents from the graph
			// with ProviderAssessed false, reasoning that the pass should report
			// what it could not check. The effect was that every namespace
			// provider was blocked by the gate on every pass, permanently: a
			// container with dependents could never be updated by automation at
			// all, because the one thing that would clear it -- a live-host
			// rebindability assessment -- is not something a decision pass does.
			//
			// Invariant A belongs to the EXECUTION PREFLIGHT, which runs twice,
			// reads the live host, and fails closed immediately before anything
			// is stopped. That is where the question can actually be answered,
			// and it is answered there for manual recreations as well as
			// automated ones.
			//
			// So the pass decides ORDER and leaves rebindability to the layer
			// that stops containers. Both fields are left at their zero value,
			// which is what "this pass did not ask" looks like.
		})

		// The ONLY writes. A verdict, a reason, a detail, and the state -- all
		// taken from the pure function's answer.
		outcome.Decision.Verdict = verdict.Verdict
		outcome.Decision.Reason = verdict.Reason
		outcome.Decision.DependencyState = verdict.State
		outcome.Decision.BlockedBy = verdict.BlockedBy
		if verdict.Detail != "" && verdict.State != domain.DependencySatisfied {
			outcome.Decision.Detail = verdict.Detail
		}
	}

	return stageOrder(view.Graph, outcomes)
}

// dependencyFactsFor projects the pass's own decisions onto the facts the gate
// reads.
//
// # Why these come from the decisions rather than from a fresh query
//
// The question a dependent asks is "what did THIS PASS decide about the thing I
// depend on". Re-reading the world would answer a different question, and the
// two could disagree -- a container the pass decided to update would look idle.
//
// Every field is a POSITIVE fact. A container the pass never considered has no
// entry, and DecideDependency treats a missing entry as establishing nothing,
// which holds its dependents rather than releasing them.
func dependencyFactsFor(
	outcomes []AutomationOutcome,
	assessments map[string]domain.CurrentAssessment,
) map[string]DependencyFact {
	facts := make(map[string]DependencyFact, len(outcomes))
	for _, outcome := range outcomes {
		name := domain.NormaliseContainerName(outcome.Decision.ContainerName)
		if name == "" {
			continue
		}

		fact := DependencyFact{
			Present: true,
			Running: outcome.Decision.ContainerState == domain.StateRunning,
			// The pass found work for it: it has a plan proposing a change and
			// reached a verdict that acknowledges one.
			//
			// The assessment is consulted for exactly one reason -- see
			// needsWork -- and only ever to turn "unknown" into "current". A
			// container with no entry keeps the fail-closed answer.
			NeedsWork: needsWork(outcome.Decision, assessments[name]),
			Eligible:  outcome.Eligible(),
			// A pass NEVER verifies anything. Verification is an execution
			// record reaching ExecutionSucceeded, which happens minutes later
			// and is what the follower reads. Always false here, deliberately:
			// a dependent is released by the follower, not by the pass that
			// submitted its provider.
			Verified: false,
		}
		facts[name] = fact
	}
	return facts
}

// needsWork reports whether the pass found something to do for a container.
//
// True for every verdict that acknowledges an available change, including the
// ones that will not act on it: a container awaiting approval NEEDS work, and a
// dependent must wait for it rather than treating it as settled.
func needsWork(
	decision domain.AutomationDecision,
	assessment domain.CurrentAssessment,
) bool {
	switch decision.Verdict {
	case domain.VerdictUpdate, domain.VerdictWouldUpdate, domain.VerdictAwaitingApproval:
		return true
	default:
		// TWO things mean "no work", and both are POSITIVE findings:
		//
		//   1. An explicit no-change plan. The planner assessed this container
		//      and the plan it wrote proposes nothing.
		//   2. A positive current assessment. The planner assessed this
		//      container, found it current, and therefore wrote NO plan at all
		//      -- see domain.AssessCurrent for why that gap exists.
		//
		// The second was missing, and Stage 5b found the consequence live: an
		// upstream on the newest published tag reported `noPlan`, which held
		// its dependent for ever behind "a container this one depends on needs
		// an update that the rules in force do not permit". The upstream needed
		// nothing.
		//
		// # Why every other skip still counts as work
		//
		// This was originally written the other way round -- treating
		// `notSelected`, `noPolicy`, `notEligible` and `noPlan` as "nothing to
		// do" -- and test F caught it. Those four do not say the container is
		// current. They say HarborMaster DID NOT LOOK:
		//
		//   - notSelected / noPolicy / notEligible: no policy governs it, so
		//     the pass never loaded its plan and knows nothing about whether an
		//     update is waiting for it.
		//   - noPlan: the planner wrote no row, which is BOTH "it is current"
		//     and "it was never assessed". Only the assessment tells them
		//     apart, and only for this one reason is it consulted: a container
		//     no policy governs is still unknown however current its image is,
		//     because nothing established that a policy would have let it move.
		//
		// Reading "I did not look" as "it is fine" is the inversion this whole
		// phase exists to avoid, and here it had a concrete consequence: a
		// container excluded from every policy would silently RELEASE its
		// dependents, which is precisely the broadening the dependency
		// subsystem must never do -- in the one direction that lets work
		// proceed rather than holding it.
		//
		// So unknown holds. A dependent whose upstream was never assessed
		// reports dependencyIneligible, an operator sees which container is
		// responsible, and nothing moves until somebody says what should happen
		// to it.
		if decision.Reason == domain.ReasonNoUpdate {
			return false
		}
		// The assessment is admitted for `noPlan` ALONE. Narrowing it to the one
		// reason it explains is what stops it becoming a general override: it
		// cannot release a container a policy excluded, opted out of, paused, or
		// declined, however up to date that container's image happens to be.
		if decision.Reason == domain.ReasonNoPlan && assessment.Established {
			return false
		}
		return true
	}
}

// stageOrder returns the outcome indices in graph-stage order.
//
// Containers the graph cannot place -- absent from it, caught in a cycle, or
// depending on something missing -- go LAST rather than being dropped. They are
// already held by the gate above, and dropping them from the walk would mean
// their decisions were never counted.
//
// Deterministic: the graph's stages are sorted, and containers sharing a stage
// keep their target order, which is itself sorted by container id.
func stageOrder(graph domain.DependencyGraph, outcomes []AutomationOutcome) []int {
	const unplaced = -1

	stageOf := make([]int, len(outcomes))
	for index, outcome := range outcomes {
		name := domain.NormaliseContainerName(outcome.Decision.ContainerName)
		if stage, ordered := graph.StageOf(name); ordered {
			stageOf[index] = stage
			continue
		}
		stageOf[index] = unplaced
	}

	order := naturalOrder(len(outcomes))
	sort.SliceStable(order, func(i, j int) bool {
		left, right := stageOf[order[i]], stageOf[order[j]]
		switch {
		case left == right:
			// Stable: the target order, which the repository sorted by id.
			return false
		case left == unplaced:
			return false
		case right == unplaced:
			return true
		default:
			return left < right
		}
	})
	return order
}

// naturalOrder returns 0..n-1.
func naturalOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	return order
}

// ------------------------------------------------------------- follower --

// advanceDependencyOperations takes the one next step every coordinated update
// is owed.
//
// # Wired into the EXISTING follower
//
// Called from `follow`, which already runs on a timer, already holds no state,
// and already re-reads everything it needs from the database on every tick. This
// adds no loop, no goroutine, and no memory.
//
// # One step per operation per tick
//
// Same discipline the acquisition and execution follow-through uses: each step
// re-reads the world rather than acting on what the previous step believed. An
// operation that needs plans produced and then executions requested gets them on
// separate ticks.
func (s *AutomationService) advanceDependencyOperations(ctx context.Context) {
	if s.dependencies == nil {
		return
	}
	coordinator, ok := s.dependencies.(dependencyCoordinator)
	if !ok {
		return
	}

	operations, err := coordinator.Recover(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "could not read outstanding dependency operations",
			slog.Any("error", err))
		return
	}

	for _, recovered := range operations {
		if ctx.Err() != nil {
			return
		}
		s.advanceDependencyOperation(ctx, coordinator, recovered)
	}
}

// dependencyCoordinator is the follower's half of the dependency service.
//
// Separate from AutomationDependencies so a deployment that wires only the READ
// gets the gate and not the progression -- and so the pass cannot reach the
// coordinator's writes even by accident.
type dependencyCoordinator interface {
	AutomationDependencies
	Recover(ctx context.Context) ([]RecoveredOperation, error)
	RecoverOperation(ctx context.Context, operationID string) (RecoveredOperation, error)
	ProduceRebindPlans(ctx context.Context, operationID string) (RebindPlanResult, error)
	ConcludeOperation(ctx context.Context, operationID string) (domain.DependencyOperationState, error)
	RebindMemberSubmitted(ctx context.Context, operationID, dependent, acquisitionID string) error
	RebindMemberExecuting(ctx context.Context, operationID, dependent, executionID string) error
	RebindMemberFailed(ctx context.Context, operationID, dependent string) error
}

// advanceDependencyOperation takes one operation's next step.
func (s *AutomationService) advanceDependencyOperation(
	ctx context.Context,
	coordinator dependencyCoordinator,
	recovered RecoveredOperation,
) {
	operationID := recovered.Operation.OperationID

	// 1. The provider has not finished. Nothing to do: its own execution record
	//    is being followed by the acquisition/execution follow-through, and this
	//    operation is waiting on that rather than on anything here.
	if !recovered.ProviderVerified {
		// Unless it FAILED, in which case the operation concludes.
		if _, err := coordinator.ConcludeOperation(ctx, operationID); err != nil {
			s.logger.WarnContext(ctx, "could not conclude a dependency operation",
				slog.String("operationId", operationID), slog.Any("error", err))
		}
		return
	}

	// 2. The provider succeeded. Produce the plans its dependents need.
	//
	// Idempotent: a member that already holds a usable plan keeps it, and every
	// condition is re-read against the live records first.
	result, err := coordinator.ProduceRebindPlans(ctx, operationID)
	if err != nil {
		s.logger.WarnContext(ctx, "could not produce reattachment plans",
			slog.String("operationId", operationID), slog.Any("error", err))
		return
	}
	for dependent, refusal := range result.Skipped {
		s.logger.WarnContext(ctx, "a container sharing a replaced namespace cannot be reattached",
			slog.String("operationId", operationID),
			slog.String("dependent",
				domain.SanitiseDisplayText(dependent, domain.MaxContainerNameBytes)),
			slog.String("refusal", string(refusal)))
	}

	// 3. Take each outstanding member its next step.
	//
	// Re-read rather than driven from `result`. An earlier version iterated
	// `result.Created` -- the plans produced by THIS tick -- which silently
	// skipped every member whose plan already existed. That is invisible on the
	// happy path, because the tick that creates a plan is the tick that submits
	// its acquisition; it matters when that submission fails. The plan is
	// durable, so every later tick reports it under `Reused`, and the member was
	// never looked at again. One transient error stranded a dependent
	// permanently, with a plan on disk and nothing driving it.
	//
	// So the input is the RECORDS, which is the same discipline every other step
	// here follows. `Skipped` is still logged above because a refusal is a
	// judgement worth reporting; it is not what decides the work.
	current, err := coordinator.RecoverOperation(ctx, operationID)
	if err != nil {
		s.logger.WarnContext(ctx, "could not re-read a dependency operation",
			slog.String("operationId", operationID), slog.Any("error", err))
		return
	}
	for _, member := range current.Outstanding {
		if ctx.Err() != nil {
			return
		}
		s.advanceRebindMember(ctx, coordinator, current, member)
	}

	// 4. Conclude, from the records rather than from anything this tick did.
	//
	// Succeeds only when the provider verified AND every mandatory member
	// verified. A provider success alone never reaches this.
	if _, err := coordinator.ConcludeOperation(ctx, operationID); err != nil {
		s.logger.WarnContext(ctx, "could not conclude a dependency operation",
			slog.String("operationId", operationID), slog.Any("error", err))
	}
}

// advanceRebindMember takes one outstanding reattachment its next step.
//
// # The whole of what this can do
//
// Two submissions, both to services that own their own capability and re-run
// their own preflight, and one bookkeeping write. There is no Docker interface
// here, no container id, and nowhere to put one: an acquisition names a plan, an
// execution names an acquisition, and both records were written by HarborMaster
// itself. Invariant 10, and invariant 11's statement that automation is a caller
// rather than a capability, are unchanged by this function.
//
// # Why the recreation needs its own step at all
//
// Because until Stage 5a there was not one. The follower planned the
// reattachment, pulled the image, wrote the acquisition id, moved the member to
// `acquired`, and stopped. Nothing in production code ever asked for the
// recreation, so a dependent that had been detached by its provider's
// replacement stayed detached indefinitely while the operation sat in
// `rebindRunning` looking like work in progress. Found live in Stage 5 against
// Docker 29.7.2; the member set showed one acquisition and zero executions for
// as long as the process ran.
//
// # What each answer means
//
// The step is decided by ONE fact -- how far this member's own records got --
// and the four things an acquisition can say are deliberately not collapsed:
//
//	succeeded      request the recreation
//	still running  wait. A pull in flight is not a pull that finished, and
//	               recreating from an image that is still transferring is the
//	               race the acquisition/execution split exists to prevent.
//	terminal, not
//	  succeeded    settle the member. There is no verified local image to
//	               recreate from and there never will be under this acquisition.
//	unreadable     wait. A record that could not be read establishes nothing --
//	               invariant 5 -- and settling on it would leave the dependent
//	               detached and the operation terminal over a transient error.
//
// The last two are the pair that look identical to a single
// `if state != succeeded`, and conflating them is the same mistake that ended a
// coordinated update three seconds after it began; see executionOutcome.
//
// # Why a duplicate recreation cannot happen
//
// Three independent reasons, because a duplicate here is a second
// stop-and-replace of a container that may already be correct:
//
//  1. A member that already names an execution is skipped, and the id is
//     persisted, so a restart sees it too.
//  2. Both requests carry a deterministic idempotency key derived from the
//     operation and the dependent, so a retried submission returns the record it
//     already created rather than making a second one. That is what covers the
//     gap between a successful request and a failed bookkeeping write.
//  3. The execution store permits one active execution per container.
func (s *AutomationService) advanceRebindMember(
	ctx context.Context,
	coordinator dependencyCoordinator,
	operation RecoveredOperation,
	member domain.DependencyMember,
) {
	operationID := operation.Operation.OperationID
	// Bounded and sanitised before it reaches a log line. A container name is
	// operator-supplied text.
	dependent := domain.SanitiseDisplayText(member.Dependent, domain.MaxContainerNameBytes)
	// One key per member per operation, used for both submissions so a retried
	// tick re-reads its own record rather than starting a second one.
	requestKey := "dependency:rebind:" + operationID + ":" + member.Dependent

	switch {
	case member.ExecutionID != "":
		// The recreation was already asked for. What happened to it is the
		// execution record's business, and recovery derives the member's state
		// from it on every pass. Nothing to do, and nothing to re-request.
		return

	case member.PlanID == "":
		// No plan yet. ProduceRebindPlans owns that and has either just written
		// one -- in which case the acquisition follows on the next tick -- or
		// refused this container for a reason already logged above.
		return

	case member.AcquisitionID == "":
		// A plan, and no pull yet. Ask for the image, through the SAME request
		// an operator's own update uses: one identifier, and every other detail
		// derived by the acquisition service's own preflight.
		acquisition, err := s.pipeline.RequestAcquisition(ctx, AcquisitionRequest{
			PlanID:      member.PlanID,
			RequestKey:  requestKey,
			RequestedBy: operation.Operation.RequestedBy,
		})
		if err != nil {
			// Transient by assumption, and retried on the next tick against the
			// same plan and the same key. NOT a member failure: the acquisition
			// service declining to accept a request now says nothing about
			// whether this container can be reattached.
			s.logger.WarnContext(ctx, "could not request the image for a reattachment",
				slog.String("operationId", operationID),
				slog.String("dependent", dependent),
				slog.Any("error", err))
			return
		}
		if err := coordinator.RebindMemberSubmitted(ctx, operationID, member.Dependent,
			acquisition.AcquisitionID); err != nil {
			s.logger.WarnContext(ctx, "could not record a reattachment's acquisition",
				slog.String("operationId", operationID),
				slog.String("dependent", dependent),
				slog.Any("error", err))
		}
		return
	}

	// A pull was requested. Its own record decides what happens next.
	acquisition, err := s.pipeline.Acquisition(ctx, member.AcquisitionID)
	if err != nil {
		s.logger.WarnContext(ctx, "could not read the image acquisition a reattachment names",
			slog.String("operationId", operationID),
			slog.String("dependent", dependent),
			slog.Any("error", err))
		return
	}
	if !acquisition.State.Terminal() {
		// Still pulling. Waiting is not failure.
		return
	}
	if acquisition.State != domain.AcquisitionSucceeded {
		// Established POSITIVELY: the record exists, it is terminal, and it is
		// not success. Only this settles a member.
		if err := coordinator.RebindMemberFailed(ctx, operationID, member.Dependent); err != nil {
			s.logger.WarnContext(ctx, "could not record a reattachment's failure",
				slog.String("operationId", operationID),
				slog.String("dependent", dependent),
				slog.Any("error", err))
		}
		return
	}

	// The image is present locally and was confirmed by read-only inspection.
	// Ask for the recreation -- naming the acquisition, and nothing else.
	execution, err := s.pipeline.RequestExecution(ctx, ExecutionRequest{
		AcquisitionID: member.AcquisitionID,
		RequestKey:    requestKey,
		RequestedBy:   operation.Operation.RequestedBy,
	})
	if err != nil {
		// Includes every refusal the execution preflight can raise -- the
		// container is gone, its namespace provider is missing, it is
		// HarborMaster itself. Logged as a closed-vocabulary refusal by the
		// execution service, retried here on the next tick, and never turned
		// into a member failure from this side: the record it wrote is the
		// authority on what happened.
		s.logger.WarnContext(ctx, "could not request the recreation for a reattachment",
			slog.String("operationId", operationID),
			slog.String("dependent", dependent),
			slog.Any("error", err))
		return
	}
	if err := coordinator.RebindMemberExecuting(ctx, operationID, member.Dependent,
		execution.ExecutionID); err != nil {
		// The recreation IS under way; only the note of it failed. The next tick
		// re-requests under the same key and gets this same record back, so the
		// id is recovered rather than duplicated.
		s.logger.WarnContext(ctx, "could not record a reattachment's recreation",
			slog.String("operationId", operationID),
			slog.String("dependent", dependent),
			slog.Any("error", err))
	}
}

// Compile-time proof that the dependency service satisfies both halves, so a
// change to either interface breaks here rather than at a wiring site.
var (
	_ AutomationDependencies = (*DependencyService)(nil)
	_ dependencyCoordinator  = (*DependencyService)(nil)
)
