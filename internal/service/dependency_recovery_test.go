package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Restart recovery: what a new process can work out from rows alone.
//
// # How "restart" is modelled here
//
// Every test builds a service, does something, then builds a SECOND service over
// the SAME store and asks it what remains. The second service shares no field
// with the first -- that is the whole point. If any of this depended on memory,
// the second service would answer differently, and these tests would fail.
//
// # What must never happen
//
// None of the four scenarios below may be read as "the operation is finished".
// A restart that concluded an operation was complete would stop the outstanding
// rebinds from ever being performed, leaving containers attached to a namespace
// that no longer exists -- silently, which is the failure mode this whole phase
// exists to prevent.

const (
	recoveryProvider  = "gluetun"
	recoveryDependent = "sonarr"
	recoveryOther     = "radarr"
)

// fakeOperationStore is an in-memory stand-in for the operation repository.
//
// It PERSISTS across service construction, which is what makes it a restart
// fixture rather than a mock: a second service built over the same instance sees
// exactly the rows the first one wrote, and nothing else.
type fakeOperationStore struct {
	// mu makes the fixture concurrency-safe.
	//
	// The real repository serialises writes through SQLite with
	// MaxOpenConns(1), and backs that with a partial unique index on
	// non-terminal states. This stands in for both.
	//
	// It is not a convenience. Without it the concurrent dedup test races on
	// the FIXTURE, which means it crashes before it can say anything about the
	// production deduplication it exists to exercise -- a test that fails for
	// the wrong reason tells you as little as one that passes for the wrong
	// reason.
	mu         sync.Mutex
	operations map[string]domain.DependencyOperation
}

func newFakeOperationStore() *fakeOperationStore {
	return &fakeOperationStore{operations: make(map[string]domain.DependencyOperation)}
}

func (f *fakeOperationStore) Create(
	_ context.Context, operation domain.DependencyOperation, now time.Time,
) (domain.DependencyOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if operation.OperationID == "" {
		operation.OperationID = domain.NewDependencyOperationID()
	}
	if operation.State == "" {
		operation.State = domain.OperationQueued
	}
	operation.CreatedAt, operation.UpdatedAt = now, now
	for index := range operation.Members {
		operation.Members[index].OperationID = operation.OperationID
		if operation.Members[index].State == "" {
			operation.Members[index].State = domain.MemberPending
		}
	}
	f.operations[operation.OperationID] = operation
	return operation, nil
}

func (f *fakeOperationStore) Get(_ context.Context, operationID string) (domain.DependencyOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	operation, ok := f.operations[operationID]
	if !ok {
		return domain.DependencyOperation{}, store.ErrNotFound
	}
	// Returned by value with a copied member slice, so a caller mutating what it
	// got back cannot reach into the "database".
	operation.Members = append([]domain.DependencyMember(nil), operation.Members...)
	return operation, nil
}

func (f *fakeOperationStore) Open(_ context.Context, _ int) ([]domain.DependencyOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var open []domain.DependencyOperation
	for id, operation := range f.operations {
		if operation.State.Terminal() {
			continue
		}
		operation.OperationID = id
		operation.Members = append([]domain.DependencyMember(nil), operation.Members...)
		open = append(open, operation)
	}
	return open, nil
}

func (f *fakeOperationStore) ActiveForProvider(
	_ context.Context, provider string,
) (domain.DependencyOperation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, operation := range f.operations {
		if operation.Provider == provider && !operation.State.Terminal() {
			return operation, true, nil
		}
	}
	return domain.DependencyOperation{}, false, nil
}

func (f *fakeOperationStore) AdvanceOperation(
	_ context.Context, operationID string,
	state domain.DependencyOperationState, failure domain.DependencyOperationFailure,
	now time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	operation, ok := f.operations[operationID]
	if !ok {
		return store.ErrNotFound
	}
	operation.State, operation.Failure, operation.UpdatedAt = state, failure, now
	if state.Terminal() {
		completed := now
		operation.CompletedAt = &completed
	}
	f.operations[operationID] = operation
	return nil
}

