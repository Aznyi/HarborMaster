package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The automatic-updates switch (C1).
//
// # What these tests are really defending
//
// The switch is a facade over ONE ordinary update policy. Everything that could
// go wrong with that idea is a safety property rather than a feature:
//
//   - it must not become a second authorisation path;
//   - it must not reinterpret a policy an operator wrote;
//   - turning it off must delete nothing;
//   - it must lose every precedence contest against a user policy;
//   - it must not touch a container by being switched.
//
// The precedence tests run the REAL domain.SelectUpdatePolicy rather than
// restating its ordering, because the whole safety argument is that the managed
// policy loses to that function, not to a description of it.

// ------------------------------------------------------------ fake store --

// simpleStore is an in-memory UpdatePolicyStore.
//
// It records mutations so a test can assert that reading the switch writes
// nothing, and that turning it off writes to the MANAGED policy alone.
type simpleStore struct {
	policies map[string]domain.UpdatePolicy
	order    []string

	created  []string
	applied  []string
	archived []string
}

func newSimpleStore() *simpleStore {
	return &simpleStore{policies: map[string]domain.UpdatePolicy{}}
}

func (s *simpleStore) put(p domain.UpdatePolicy) {
	if _, seen := s.policies[p.PolicyID]; !seen {
		s.order = append(s.order, p.PolicyID)
	}
	s.policies[p.PolicyID] = p
}

func (s *simpleStore) CreateUpdatePolicy(
	_ context.Context, policy domain.UpdatePolicy, _ time.Time,
) (domain.UpdatePolicy, error) {
	if _, clash := s.policies[policy.PolicyID]; clash {
		return domain.UpdatePolicy{}, errors.New("policy id already exists")
	}
	s.created = append(s.created, policy.PolicyID)
	s.put(policy)
	return policy, nil
}

func (s *simpleStore) ApplyUpdatePolicy(
	_ context.Context, policyID string, change store.UpdatePolicyChange, _ time.Time,
) (domain.UpdatePolicy, error) {
	current, ok := s.policies[policyID]
	if !ok {
		return domain.UpdatePolicy{}, store.ErrNotFound
	}
	if current.Archived {
		return domain.UpdatePolicy{}, store.ErrNotFound
	}
	s.applied = append(s.applied, policyID)

	if change.Name != nil {
		current.Name = *change.Name
	}
	if change.Description != nil {
		current.Description = *change.Description
	}
	if change.Enabled != nil {
		current.Enabled = *change.Enabled
	}
	if change.Priority != nil {
		current.Priority = *change.Priority
	}
	if change.Scope != nil {
		current.Scope = *change.Scope
	}
	if change.Selector != nil {
		current.Selector = *change.Selector
	}
	if change.Strategy != nil {
		current.Strategy = *change.Strategy
	}
	if change.MinimumRecommendation != nil {
		current.MinimumRecommendation = *change.MinimumRecommendation
	}
	if change.Mode != nil {
		current.Mode = *change.Mode
	}
	if change.Window != nil {
		current.Window = *change.Window
	}
	if change.Failure != nil {
		current.Failure = *change.Failure
	}
	s.put(current)
	return current, nil
}

func (s *simpleStore) ArchiveUpdatePolicy(_ context.Context, policyID string, _ time.Time) error {
	current, ok := s.policies[policyID]
	if !ok {
		return store.ErrNotFound
	}
	s.archived = append(s.archived, policyID)
	current.Archived = true
	current.Enabled = false
	s.put(current)
	return nil
}

func (s *simpleStore) UpdatePolicyByID(
	_ context.Context, policyID string,
) (domain.UpdatePolicy, error) {
	if p, ok := s.policies[policyID]; ok {
		return p, nil
	}
	return domain.UpdatePolicy{}, store.ErrNotFound
}

func (s *simpleStore) ListUpdatePolicies(
	_ context.Context, _ store.UpdatePolicyFilter,
) ([]domain.UpdatePolicy, int, error) {
	out := s.all()
	return out, len(out), nil
}

