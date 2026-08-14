package domain

// Validating an operator-defined relationship, before it is stored.
//
// # Why this is a pure function with an explicit input struct
//
// Same reasoning as DecideAutomation: the check is made from parameters and
// nothing else, so it can be tested exhaustively without a database and cannot
// quietly grow a lookup that reaches something it should not read.
//
// # What an operator relationship may NOT do
//
// It may not name a container that does not exist, one HarborMaster keeps as
// evidence, or HarborMaster itself. It may not duplicate a relationship, point
// at its own subject, or close a cycle. And it may not claim a DISCOVERED
// source: the three namespace sources are derived from Docker configuration on
// every read, and letting a write assert one would let an operator fabricate a
// runtime relationship the daemon does not enforce.
//
// None of these refusals stops a container being updated. An operator
// relationship only ever constrains ORDER, so refusing to create one is the
// conservative direction in the sense that matters: HarborMaster declines to
// record something it cannot stand behind, and the estate keeps whatever
// ordering its Docker configuration already implies.

// MaxOperatorDependencies bounds how many relationships an operator may record.
//
// A human-authored set. Five hundred is far past what anybody maintains by hand
// and well inside the graph's edge bound, so reaching it means something is
// writing them in a loop.
const MaxOperatorDependencies = 500

// DependencyRefusal is why an operator relationship was not accepted.
//
// A closed vocabulary, so the API reports which check said no rather than a
// generic failure -- matching ExecutionRefusal and AcquisitionRefusal.
type DependencyRefusal string

// Dependency refusals.
const (
	// DependencyRefusalNone means the relationship may be recorded.
	DependencyRefusalNone DependencyRefusal = ""

	// DependencyRefusalMalformed means a name is not one HarborMaster can key
	// a relationship on.
	DependencyRefusalMalformed DependencyRefusal = "malformedName"

	// DependencyRefusalSelf means both ends are the same container.
	DependencyRefusalSelf DependencyRefusal = "selfDependency"

	// DependencyRefusalDuplicate means the relationship already exists.
	DependencyRefusalDuplicate DependencyRefusal = "duplicate"

	// DependencyRefusalUnknown means a named container is not in the inventory.
	DependencyRefusalUnknown DependencyRefusal = "unknownContainer"

	// DependencyRefusalNotPresent means a named container is known but the last
	// refresh did not see it.
	DependencyRefusalNotPresent DependencyRefusal = "containerNotPresent"

	// DependencyRefusalPreserved means a named container is a parked original
	// or a quarantined replacement HarborMaster keeps as evidence.
	DependencyRefusalPreserved DependencyRefusal = "preservedContainer"

	// DependencyRefusalHarborMaster means a named container is HarborMaster.
	//
	// Refused at BOTH ends. As a dependent it would be asserting that
	// HarborMaster must be recreated in some order, which it cannot be at all;
	// as a dependency it would make HarborMaster's own state a gate on other
	// containers' updates, which is a relationship nothing should be able to
	// create from an HTTP request.
	DependencyRefusalHarborMaster DependencyRefusal = "harborMasterContainer"

	// DependencyRefusalCycle means the relationship would close a loop.
	DependencyRefusalCycle DependencyRefusal = "dependencyCycle"

	// DependencyRefusalDiscoveredSource means the write claimed a source
	// HarborMaster derives rather than stores.
	DependencyRefusalDiscoveredSource DependencyRefusal = "discoveredSource"

	// DependencyRefusalLimit means the operator relationship bound is reached.
	DependencyRefusalLimit DependencyRefusal = "limit"
)

// DependencyRefusals lists every refusal, for validation and for the UI.
var DependencyRefusals = []DependencyRefusal{
	DependencyRefusalMalformed,
	DependencyRefusalSelf,
	DependencyRefusalDuplicate,
	DependencyRefusalUnknown,
	DependencyRefusalNotPresent,
	DependencyRefusalPreserved,
	DependencyRefusalHarborMaster,
	DependencyRefusalCycle,
	DependencyRefusalDiscoveredSource,
	DependencyRefusalLimit,
}

// ValidDependencyRefusal reports whether name is a known refusal.
func ValidDependencyRefusal(name string) bool {
	for _, refusal := range DependencyRefusals {
		if string(refusal) == name {
			return true
		}
	}
	return false
}

