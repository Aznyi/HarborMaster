package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Policy rule tests.
//
// The rule evaluator is pure, so it can be tested exhaustively, and it is:
// TestEveryRuleTypeIsEvaluatedBothWays asserts that every type in the catalogue
// has both a passing and a failing case, so a rule added later without a test
// fails the suite rather than shipping unchecked.
//
// The other properties defended here are the ones a mistake would be expensive
// in: the matcher must not be exponential, a violation must never render an
// environment variable VALUE, and severity must follow the rule rather than the
// container.

// ------------------------------------------------------------ the matcher --

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		// Literals.
		{"nginx", "nginx", true},
		{"nginx", "nginxx", false},
		{"", "", true},
		{"nginx", "", false},

		// Single-character wildcard.
		{"nginx:?", "nginx:1", true},
		{"nginx:?", "nginx:12", false},
		{"?", "", false},

		// Runs.
		{"*", "", true},
		{"*", "anything at all", true},
		{"nginx:*", "nginx:1.25", true},
		{"*:latest", "registry.example.com/app:latest", true},
		{"*:latest", "registry.example.com/app:1.2", false},
		{"registry.example.com/*", "registry.example.com/team/app:1", true},
		{"registry.example.com/*", "docker.io/library/app:1", false},

		// A '*' spans '/' deliberately: an image reference is one string to an
		// operator, not a path.
		{"docker.io/*/nginx", "docker.io/library/nginx", true},

		// Multiple wildcards, including the shape that makes a backtracking
		// matcher exponential.
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"*a*a*a*b", strings.Repeat("a", 40), false},

		// Metacharacters that are NOT part of the syntax match literally.
		{"app[1]", "app[1]", true},
		{"app[1]", "app1", false},
		{`a\b`, `a\b`, true},

		// Case sensitivity: Docker treats these as distinct, so the matcher
		// must too.
		{"NGINX", "nginx", false},
	}

	for _, tc := range cases {
		if got := domain.MatchGlob(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

// The bound is what makes the worst case computable, so it is asserted rather
// than assumed. A subject past the cap must not match a wildcard pattern.
func TestGlobRefusesAnOversizedSubject(t *testing.T) {
	huge := strings.Repeat("a", domain.MaxPolicySubjectBytes+1)
	if domain.MatchGlob("*", huge) {
		t.Error("an oversized subject matched a wildcard; the bound is not enforced")
	}
	if domain.MatchGlob(strings.Repeat("a", domain.MaxPolicyPatternBytes+1), "a") {
		t.Error("an oversized pattern was evaluated")
	}
}

// The pathological pattern for a backtracking matcher. This test is about
// TERMINATION and time: an exponential implementation does not finish.
func TestGlobTerminatesOnThePathologicalPattern(t *testing.T) {
	pattern := strings.Repeat("a*", domain.MaxPolicyWildcards) + "b"
	subject := strings.Repeat("a", domain.MaxPolicySubjectBytes)

	if domain.MatchGlob(pattern, subject) {
		t.Error("a subject with no 'b' matched a pattern requiring one")
	}
}

func TestPatternValidationRejectsWhatTheMatcherShouldNeverSee(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{"a plain reference", "registry.example.com/app:*", false},
		{"empty", "", true},
		{"too long", strings.Repeat("a", domain.MaxPolicyPatternBytes+1), true},
		{"too many wildcards", strings.Repeat("*a", domain.MaxPolicyWildcards+1), true},
		{"a newline", "app\nname", true},
		{"a NUL", "app\x00name", true},
		{"invalid UTF-8", "app\xff", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := domain.ValidatePolicyPattern(tc.pattern)
			if (message != "") != tc.wantErr {
				t.Fatalf("ValidatePolicyPattern = %q, wantErr %v", message, tc.wantErr)
			}
			// A validation message must never reflect the caller's value.
			if message != "" && tc.pattern != "" && strings.Contains(message, tc.pattern) {
				t.Errorf("the message echoed the pattern: %q", message)
			}
		})
	}
}

// -------------------------------------------------------------- the rules --

