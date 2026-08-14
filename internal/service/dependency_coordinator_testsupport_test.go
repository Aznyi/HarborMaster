package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The fixture the coordinator tests are built on.
//
// # Why the stores are separate objects from the service
//
// Every store below OUTLIVES the service that reads it, and `restart()` builds a
// brand new DependencyService over the same stores while sharing no field with
// the old one. That is what makes "recovery needs no process-local state" a
// property this file can actually demonstrate rather than assert.
//
// The stores are deliberately dumb. They hold rows and hand them back; every
// decision under test lives in the production code.

// Full 64-hex container ids, in the shape Docker actually records.
var (
	providerAID  = strings.Repeat("a", 64) // the provider before the update
	providerBID  = strings.Repeat("b", 64) // the verified replacement
	providerCID  = strings.Repeat("c", 64) // a SECOND replacement, for case 9D
	dependentID  = strings.Repeat("d", 64)
	dependent2ID = strings.Repeat("e", 64)
	newDependent = strings.Repeat("f", 64) // an externally recreated dependent
)

const (
	depProvider   = "gluetun"
	depDependent  = "sonarr"
	depDependent2 = "radarr"
	depDigestX    = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	depDigestY    = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	depImageRef   = "linuxserver/sonarr:4.0.0"
	depExecutionA = "exec_0123456789abcdef0aa0"
)

// ---------------------------------------------------------------- stores --

// fakeDependencyStore holds the inventory projection the graph is derived from.
type fakeDependencyStore struct {
	mu        sync.Mutex
	rows      []domain.ContainerNamespaceRow
	endpoints []domain.DependencyEndpoint
	operator  []domain.WorkloadDependency
}

func (f *fakeDependencyStore) NamespaceRows(context.Context) ([]domain.ContainerNamespaceRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ContainerNamespaceRow(nil), f.rows...), nil
}

func (f *fakeDependencyStore) Endpoints(context.Context) ([]domain.DependencyEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.DependencyEndpoint(nil), f.endpoints...), nil
}

func (f *fakeDependencyStore) OperatorDependencies(context.Context) ([]domain.WorkloadDependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.WorkloadDependency(nil), f.operator...), nil
}

func (f *fakeDependencyStore) OperatorDependencyCount(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.operator), nil
}

func (f *fakeDependencyStore) Get(context.Context, string) (domain.WorkloadDependency, error) {
	return domain.WorkloadDependency{}, store.ErrNotFound
}

func (f *fakeDependencyStore) Create(
	_ context.Context, edge domain.WorkloadDependency, _ time.Time,
) (domain.WorkloadDependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operator = append(f.operator, edge)
	return edge, nil
}

func (f *fakeDependencyStore) Delete(context.Context, string) error { return store.ErrNotFound }

// setNamespace replaces one container's declared network mode.
//
// The TOCTOU lever: every 9x case below changes the world through this or
// setEndpoint and then asks the coordinator what it makes of the world NOW.
func (f *fakeDependencyStore) setNamespace(name, containerID, mode string, observed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].Name == name {
			f.rows[i].ContainerID = containerID
			f.rows[i].Modes = domain.NamespaceModes{Network: mode, Observed: observed}
			return
		}
	}
	f.rows = append(f.rows, domain.ContainerNamespaceRow{
		ContainerID: containerID, Name: name,
		Modes: domain.NamespaceModes{Network: mode, Observed: observed},
	})
}

// setModes replaces all three namespace modes, for the IPC and PID cases.
func (f *fakeDependencyStore) setModes(name, containerID string, modes domain.NamespaceModes) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].Name == name {
			f.rows[i].ContainerID = containerID
			f.rows[i].Modes = modes
			return
		}
	}
	f.rows = append(f.rows, domain.ContainerNamespaceRow{
		ContainerID: containerID, Name: name, Modes: modes,
	})
}

// removeContainer takes a container out of the estate entirely.
func (f *fakeDependencyStore) removeContainer(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.rows[:0]
	for _, row := range f.rows {
		if row.Name != name {
			rows = append(rows, row)
		}
	}
	f.rows = rows

	endpoints := f.endpoints[:0]
	for _, endpoint := range f.endpoints {
		if endpoint.Name != name {
			endpoints = append(endpoints, endpoint)
		}
	}
	f.endpoints = endpoints
}

// namespaceIDOf returns the id a name currently holds, or "".
func (f *fakeDependencyStore) namespaceIDOf(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Name == name {
			return row.ContainerID
		}
	}
	return ""
}

// mutateEndpoint applies a change to one endpoint.
func (f *fakeDependencyStore) mutateEndpoint(name string, apply func(*domain.DependencyEndpoint)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.endpoints {
		if f.endpoints[i].Name == name {
			apply(&f.endpoints[i])
			return
		}
	}
}

