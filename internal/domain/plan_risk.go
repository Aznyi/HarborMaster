package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The risk engine.
//
// # Determinism is the requirement, not an aspiration
//
// The same inputs must produce a byte-identical assessment, on any machine, in
// any order, forever. Everything below follows from that:
//
//   - Rules are a FIXED SLICE of typed functions, iterated in order. There is
//     no rule registry, no configuration, and no data-driven dispatch -- adding
//     a rule means editing planRules, in a diff a reviewer sees.
//   - No map is ever ranged over. Go randomises map iteration order, so a
//     factor list built from one would differ between runs of the same input.
//   - Scores are integers. No floating point, so no platform-dependent
//     rounding.
//   - There is no clock, no randomness, and no I/O. Every time-dependent input
//     is passed IN as a resolved value, so a test can pin it.
//
// Nothing here is probabilistic, learned, or heuristic in the statistical
// sense. Each number is a judgement written down by a person and reviewable as
// such.
//
// # Why points and severity are separate
//
// Points decide the BAND. Severity decides the RECOMMENDATION. They answer
// different questions, and collapsing them would lose the case that matters
// most: a critical policy violation must argue against a change even when
// everything else about it is unremarkable, and a low total must not launder it
// into "proceed".

// PlannerVersion identifies the rule set.
//
// Bump it when a rule is added, removed, or re-weighted. Plans record it, so a
// plan produced by an older rule set is identifiable rather than silently
// compared against a newer one -- and a bump makes every stored fingerprint
// stale, which is what forces regeneration.
const PlannerVersion = "1"

// PlanRule names one rule. A closed vocabulary.
type PlanRule string

// Plan rules, in evaluation order.
const (
	RuleUpdateClassification PlanRule = "updateClassification"
	RuleMutableTag           PlanRule = "mutableTag"
	RuleUnknownDigest        PlanRule = "unknownDigest"
	RuleRegistryQuality      PlanRule = "registryQuality"
	RuleImageAge             PlanRule = "imageAge"
	RulePlatform             PlanRule = "platform"
	RuleSnapshotAvailable    PlanRule = "snapshotAvailable"
	RuleRestoreReadiness     PlanRule = "restoreReadiness"
	RuleActiveDrift          PlanRule = "activeDrift"
	RulePolicyViolations     PlanRule = "policyViolations"
	RuleBlastRadius          PlanRule = "blastRadius"
)

// PlanRules lists every rule in evaluation order.
//
// The order is the order factors appear in an explanation, so it is chosen to
// read as an argument: what the change IS, then what is known about it, then
// what the container's current state says about absorbing it.
var PlanRules = []PlanRule{
	RuleUpdateClassification,
	RuleMutableTag,
	RuleUnknownDigest,
	RuleRegistryQuality,
	RuleImageAge,
	RulePlatform,
	RuleSnapshotAvailable,
	RuleRestoreReadiness,
	RuleActiveDrift,
	RulePolicyViolations,
	RuleBlastRadius,
}

// ValidPlanRule reports whether name is a known rule.
func ValidPlanRule(name string) bool {
	for _, rule := range PlanRules {
		if string(rule) == name {
			return true
		}
	}
	return false
}

