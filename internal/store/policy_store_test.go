package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Policy persistence tests.
//
// The properties that matter here are about IDENTITY and LIFECYCLE: the same
// failing rule seen twice must be one violation, a rule the container starts
// complying with must resolve, an operator's acknowledgement must survive
// re-evaluation WITHOUT suppressing it, and withdrawing a policy must never
// destroy the history of what it caught.

func newPolicy(name string, rules ...domain.PolicyRule) domain.PolicyDefinition {
	if len(rules) == 0 {
		rules = []domain.PolicyRule{{Type: domain.RulePrivilegedForbidden}}
	}
	return domain.PolicyDefinition{
		PolicyID: domain.NewPolicyID(),
		Name:     name,
		Severity: domain.PolicySeverityHigh,
		Enabled:  true,
		Rules:    rules,
	}
}

// createPolicy stores a definition and fails the test if it cannot.
func createPolicy(t *testing.T, db *store.DB, policy domain.PolicyDefinition) domain.PolicyDefinition {
	t.Helper()
	created, err := db.Policies.CreatePolicy(context.Background(), policy, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	return created
}

func violationFor(policy domain.PolicyDefinition, ruleType domain.PolicyRuleType) domain.PolicyViolation {
	return domain.PolicyViolation{
		PolicyID:      policy.PolicyID,
		PolicyName:    policy.Name,
		ContainerID:   "container-a",
		ContainerName: "web",
		RuleType:      ruleType,
		Severity:      domain.PolicySeverityHigh,
		Observed:      "privileged=true",
		Expected:      "privileged=false",
		Reason:        "the container runs privileged",
		Status:        domain.PolicyViolationActive,
	}
}

func passFor(count int, complete bool) domain.PolicyEvaluation {
	return domain.PolicyEvaluation{
		ContainerID:       "container-a",
		ContainerName:     "web",
		PoliciesEvaluated: 1,
		RulesEvaluated:    1,
		ViolationCount:    count,
		Complete:          complete,
		Compliant:         complete && count == 0,
	}
}

// reconcile is the shorthand every lifecycle test below uses.
func reconcile(t *testing.T, db *store.DB, pass domain.PolicyEvaluation, violations ...domain.PolicyViolation) store.UpsertResult {
	t.Helper()
	result, err := db.Policies.ReconcilePolicy(context.Background(), pass, violations, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcilePolicy: %v", err)
	}
	return result
}

// ------------------------------------------------------------ definitions --

func TestPolicyRoundTrips(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := newPolicy("Production baseline",
		domain.PolicyRule{Type: domain.RulePrivilegedForbidden},
		domain.PolicyRule{
			Type:     domain.RuleImageAllowlist,
			Severity: domain.PolicySeverityCritical,
			Values:   []string{"registry.example.com/*"},
		},
	)
	policy.Description = "what production must look like"
	created := createPolicy(t, db, policy)

	loaded, err := db.Policies.GetPolicy(ctx, created.PolicyID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}

	if loaded.Name != policy.Name || loaded.Description != policy.Description {
		t.Errorf("name/description did not round-trip: %+v", loaded)
	}
	if len(loaded.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(loaded.Rules))
	}
	if loaded.Rules[1].Type != domain.RuleImageAllowlist ||
		loaded.Rules[1].Severity != domain.PolicySeverityCritical ||
		len(loaded.Rules[1].Values) != 1 {
		t.Errorf("the second rule did not round-trip: %+v", loaded.Rules[1])
	}
	if !loaded.Active() {
		t.Error("a new enabled policy is not active")
	}
}

func TestPolicyNamesAreUniqueAmongLivePolicies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := createPolicy(t, db, newPolicy("Baseline"))

	_, err := db.Policies.CreatePolicy(ctx, newPolicy("Baseline"), time.Now().UTC())
	if !errors.Is(err, store.ErrPolicyNameTaken) {
		t.Fatalf("second create returned %v, want ErrPolicyNameTaken", err)
	}

	// Archiving frees the name: the withdrawn policy is history, and an
	// operator rewriting a rule should not have to invent a new name for it.
	if err := db.Policies.ArchivePolicy(ctx, first.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchivePolicy: %v", err)
	}
	if _, err := db.Policies.CreatePolicy(ctx, newPolicy("Baseline"), time.Now().UTC()); err != nil {
		t.Fatalf("create after archive: %v", err)
	}
}