func (s *simpleStore) ActivePolicies(_ context.Context) ([]domain.UpdatePolicy, error) {
	var out []domain.UpdatePolicy
	for _, p := range s.all() {
		if p.Enabled && !p.Archived {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *simpleStore) CountUpdatePolicies(_ context.Context) (int, int, error) {
	total, enabled := 0, 0
	for _, p := range s.all() {
		total++
		if p.Enabled && !p.Archived {
			enabled++
		}
	}
	return total, enabled, nil
}

func (s *simpleStore) all() []domain.UpdatePolicy {
	out := make([]domain.UpdatePolicy, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.policies[id])
	}
	return out
}

// mutations counts every write the store saw.
func (s *simpleStore) mutations() int {
	return len(s.created) + len(s.applied) + len(s.archived)
}

func simpleService(st *simpleStore) *service.UpdatePolicyService {
	return service.NewUpdatePolicyService(service.UpdatePolicyOptions{
		Store:  st,
		Limits: domain.DefaultUpdatePolicyLimits(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
	})
}

// userPolicy is a policy an operator wrote.
func userPolicy(name string, mutate ...func(*domain.UpdatePolicy)) domain.UpdatePolicy {
	p := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  name,
		Enabled:               true,
		Priority:              0,
		Scope:                 domain.ScopeSelector,
		Selector:              domain.UpdateSelector{Include: []string{"web"}},
		Strategy:              domain.StrategyMinor,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  domain.ModeObserve,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	for _, m := range mutate {
		m(&p)
	}
	p.Normalise()
	return p
}

// ------------------------------------------------------------- the switch --

func TestAFreshInstallationHasTheSwitchOff(t *testing.T) {
	st := newSimpleStore()
	state, err := simpleService(st).SimpleUpdates(context.Background())
	if err != nil {
		t.Fatalf("SimpleUpdates: %v", err)
	}
	if state.Enabled {
		t.Error("a fresh installation reports automatic updates as on")
	}
	if state.Configured {
		t.Error("a fresh installation reports the switch as configured")
	}
	if st.mutations() != 0 {
		t.Errorf("reading the switch wrote %d times", st.mutations())
	}
}

func TestEnablingWritesExactlyOneManagedPolicy(t *testing.T) {
	st := newSimpleStore()
	svc := simpleService(st)

	result, err := svc.EnableSimpleUpdates(context.Background(), service.Actor{})
	if err != nil {
		t.Fatalf("EnableSimpleUpdates: %v", err)
	}

	if len(st.created) != 1 || st.created[0] != domain.SimpleUpdatesPolicyID {
		t.Fatalf("created = %v, want exactly the reserved id", st.created)
	}
	p := result.Policy
	// Every one of these is a semantic the UI promises. They are asserted
	// rather than described so the promise cannot drift from the policy.
	if p.Mode != domain.ModeAutomatic {
		t.Errorf("mode = %q, want automatic", p.Mode)
	}
	if p.Strategy != domain.StrategyPatch {
		t.Errorf("strategy = %q, want patch (no minor, no major)", p.Strategy)
	}
	if p.Scope != domain.ScopeAllEligible {
		t.Errorf("scope = %q, want allEligible", p.Scope)
	}
	if !p.Failure.AutoRollback {
		t.Error("automatic rollback is off; the UI says automation puts a failed update back")
	}
	if p.MinimumRecommendation != domain.RecommendCaution {
		t.Errorf("recommendation floor = %q", p.MinimumRecommendation)
	}
	if p.Priority != 0 {
		t.Errorf("priority = %d, want the minimum so user policies outrank it", p.Priority)
	}
	if len(result.Warnings) == 0 {
		t.Error("the managed policy earned no warnings, so the confirmation has nothing honest to show")
	}
}

func TestEnablingIsIdempotentAndCreatesNoSecondPolicy(t *testing.T) {
	st := newSimpleStore()
	svc := simpleService(st)
	ctx := context.Background()

	if _, err := svc.EnableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	if _, err := svc.EnableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("second enable: %v", err)
	}

	if len(st.created) != 1 {
		t.Errorf("created %d policies, want 1", len(st.created))
	}
	if len(st.all()) != 1 {
		t.Errorf("store holds %d policies, want 1", len(st.all()))
	}
}

func TestDisablingLeavesTheRowAndDeletesNothing(t *testing.T) {
	st := newSimpleStore()
	svc := simpleService(st)
	ctx := context.Background()

	if _, err := svc.EnableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := svc.DisableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	stored, err := st.UpdatePolicyByID(ctx, domain.SimpleUpdatesPolicyID)
	if err != nil {
		t.Fatalf("the managed policy was removed: %v", err)
	}
	if stored.Enabled {
		t.Error("the managed policy is still in force after being turned off")
	}
	if stored.Archived {
		t.Error("the managed policy was archived; archiving it would make the switch unrepairable")
	}
	if len(st.archived) != 0 {
		t.Errorf("archived %v, want nothing archived", st.archived)
	}

	// And it can come back on.
	if _, err := svc.EnableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	again, _ := st.UpdatePolicyByID(ctx, domain.SimpleUpdatesPolicyID)
	if !again.Enabled {
		t.Error("the switch could not be turned back on")
	}
}

func TestTurningOffASwitchThatWasNeverOnSucceeds(t *testing.T) {
	st := newSimpleStore()
	if _, err := simpleService(st).DisableSimpleUpdates(context.Background(), service.Actor{}); err != nil {
		t.Fatalf("DisableSimpleUpdates: %v", err)
	}
	if st.mutations() != 0 {
		t.Errorf("turning off an absent switch wrote %d times", st.mutations())
	}
}

// ------------------------------------------------------- user policies --

func TestTheSwitchNeverTouchesAPolicyAnOperatorWrote(t *testing.T) {
	st := newSimpleStore()
	mine := userPolicy("my careful rule")
	st.put(mine)
	svc := simpleService(st)
	ctx := context.Background()

	if _, err := svc.EnableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := svc.DisableSimpleUpdates(ctx, service.Actor{}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	after, err := st.UpdatePolicyByID(ctx, mine.PolicyID)
	if err != nil {
		t.Fatalf("the user policy disappeared: %v", err)
	}
	if after.Enabled != mine.Enabled || after.Mode != mine.Mode ||
		after.Strategy != mine.Strategy || after.Priority != mine.Priority ||
		after.Archived != mine.Archived || after.Name != mine.Name {
		t.Errorf("the user policy changed:\n before %+v\n after  %+v", mine, after)
	}
	// Every write the store saw named the managed policy and nothing else.
	for _, id := range append(append([]string{}, st.created...), st.applied...) {
		if !domain.IsSimpleUpdatesPolicy(id) {
			t.Errorf("the switch wrote to %s, which it does not own", id)
		}
	}
}

func TestTheSwitchReportsThePoliciesThatOutrankIt(t *testing.T) {
	st := newSimpleStore()
	st.put(userPolicy("my careful rule"))
	svc := simpleService(st)

	state, err := svc.SimpleUpdates(context.Background())
	if err != nil {
		t.Fatalf("SimpleUpdates: %v", err)
	}
	if len(state.OverriddenBy) != 1 || state.OverriddenBy[0].Name != "my careful rule" {
		t.Errorf("overriddenBy = %+v, want the operator's rule", state.OverriddenBy)
	}
}

// --------------------------------------------------------- precedence --
//
// Run through the REAL selection function. The safety argument is that the
// managed policy loses to domain.SelectUpdatePolicy, so that is what is called.

// eligibleTarget is an ordinary workload HarborMaster established it could act
// on. `Recreatable` is REQUIRED for broad selection: the zero value selects
// nothing, which is why it has to be stated rather than assumed.
func eligibleTarget(name string) domain.SelectionTarget {
	return domain.SelectionTarget{
		Name:        name,
		Image:       "nginx:1.27.0",
		Eligibility: domain.TargetEligibility{Recreatable: true},
	}
}

func selected(t *testing.T, policies []domain.UpdatePolicy, name string) domain.UpdatePolicy {
	t.Helper()
	target := eligibleTarget(name)
	chosen, ok := domain.SelectUpdatePolicy(policies, target, domain.SelfIdentity{})
	if !ok {
		t.Fatalf("no policy governs %q", name)
	}
	return chosen
}

func TestAnyUserPolicyOutranksTheManagedOne(t *testing.T) {
	managed := domain.SimpleUpdatesPolicy()
	managed.Normalise()

	for _, testCase := range []struct {
		name string
		user domain.UpdatePolicy
	}{
		{
			// The ordinary case: a narrow rule at the same priority. Narrow
			// beats broad, which is the second ordering key.
			name: "a narrow policy at the same priority",
			user: userPolicy("narrow"),
		},
		{
			// The dangerous case, and the reason the reserved id is all-`f`.
			// Two BROAD policies at the SAME priority fall through to the id
			// comparison, and the largest id loses.
			name: "a broad policy at the same priority",
			user: userPolicy("broad", func(p *domain.UpdatePolicy) {
				p.Scope = domain.ScopeAllEligible
				p.Selector = domain.UpdateSelector{}
			}),
		},
		{
			name: "a higher-priority policy",
			user: userPolicy("higher", func(p *domain.UpdatePolicy) { p.Priority = 10 }),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			chosen := selected(t, []domain.UpdatePolicy{managed, testCase.user}, "web")
			if domain.IsSimpleUpdatesPolicy(chosen.PolicyID) {
				t.Errorf("the managed policy beat %q; a switch must never reinterpret "+
					"a rule an operator wrote", testCase.user.Name)
			}
			// And the order the policies arrive in cannot change the answer.
			reversed := selected(t, []domain.UpdatePolicy{testCase.user, managed}, "web")
			if reversed.PolicyID != chosen.PolicyID {
				t.Error("the winner depends on the order the policies were loaded in")
			}
		})
	}
}

func TestTheManagedPolicyStillGovernsWhatNothingElseClaims(t *testing.T) {
	managed := domain.SimpleUpdatesPolicy()
	managed.Normalise()
	// The operator's rule names "web"; this container is not it.
	chosen := selected(t, []domain.UpdatePolicy{managed, userPolicy("narrow")}, "database")
	if !domain.IsSimpleUpdatesPolicy(chosen.PolicyID) {
		t.Errorf("chosen = %q, want the managed policy to cover the containers "+
			"no user policy claims", chosen.Name)
	}
}

func TestADisabledSwitchGovernsNothing(t *testing.T) {
	managed := domain.SimpleUpdatesPolicy()
	managed.Enabled = false
	managed.Normalise()

	if _, ok := domain.SelectUpdatePolicy(
		[]domain.UpdatePolicy{managed}, eligibleTarget("database"), domain.SelfIdentity{}); ok {
		t.Error("a switch that is off still governs a container")
	}
}

// --------------------------------------------------------- exclusions --
//
// The switch inherits every exclusion rather than restating one. These assert
// the inheritance, through the same Governs the engine calls.

func TestTheManagedPolicyInheritsEveryBroadScopeExclusion(t *testing.T) {
	managed := domain.SimpleUpdatesPolicy()
	managed.Normalise()

	// The control: an ordinary workload IS governed. Without this the two
	// exclusion cases below would pass even if nothing were excluded.
	if !managed.Governs(eligibleTarget("ordinary"), domain.SelfIdentity{}) {
		t.Fatal("the managed policy governs no ordinary container, so the exclusions below prove nothing")
	}

	self := domain.SelfIdentity{ContainerName: "harbormaster"}
	for _, testCase := range []struct {
		name   string
		target domain.SelectionTarget
		self   domain.SelfIdentity
	}{
		{
			name: "HarborMaster's own container",
			target: func() domain.SelectionTarget {
				t := eligibleTarget("harbormaster")
				t.Image = "harbormaster:local"
				return t
			}(),
			self: self,
		},
		{
			name: "a container opted out by label",
			target: func() domain.SelectionTarget {
				t := eligibleTarget("opted-out")
				t.Labels = map[string]string{domain.LabelHarborMasterEnabled: "false"}
				t.Eligibility.OptedOut = true
				return t
			}(),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if managed.Governs(testCase.target, testCase.self) {
				t.Error("the managed policy governs a container the broad scope refuses")
			}
		})
	}
}

