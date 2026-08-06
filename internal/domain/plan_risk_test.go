package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Risk model tests.
//
// The property under test throughout is DETERMINISM. A risk score that moved
// between runs would make every stored plan unreviewable: an operator could not
// re-derive what HarborMaster said, and the duplicate suppression built on the
// fingerprint would write a new row every pass.
//
// The second property is that MISSING EVIDENCE NEVER READS AS SAFE. A model
// that quietly reports "proceed" when it could not check anything is worse than
// no model, so several tests below exist only to pin that boundary.

// baseInputs is a container in an unremarkable state: a patch update, digests
// on both sides, a ready snapshot, no drift, no violations.
func baseInputs() domain.PlanInputs {
	published := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return domain.PlanInputs{
		ContainerID:    "container-a",
		ContainerName:  "web",
		CurrentImage:   "nginx:1.27.0",
		ProposedImage:  "nginx:1.27.1",
		CurrentDigest:  "sha256:" + strings.Repeat("a", 64),
		ProposedDigest: "sha256:" + strings.Repeat("b", 64),
		CurrentTag:     "1.27.0",
		UpdateType:     domain.UpdatePatch,

		SnapshotID:       7,
		RestoreReadiness: domain.ReadinessReady,

		RegistryStatus: domain.CheckOK,
		LocalPlatform:  domain.Platform{OS: "linux", Architecture: "amd64"},

		ProposedPublishedAt: &published,
		ContainerCount:      1,

		EvaluatedAt: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	}
}

// factorFor returns the factor a rule produced, or nil when it did not fire.
func factorFor(assessment domain.RiskAssessment, rule domain.PlanRule) *domain.RiskFactor {
	for index := range assessment.Factors {
		if assessment.Factors[index].Rule == rule {
			return &assessment.Factors[index]
		}
	}
	return nil
}

// ------------------------------------------------------------ determinism --

// A thousand evaluations of one input must be byte-identical. Any map
// iteration, clock read, or random tie-break inside the model would show up
// here.
func TestAssessmentIsByteIdenticalAcrossRuns(t *testing.T) {
	inputs := baseInputs()

	first, err := json.Marshal(domain.AssessRisk(inputs))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for attempt := 0; attempt < 1000; attempt++ {
		again, err := json.Marshal(domain.AssessRisk(inputs))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("assessment %d differs:\n first: %s\nsecond: %s", attempt, first, again)
		}
	}
}

// Factors appear in rule order, which is fixed at compile time. The ordering is
// part of the explanation: a reviewer reads it as an argument, and a shuffled
// one would not be reproducible.
func TestFactorsAppearInRuleOrder(t *testing.T) {
	inputs := baseInputs()
	inputs.UpdateType = domain.UpdateMajor
	inputs.CurrentTag = "latest"
	inputs.SnapshotID = 0
	inputs.DriftOpen = 2
	inputs.DriftMaxSeverity = domain.DriftSeverityHigh
	inputs.PolicyOpen = 1
	inputs.PolicyMaxSeverity = domain.PolicySeverityMedium
	inputs.ContainerCount = 12

	assessment := domain.AssessRisk(inputs)

	position := make(map[domain.PlanRule]int, len(domain.PlanRules))
	for index, rule := range domain.PlanRules {
		position[rule] = index
	}

	previous := -1
	for _, factor := range assessment.Factors {
		index, known := position[factor.Rule]
		if !known {
			t.Fatalf("factor names unknown rule %q", factor.Rule)
		}
		if index <= previous {
			t.Fatalf("factors out of rule order at %q", factor.Rule)
		}
		previous = index
	}
}