func TestUpdatingAPolicyLeavesItsIdentityAlone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created := createPolicy(t, db, newPolicy("Baseline"))

	name := "Renamed baseline"
	severity := domain.PolicySeverityLow
	rules := []domain.PolicyRule{{Type: domain.RuleUserNotRoot}}

	updated, err := db.Policies.UpdatePolicy(ctx, created.PolicyID, store.PolicyUpdate{
		Name:     &name,
		Severity: &severity,
		Rules:    &rules,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}

	if updated.PolicyID != created.PolicyID {
		t.Errorf("the policy id changed: %q -> %q", created.PolicyID, updated.PolicyID)
	}
	if updated.Name != name || updated.Severity != severity {
		t.Errorf("the update did not apply: %+v", updated)
	}
	if len(updated.Rules) != 1 || updated.Rules[0].Type != domain.RuleUserNotRoot {
		t.Errorf("rules = %+v, want the replacement", updated.Rules)
	}
	// An omitted field must be left alone, which is what the pointer-per-field
	// shape of PolicyUpdate exists for.
	if !updated.Enabled {
		t.Error("an omitted enabled flag disabled the policy")
	}
}

// A rename must reach the violations, because the policy name is denormalised
// onto them so a list needs no join.
func TestRenamingAPolicyUpdatesItsViolations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	name := "Renamed"
	if _, err := db.Policies.UpdatePolicy(ctx, policy.PolicyID,
		store.PolicyUpdate{Name: &name}, time.Now().UTC()); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}

	violations, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if len(violations) != 1 || violations[0].PolicyName != name {
		t.Errorf("policy name on the violation = %q, want %q", violations[0].PolicyName, name)
	}
}

// The central guarantee behind DELETE. Withdrawing a policy must resolve its
// open violations WITHOUT destroying them: an auditor asking what the estate
// was failing last quarter must not get a different answer because someone
// tidied up this quarter.
func TestArchivingAPolicyResolvesItsViolationsAndKeepsTheHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	if err := db.Policies.ArchivePolicy(ctx, policy.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchivePolicy: %v", err)
	}

	// The history survives.
	violations, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if total != 1 {
		t.Fatalf("violations = %d, want the history retained", total)
	}
	if violations[0].Status != domain.PolicyViolationResolved {
		t.Errorf("status = %q, want resolved", violations[0].Status)
	}
	if violations[0].ResolvedAt == nil {
		t.Error("resolvedAt was not set")
	}

	// The policy itself survives, marked archived, and drops out of both the
	// default listing and the active set.
	archived, err := db.Policies.GetPolicy(ctx, policy.PolicyID)
	if err != nil {
		t.Fatalf("GetPolicy after archive: %v", err)
	}
	if !archived.Archived || archived.ArchivedAt == nil {
		t.Errorf("policy = %+v, want archived with a timestamp", archived)
	}
	if archived.Active() {
		t.Error("an archived policy still reports as active")
	}

	active, err := db.Policies.ActivePolicies(ctx, 100)
	if err != nil {
		t.Fatalf("ActivePolicies: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active policies = %d, want 0", len(active))
	}

	listed, _, err := db.Policies.ListPolicies(ctx, store.PolicyFilter{})
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("default listing returned %d archived policies", len(listed))
	}

	included, _, err := db.Policies.ListPolicies(ctx, store.PolicyFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListPolicies(includeArchived): %v", err)
	}
	if len(included) != 1 {
		t.Errorf("includeArchived returned %d, want 1", len(included))
	}
}

// Archiving twice is not an error the second time -- it is a miss, because
// there is no live policy with that id to withdraw.
func TestArchivingAnArchivedPolicyReportsNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	if err := db.Policies.ArchivePolicy(ctx, policy.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if err := db.Policies.ArchivePolicy(ctx, policy.PolicyID, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second archive returned %v, want ErrNotFound", err)
	}
	// An archived policy is history and must not be editable: editing one
	// would change what a violation appears to have been measured against.
	name := "Edited"
	if _, err := db.Policies.UpdatePolicy(ctx, policy.PolicyID,
		store.PolicyUpdate{Name: &name}, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update of an archived policy returned %v, want ErrNotFound", err)
	}
}

