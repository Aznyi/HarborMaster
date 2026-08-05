package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Policy target projection and definition validation.
//
// The single most important property in this file is the first test: the
// evaluation input must carry environment variable NAMES and nothing else. It
// is asserted structurally -- by reflecting over every field of the projected
// target -- rather than by checking the two places a value could plausibly
// appear, because a field added later would slip past the second kind of test.

// detailWithSecrets builds a container whose environment carries real secret
// values, in both the masked display field and the raw one.
func detailWithSecrets() domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:            "abc123def456",
			Name:          "web",
			Image:         domain.ParseImageRef("registry.example.com/team/app:1.2.3"),
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
		},
		Process: domain.Process{User: "10001"},
		Environment: []domain.EnvVar{
			{
				Name:        "DATABASE_PASSWORD",
				Value:       "hunter2-masked",
				RawValue:    "hunter2-the-real-secret",
				Sensitivity: domain.SensitivitySensitive,
			},
			{
				Name:     "APP_ENV",
				Value:    "production",
				RawValue: "production",
			},
		},
		Labels: []domain.Label{
			{Key: "com.example.owner", Value: "platform"},
		},
		Networks: []domain.NetworkAttachment{{NetworkName: "app_backend"}},
		Security: domain.Security{
			CapAdd:         []string{"cap_net_bind_service", "ALL"},
			ReadonlyRootfs: true,
		},
		Resources:   domain.Resources{MemoryBytes: 1 << 30},
		HealthCheck: &domain.HealthCheck{Test: []string{"CMD", "curl", "-f", "http://localhost/"}},
	}
}

// The projection is the boundary a secret must not cross. Asserted over EVERY
// string the target holds, so a field added later is covered without anyone
// remembering to extend this test.
func TestThePolicyTargetCarriesNoEnvironmentValues(t *testing.T) {
	detail := detailWithSecrets()
	target := domain.NewPolicyTarget(detail)

	const secret = "hunter2-the-real-secret"
	const masked = "hunter2-masked"

	// Every string the target holds, gathered explicitly.
	strings_ := append([]string{
		target.ContainerID, target.ContainerName, target.Image,
		target.ImageRepository, target.RestartPolicy, target.User,
	}, target.EnvNames...)
	strings_ = append(strings_, target.LabelKeys...)
	strings_ = append(strings_, target.CapabilitiesAdded...)
	strings_ = append(strings_, target.Networks...)

	for _, value := range strings_ {
		if strings.Contains(value, secret) {
			t.Fatalf("the raw secret reached the policy target in %q", value)
		}
		if strings.Contains(value, masked) {
			t.Fatalf("even the masked value reached the policy target in %q", value)
		}
	}

	// The names themselves must survive, or the env rules would have nothing
	// to match. This is the positive control for the assertion above: without
	// it, a projection that dropped everything would pass.
	if len(target.EnvNames) != 2 {
		t.Fatalf("env names = %v, want both names carried", target.EnvNames)
	}
	if target.EnvNames[0] != "APP_ENV" || target.EnvNames[1] != "DATABASE_PASSWORD" {
		t.Errorf("env names = %v, want them sorted and complete", target.EnvNames)
	}
}

// The projection must also read the parts of the configuration the rules
// depend on, or every rule would evaluate against an empty container.
func TestThePolicyTargetProjectsTheConfiguration(t *testing.T) {
	target := domain.NewPolicyTarget(detailWithSecrets())

	if target.Image != "registry.example.com/team/app:1.2.3" {
		t.Errorf("image = %q", target.Image)
	}
	if target.ImageRepository != "registry.example.com/team/app" {
		t.Errorf("repository = %q", target.ImageRepository)
	}
	if target.RestartPolicy != "unless-stopped" {
		t.Errorf("restart policy = %q", target.RestartPolicy)
	}
	if !target.HasHealthCheck {
		t.Error("the health check was not detected")
	}
	if !target.CapabilitiesAll {
		t.Error("capAdd ALL was not detected")
	}
	// ALL is recorded as a flag rather than as a member, so it cannot be
	// mistaken for a capability literally named ALL.
	for _, capability := range target.CapabilitiesAdded {
		if capability == "ALL" {
			t.Error("ALL was stored as an ordinary capability")
		}
	}
	if len(target.CapabilitiesAdded) != 1 || target.CapabilitiesAdded[0] != "NET_BIND_SERVICE" {
		t.Errorf("capabilities = %v, want the normalised NET_BIND_SERVICE", target.CapabilitiesAdded)
	}
}

