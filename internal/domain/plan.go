package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Change plans.
//
// # What a plan is
//
// A plan answers one question about one container: "if you moved this onto the
// image update HarborMaster found, how risky would that be, and what should you
// do about it?" It is an ASSESSMENT, produced by combining data HarborMaster
// already holds -- inventory, snapshots, restore readiness, drift, policy
// compliance, and image intelligence.
//
// # What a plan is not
//
// It is not an instruction, a script, or a queue entry. Nothing executes a
// plan. HarborMaster cannot pull an image, recreate a container, or roll one
// back, and generating a plan does not bring it closer to being able to: the
// planner reads six existing data sources and writes one row.
//
// The word "plan" is chosen carefully. An operator reads it and then acts with
// their own tooling; HarborMaster's contribution is the analysis, not the
// action.
//
// # Immutability
//
// A plan records what was known AT ONE MOMENT. It is never updated in place --
// a changed world produces a NEW plan, and the old one remains as the record of
// what was believed when a decision was made. That is what makes a plan usable
// as evidence afterwards, and it is enforced by the schema rather than by
// convention.

// RiskBand ranks how risky a proposed change is.
//
// Five bands, from a deterministic integer score. The bands exist because a
// number alone invites false precision: "47" implies a measurement, while
// "medium" is honest about being a judgement built from named factors.
type RiskBand string

// Risk bands.
const (
	RiskVeryLow  RiskBand = "veryLow"
	RiskLow      RiskBand = "low"
	RiskMedium   RiskBand = "medium"
	RiskHigh     RiskBand = "high"
	RiskCritical RiskBand = "critical"
)

// RiskBands lists every band, least to most risky.
var RiskBands = []RiskBand{
	RiskVeryLow, RiskLow, RiskMedium, RiskHigh, RiskCritical,
}

// ValidRiskBand reports whether name is a known band.
func ValidRiskBand(name string) bool {
	for _, band := range RiskBands {
		if string(band) == name {
			return true
		}
	}
	return false
}

// Rank orders bands by how much they matter, higher being riskier.
func (b RiskBand) Rank() int {
	switch b {
	case RiskCritical:
		return 5
	case RiskHigh:
		return 4
	case RiskMedium:
		return 3
	case RiskLow:
		return 2
	case RiskVeryLow:
		return 1
	default:
		return 0
	}
}

// Risk band thresholds.
//
// Stated as constants rather than buried in a switch, because these numbers are
// the whole calibration of the model and a reviewer should be able to see them
// in one place. A score at or above each bound falls in that band.
const (
	RiskThresholdLow      = 10
	RiskThresholdMedium   = 25
	RiskThresholdHigh     = 50
	RiskThresholdCritical = 80
	// MaxRiskScore clamps the total. Without it, a container that is failing in
	// every possible way produces an arbitrarily large number that says nothing
	// more than "critical" already did.
	MaxRiskScore = 100
)

// BandForScore maps a score to its band.
//
// The single place the mapping exists, so the API, the UI, and the tests cannot
// disagree about where a boundary is.
func BandForScore(score int) RiskBand {
	switch {
	case score >= RiskThresholdCritical:
		return RiskCritical
	case score >= RiskThresholdHigh:
		return RiskHigh
	case score >= RiskThresholdMedium:
		return RiskMedium
	case score >= RiskThresholdLow:
		return RiskLow
	default:
		return RiskVeryLow
	}
}

// Recommendation is what HarborMaster suggests an operator do.
type Recommendation string

// Recommendations.
const (
	// RecommendProceed means nothing in the available evidence argues against
	// the change.
	RecommendProceed Recommendation = "proceed"
	// RecommendCaution means the change is probably fine but something is
	// worth reading first.
	RecommendCaution Recommendation = "proceedWithCaution"
	// RecommendManualReview means a person should look before acting.
	RecommendManualReview Recommendation = "manualReview"
	// RecommendAgainst means the evidence argues against the change as things
	// stand.
	RecommendAgainst Recommendation = "notRecommended"
	// RecommendUnknown means HarborMaster does not know enough to advise.
	//
	// Deliberately NOT the same as "proceed". A gap in evidence is not a clean
	// bill of health, and collapsing the two would be the single most
	// misleading thing this feature could do.
	RecommendUnknown Recommendation = "unknown"
)

// Recommendations lists every value.
var Recommendations = []Recommendation{
	RecommendProceed, RecommendCaution, RecommendManualReview,
	RecommendAgainst, RecommendUnknown,
}

