package arch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Architecture tests for the allEligible policy scope.
//
// # What must stay true
//
// A policy can now say "all eligible containers". That is a widening of
// SELECTION, and the whole safety argument for it rests on three claims that a
// future edit could quietly break:
//
//  1. "Eligible" is not "present". The scope requires POSITIVE facts, and a
//     container nobody screened is not selected.
//  2. Broad selection is not broad authorisation. The scope changes what a
//     policy looks at and changes nothing else in the pipeline.
//  3. The breadth of a policy is a FIELD, never a string in a selector.
//
// A comment claiming any of those is worth little. These are the tests.

// ------------------------------------------------------- eligible ≠ present --

// TestBroadScopeRequiresPositiveEvidence fails if the zero value of the
// eligibility facts ever becomes selectable.
//
// This is the property that makes every other guarantee reachable. If the zero
// value were selectable, then any caller that built a target without screening
// it -- a new repository method, a test helper promoted to production, a
// serialisation round trip that dropped a field -- would hand the broad scope
// the entire estate.
func TestBroadScopeRequiresPositiveEvidence(t *testing.T) {
	unscreened := domain.SelectionTarget{Name: "web", Image: "nginx:1.27"}

	if selectable, _ := unscreened.BroadlySelectable(domain.SelfIdentity{}); selectable {
		t.Fatal("the zero TargetEligibility must not be broadly selectable\n" +
			"\tthe broad scope is safe only because it requires facts HarborMaster " +
			"established; a target nobody screened must be declined, not waved through")
	}

	policy := domain.UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "everything",
		Enabled:  true,
		Scope:    domain.ScopeAllEligible,
	}
	if policy.Governs(unscreened, domain.SelfIdentity{}) {
		t.Fatal("a broad policy must not govern an unscreened container")
	}
}

// TestScreeningIsTheOnlyProducerOfEligibilityFacts fails if a second place
// starts constructing the facts.
//
// One producer, called from the repository that loads a pass's targets. A
// second one is how the two drift, and the drift would be invisible: both
// would compile, and only one would be consulted on the path that matters.
func TestScreeningIsTheOnlyProducerOfEligibilityFacts(t *testing.T) {
	root := moduleRoot(t)

	// Composite literals of the facts type, outside the file that defines it
	// and outside tests, would be a second producer.
	var offenders []string
	walkSourceFiles(t, root, func(rel, source string) {
		switch {
		case strings.HasSuffix(rel, "_test.go"),
			rel == "internal/domain/update_scope.go":
			return
		}
		if strings.Contains(source, "TargetEligibility{") {
			offenders = append(offenders, rel)
		}
	})

	if len(offenders) > 0 {
		t.Fatalf("these files construct TargetEligibility directly: %v\n"+
			"\tdomain.ScreenTarget is the one place the facts are established, and it is "+
			"called from the repository that loads a pass's targets. A second producer is "+
			"how the two answers drift apart, and the drift is invisible because both compile",
			offenders)
	}
}

// ------------------------------------ broad selection ≠ broad authorisation --

// TestTheBroadScopeReachesNoMutationDecision fails if the scope is consulted
// anywhere that decides whether to CHANGE something.
//
// The scope belongs to selection. Every check after selection -- the pause, the
// labels, the ceiling, the recommendation, the window, the mode, the budgets,
// and all four preflights -- must be blind to how the container was selected. A
// `Scope ==` in any of them would be the first line of "except when the policy
// is broad", which is exactly the shortcut this test exists to refuse.
func TestTheBroadScopeReachesNoMutationDecision(t *testing.T) {
	root := moduleRoot(t)

	// Files that decide whether or how the host changes. None of them may
	// branch on the scope.
	mutationDeciders := []string{
		"internal/service/acquisition.go",
		"internal/service/execution.go",
		"internal/service/execution_preflight.go",
		"internal/service/execution_pipeline.go",
		"internal/service/execution_verify.go",
		"internal/service/rollback.go",
		"internal/service/rollback_preflight.go",
		"internal/service/rollback_pipeline.go",
		"internal/service/automation_follow.go",
	}

	for _, rel := range mutationDeciders {
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v\n"+
				"\tthis file decides whether the host changes; if it moved, move this "+
				"test with it rather than deleting the entry", rel, err)
			continue
		}
		text := string(source)
		for _, marker := range []string{"ScopeAllEligible", "Scope.Broad", ".Scope ==", ".Scope !="} {
			if strings.Contains(text, marker) {
				t.Errorf("%s branches on the policy scope (%q)\n"+
					"\tthe scope decides which containers a policy LOOKS AT. Every check "+
					"after selection must be blind to how a container was selected, or "+
					"\"broad selection is not broad authorisation\" stops being true",
					rel, marker)
			}
		}
	}
}