func (f *fakeOperationStore) AdvanceMember(
	_ context.Context, update store.MemberUpdate, now time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	operation, ok := f.operations[update.OperationID]
	if !ok {
		return store.ErrNotFound
	}
	for index := range operation.Members {
		if operation.Members[index].Dependent != update.Dependent {
			continue
		}
		member := &operation.Members[index]
		member.State, member.Refusal, member.UpdatedAt = update.State, update.Refusal, now
		if update.PlanID != "" {
			member.PlanID = update.PlanID
		}
		if update.AcquisitionID != "" {
			member.AcquisitionID = update.AcquisitionID
		}
		if update.ExecutionID != "" {
			member.ExecutionID = update.ExecutionID
		}
		// The provider identity the plan targets.
		//
		// Persisted here because the real repository persists it, and because
		// dropping it silently defeats the staleness check: `existingPlanFor`
		// compares it against the provider's CURRENT id to decide whether a
		// recorded plan still describes the transition that happened. A fake
		// that forgot it would make every plan look eternally valid.
		if update.TargetProviderID != "" {
			member.TargetProviderID = update.TargetProviderID
		}
		f.operations[update.OperationID] = operation
		return nil
	}
	return store.ErrNotFound
}

func (f *fakeOperationStore) MembersForDependent(
	_ context.Context, dependent string,
) ([]domain.DependencyMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var members []domain.DependencyMember
	for _, operation := range f.operations {
		for _, member := range operation.Members {
			if member.Dependent == dependent {
				members = append(members, member)
			}
		}
	}
	return members, nil
}

// fakeExecutions is the execution record read recovery derives member state from.
type fakeExecutions struct {
	records map[string]domain.Execution

	// replacements maps an ORIGINAL container id to the one that replaced it,
	// as the executions table does through container_id -> replacement_id.
	//
	// This is the authoritative half of a namespace rebind: it is how
	// ResolveNamespaceProvider learns that a dependent's captured
	// `container:<old>` must become `container:<new>`. A fake that could not
	// answer it would make the rebind path untestable, which is how it went
	// untested and unworking to a real daemon.
	replacements map[string]string
}

func (f fakeExecutions) Execution(_ context.Context, executionID string) (domain.Execution, error) {
	execution, ok := f.records[executionID]
	if !ok {
		return domain.Execution{}, store.ErrNotFound
	}
	return execution, nil
}

func (f fakeExecutions) ReplacementFor(_ context.Context, containerID string) (string, error) {
	replacement, ok := f.replacements[containerID]
	if !ok {
		return "", store.ErrNotFound
	}
	return replacement, nil
}

// restartOver builds a NEW service over an existing store, which is the whole
// point: it shares no field with whatever wrote the rows.
func restartOver(operations *fakeOperationStore, executions fakeExecutions) *service.DependencyService {
	return service.NewDependencyService(service.DependencyOptions{
		Operations: operations,
		Executions: executions,
		Now:        func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) },
	})
}

func operationWith(members ...domain.DependencyMember) domain.DependencyOperation {
	return domain.DependencyOperation{
		Provider:            recoveryProvider,
		ProviderExecutionID: "exec_" + "0123456789abcdef0000",
		State:               domain.OperationProviderVerified,
		Members:             members,
	}
}

func memberFor(dependent string, state domain.DependencyMemberState, executionID string) domain.DependencyMember {
	return domain.DependencyMember{
		Dependent:   dependent,
		Provider:    recoveryProvider,
		Source:      domain.DependencyNetworkNamespace,
		State:       state,
		ExecutionID: executionID,
	}
}

func succeededExecution() domain.Execution {
	return domain.Execution{State: domain.ExecutionSucceeded}
}

// A. Provider verified, no rebind started.
//
// The operation must remain INCOMPLETE and both members must remain pending. A
// restart that read "the provider succeeded" as "the operation succeeded" would
// leave two containers attached to a namespace that no longer exists.
func TestRestartAfterProviderVerifiedLeavesEveryRebindOutstanding(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": succeededExecution(),
	}}

	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberPending, ""),
		memberFor(recoveryOther, domain.MemberPending, ""),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The restart.
	recovered, err := restartOver(operations, executions).
		RecoverOperation(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	if !recovered.ProviderVerified {
		t.Fatal("the provider's verified execution was not recognised")
	}
	// THE assertion.
	if recovered.Complete {
		t.Fatal("a provider success was read as the whole operation succeeding")
	}
	if len(recovered.Outstanding) != 2 {
		t.Fatalf("outstanding = %d, want 2", len(recovered.Outstanding))
	}
	if len(recovered.Unsuccessful) != 0 {
		t.Fatalf("unsuccessful = %v, want none", recovered.Unsuccessful)
	}
	if !recovered.NeedsWork() {
		t.Fatal("the operation does not report that work remains")
	}
}