// The clock reaches the model only through EvaluatedAt. If any rule read
// time.Now, two assessments of the same input taken at different wall-clock
// moments would differ -- so pinning EvaluatedAt must pin everything.
func TestOnlyEvaluatedAtMakesTheModelTimeDependent(t *testing.T) {
	inputs := baseInputs()

	published := inputs.EvaluatedAt.Add(-2 * time.Hour)
	inputs.ProposedPublishedAt = &published

	fresh := domain.AssessRisk(inputs)
	if factor := factorFor(fresh, domain.RuleImageAge); factor == nil || factor.Points == 0 {
		t.Fatalf("a two-hour-old image should be scored as fresh: %+v", fresh.Factors)
	}

	// Move only the evaluation moment. The image is now old, and nothing else
	// about the input changed.
	inputs.EvaluatedAt = inputs.EvaluatedAt.Add(30 * 24 * time.Hour)
	aged := domain.AssessRisk(inputs)
	if factor := factorFor(aged, domain.RuleImageAge); factor == nil || factor.Points != 0 {
		t.Fatalf("a month-old image should not be scored as fresh: %+v", aged.Factors)
	}
	if aged.Score >= fresh.Score {
		t.Errorf("aged score %d should be below fresh score %d", aged.Score, fresh.Score)
	}
}

// ------------------------------------------------------------------ bands --

func TestBandBoundariesAreExact(t *testing.T) {
	for _, tc := range []struct {
		score int
		want  domain.RiskBand
	}{
		{0, domain.RiskVeryLow},
		{domain.RiskThresholdLow - 1, domain.RiskVeryLow},
		{domain.RiskThresholdLow, domain.RiskLow},
		{domain.RiskThresholdMedium - 1, domain.RiskLow},
		{domain.RiskThresholdMedium, domain.RiskMedium},
		{domain.RiskThresholdHigh - 1, domain.RiskMedium},
		{domain.RiskThresholdHigh, domain.RiskHigh},
		{domain.RiskThresholdCritical - 1, domain.RiskHigh},
		{domain.RiskThresholdCritical, domain.RiskCritical},
		{domain.MaxRiskScore, domain.RiskCritical},
	} {
		if got := domain.BandForScore(tc.score); got != tc.want {
			t.Errorf("BandForScore(%d) = %q, want %q", tc.score, got, tc.want)
		}
	}
}

// The score is clamped. A container failing in every way must not produce an
// arbitrarily large number that says nothing more than "critical" already did.
func TestScoreIsClampedToTheMaximum(t *testing.T) {
	inputs := baseInputs()
	inputs.UpdateType = domain.UpdateMajor
	inputs.CurrentTag = "latest"
	inputs.CurrentDigest = ""
	inputs.ProposedDigest = ""
	inputs.SnapshotID = 0
	inputs.DriftOpen = 9
	inputs.DriftMaxSeverity = domain.DriftSeverityCritical
	inputs.PolicyOpen = 9
	inputs.PolicyMaxSeverity = domain.PolicySeverityCritical
	inputs.ContainerCount = 40

	assessment := domain.AssessRisk(inputs)

	if assessment.Score != domain.MaxRiskScore {
		t.Errorf("score = %d, want the clamp at %d", assessment.Score, domain.MaxRiskScore)
	}
	if err := assessment.Validate(); err != nil {
		t.Errorf("a clamped assessment must stay internally consistent: %v", err)
	}
}

// A settled container with nothing to change scores nothing and says so.
func TestAQuietContainerScoresNothing(t *testing.T) {
	inputs := baseInputs()
	inputs.UpdateType = domain.UpdateNone

	assessment := domain.AssessRisk(inputs)

	if assessment.Score != 0 {
		t.Errorf("score = %d, want 0 for a container with nothing to change", assessment.Score)
	}
	if assessment.Band != domain.RiskVeryLow {
		t.Errorf("band = %q, want veryLow", assessment.Band)
	}
	if assessment.Recommendation != domain.RecommendProceed {
		t.Errorf("recommendation = %q, want proceed", assessment.Recommendation)
	}
}

// -------------------------------------------------------- recommendations --

