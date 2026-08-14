package domain

import (
	"sort"
	"time"
)

// Rebinding a container whose namespace provider was replaced.
//
// # What the live experiment established
//
// Against Docker 29.6.2: recreating a container that others share a namespace
// with leaves every dependent RUNNING WITH NO NETWORK. Nothing crashes, nothing
// logs, and the dependent's own healthcheck may well keep passing. `docker
// restart` cannot repair it -- it fails and leaves the dependent stopped. The
// only repair observed to work is recreating the dependent with its namespace
// reference re-resolved to the provider's current identity.
//
// So HarborMaster can today update a VPN provider, pass all four verification
// proofs on the replacement, record success, and silently break three
// application containers. This file is what stops that.
//
// # The two halves
//
// INVARIANT A, before anything is stopped: a provider may not enter mutation
// unless every hard dependent has POSITIVELY passed rebind preflight. Unknown
// is refusal. AssessRebind and AssessProviderRebindability are that check.
//
// INVARIANT B, after the provider succeeds: the operation is not finished until
// every required rebind has itself reached verified success. That lives in the
// operation record; this file supplies the per-dependent verdict it is built
// from.
//
// # Why RebindEvidence has unexported fields
//
// This is the structural guarantee that no caller can ask for a rebind.
//
// A rebind plan proposes recreating a container. If any type in the request path
// could carry "please rebind this container", that would be a caller-supplied
// container id reaching a mutation -- the thing invariant 10 exists to prevent.
//
// So the evidence a rebind rests on cannot be written as a literal outside this
// package. It is produced by exactly one function, from a DependencyProblem
// that HarborMaster derived from its own inventory, and only when that problem
// is the specific stale-binding signal. An API handler cannot construct one,
// cannot forge one, and cannot reach the plan builder without one.

// RebindRefusal is why a dependent cannot be rebound.
//
// A closed vocabulary of POSITIVE-fact failures. Every one of them blocks the
// PROVIDER, because a provider whose dependents cannot be repaired is a
// provider that must not be stopped.
type RebindRefusal string

// Rebind refusals.
const (
	// RebindRefusalNone means the dependent can be rebound.
	RebindRefusalNone RebindRefusal = ""

	// RebindRefusalNoEvidence means nothing established that a rebind is needed.
	RebindRefusalNoEvidence RebindRefusal = "noEvidence"

	// RebindRefusalNotPresent means the last refresh did not see the dependent.
	RebindRefusalNotPresent RebindRefusal = "dependentNotPresent"

	// RebindRefusalNamespaceStale means the dependent's namespace facts have not
	// been observed, so what it is attached to is not established.
	RebindRefusalNamespaceStale RebindRefusal = "namespaceObservationStale"

	// RebindRefusalProviderMismatch means the dependent does not resolve to the
	// provider this operation is about.
	RebindRefusalProviderMismatch RebindRefusal = "providerMismatch"

	// RebindRefusalNotRecreatable means the dependent is not a workload
	// HarborMaster could recreate at all.
	RebindRefusalNotRecreatable RebindRefusal = "notRecreatable"

	// RebindRefusalHarborMaster means the dependent is HarborMaster itself.
	//
	// A rebind is a recreation, and HarborMaster cannot recreate itself. This is
	// a fifth independent site for that refusal, not a replacement for the four
	// that already exist.
	RebindRefusalHarborMaster RebindRefusal = "harborMasterContainer"

	// RebindRefusalPreserved means the dependent is a parked original or a
	// quarantined replacement HarborMaster keeps as evidence.
	RebindRefusalPreserved RebindRefusal = "preservedContainer"

	// RebindRefusalDisabled means the container's owner opted it out.
	//
	// Honoured even though a rebind repairs rather than changes. An opt-out is a
	// statement that HarborMaster should leave the container alone, and reading
	// it as "except when we think we know better" is how an opt-out stops
	// meaning anything.
	RebindRefusalDisabled RebindRefusal = "optedOut"

	// RebindRefusalReferenceUnestablished means the running image reference is
	// not established, so no plan can name what to recreate from.
	RebindRefusalReferenceUnestablished RebindRefusal = "runningReferenceUnestablished"

	// RebindRefusalDigestUnestablished means the running digest is not
	// established.
	//
	// The commonest real refusal, and the most important. A rebind is pinned to
	// the digest HarborMaster OBSERVED running; without one there is nothing to
	// pin to, and recreating from a mutable tag would silently change what
	// executes on the host while claiming to repair a network attachment.
	RebindRefusalDigestUnestablished RebindRefusal = "runningDigestUnestablished"
)

