package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// A provider execution that is STILL RUNNING is not a failed one.
//
// # The defect this pins
//
// `ConcludeOperation` used to read `!ProviderVerified` as "the provider's
// update did not succeed". That predicate is also true while the execution is
// in flight, so a coordinated update was concluded FAILED three seconds after
// it was recorded -- while the provider's own recreation was running and about
// to succeed.
//
// The consequence is the worst available: `failed` is terminal, so the follower
// stopped looking at the operation, the dependent was never reattached, and it
// was left holding a namespace reference to a container that no longer existed.
// Silently, with the container still running and Docker reporting nothing.
//
// Found in Stage 5 live acceptance against Docker 29.7.2. Nothing in the unit
// suite covered it: every existing fixture set the provider's execution to a
// TERMINAL state before concluding, so the in-flight moment was never modelled.
//
// This is CLAUDE.md invariant 5 in miniature -- a check that could not be
// performed establishes nothing, and must not be read as failure.

// inflightOperation records one operation with a single pending rebind.
//
// The provider execution it NAMES is supplied separately by each test, because
// the whole subject here is what different execution states establish.
func inflightOperation(t *testing.T) (*fakeOperationStore, domain.DependencyOperation) {
	t.Helper()

	operations := newFakeOperationStore()
	created, err := operations.Create(context.Background(), operationWith(
		memberFor("sonarr", domain.MemberPending, ""),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("record the operation: %v", err)
	}
	return operations, created
}

// providerIn builds the execution read for a provider in one state.
func providerIn(state domain.ExecutionState) fakeExecutions {
	return fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": {State: state},
	}}
}

func TestAProviderExecutionInFlightDoesNotFailTheOperation(t *testing.T) {
	t.Parallel()

	for _, state := range []domain.ExecutionState{
		domain.ExecutionQueued,
		domain.ExecutionValidating,
		domain.ExecutionCapturing,
		domain.ExecutionCreating,
		domain.ExecutionStarting,
		domain.ExecutionVerifying,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			operations, created := inflightOperation(t)
			svc := restartOver(operations, providerIn(state))
			ctx := context.Background()

			concluded, err := svc.ConcludeOperation(ctx, created.OperationID)
			if err != nil {
				t.Fatalf("ConcludeOperation: %v", err)
			}
			if concluded == domain.OperationFailed {
				t.Fatalf("an in-flight provider execution (%s) concluded the operation "+
					"as %q\n\n"+
					"`failed` is terminal, so nothing looks at the operation again: the "+
					"dependent is never reattached and is left holding a namespace "+
					"reference to a container that no longer exists. An execution that "+
					"has not finished establishes nothing and must not be read as a "+
					"failure.", state, concluded)
			}
			if concluded.Terminal() {
				t.Errorf("state = %q, which is terminal; the operation is still running",
					concluded)
			}

			recovered, err := svc.RecoverOperation(ctx, created.OperationID)
			if err != nil {
				t.Fatalf("RecoverOperation: %v", err)
			}
			if recovered.ProviderVerified {
				t.Error("a running execution was reported as verified")
			}
			if recovered.ProviderFailed {
				t.Error("a running execution was reported as failed")
			}
			if recovered.Complete {
				t.Error("the operation was complete with its provider still running")
			}
			// And there is still work to do, so the follower keeps looking.
			if !recovered.NeedsWork() {
				t.Error("the operation reports no outstanding work while its provider runs")
			}
		})
	}
}

// A provider execution that finished unsuccessfully DOES fail the operation.
//
// The other half of the pair: the fix must not have turned a real provider
// failure into an operation that waits forever.
func TestASettledUnsuccessfulProviderStillFailsTheOperation(t *testing.T) {
	t.Parallel()

	for _, state := range []domain.ExecutionState{
		domain.ExecutionFailed,
		domain.ExecutionCancelled,
		domain.ExecutionExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			operations, created := inflightOperation(t)
			svc := restartOver(operations, providerIn(state))
			ctx := context.Background()

			concluded, err := svc.ConcludeOperation(ctx, created.OperationID)
			if err != nil {
				t.Fatalf("ConcludeOperation: %v", err)
			}
			if concluded != domain.OperationFailed {
				t.Fatalf("state = %q, want %q", concluded, domain.OperationFailed)
			}

			recovered, err := svc.RecoverOperation(ctx, created.OperationID)
			if err != nil {
				t.Fatalf("RecoverOperation: %v", err)
			}
			if !recovered.ProviderFailed {
				t.Error("a settled unsuccessful execution was not reported as failed")
			}
			if recovered.ProviderVerified {
				t.Error("a failed execution was reported as verified")
			}
		})
	}
}

// An execution record that cannot be read establishes nothing.
//
// Not verified, and NOT failed. A missing record is a gap in evidence, and a
// gap must not conclude an operation in either direction.
func TestAnUnreadableProviderExecutionEstablishesNothing(t *testing.T) {
	t.Parallel()

	operations, created := inflightOperation(t)
	// No records at all: every lookup answers ErrNotFound.
	svc := restartOver(operations, fakeExecutions{records: map[string]domain.Execution{}})
	ctx := context.Background()

	recovered, err := svc.RecoverOperation(ctx, created.OperationID)
	if err != nil {
		t.Fatalf("RecoverOperation: %v", err)
	}
	if recovered.ProviderVerified || recovered.ProviderFailed {
		t.Errorf("an unreadable record established verified=%v failed=%v",
			recovered.ProviderVerified, recovered.ProviderFailed)
	}

	concluded, err := svc.ConcludeOperation(ctx, created.OperationID)
	if err != nil {
		t.Fatalf("ConcludeOperation: %v", err)
	}
	if concluded == domain.OperationFailed {
		t.Errorf("an unreadable provider execution concluded the operation as %q",
			concluded)
	}
}

// Every execution state lands in exactly one of the three answers.
//
// Walked end to end so a state added later cannot fall into the wrong bucket by
// default. `verified` and `failed` must never both hold, and a non-terminal
// state must produce neither.
func TestEveryExecutionStateIsClassifiedOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Non-vacuity: the vocabulary must actually contain both kinds.
	var terminal, running int
	for _, state := range domain.ExecutionStates {
		if state.Terminal() {
			terminal++
		} else {
			running++
		}
	}
	if terminal == 0 || running == 0 {
		t.Fatalf("execution states: %d terminal, %d running; this test needs both",
			terminal, running)
	}

	for _, state := range domain.ExecutionStates {
		operations, created := inflightOperation(t)
		svc := restartOver(operations, providerIn(state))

		recovered, err := svc.RecoverOperation(ctx, created.OperationID)
		if err != nil {
			t.Fatalf("%s: RecoverOperation: %v", state, err)
		}

		switch {
		case recovered.ProviderVerified && recovered.ProviderFailed:
			t.Errorf("%s: reported as both verified and failed", state)
		case !state.Terminal() && (recovered.ProviderVerified || recovered.ProviderFailed):
			t.Errorf("%s: a non-terminal state established verified=%v failed=%v; "+
				"an execution that has not finished establishes nothing",
				state, recovered.ProviderVerified, recovered.ProviderFailed)
		case state == domain.ExecutionSucceeded && !recovered.ProviderVerified:
			t.Errorf("%s: a succeeded execution was not reported as verified", state)
		case state.Terminal() && state != domain.ExecutionSucceeded && !recovered.ProviderFailed:
			t.Errorf("%s: a settled unsuccessful execution was not reported as failed", state)
		}
	}
}