// A critical policy violation is the model's only blocker, and it must survive
// an otherwise unremarkable score. The container has a prior problem that a
// change plan is not the place to overrule.
func TestACriticalPolicyViolationBlocksWhateverTheScore(t *testing.T) {
	inputs := baseInputs()
	inputs.PolicyOpen = 1
	inputs.PolicyMaxSeverity = domain.PolicySeverityCritical

	assessment := domain.AssessRisk(inputs)

	if assessment.Band == domain.RiskCritical {
		t.Fatalf("this test is only meaningful below the critical band; score = %d", assessment.Score)
	}
	if assessment.Recommendation != domain.RecommendAgainst {
		t.Errorf("recommendation = %q, want notRecommended", assessment.Recommendation)
	}

	factor := factorFor(assessment, domain.RulePolicyViolations)
	if factor == nil || factor.Severity != domain.FactorBlocker {
		t.Fatalf("the policy factor should be a blocker: %+v", factor)
	}
	if !strings.Contains(assessment.Summary, factor.Detail) {
		t.Errorf("the summary should name the blocking reason, got %q", assessment.Summary)
	}
}

// Missing evidence must never be laundered into "proceed". This is the single
// most misleading thing the feature could do, so it is pinned from both sides:
// the recommendation is unknown, and it is not proceed.
func TestMissingEvidenceProducesUnknownRatherThanProceed(t *testing.T) {
	for name, mutate := range map[string]func(*domain.PlanInputs){
		"no digests at all": func(i *domain.PlanInputs) {
			i.CurrentDigest, i.ProposedDigest = "", ""
		},
		"never checked": func(i *domain.PlanInputs) {
			i.RegistryStatus = domain.CheckPending
		},
		"lookup failed": func(i *domain.PlanInputs) {
			i.RegistryStatus = domain.CheckFailed
		},
		"private repository": func(i *domain.PlanInputs) {
			i.RegistryStatus = domain.CheckUnauthorized
		},
		"unclassifiable update": func(i *domain.PlanInputs) {
			i.UpdateType = domain.UpdateUnknown
		},
	} {
		t.Run(name, func(t *testing.T) {
			inputs := baseInputs()
			mutate(&inputs)

			assessment := domain.AssessRisk(inputs)

			if assessment.Recommendation == domain.RecommendProceed {
				t.Fatalf("a gap in evidence must not read as proceed: %+v", assessment)
			}
			if assessment.Recommendation != domain.RecommendUnknown {
				t.Errorf("recommendation = %q, want unknown", assessment.Recommendation)
			}
			if !strings.HasPrefix(assessment.Summary, "Cannot advise") {
				t.Errorf("summary = %q, want it to admit the gap", assessment.Summary)
			}
		})
	}
}

// A blocker outranks missing evidence. Both override the band, and the order
// matters: "we cannot advise" would hide a violation an operator must settle.
func TestABlockerOutranksMissingEvidence(t *testing.T) {
	inputs := baseInputs()
	inputs.RegistryStatus = domain.CheckFailed
	inputs.PolicyOpen = 1
	inputs.PolicyMaxSeverity = domain.PolicySeverityCritical

	if got := domain.AssessRisk(inputs).Recommendation; got != domain.RecommendAgainst {
		t.Errorf("recommendation = %q, want notRecommended", got)
	}
}

func TestCriticalRiskIsNotRecommended(t *testing.T) {
	inputs := baseInputs()
	inputs.UpdateType = domain.UpdateMajor
	inputs.SnapshotID = 0
	inputs.DriftOpen = 3
	inputs.DriftMaxSeverity = domain.DriftSeverityCritical

	assessment := domain.AssessRisk(inputs)

	if assessment.Band != domain.RiskCritical {
		t.Fatalf("band = %q (score %d), want critical", assessment.Band, assessment.Score)
	}
	if assessment.Recommendation != domain.RecommendAgainst {
		t.Errorf("recommendation = %q, want notRecommended", assessment.Recommendation)
	}
}