// PlanInputs is everything the rules read.
//
// A single struct rather than a set of arguments, so adding an input is a
// compile-time change everywhere it matters, and so the fingerprint below can
// be derived from exactly the values the rules saw. There is no clock here:
// EvaluatedAt is supplied, which is what lets a test pin every time-dependent
// rule.
type PlanInputs struct {
	ContainerID   string
	ContainerName string

	CurrentImage   string
	ProposedImage  string
	CurrentDigest  string
	ProposedDigest string
	// CurrentTag is the tag the container runs, used to detect a channel tag.
	CurrentTag string
	UpdateType UpdateType

	SnapshotID       int64
	RestoreReadiness ReadinessStatus

	DriftOpen        int
	DriftMaxSeverity DriftSeverity

	PolicyOpen        int
	PolicyMaxSeverity PolicySeverity

	RegistryStatus CheckStatus
	RegistryDetail string
	// LocalPlatform is what the container runs on.
	LocalPlatform Platform
	// ProposedPlatformMissing reports that the most recent successful registry
	// lookup found the proposed image advertising platforms, but not the one in
	// use. A boolean rather than a second Platform because that is exactly what
	// the lookup can establish: the alternative -- comparing two platform values
	// -- would compare a value against itself, since HarborMaster asks the
	// registry for the local platform in the first place.
	ProposedPlatformMissing bool

	// ProposedPublishedAt is when the proposed image was built, when a
	// publisher recorded it.
	ProposedPublishedAt *time.Time
	// ContainerCount is how many present containers run the current image --
	// the blast radius of acting on this plan everywhere.
	ContainerCount int
	// CurrentDigestDetail says WHY CurrentDigest is empty, when it is: an image
	// built on this host, a repository the local image reports ambiguously, or
	// a reference naming no registry.
	//
	// Carried so the unknown-digest factor names the actual situation instead
	// of restating the absence. Empty falls back to the generic phrasing.
	CurrentDigestDetail string

	// EvaluatedAt is the moment the assessment is made. Passed in rather than
	// read from a clock, so image age is reproducible.
	EvaluatedAt time.Time
}

// planRule is one rule's implementation.
//
// A struct of a name and a function, held in a fixed slice. This is what "rules
// are compile-time typed" means in practice: there is no way to add one at
// runtime, from configuration, or from an API request.
type planRule struct {
	id    PlanRule
	apply func(PlanInputs) (RiskFactor, bool)
}

// planRules is the rule set, in evaluation order.
//
// Deliberately a slice and not a map: a map would randomise the order of the
// explanation, and a reproducible score with an irreproducible explanation is
// not reproducible.
var planRules = []planRule{
	{RuleUpdateClassification, assessUpdateClassification},
	{RuleMutableTag, assessMutableTag},
	{RuleUnknownDigest, assessUnknownDigest},
	{RuleRegistryQuality, assessRegistryQuality},
	{RuleImageAge, assessImageAge},
	{RulePlatform, assessPlatform},
	{RuleSnapshotAvailable, assessSnapshotAvailable},
	{RuleRestoreReadiness, assessRestoreReadiness},
	{RuleActiveDrift, assessActiveDrift},
	{RulePolicyViolations, assessPolicyViolations},
	{RuleBlastRadius, assessBlastRadius},
}

// AssessRisk evaluates every rule and produces the verdict.
//
// Pure: no clock, no randomness, no I/O, no map iteration. Given the same
// inputs it returns the same score, the same band, the same recommendation, and
// the same factors in the same order.
func AssessRisk(inputs PlanInputs) RiskAssessment {
	factors := make([]RiskFactor, 0, len(planRules))
	score := 0

	for _, rule := range planRules {
		factor, applies := rule.apply(inputs)
		if !applies {
			continue
		}
		factor.Rule = rule.id
		factors = append(factors, factor)
		score += factor.Points
	}

	if score < 0 {
		score = 0
	}
	if score > MaxRiskScore {
		score = MaxRiskScore
	}

	band := BandForScore(score)
	recommendation := recommend(band, factors)

	return RiskAssessment{
		Score:          score,
		Band:           band,
		Recommendation: recommendation,
		Summary:        summarise(recommendation, band, factors),
		Factors:        factors,
	}
}

