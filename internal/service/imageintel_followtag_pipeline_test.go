package service_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.5 §15-§17: the corrected assessment, all the way through.
//
// # What this proves that imageintel_truthtable_test.go cannot
//
// The truth table establishes that `assess` reaches the right verdict. It says
// nothing about whether that verdict survives the four layers between it and an
// update: persistence, the planner's own re-derivation, the automation
// decision, and the strategy ceiling.
//
// Each of those has its own view of an update, and each has refused things
// before. So this file runs the REAL image-intelligence service, the REAL
// planner, and the REAL automation engine over one world:
//
//	a versioned tag whose digest moved, on a repository too large to enumerate
//
// and asserts what an operator would see at each layer.
//
// # No Docker anywhere
//
// The registry is a fake because a public registry cannot be made to move a tag
// on demand, and §13 forbids standing up a local one. Everything downstream of
// the registry is real. The automation pipeline is the counting fake, so "zero
// acquisitions" is an assertion about a call list rather than a hope.

const (
	followContainerID = "container-follow-0001"
	followName        = "follow-web"
	followTag         = "1.27.4"
	followImage       = "docker.io/library/nginx:" + followTag
)

// followEstate is one container on a versioned tag, with no registry evidence
// yet -- the intelligence service is what will produce it.
func followEstate(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	detail := domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			HostID:        domain.LocalHostID,
			ID:            followContainerID,
			ShortID:       domain.ShortenID(followContainerID),
			Name:          followName,
			Image:         domain.ParseImageRef(followImage),
			ImageID:       "sha256:image-follow",
			State:         domain.StateRunning,
			Health:        domain.HealthHealthy,
			CreatedAt:     now.Add(-time.Hour),
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
			Present:       true,
		},
		State:       domain.StateDetail{State: domain.StateRunning, RawState: "running"},
		Labels:      []domain.Label{},
		Environment: []domain.EnvVar{},
		Mounts:      []domain.Mount{},
		Networks:    []domain.NetworkAttachment{},
		Warnings:    []domain.InventoryWarning{},
	}

	if _, err := db.Inventory.CommitRefresh(context.Background(), store.RefreshCommit{
		Host: domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: []store.ContainerRecord{{
			Detail:  detail,
			RawJSON: []byte(`{"Id":"` + followContainerID + `"}`),
		}},
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        now,
			ContainersListed: 1,
			Checksum:         "follow-checksum",
		},
		Now: now,
	}); err != nil {
		t.Fatalf("commit inventory: %v", err)
	}

	// The image row, so the reference is tracked with the digest the container
	// is actually running.
	if _, err := db.ImageIntel.SyncReferences(context.Background(),
		[]store.ImageReferenceSeed{{
			Reference:      followImage,
			Familiar:       "nginx:" + followTag,
			Kind:           domain.RegistryDockerHub,
			Registry:       "docker.io",
			Namespace:      "library",
			Repository:     "library/nginx",
			Tag:            followTag,
			LocalDigest:    digestA,
			ContainerCount: 1,
			Supported:      true,
		}}, now); err != nil {
		t.Fatalf("seed image intel: %v", err)
	}

	return db
}