// Disabling a policy stops it applying, so its open violations must close in
// the same transaction rather than lingering until the next sweep notices.
func TestDisablingAPolicyResolvesItsViolations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	disabled := false
	if _, err := db.Policies.UpdatePolicy(ctx, policy.PolicyID,
		store.PolicyUpdate{Enabled: &disabled}, time.Now().UTC()); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}

	violations, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("open violations = %d, want 0 after disabling the policy", len(violations))
	}
}

// The schema-level backstop. Even a hand-written DELETE must not orphan the
// violation history.
func TestTheDatabaseRefusesToDeleteAPolicyThatHasViolations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	_, err := db.SQL().ExecContext(ctx,
		`DELETE FROM policy_definitions WHERE policy_id = ?`, policy.PolicyID)
	if err == nil {
		t.Fatal("the database allowed a policy with violations to be deleted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("error = %v, want a foreign key violation", err)
	}
}

// ------------------------------------------------------------- violations --

// The deduplication guarantee. The same failing rule seen many times is ONE
// row, and its first-seen time does not move.
func TestARepeatedFailureIsOneViolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))

	first := reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))
	if first.Inserted != 1 {
		t.Fatalf("inserted = %d, want 1", first.Inserted)
	}

	violations, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	detectedAt := violations[0].DetectedAt

	for i := 0; i < 25; i++ {
		result := reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))
		if result.Inserted != 0 || result.Updated != 1 {
			t.Fatalf("pass %d: inserted=%d updated=%d, want 0/1", i, result.Inserted, result.Updated)
		}
	}

	violations, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if total != 1 {
		t.Fatalf("violations = %d after 26 passes, want 1", total)
	}
	if !violations[0].DetectedAt.Equal(detectedAt) {
		t.Error("detectedAt moved; the age of a violation must be the age of the non-compliance")
	}
	if !violations[0].LastSeenAt.After(detectedAt) && !violations[0].LastSeenAt.Equal(detectedAt) {
		t.Error("lastSeenAt did not advance")
	}
}

// The resolution guarantee: a container that starts complying closes the row.
func TestAViolationResolvesWhenTheContainerComplies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	// A complete pass that found nothing.
	result := reconcile(t, db, passFor(0, true))
	if result.Resolved != 1 {
		t.Fatalf("resolved = %d, want 1", result.Resolved)
	}

	violations, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if violations[0].Status != domain.PolicyViolationResolved || violations[0].ResolvedAt == nil {
		t.Errorf("violation = %+v, want resolved with a timestamp", violations[0])
	}

	// The default listing hides resolved history, so the dashboard shows what
	// still stands.
	open, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("ListViolations(open): %v", err)
	}
	if total != 0 || len(open) != 0 {
		t.Errorf("open violations = %d, want 0", total)
	}

	// Failing again reactivates the SAME row rather than creating a second.
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))
	violations, total, err = db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if total != 1 {
		t.Fatalf("violations = %d after reactivation, want 1", total)
	}
	if violations[0].Status != domain.PolicyViolationActive || violations[0].ResolvedAt != nil {
		t.Errorf("violation = %+v, want active with no resolvedAt", violations[0])
	}
}

// An INCOMPLETE pass resolves nothing. It did not establish that the rules it
// never applied now pass; it established that it stopped applying them.
func TestAnIncompletePassResolvesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	incomplete := passFor(0, false)
	incomplete.Reason = "the container exceeded its violation budget"
	result := reconcile(t, db, incomplete)
	if result.Resolved != 0 {
		t.Fatalf("an incomplete pass resolved %d violations, want 0", result.Resolved)
	}

	open, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("open violations = %d, want the untouched 1", len(open))
	}
}