// A high band asks for a person even when nothing individually blocks.
func TestHighRiskAsksForAPerson(t *testing.T) {
	inputs := baseInputs()
	inputs.UpdateType = domain.UpdateMajor
	inputs.DriftOpen = 1
	inputs.DriftMaxSeverity = domain.DriftSeverityHigh

	assessment := domain.AssessRisk(inputs)

	if assessment.Band != domain.RiskHigh {
		t.Fatalf("band = %q (score %d), want high", assessment.Band, assessment.Score)
	}
	if assessment.Recommendation != domain.RecommendManualReview {
		t.Errorf("recommendation = %q, want manualReview", assessment.Recommendation)
	}
}

// Every assessment the model produces is internally consistent. Validate is the
// repository's backstop, and a rule set that could fail it would be caught here
// rather than at insert time.
func TestEveryAssessmentValidates(t *testing.T) {
	updates := []domain.UpdateType{
		domain.UpdateNone, domain.UpdateDigest, domain.UpdatePatch,
		domain.UpdateMinor, domain.UpdateMajor, domain.UpdatePrerelease,
		domain.UpdateUnknown,
	}
	statuses := domain.CheckStatuses
	drifts := []domain.DriftSeverity{
		"", domain.DriftSeverityLow, domain.DriftSeverityMedium,
		domain.DriftSeverityHigh, domain.DriftSeverityCritical,
	}
	policies := []domain.PolicySeverity{
		"", domain.PolicySeverityLow, domain.PolicySeverityMedium,
		domain.PolicySeverityHigh, domain.PolicySeverityCritical,
	}

	for _, update := range updates {
		for _, status := range statuses {
			for _, drift := range drifts {
				for _, policy := range policies {
					inputs := baseInputs()
					inputs.UpdateType = update
					inputs.RegistryStatus = status
					if drift != "" {
						inputs.DriftOpen, inputs.DriftMaxSeverity = 2, drift
					}
					if policy != "" {
						inputs.PolicyOpen, inputs.PolicyMaxSeverity = 2, policy
					}

					assessment := domain.AssessRisk(inputs)
					if err := assessment.Validate(); err != nil {
						t.Fatalf("update=%s status=%s drift=%s policy=%s: %v",
							update, status, drift, policy, err)
					}
				}
			}
		}
	}
}

// ------------------------------------------------------------------ rules --

// A major version is the only classification carrying an explicit promise of
// breaking changes, so it must outweigh every other classification.
func TestUpdateClassificationIsOrderedByHowMuchItCanBreak(t *testing.T) {
	scoreFor := func(update domain.UpdateType) int {
		inputs := baseInputs()
		inputs.UpdateType = update
		return domain.AssessRisk(inputs).Score
	}

	major, prerelease := scoreFor(domain.UpdateMajor), scoreFor(domain.UpdatePrerelease)
	minor, patch := scoreFor(domain.UpdateMinor), scoreFor(domain.UpdatePatch)
	none := scoreFor(domain.UpdateNone)

	if major <= prerelease || prerelease <= minor || minor <= patch || patch <= none {
		t.Errorf("classification scores are not ordered: major=%d prerelease=%d minor=%d patch=%d none=%d",
			major, prerelease, minor, patch, none)
	}
}

func TestAMutableTagIsReportedAsSuch(t *testing.T) {
	inputs := baseInputs()
	inputs.CurrentTag = "latest"

	factor := factorFor(domain.AssessRisk(inputs), domain.RuleMutableTag)
	if factor == nil {
		t.Fatal("a container tracking \"latest\" should produce a mutable-tag factor")
	}
	if !strings.Contains(factor.Detail, "latest") {
		t.Errorf("detail should name the tag, got %q", factor.Detail)
	}

	inputs.CurrentTag = "1.27.0"
	if factor := factorFor(domain.AssessRisk(inputs), domain.RuleMutableTag); factor != nil {
		t.Errorf("a pinned version tag is not mutable: %+v", factor)
	}
}

