package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Reconstructing a dependency operation after a restart.
//
// # Nothing here remembers anything
//
// The whole of what remains to be done is derived, on every call, from rows: the
// operation, its members, and the acquisition and execution records those
// members name. No field on this service survives a restart, because no field on
// this service is consulted.
//
// That is not a stylistic preference. A provider update plus three rebinds spans
// minutes and four separate executions, and HarborMaster can be killed at any
// point in it. An in-memory coordinator would have to guess what remained, and
// the safe-looking guess -- redo everything -- is a second unattended mutation
// of containers that may already be correct.
//
// # The execution record outranks the member's own state
//
// A member row says `executing`; the execution it names says `succeeded`. The
// EXECUTION is right. The member's state is a cache written by whichever process
// last got that far, and a process killed between the mutation completing and
// the cache being updated leaves it stale by exactly one step.
//
// So recovery re-reads the record and derives the member's true position from
// it. The member row is then corrected. Reading it the other way round is how a
// completed rebind gets repeated.
//
// # Recovery decides nothing about Docker
//
// It reports what is outstanding. It does not retry, resubmit, or roll anything
// back, and it does not interpret an interrupted execution -- the execution
// service's own recovery pass owns that, and this code consumes its verdict
// rather than forming a second one.

// DependencyExecutions is the record read the dependency subsystem needs.
//
// Two methods, both READS of HarborMaster's own execution table. There is
// nothing here that can start, retry, or cancel anything.
type DependencyExecutions interface {
	Execution(ctx context.Context, executionID string) (domain.Execution, error)
	// ReplacementFor answers "what did HarborMaster replace this container
	// with", from the execution that did it. The authoritative half of a
	// namespace rebind; see DependencyService.ResolveNamespaceProvider.
	ReplacementFor(ctx context.Context, containerID string) (string, error)
}

// DependencyExecutionReader is the repository shape the adapter below wraps.
type DependencyExecutionReader interface {
	Get(ctx context.Context, executionID string) (domain.Execution, error)
	ReplacementFor(ctx context.Context, containerID string) (string, error)
}

// NewDependencyExecutions adapts the execution repository for the dependency
// subsystem.
//
// A rename, and deliberately not a widening: the repository has a dozen methods
// including several that advance state, and the dependency subsystem is handed
// exactly two -- both READS. What it cannot reach it cannot call.
func NewDependencyExecutions(executions DependencyExecutionReader) DependencyExecutions {
	return dependencyExecutions{executions: executions}
}

type dependencyExecutions struct{ executions DependencyExecutionReader }

func (d dependencyExecutions) Execution(
	ctx context.Context,
	executionID string,
) (domain.Execution, error) {
	if d.executions == nil {
		return domain.Execution{}, store.ErrNotFound
	}
	return d.executions.Get(ctx, executionID)
}

func (d dependencyExecutions) ReplacementFor(
	ctx context.Context,
	containerID string,
) (string, error) {
	if d.executions == nil {
		return "", store.ErrNotFound
	}
	return d.executions.ReplacementFor(ctx, containerID)
}

// DependencyOperationStore is the operation persistence recovery reads and
// corrects.
type DependencyOperationStore interface {
	Open(ctx context.Context, limit int) ([]domain.DependencyOperation, error)
	Get(ctx context.Context, operationID string) (domain.DependencyOperation, error)
	ActiveForProvider(ctx context.Context, provider string) (domain.DependencyOperation, bool, error)
	Create(ctx context.Context, operation domain.DependencyOperation, now time.Time) (domain.DependencyOperation, error)
	AdvanceOperation(ctx context.Context, operationID string,
		state domain.DependencyOperationState, failure domain.DependencyOperationFailure,
		now time.Time) error
	AdvanceMember(ctx context.Context, update store.MemberUpdate, now time.Time) error
	MembersForDependent(ctx context.Context, dependent string) ([]domain.DependencyMember, error)
	AttachProviderExecution(ctx context.Context, operationID, executionID string, now time.Time) error
}