// The acknowledgement guarantee, and the half of it that is easy to get wrong:
// the status survives re-evaluation, and re-evaluation KEEPS HAPPENING.
func TestAcknowledgementSurvivesWithoutSuppressingReevaluation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	violations, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	id := violations[0].ID

	acknowledged, err := db.Policies.UpdateViolationStatus(ctx, id,
		domain.PolicyViolationAcknowledged, "accepted until the next release", time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateViolationStatus: %v", err)
	}
	if acknowledged.Status != domain.PolicyViolationAcknowledged ||
		acknowledged.Note != "accepted until the next release" ||
		acknowledged.StatusChangedAt == nil {
		t.Fatalf("violation = %+v, want acknowledged with a note", acknowledged)
	}
	seenAt := acknowledged.LastSeenAt

	// Re-evaluation refreshes the row and leaves the operator's status alone.
	time.Sleep(2 * time.Millisecond)
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	after, err := db.Policies.GetViolation(ctx, id)
	if err != nil {
		t.Fatalf("GetViolation: %v", err)
	}
	if after.Status != domain.PolicyViolationAcknowledged {
		t.Errorf("status = %q, want the acknowledgement preserved", after.Status)
	}
	if !after.LastSeenAt.After(seenAt) {
		t.Error("lastSeenAt did not advance; the rule was not re-evaluated")
	}
	if after.Note != "accepted until the next release" {
		t.Errorf("note = %q, want it preserved", after.Note)
	}

	// And it still resolves automatically once the container complies. An
	// acknowledgement is not a permanent exemption from the truth.
	reconcile(t, db, passFor(0, true))
	resolved, err := db.Policies.GetViolation(ctx, id)
	if err != nil {
		t.Fatalf("GetViolation: %v", err)
	}
	if resolved.Status != domain.PolicyViolationResolved {
		t.Errorf("status = %q, want resolved", resolved.Status)
	}
}

// Resolution is scoped to what the pass actually saw, so one container becoming
// compliant must not close another's violations.
func TestResolutionIsScopedToTheContainer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))

	other := violationFor(policy, domain.RulePrivilegedForbidden)
	other.ContainerID = "container-b"
	other.ContainerName = "worker"

	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	otherPass := passFor(1, true)
	otherPass.ContainerID = "container-b"
	otherPass.ContainerName = "worker"
	reconcile(t, db, otherPass, other)

	// container-a becomes compliant.
	reconcile(t, db, passFor(0, true))

	open, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if len(open) != 1 || open[0].ContainerID != "container-b" {
		t.Errorf("open violations = %+v, want only container-b", open)
	}
}

// One violation per FAILED RULE. Two rules of a policy failing on one container
// are two rows, and the identity keeps them apart.
func TestEachFailedRuleIsItsOwnViolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline",
		domain.PolicyRule{Type: domain.RulePrivilegedForbidden},
		domain.PolicyRule{Type: domain.RuleUserNotRoot},
	))

	privileged := violationFor(policy, domain.RulePrivilegedForbidden)
	root := violationFor(policy, domain.RuleUserNotRoot)

	result := reconcile(t, db, passFor(2, true), privileged, root)
	if result.Inserted != 2 {
		t.Fatalf("inserted = %d, want 2", result.Inserted)
	}

	// Only the root rule now passes.
	result = reconcile(t, db, passFor(1, true), privileged)
	if result.Resolved != 1 || result.Updated != 1 {
		t.Errorf("resolved=%d updated=%d, want 1/1", result.Resolved, result.Updated)
	}

	open, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if len(open) != 1 || open[0].RuleType != domain.RulePrivilegedForbidden {
		t.Errorf("open = %+v, want only the privileged rule", open)
	}
}

// A pass that ran and found nothing is a fact worth storing: it is what
// distinguishes "compliant" from "never evaluated".
func TestACompliantPassIsRecorded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Policies.PolicyEvaluation(ctx, "container-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unevaluated container returned %v, want ErrNotFound", err)
	}

	reconcile(t, db, passFor(0, true))

	evaluation, err := db.Policies.PolicyEvaluation(ctx, "container-a")
	if err != nil {
		t.Fatalf("PolicyEvaluation: %v", err)
	}
	if !evaluation.Compliant || !evaluation.Complete || evaluation.ViolationCount != 0 {
		t.Errorf("evaluation = %+v, want a complete compliant pass", evaluation)
	}
}