// A missing snapshot is a property of the CHANGE, not of the image: there is no
// recorded configuration to refer back to.
func TestAMissingSnapshotIsAWarning(t *testing.T) {
	inputs := baseInputs()
	inputs.SnapshotID = 0

	assessment := domain.AssessRisk(inputs)

	factor := factorFor(assessment, domain.RuleSnapshotAvailable)
	if factor == nil || factor.Severity != domain.FactorWarning || factor.Points == 0 {
		t.Fatalf("a container with no snapshot should be warned about: %+v", factor)
	}
	// Readiness is not scored on top: without a snapshot there is nothing whose
	// readiness could be assessed, and scoring both would double-count.
	if factor := factorFor(assessment, domain.RuleRestoreReadiness); factor != nil {
		t.Errorf("readiness should not be assessed without a snapshot: %+v", factor)
	}
}

func TestRestoreReadinessIsScoredOnlyWithASnapshot(t *testing.T) {
	inputs := baseInputs()
	inputs.RestoreReadiness = domain.ReadinessNotReady

	factor := factorFor(domain.AssessRisk(inputs), domain.RuleRestoreReadiness)
	if factor == nil || factor.Severity != domain.FactorWarning {
		t.Fatalf("a failed readiness check should be a warning: %+v", factor)
	}
}

// The platform rule is the only one that can say the change would not run at
// all, and it must rest on a SUCCESSFUL lookup: silence from a registry is not
// an answer of "no".
func TestPlatformIsOnlyJudgedWhenTheRegistryAnswered(t *testing.T) {
	inputs := baseInputs()
	inputs.ProposedPlatformMissing = true

	factor := factorFor(domain.AssessRisk(inputs), domain.RulePlatform)
	if factor == nil || factor.Severity != domain.FactorWarning {
		t.Fatalf("an unpublished platform should be a warning: %+v", factor)
	}
	if !strings.Contains(factor.Detail, "linux/amd64") {
		t.Errorf("detail should name the platform, got %q", factor.Detail)
	}

	inputs.RegistryStatus = domain.CheckFailed
	if factor := factorFor(domain.AssessRisk(inputs), domain.RulePlatform); factor != nil {
		t.Errorf("a failed lookup establishes nothing about platform support: %+v", factor)
	}
}

func TestBlastRadiusFiresOnlyForAWidelyUsedImage(t *testing.T) {
	inputs := baseInputs()
	inputs.ContainerCount = 5

	if factor := factorFor(domain.AssessRisk(inputs), domain.RuleBlastRadius); factor != nil {
		t.Errorf("five containers is not estate-wide: %+v", factor)
	}

	inputs.ContainerCount = 20
	factor := factorFor(domain.AssessRisk(inputs), domain.RuleBlastRadius)
	if factor == nil {
		t.Fatal("twenty containers on one image should be reported")
	}
	if !strings.Contains(factor.Detail, "20") {
		t.Errorf("detail should name the count, got %q", factor.Detail)
	}
}

// An absent publication date is stated but never scored. Penalising it would
// make the model measure a publisher's metadata hygiene rather than risk.
func TestAbsentProvenanceIsStatedButNotScored(t *testing.T) {
	inputs := baseInputs()
	inputs.ProposedPublishedAt = nil

	factor := factorFor(domain.AssessRisk(inputs), domain.RuleImageAge)
	if factor == nil {
		t.Fatal("absent provenance should still be stated")
	}
	if factor.Points != 0 || factor.Severity != domain.FactorInfo {
		t.Errorf("absent provenance must not be scored: %+v", factor)
	}
}