// fakeRebindPlanStore holds change plans.
type fakeRebindPlanStore struct {
	mu       sync.Mutex
	plans    map[string]domain.ChangePlan
	inserted int
	// getErr makes a read FAIL rather than report absence. Case F depends on
	// the difference: unreadable is not evidence of absence.
	getErr error
	// gatheredRefs records every image reference the coordinator asked for, so
	// a test can assert it asked in the form the repository can answer.
	gatheredRefs []string
}

func newFakeRebindPlanStore() *fakeRebindPlanStore {
	return &fakeRebindPlanStore{plans: make(map[string]domain.ChangePlan)}
}

func (f *fakeRebindPlanStore) InsertPlans(
	_ context.Context, plans []domain.ChangePlan, _ time.Time,
) (store.InsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, plan := range plans {
		f.plans[plan.PlanID] = plan
		f.inserted++
	}
	return store.InsertResult{Inserted: len(plans)}, nil
}

func (f *fakeRebindPlanStore) Get(_ context.Context, planID string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return domain.ChangePlan{}, f.getErr
	}
	plan, ok := f.plans[planID]
	if !ok {
		return domain.ChangePlan{}, store.ErrNotFound
	}
	return plan, nil
}

// GatherInputs answers the way the REAL repository answers.
//
// # Why the reference argument is honoured rather than ignored
//
// It used to be `_ []string`. The real repository queries `image_intel` by the
// reference it is given and keys the result by the stored CANONICAL form, so a
// caller passing a container's raw `alpine:3.22.1` gets nothing back. The
// coordinator was doing exactly that, and this fake -- being indifferent to the
// argument -- returned a usable batch anyway.
//
// The consequence shipped: every rebind plan was assessed with its registry
// evidence missing, the model reported "cannot advise", and the acquisition
// service refused to reattach anything. Found live in Stage 5, against a real
// daemon, three layers from where the mistake was.
//
// So this fake now only answers for a CANONICAL reference, which is what the
// real one does.
func (f *fakeRebindPlanStore) GatherInputs(
	_ context.Context, containerIDs []string, imageRefs []string,
) (store.PlanBatchInputs, error) {
	f.mu.Lock()
	f.gatheredRefs = append(f.gatheredRefs, imageRefs...)
	f.mu.Unlock()

	inputs := store.PlanBatchInputs{
		Drift:        make(map[string]store.SeverityRollup),
		Policy:       make(map[string]store.SeverityRollup),
		Baselines:    make(map[string]store.BaselineRollup),
		Intel:        make(map[string]domain.ImageIntel),
		Fingerprints: make(map[string]string),
	}
	for _, id := range containerIDs {
		// A snapshot exists and restoration is ready: the ordinary state of a
		// managed container, and the one that lets a plan be assessed rather
		// than declined for a reason unrelated to what is under test.
		inputs.Baselines[id] = store.BaselineRollup{
			SnapshotID: 1, Readiness: domain.ReadinessReady,
		}
	}
	for _, reference := range imageRefs {
		// Keyed on what was asked for, exactly as the repository keys on what it
		// read back. A raw reference matches no row and produces no record.
		normalised, err := domain.NormalizeImageRef(reference)
		if err != nil || normalised.Canonical != reference {
			continue
		}
		inputs.Intel[reference] = domain.ImageIntel{
			Reference: reference,
			Status:    domain.CheckOK,
		}
	}
	return inputs, nil
}

// gathered returns every reference the coordinator asked for.
func (f *fakeRebindPlanStore) gathered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gatheredRefs...)
}

func (f *fakeRebindPlanStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserted
}

// fakeDependencyLineage reports what each container is running.
type fakeDependencyLineage struct {
	mu      sync.Mutex
	running map[string][2]string // container id -> {reference, digest}
}

func (f *fakeDependencyLineage) RunningDigestFor(
	_ context.Context, containerID string,
) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pair, ok := f.running[containerID]
	if !ok {
		return "", "", store.ErrNotFound
	}
	return pair[0], pair[1], nil
}

func (f *fakeDependencyLineage) set(containerID, reference, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[containerID] = [2]string{reference, digest}
}

// --------------------------------------------------------------- harness --

// depHarness owns the stores. The SERVICE is built on demand, so a test can
// discard one and build another over the same rows.
type depHarness struct {
	store      *fakeDependencyStore
	operations *fakeOperationStore
	plans      *fakeRebindPlanStore
	lineage    *fakeDependencyLineage
	executions fakeExecutions
	self       domain.SelfIdentity
}