// RecoveredOperation is one operation as the RECORDS describe it, not as any
// process remembers it.
type RecoveredOperation struct {
	Operation domain.DependencyOperation

	// ProviderVerified is derived from the provider's execution record. False
	// when there is no execution, when it has not finished, or when it finished
	// unsuccessfully -- all three of which mean the same thing here: the
	// operation is not done.
	ProviderVerified bool

	// ProviderFailed is the POSITIVE statement that the provider's execution
	// finished and did not succeed.
	//
	// Deliberately not `!ProviderVerified`. An execution still running is not a
	// verified one and is not a failed one either, and conflating those two
	// ended a coordinated update as failed while its provider was mid-flight --
	// see executionOutcome.
	ProviderFailed bool

	// Outstanding are the members that still hold the operation open, sorted.
	Outstanding []domain.DependencyMember
	// Unsuccessful are the members that settled without succeeding, sorted.
	Unsuccessful []domain.DependencyMember

	// Complete is the DERIVED answer: provider verified AND every mandatory
	// rebind verified. Never read from a stored flag.
	Complete bool
}

// NeedsWork reports whether anything remains to be done for this operation.
func (r RecoveredOperation) NeedsWork() bool {
	return !r.Complete && len(r.Unsuccessful) == 0 && !r.Operation.State.Terminal()
}

// Recover reconstructs every operation that has not reached a conclusion.
//
// Bounded by the repository's own limit. Two queries for the operations and
// their members, then one point lookup per execution actually referenced --
// which is at most one per member, and only for members that got far enough to
// name one.
func (s *DependencyService) Recover(ctx context.Context) ([]RecoveredOperation, error) {
	if s == nil || s.operations == nil {
		return nil, nil
	}

	open, err := s.operations.Open(ctx, 0)
	if err != nil {
		return nil, err
	}

	recovered := make([]RecoveredOperation, 0, len(open))
	for _, operation := range open {
		recovered = append(recovered, s.recoverOne(ctx, operation))
	}
	return recovered, nil
}

// RecoverOperation reconstructs one operation by id.
func (s *DependencyService) RecoverOperation(
	ctx context.Context,
	operationID string,
) (RecoveredOperation, error) {
	if s == nil || s.operations == nil {
		return RecoveredOperation{}, store.ErrNotFound
	}
	operation, err := s.operations.Get(ctx, operationID)
	if err != nil {
		return RecoveredOperation{}, err
	}
	return s.recoverOne(ctx, operation), nil
}

// recoverOne derives one operation's true position from the records.
func (s *DependencyService) recoverOne(
	ctx context.Context,
	operation domain.DependencyOperation,
) RecoveredOperation {
	// The provider first. An operation whose provider never succeeded is not
	// waiting on rebinds -- there is nothing for a dependent to reattach TO.
	//
	// Both answers are read, and neither is the negation of the other: an
	// execution still running establishes nothing in either direction.
	providerVerified, providerFailed := s.executionOutcome(ctx, operation.ProviderExecutionID)

	// Then each member, corrected against the execution it names.
	for index := range operation.Members {
		member := &operation.Members[index]
		derived := s.deriveMemberState(ctx, *member)
		if derived == member.State {
			continue
		}

		// The row disagreed with the record. The RECORD wins, and the row is
		// corrected so the next reader does not have to re-derive it.
		//
		// A failure to write the correction is logged and not raised: the
		// derived answer is already correct in memory, and refusing to continue
		// because a cache could not be updated would turn a cosmetic problem
		// into a stalled operation.
		previous := member.State
		member.State = derived
		if err := s.operations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID: member.OperationID,
			Dependent:   member.Dependent,
			State:       derived,
			Refusal:     member.Refusal,
		}, s.now()); err != nil {
			s.logger.WarnContext(ctx,
				"could not correct a dependency member's recorded state",
				slog.String("operationId", member.OperationID),
				slog.String("dependent",
					domain.SanitiseDisplayText(member.Dependent, domain.MaxContainerNameBytes)),
				slog.String("from", string(previous)),
				slog.String("to", string(derived)),
				slog.Any("error", err))
		}
	}

	return RecoveredOperation{
		Operation:        operation,
		ProviderVerified: providerVerified,
		ProviderFailed:   providerFailed,
		Outstanding:      operation.Outstanding(),
		Unsuccessful:     operation.Unsuccessful(),
		// The definition of dependency-operation success, in one place.
		Complete: operation.Successful(providerVerified),
	}
}