// recommend turns a band and its factors into advice.
//
// Severity OVERRIDES the band in both directions that matter:
//
//   - A missing-evidence factor forces "unknown". A confident answer built on a
//     gap is worse than admitting the gap, and this is the case where a
//     scoring model would otherwise quietly report "proceed".
//   - A blocker forces "not recommended" whatever the total. A critical policy
//     violation is not made acceptable by everything else being fine.
//
// Otherwise the band decides, with a warning factor pulling a medium up to
// manual review.
func recommend(band RiskBand, factors []RiskFactor) Recommendation {
	var (
		hasUnknown bool
		hasBlocker bool
		hasWarning bool
	)
	for _, factor := range factors {
		switch factor.Severity {
		case FactorUnknown:
			hasUnknown = true
		case FactorBlocker:
			hasBlocker = true
		case FactorWarning:
			hasWarning = true
		}
	}

	switch {
	case hasBlocker:
		return RecommendAgainst
	case hasUnknown:
		return RecommendUnknown
	}

	switch band {
	case RiskCritical:
		return RecommendAgainst
	case RiskHigh:
		return RecommendManualReview
	case RiskMedium:
		if hasWarning {
			return RecommendManualReview
		}
		return RecommendCaution
	default:
		if hasWarning {
			return RecommendCaution
		}
		return RecommendProceed
	}
}

// summarise writes the one-sentence explanation.
//
// Built from the recommendation and the single heaviest factor, so the sentence
// names the actual reason rather than restating the score. Selection is by
// severity then points then RULE ORDER, all of which are deterministic -- no
// ties are broken arbitrarily.
func summarise(recommendation Recommendation, band RiskBand, factors []RiskFactor) string {
	lead := dominantFactor(factors)

	switch recommendation {
	case RecommendAgainst:
		if lead != nil {
			return "Not recommended: " + lead.Detail
		}
		return "Not recommended: the assessed risk is " + string(band) + "."
	case RecommendUnknown:
		if lead != nil {
			return "Cannot advise: " + lead.Detail
		}
		return "Cannot advise: not enough is known about this change."
	case RecommendManualReview:
		if lead != nil {
			return "Worth a look first: " + lead.Detail
		}
		return "Worth a look first: the assessed risk is " + string(band) + "."
	case RecommendCaution:
		if lead != nil {
			return "Probably fine, but note: " + lead.Detail
		}
		return "Probably fine; the assessed risk is " + string(band) + "."
	default:
		if lead != nil && lead.Points > 0 {
			return "Nothing argues against this change. The main consideration: " + lead.Detail
		}
		return "Nothing in the available evidence argues against this change."
	}
}

// dominantFactor picks the factor that best explains the verdict.
func dominantFactor(factors []RiskFactor) *RiskFactor {
	var best *RiskFactor
	bestRank := -1

	for index := range factors {
		factor := &factors[index]
		rank := severityRank(factor.Severity)*1000 + factor.Points
		// Strictly greater, so an earlier rule wins a tie. Rule order is fixed,
		// which makes the choice reproducible.
		if rank > bestRank {
			best, bestRank = factor, rank
		}
	}
	return best
}

func severityRank(severity FactorSeverity) int {
	switch severity {
	case FactorBlocker:
		return 5
	case FactorUnknown:
		return 4
	case FactorWarning:
		return 3
	case FactorCaution:
		return 2
	case FactorInfo:
		return 1
	default:
		return 0
	}
}

// ------------------------------------------------------------------ rules --

// assessUpdateClassification scores the size of the version change.
//
// The primary input. A major version is the only classification that carries an
// explicit promise of breaking changes, so it dominates; a patch carries the
// opposite promise and scores accordingly.
func assessUpdateClassification(inputs PlanInputs) (RiskFactor, bool) {
	switch inputs.UpdateType {
	case UpdateMajor:
		return RiskFactor{
			Points:   40,
			Severity: FactorWarning,
			Detail: "this is a major version change, which by convention is " +
				"allowed to break compatibility",
		}, true

	case UpdateMinor:
		return RiskFactor{
			Points:   15,
			Severity: FactorCaution,
			Detail:   "this is a minor version change, which may add behaviour",
		}, true

	case UpdatePatch:
		return RiskFactor{
			Points:   5,
			Severity: FactorInfo,
			Detail:   "this is a patch version change",
		}, true

	case UpdatePrerelease:
		return RiskFactor{
			Points:   35,
			Severity: FactorWarning,
			Detail: "the only newer release is a pre-release, which the " +
				"publisher has not declared ready",
		}, true

	case UpdateDigest:
		return RiskFactor{
			Points:   12,
			Severity: FactorCaution,
			Detail: "the tag has not changed but its content has; the publisher " +
				"republished it, and what changed is not described by a version",
		}, true

	case UpdateUnknown:
		return RiskFactor{
			Points:   20,
			Severity: FactorUnknown,
			Detail: "the size of this change could not be determined, so its " +
				"impact is unknown rather than small",
		}, true

	default:
		return RiskFactor{
			Points:   0,
			Severity: FactorInfo,
			Detail:   "no update was found for this image",
		}, true
	}
}