// runTruncatedCheck drives the REAL intelligence service over a registry that
// has moved the configured tag's digest and cannot finish enumerating.
func runTruncatedCheck(t *testing.T, db *store.DB) {
	t.Helper()

	remote := &fakeRegistry{
		digest: digestB,
		// Older tags only, so nothing newer is observed within the budget.
		tags:      []string{"1.20.0", "1.21.0", "1.22.0"},
		truncated: true,
	}

	intel := service.NewImageIntelService(service.ImageIntelOptions{
		Store:    db.ImageIntel,
		Registry: remote,
		Config:   intelConfig(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if _, err := intel.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Positive control on the fixture: the registry really was reached and the
	// listing really was truncated.
	manifests, tags := remote.calls()
	if manifests == 0 || tags == 0 {
		t.Fatalf("fixture defect: manifests=%d tagLists=%d; the registry was not "+
			"exercised", manifests, tags)
	}
}

// followPlanner is the planner wired exactly as the composition root wires it,
// INCLUDING the Stage 17.2 snapshot preparer.
//
// # Why the preparer has to be here
//
// Without it the container has no configuration snapshot, and the risk model
// charges 25 points for that -- enough on its own to push a digest update to
// `manualReview`, which no policy can automate. A test that omitted the
// preparer would therefore "prove" that Follow-current-tag does not work, for a
// reason that has nothing to do with registry intelligence.
//
// That is not a workaround. It is the production arrangement: baselines are
// captured before planning precisely so a plan carries the evidence, and Stage
// 17.2 exists to make that true. These two stages have to hold together for the
// preset to work at all, and wiring both here is what proves they do.
func followPlanner(t *testing.T, db *store.DB) *service.PlannerService {
	t.Helper()

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}

	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: db.Containers,
		Snapshots:  db.Snapshots,
		Inventory:  db.Inventory,
		Hasher:     service.NewHasher(key),
		Versions:   service.HostVersions{HarborMaster: "test"},
		Config:     config.Snapshots{Enabled: true},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	assurance := service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
		Capturer: snapshots,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	preparer := service.NewSnapshotPreparer(service.SnapshotPreparerOptions{
		Assurance: assurance,
		Policies:  &followPolicies{policies: []domain.UpdatePolicy{followPolicy(domain.ModeAutomatic)}},
		Targets:   db.Containers,
		Baselines: db.Snapshots,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	return service.NewPlannerService(service.PlannerOptions{
		Store:   db.Plans,
		Lineage: db.Lineage,
		Prepare: preparer,
		Config: config.Planner{
			Enabled: true, BatchSize: 100, MaxContainers: 1000,
			GenerationTimeout: time.Minute,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// followPolicies is the policy source the preparer reads.
type followPolicies struct{ policies []domain.UpdatePolicy }

func (p *followPolicies) ActivePolicies(context.Context) ([]domain.UpdatePolicy, error) {
	return p.policies, nil
}

func (p *followPolicies) CountUpdatePolicies(context.Context) (int, int, error) {
	return len(p.policies), len(p.policies), nil
}

// plan runs a full pass and returns the container's current plan.
func followPlan(t *testing.T, db *store.DB) domain.ChangePlan {
	t.Helper()

	if _, err := followPlanner(t, db).Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	plan, err := db.Plans.Current(context.Background(), followContainerID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	// Positive control: Stage 17.2's preparer must have produced the baseline,
	// or the recommendation below is about a missing snapshot rather than about
	// the update.
	if !plan.SnapshotAvailable {
		t.Fatalf("fixture defect: the plan carries no snapshot, so its risk is " +
			"dominated by that rather than by the update under test")
	}
	return plan
}

// TestTheCorrectedAssessmentIsPersisted is the first layer.
func TestTheCorrectedAssessmentIsPersisted(t *testing.T) {
	db := followEstate(t)
	runTruncatedCheck(t, db)

	record, err := db.ImageIntel.Get(context.Background(), followImage)
	if err != nil {
		t.Fatalf("read intel: %v", err)
	}

	if record.Status != domain.CheckOK {
		t.Fatalf("status = %q, want ok", record.Status)
	}
	if record.Update != domain.UpdateDigest {
		t.Fatalf("persisted update = %q, want %q", record.Update, domain.UpdateDigest)
	}
	if record.RemoteDigest != digestB {
		t.Errorf("remoteDigest = %q, want %q", record.RemoteDigest, digestB)
	}
	if record.LatestTag != "" {
		t.Errorf("latestTag = %q, want empty; no newer tag was observed", record.LatestTag)
	}
	// The operator-facing sentence carries BOTH facts.
	if !containsSubstringLocal(record.UpdateReason, "republished") {
		t.Errorf("reason %q does not say the tag was republished", record.UpdateReason)
	}
	if !containsSubstringLocal(record.UpdateReason, "search limit") {
		t.Errorf("reason %q does not say version discovery was incomplete",
			record.UpdateReason)
	}
}

// TestThePlannerEmitsADigestPlanOnTheSameTag is §15.
//
// The shape the Follow-current-tag preset will rely on: same configured
// reference in and out, a new digest, classified as a digest update.
func TestThePlannerEmitsADigestPlanOnTheSameTag(t *testing.T) {
	db := followEstate(t)
	runTruncatedCheck(t, db)

	plan := followPlan(t, db)

	if plan.UpdateType != domain.UpdateDigest {
		t.Errorf("plan updateType = %q, want %q", plan.UpdateType, domain.UpdateDigest)
	}
	// The FAMILIAR form, which is what the planner records and what an operator
	// typed. The canonical form is an internal key.
	if plan.CurrentImage != "nginx:"+followTag {
		t.Errorf("currentImage = %q, want %q", plan.CurrentImage, "nginx:"+followTag)
	}
	// THE Follow-current-tag property: the reference does not change.
	if plan.ProposedImage != plan.CurrentImage {
		t.Errorf("proposedImage = %q, want it identical to currentImage %q; "+
			"following a tag means the tag stays the same",
			plan.ProposedImage, plan.CurrentImage)
	}
	if plan.ProposedDigest != digestB {
		t.Errorf("proposedDigest = %q, want the newly resolved %q",
			plan.ProposedDigest, digestB)
	}
	if !plan.ValidTarget() {
		t.Error("the plan's proposed reference and digest were not resolved together")
	}
}

// ------------------------------------------------------------ automation --

// followPolicy is a Follow-current-tag policy: digestOnly, in the given mode.
func followPolicy(mode domain.AutomationMode) domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "follow current tag",
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Strategy:              domain.StrategyDigestOnly,
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  mode,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	return policy
}

// followDecision runs the REAL decision function over the real plan.
func followDecision(t *testing.T, db *store.DB, mode domain.AutomationMode) service.AutomationOutcome {
	t.Helper()

	plan, err := db.Plans.Current(context.Background(), followContainerID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}

	return service.DecideAutomation(service.AutomationInput{
		Target: domain.SelectionTarget{
			Name:  followName,
			Image: followImage,
			Eligibility: domain.TargetEligibility{
				Recreatable: true,
			},
		},
		ContainerID: followContainerID,
		Policies:    []domain.UpdatePolicy{followPolicy(mode)},
		Plan:        plan,
		HasPlan:     true,
		Now:         time.Now().UTC(),
	})
}

// TestObserveModeReportsWouldUpdateAndChangesNothing is §16.
func TestObserveModeReportsWouldUpdateAndChangesNothing(t *testing.T) {
	db := followEstate(t)
	runTruncatedCheck(t, db)

	followPlan(t, db)

	outcome := followDecision(t, db, domain.ModeObserve)

	if outcome.Decision.Verdict != domain.VerdictWouldUpdate {
		t.Errorf("verdict = %q, want %q\n\treason: %s / %s",
			outcome.Decision.Verdict, domain.VerdictWouldUpdate,
			outcome.Decision.Reason, outcome.Decision.Detail)
	}
	if outcome.Decision.Reason != domain.ReasonObserveMode {
		t.Errorf("reason = %q, want %q", outcome.Decision.Reason, domain.ReasonObserveMode)
	}
	if outcome.Eligible() {
		t.Error("an observe-mode decision reported itself eligible to act")
	}
	// Nothing was created, because a decision creates nothing.
	if outcome.Decision.AcquisitionID != "" || outcome.Decision.ExecutionID != "" {
		t.Errorf("observe produced acquisition=%q execution=%q, want neither",
			outcome.Decision.AcquisitionID, outcome.Decision.ExecutionID)
	}
	// The plan behind it is still the digest update.
	if outcome.Decision.UpdateType != domain.UpdateDigest {
		t.Errorf("decision updateType = %q, want %q",
			outcome.Decision.UpdateType, domain.UpdateDigest)
	}
}

// TestADigestOnlyAutomaticPolicyMayAct is §17's decision half.
//
// The same world, in automatic mode. This is what the Follow-current-tag preset
// will compile to, and before Stage 17.5 it produced `skip / recommendation`
// -- no, worse: `skip / strategyCeiling`, because the assessment was `unknown`.
func TestADigestOnlyAutomaticPolicyMayAct(t *testing.T) {
	db := followEstate(t)
	runTruncatedCheck(t, db)

	followPlan(t, db)

	outcome := followDecision(t, db, domain.ModeAutomatic)

	if outcome.Decision.Verdict != domain.VerdictUpdate {
		t.Fatalf("verdict = %q, want %q\n\treason: %s / %s\n"+
			"\tthis is the Follow-current-tag case; if it is refused the preset "+
			"cannot work",
			outcome.Decision.Verdict, domain.VerdictUpdate,
			outcome.Decision.Reason, outcome.Decision.Detail)
	}
	if outcome.Decision.Reason != domain.ReasonEligible {
		t.Errorf("reason = %q, want %q", outcome.Decision.Reason, domain.ReasonEligible)
	}

	// Eligibility is NOT action. The decision names a plan and nothing else --
	// no image, no digest the caller chose, no container id from anywhere but
	// HarborMaster's own records.
	if outcome.Decision.PlanID == "" {
		t.Error("an eligible decision names no plan")
	}
	if outcome.Decision.AcquisitionID != "" {
		t.Error("the decision function created an acquisition; it must only decide")
	}
}

// TestTheOldAssessmentWouldHaveBeenRefused is the before/after proof for §10.
//
// The SAME policy, the SAME container, the SAME selector -- only the
// intelligence verdict differs. This is what makes the behaviour change on
// upgrade attributable to the assessment rather than to any widening of scope.
func TestTheOldAssessmentWouldHaveBeenRefused(t *testing.T) {
	db := followEstate(t)
	runTruncatedCheck(t, db)

	plan := followPlan(t, db)

	policy := followPolicy(domain.ModeAutomatic)
	target := domain.SelectionTarget{
		Name:        followName,
		Image:       followImage,
		Eligibility: domain.TargetEligibility{Recreatable: true},
	}
	now := time.Now().UTC()

	// After the fix.
	after := service.DecideAutomation(service.AutomationInput{
		Target: target, ContainerID: followContainerID,
		Policies: []domain.UpdatePolicy{policy},
		Plan:     plan, HasPlan: true, Now: now,
	})
	if after.Decision.Verdict != domain.VerdictUpdate {
		t.Fatalf("after: verdict = %q, want update", after.Decision.Verdict)
	}

	// Before the fix, the identical world produced an `unknown` update. The
	// plan is reused with only that one field changed, so the comparison
	// isolates the assessment.
	stale := plan
	stale.UpdateType = domain.UpdateUnknown
	before := service.DecideAutomation(service.AutomationInput{
		Target: target, ContainerID: followContainerID,
		Policies: []domain.UpdatePolicy{policy},
		Plan:     stale, HasPlan: true, Now: now,
	})
	if before.Decision.Verdict == domain.VerdictUpdate {
		t.Fatal("fixture defect: the old `unknown` verdict was automatable, so " +
			"the defect would not have blocked anything")
	}
	if before.Decision.Reason != domain.ReasonStrategy {
		t.Errorf("before: reason = %q, want %q -- the strategy ceiling is what "+
			"refused an unknown update", before.Decision.Reason, domain.ReasonStrategy)
	}

	// And the POLICY is byte-identical across both. Nothing about scope,
	// selector, strategy, mode, or window moved.
	if after.Effective.Policy.Scope != policy.Scope ||
		after.Effective.Policy.Strategy != policy.Strategy ||
		after.Effective.Policy.Mode != policy.Mode {
		t.Error("the effective policy differs from the one configured; the fix " +
			"must change assessment only")
	}
	if before.Effective.Policy.Strategy != after.Effective.Policy.Strategy {
		t.Error("the strategy differs between the two runs; they must differ " +
			"only in the plan's update type")
	}
}

func containsSubstringLocal(haystack, needle string) bool {
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