// TestEveryRefusalSurvivesTheBroadScope walks a container that must never be
// enrolled through a broad policy, and fails if any of them comes out selected.
//
// A behavioural companion to the textual test above: that one proves the code
// does not mention the scope, this one proves the outcome does not depend on it.
func TestEveryRefusalSurvivesTheBroadScope(t *testing.T) {
	self := domain.SelfIdentity{
		ContainerID:   strings.Repeat("a", 64),
		ContainerName: "harbormaster",
		ImageRef:      "ghcr.io/aznyi/harbormaster:1",
	}
	broad := domain.UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "everything",
		Enabled:  true,
		Scope:    domain.ScopeAllEligible,
		Selector: domain.UpdateSelector{Exclude: []string{"database"}},
	}

	screen := func(name, image string, labels map[string]string) domain.SelectionTarget {
		return domain.SelectionTarget{
			Name:        name,
			Image:       image,
			Labels:      labels,
			Eligibility: domain.ScreenTarget(name, image, labels),
		}
	}

	mustNotSelect := map[string]domain.SelectionTarget{
		"HarborMaster itself": screen("harbormaster", "ghcr.io/aznyi/harbormaster:1", nil),
		"a parked original":   screen("web.hm-old-exec_0123456789abcdef0123", "nginx:1", nil),
		"a quarantined replacement": screen(
			"web.hm-failed-exec_0123456789abcdef0123", "nginx:1", nil),
		"an excluded container": screen("database", "postgres:16", nil),
		"an opted-out container": screen("legacy", "app:1", map[string]string{
			domain.LabelHarborMasterEnabled: "false",
		}),
	}
	for description, target := range mustNotSelect {
		if broad.Governs(target, self) {
			t.Errorf("the broad scope selected %s\n"+
				"\tthis is one of the containers the scope must never enrol; if the "+
				"definition of eligible changed, this test is the record of what it "+
				"used to promise", description)
		}
	}

	// And the control: an ordinary workload IS selected, so the test above is
	// not passing because the scope selects nothing at all.
	if !broad.Governs(screen("web", "nginx:1.27", nil), self) {
		t.Fatal("the broad scope must still select an ordinary workload")
	}
}

// ------------------------------------------ breadth is a field, not a string --

// TestBreadthIsNotExpressibleAsASelectorString fails if any of the three
// stringly-typed ways of saying "everything" starts working.
//
// Each was possible before the scope existed, and each was refused. The scope
// must not have made any of them legal as a side effect.
func TestBreadthIsNotExpressibleAsASelectorString(t *testing.T) {
	limits := domain.DefaultUpdatePolicyLimits()

	base := func() domain.UpdatePolicy {
		return domain.UpdatePolicy{
			PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
			Name:                  "sneaky",
			Enabled:               true,
			Scope:                 domain.ScopeSelector,
			Strategy:              domain.StrategyPatch,
			Mode:                  domain.ModeObserve,
			MinimumRecommendation: domain.RecommendProceed,
			Window:                domain.MaintenanceWindow{AlwaysOpen: true},
		}
	}

	t.Run("a bare wildcard image pattern is refused", func(t *testing.T) {
		policy := base()
		policy.Selector = domain.UpdateSelector{Images: []string{"*"}}
		policy.Normalise()
		if err := policy.Validate(limits); err == nil {
			t.Fatal("`*` must still be refused by name")
		}
	})

	t.Run("an empty selector is refused and governs nothing", func(t *testing.T) {
		policy := base()
		policy.Normalise()
		if err := policy.Validate(limits); err == nil {
			t.Fatal("an empty selector must still be refused")
		}
		if policy.Governs(domain.SelectionTarget{
			Name:        "web",
			Image:       "nginx:1.27",
			Eligibility: domain.ScreenTarget("web", "nginx:1.27", nil),
		}, domain.SelfIdentity{}) {
			t.Fatal("an empty selector must still govern nothing")
		}
	})

	t.Run("the literal scope name in include is just a container name", func(t *testing.T) {
		policy := base()
		policy.Selector = domain.UpdateSelector{Include: []string{"allEligible"}}
		policy.Normalise()
		if err := policy.Validate(limits); err != nil {
			t.Fatalf("it is a legal container name: %v", err)
		}
		if policy.Governs(domain.SelectionTarget{
			Name:        "web",
			Image:       "nginx:1.27",
			Eligibility: domain.ScreenTarget("web", "nginx:1.27", nil),
		}, domain.SelfIdentity{}) {
			t.Fatal("a magic string in `include` must select nothing but a container of that name")
		}
	})
}