// B. Provider verified, dependent A rebound, dependent B pending.
//
// A must NOT be repeated and B must remain outstanding.
func TestRestartMidChainRepeatsNothingAndForgetsNothing(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": succeededExecution(),
		"exec_0123456789abcdef0001": succeededExecution(),
	}}

	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberVerified, "exec_0123456789abcdef0001"),
		memberFor(recoveryOther, domain.MemberPending, ""),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recovered, err := restartOver(operations, executions).
		RecoverOperation(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	if recovered.Complete {
		t.Fatal("the operation reported complete with a rebind still pending")
	}
	outstanding := recovered.Outstanding
	if len(outstanding) != 1 {
		t.Fatalf("outstanding = %v, want only %s", outstanding, recoveryOther)
	}
	// A is not repeated: it is not in the outstanding set.
	if outstanding[0].Dependent != recoveryOther {
		t.Fatalf("outstanding names %q; the already-verified rebind would be repeated",
			outstanding[0].Dependent)
	}
}

// C. A rebind execution exists but has not finished.
//
// The member reads as EXECUTING, not verified and not failed. The execution
// service's own recovery owns what happens to it; dependency code must not
// resubmit the mutation or guess at its outcome.
func TestAnUnfinishedRebindExecutionIsNotConcludedByDependencyCode(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": succeededExecution(),
		// Mid-flight. Deliberately NOT terminal.
		"exec_0123456789abcdef0002": {State: domain.ExecutionCreating},
	}}

	// The row optimistically claims verified; the RECORD says otherwise.
	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberVerified, "exec_0123456789abcdef0002"),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recovered, err := restartOver(operations, executions).
		RecoverOperation(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	if recovered.Complete {
		t.Fatal("an unfinished execution was read as a completed rebind")
	}
	if len(recovered.Outstanding) != 1 ||
		recovered.Outstanding[0].State != domain.MemberExecuting {
		t.Fatalf("outstanding = %+v, want one member in executing", recovered.Outstanding)
	}

	// And the correction was written back, so the next reader does not have to
	// re-derive it.
	stored, err := operations.Get(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Members[0].State != domain.MemberExecuting {
		t.Fatalf("the stored member state was not corrected: %q", stored.Members[0].State)
	}
}

// D. A terminal operation creates no new work on restart.
func TestATerminalOperationIsNotReopenedByARestart(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": succeededExecution(),
		"exec_0123456789abcdef0001": succeededExecution(),
	}}

	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberVerified, "exec_0123456789abcdef0001"),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := operations.AdvanceOperation(context.Background(), created.OperationID,
		domain.OperationSucceeded, domain.OperationFailureNone, time.Now().UTC()); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// The recovery sweep must not pick it up at all.
	open, err := restartOver(operations, executions).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("a terminal operation was reopened: %+v", open)
	}
}

// Partial failure: the provider and one dependent succeeded, another failed.
//
// The approved semantics, and they are load-bearing:
//
//   - the provider stays replaced
//   - the successfully rebound dependent stays rebound
//   - the operation is FAILED and needs attention
//   - nothing is rolled back automatically
//
// See domain.GroupRollbackIsNotPerformed for why reverting the provider would
// break exactly the containers that are currently correct.
func TestPartialRebindFailureFailsTheOperationWithoutUndoingAnything(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": succeededExecution(),
		"exec_0123456789abcdef0001": succeededExecution(),
		"exec_0123456789abcdef0003": {State: domain.ExecutionFailed},
	}}

	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberVerified, "exec_0123456789abcdef0001"),
		memberFor(recoveryOther, domain.MemberExecuting, "exec_0123456789abcdef0003"),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	svc := restartOver(operations, executions)
	state, err := svc.ConcludeOperation(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if state != domain.OperationFailed {
		t.Fatalf("state = %q, want failed", state)
	}

	stored, err := operations.Get(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Failure != domain.OperationFailureRebind {
		t.Fatalf("failure = %q, want rebindFailed", stored.Failure)
	}
	if !stored.State.NeedsOperator() {
		t.Fatal("a failed operation does not report that it needs a person")
	}

	// NOTHING was undone. The verified member is still verified.
	for _, member := range stored.Members {
		if member.Dependent == recoveryDependent && member.State != domain.MemberVerified {
			t.Fatalf("a successfully rebound dependent was reverted to %q", member.State)
		}
	}
	// And the policy is stated in code, not just in a comment somebody can
	// delete.
	if domain.GroupRollbackIsNotPerformed() == "" {
		t.Fatal("the no-group-rollback policy has no stated form")
	}
}

// A provider whose own update failed fails the operation for THAT reason, and
// no rebind is expected: the namespace the dependents hold was never replaced.
func TestAFailedProviderFailsTheOperationWithoutRequiringRebinds(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": {State: domain.ExecutionFailed},
	}}

	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberPending, ""),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	state, err := restartOver(operations, executions).
		ConcludeOperation(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if state != domain.OperationFailed {
		t.Fatalf("state = %q, want failed", state)
	}
	stored, _ := operations.Get(context.Background(), created.OperationID)
	if stored.Failure != domain.OperationFailureProvider {
		t.Fatalf("failure = %q, want providerFailed", stored.Failure)
	}
}