// RebindRefusals lists every refusal.
var RebindRefusals = []RebindRefusal{
	RebindRefusalNoEvidence,
	RebindRefusalNotPresent,
	RebindRefusalNamespaceStale,
	RebindRefusalProviderMismatch,
	RebindRefusalNotRecreatable,
	RebindRefusalHarborMaster,
	RebindRefusalPreserved,
	RebindRefusalDisabled,
	RebindRefusalReferenceUnestablished,
	RebindRefusalDigestUnestablished,
}

// ValidRebindRefusal reports whether name is a known refusal.
func ValidRebindRefusal(name string) bool {
	for _, refusal := range RebindRefusals {
		if string(refusal) == name {
			return true
		}
	}
	return false
}

// Explain renders the refusal in HarborMaster's own words.
func (r RebindRefusal) Explain() string {
	switch r {
	case RebindRefusalNone:
		return "no refusal"
	case RebindRefusalNoEvidence:
		return "HarborMaster has no evidence that this container needs reattaching"
	case RebindRefusalNotPresent:
		return "the last inventory refresh did not see this container"
	case RebindRefusalNamespaceStale:
		return "HarborMaster has not established which namespace this container is attached to"
	case RebindRefusalProviderMismatch:
		return "this container is not attached to the namespace of the container being replaced"
	case RebindRefusalNotRecreatable:
		return "this is not a workload HarborMaster could recreate"
	case RebindRefusalHarborMaster:
		return "this is the container HarborMaster is running in"
	case RebindRefusalPreserved:
		return "this container is a parked original or a quarantined replacement " +
			"HarborMaster keeps as evidence"
	case RebindRefusalDisabled:
		return "the container's owner opted it out of HarborMaster acting on it"
	case RebindRefusalReferenceUnestablished:
		return "HarborMaster has not established which image this container runs"
	case RebindRefusalDigestUnestablished:
		return "HarborMaster has not established the immutable digest this container " +
			"is running, so it cannot recreate it on the same image"
	default:
		return "this container cannot be reattached, for a reason this build does not recognise"
	}
}

// ------------------------------------------------------------- evidence --

// RebindEvidence is HarborMaster's proof that a container is attached to a
// namespace that no longer exists.
//
// Every field is unexported. The zero value is inert and BuildRebindPlan
// refuses it, so a package that cannot call RebindEvidenceFrom cannot produce a
// rebind at all. See the note at the top of this file.
type RebindEvidence struct {
	dependent  string
	provider   string
	source     DependencySource
	staleID    string
	observedAt time.Time
}

// RebindEvidenceFrom derives evidence from a discovery problem.
//
// The ONLY constructor. Refuses unless the problem is the specific
// stale-binding signal -- a namespace reference naming a container the
// inventory no longer has. Every other discovery refusal is a state of NOT
// KNOWING, and not knowing is never grounds for recreating something.
//
// provider is the name HarborMaster holds for the container that id used to be.
// It is looked up by the caller from its own records; an empty one is refused,
// because a rebind that cannot name what it is reattaching to is a rebind that
// cannot be verified afterwards.
func RebindEvidenceFrom(
	problem DependencyProblem,
	provider string,
	observedAt time.Time,
) (RebindEvidence, bool) {
	if !problem.Refusal.RebindSignal() {
		return RebindEvidence{}, false
	}
	if !problem.Source.Hard() {
		// Only a namespace the runtime enforces can leave a container broken
		// this way. An operator ordering assertion cannot.
		return RebindEvidence{}, false
	}

	dependent := NormaliseContainerName(problem.Container)
	providerName := NormaliseContainerName(provider)
	if !ValidContainerName(dependent) || !ValidContainerName(providerName) {
		return RebindEvidence{}, false
	}
	if dependent == providerName {
		return RebindEvidence{}, false
	}
	if !ValidFullContainerID(problem.ReferencedID) {
		return RebindEvidence{}, false
	}

	return RebindEvidence{
		dependent:  dependent,
		provider:   providerName,
		source:     problem.Source,
		staleID:    problem.ReferencedID,
		observedAt: observedAt,
	}, true
}