// TestTheScopeVocabularyIsClosed fails if a value outside the two is accepted
// or silently reinterpreted.
func TestTheScopeVocabularyIsClosed(t *testing.T) {
	if len(domain.UpdateScopes) != 2 {
		t.Fatalf("the scope vocabulary has %d values\n"+
			"\tadding one means auditing every place breadth is decided: the migration's "+
			"CHECK, the OpenAPI enum, Governs, the tie-break in SelectUpdatePolicy, and "+
			"the selector validation that branches on it", len(domain.UpdateScopes))
	}

	for _, rejected := range []string{"all", "everything", "*", "ALLELIGIBLE", " allEligible"} {
		if domain.ValidUpdateScope(rejected) {
			t.Errorf("%q must not name a scope", rejected)
		}
	}

	// An unrecognised scope that somehow reached a policy governs nothing.
	policy := domain.UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Enabled:  true,
		Scope:    "everything",
		Selector: domain.UpdateSelector{Include: []string{"web"}},
	}
	if policy.Governs(domain.SelectionTarget{
		Name:        "web",
		Image:       "nginx:1.27",
		Eligibility: domain.ScreenTarget("web", "nginx:1.27", nil),
	}, domain.SelfIdentity{}) {
		t.Fatal("a scope this build cannot evaluate must govern nothing")
	}
}

// TestTheMigrationConstrainsTheScopeColumn fails if the schema stops enforcing
// the vocabulary.
//
// The domain refuses an unknown scope and the database must too. Two layers,
// because a policy row is also reachable by anything that can open the file,
// and "the application always validates" is a claim about today's callers.
func TestTheMigrationConstrainsTheScopeColumn(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "store", "migrations",
		"0023_update_policy_scope.sql")

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the scope migration: %v", err)
	}
	text := string(source)

	for _, required := range []string{
		"DEFAULT 'selector'",
		"CHECK (scope IN ('selector', 'allEligible'))",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("the scope migration no longer contains %q\n"+
				"\tthe DEFAULT is what makes every pre-existing policy come out narrow, "+
				"and the CHECK is what stops a value the domain would refuse from being "+
				"written by anything else", required)
		}
	}

	// The default must be the narrow scope. A default of allEligible would
	// broaden every policy already stored, which is the one outcome this
	// migration must be incapable of.
	if strings.Contains(text, "DEFAULT 'allEligible'") {
		t.Fatal("the scope column must default to 'selector'; defaulting to the broad " +
			"scope would silently widen every policy that already exists")
	}
}

// walkSourceFiles visits every non-vendored Go file under root as TEXT.
//
// Deliberately not the AST walk in notification_arch_test.go: this test asks
// whether a type is CONSTRUCTED anywhere, and a composite literal is a shape
// that reads unambiguously in source. The two helpers answer different
// questions and neither is a worse version of the other.
func walkSourceFiles(t *testing.T, root string, visit func(rel, source string)) {
	t.Helper()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "web", "bin", "data":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), string(source))
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
}
