package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// estate builds the validation index from plain names.
func estate(names ...string) map[string]domain.DependencyEndpoint {
	endpoints := make([]domain.DependencyEndpoint, 0, len(names))
	for _, name := range names {
		endpoints = append(endpoints, domain.DependencyEndpoint{
			Name:        name,
			ContainerID: strings.Repeat("a", 63) + string(rune('0'+len(name)%10)),
			ImageRef:    "alpine:3.22",
			Present:     true,
		})
	}
	return domain.EndpointsFromNames(endpoints)
}

func validationInput(dependent, dependency string) domain.DependencyValidationInput {
	return domain.DependencyValidationInput{
		Dependent:  dependent,
		Dependency: dependency,
		Source:     domain.DependencyOperator,
		Containers: estate("api", "postgres", "worker"),
	}
}

// The accepted case, and what it produces.
func TestAnOperatorRelationshipIsAccepted(t *testing.T) {
	t.Parallel()

	edge, refusal := domain.ValidateOperatorDependency(validationInput("api", "postgres"))
	if refusal != domain.DependencyRefusalNone {
		t.Fatalf("refused with %q", refusal)
	}
	if edge.Dependent != "api" || edge.Dependency != "postgres" {
		t.Fatalf("edge = %s -> %s", edge.Dependent, edge.Dependency)
	}
	if edge.Source != domain.DependencyOperator {
		t.Fatalf("source = %q", edge.Source)
	}
	// An operator relationship constrains order; it is not a runtime
	// requirement, so it must never claim to be one.
	if edge.Hard() {
		t.Fatal("an operator relationship must not be hard")
	}
}

// Every refusal the phase brief names, in one table.
func TestOperatorRelationshipRefusals(t *testing.T) {
	t.Parallel()

	selfIdentity := domain.SelfIdentity{ContainerName: "harbormaster"}

	cases := []struct {
		name  string
		input domain.DependencyValidationInput
		want  domain.DependencyRefusal
	}{
		{
			name:  "self dependency",
			input: validationInput("api", "api"),
			want:  domain.DependencyRefusalSelf,
		},
		{
			name: "self dependency across the leading slash",
			// Normalisation happens BEFORE the comparison, so the two
			// spellings of one container cannot slip past as different strings.
			input: validationInput("/api", "api"),
			want:  domain.DependencyRefusalSelf,
		},
		{
			name:  "malformed dependent",
			input: validationInput("not a valid name!", "postgres"),
			want:  domain.DependencyRefusalMalformed,
		},
		{
			name:  "empty name",
			input: validationInput("", "postgres"),
			want:  domain.DependencyRefusalMalformed,
		},
		{
			name:  "unknown container",
			input: validationInput("api", "redis"),
			want:  domain.DependencyRefusalUnknown,
		},
		{
			name: "container not present",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "postgres")
				endpoint := input.Containers["postgres"]
				endpoint.Present = false
				input.Containers["postgres"] = endpoint
				return input
			}(),
			want: domain.DependencyRefusalNotPresent,
		},
		{
			name: "preserved container",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "postgres")
				endpoint := input.Containers["postgres"]
				endpoint.Derived = true
				input.Containers["postgres"] = endpoint
				return input
			}(),
			want: domain.DependencyRefusalPreserved,
		},
		{
			name: "harbormaster as the dependency",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "harbormaster")
				input.Containers = estate("api", "harbormaster")
				input.Self = selfIdentity
				return input
			}(),
			want: domain.DependencyRefusalHarborMaster,
		},
		{
			name: "harbormaster as the dependent",
			input: func() domain.DependencyValidationInput {
				input := validationInput("harbormaster", "api")
				input.Containers = estate("api", "harbormaster")
				input.Self = selfIdentity
				return input
			}(),
			want: domain.DependencyRefusalHarborMaster,
		},
		{
			name: "duplicate of an existing operator relationship",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "postgres")
				input.Existing = []domain.WorkloadDependency{edge("api", "postgres")}
				return input
			}(),
			want: domain.DependencyRefusalDuplicate,
		},
		{
			name: "duplicate of a DISCOVERED relationship",
			input: func() domain.DependencyValidationInput {
				// Still a duplicate. The ordering already holds, and recording
				// it again would be a second row expressing one constraint.
				input := validationInput("api", "postgres")
				input.Existing = []domain.WorkloadDependency{hardEdge("api", "postgres")}
				return input
			}(),
			want: domain.DependencyRefusalDuplicate,
		},
		{
			name: "direct cycle",
			input: func() domain.DependencyValidationInput {
				input := validationInput("postgres", "api")
				input.Existing = []domain.WorkloadDependency{edge("api", "postgres")}
				return input
			}(),
			want: domain.DependencyRefusalCycle,
		},
		{
			name: "transitive cycle",
			input: func() domain.DependencyValidationInput {
				// api -> postgres, worker -> api, and now postgres -> worker.
				// No pairwise check finds this; the graph does.
				input := validationInput("postgres", "worker")
				input.Existing = []domain.WorkloadDependency{
					edge("api", "postgres"),
					edge("worker", "api"),
				}
				return input
			}(),
			want: domain.DependencyRefusalCycle,
		},
		{
			name: "cycle closed through a DISCOVERED relationship",
			input: func() domain.DependencyValidationInput {
				input := validationInput("postgres", "api")
				input.Existing = []domain.WorkloadDependency{hardEdge("api", "postgres")}
				return input
			}(),
			want: domain.DependencyRefusalCycle,
		},
		{
			name: "claiming a discovered source",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "postgres")
				input.Source = domain.DependencyNetworkNamespace
				return input
			}(),
			want: domain.DependencyRefusalDiscoveredSource,
		},
		{
			name: "claiming an unrecognised source",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "postgres")
				input.Source = "somethingElse"
				return input
			}(),
			want: domain.DependencyRefusalDiscoveredSource,
		},
		{
			name: "operator relationship bound reached",
			input: func() domain.DependencyValidationInput {
				input := validationInput("api", "postgres")
				input.OperatorCount = domain.MaxOperatorDependencies
				return input
			}(),
			want: domain.DependencyRefusalLimit,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			edge, refusal := domain.ValidateOperatorDependency(testCase.input)
			if refusal != testCase.want {
				t.Fatalf("refusal = %q, want %q", refusal, testCase.want)
			}
			if edge != (domain.WorkloadDependency{}) {
				t.Fatal("a refused relationship must produce no edge")
			}
			if refusal.Explain() == "" {
				t.Fatal("the refusal has no operator-facing explanation")
			}
		})
	}
}