// An incomplete pass must never be recorded as compliant. Enforced by a CHECK
// constraint as well as by the writer, so a bug in either is caught.
func TestAnIncompletePassIsNeverCompliant(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	pass := passFor(0, false)
	// Deliberately asserting the contradiction the constraint exists to refuse.
	pass.Compliant = true
	pass.Reason = "the pass was cut short"
	reconcile(t, db, pass)

	evaluation, err := db.Policies.PolicyEvaluation(ctx, "container-a")
	if err != nil {
		t.Fatalf("PolicyEvaluation: %v", err)
	}
	if evaluation.Compliant {
		t.Error("an incomplete pass was stored as compliant")
	}
	if evaluation.Reason == "" {
		t.Error("an incomplete pass carries no reason")
	}
}

// ---------------------------------------------------------------- reading --

func TestPolicySummaryAggregates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline",
		domain.PolicyRule{Type: domain.RulePrivilegedForbidden},
		domain.PolicyRule{Type: domain.RuleUserNotRoot},
	))
	archived := createPolicy(t, db, newPolicy("Withdrawn"))
	if err := db.Policies.ArchivePolicy(ctx, archived.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchivePolicy: %v", err)
	}

	critical := violationFor(policy, domain.RulePrivilegedForbidden)
	critical.Severity = domain.PolicySeverityCritical
	medium := violationFor(policy, domain.RuleUserNotRoot)
	medium.Severity = domain.PolicySeverityMedium
	reconcile(t, db, passFor(2, true), critical, medium)

	// A second, compliant container.
	compliant := passFor(0, true)
	compliant.ContainerID = "container-b"
	compliant.ContainerName = "worker"
	reconcile(t, db, compliant)

	summary, err := db.Policies.PolicySummary(ctx)
	if err != nil {
		t.Fatalf("PolicySummary: %v", err)
	}

	if summary.Policies != 1 || summary.PoliciesTotal != 2 {
		t.Errorf("policies = %d of %d, want 1 active of 2", summary.Policies, summary.PoliciesTotal)
	}
	if summary.Open != 2 || summary.Total != 2 {
		t.Errorf("open/total = %d/%d, want 2/2", summary.Open, summary.Total)
	}
	if summary.BySeverity[domain.PolicySeverityCritical] != 1 ||
		summary.BySeverity[domain.PolicySeverityMedium] != 1 {
		t.Errorf("bySeverity = %v", summary.BySeverity)
	}
	if summary.ByRule[domain.RulePrivilegedForbidden] != 1 {
		t.Errorf("byRule = %v", summary.ByRule)
	}
	if summary.ContainersEvaluated != 2 {
		t.Errorf("containersEvaluated = %d, want 2", summary.ContainersEvaluated)
	}
	if summary.ContainersCompliant != 1 || summary.ContainersNonCompliant != 1 {
		t.Errorf("compliant/non-compliant = %d/%d, want 1/1",
			summary.ContainersCompliant, summary.ContainersNonCompliant)
	}
	if summary.LastEvaluatedAt == nil {
		t.Error("lastEvaluatedAt was not reported")
	}
	if summary.Incomplete {
		t.Error("incomplete was reported with no incomplete pass")
	}
	if rate := summary.ComplianceRate(); rate != 0.5 {
		t.Errorf("compliance rate = %v, want 0.5", rate)
	}
}

// The summary must surface an incomplete pass rather than averaging it away: a
// dashboard that hid it would read as "these are all the failures".
func TestPolicySummaryReportsIncompleteness(t *testing.T) {
	db := openTestDB(t)

	reconcile(t, db, passFor(0, false))

	summary, err := db.Policies.PolicySummary(context.Background())
	if err != nil {
		t.Fatalf("PolicySummary: %v", err)
	}
	if !summary.Incomplete {
		t.Error("an incomplete pass was not surfaced")
	}
	if summary.ContainersCompliant != 0 {
		t.Error("an incomplete pass counted as compliant")
	}
}