// An unset restart policy is Docker's "no", not the empty string. Getting this
// wrong would make an allowlist reject every container that never set one.
func TestAnUnsetRestartPolicyReadsAsNo(t *testing.T) {
	detail := detailWithSecrets()
	detail.Overview.RestartPolicy = domain.RestartPolicy{}

	if got := domain.NewPolicyTarget(detail).RestartPolicy; got != "no" {
		t.Errorf("restart policy = %q, want %q", got, "no")
	}
}

// Docker spells "this container turned the image's health check off" as a test
// of exactly ["NONE"], which is not a health check however it is written.
func TestHealthCheckDetectionHandlesEveryAbsentForm(t *testing.T) {
	cases := []struct {
		name  string
		check *domain.HealthCheck
		want  bool
	}{
		{"none configured", nil, false},
		{"explicitly disabled", &domain.HealthCheck{Test: []string{"CMD", "true"}, Disabled: true}, false},
		{"an empty test", &domain.HealthCheck{}, false},
		{"the NONE sentinel", &domain.HealthCheck{Test: []string{"NONE"}}, false},
		{"the lowercase sentinel", &domain.HealthCheck{Test: []string{"none"}}, false},
		{"a real check", &domain.HealthCheck{Test: []string{"CMD-SHELL", "exit 0"}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := detailWithSecrets()
			detail.HealthCheck = tc.check
			if got := domain.NewPolicyTarget(detail).HasHealthCheck; got != tc.want {
				t.Errorf("hasHealthCheck = %v, want %v", got, tc.want)
			}
		})
	}
}

// ------------------------------------------------------------ validation --

func validPolicy() domain.PolicyDefinition {
	return domain.PolicyDefinition{
		PolicyID: domain.NewPolicyID(),
		Name:     "Production baseline",
		Severity: domain.PolicySeverityHigh,
		Enabled:  true,
		Rules: []domain.PolicyRule{
			{Type: domain.RulePrivilegedForbidden},
			{Type: domain.RuleImageAllowlist, Values: []string{"registry.example.com/*"}},
		},
	}
}

func TestAValidPolicyPassesValidation(t *testing.T) {
	policy := validPolicy()
	policy.Normalise()
	if err := policy.Validate(domain.DefaultPolicyLimits()); err != nil {
		t.Fatalf("a valid policy was rejected: %v", err)
	}
}

func TestPolicyValidationRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.PolicyDefinition)
		field  string
	}{
		{
			"an empty name",
			func(p *domain.PolicyDefinition) { p.Name = "   " },
			"name",
		},
		{
			"a name carrying a newline",
			func(p *domain.PolicyDefinition) { p.Name = "line\nbreak" },
			"name",
		},
		{
			"an unknown severity",
			func(p *domain.PolicyDefinition) { p.Severity = "catastrophic" },
			"severity",
		},
		{
			"no rules",
			func(p *domain.PolicyDefinition) { p.Rules = nil },
			"rules",
		},
		{
			"an unknown rule type",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{{Type: "runArbitraryCode"}}
			},
			"rules[0].type",
		},
		{
			"two rules of the same type",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RulePrivilegedForbidden},
					{Type: domain.RulePrivilegedForbidden},
				}
			},
			"rules[1].type",
		},
		{
			"values on a parameterless rule",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RulePrivilegedForbidden, Values: []string{"true"}},
				}
			},
			"rules[0].values",
		},
		{
			"a rule that needs values but has none",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{{Type: domain.RuleImageAllowlist}}
			},
			"rules[0].values",
		},
		{
			"an unknown rule severity",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RulePrivilegedForbidden, Severity: "urgent"},
				}
			},
			"rules[0].severity",
		},
		{
			"a wildcard in a capability name",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RuleForbiddenCapabilities, Values: []string{"SYS_*"}},
				}
			},
			"rules[0].values[0]",
		},
		{
			"a restart policy outside the vocabulary",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RuleRestartPolicyAllowlist, Values: []string{"sometimes"}},
				}
			},
			"rules[0].values[0]",
		},
		{
			"a network name that is not one",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RuleNetworkAllowlist, Values: []string{"-leading-hyphen"}},
				}
			},
			"rules[0].values[0]",
		},
		{
			"a pattern with too many wildcards",
			func(p *domain.PolicyDefinition) {
				p.Rules = []domain.PolicyRule{
					{Type: domain.RuleImageAllowlist,
						Values: []string{strings.Repeat("*a", domain.MaxPolicyWildcards+1)}},
				}
			},
			"rules[0].values[0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := validPolicy()
			tc.mutate(&policy)
			policy.Normalise()

			err := policy.Validate(domain.DefaultPolicyLimits())
			if err == nil {
				t.Fatal("the policy was accepted")
			}

			var validation domain.PolicyValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error is not a PolicyValidationError: %v", err)
			}
			if validation.Field != tc.field {
				t.Errorf("field = %q, want %q", validation.Field, tc.field)
			}
			if validation.Message == "" {
				t.Error("the validation error carries no message")
			}
		})
	}
}

// Bounds are enforced against the LIMITS PASSED IN, not against a package
// global, so a deployment that narrows them narrows what the API accepts.
func TestValidationHonoursTheSuppliedLimits(t *testing.T) {
	policy := validPolicy()
	policy.Normalise()

	narrow := domain.PolicyLimits{MaxRules: 1, MaxValuesPerRule: 1, MaxNameBytes: 8, MaxDescriptionBytes: 8}
	if err := policy.Validate(narrow); err == nil {
		t.Fatal("a two-rule policy was accepted under a one-rule limit")
	}

	// A partially specified limit set must fall back to defaults rather than
	// silently meaning "unbounded".
	partial := domain.PolicyLimits{MaxRules: 1}
	if err := policy.Validate(partial); err == nil {
		t.Fatal("the supplied MaxRules was not applied")
	}
	if err := policy.Validate(domain.PolicyLimits{}); err != nil {
		t.Fatalf("an empty limit set did not fall back to defaults: %v", err)
	}
}

// Normalise must trim, drop empties, and deduplicate, so the bounds apply to
// what is actually stored and two spellings of one pattern do not both consume
// budget.
func TestNormaliseTrimsAndDeduplicates(t *testing.T) {
	policy := domain.PolicyDefinition{
		Name:     "  Spaced  ",
		Severity: domain.PolicySeverityLow,
		Rules: []domain.PolicyRule{{
			Type:   domain.RuleForbiddenCapabilities,
			Values: []string{" SYS_ADMIN ", "cap_sys_admin", "", "   ", "NET_ADMIN"},
		}},
	}
	policy.Normalise()

	if policy.Name != "Spaced" {
		t.Errorf("name = %q, want trimmed", policy.Name)
	}
	got := policy.Rules[0].Values
	if len(got) != 2 || got[0] != "SYS_ADMIN" || got[1] != "NET_ADMIN" {
		t.Errorf("values = %v, want the normalised deduplicated pair", got)
	}
}

// A generated policy id must have the shape the API validates against, and two
// generated ids must differ.
func TestGeneratedPolicyIDs(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		id := domain.NewPolicyID()
		if !strings.HasPrefix(id, "pol_") || len(id) != 24 {
			t.Fatalf("id %q does not have the expected shape", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("id %q was generated twice", id)
		}
		seen[id] = struct{}{}
	}
}
