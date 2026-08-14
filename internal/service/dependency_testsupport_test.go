package service_test

import (
	"context"
	"errors"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Test doubles for the namespace evidence the recreation path consults.
//
// # Why the recreation tests need one at all
//
// The execution service fails CLOSED when its dependency evidence is missing:
// "HarborMaster cannot establish what shares this container's namespace" and
// "nothing shares this container's namespace" are opposite facts, and only the
// second permits a stop. So a harness that wires no evidence gets every
// recreation refused, which is correct behaviour and useless as a fixture.
//
// standaloneDependencies is therefore the explicit statement that the container
// under test is an ORDINARY one: nothing shares its namespace, and it shares
// nobody's. Tests that are about namespace sharing use the other two.

// standaloneDependencies reports a container that shares no namespace and whose
// namespace nothing shares.
//
// EnsureOperation returns isProvider=false, which is what makes the ordinary
// recreation path unchanged: no operation is recorded for a container nothing
// depends on.
type standaloneDependencies struct{}

func (standaloneDependencies) EnsureOperation(
	context.Context, string, string, domain.Requester,
) (string, bool, error) {
	return "", false, nil
}

func (standaloneDependencies) AttachProviderExecution(context.Context, string, string) error {
	return nil
}

func (blockingDependencies) EnsureOperation(
	context.Context, string, string, domain.Requester,
) (string, bool, error) {
	// Never reached: invariant A refuses this container in the preflight, well
	// before the operation would be recorded. Returning an error rather than a
	// success makes a regression visible rather than silently passing.
	return "", true, errors.New("this provider's dependents cannot be reattached")
}

func (blockingDependencies) AttachProviderExecution(context.Context, string, string) error {
	return nil
}

func (unavailableDependencies) EnsureOperation(
	context.Context, string, string, domain.Requester,
) (string, bool, error) {
	return "", false, errors.New("the dependency graph could not be built")
}

func (unavailableDependencies) AttachProviderExecution(context.Context, string, string) error {
	return errors.New("the dependency graph could not be built")
}

// providerDependencies is a container whose dependents CAN be reattached.
//
// Invariant A passes, so the pipeline proceeds to the operation record -- which
// is exactly where the persistence failure boundaries live. `ensureErr` is what
// each of those tests injects.
type providerDependencies struct {
	operationID string
	ensureErr   error
	// attachErr fails the execution link only. It must NOT refuse: the member
	// set is already durable, which is the property that matters.
	attachErr error
}

func (providerDependencies) ResolveNamespaceProvider(_ context.Context, capturedID string) (string, error) {
	return capturedID, nil
}

func (providerDependencies) AssessProvider(
	_ context.Context, provider string,
) (domain.ProviderRebindAssessment, bool, error) {
	// A provider whose one dependent passed. MayStop is true, so the preflight
	// clears and the pipeline reaches the persistence step.
	return domain.ProviderRebindAssessment{
		Provider: provider, Rebindable: []string{"sonarr"},
	}, true, nil
}

func (p providerDependencies) EnsureOperation(
	context.Context, string, string, domain.Requester,
) (string, bool, error) {
	if p.ensureErr != nil {
		return "", true, p.ensureErr
	}
	id := p.operationID
	if id == "" {
		id = "depop_0123456789abcdef0123"
	}
	return id, true, nil
}

func (p providerDependencies) AttachProviderExecution(context.Context, string, string) error {
	return p.attachErr
}

func (standaloneDependencies) ResolveNamespaceProvider(_ context.Context, capturedID string) (string, error) {
	// Identity. A provider that is still live resolves to itself, which is what
	// makes the ordinary recreation path a no-op rather than a special case.
	return capturedID, nil
}

func (standaloneDependencies) AssessProvider(
	_ context.Context,
	provider string,
) (domain.ProviderRebindAssessment, bool, error) {
	// isProvider = false: nothing shares this container's namespace.
	return domain.ProviderRebindAssessment{Provider: provider}, false, nil
}

// blockingDependencies reports a provider whose dependent cannot be reattached.
//
// The invariant A refusal, in the shape the live experiment produced: a
// dependent HarborMaster cannot pin, so stopping the provider would break it
// with no way to repair it.
type blockingDependencies struct {
	dependent string
	refusal   domain.RebindRefusal
}

func (blockingDependencies) ResolveNamespaceProvider(_ context.Context, capturedID string) (string, error) {
	return capturedID, nil
}

func (b blockingDependencies) AssessProvider(
	_ context.Context,
	provider string,
) (domain.ProviderRebindAssessment, bool, error) {
	refusal := b.refusal
	if refusal == domain.RebindRefusalNone {
		refusal = domain.RebindRefusalDigestUnestablished
	}
	dependent := b.dependent
	if dependent == "" {
		dependent = "sonarr"
	}
	return domain.ProviderRebindAssessment{
		Provider: provider,
		Blocked:  []domain.ProviderBlock{{Dependent: dependent, Refusal: refusal}},
	}, true, nil
}

// unavailableDependencies reports that the check could not be performed.
//
// The case that must NOT be read as passing. An assessment that errored
// establishes nothing.
type unavailableDependencies struct{}

func (unavailableDependencies) ResolveNamespaceProvider(context.Context, string) (string, error) {
	return "", errors.New("the dependency graph could not be built")
}

func (unavailableDependencies) AssessProvider(
	context.Context,
	string,
) (domain.ProviderRebindAssessment, bool, error) {
	return domain.ProviderRebindAssessment{}, false, errors.New("the dependency graph could not be built")
}

// Compile-time proof that each double satisfies the interface the service
// depends on, so a change to that interface breaks here rather than at a call
// site.
var (
	_ service.ExecutionDependencies = standaloneDependencies{}
	_ service.ExecutionDependencies = blockingDependencies{}
	_ service.ExecutionDependencies = unavailableDependencies{}
	_ service.ExecutionDependencies = providerDependencies{}
)