// The accessors. Read-only by construction: every field is unexported and there
// is no setter for any of them, so evidence can be READ anywhere and CREATED
// only by RebindEvidenceFrom.

// Dependent returns the container that must be reattached.
func (e RebindEvidence) Dependent() string { return e.dependent }

// Provider returns the container whose namespace it must reattach to.
func (e RebindEvidence) Provider() string { return e.provider }

// Source returns which namespace the two containers share.
func (e RebindEvidence) Source() DependencySource { return e.source }

// StaleContainerID returns the dead provider id the dependent still names.
func (e RebindEvidence) StaleContainerID() string { return e.staleID }

// ObservedAt returns when the evidence was derived.
func (e RebindEvidence) ObservedAt() time.Time { return e.observedAt }

// Established reports whether this evidence was actually derived.
//
// The zero value is not. Every consumer checks it, which is what makes the
// unexported fields load-bearing rather than decorative.
func (e RebindEvidence) Established() bool {
	return e.dependent != "" && e.provider != "" && e.source.Hard() && e.staleID != ""
}

// Explain renders why a rebind is needed, in HarborMaster's own words.
func (e RebindEvidence) Explain() string {
	if !e.Established() {
		return ""
	}
	return "this container shares the " + e.source.Describe() +
		" of a container that has been replaced, so it is attached to a namespace " +
		"that no longer exists"
}

// ------------------------------------------------------------ candidates --

// RebindCandidate is everything the preflight is decided from.
//
// Every field is a POSITIVE fact established from HarborMaster's own records.
// The zero value satisfies none of them, so a dependent the caller failed to
// look up is REFUSED rather than cleared -- which is the whole of "unknown is
// refusal".
type RebindCandidate struct {
	// Evidence is why a rebind is believed necessary. An unestablished one
	// refuses.
	Evidence RebindEvidence

	// Name is the dependent, and Provider the container it must reattach to.
	Name     string
	Provider string

	// ContainerID, ImageRef and Labels identify it for the self match.
	ContainerID string
	ImageRef    string
	Labels      map[string]string

	// Present records that the last refresh saw it.
	Present bool
	// NamespacesObserved records that its namespace facts were read.
	NamespacesObserved bool
	// Recreatable records that it is a workload HarborMaster could act on.
	Recreatable bool
	// Derived records that HarborMaster created it during a recreation.
	Derived bool

	// RunningReference and RunningDigest are what it is ACTUALLY running.
	// Both are required: a rebind is pinned to what HarborMaster observed.
	RunningReference string
	RunningDigest    string
}

// AssessRebind decides whether one dependent can be rebound.
//
// The order is cheapest and most absolute first, so a container that must never
// be touched is refused before anything about images is consulted.
func AssessRebind(candidate RebindCandidate, self SelfIdentity) RebindRefusal {
	if !candidate.Evidence.Established() {
		return RebindRefusalNoEvidence
	}

	name := NormaliseContainerName(candidate.Name)
	provider := NormaliseContainerName(candidate.Provider)

	// The evidence must be about THIS dependent and THIS provider. A candidate
	// assembled from one container's evidence and another's facts is not
	// something to reason about; it is a bug, and it refuses.
	if candidate.Evidence.Dependent() != name || candidate.Evidence.Provider() != provider {
		return RebindRefusalProviderMismatch
	}

	// HarborMaster itself, before anything else. A rebind is a recreation.
	if match, _ := self.SelfMatch(SelfTarget{
		ContainerID:   candidate.ContainerID,
		ContainerName: name,
		ImageRef:      candidate.ImageRef,
		Labels:        candidate.Labels,
	}); match {
		return RebindRefusalHarborMaster
	}

	if candidate.Derived || IsHarborMasterDerivedName(name) {
		return RebindRefusalPreserved
	}
	if !candidate.Present {
		return RebindRefusalNotPresent
	}
	if !candidate.NamespacesObserved {
		return RebindRefusalNamespaceStale
	}
	if !candidate.Recreatable {
		return RebindRefusalNotRecreatable
	}

	// The owner's opt-outs. Both the estate-wide label and the update-specific
	// one, because a rebind is HarborMaster acting on the container.
	if value, ok := candidate.Labels[LabelHarborMasterEnabled]; ok && falsyLabel(value) {
		return RebindRefusalDisabled
	}
	if overrides := ParseUpdateLabels(candidate.Labels); overrides.Disabled {
		return RebindRefusalDisabled
	}

	if candidate.RunningReference == "" {
		return RebindRefusalReferenceUnestablished
	}
	if !ValidImageDigest(candidate.RunningDigest) {
		return RebindRefusalDigestUnestablished
	}

	return RebindRefusalNone
}