// Explain renders the refusal in operator-facing words.
//
// HarborMaster's own sentence. The names involved are carried by the caller and
// never interpolated here.
func (r DependencyRefusal) Explain() string {
	switch r {
	case DependencyRefusalNone:
		return "no refusal"
	case DependencyRefusalMalformed:
		return "a container name was not one HarborMaster can record a relationship for"
	case DependencyRefusalSelf:
		return "a container cannot depend on itself"
	case DependencyRefusalDuplicate:
		return "HarborMaster already records this relationship"
	case DependencyRefusalUnknown:
		return "HarborMaster has no container by that name"
	case DependencyRefusalNotPresent:
		return "the last inventory refresh did not see that container"
	case DependencyRefusalPreserved:
		return "that container is a parked original or a quarantined replacement " +
			"HarborMaster keeps as evidence"
	case DependencyRefusalHarborMaster:
		return "that is the container HarborMaster is running in"
	case DependencyRefusalCycle:
		return "this relationship would make the containers depend on each other in a loop"
	case DependencyRefusalDiscoveredSource:
		return "HarborMaster derives namespace relationships from Docker configuration; " +
			"they cannot be created or removed by hand"
	case DependencyRefusalLimit:
		return "HarborMaster records as many operator relationships as it will hold"
	default:
		return "the relationship was refused for a reason this build does not recognise"
	}
}

// DependencyEndpoint is what validation knows about one container.
//
// Every field is a POSITIVE fact established from HarborMaster's own inventory,
// and the zero value satisfies none of them -- so a container the caller failed
// to look up is refused rather than accepted. The same discipline
// TargetEligibility uses, for the same reason.
type DependencyEndpoint struct {
	Name        string
	ContainerID string
	ImageRef    string
	Labels      map[string]string
	// Present records that the last refresh saw it.
	Present bool
	// Derived records that HarborMaster created it during a recreation.
	Derived bool
}

// DependencyValidationInput is everything the check is made from.
type DependencyValidationInput struct {
	// Dependent and Dependency are the names as the request supplied them,
	// before normalisation.
	Dependent  string
	Dependency string
	// Source is what the request claimed. Only DependencyOperator is accepted;
	// the empty value is treated as operator, so a client that omits it gets the
	// only thing it could have meant.
	Source DependencySource

	// Containers is the present estate indexed by normalised name.
	Containers map[string]DependencyEndpoint
	// Existing is every relationship already in force, discovered and operator
	// alike. Both are needed: a duplicate of a DISCOVERED relationship is still
	// a duplicate, and a cycle can be closed through one.
	Existing []WorkloadDependency
	// OperatorCount is how many operator relationships are already stored.
	OperatorCount int
	// Self is HarborMaster's own identity. The zero value matches nothing, so a
	// build that could not detect itself refuses nothing here -- and the four
	// existing self-update layers are untouched by that.
	Self SelfIdentity
}

// ValidateOperatorDependency decides whether a relationship may be recorded.
//
// Returns DependencyRefusalNone and the normalised pair when it may. The order
// of the checks is the design: cheapest and most absolute first, so a malformed
// or self-referential request is refused before anything scans the estate.
func ValidateOperatorDependency(input DependencyValidationInput) (WorkloadDependency, DependencyRefusal) {
	// 0. The claimed source. First, because accepting a discovered source would
	// mean storing a row that asserts a runtime requirement the daemon does not
	// enforce -- and every check below would then be validating the wrong thing.
	switch input.Source {
	case "", DependencyOperator:
		// Both mean operator. An omitted source is not an invitation to guess
		// at a stronger one.
	default:
		return WorkloadDependency{}, DependencyRefusalDiscoveredSource
	}

	dependent := NormaliseContainerName(input.Dependent)
	dependency := NormaliseContainerName(input.Dependency)

	// 1. Names HarborMaster can key on.
	if !ValidContainerName(dependent) || !ValidContainerName(dependency) {
		return WorkloadDependency{}, DependencyRefusalMalformed
	}

	// 2. Self-dependency. Compared AFTER normalisation, so "/api" and "api"
	// cannot slip past by differing as strings.
	if dependent == dependency {
		return WorkloadDependency{}, DependencyRefusalSelf
	}

	// 3. The bound, before any estate scan.
	if input.OperatorCount >= MaxOperatorDependencies {
		return WorkloadDependency{}, DependencyRefusalLimit
	}

	// 4. Both ends must be containers HarborMaster may legitimately relate.
	for _, name := range []string{dependent, dependency} {
		if refusal := validateEndpoint(name, input); refusal != DependencyRefusalNone {
			return WorkloadDependency{}, refusal
		}
	}

	// 5. Not already recorded, from any source.
	for _, existing := range input.Existing {
		if existing.Dependent == dependent && existing.Dependency == dependency {
			return WorkloadDependency{}, DependencyRefusalDuplicate
		}
	}

	// 6. The cycle check, run against the graph this write WOULD produce.
	//
	// Built rather than reasoned about: a transitive cycle three hops long is
	// not something a pairwise check finds, and the graph builder is the one
	// piece of code that already knows how to find one.
	candidate := WorkloadDependency{
		Dependent:  dependent,
		Dependency: dependency,
		Source:     DependencyOperator,
	}
	if closesCycle(input, candidate) {
		return WorkloadDependency{}, DependencyRefusalCycle
	}

	return candidate, DependencyRefusalNone
}