// An omitted source means operator, and nothing stronger.
func TestAnOmittedSourceMeansOperator(t *testing.T) {
	t.Parallel()

	input := validationInput("api", "postgres")
	input.Source = ""

	edge, refusal := domain.ValidateOperatorDependency(input)
	if refusal != domain.DependencyRefusalNone {
		t.Fatalf("refused with %q", refusal)
	}
	if edge.Source != domain.DependencyOperator {
		t.Fatalf("an omitted source became %q", edge.Source)
	}
}

// The plain-English meaning shown before saving is the same sentence the API
// and the audit log use, so it is generated once.
func TestDescribeOperatorDependency(t *testing.T) {
	t.Parallel()

	got := domain.DescribeOperatorDependency("api", "postgres")
	want := "Update postgres before api. If postgres cannot be verified, api will not be updated."
	if got != want {
		t.Fatalf("described %q, want %q", got, want)
	}

	// A name that would not pass validation describes as nothing rather than
	// producing a sentence carrying unvalidated text.
	if domain.DescribeOperatorDependency("api", "nasty\nname") != "" {
		t.Fatal("an invalid name produced a description")
	}
}

// A source vocabulary that has grown must keep Hard() an allowlist, or an
// unrecognised value would claim a runtime requirement HarborMaster cannot
// check.
func TestUnrecognisedSourcesAreNotHardAndNotDiscovered(t *testing.T) {
	t.Parallel()

	unknown := domain.DependencySource("somethingNew")
	if unknown.Hard() {
		t.Fatal("an unrecognised source must not be hard")
	}
	if unknown.Discovered() {
		t.Fatal("an unrecognised source must not be discovered")
	}
	if domain.ValidDependencySource(string(unknown)) {
		t.Fatal("an unrecognised source must not validate")
	}
	if domain.DependencyOperator.Hard() {
		t.Fatal("an operator relationship must not be hard")
	}
	for _, source := range domain.DiscoveredDependencySources {
		if !source.Hard() || !source.Discovered() {
			t.Errorf("%q should be a hard, discovered source", source)
		}
	}
}

// The dependency state vocabulary must clear on exactly one member, and the
// zero value must not be it.
func TestOnlySatisfiedClears(t *testing.T) {
	t.Parallel()

	if !domain.DependencySatisfied.Clears() {
		t.Fatal("satisfied must clear")
	}
	if domain.DependencyStateInvalid.Clears() {
		t.Fatal("the zero value must not clear")
	}
	for _, state := range domain.DependencyStates {
		if state == domain.DependencySatisfied {
			continue
		}
		if state.Clears() {
			t.Errorf("%q must not clear", state)
		}
		if state.Explain() == "" || state.Label() == "" {
			t.Errorf("%q has no operator-facing words", state)
		}
	}
	if domain.DependencyState("inventedLater").Clears() {
		t.Fatal("an unrecognised state must not clear")
	}
	// Only a wait is revisited; everything else is a conclusion.
	if domain.DependencyWaiting.Terminal() {
		t.Fatal("waiting must not be terminal")
	}
	if !domain.DependencyState("inventedLater").Terminal() {
		t.Fatal("an unrecognised state must be terminal rather than retried forever")
	}
}

// Generated identifiers are validated by shape wherever they are read back.
func TestDependencyIDShape(t *testing.T) {
	t.Parallel()

	id := domain.NewDependencyID()
	if !domain.ValidDependencyID(id) {
		t.Fatalf("generated id %q does not validate", id)
	}
	if !strings.HasPrefix(id, domain.DependencyIDPrefix) {
		t.Fatalf("generated id %q has the wrong prefix", id)
	}
	for _, bad := range []string{
		"", "dep_", "dep_zzzz", id + "a", strings.ToUpper(id),
		"exec_" + strings.Repeat("a", 20),
	} {
		if domain.ValidDependencyID(bad) {
			t.Errorf("%q should not validate", bad)
		}
	}
	// Two draws from the entropy source. Assigned first because comparing the
	// two calls inline reads to a static analyser as comparing an expression
	// with itself.
	first, second := domain.NewDependencyID(), domain.NewDependencyID()
	if first == second {
		t.Fatal("generated ids collided")
	}
}