// The only path to success: provider verified AND every member verified.
func TestAnOperationSucceedsOnlyWhenEveryRebindIsVerified(t *testing.T) {
	t.Parallel()

	operations := newFakeOperationStore()
	executions := fakeExecutions{records: map[string]domain.Execution{
		"exec_0123456789abcdef0000": succeededExecution(),
		"exec_0123456789abcdef0001": succeededExecution(),
		"exec_0123456789abcdef0002": succeededExecution(),
	}}

	created, err := operations.Create(context.Background(), operationWith(
		memberFor(recoveryDependent, domain.MemberVerified, "exec_0123456789abcdef0001"),
		memberFor(recoveryOther, domain.MemberVerified, "exec_0123456789abcdef0002"),
	), time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	state, err := restartOver(operations, executions).
		ConcludeOperation(context.Background(), created.OperationID)
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if state != domain.OperationSucceeded {
		t.Fatalf("state = %q, want succeeded", state)
	}
}

// The definition itself, exercised directly over both halves.
func TestDependencyOperationSuccessRequiresBothHalves(t *testing.T) {
	t.Parallel()

	complete := domain.DependencyOperation{Members: []domain.DependencyMember{
		{Dependent: "a", State: domain.MemberVerified},
	}}
	incomplete := domain.DependencyOperation{Members: []domain.DependencyMember{
		{Dependent: "a", State: domain.MemberVerified},
		{Dependent: "b", State: domain.MemberPending},
	}}

	if complete.Successful(false) {
		t.Fatal("every rebind verified but the provider unverified reported success")
	}
	if incomplete.Successful(true) {
		t.Fatal("provider verified but a rebind pending reported success")
	}
	if !complete.Successful(true) {
		t.Fatal("both halves satisfied did not report success")
	}
	// Only `verified` clears. An id existing is not a completion.
	for _, state := range domain.DependencyMemberStates {
		member := domain.DependencyOperation{Members: []domain.DependencyMember{
			{Dependent: "a", State: state, ExecutionID: "exec_0123456789abcdef0001"},
		}}
		if member.Complete() != (state == domain.MemberVerified) {
			t.Errorf("member state %q reported complete = %v", state, member.Complete())
		}
	}
}

// AttachProviderExecution links the record to the execution performing it.
func (f *fakeOperationStore) AttachProviderExecution(
	_ context.Context, operationID, executionID string, now time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	operation, ok := f.operations[operationID]
	if !ok {
		return store.ErrNotFound
	}
	operation.ProviderExecutionID = executionID
	operation.UpdatedAt = now
	f.operations[operationID] = operation
	return nil
}

// markProviderExecution records that the provider's own recreation is the named
// execution, without going through the service.
//
// A test lever, not a production path: the execution service attaches this
// itself. Tests use it to place an operation at the point AFTER the provider
// finished, which is where rebind production begins.
func (f *fakeOperationStore) markProviderExecution(operationID, executionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	operation, ok := f.operations[operationID]
	if !ok {
		return
	}
	operation.ProviderExecutionID = executionID
	operation.State = domain.OperationProviderVerified
	f.operations[operationID] = operation
}

// markMemberExecution places one member at a given execution and state.
func (f *fakeOperationStore) markMemberExecution(
	operationID, dependent, executionID string, state domain.DependencyMemberState,
) {
	f.mu.Lock()
	defer f.mu.Unlock()

	operation, ok := f.operations[operationID]
	if !ok {
		return
	}
	for index := range operation.Members {
		if operation.Members[index].Dependent != dependent {
			continue
		}
		operation.Members[index].ExecutionID = executionID
		operation.Members[index].State = state
	}
	f.operations[operationID] = operation
}