// No factor detail may carry text HarborMaster did not write. Registry-supplied
// strings reach a UI, and a third party's text must not be one of them.
func TestFactorDetailsNeverCarryRegistryText(t *testing.T) {
	inputs := baseInputs()
	inputs.RegistryStatus = domain.CheckFailed
	inputs.RegistryDetail = "<script>alert(1)</script> upstream said so"

	for _, factor := range domain.AssessRisk(inputs).Factors {
		if strings.Contains(factor.Detail, "script") || strings.Contains(factor.Detail, "upstream said so") {
			t.Errorf("factor %q leaked registry text: %q", factor.Rule, factor.Detail)
		}
	}
}

// ------------------------------------------------------------ fingerprint --

func TestFingerprintIsStableAcrossRuns(t *testing.T) {
	inputs := baseInputs()
	first := inputs.Fingerprint()

	for attempt := 0; attempt < 100; attempt++ {
		if again := baseInputs().Fingerprint(); again != first {
			t.Fatalf("fingerprint %d = %q, want %q", attempt, again, first)
		}
	}
}

// Every field the rules read must move the fingerprint. A field that did not
// would let a changed world be reported with a stale plan, which is the exact
// failure duplicate suppression is supposed to be safe against.
func TestEveryScoredInputMovesTheFingerprint(t *testing.T) {
	published := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	for name, mutate := range map[string]func(*domain.PlanInputs){
		"container":         func(i *domain.PlanInputs) { i.ContainerID = "container-b" },
		"current image":     func(i *domain.PlanInputs) { i.CurrentImage = "nginx:1.26.0" },
		"proposed image":    func(i *domain.PlanInputs) { i.ProposedImage = "nginx:1.28.0" },
		"current digest":    func(i *domain.PlanInputs) { i.CurrentDigest = "" },
		"proposed digest":   func(i *domain.PlanInputs) { i.ProposedDigest = "" },
		"current tag":       func(i *domain.PlanInputs) { i.CurrentTag = "latest" },
		"update type":       func(i *domain.PlanInputs) { i.UpdateType = domain.UpdateMajor },
		"snapshot":          func(i *domain.PlanInputs) { i.SnapshotID = 8 },
		"readiness":         func(i *domain.PlanInputs) { i.RestoreReadiness = domain.ReadinessNotReady },
		"drift count":       func(i *domain.PlanInputs) { i.DriftOpen = 1 },
		"drift severity":    func(i *domain.PlanInputs) { i.DriftMaxSeverity = domain.DriftSeverityHigh },
		"policy count":      func(i *domain.PlanInputs) { i.PolicyOpen = 1 },
		"policy severity":   func(i *domain.PlanInputs) { i.PolicyMaxSeverity = domain.PolicySeverityHigh },
		"registry status":   func(i *domain.PlanInputs) { i.RegistryStatus = domain.CheckFailed },
		"local platform":    func(i *domain.PlanInputs) { i.LocalPlatform = domain.Platform{OS: "linux", Architecture: "arm64"} },
		"platform missing":  func(i *domain.PlanInputs) { i.ProposedPlatformMissing = true },
		"published at":      func(i *domain.PlanInputs) { i.ProposedPublishedAt = &published },
		"published cleared": func(i *domain.PlanInputs) { i.ProposedPublishedAt = nil },
		"container count":   func(i *domain.PlanInputs) { i.ContainerCount = 9 },
	} {
		t.Run(name, func(t *testing.T) {
			inputs := baseInputs()
			before := inputs.Fingerprint()

			mutate(&inputs)
			if after := inputs.Fingerprint(); after == before {
				t.Errorf("changing %s did not move the fingerprint", name)
			}
		})
	}
}

// The clock is excluded, by design. Including it would make every fingerprint
// unique and defeat duplicate suppression entirely.
func TestTheEvaluationClockIsExcludedFromTheFingerprint(t *testing.T) {
	inputs := baseInputs()
	before := inputs.Fingerprint()

	inputs.EvaluatedAt = inputs.EvaluatedAt.Add(365 * 24 * time.Hour)
	if after := inputs.Fingerprint(); after != before {
		t.Errorf("the evaluation clock moved the fingerprint: %q then %q", before, after)
	}
}

