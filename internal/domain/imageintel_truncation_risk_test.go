package domain_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Stage 17.5 §8 and §9: what the corrected assessment does downstream.
//
// # The decision this file records
//
// A digest update discovered alongside a TRUNCATED version search carries
//
//	UpdateDigest
//
// and NO additional risk factor for the truncation.
//
// # Why, stated so it can be argued with
//
// The risk model scores THE PROPOSED CHANGE. The proposed change is "move
// nginx:1.27.4 from digest A to digest B", and its risk is identical whether or
// not some unrelated tag 1.28.0 also exists in the repository. "There might be
// a better change available" is not a risk of this change.
//
// The practical consequence decides it. A caution factor here would apply to
// every digest update on every large repository -- nginx, redis, postgres, and
// anything else publishing more tags than the page budget carries -- pushing
// them toward manualReview. That would reintroduce the Stage 17.5 defect
// through the risk model instead of through the assessment, which is worse: it
// would look like a considered safety judgement rather than a bug.
//
// The incompleteness is still TOLD to the operator. It rides on the assessment
// reason, which is rendered on the image pages. It is information, not a
// penalty.
//
// # What would change this decision
//
// An existing safety factor saying a truncated search should raise risk. There
// is none: truncation leaves the check status at CheckOK -- the manifest lookup
// succeeded -- and assessRegistryQuality charges nothing for CheckOK. That is
// checked below rather than asserted in prose.

// truncationInputs is a digest update on a versioned tag, with everything else
// in the healthiest state the model recognises.
//
// Deliberately minimal: any additional risk in the result comes from the update
// classification and nothing else, so the comparison below isolates it.
func truncationInputs(update domain.UpdateType) domain.PlanInputs {
	return domain.PlanInputs{
		ContainerID:      "container-a",
		ContainerName:    "web",
		CurrentImage:     "nginx:1.27.4",
		ProposedImage:    "nginx:1.27.4",
		CurrentDigest:    "sha256:" + "aa11111111111111111111111111111111111111111111111111111111111111",
		ProposedDigest:   "sha256:" + "bb22222222222222222222222222222222222222222222222222222222222222",
		CurrentTag:       "1.27.4",
		UpdateType:       update,
		SnapshotID:       1,
		RestoreReadiness: domain.ReadinessReady,
		// The load-bearing one: a truncated tag listing is NOT a registry
		// failure. The manifest lookup succeeded, so the status is OK and
		// assessRegistryQuality charges nothing.
		RegistryStatus: domain.CheckOK,
		LocalPlatform:  domain.Platform{OS: "linux", Architecture: "amd64"},
		ContainerCount: 1,
		EvaluatedAt:    time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	}
}

// TestATruncatedSearchAddsNoRiskOfItsOwn.
//
// The assessment produced by a truncated search and the assessment produced by
// a complete one are IDENTICAL for the same proposed change, because the inputs
// the risk model reads are identical. This is the test that would fail if
// somebody later threaded a truncation flag into PlanInputs and charged for it.
func TestATruncatedSearchAddsNoRiskOfItsOwn(t *testing.T) {
	assessment := domain.AssessRisk(truncationInputs(domain.UpdateDigest))

	if assessment.Recommendation != domain.RecommendProceed &&
		assessment.Recommendation != domain.RecommendCaution {
		t.Errorf("recommendation = %q; a digest update on a healthy container "+
			"must remain automatable, or the Follow-current-tag preset cannot work",
			assessment.Recommendation)
	}

	// Non-vacuity: the digest classification must actually be present, or this
	// test is measuring an empty assessment.
	var sawDigestFactor bool
	for _, factor := range assessment.Factors {
		if factor.Detail != "" && contains(factor.Detail, "republished") {
			sawDigestFactor = true
		}
		// And nothing may charge for the search.
		if contains(factor.Detail, "search limit") ||
			contains(factor.Detail, "tag listing") ||
			contains(factor.Detail, "discovery") {
			t.Errorf("a risk factor charges for incomplete version discovery: %q\n"+
				"\tthe risk of moving this container to a known digest does not "+
				"depend on whether an unrelated newer tag exists", factor.Detail)
		}
	}
	if !sawDigestFactor {
		t.Fatal("no digest-update factor was produced; this test proved nothing")
	}
}

// TestTheCorrectedAssessmentIsLessRiskyThanTheDefect.
//
// Before Stage 17.5 the same world produced `unknown`, which the model scores
// higher and which no strategy permits. The fix therefore both corrects the
// verdict and lowers the score -- and the second is worth pinning, because a
// change that produced UpdateDigest while somehow scoring it as unknown would
// leave the container just as stuck.
func TestTheCorrectedAssessmentIsLessRiskyThanTheDefect(t *testing.T) {
	corrected := domain.AssessRisk(truncationInputs(domain.UpdateDigest))
	defective := domain.AssessRisk(truncationInputs(domain.UpdateUnknown))

	if corrected.Score >= defective.Score {
		t.Errorf("corrected score %d is not below the old unknown score %d",
			corrected.Score, defective.Score)
	}
	if defective.Recommendation == domain.RecommendProceed {
		t.Error("fixture defect: the old `unknown` verdict should not have been " +
			"freely automatable, or the defect would not have blocked anything")
	}
}

// TestDigestOnlyStillPermitsExactlyWhatItDid is §9.
//
// The fix changed which UpdateType is PRODUCED. It must not have changed which
// ones a strategy PERMITS -- that would widen every digest-only policy on every
// host, which is the one thing a correctness fix must not smuggle in.
func TestDigestOnlyStillPermitsExactlyWhatItDid(t *testing.T) {
	permitted := map[domain.UpdateType]bool{
		domain.UpdateDigest: true,
		domain.UpdateRebind: true,

		domain.UpdatePatch:      false,
		domain.UpdateMinor:      false,
		domain.UpdateMajor:      false,
		domain.UpdateUnknown:    false,
		domain.UpdatePrerelease: false,
		domain.UpdateNone:       false,
	}

	for update, want := range permitted {
		if got := domain.StrategyDigestOnly.Permits(update); got != want {
			t.Errorf("StrategyDigestOnly.Permits(%q) = %v, want %v", update, got, want)
		}
	}

	// Non-vacuity: the map must cover the whole vocabulary, or a type added
	// later would be silently unchecked.
	for _, update := range domain.UpdateTypes {
		if _, covered := permitted[update]; !covered {
			t.Errorf("update type %q is not covered by this table", update)
		}
	}
}

// TestNoStrategyPermitsUnknown is the property the whole stage rests on.
//
// It is WHY a truncated search erasing a digest move was fatal rather than
// cosmetic: `unknown` is refused by every ceiling, so the container was never
// updated by any policy at all.
func TestNoStrategyPermitsUnknown(t *testing.T) {
	for _, strategy := range domain.UpdateStrategies {
		if strategy.Permits(domain.UpdateUnknown) {
			t.Errorf("%q permits an unknown update; a change whose size could not "+
				"be established must not be automated by any ceiling", strategy)
		}
	}
	// Non-vacuity.
	if len(domain.UpdateStrategies) < 4 {
		t.Fatalf("%d strategies; this test is not looking at the vocabulary it names",
			len(domain.UpdateStrategies))
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