// deriveMemberState reads a member's true position from the execution it names.
//
// # The four cases, and why each answers the way it does
//
//  1. No execution id. The member never got as far as requesting a recreation,
//     so whatever the row says about plans or acquisitions stands -- those are
//     HarborMaster's own records and nothing else writes them.
//
//  2. The execution cannot be read. NOT downgraded, and not promoted. The row
//     keeps what it had, because "the record is unavailable" establishes
//     nothing in either direction.
//
//  3. The execution has not finished. `executing`, whatever the row said. A
//     member the last process believed was verified while its execution is
//     still running is a member whose cache was written optimistically.
//
//  4. The execution finished. `verified` only for ExecutionSucceeded, which is
//     HarborMaster's terminal state AFTER the health proof, the digest
//     verification, the preservation comparison, and the network check.
//     Everything else is `failed`.
//
// Case 3 is the one that matters for restart safety: an interrupted execution
// stays interrupted from this code's point of view, and the EXECUTION
// SERVICE'S own recovery pass decides what happened to it. This function never
// second-guesses that and never retries a mutation.
func (s *DependencyService) deriveMemberState(
	ctx context.Context,
	member domain.DependencyMember,
) domain.DependencyMemberState {
	// A member that was blocked stays blocked. The refusal was a judgement about
	// the container, not about an execution, and no record can overturn it.
	if member.State == domain.MemberBlocked {
		return domain.MemberBlocked
	}
	if member.ExecutionID == "" {
		return member.State
	}
	if s.executions == nil {
		return member.State
	}

	execution, err := s.executions.Execution(ctx, member.ExecutionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.WarnContext(ctx, "could not read the execution a rebind names",
				slog.String("executionId", member.ExecutionID), slog.Any("error", err))
		}
		return member.State
	}

	if !execution.State.Terminal() {
		return domain.MemberExecuting
	}
	if execution.State == domain.ExecutionSucceeded {
		return domain.MemberVerified
	}
	return domain.MemberFailed
}

// executionOutcome reports what an execution record POSITIVELY establishes.
//
// # Why two booleans rather than one
//
// Because "verified" and "failed" are not opposites here, and treating them as
// opposites is a defect Stage 5 found against a real daemon.
//
// A single `verified bool` answers false for four different situations: no id,
// an unreadable record, an execution STILL RUNNING, and one that finished
// unsuccessfully. Holding the rebinds is the right response to all four, and
// `Complete` uses it that way. But CONCLUDING the operation is a different
// question, and reading "not verified yet" as "the provider failed" ended a
// coordinated update as failed three seconds after it started -- while the
// provider's own execution was still running and about to succeed. The
// dependent was never reattached, and the operation was terminal so nothing
// retried.
//
// So each answer is established positively:
//
//	verified  the record exists, is terminal, and is ExecutionSucceeded
//	failed    the record exists, is terminal, and is anything else
//	neither   no id, no store, an unreadable record, or one still running
//
// "Neither" is the fail-closed case in both directions: it does not release the
// rebinds and it does not condemn the operation.
func (s *DependencyService) executionOutcome(
	ctx context.Context,
	executionID string,
) (verified bool, failed bool) {
	if executionID == "" || s.executions == nil {
		return false, false
	}
	execution, err := s.executions.Execution(ctx, executionID)
	if err != nil {
		return false, false
	}
	if !execution.State.Terminal() {
		// Still running. Nothing is established.
		return false, false
	}
	if execution.State == domain.ExecutionSucceeded {
		return true, false
	}
	return false, true
}