// ------------------------------------------------------------- provider --

// ProviderBlock is one dependent that stops a provider being stopped.
type ProviderBlock struct {
	Dependent string        `json:"dependent"`
	Refusal   RebindRefusal `json:"refusal"`
}

// ProviderRebindAssessment is invariant A's verdict for one provider.
type ProviderRebindAssessment struct {
	Provider string `json:"provider"`
	// Rebindable are the dependents that positively passed, sorted.
	Rebindable []string `json:"rebindable"`
	// Blocked are the dependents that did not, sorted, with why.
	Blocked []ProviderBlock `json:"blocked"`
}

// MayStop reports whether the provider may enter mutation.
//
// An allowlist: the provider may proceed only when NOTHING blocked. Written as
// "no blocks" rather than "all rebindable" deliberately -- a candidate list that
// was never populated produces neither, and this phrasing lets that through
// only when there was genuinely nothing to check, which the caller establishes
// separately by counting hard dependents.
func (a ProviderRebindAssessment) MayStop() bool { return len(a.Blocked) == 0 }

// AssessProviderRebindability runs invariant A over every hard dependent.
//
// This is the check that must happen BEFORE the provider is stopped. The live
// experiment established that stopping is the moment dependents break: not
// removing, not recreating -- stopping. Everything after that point is already
// too late.
func AssessProviderRebindability(
	provider string,
	candidates []RebindCandidate,
	self SelfIdentity,
) ProviderRebindAssessment {
	assessment := ProviderRebindAssessment{Provider: NormaliseContainerName(provider)}

	for _, candidate := range candidates {
		name := NormaliseContainerName(candidate.Name)
		if refusal := AssessRebind(candidate, self); refusal != RebindRefusalNone {
			assessment.Blocked = append(assessment.Blocked,
				ProviderBlock{Dependent: name, Refusal: refusal})
			continue
		}
		assessment.Rebindable = append(assessment.Rebindable, name)
	}

	sort.Strings(assessment.Rebindable)
	sort.SliceStable(assessment.Blocked, func(i, j int) bool {
		return assessment.Blocked[i].Dependent < assessment.Blocked[j].Dependent
	})
	return assessment
}

// ------------------------------------------------------------ the target --

// RebindTarget is what a rebind plan proposes.
//
// Constructed only from established evidence and an established running digest,
// so the pair cannot be crossed the way a registry-proposed pair can.
type RebindTarget struct {
	Reference string
	Digest    string
}

