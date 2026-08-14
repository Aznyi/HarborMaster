package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The rebind exception is exactly ONE approved change.
//
// # What these guard
//
// Blocker 2 is fixed by rewriting the EXPECTED configuration to name the
// approved provider before the comparison, rather than by excluding the
// namespace modes from comparison. The difference matters only if the
// comparison still catches everything else, so this file is the proof that it
// does.
//
// Each case starts from a dependent sharing a provider's namespace, applies the
// approved rebind to the expectation, and then changes ONE other thing about
// the replacement. Every one of them must be reported.

const (
	rebindDependentID = "7777777777777777777777777777777777777777777777777777777777777777"
	rebindProviderOld = "8888888888888888888888888888888888888888888888888888888888888888"
	rebindProviderNew = "9999999999999999999999999999999999999999999999999999999999999999"
	rebindProviderOdd = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// dependentDetail is a realistic dependent: it shares one namespace and carries
// enough other configuration for the negative cases to have something to break.
func dependentDetail(kind, providerID string) domain.ContainerDetail {
	detail := domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:            rebindDependentID,
			ShortID:       rebindDependentID[:12],
			Name:          "dependent",
			Image:         domain.ImageRef{Raw: "alpine:3.24.0"},
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
		},
		Process: domain.Process{Command: []string{"sleep", "infinity"}},
		Environment: []domain.EnvVar{
			{Name: "MODE", Value: "production", RawValue: "production"},
		},
		Labels: []domain.Label{{Key: "com.example.team", Value: "platform"}},
		Mounts: []domain.Mount{{
			Type: domain.MountTypeVolume, Source: "data", Destination: "/var/lib/data", VolumeName: "data",
		}},
		Resources: domain.Resources{MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000},
		HealthCheck: &domain.HealthCheck{
			Test:          []string{"CMD-SHELL", "true"},
			IntervalMS:    30_000,
			TimeoutMS:     5_000,
			Retries:       3,
			StartPeriodMS: 10_000,
		},
	}

	mode := "container:" + providerID
	switch kind {
	case "network":
		detail.Security.NetworkMode = mode
	case "ipc":
		detail.Security.IPCMode = mode
		detail.Security.NetworkMode = "bridge"
	case "pid":
		detail.Security.PIDMode = mode
		detail.Security.NetworkMode = "bridge"
	}
	return detail
}

// rebound applies the approved provider change to a detail, exactly as the
// capture's own rewrite does before the expectation is built.
func rebound(detail domain.ContainerDetail, kind, newProviderID string) domain.ContainerDetail {
	mode := "container:" + newProviderID
	switch kind {
	case "network":
		detail.Security.NetworkMode = mode
	case "ipc":
		detail.Security.IPCMode = mode
	case "pid":
		detail.Security.PIDMode = mode
	}
	return detail
}

var rebindKinds = []string{"network", "ipc", "pid"}

// A. The intended old -> new provider change passes.
func TestTheApprovedProviderChangePasses(t *testing.T) {
	t.Parallel()

	for _, kind := range rebindKinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			expected := domain.BuildPreservationSummary(
				rebound(dependentDetail(kind, rebindProviderOld), kind, rebindProviderNew), nil)
			actual := domain.BuildPreservationSummary(
				dependentDetail(kind, rebindProviderNew), nil)

			report := domain.ComparePreservation(expected, actual)
			if report.Status != domain.VerificationPassed {
				t.Fatalf("the approved rebind was reported as drift: %+v", report.Differences)
			}
		})
	}
}

// B. The WRONG provider id fails.
//
// The whole safety property. If a replacement came back attached to some other
// container, the expectation rewritten to the approved provider must not match.
func TestTheWrongProviderFails(t *testing.T) {
	t.Parallel()

	for _, kind := range rebindKinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			expected := domain.BuildPreservationSummary(
				rebound(dependentDetail(kind, rebindProviderOld), kind, rebindProviderNew), nil)
			// The replacement joined a DIFFERENT container's namespace.
			actual := domain.BuildPreservationSummary(
				dependentDetail(kind, rebindProviderOdd), nil)

			report := domain.ComparePreservation(expected, actual)
			if report.Status == domain.VerificationPassed {
				t.Fatalf("a replacement attached to the wrong provider passed "+
					"preservation for the %s namespace", kind)
			}
		})
	}
}