// target builds a fully compliant container, which each test then breaks in
// exactly one way. Starting from compliance makes each case's subject obvious.
func compliantTarget() domain.PolicyTarget {
	return domain.PolicyTarget{
		ContainerID:       "abc123",
		ContainerName:     "web",
		Image:             "registry.example.com/team/app:1.2.3",
		ImageRepository:   "registry.example.com/team/app",
		EnvNames:          []string{"APP_ENV", "DATABASE_URL"},
		LabelKeys:         []string{"com.example.owner", "com.example.tier"},
		CapabilitiesAdded: []string{"NET_BIND_SERVICE"},
		Networks:          []string{"app_backend"},
		RestartPolicy:     "unless-stopped",
		User:              "10001:10001",
		MemoryBytes:       512 << 20,
		NanoCPUs:          1_000_000_000,
		Privileged:        false,
		ReadonlyRootfs:    true,
		HasHealthCheck:    true,
	}
}

// ruleCase is one rule type with a target that passes and one that fails.
type ruleCase struct {
	rule domain.PolicyRule
	// breaks mutates a compliant target into a non-compliant one.
	breaks func(*domain.PolicyTarget)
	// wantObserved, when set, must appear in the failing result's Observed.
	wantObserved string
}

// ruleCases covers EVERY rule type in the catalogue. The completeness of this
// map is asserted below.
func ruleCases() map[domain.PolicyRuleType]ruleCase {
	return map[domain.PolicyRuleType]ruleCase{
		domain.RulePrivilegedForbidden: {
			rule:   domain.PolicyRule{Type: domain.RulePrivilegedForbidden},
			breaks: func(t *domain.PolicyTarget) { t.Privileged = true },
		},
		domain.RuleReadOnlyRootFilesystem: {
			rule:   domain.PolicyRule{Type: domain.RuleReadOnlyRootFilesystem},
			breaks: func(t *domain.PolicyTarget) { t.ReadonlyRootfs = false },
		},
		domain.RuleImageAllowlist: {
			rule: domain.PolicyRule{
				Type:   domain.RuleImageAllowlist,
				Values: []string{"registry.example.com/*"},
			},
			breaks: func(t *domain.PolicyTarget) {
				t.Image = "docker.io/library/nginx:latest"
				t.ImageRepository = "docker.io/library/nginx"
			},
			wantObserved: "docker.io/library/nginx:latest",
		},
		domain.RuleImageDenylist: {
			rule: domain.PolicyRule{
				Type:   domain.RuleImageDenylist,
				Values: []string{"*:latest"},
			},
			breaks: func(t *domain.PolicyTarget) { t.Image = "registry.example.com/team/app:latest" },
		},
		domain.RuleRequiredLabels: {
			rule: domain.PolicyRule{
				Type:   domain.RuleRequiredLabels,
				Values: []string{"com.example.owner"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.LabelKeys = []string{"com.example.tier"} },
			wantObserved: "com.example.owner",
		},
		domain.RuleForbiddenLabels: {
			rule: domain.PolicyRule{
				Type:   domain.RuleForbiddenLabels,
				Values: []string{"internal.*"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.LabelKeys = append(t.LabelKeys, "internal.debug") },
			wantObserved: "internal.debug",
		},
		domain.RuleRequiredEnv: {
			rule: domain.PolicyRule{
				Type:   domain.RuleRequiredEnv,
				Values: []string{"APP_ENV"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.EnvNames = []string{"DATABASE_URL"} },
			wantObserved: "APP_ENV",
		},
		domain.RuleForbiddenEnv: {
			rule: domain.PolicyRule{
				Type:   domain.RuleForbiddenEnv,
				Values: []string{"AWS_*"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.EnvNames = append(t.EnvNames, "AWS_SECRET_ACCESS_KEY") },
			wantObserved: "AWS_SECRET_ACCESS_KEY",
		},
		domain.RuleRequiredCapabilities: {
			rule: domain.PolicyRule{
				Type:   domain.RuleRequiredCapabilities,
				Values: []string{"NET_BIND_SERVICE"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.CapabilitiesAdded = nil },
			wantObserved: "NET_BIND_SERVICE",
		},
		domain.RuleForbiddenCapabilities: {
			rule: domain.PolicyRule{
				Type:   domain.RuleForbiddenCapabilities,
				Values: []string{"SYS_ADMIN"},
			},
			breaks: func(t *domain.PolicyTarget) {
				t.CapabilitiesAdded = append(t.CapabilitiesAdded, "SYS_ADMIN")
			},
			wantObserved: "SYS_ADMIN",
		},
		domain.RuleMemoryLimitRequired: {
			rule:   domain.PolicyRule{Type: domain.RuleMemoryLimitRequired},
			breaks: func(t *domain.PolicyTarget) { t.MemoryBytes = 0 },
		},
		domain.RuleCPULimitRequired: {
			rule: domain.PolicyRule{Type: domain.RuleCPULimitRequired},
			breaks: func(t *domain.PolicyTarget) {
				t.NanoCPUs = 0
				t.CPUQuota = 0
			},
		},
		domain.RuleRestartPolicyAllowlist: {
			rule: domain.PolicyRule{
				Type:   domain.RuleRestartPolicyAllowlist,
				Values: []string{"unless-stopped", "on-failure"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.RestartPolicy = "always" },
			wantObserved: "always",
		},
		domain.RuleNetworkAllowlist: {
			rule: domain.PolicyRule{
				Type:   domain.RuleNetworkAllowlist,
				Values: []string{"app_backend"},
			},
			breaks:       func(t *domain.PolicyTarget) { t.Networks = []string{"host"} },
			wantObserved: "host",
		},
		domain.RuleUserNotRoot: {
			rule:   domain.PolicyRule{Type: domain.RuleUserNotRoot},
			breaks: func(t *domain.PolicyTarget) { t.User = "root" },
		},
		domain.RuleHealthCheckRequired: {
			rule:   domain.PolicyRule{Type: domain.RuleHealthCheckRequired},
			breaks: func(t *domain.PolicyTarget) { t.HasHealthCheck = false },
		},
	}
}

// The completeness assertion. A rule added to the catalogue without a case here
// fails this test, which is what stops an unchecked rule from shipping.
func TestEveryRuleTypeIsEvaluatedBothWays(t *testing.T) {
	cases := ruleCases()

	if len(cases) != len(domain.PolicyRuleTypes) {
		t.Fatalf("ruleCases covers %d rule types, the catalogue has %d",
			len(cases), len(domain.PolicyRuleTypes))
	}

	for _, ruleType := range domain.PolicyRuleTypes {
		tc, ok := cases[ruleType]
		if !ok {
			t.Errorf("rule type %q has no test case", ruleType)
			continue
		}

		t.Run(string(ruleType), func(t *testing.T) {
			passing := compliantTarget()
			if result := domain.EvaluatePolicyRule(tc.rule, passing); !result.Compliant {
				t.Errorf("a compliant container failed: reason %q", result.Reason)
			}

			failing := compliantTarget()
			tc.breaks(&failing)

			result := domain.EvaluatePolicyRule(tc.rule, failing)
			if result.Compliant {
				t.Fatal("a non-compliant container passed")
			}
			if result.Reason == "" {
				t.Error("a violation carries no reason")
			}
			if tc.wantObserved != "" && !strings.Contains(result.Observed, tc.wantObserved) {
				t.Errorf("observed = %q, want it to name %q", result.Observed, tc.wantObserved)
			}
		})
	}
}

// Every rule type in the catalogue must carry a description, because the
// editor renders it and an operator choosing a rule with no explanation is
// choosing blind.
func TestTheCatalogueIsFullyDescribed(t *testing.T) {
	catalogue := domain.PolicyRuleCatalogue()
	if len(catalogue) != len(domain.PolicyRuleTypes) {
		t.Fatalf("catalogue has %d entries, %d rule types", len(catalogue), len(domain.PolicyRuleTypes))
	}
	for _, spec := range catalogue {
		if spec.Label == "" || spec.Description == "" {
			t.Errorf("%s: label or description is empty", spec.Type)
		}
		if spec.RequiresValues && spec.ValueKind == domain.ValueKindNone {
			t.Errorf("%s: requires values but declares no value kind", spec.Type)
		}
		if !spec.RequiresValues && spec.ValueKind != domain.ValueKindNone {
			t.Errorf("%s: declares a value kind but takes no values", spec.Type)
		}
	}
}

// ------------------------------------------------------------ the corners --

// An unset user is the case this rule exists for: the container inherits the
// image's default, which is root unless the image says otherwise.
func TestAnUnsetUserCountsAsRoot(t *testing.T) {
	for _, user := range []string{"", "  ", "root", "0", "0:0", "root:root", "ROOT"} {
		if !domain.IsRootUser(user) {
			t.Errorf("IsRootUser(%q) = false, want true", user)
		}
	}
	for _, user := range []string{"app", "1000", "1000:1000", "nobody:nogroup"} {
		if domain.IsRootUser(user) {
			t.Errorf("IsRootUser(%q) = true, want false", user)
		}
	}
}

// An unset user must produce a reason that says WHY, because "runs as root" for
// a container that declares no user at all would be an overstatement.
func TestAnUnsetUserExplainsItself(t *testing.T) {
	target := compliantTarget()
	target.User = ""

	result := domain.EvaluatePolicyRule(domain.PolicyRule{Type: domain.RuleUserNotRoot}, target)
	if result.Compliant {
		t.Fatal("an unset user passed the non-root rule")
	}
	if !strings.Contains(result.Reason, "inherits") {
		t.Errorf("reason = %q, want it to explain the inherited default", result.Reason)
	}
}

// Docker accepts both spellings and is case-insensitive, so a policy naming one
// form must match a container declaring the other.
func TestCapabilityNamesAreNormalised(t *testing.T) {
	for _, name := range []string{"CAP_SYS_ADMIN", "sys_admin", "cap_sys_admin", " SYS_ADMIN "} {
		if got := domain.NormaliseCapability(name); got != "SYS_ADMIN" {
			t.Errorf("NormaliseCapability(%q) = %q, want SYS_ADMIN", name, got)
		}
	}

	target := compliantTarget()
	target.CapabilitiesAdded = []string{"SYS_ADMIN"}

	rule := domain.PolicyRule{
		Type:   domain.RuleForbiddenCapabilities,
		Values: []string{"cap_sys_admin"},
	}
	if domain.EvaluatePolicyRule(rule, target).Compliant {
		t.Error("a differently-spelled capability was not matched")
	}
}

// capAdd of ALL grants everything, so a forbidden-capability rule must catch it
// even though the specific name never appears.
func TestCapAddAllGrantsEveryCapability(t *testing.T) {
	target := compliantTarget()
	target.CapabilitiesAll = true

	rule := domain.PolicyRule{
		Type:   domain.RuleForbiddenCapabilities,
		Values: []string{"SYS_ADMIN"},
	}
	result := domain.EvaluatePolicyRule(rule, target)
	if result.Compliant {
		t.Fatal("capAdd ALL passed a forbidden-capability rule")
	}
	if !strings.Contains(result.Reason, "ALL") {
		t.Errorf("reason = %q, want it to name the ALL grant", result.Reason)
	}
}

// A container attached to no network shares another container's namespace.
// Treating "nothing to check" as compliant would let the one mode HarborMaster
// cannot see through be the one that always passes.
func TestNoAttachedNetworkFailsAnAllowlist(t *testing.T) {
	target := compliantTarget()
	target.Networks = nil

	rule := domain.PolicyRule{
		Type:   domain.RuleNetworkAllowlist,
		Values: []string{"app_backend"},
	}
	result := domain.EvaluatePolicyRule(rule, target)
	if result.Compliant {
		t.Fatal("a container with no network passed an allowlist")
	}
	if !strings.Contains(result.Reason, "namespace") {
		t.Errorf("reason = %q, want it to explain the shared namespace", result.Reason)
	}
}

// The allowlist accepts either the full reference or the repository, so a
// policy can name a repository without enumerating every tag.
func TestTheImageAllowlistAcceptsEitherForm(t *testing.T) {
	target := compliantTarget()

	for _, pattern := range []string{
		"registry.example.com/team/app",       // repository exactly
		"registry.example.com/team/app:1.2.3", // full reference exactly
		"registry.example.com/*",              // registry prefix
	} {
		rule := domain.PolicyRule{Type: domain.RuleImageAllowlist, Values: []string{pattern}}
		if !domain.EvaluatePolicyRule(rule, target).Compliant {
			t.Errorf("pattern %q did not match", pattern)
		}
	}
}

// CPU shares are a relative weight, not a cap. Accepting them would let a
// container with no limit at all report a limit.
func TestCPUSharesDoNotSatisfyTheCPULimitRule(t *testing.T) {
	target := compliantTarget()
	target.NanoCPUs = 0
	target.CPUQuota = 0

	rule := domain.PolicyRule{Type: domain.RuleCPULimitRequired}
	if domain.EvaluatePolicyRule(rule, target).Compliant {
		t.Fatal("a container with no cpu cap passed")
	}

	// A quota alone is a cap, and must satisfy the rule.
	target.CPUQuota = 50000
	if !domain.EvaluatePolicyRule(rule, target).Compliant {
		t.Error("a cpu quota did not satisfy the rule")
	}
}

// A rendered list must stay bounded: a container with hundreds of matching
// labels must not produce a value that is then stored on every pass and served
// to every dashboard.
func TestRenderedValuesAreBounded(t *testing.T) {
	target := compliantTarget()
	target.LabelKeys = nil
	for i := 0; i < 400; i++ {
		target.LabelKeys = append(target.LabelKeys, "internal.key"+strings.Repeat("x", 20))
	}

	rule := domain.PolicyRule{
		Type:   domain.RuleForbiddenLabels,
		Values: []string{"internal.*"},
	}
	result := domain.EvaluatePolicyRule(rule, target)
	if result.Compliant {
		t.Fatal("forbidden labels passed")
	}
	// The schema caps these columns at 1024 bytes, so the renderer must stay
	// under it or the write fails.
	if len(result.Observed) > 1024 || len(result.Reason) > 1024 {
		t.Errorf("observed=%d reason=%d bytes; the column limit is 1024",
			len(result.Observed), len(result.Reason))
	}
	// The overflow is counted rather than dropped silently.
	if !strings.Contains(result.Observed, "more") {
		t.Errorf("observed = %q, want it to report the overflow", result.Observed)
	}
}

// Severity follows the RULE, not the container. An override applies only where
// it is set; everything else inherits the policy's default.
func TestRuleSeverityOverridesThePolicyDefault(t *testing.T) {
	inherited := domain.PolicyRule{Type: domain.RulePrivilegedForbidden}
	if got := inherited.EffectiveSeverity(domain.PolicySeverityLow); got != domain.PolicySeverityLow {
		t.Errorf("inherited severity = %q, want low", got)
	}

	overridden := domain.PolicyRule{
		Type:     domain.RulePrivilegedForbidden,
		Severity: domain.PolicySeverityCritical,
	}
	if got := overridden.EffectiveSeverity(domain.PolicySeverityLow); got != domain.PolicySeverityCritical {
		t.Errorf("overridden severity = %q, want critical", got)
	}

	// A policy with no severity at all still yields a usable one rather than
	// an empty string, which the CHECK constraint would reject.
	if got := inherited.EffectiveSeverity(""); got != domain.PolicySeverityMedium {
		t.Errorf("defaulted severity = %q, want medium", got)
	}
}

// A rule type the engine does not understand returns COMPLIANT. A rule the
// engine cannot evaluate has established nothing, and manufacturing a violation
// from ignorance would put an unfixable row on an operator's dashboard.
func TestAnUnknownRuleTypeIsNotAViolation(t *testing.T) {
	rule := domain.PolicyRule{Type: domain.PolicyRuleType("somethingFromTheFuture")}
	if !domain.EvaluatePolicyRule(rule, compliantTarget()).Compliant {
		t.Error("an unknown rule type produced a violation")
	}
}

// Severity ranking must order by how much a violation matters, not by the
// spelling of the word.
func TestSeverityRanksByImportance(t *testing.T) {
	if domain.PolicySeverityCritical.Rank() <= domain.PolicySeverityHigh.Rank() {
		t.Error("critical does not outrank high")
	}
	if domain.PolicySeverityLow.Rank() <= domain.PolicySeverity("nonsense").Rank() {
		t.Error("an unknown severity outranks low")
	}
}

// Operator statuses are a strict subset of all statuses, and must exclude the
// two the engine owns.
func TestOperatorStatusesExcludeTheEngineOwnedOnes(t *testing.T) {
	for _, status := range []string{"active", "resolved"} {
		if domain.ValidOperatorPolicyStatus(status) {
			t.Errorf("%q is accepted as an operator transition", status)
		}
		if !domain.ValidPolicyViolationStatus(status) {
			t.Errorf("%q is not a known status", status)
		}
	}
	for _, status := range []string{"acknowledged", "exempted"} {
		if !domain.ValidOperatorPolicyStatus(status) {
			t.Errorf("%q is not accepted as an operator transition", status)
		}
	}
	// Neither operator status closes the violation: both must count as open,
	// which is what keeps an acknowledged failure on the dashboard.
	for _, status := range domain.OperatorPolicyStatuses {
		if !status.Open() {
			t.Errorf("%q reports as closed; acknowledgement must not resolve", status)
		}
	}
}

// The compliance rate is computed over EVALUATED containers. A denominator that
// silently included containers nobody checked would improve every time coverage
// got worse.
func TestComplianceRateUsesEvaluatedContainers(t *testing.T) {
	summary := domain.PolicySummary{ContainersEvaluated: 4, ContainersCompliant: 3}
	if got := summary.ComplianceRate(); got != 0.75 {
		t.Errorf("rate = %v, want 0.75", got)
	}

	empty := domain.PolicySummary{}
	if got := empty.ComplianceRate(); got != 0 {
		t.Errorf("rate with nothing evaluated = %v, want 0", got)
	}
}