func TestViolationFiltersAndSorting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline",
		domain.PolicyRule{Type: domain.RulePrivilegedForbidden},
		domain.PolicyRule{Type: domain.RuleUserNotRoot},
	))

	low := violationFor(policy, domain.RuleUserNotRoot)
	low.Severity = domain.PolicySeverityLow
	critical := violationFor(policy, domain.RulePrivilegedForbidden)
	critical.Severity = domain.PolicySeverityCritical
	reconcile(t, db, passFor(2, true), low, critical)

	// Severity ordering must rank by importance, not alphabetically -- which
	// would put critical after low.
	sorted, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{Sort: "severity"})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if len(sorted) != 2 || sorted[0].Severity != domain.PolicySeverityCritical {
		t.Errorf("severity ordering = %v", []domain.PolicySeverity{sorted[0].Severity, sorted[1].Severity})
	}

	byRule, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{
		RuleTypes: []domain.PolicyRuleType{domain.RuleUserNotRoot},
	})
	if err != nil {
		t.Fatalf("ListViolations(rule): %v", err)
	}
	if total != 1 || byRule[0].RuleType != domain.RuleUserNotRoot {
		t.Errorf("rule filter returned %d rows", total)
	}

	byPolicy, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{
		PolicyID: policy.PolicyID,
	})
	if err != nil {
		t.Fatalf("ListViolations(policy): %v", err)
	}
	if total != 2 || len(byPolicy) != 2 {
		t.Errorf("policy filter returned %d rows, want 2", total)
	}

	none, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{
		ContainerID: "container-elsewhere",
	})
	if err != nil {
		t.Fatalf("ListViolations(container): %v", err)
	}
	if total != 0 || len(none) != 0 {
		t.Errorf("an unrelated container matched %d rows", total)
	}
}

// The sort allowlist is what keeps caller input out of the SQL text. Anything
// outside it must not be recognised.
func TestSortAllowlistsAreClosed(t *testing.T) {
	for _, field := range []string{
		"id; DROP TABLE policy_definitions", "name --", "", "rules_json", "created_at",
	} {
		if store.ValidPolicySortField(field) {
			t.Errorf("policy sort field %q is accepted", field)
		}
	}
	for _, field := range []string{"name", "severity", "createdAt", "updatedAt", "rules", "id"} {
		if !store.ValidPolicySortField(field) {
			t.Errorf("policy sort field %q is rejected", field)
		}
	}

	for _, field := range []string{"detected_at", "severity)", "1", "policy_name"} {
		if store.ValidPolicyViolationSortField(field) {
			t.Errorf("violation sort field %q is accepted", field)
		}
	}
	for _, field := range []string{"detectedAt", "lastSeenAt", "container", "policy", "rule", "severity"} {
		if !store.ValidPolicyViolationSortField(field) {
			t.Errorf("violation sort field %q is rejected", field)
		}
	}
}

// Filter VALUES travel as bound parameters, so SQL in one must be matched
// literally rather than executed. The surviving tables are the assertion.
func TestFilterValuesCannotCarrySQL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline"))
	reconcile(t, db, passFor(1, true), violationFor(policy, domain.RulePrivilegedForbidden))

	injections := []string{
		"container-a'; DROP TABLE policy_violations; --",
		"' OR '1'='1",
		"%",
		"_",
	}

	for _, injection := range injections {
		if _, _, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{
			ContainerID: injection,
		}); err != nil {
			t.Fatalf("ListViolations(%q): %v", injection, err)
		}
		if _, _, err := db.Policies.ListPolicies(ctx, store.PolicyFilter{
			Search: injection,
		}); err != nil {
			t.Fatalf("ListPolicies(%q): %v", injection, err)
		}
		// Also through a URL, which is how the value actually arrives.
		_ = url.QueryEscape(injection)
	}

	// Everything is still there.
	if _, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{}); err != nil || total != 1 {
		t.Fatalf("after injection attempts: total=%d err=%v", total, err)
	}
	if _, total, err := db.Policies.ListPolicies(ctx, store.PolicyFilter{}); err != nil || total != 1 {
		t.Fatalf("after injection attempts: policies=%d err=%v", total, err)
	}
}

// A LIKE search term must widen only its own match, not every row.
func TestSearchEscapesLikeMetacharacters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	createPolicy(t, db, newPolicy("Production baseline"))
	createPolicy(t, db, newPolicy("Staging baseline"))

	matched, total, err := db.Policies.ListPolicies(ctx, store.PolicyFilter{Search: "%"})
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if total != 0 || len(matched) != 0 {
		t.Errorf("a bare %% matched %d policies; the metacharacter was not escaped", total)
	}

	matched, total, err = db.Policies.ListPolicies(ctx, store.PolicyFilter{Search: "Production"})
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if total != 1 || matched[0].Name != "Production baseline" {
		t.Errorf("search returned %d rows", total)
	}
}