// C. Dropping the share entirely fails.
//
// A replacement that came back on `bridge`, `host`, `none` or a private
// namespace instead of the provider's is a container that lost the thing it
// existed to share.
func TestLosingTheSharedNamespaceFails(t *testing.T) {
	t.Parallel()

	replacements := map[string][]string{
		"network": {"bridge", "host", "none"},
		"ipc":     {"private", "shareable", "host"},
		"pid":     {"", "host"},
	}

	for _, kind := range rebindKinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			expected := domain.BuildPreservationSummary(
				rebound(dependentDetail(kind, rebindProviderOld), kind, rebindProviderNew), nil)

			for _, mode := range replacements[kind] {
				detail := dependentDetail(kind, rebindProviderNew)
				switch kind {
				case "network":
					detail.Security.NetworkMode = mode
				case "ipc":
					detail.Security.IPCMode = mode
				case "pid":
					detail.Security.PIDMode = mode
				}
				actual := domain.BuildPreservationSummary(detail, nil)

				if domain.ComparePreservation(expected, actual).Status == domain.VerificationPassed {
					t.Fatalf("a replacement that dropped the %s share to %q passed "+
						"preservation", kind, mode)
				}
			}
		})
	}
}

// D. A SECOND, unapproved namespace field moving fails.
//
// The rebind approves one namespace reference. A replacement that also changed
// a different namespace has drifted, and the approval for the first must not
// cover the second.
func TestASecondUnapprovedNamespaceChangeFails(t *testing.T) {
	t.Parallel()

	expected := domain.BuildPreservationSummary(
		rebound(dependentDetail("network", rebindProviderOld), "network", rebindProviderNew), nil)

	// The network share moved as approved -- and the IPC namespace moved too.
	drifted := dependentDetail("network", rebindProviderNew)
	drifted.Security.IPCMode = "container:" + rebindProviderOdd
	actual := domain.BuildPreservationSummary(drifted, nil)

	report := domain.ComparePreservation(expected, actual)
	if report.Status == domain.VerificationPassed {
		t.Fatal("a replacement that also changed its IPC namespace passed " +
			"preservation; the approval covers one reference, not a licence to drift")
	}
	if len(report.Differences) != 1 || report.Differences[0].Field != "security.ipcMode" {
		t.Fatalf("differences = %+v, want exactly [security.ipcMode]", report.Differences)
	}
}

// E through I: everything else must still be compared during a rebind.
//
// A single table, because the assertion is identical and the point is coverage
// rather than per-case nuance: the rebind exception must not have widened into
// "a dependency recreation may differ".
func TestEveryOtherFieldIsStillComparedDuringARebind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		field  string
		break_ func(*domain.ContainerDetail)
	}{
		{
			name: "an operator label is lost", field: "labels",
			break_: func(d *domain.ContainerDetail) { d.Labels = nil },
		},
		{
			name: "a mount changes", field: "mounts",
			break_: func(d *domain.ContainerDetail) {
				d.Mounts[0].Destination = "/somewhere/else"
			},
		},
		{
			name: "an environment variable changes", field: "environment",
			break_: func(d *domain.ContainerDetail) {
				d.Environment[0].Value = "staging"
				d.Environment[0].RawValue = "staging"
			},
		},
		{
			name: "a memory limit changes", field: "resources.memoryBytes",
			break_: func(d *domain.ContainerDetail) {
				d.Resources.MemoryBytes = 256 << 20
			},
		},
		{
			name: "a cpu limit changes", field: "resources.nanoCpus",
			break_: func(d *domain.ContainerDetail) {
				d.Resources.NanoCPUs = 500_000_000
			},
		},
		{
			name: "the health check changes", field: "healthCheck",
			break_: func(d *domain.ContainerDetail) {
				d.HealthCheck.Retries = 9
			},
		},
		{
			name: "the restart policy changes", field: "restartPolicy",
			break_: func(d *domain.ContainerDetail) {
				d.Overview.RestartPolicy = domain.RestartPolicy{Name: "no"}
			},
		},
		{
			name: "the command changes", field: "process.command",
			break_: func(d *domain.ContainerDetail) {
				d.Process.Command = []string{"sleep", "5"}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// The approved rebind, applied to the expectation.
			expected := domain.BuildPreservationSummary(
				rebound(dependentDetail("ipc", rebindProviderOld), "ipc", rebindProviderNew), nil)

			// The replacement reattached correctly AND broke something else.
			drifted := dependentDetail("ipc", rebindProviderNew)
			testCase.break_(&drifted)
			actual := domain.BuildPreservationSummary(drifted, nil)

			report := domain.ComparePreservation(expected, actual)
			if report.Status == domain.VerificationPassed {
				t.Fatalf("%s went unnoticed during a rebind; the namespace approval "+
					"must not weaken any other comparison", testCase.name)
			}

			var fields []string
			for _, difference := range report.Differences {
				fields = append(fields, difference.Field)
			}
			found := false
			for _, field := range fields {
				if field == testCase.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("differences = %v, want one naming %q", fields, testCase.field)
			}
		})
	}
}