// assessMutableTag flags a container tracking a channel rather than a version.
//
// A channel tag means the operator has already accepted that content moves
// underneath them. That is a legitimate choice, but it means this plan cannot
// say WHAT changed -- only that something did.
func assessMutableTag(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.CurrentTag == "" || !IsMutableTag(inputs.CurrentTag) {
		return RiskFactor{}, false
	}
	return RiskFactor{
		Points:   12,
		Severity: FactorCaution,
		Detail: "the container tracks the mutable tag \"" + inputs.CurrentTag +
			"\", so the change cannot be described as a version difference",
	}, true
}

// assessUnknownDigest flags a comparison that could not be made.
//
// Missing on either side means the plan rests on the tag alone. That is a gap
// in evidence rather than a risk, so it is severity-unknown: it forces the
// recommendation to "unknown" instead of quietly reducing confidence.
func assessUnknownDigest(inputs PlanInputs) (RiskFactor, bool) {
	switch {
	case inputs.CurrentDigest == "" && inputs.ProposedDigest == "":
		return RiskFactor{
			Points:   18,
			Severity: FactorUnknown,
			Detail: "neither the running image nor the proposed one has a known " +
				"digest, so there is nothing to compare",
		}, true

	case inputs.CurrentDigest == "":
		// The reason, when the image check established one. "This image was
		// built on this host" is a different situation from "this image reports
		// two conflicting digests", and an operator can act on the second.
		detail := "the running image has no registry digest, so HarborMaster " +
			"cannot confirm what is actually deployed"
		if inputs.CurrentDigestDetail != "" {
			detail = inputs.CurrentDigestDetail +
				", so HarborMaster cannot confirm what is actually deployed"
		}
		return RiskFactor{
			Points:   15,
			Severity: FactorUnknown,
			Detail:   detail,
		}, true

	case inputs.ProposedDigest == "":
		return RiskFactor{
			Points:   12,
			Severity: FactorUnknown,
			Detail:   "the proposed image has no resolved digest",
		}, true
	}
	return RiskFactor{}, false
}

// assessRegistryQuality scores how much the registry data can be relied on.
//
// A plan is only as good as the lookup behind it. A failed or stale lookup does
// not make a change risky, it makes the ASSESSMENT unreliable -- which is why
// the failure modes here are severity-unknown rather than warnings.
func assessRegistryQuality(inputs PlanInputs) (RiskFactor, bool) {
	switch inputs.RegistryStatus {
	case CheckOK:
		return RiskFactor{}, false

	case CheckPending:
		return RiskFactor{
			Points:   20,
			Severity: FactorUnknown,
			Detail: "this image has never been checked against its registry, so " +
				"there is no evidence to assess",
		}, true

	case CheckFailed, CheckRateLimited:
		return RiskFactor{
			Points:   15,
			Severity: FactorUnknown,
			Detail: "the most recent registry lookup did not succeed, so this " +
				"plan may rest on stale information",
		}, true

	case CheckUnauthorized, CheckNotFound:
		return RiskFactor{
			Points:   15,
			Severity: FactorUnknown,
			Detail: "the registry did not answer for this repository, so the " +
				"proposed image could not be verified",
		}, true

	case CheckUnsupported:
		return RiskFactor{
			Points:   15,
			Severity: FactorUnknown,
			Detail: "this image reference cannot be looked up, so no registry " +
				"evidence is available",
		}, true
	}
	return RiskFactor{}, false
}