// ValidRecommendation reports whether name is a known recommendation.
func ValidRecommendation(name string) bool {
	for _, recommendation := range Recommendations {
		if string(recommendation) == name {
			return true
		}
	}
	return false
}

// FactorSeverity is how much one contributing factor matters.
//
// Separate from its point contribution, because the two answer different
// questions: points decide the band, severity decides the recommendation. A
// factor can add few points and still block -- a critical policy violation is
// not made acceptable by everything else being fine.
type FactorSeverity string

// Factor severities.
const (
	// FactorInfo records something worth stating that argues neither way.
	FactorInfo FactorSeverity = "info"
	// FactorCaution records a reason to read before acting.
	FactorCaution FactorSeverity = "caution"
	// FactorWarning records something that should pull a human in.
	FactorWarning FactorSeverity = "warning"
	// FactorBlocker records something that argues against the change on its
	// own, whatever the score.
	FactorBlocker FactorSeverity = "blocker"
	// FactorUnknown records missing evidence. It forces the recommendation to
	// unknown rather than allowing a confident answer built on a gap.
	FactorUnknown FactorSeverity = "unknown"
)

// FactorSeverities lists every value.
var FactorSeverities = []FactorSeverity{
	FactorInfo, FactorCaution, FactorWarning, FactorBlocker, FactorUnknown,
}

// RiskFactor is one named contribution to a plan's risk.
//
// Every factor carries the RULE that produced it, so a score is always
// traceable to a specific, named piece of code rather than to an opaque total.
type RiskFactor struct {
	Rule PlanRule `json:"rule"`
	// Points is this factor's contribution to the score. May be zero: a factor
	// can be worth STATING without being worth scoring, and the reasoning
	// timeline is more useful when it includes those.
	Points   int            `json:"points"`
	Severity FactorSeverity `json:"severity"`
	// Detail is the human-readable reason, from a fixed set of phrases built by
	// the rule. Never an error string, never registry text, never caller input.
	Detail string `json:"detail"`
}

// RiskAssessment is the verdict for one proposed change.
type RiskAssessment struct {
	Score          int            `json:"riskScore"`
	Band           RiskBand       `json:"riskBand"`
	Recommendation Recommendation `json:"recommendation"`
	// Summary is a one-sentence explanation of the recommendation.
	Summary string `json:"summary"`
	// Factors are the contributions in RULE ORDER, which is fixed at compile
	// time. Stable ordering is part of determinism: the same inputs must
	// produce a byte-identical explanation.
	Factors []RiskFactor `json:"factors"`
}