// The same instant in two zones is one fingerprint, not two.
func TestFingerprintNormalisesTimeZones(t *testing.T) {
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	elsewhere := utc.In(time.FixedZone("elsewhere", 5*3600))

	first, second := baseInputs(), baseInputs()
	first.ProposedPublishedAt = &utc
	second.ProposedPublishedAt = &elsewhere

	if first.Fingerprint() != second.Fingerprint() {
		t.Error("the same instant in two zones produced two fingerprints")
	}
}

// goldenFingerprint pins the fingerprint of baseInputs under planner version 1.
//
// A golden value rather than a property, because the thing worth catching is an
// ACCIDENTAL change: adding a field to the preimage, reordering it, or changing
// how a value renders would all silently invalidate every stored plan. A
// deliberate rule-set change is expected to update this constant in the same
// diff that bumps PlannerVersion -- which is the point, since that bump is what
// forces a clean regeneration.
const goldenFingerprint = "3391b2426655ccef1055beca956c24f29476553e020885c9015180abcb847c3d"

func TestFingerprintCompositionIsPinned(t *testing.T) {
	if domain.PlannerVersion != "1" || domain.PlanSchemaVersion != 1 {
		t.Skipf("golden fingerprint is for planner 1 / schema 1, not %s / %d",
			domain.PlannerVersion, domain.PlanSchemaVersion)
	}
	if got := baseInputs().Fingerprint(); got != goldenFingerprint {
		t.Errorf("fingerprint composition changed:\n got %s\nwant %s\n"+
			"If this was deliberate, bump PlannerVersion so stored plans regenerate.",
			got, goldenFingerprint)
	}
}

// --------------------------------------------------------- vocabularies --

func TestPlanVocabulariesRejectUnknownNames(t *testing.T) {
	if domain.ValidRiskBand("catastrophic") || domain.ValidRiskBand("") {
		t.Error("unknown risk bands must be rejected")
	}
	if domain.ValidRecommendation("justDoIt") || domain.ValidRecommendation("") {
		t.Error("unknown recommendations must be rejected")
	}
	if domain.ValidPlanRule("phaseOfTheMoon") || domain.ValidPlanRule("") {
		t.Error("unknown plan rules must be rejected")
	}

	for _, band := range domain.RiskBands {
		if !domain.ValidRiskBand(string(band)) {
			t.Errorf("band %q is not accepted by its own vocabulary", band)
		}
	}
	for _, rule := range domain.PlanRules {
		if !domain.ValidPlanRule(string(rule)) {
			t.Errorf("rule %q is not accepted by its own vocabulary", rule)
		}
	}
}

// Bands rank in the order they are listed, which the API's sort ordering
// depends on.
func TestRiskBandsRankInOrder(t *testing.T) {
	for index := 1; index < len(domain.RiskBands); index++ {
		previous, current := domain.RiskBands[index-1], domain.RiskBands[index]
		if previous.Rank() >= current.Rank() {
			t.Errorf("%q ranks %d, not below %q at %d",
				previous, previous.Rank(), current, current.Rank())
		}
	}
	if domain.RiskBand("nonsense").Rank() != 0 {
		t.Error("an unknown band should rank below every known one")
	}
}

// A plan id is unguessable and shaped. It appears in URLs, so a sequential one
// would leak how many plans exist and invite a caller to walk them.
func TestPlanIDsAreUniqueAndShaped(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for attempt := 0; attempt < 1000; attempt++ {
		id := domain.NewPlanID()
		if !strings.HasPrefix(id, "plan_") || len(id) != len("plan_")+20 {
			t.Fatalf("malformed plan id %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate plan id %q after %d draws", id, attempt)
		}
		seen[id] = struct{}{}
	}
}