// freshImageWindow is how new a published image has to be to count as
// untested.
//
// Not a claim that new images are bad -- a claim that an image published hours
// ago has had less exposure than one published weeks ago, which is worth
// stating on a plan an operator is about to act on.
const freshImageWindow = 48 * time.Hour

// assessImageAge scores how long the proposed image has been in the world.
func assessImageAge(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.ProposedPublishedAt == nil {
		// Absent provenance is common and unremarkable -- most publishers do
		// not set the annotation. Stated as information, and deliberately NOT
		// scored: penalising every image whose publisher omits a label would
		// make the model measure metadata hygiene rather than risk.
		return RiskFactor{
			Points:   0,
			Severity: FactorInfo,
			Detail:   "the proposed image does not record when it was published",
		}, true
	}

	age := inputs.EvaluatedAt.Sub(*inputs.ProposedPublishedAt)
	if age < freshImageWindow {
		return RiskFactor{
			Points:   8,
			Severity: FactorCaution,
			Detail: "the proposed image was published less than 48 hours ago, so " +
				"it has had little exposure",
		}, true
	}

	return RiskFactor{
		Points:   0,
		Severity: FactorInfo,
		Detail:   "the proposed image was published " + describeAge(age) + " ago",
	}, true
}

// describeAge renders a duration in whole units.
//
// Whole units only, and no clock: the same duration always renders the same
// string, which the fingerprint depends on.
func describeAge(age time.Duration) string {
	switch {
	case age < 0:
		return "less than a day"
	case age < 24*time.Hour:
		return "less than a day"
	case age < 14*24*time.Hour:
		return strconv.Itoa(int(age.Hours()/24)) + " days"
	case age < 365*24*time.Hour:
		return strconv.Itoa(int(age.Hours()/24/7)) + " weeks"
	default:
		return strconv.Itoa(int(age.Hours()/24/365)) + " years"
	}
}

// assessPlatform flags a proposed image that does not publish the platform in
// use.
//
// The one rule that can say the change would not merely misbehave but not run
// at all, which is why it is a warning rather than a caution.
//
// Only meaningful when the registry actually answered. A lookup that failed
// establishes nothing about platform support, and reporting silence as a
// mismatch would both double-count the failure assessRegistryQuality already
// scored and turn "we could not ask" into "the answer was no".
func assessPlatform(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.RegistryStatus != CheckOK || !inputs.ProposedPlatformMissing {
		return RiskFactor{}, false
	}

	platform := inputs.LocalPlatform.String()
	if platform == "" {
		platform = "the platform in use"
	} else {
		platform = "the " + platform + " platform"
	}
	return RiskFactor{
		Points:   20,
		Severity: FactorWarning,
		Detail: "the proposed image does not publish " + platform +
			" this container runs on",
	}, true
}

// assessSnapshotAvailable scores whether there is anything to fall back to.
//
// A snapshot is the record of what the container looked like before. Without
// one, an operator who changes the image and dislikes the result has no
// HarborMaster evidence of what to put back -- which is a property of the
// CHANGE, not of the image.
func assessSnapshotAvailable(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.SnapshotID > 0 {
		return RiskFactor{
			Points:   0,
			Severity: FactorInfo,
			Detail:   "a configuration snapshot exists for this container",
		}, true
	}
	return RiskFactor{
		Points:   25,
		Severity: FactorWarning,
		Detail: "no configuration snapshot exists for this container, so there " +
			"is no record of its current configuration to refer back to",
	}, true
}