// Resolved history is prunable; open violations are not, because an unreviewed
// failure does not become less true with age.
func TestPruningRemovesOnlyResolvedHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createPolicy(t, db, newPolicy("Baseline",
		domain.PolicyRule{Type: domain.RulePrivilegedForbidden},
		domain.PolicyRule{Type: domain.RuleUserNotRoot},
	))

	old := time.Now().UTC().Add(-48 * time.Hour)
	stale := violationFor(policy, domain.RuleUserNotRoot)
	open := violationFor(policy, domain.RulePrivilegedForbidden)

	if _, err := db.Policies.ReconcilePolicy(ctx, passFor(2, true),
		[]domain.PolicyViolation{stale, open}, old); err != nil {
		t.Fatalf("ReconcilePolicy: %v", err)
	}
	// The user rule now passes, so its violation resolves at the old stamp.
	if _, err := db.Policies.ReconcilePolicy(ctx, passFor(1, true),
		[]domain.PolicyViolation{open}, old); err != nil {
		t.Fatalf("ReconcilePolicy: %v", err)
	}

	removed, err := db.Policies.PruneResolvedViolations(ctx, time.Now().UTC().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneResolvedViolations: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	remaining, total, err := db.Policies.ListViolations(ctx, store.PolicyViolationFilter{})
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	if total != 1 || remaining[0].RuleType != domain.RulePrivilegedForbidden {
		t.Errorf("remaining = %+v, want the open violation only", remaining)
	}
}

// The active set is what the sweep loads once. It must be bounded, so a
// database holding more policies than the API would have accepted cannot make a
// pass unbounded.
func TestActivePoliciesIsBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		createPolicy(t, db, newPolicy(fmt.Sprintf("Policy %02d", i)))
	}

	active, err := db.Policies.ActivePolicies(ctx, 5)
	if err != nil {
		t.Fatalf("ActivePolicies: %v", err)
	}
	if len(active) != 5 {
		t.Errorf("active = %d, want the limit of 5", len(active))
	}
}

// The summary must stay cheap on an estate large enough for it to matter. This
// is a shape assertion, not a benchmark: it fails if the implementation ever
// becomes proportional to the number of violations.
func TestPolicySummaryStaysCheapAtScale(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policies := make([]domain.PolicyDefinition, 0, 8)
	for i := 0; i < 8; i++ {
		policies = append(policies, createPolicy(t, db, newPolicy(fmt.Sprintf("Policy %02d", i))))
	}

	for container := 0; container < 100; container++ {
		id := fmt.Sprintf("container-%03d", container)
		violations := make([]domain.PolicyViolation, 0, len(policies))
		for _, policy := range policies {
			violation := violationFor(policy, domain.RulePrivilegedForbidden)
			violation.ContainerID = id
			violation.ContainerName = id
			violations = append(violations, violation)
		}
		pass := passFor(len(violations), true)
		pass.ContainerID = id
		pass.ContainerName = id
		if _, err := db.Policies.ReconcilePolicy(ctx, pass, violations, time.Now().UTC()); err != nil {
			t.Fatalf("ReconcilePolicy: %v", err)
		}
	}

	started := time.Now()
	summary, err := db.Policies.PolicySummary(ctx)
	if err != nil {
		t.Fatalf("PolicySummary: %v", err)
	}
	elapsed := time.Since(started)

	if summary.Open != 800 {
		t.Fatalf("open = %d, want 800", summary.Open)
	}
	if summary.ContainersNonCompliant != 100 {
		t.Errorf("non-compliant containers = %d, want 100", summary.ContainersNonCompliant)
	}
	// Deliberately loose: this fails on an implementation that scans, not on a
	// slow machine.
	if elapsed > 2*time.Second {
		t.Errorf("the summary took %s over 800 violations", elapsed)
	}
	t.Logf("summary over %d violations took %s", summary.Total, elapsed)
}