// validateEndpoint checks one end of a proposed relationship.
func validateEndpoint(name string, input DependencyValidationInput) DependencyRefusal {
	endpoint, known := input.Containers[name]
	if !known {
		return DependencyRefusalUnknown
	}
	if !endpoint.Present {
		return DependencyRefusalNotPresent
	}
	if endpoint.Derived || IsHarborMasterDerivedName(name) {
		return DependencyRefusalPreserved
	}
	if match, _ := input.Self.SelfMatch(SelfTarget{
		ContainerID:   endpoint.ContainerID,
		ContainerName: name,
		ImageRef:      endpoint.ImageRef,
		Labels:        endpoint.Labels,
	}); match {
		return DependencyRefusalHarborMaster
	}
	return DependencyRefusalNone
}

// closesCycle reports whether adding candidate would leave its dependent unable
// to be ordered.
//
// Builds the graph WITH the candidate and asks whether the dependent came out
// cycle-blocked. A graph that refuses to build at all -- because the estate is
// past a bound -- is reported as closing a cycle, which is the fail-closed
// reading: HarborMaster could not establish that the relationship is safe, so
// it does not record it.
func closesCycle(input DependencyValidationInput, candidate WorkloadDependency) bool {
	nodes := make([]string, 0, len(input.Containers))
	for name := range input.Containers {
		nodes = append(nodes, name)
	}

	edges := make([]WorkloadDependency, 0, len(input.Existing)+1)
	edges = append(edges, input.Existing...)
	edges = append(edges, candidate)

	graph, err := BuildDependencyGraph(nodes, edges)
	if err != nil {
		return true
	}
	return graph.CycleBlocked(candidate.Dependent)
}

// DescribeOperatorDependency renders what a relationship means, in the sentence
// the editor shows before saving.
//
// # Why this lives in the domain rather than the frontend
//
// It is the same sentence the API returns and the audit log records. Written
// once, it cannot drift between the confirmation an operator read and the
// record of what they agreed to.
//
// The names are interpolated here because both have already passed
// ValidContainerName, whose allowlist excludes every character that could forge
// a log line or inject markup. Callers still render, never execute.
func DescribeOperatorDependency(dependent, dependency string) string {
	dependent = NormaliseContainerName(dependent)
	dependency = NormaliseContainerName(dependency)
	if !ValidContainerName(dependent) || !ValidContainerName(dependency) {
		return ""
	}
	return "Update " + dependency + " before " + dependent + ". If " + dependency +
		" cannot be verified, " + dependent + " will not be updated."
}

// EndpointsFromNames builds the validation index from a list of endpoints.
//
// A helper so the repository and the tests build the map the same way, keyed on
// the normalised name the validator looks up.
func EndpointsFromNames(endpoints []DependencyEndpoint) map[string]DependencyEndpoint {
	index := make(map[string]DependencyEndpoint, len(endpoints))
	for _, endpoint := range endpoints {
		name := NormaliseContainerName(endpoint.Name)
		if name == "" {
			continue
		}
		endpoint.Name = name
		index[name] = endpoint
	}
	return index
}