// BuildRebindPlan produces the change plan that repairs one dependent.
//
// # This is the only function that can produce an UpdateRebind plan
//
// It requires ESTABLISHED evidence, and RebindEvidence's fields are unexported
// with exactly one constructor — RebindEvidenceFrom, which itself refuses
// anything that is not a hard namespace share whose provider id the inventory no
// longer has. So the chain from "a caller asked" to "a container is recreated"
// is broken structurally rather than by a check somebody has to remember:
//
//   - the API package cannot write a RebindEvidence literal, because the fields
//     are unexported;
//   - it cannot obtain one from RebindEvidenceFrom without a DependencyProblem
//     carrying the stale-binding refusal, which only DiscoverDependencies
//     produces, and only from the inventory HarborMaster wrote itself;
//   - and BuildRebindPlan refuses the zero value.
//
// TestOnlyCoordinatorEvidenceProducesARebindPlan pins the last link.
//
// # The proposal cannot change what executes
//
// CurrentImage EQUALS ProposedImage, and ProposedDigest is the digest
// HarborMaster OBSERVED the container running. A rebind therefore recreates the
// container on the identical artefact; the only thing that changes is which
// namespace it is attached to.
//
// # Every other gate still applies
//
// inputs carries the container's snapshot, drift, compliance, and registry facts
// exactly as the planner assembled them for an ordinary plan, and the risk
// assessment runs over them unchanged. A rebind of a container with no snapshot
// still scores manualReview; a rebind of a container with an open critical
// violation is still refused by the execution preflight. This function overrides
// the image fields and NOTHING else.
func BuildRebindPlan(
	evidence RebindEvidence,
	candidate RebindCandidate,
	self SelfIdentity,
	inputs PlanInputs,
	planID string,
	now time.Time,
) (ChangePlan, RebindRefusal) {
	if !evidence.Established() {
		return ChangePlan{}, RebindRefusalNoEvidence
	}
	// The candidate must be the one the evidence is about, and must pass every
	// rebindability check in its own right.
	candidate.Evidence = evidence
	target, refusal := NewRebindTarget(candidate, self)
	if refusal != RebindRefusalNone {
		return ChangePlan{}, refusal
	}

	// The image fields, and only these.
	inputs.CurrentImage = target.Reference
	inputs.ProposedImage = target.Reference
	inputs.ProposedDigest = target.Digest
	inputs.UpdateType = UpdateRebind
	inputs.EvaluatedAt = now.UTC()

	// Every enumerated field the storage constrains, given its honest
	// "nothing established" value when the caller had nothing to put there.
	//
	// # Why this is here and why it matters
	//
	// A rebind consults NO REGISTRY and may be built for a container with no
	// baseline. The zero value of each of these types is the empty string,
	// which is not a member of the stored vocabulary -- so a plan carrying one
	// is valid to the type system and IMPOSSIBLE TO PERSIST.
	//
	// Stage 5 acceptance found exactly that against a real daemon: the
	// dependent's image intelligence record was not in the batch, the status
	// stayed empty, and every attempt to store the reattachment plan was
	// refused by the schema. The provider had already been replaced, so the
	// dependent sat attached to a container id that no longer existed while
	// the follower retried, forever, every ten seconds.
	//
	// `pending` and `unknown` are the honest words. Neither weakens anything:
	// the execution preflight reads the image intelligence TABLE and the
	// snapshot record directly, and independently requires a fresh successful
	// registry record before it will recreate anything. These two fields are
	// what the plan REPORTS, not what any gate consults.
	if inputs.RegistryStatus == "" {
		inputs.RegistryStatus = CheckPending
	}
	if inputs.RestoreReadiness == "" {
		inputs.RestoreReadiness = ReadinessUnknown
	}

	plan := ChangePlan{
		PlanID:        planID,
		ContainerID:   candidate.ContainerID,
		ContainerName: NormaliseContainerName(candidate.Name),

		CurrentImage:   inputs.CurrentImage,
		ProposedImage:  inputs.ProposedImage,
		CurrentDigest:  target.Digest,
		ProposedDigest: inputs.ProposedDigest,
		UpdateType:     UpdateRebind,

		SnapshotID:        inputs.SnapshotID,
		SnapshotAvailable: inputs.SnapshotID > 0,
		RestoreReadiness:  inputs.RestoreReadiness,

		DriftOpen:        inputs.DriftOpen,
		DriftMaxSeverity: inputs.DriftMaxSeverity,

		PolicyOpen:        inputs.PolicyOpen,
		PolicyMaxSeverity: inputs.PolicyMaxSeverity,

		RegistryStatus: inputs.RegistryStatus,
		RegistryDetail: inputs.RegistryDetail,

		Risk: AssessRisk(inputs),

		PlanVersion:    PlanSchemaVersion,
		PlannerVersion: PlannerVersion,
		InputDigest:    inputs.Fingerprint(),
		GeneratedAt:    inputs.EvaluatedAt,
	}

	// The structural gate every plan passes. A rebind whose reference and digest
	// were not resolved together is not a rebind HarborMaster may act on.
	if !plan.ValidTarget() {
		return ChangePlan{}, RebindRefusalDigestUnestablished
	}
	return plan, RebindRefusalNone
}

// NewRebindTarget builds the proposal for one dependent.
//
// The proposed reference EQUALS the current reference and the proposed digest is
// the one the container is already running. That is the definition of a rebind:
// nothing about what executes changes, only what it is attached to.
func NewRebindTarget(candidate RebindCandidate, self SelfIdentity) (RebindTarget, RebindRefusal) {
	if refusal := AssessRebind(candidate, self); refusal != RebindRefusalNone {
		return RebindTarget{}, refusal
	}
	return RebindTarget{
		Reference: candidate.RunningReference,
		Digest:    candidate.RunningDigest,
	}, RebindRefusalNone
}
