package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Policy definitions and compliance violations.
//
// # Policy is not drift
//
// Drift answers "did this container change from its baseline?". A policy
// answers "does this container comply with an organisational rule?". The two
// are independent: a container can drift from its baseline into a still-
// compliant configuration, and a container that has never moved can be
// non-compliant from the day it was created because the rule arrived later.
// Nothing in this file reads a snapshot, and nothing in the drift model reads a
// policy.
//
// # No interpreter
//
// A policy is a list of STRONGLY TYPED rules from a closed vocabulary. There is
// no expression language, no template, no script, and no user-supplied code
// path of any kind. Every rule's semantics are fixed at compile time in
// policy_rules.go; the only thing an administrator supplies is a rule type from
// the vocabulary and a bounded list of parameters. This is the whole reason the
// design is shaped this way: a policy is administrator-supplied input to an
// unauthenticated API, and an interpreter would make that input executable.
//
// # Secrets
//
// Environment-variable rules match NAMES. The evaluation input (PolicyTarget)
// carries no field that can hold a value, so a rule has nothing to read even if
// one were written carelessly. See NewPolicyTarget.

// PolicySeverity ranks how much a violation matters.
type PolicySeverity string

// Policy severities.
const (
	PolicySeverityCritical PolicySeverity = "critical"
	PolicySeverityHigh     PolicySeverity = "high"
	PolicySeverityMedium   PolicySeverity = "medium"
	PolicySeverityLow      PolicySeverity = "low"
)

// PolicySeverities lists every severity, most severe first.
var PolicySeverities = []PolicySeverity{
	PolicySeverityCritical, PolicySeverityHigh, PolicySeverityMedium, PolicySeverityLow,
}

// ValidPolicySeverity reports whether name is a known severity.
func ValidPolicySeverity(name string) bool {
	for _, severity := range PolicySeverities {
		if string(severity) == name {
			return true
		}
	}
	return false
}

// Rank returns a sortable rank, higher being more severe.
func (s PolicySeverity) Rank() int {
	switch s {
	case PolicySeverityCritical:
		return 4
	case PolicySeverityHigh:
		return 3
	case PolicySeverityMedium:
		return 2
	case PolicySeverityLow:
		return 1
	default:
		return 0
	}
}

// PolicyViolationStatus is the lifecycle state of a violation.
//
// # Who owns which value
//
// The same split drift uses, and for the same reason:
//
//   - ACTIVE and RESOLVED are owned by the ENGINE. They are facts about the
//     world: the rule still fails, or it no longer does.
//   - ACKNOWLEDGED and EXEMPTED are owned by the OPERATOR. They are statements
//     of intent about a failure that still stands.
//
// Neither operator status suppresses re-evaluation. An acknowledged violation
// is re-checked on every pass, keeps its last-seen timestamp current, and
// resolves automatically the moment the container becomes compliant. An
// acknowledgement that stopped the checking would turn the compliance report
// into a list of things somebody once clicked.
type PolicyViolationStatus string

// Policy violation statuses.
const (
	// PolicyViolationActive means the rule fails and nobody has reviewed it.
	PolicyViolationActive PolicyViolationStatus = "active"
	// PolicyViolationResolved means a later evaluation found the container
	// compliant with the rule. Engine-owned.
	PolicyViolationResolved PolicyViolationStatus = "resolved"
	// PolicyViolationAcknowledged means an operator has seen it and accepts it
	// for now. Re-evaluation continues.
	PolicyViolationAcknowledged PolicyViolationStatus = "acknowledged"
	// PolicyViolationExempted means an operator has accepted the risk for this
	// container. Re-evaluation continues: an exemption is a statement about
	// tolerance, not a claim that the rule now passes.
	PolicyViolationExempted PolicyViolationStatus = "exempted"
)

// PolicyViolationStatuses lists every status.
var PolicyViolationStatuses = []PolicyViolationStatus{
	PolicyViolationActive, PolicyViolationResolved,
	PolicyViolationAcknowledged, PolicyViolationExempted,
}

// OperatorPolicyStatuses are the transitions an API caller may request.
//
// Deliberately excludes active and resolved: those are engine-owned. Widening
// the API surface means editing this line, in a diff a reviewer sees.
var OperatorPolicyStatuses = []PolicyViolationStatus{
	PolicyViolationAcknowledged, PolicyViolationExempted,
}

// ValidPolicyViolationStatus reports whether name is a known status.
func ValidPolicyViolationStatus(name string) bool {
	for _, status := range PolicyViolationStatuses {
		if string(status) == name {
			return true
		}
	}
	return false
}

// ValidOperatorPolicyStatus reports whether name is a transition an API caller
// may request.
func ValidOperatorPolicyStatus(name string) bool {
	for _, status := range OperatorPolicyStatuses {
		if string(status) == name {
			return true
		}
	}
	return false
}

// Open reports whether a status means the rule still fails.
//
// Everything except resolved: an acknowledged violation has been read, not
// fixed.
func (s PolicyViolationStatus) Open() bool { return s != PolicyViolationResolved }