// assessRestoreReadiness scores what the readiness check established.
//
// Only meaningful when a snapshot exists; without one, assessSnapshotAvailable
// has already made the point and scoring it twice would double-count.
func assessRestoreReadiness(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.SnapshotID == 0 {
		return RiskFactor{}, false
	}

	switch inputs.RestoreReadiness {
	case ReadinessReady:
		return RiskFactor{
			Points:   0,
			Severity: FactorInfo,
			Detail:   "the snapshot's restore readiness check passed",
		}, true

	case ReadinessWarning:
		return RiskFactor{
			Points:   10,
			Severity: FactorCaution,
			Detail: "the snapshot's restore readiness check raised a warning, so " +
				"falling back may need attention",
		}, true

	case ReadinessNotReady:
		return RiskFactor{
			Points:   22,
			Severity: FactorWarning,
			Detail: "the snapshot's restore readiness check failed, so the " +
				"recorded configuration may not be usable as a reference point",
		}, true

	default:
		return RiskFactor{
			Points:   8,
			Severity: FactorCaution,
			Detail: "the snapshot's restore readiness has not been evaluated, so " +
				"its usefulness as a reference point is unverified",
		}, true
	}
}

// assessActiveDrift scores how far the container has already moved from its
// baseline.
//
// A drifting container is a container whose deployed state is not fully
// described by anything HarborMaster recorded. Changing its image on top of
// that compounds the uncertainty: if the result misbehaves, there are now two
// candidate causes.
func assessActiveDrift(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.DriftOpen == 0 {
		return RiskFactor{
			Points:   0,
			Severity: FactorInfo,
			Detail:   "the container matches its baseline snapshot",
		}, true
	}

	// Declared without a value: every branch below assigns both, and a default
	// here would be a value no path can use.
	var (
		points   int
		severity FactorSeverity
	)
	switch inputs.DriftMaxSeverity {
	case DriftSeverityCritical:
		points, severity = 25, FactorWarning
	case DriftSeverityHigh:
		points, severity = 15, FactorCaution
	case DriftSeverityMedium:
		points, severity = 8, FactorCaution
	default:
		points, severity = 3, FactorInfo
	}

	return RiskFactor{
		Points:   points,
		Severity: severity,
		Detail: countPhrase(inputs.DriftOpen, "unresolved difference", "unresolved differences") +
			" from the baseline snapshot (most severe: " +
			string(inputs.DriftMaxSeverity) + "), so the deployed state is not " +
			"fully described by what was recorded",
	}, true
}

// assessPolicyViolations scores the container's current compliance.
//
// A CRITICAL violation is the model's only blocker. The reasoning is narrow and
// deliberate: an organisation that marked a rule critical has already said this
// container should not be running as it is, and a change plan is not the place
// to overrule that. It does not say the change is dangerous -- it says the
// container has a prior problem that should be settled first.
func assessPolicyViolations(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.PolicyOpen == 0 {
		return RiskFactor{
			Points:   0,
			Severity: FactorInfo,
			Detail:   "the container complies with every enabled policy",
		}, true
	}

	var (
		points   int
		severity FactorSeverity
	)
	switch inputs.PolicyMaxSeverity {
	case PolicySeverityCritical:
		points, severity = 30, FactorBlocker
	case PolicySeverityHigh:
		points, severity = 18, FactorWarning
	case PolicySeverityMedium:
		points, severity = 8, FactorCaution
	default:
		points, severity = 3, FactorInfo
	}

	return RiskFactor{
		Points:   points,
		Severity: severity,
		Detail: countPhrase(inputs.PolicyOpen, "open policy violation", "open policy violations") +
			" (most severe: " + string(inputs.PolicyMaxSeverity) +
			"), which should be settled before changing the image",
	}, true
}

// blastRadiusThreshold is how many containers make a change estate-wide rather
// than local.
const blastRadiusThreshold = 5

// assessBlastRadius scores how much of the estate one image change touches.
//
// Not a property of the image at all -- a property of acting on the plan. Ten
// containers on one image means one bad update takes out ten things at once,
// and that is worth a line on the plan even though it changes nothing about the
// image itself.
func assessBlastRadius(inputs PlanInputs) (RiskFactor, bool) {
	if inputs.ContainerCount <= blastRadiusThreshold {
		return RiskFactor{}, false
	}
	return RiskFactor{
		Points:   6,
		Severity: FactorCaution,
		Detail: strconv.Itoa(inputs.ContainerCount) + " containers run this image, " +
			"so acting on this plan everywhere would change all of them at once",
	}, true
}