// ---------------------------------------------------------- identity --

func TestTheReservedIdIsWellFormedAndUnreachableByGeneration(t *testing.T) {
	if !domain.ValidUpdatePolicyID(domain.SimpleUpdatesPolicyID) {
		t.Fatal("the reserved id is not a well-formed policy id, so the API would refuse it")
	}
	if !domain.IsSimpleUpdatesPolicy(domain.SimpleUpdatesPolicyID) {
		t.Error("the reserved id is not recognised as the managed one")
	}
	if domain.IsSimpleUpdatesPolicy(domain.NewUpdatePolicyID()) {
		t.Error("a generated id was mistaken for the managed one")
	}
	// The reservation is structural: generation may never return it.
	for i := 0; i < 2000; i++ {
		if domain.NewUpdatePolicyID() == domain.SimpleUpdatesPolicyID {
			t.Fatal("generation produced the reserved id")
		}
	}
}

func TestTheManagedPolicyIsIdentifiedByIdRatherThanName(t *testing.T) {
	// An operator may legitimately name their own rule the same thing. Name
	// matching as an ownership mechanism would hand them the switch's row.
	impostor := userPolicy(domain.SimpleUpdatesPolicyName)
	if domain.IsSimpleUpdatesPolicy(impostor.PolicyID) {
		t.Error("a user policy sharing the display name was treated as the managed one")
	}
}