// ------------------------------------------------------------ definitions --

// PolicyDefinition is one administrator-defined policy.
//
// A policy is a NAMED SET OF RULES with a default severity. Every enabled,
// unarchived policy is evaluated against every present container: there is no
// selector language, because a selector is a second thing an unauthenticated
// caller could make expensive and because "this rule applies to the estate" is
// what an organisational rule usually means.
type PolicyDefinition struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// PolicyID is the IMMUTABLE public identifier. Generated server-side at
	// creation, never accepted from a caller, and never changed by an update:
	// a violation references it, so a mutable id would orphan history.
	PolicyID string `json:"policyId"`

	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Severity    PolicySeverity `json:"severity"`

	// Enabled turns evaluation on or off without losing the definition. A
	// disabled policy's open violations resolve on the next pass, because the
	// rule no longer applies.
	Enabled bool `json:"enabled"`
	// Archived is what DELETE sets. The row is kept rather than removed
	// because violations reference it and history must survive: see
	// PolicyRepository.Archive.
	Archived   bool       `json:"archived"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`

	Rules []PolicyRule `json:"rules"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Active reports whether the policy participates in evaluation.
func (p PolicyDefinition) Active() bool { return p.Enabled && !p.Archived }

// PolicyRule is one typed check.
//
// The Type fixes the semantics at compile time. Values is the rule's parameter
// list and its MEANING is determined entirely by Type -- image patterns, label
// key patterns, environment variable NAME patterns, capability names, restart
// policy names, or network names. A rule whose type takes no parameters must
// carry none, which Validate enforces rather than ignoring.
type PolicyRule struct {
	Type PolicyRuleType `json:"type"`
	// Severity overrides the policy's default for this one rule. Empty
	// inherits, which is the common case.
	Severity PolicySeverity `json:"severity,omitempty"`
	Values   []string       `json:"values,omitempty"`
}

// EffectiveSeverity resolves the rule's severity against its policy's default.
func (r PolicyRule) EffectiveSeverity(policy PolicySeverity) PolicySeverity {
	if r.Severity != "" {
		return r.Severity
	}
	if policy != "" {
		return policy
	}
	return PolicySeverityMedium
}

// NewPolicyID generates an immutable public policy identifier.
//
// Random rather than sequential: the id appears in URLs and in violation rows,
// and a sequential one would leak how many policies exist and invite a caller
// to walk them. crypto/rand rather than math/rand because this is an
// identifier that must not be predictable, and the failure mode of a weak one
// is a caller guessing a policy id it was never shown.
//
// Panics only if the system entropy source fails, which on every supported
// platform means the process cannot safely continue anyway. crypto/rand.Read
// has not returned an error on any supported platform since Go 1.24.
func NewPolicyID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return "pol_" + hex.EncodeToString(raw[:])
}

// ------------------------------------------------------------- violations --

// PolicyViolation is one rule that a container fails.
//
// One row per FAILED RULE, not per offending item: a forbidden-environment rule
// listing four patterns against a container carrying three of them is one
// violation naming the rule, with the offending NAMES rendered into Observed.
// A row per match would make a single misconfiguration look like a fleet-wide
// incident and would let one container flood the table.
type PolicyViolation struct {
	ID int64 `json:"id"`

	// PolicyID is the immutable policy identifier, and PolicyName is
	// denormalised so a list response needs no join and stays readable after a
	// policy is archived and renamed.
	PolicyID   string `json:"policyId"`
	PolicyName string `json:"policyName"`

	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`

	// RuleType is the rule that failed. Part of the identity: see
	// PolicyViolationIdentity.
	RuleType PolicyRuleType `json:"ruleType"`
	Severity PolicySeverity `json:"severity"`

	// DetectedAt is when the violation was FIRST seen and does not move when a
	// later evaluation sees it again, so the age of a violation is the age of
	// the non-compliance.
	DetectedAt time.Time  `json:"detectedAt"`
	LastSeenAt time.Time  `json:"lastSeenAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
	// InventoryGeneration is the inventory the evaluation read.
	InventoryGeneration int64 `json:"inventoryGeneration"`

	// Observed is what the container actually has, rendered by the rule.
	// NEVER an environment variable value: env rules render names.
	Observed string `json:"observed,omitempty"`
	// Expected is what the rule required, rendered from the rule's own
	// parameters.
	Expected string `json:"expected,omitempty"`
	// Reason is engine prose from a fixed set of phrases explaining the
	// failure. Never an error string and never caller free text.
	Reason string `json:"reason,omitempty"`

	Status PolicyViolationStatus `json:"status"`
	// Note carries an operator's comment from a status change. Length-bounded,
	// UTF-8 validated, and control-character free before it reaches the
	// database.
	Note            string     `json:"note,omitempty"`
	StatusChangedAt *time.Time `json:"statusChangedAt,omitempty"`
}

// PolicyViolationIdentity is what makes two observations the same violation.
//
// A violation is identified by the container, the policy, and the RULE TYPE --
// not by the offending values. That is what makes a container whose forbidden
// label keeps changing one long-lived record rather than a new record per
// evaluation, and it is what lets an operator's acknowledgement survive the
// next pass.
//
// The rule type rather than a rule index, because an index moves when an
// administrator reorders a policy's rules and history must not be re-keyed by
// an edit that changed nothing about the world. A policy may therefore carry at
// most one rule of each type, which PolicyDefinition.Validate enforces.
type PolicyViolationIdentity struct {
	ContainerID string
	PolicyID    string
	RuleType    PolicyRuleType
}

// Identity returns the violation's identity.
func (v PolicyViolation) Identity() PolicyViolationIdentity {
	return PolicyViolationIdentity{
		ContainerID: v.ContainerID,
		PolicyID:    v.PolicyID,
		RuleType:    v.RuleType,
	}
}

// PolicyEvaluation records one compliance pass over one container.
//
// Kept separate from the violations because a pass that found NOTHING is a fact
// worth storing: it is the difference between "this container is compliant" and
// "this container has never been evaluated". Reporting the second as the first
// is the worst thing a compliance dashboard can do.
type PolicyEvaluation struct {
	ContainerID   string    `json:"containerId"`
	ContainerName string    `json:"containerName"`
	EvaluatedAt   time.Time `json:"evaluatedAt"`

	InventoryGeneration int64 `json:"inventoryGeneration"`
	// PoliciesEvaluated is how many active policies the pass applied.
	PoliciesEvaluated int `json:"policiesEvaluated"`
	// RulesEvaluated is how many individual rules ran.
	RulesEvaluated int `json:"rulesEvaluated"`
	// ViolationCount is how many rules failed.
	ViolationCount int `json:"violationCount"`

	// Compliant is true only when the pass was COMPLETE and found nothing. An
	// incomplete pass is never reported as compliant: it did not establish
	// compliance, it stopped looking.
	Compliant bool `json:"compliant"`
	// Complete reports that every active policy was applied.
	Complete bool `json:"complete"`
	// Reason explains an incomplete pass, from a fixed set of phrases.
	Reason string `json:"reason,omitempty"`
}

// PolicySummary is the compliance aggregate the dashboard renders.
//
// Computed by grouped aggregate queries rather than by counting a list in
// memory: the summary is what a dashboard polls, so it is the one that must
// stay cheap on a host with a thousand containers.
type PolicySummary struct {
	// Policies counts definitions that are enabled and unarchived; of those,
	// PoliciesTotal counts every definition that still exists.
	Policies      int `json:"policies"`
	PoliciesTotal int `json:"policiesTotal"`

	// Total counts every violation in any status; Open counts those that are
	// not resolved.
	Total int `json:"total"`
	Open  int `json:"open"`

	BySeverity map[PolicySeverity]int        `json:"bySeverity"`
	ByStatus   map[PolicyViolationStatus]int `json:"byStatus"`
	ByRule     map[PolicyRuleType]int        `json:"byRule"`

	// ContainersEvaluated counts containers a pass has been attempted for, and
	// ContainersCompliant those whose most recent COMPLETE pass found nothing.
	// Reported side by side so a UI can say "38 of 40 evaluated containers are
	// compliant" without implying anything about the two it never checked.
	ContainersEvaluated    int `json:"containersEvaluated"`
	ContainersCompliant    int `json:"containersCompliant"`
	ContainersNonCompliant int `json:"containersNonCompliant"`

	LastEvaluatedAt *time.Time `json:"lastEvaluatedAt,omitempty"`
	// Incomplete reports that at least one container's most recent pass could
	// not apply every policy, so these counts are a floor rather than a total.
	Incomplete bool `json:"incomplete"`
}

// ComplianceRate returns the share of evaluated containers that are compliant,
// in the range 0..1, or zero when nothing has been evaluated.
//
// Computed over EVALUATED containers rather than over the estate. A rate whose
// denominator silently included containers nobody checked would improve every
// time evaluation coverage got worse.
func (s PolicySummary) ComplianceRate() float64 {
	if s.ContainersEvaluated <= 0 {
		return 0
	}
	return float64(s.ContainersCompliant) / float64(s.ContainersEvaluated)
}

// PolicyEngineStatus reports the evaluation queue's state.
//
// Surfaced for the same reason the drift engine's is: an overflowed queue means
// the estate is being evaluated by sweep rather than per container, which is
// correct but slower, and an operator should be able to see that rather than
// infer it from stale timestamps.
type PolicyEngineStatus struct {
	Enabled bool `json:"enabled"`
	// PolicyCount is how many policies are active.
	PolicyCount int `json:"policyCount"`
	// PendingEvaluations is how many containers are waiting out their debounce
	// window.
	PendingEvaluations int `json:"pendingEvaluations"`
	// SweepPending reports that a full pass is owed.
	SweepPending bool `json:"sweepPending"`
	// Overflowed reports that the queue hit its cap and escalated. Cleared
	// when the resulting sweep completes.
	Overflowed bool `json:"overflowed"`
}