// ConcludeOperation records an operation's ending, derived from its members.
//
// # Why the conclusion is computed rather than passed in
//
// A caller that decides "this succeeded" and writes it is a caller that can be
// wrong. The only inputs here are the records, and the rule is the one stated on
// domain.DependencyOperation.Successful: the provider verified AND every
// mandatory rebind verified.
//
// # There is no group rollback
//
// A failed member does not undo the provider and does not revert members that
// already succeeded. See domain.GroupRollbackIsNotPerformed for the three
// reasons; the short one is that reverting the provider would break exactly the
// containers that are currently correct.
func (s *DependencyService) ConcludeOperation(
	ctx context.Context,
	operationID string,
) (domain.DependencyOperationState, error) {
	recovered, err := s.RecoverOperation(ctx, operationID)
	if err != nil {
		return "", err
	}

	state := domain.OperationRebindPending
	failure := domain.OperationFailureNone

	switch {
	case recovered.Complete:
		state = domain.OperationSucceeded

	case recovered.ProviderFailed:
		// The provider's own update FINISHED and did not succeed. Nothing was
		// rebound and nothing needs to be: the namespace it would have replaced
		// is the one the dependents are already attached to.
		//
		// `ProviderFailed`, not `!ProviderVerified`. The second is also true
		// while the provider's execution is still running, and reading that as
		// a failure ended a coordinated update three seconds after it began --
		// terminal, so nothing retried, and the dependent was never reattached.
		// Found in Stage 5 acceptance against a real daemon.
		state = domain.OperationFailed
		failure = domain.OperationFailureProvider

	case len(recovered.Unsuccessful) > 0:
		// At least one mandatory rebind settled without succeeding. The
		// operation has failed even though the provider and possibly several
		// dependents are fine -- which is exactly the partial state an operator
		// needs told about rather than tidied away.
		state = domain.OperationFailed
		failure = domain.OperationFailureRebind
		for _, member := range recovered.Unsuccessful {
			if member.State == domain.MemberBlocked {
				failure = domain.OperationFailureNotRebindable
				break
			}
		}

	case len(recovered.Outstanding) > 0:
		state = domain.OperationRebindPending
		for _, member := range recovered.Outstanding {
			if member.State == domain.MemberExecuting || member.State == domain.MemberAcquired {
				state = domain.OperationRebindRunning
				break
			}
		}

	case recovered.ProviderVerified:
		state = domain.OperationProviderVerified
	}

	if state == recovered.Operation.State {
		return state, nil
	}
	if err := s.operations.AdvanceOperation(ctx, operationID, state, failure, s.now()); err != nil {
		return state, err
	}

	// One notification per container that was left unattached.
	//
	// Raised HERE, on the transition, so it is sent once rather than on every
	// recovery sweep over a record that has not changed. The message itself is
	// written in notify_raise.go and carries two container names and one
	// HarborMaster identifier -- no image, no digest, no daemon text.
	//
	// Only this condition notifies. A loop, a wait, and a block are all real
	// dependency states and none of them can leave a workload broken with no
	// other signal, which is what this one does.
	if state == domain.OperationFailed {
		for _, member := range recovered.Unsuccessful {
			NotifyRebindFailed(s.notifier, member.Dependent, member.Provider, operationID)
		}
	}

	if state == domain.OperationFailed {
		s.logger.WarnContext(ctx, "a dependency operation failed",
			slog.String("operationId", operationID),
			slog.String("provider", domain.SanitiseDisplayText(
				recovered.Operation.Provider, domain.MaxContainerNameBytes)),
			slog.String("failure", string(failure)),
			slog.Int("unsuccessful", len(recovered.Unsuccessful)))
	}
	return state, nil
}