// countPhrase renders "1 thing" or "N things".
func countPhrase(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(count) + " " + plural
}

// ------------------------------------------------------------ fingerprint --

// Fingerprint derives a stable digest of everything an assessment read.
//
// # What it is for
//
// Two generation passes over an unchanged world must produce the same
// fingerprint, so the second writes nothing. That is what "avoid regenerating
// unchanged plans" means exactly rather than approximately, and it is what
// keeps a table of plans from growing on every pass.
//
// # Why it is built the way it is
//
// The input is a SORTED list of explicit key=value lines, hashed with SHA-256.
// Sorting removes any dependence on field order; naming each field means adding
// one changes the digest for the right reason rather than by accident; and the
// planner version is included, so bumping the rule set invalidates every stored
// fingerprint and forces a clean regeneration.
//
// EvaluatedAt is deliberately EXCLUDED. Including a clock would make every
// fingerprint unique and defeat the whole mechanism. Time-dependent rules read
// it, which is a known and accepted limitation: an image crossing the 48-hour
// freshness boundary does not by itself trigger a new plan.
func (i PlanInputs) Fingerprint() string {
	fields := []string{
		"planner=" + PlannerVersion,
		"schema=" + strconv.Itoa(PlanSchemaVersion),
		"container=" + i.ContainerID,
		"currentImage=" + i.CurrentImage,
		"proposedImage=" + i.ProposedImage,
		"currentDigest=" + i.CurrentDigest,
		"proposedDigest=" + i.ProposedDigest,
		"currentTag=" + i.CurrentTag,
		"update=" + string(i.UpdateType),
		"snapshot=" + strconv.FormatInt(i.SnapshotID, 10),
		"readiness=" + string(i.RestoreReadiness),
		"driftOpen=" + strconv.Itoa(i.DriftOpen),
		"driftSeverity=" + string(i.DriftMaxSeverity),
		"policyOpen=" + strconv.Itoa(i.PolicyOpen),
		"policySeverity=" + string(i.PolicyMaxSeverity),
		"registryStatus=" + string(i.RegistryStatus),
		"localPlatform=" + i.LocalPlatform.String(),
		"platformMissing=" + strconv.FormatBool(i.ProposedPlatformMissing),
		"published=" + formatFingerprintTime(i.ProposedPublishedAt),
		"containers=" + strconv.Itoa(i.ContainerCount),
	}
	sort.Strings(fields)

	sum := sha256.Sum256([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(sum[:])
}

// formatFingerprintTime renders an optional timestamp for the fingerprint.
//
// UTC and RFC3339, so the same instant expressed in two zones produces one
// string rather than two fingerprints.
func formatFingerprintTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// Validate reports whether an assessment is internally consistent.
//
// Used by tests and by the repository as a backstop. A score outside its band,
// or a band outside the vocabulary, means the model and the storage have
// diverged -- which would show up as a dashboard that sorts wrongly rather than
// as an error, so it is worth catching loudly.
func (r RiskAssessment) Validate() error {
	if r.Score < 0 || r.Score > MaxRiskScore {
		return fmt.Errorf("risk score %d is outside 0..%d", r.Score, MaxRiskScore)
	}
	if !ValidRiskBand(string(r.Band)) {
		return fmt.Errorf("unknown risk band %q", r.Band)
	}
	if band := BandForScore(r.Score); band != r.Band {
		return fmt.Errorf("score %d belongs in band %q, not %q", r.Score, band, r.Band)
	}
	if !ValidRecommendation(string(r.Recommendation)) {
		return fmt.Errorf("unknown recommendation %q", r.Recommendation)
	}
	for _, factor := range r.Factors {
		if !ValidPlanRule(string(factor.Rule)) {
			return fmt.Errorf("unknown plan rule %q", factor.Rule)
		}
	}
	return nil
}