// ChangePlan is one immutable assessment of one proposed container change.
type ChangePlan struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// PlanID is the IMMUTABLE public identifier, generated server-side.
	PlanID string `json:"planId"`

	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`

	// CurrentImage and ProposedImage are familiar references, e.g.
	// "nginx:1.25". CurrentDigest and ProposedDigest are the manifest digests
	// each resolves to, when known.
	CurrentImage   string `json:"currentImage"`
	ProposedImage  string `json:"proposedImage"`
	CurrentDigest  string `json:"currentDigest,omitempty"`
	ProposedDigest string `json:"proposedDigest,omitempty"`
	// UpdateType is the classification image intelligence produced.
	UpdateType UpdateType `json:"updateType"`

	// SnapshotID is the baseline this container would have to fall back to.
	// Zero when none exists, which SnapshotAvailable states plainly.
	SnapshotID        int64           `json:"snapshotId,omitempty"`
	SnapshotAvailable bool            `json:"snapshotAvailable"`
	RestoreReadiness  ReadinessStatus `json:"restoreReadiness"`

	// DriftOpen and DriftMaxSeverity summarise active drift. The container's
	// drift RECORDS are not copied: a plan references the summary it acted on,
	// and the records have one home.
	DriftOpen        int           `json:"driftOpen"`
	DriftMaxSeverity DriftSeverity `json:"driftMaxSeverity,omitempty"`

	PolicyOpen        int            `json:"policyOpen"`
	PolicyMaxSeverity PolicySeverity `json:"policyMaxSeverity,omitempty"`

	// RegistryStatus is the quality of the registry data the plan rests on, and
	// RegistryDetail HarborMaster's own description of a non-OK one.
	RegistryStatus CheckStatus `json:"registryStatus"`
	RegistryDetail string      `json:"registryDetail,omitempty"`
	// ProposedPublishedAt is when the proposed image was built, when a
	// publisher recorded it.
	ProposedPublishedAt *time.Time `json:"proposedPublishedAt,omitempty"`

	Risk RiskAssessment `json:"risk"`

	// PlanVersion is the schema version of this plan's shape, and
	// PlannerVersion identifies the rule set that produced it. Both are stored
	// so a plan generated by an older planner is identifiable rather than
	// silently comparable with a newer one.
	PlanVersion    int    `json:"planVersion"`
	PlannerVersion string `json:"plannerVersion"`

	// InputDigest fingerprints everything the assessment read. It is what makes
	// "avoid regenerating unchanged plans" exact rather than approximate: two
	// runs over an unchanged world produce the same digest and the second
	// writes nothing.
	InputDigest string `json:"inputDigest"`

	GeneratedAt time.Time `json:"generatedAt"`
	// Superseded reports that a newer plan exists for this container. Derived
	// at read time rather than stored, because storing it would mean writing to
	// an immutable row.
	//
	// Note what this is NOT: it is not "this plan is no longer current". A plan
	// can stop being current with no newer plan behind it -- see the currency
	// contract below -- so a reader deciding whether to ACT on a plan must ask
	// for the current plan rather than trust this flag's absence.
	Superseded bool `json:"superseded"`
}

// # The ChangePlan currency contract
//
// A plan is an IMMUTABLE ASSESSMENT: what HarborMaster believed about one
// container at one moment. It is never updated in place, it is never a work
// item, and it is never deleted while anything references it. Migration 0008
// states this and deliberately gives the table no `status`, `applied_at` or
// `superseded` column.
//
// Four states matter, and only the first two are the same question:
//
//	HISTORICAL   every plan ever written. Readable through History and the
//	             plan endpoints forever, and what Activity reconstructs a past
//	             attempt from. Never mutated, whatever happens later.
//
//	CURRENT      the one plan, if any, that represents HarborMaster's decision
//	             about this container right now. Answered ONLY by
//	             PlanRepository.Current -- never by MAX(id) at a call site.
//
//	SUPERSEDED   a newer plan replaced it. Derived from the existence of that
//	             newer row, which is the `Superseded` flag above.
//
//	RETIRED      no newer plan exists, and the evidence this plan rests on has
//	             settled into the state where the planner would decline to write
//	             it at all: a registry comparison that ANSWERED and found
//	             nothing newer. The planner skips such a container permanently,
//	             so it never writes a superseding row -- which is exactly why
//	             "newest row" and "current decision" are different questions.
//
// A retired plan is still historical truth and still readable. "Retired" means
// this is no longer the decision, never that the decision was wrong or that an
// action taken under it did not happen.
//
// Consequences a reader must not re-derive for themselves:
//
//   - Currency is decided by PlanRepository.Current, which applies both ends.
//     Approval, acquisition, execution and automation all go through it.
//   - A plan that is not current cannot be newly approved and cannot start an
//     acquisition. Those gates read currency; they do not reimplement it.
//   - History queries legitimately want the newest RECORDED plan and should
//     keep using it -- duplicate suppression and the planner-version readout
//     are asking a different question and are correct as they are.
//
// # Currency is keyed by container ID, and a recreation starts a new lineage
//
// C2 and C3A made the container NAME HarborMaster's stable identity, and
// deliberately did not extend that here. The two are answering different
// questions.
//
// Name-keyed identity exists for things that must SURVIVE a recreation because
// they are the operator's settings or the workload's continuity: an automation
// pause, a container update preference, image lineage, and resolving which
// container id is current now. Losing any of those on the update they
// authorised would be the defect.
//
// A plan is not a setting. It is an assessment of ONE container as it was: its
// configuration snapshot, its open drift, its policy violations and its
// running digest, frozen together with the verdict they produced. The
// replacement is a different container with a different id, a different
// snapshot and its own evidence, so a plan written for the original is not an
// assessment of it. Making the replacement's first plan "supersede" the
// original's would be a claim about history -- that an assessment of one
// container was replaced by an assessment of another -- and change_plans is
// immutable evidence precisely so that cannot happen.
//
// So a recreation starts a NEW plan lineage, and the original's plans stay
// current-for-a-container-that-is-gone rather than becoming superseded. Nothing
// can act on them: every preflight re-resolves the container and refuses
// `containerMissing`, and the retirement predicate requires a PRESENT
// container, so an absent one is never retired on evidence either -- it is
// simply no longer a container anybody can act on.

// ErrPlanTargetCrossed reports a plan whose proposed reference and proposed
// digest were not resolved for each other.
var ErrPlanTargetCrossed = errors.New(
	"a change plan proposes a reference and a digest that do not belong together")

// ValidTarget reports whether the proposed pair is internally consistent.
//
// # What this catches
//
// A plan that names a reference but carries no digest, or carries a digest that
// is not a digest. Both are unpinnable, and acquisition is digest-pinned.
//
// # What it deliberately cannot catch
//
// Whether the digest is genuinely the one the registry serves for that
// reference. Nothing at this layer can know that -- only the registry lookup
// can, which is why the pair is built by domain.NewProposedTarget at the moment
// both are resolved and re-checked against a fresh lookup before any pull. This
// is the last cheap structural gate before persistence, not a substitute for
// either.
//
// A plan with NO proposal is valid: "nothing is on offer" is a legitimate
// assessment and the common one on a settled estate.
func (p ChangePlan) ValidTarget() bool {
	if p.ProposedImage == "" && p.ProposedDigest == "" {
		return true
	}
	if p.ProposedImage == "" || p.ProposedDigest == "" {
		return false
	}
	return ValidImageDigest(p.ProposedDigest)
}

// PlanSchemaVersion is the current plan shape.
//
// Bumped when the stored plan gains or loses a field, so an older row is
// recognisable. Distinct from PlannerVersion, which changes when the RULES
// change even though the shape does not.
//
// # Why this is 2
//
// Version 1 rows can carry a CROSSED proposed pair: a newer tag paired with the
// digest resolved for the tag it replaces. Those rows are not merely stale,
// they are wrong in the one field the whole acquisition path pins on, and an
// acquisition built from one would pull the old image and report the new one.
//
// Bumping the schema makes every stored fingerprint stale, which forces the
// next pass to regenerate every plan from evidence gathered by the fixed code.
// That is the migration: there is no way to repair a v1 row in place, because
// the digest it needed was never resolved.
const PlanSchemaVersion = 2

// ChangePlanSummary is the dashboard aggregate.
type ChangePlanSummary struct {
	// Plans counts the newest plan per container, and Containers how many
	// containers have one at all.
	Plans      int `json:"plans"`
	Containers int `json:"containers"`

	ByBand           map[RiskBand]int       `json:"byRiskBand"`
	ByRecommendation map[Recommendation]int `json:"byRecommendation"`
	ByUpdateType     map[UpdateType]int     `json:"byUpdateType"`

	// Actionable counts plans recommending proceed or proceed-with-caution --
	// the changes an operator could make today.
	Actionable int `json:"actionable"`
	// NeedsReview counts plans that ask for a person.
	NeedsReview int `json:"needsReview"`
	// Blocked counts plans recommending against the change.
	Blocked int `json:"blocked"`
	// Undetermined counts plans HarborMaster could not judge. Reported beside
	// the rest so a gap in evidence is visible rather than absorbed into
	// "proceed".
	Undetermined int `json:"undetermined"`

	LastGeneratedAt *time.Time `json:"lastGeneratedAt,omitempty"`
	// PlannerVersion is the rule set the newest plans were built with.
	PlannerVersion string `json:"plannerVersion,omitempty"`
}

// PlannerStatus reports the generation scheduler's state.
type PlannerStatus struct {
	Enabled        bool   `json:"enabled"`
	PlannerVersion string `json:"plannerVersion"`
	// Running reports that a generation pass is in flight.
	Running bool `json:"running"`
	// Pending reports that a pass is owed.
	Pending bool `json:"pending"`

	LastRunAt *time.Time `json:"lastRunAt,omitempty"`
	// Generated, Unchanged and Skipped describe the most recent pass.
	// Unchanged is the interesting one: it is how much work the fingerprint
	// saved.
	Generated int `json:"lastGenerated"`
	Unchanged int `json:"lastUnchanged"`
	Skipped   int `json:"lastSkipped"`
}

// NewPlanID generates an immutable public plan identifier.
//
// Random rather than sequential, for the same reason a policy id is: it appears
// in URLs, and a sequential one would leak how many plans exist and invite a
// caller to walk them.
//
// Panics only if the system entropy source fails, which on every supported
// platform means the process cannot safely continue anyway.
func NewPlanID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return "plan_" + hex.EncodeToString(raw[:])
}