// newDepHarness builds the baseline world: gluetun on B, sonarr still declaring
// the OLD provider id A, everything else healthy.
//
// That is the exact situation a rebind exists for -- the provider has been
// replaced and the dependent is still bound to the namespace that went with it.
func newDepHarness() *depHarness {
	dependencyStore := &fakeDependencyStore{}
	// The provider, on its NEW identity.
	dependencyStore.setNamespace(depProvider, providerBID, "bridge", true)
	// The dependent, still naming the OLD one.
	dependencyStore.setNamespace(depDependent, dependentID, "container:"+providerAID, true)

	dependencyStore.endpoints = []domain.DependencyEndpoint{
		{Name: depProvider, ContainerID: providerBID, ImageRef: "qmcgaw/gluetun:v3", Present: true},
		{Name: depDependent, ContainerID: dependentID, ImageRef: depImageRef, Present: true},
	}

	lineage := &fakeDependencyLineage{running: map[string][2]string{
		dependentID:  {depImageRef, depDigestX},
		dependent2ID: {depImageRef, depDigestX},
		newDependent: {depImageRef, depDigestX},
	}}

	return &depHarness{
		store:      dependencyStore,
		operations: newFakeOperationStore(),
		plans:      newFakeRebindPlanStore(),
		lineage:    lineage,
		executions: fakeExecutions{
			records: map[string]domain.Execution{
				depExecutionA: {State: domain.ExecutionSucceeded},
			},
			replacements: map[string]string{},
		},
	}
}

// service builds a FRESH DependencyService over the harness's stores.
//
// Called once per logical "process". A test that wants to prove recovery needs
// no memory calls it twice and never reuses the first result.
func (h *depHarness) service() *service.DependencyService {
	return service.NewDependencyService(service.DependencyOptions{
		Store:      h.store,
		Lineage:    h.lineage,
		Operations: h.operations,
		Executions: h.executions,
		Plans:      h.plans,
		Self:       fixedSelf{identity: h.self},
		Now:        func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) },
	})
}

// beforeReplacement rewinds the estate to the moment BEFORE the provider was
// recreated.
//
// # Why the two phases need different worlds, and why that is not a fixture
// # detail
//
// EnsureOperation runs before the provider is stopped: the provider is on id A
// and the dependent's `container:A` reference RESOLVES, so discovery produces an
// EDGE and the provider has hard dependents.
//
// ProduceRebindPlans runs after: the provider is on id B and the dependent's
// `container:A` no longer resolves, so discovery produces a PROBLEM instead --
// which is precisely the stale-binding signal a rebind exists for.
//
// A fixture that used one world for both would be testing a state that never
// occurs. `newDepHarness` gives the AFTER world; this gives the BEFORE one.
func (h *depHarness) beforeReplacement() *depHarness {
	h.store.setNamespace(depProvider, providerAID, "bridge", true)
	h.store.mutateEndpoint(depProvider, func(e *domain.DependencyEndpoint) {
		e.ContainerID = providerAID
	})
	return h
}

// afterReplacement advances the estate to the moment AFTER the provider was
// recreated: it now holds a new id, and every dependent's reference is stale.
func (h *depHarness) afterReplacement(providerID string) *depHarness {
	// The execution record HarborMaster would have written: this id replaced
	// that one. Without it there is nothing to resolve a dependent's captured
	// `container:<old>` reference through, which is exactly the state the real
	// system was in before Stage 5a.
	if previous := h.store.namespaceIDOf(depProvider); previous != "" && previous != providerID {
		h.executions.replacements[previous] = providerID
	}
	h.store.setNamespace(depProvider, providerID, "bridge", true)
	h.store.mutateEndpoint(depProvider, func(e *domain.DependencyEndpoint) {
		e.ContainerID = providerID
	})
	return h
}

// seedOperation records the operation the way EnsureOperation would, with the
// provider's execution already verified.
//
// Used by the TOCTOU cases, which are about what happens AFTER the provider
// succeeded. The production-path recovery tests call EnsureOperation itself.
func (h *depHarness) seedOperation() domain.DependencyOperation {
	operation, _ := h.operations.Create(context.Background(), domain.DependencyOperation{
		Provider:            depProvider,
		ProviderExecutionID: depExecutionA,
		State:               domain.OperationProviderVerified,
		Members: []domain.DependencyMember{{
			Dependent:          depDependent,
			Provider:           depProvider,
			Source:             domain.DependencyNetworkNamespace,
			ExpectedProviderID: providerAID,
			State:              domain.MemberPending,
		}},
	}, time.Now().UTC())
	return operation
}

// memberOf reads one member back from the store.
func (h *depHarness) memberOf(operationID, dependent string) (domain.DependencyMember, bool) {
	operation, err := h.operations.Get(context.Background(), operationID)
	if err != nil {
		return domain.DependencyMember{}, false
	}
	for _, member := range operation.Members {
		if member.Dependent == dependent {
			return member, true
		}
	}
	return domain.DependencyMember{}, false
}

// fixedSelf reports a constant identity.
type fixedSelf struct{ identity domain.SelfIdentity }

func (f fixedSelf) Identity() domain.SelfIdentity { return f.identity }

// errUnreadable stands in for a read that failed rather than found nothing.
var errUnreadable = errors.New("the plan row could not be read")
