package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Classification tests.
//
// The ranking is security POLICY, so these are the tests that say what
// HarborMaster considers dangerous. Each case names the movement rather than
// the field, because direction is what the ranking turns on.

// The headline property: losing a containment boundary is critical, and
// regaining one is not.
func TestSecurityDirectionDecidesSeverity(t *testing.T) {
	tests := map[string]struct {
		field    string
		kind     domain.ChangeKind
		old, new string
		want     domain.DriftSeverity
	}{
		"privileged turned on":  {"privileged", domain.ChangeModified, "false", "true", domain.DriftSeverityCritical},
		"privileged turned off": {"privileged", domain.ChangeModified, "true", "false", domain.DriftSeverityLow},

		"read-only rootfs disabled": {"readonlyRootfs", domain.ChangeModified, "true", "false", domain.DriftSeverityCritical},
		"read-only rootfs enabled":  {"readonlyRootfs", domain.ChangeModified, "false", "true", domain.DriftSeverityLow},

		"no-new-privileges disabled": {"noNewPrivileges", domain.ChangeModified, "true", "false", domain.DriftSeverityCritical},
		"no-new-privileges enabled":  {"noNewPrivileges", domain.ChangeModified, "false", "true", domain.DriftSeverityLow},

		"capability added":   {"capAdd", domain.ChangeModified, "NET_ADMIN", "NET_ADMIN,SYS_ADMIN", domain.DriftSeverityCritical},
		"capability removed": {"capAdd", domain.ChangeModified, "NET_ADMIN,SYS_ADMIN", "NET_ADMIN", domain.DriftSeverityLow},

		"dropped capability restored": {"capDrop", domain.ChangeModified, "ALL,NET_RAW", "ALL", domain.DriftSeverityCritical},
		"more capabilities dropped":   {"capDrop", domain.ChangeModified, "ALL", "ALL,NET_RAW", domain.DriftSeverityLow},

		"security option removed": {"securityOpt", domain.ChangeModified, "seccomp=x,apparmor=y", "seccomp=x", domain.DriftSeverityCritical},
		"security option gone":    {"securityOpt", domain.ChangeRemoved, "seccomp=x", "", domain.DriftSeverityCritical},

		"seccomp profile removed": {"seccompProfile", domain.ChangeRemoved, "runtime/default", "", domain.DriftSeverityCritical},
		"apparmor profile blank":  {"apparmorProfile", domain.ChangeModified, "docker-default", "", domain.DriftSeverityCritical},

		"pid namespace shared with host": {"pidMode", domain.ChangeModified, "", "host", domain.DriftSeverityCritical},
		"host device exposed":            {"device./dev/sda", domain.ChangeAdded, "", "/dev/sda:rwm", domain.DriftSeverityCritical},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := domain.ClassifyDrift(domain.DiffGroupSecurity, tc.field, tc.kind, tc.old, tc.new)
			if got.Severity != tc.want {
				t.Errorf("severity = %q, want %q\n  reason: %s", got.Severity, tc.want, got.Reason)
			}
			if got.Category != domain.DriftCategorySecurity {
				t.Errorf("category = %q, want security", got.Category)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("every classification must carry a reason; an unexplained severity is not actionable")
			}
		})
	}
}

// The digest outranks the reference: a tag can be repointed without anything
// being rebuilt, but a digest change means different bytes are running.
func TestImageDigestOutranksImageReference(t *testing.T) {
	digest := domain.ClassifyDrift(domain.DiffGroupMetadata, "image.digest",
		domain.ChangeModified, "sha256:aaa", "sha256:bbb")
	reference := domain.ClassifyDrift(domain.DiffGroupMetadata, "image.reference",
		domain.ChangeModified, "nginx:1.27", "nginx:1.28")

	if digest.Severity != domain.DriftSeverityCritical {
		t.Errorf("image.digest = %q, want critical", digest.Severity)
	}
	if reference.Severity != domain.DriftSeverityHigh {
		t.Errorf("image.reference = %q, want high", reference.Severity)
	}
	if digest.Severity.Rank() <= reference.Severity.Rank() {
		t.Error("a digest change must outrank a reference change")
	}
	if digest.Category != domain.DriftCategoryImage || reference.Category != domain.DriftCategoryImage {
		t.Error("both belong to the image category")
	}
}

func TestMountExposureRanking(t *testing.T) {
	tests := map[string]struct {
		kind     domain.ChangeKind
		old, new string
		want     domain.DriftSeverity
	}{
		"bind mount added":       {domain.ChangeAdded, "", "bind source=/etc", domain.DriftSeverityHigh},
		"named volume added":     {domain.ChangeAdded, "", "volume volume=data", domain.DriftSeverityMedium},
		"mount lost read-only":   {domain.ChangeModified, "bind source=/etc ro", "bind source=/etc", domain.DriftSeverityHigh},
		"mount gained read-only": {domain.ChangeModified, "bind source=/etc", "bind source=/etc ro", domain.DriftSeverityMedium},
		"mount removed":          {domain.ChangeRemoved, "bind source=/etc", "", domain.DriftSeverityMedium},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := domain.ClassifyDrift(domain.DiffGroupMounts, "/etc", tc.kind, tc.old, tc.new)
			if got.Severity != tc.want {
				t.Errorf("severity = %q, want %q\n  reason: %s", got.Severity, tc.want, got.Reason)
			}
			if got.Category != domain.DriftCategoryMounts {
				t.Errorf("category = %q, want mounts", got.Category)
			}
		})
	}
}

// Published is what matters: an exposed port is documentation, a published one
// is reachable.
func TestPortPublicationRanking(t *testing.T) {
	published := domain.ClassifyDrift(domain.DiffGroupPorts, "80/tcp",
		domain.ChangeAdded, "", "published 0.0.0.0:8080")
	exposed := domain.ClassifyDrift(domain.DiffGroupPorts, "80/tcp",
		domain.ChangeAdded, "", "exposed")
	promoted := domain.ClassifyDrift(domain.DiffGroupPorts, "80/tcp",
		domain.ChangeModified, "exposed", "published 0.0.0.0:8080")

	if published.Severity != domain.DriftSeverityHigh {
		t.Errorf("a published port added = %q, want high", published.Severity)
	}
	if exposed.Severity != domain.DriftSeverityMedium {
		t.Errorf("an exposed port added = %q, want medium", exposed.Severity)
	}
	if promoted.Severity != domain.DriftSeverityHigh {
		t.Errorf("exposed becoming published = %q, want high", promoted.Severity)
	}
}

// Removing the check outranks changing its timing: a container with no health
// check reports healthy forever.
func TestHealthCheckRemovalOutranksTiming(t *testing.T) {
	removed := domain.ClassifyDrift(domain.DiffGroupMetadata, "healthCheck.test",
		domain.ChangeRemoved, "CMD curl -f http://localhost", "")
	interval := domain.ClassifyDrift(domain.DiffGroupMetadata, "healthCheck.intervalMs",
		domain.ChangeModified, "30000", "60000")

	if removed.Severity != domain.DriftSeverityHigh {
		t.Errorf("health check removed = %q, want high", removed.Severity)
	}
	if interval.Severity != domain.DriftSeverityLow {
		t.Errorf("health interval changed = %q, want low", interval.Severity)
	}
	if removed.Category != domain.DriftCategoryHealth || interval.Category != domain.DriftCategoryHealth {
		t.Error("both belong to the health category")
	}
}

func TestResourceLimitRanking(t *testing.T) {
	memory := domain.ClassifyDrift(domain.DiffGroupResources, "memoryBytes",
		domain.ChangeRemoved, "536870912", "")
	cpu := domain.ClassifyDrift(domain.DiffGroupResources, "nanoCpus",
		domain.ChangeModified, "1000000000", "2000000000")

	if memory.Severity != domain.DriftSeverityMedium {
		t.Errorf("memory limit removed = %q, want medium", memory.Severity)
	}
	if cpu.Severity != domain.DriftSeverityLow {
		t.Errorf("cpu limit changed = %q, want low", cpu.Severity)
	}
	if memory.Severity.Rank() <= cpu.Severity.Rank() {
		t.Error("a removed memory limit must outrank a CPU limit change")
	}
}

// The low-severity bookkeeping categories, asserted together so a future
// change that promotes one of them has to come past this test.
func TestBookkeepingCategoriesRankLow(t *testing.T) {
	tests := map[string]struct {
		group domain.DiffGroupName
		field string
	}{
		"labels":  {domain.DiffGroupLabels, "com.example.owner"},
		"compose": {domain.DiffGroupCompose, "project"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := domain.ClassifyDrift(tc.group, tc.field, domain.ChangeModified, "a", "b")
			if got.Severity != domain.DriftSeverityLow {
				t.Errorf("severity = %q, want low", got.Severity)
			}
		})
	}
}

func TestEnvironmentAndNetworkRankMedium(t *testing.T) {
	env := domain.ClassifyDrift(domain.DiffGroupEnvironment, "LOG_LEVEL",
		domain.ChangeModified, "info", "debug")
	network := domain.ClassifyDrift(domain.DiffGroupNetworks, "backend",
		domain.ChangeAdded, "", "aliases=")
	restart := domain.ClassifyDrift(domain.DiffGroupMetadata, "restartPolicy",
		domain.ChangeModified, "always", "no")

	for name, got := range map[string]domain.DriftClassification{
		"environment": env, "network": network, "restart": restart,
	} {
		if got.Severity != domain.DriftSeverityMedium {
			t.Errorf("%s = %q, want medium", name, got.Severity)
		}
	}
	if restart.Category != domain.DriftCategoryRestart {
		t.Errorf("restart category = %q, want restart", restart.Category)
	}
}

// An unverifiable comparison is not "no change". It is reported so an operator
// knows the field was not checked.
func TestUnverifiableIsReportedRatherThanTreatedAsUnchanged(t *testing.T) {
	got := domain.ClassifyDrift(domain.DiffGroupEnvironment, "DB_PASSWORD",
		domain.ChangeUnverifiable, "", "")

	if got.Severity != domain.DriftSeverityMedium {
		t.Errorf("severity = %q, want medium", got.Severity)
	}
	if !strings.Contains(got.Reason, "could not be compared") {
		t.Errorf("reason = %q, want it to say the comparison failed", got.Reason)
	}
}

// The metadata group is split into categories an operator filters on
// separately, which is the reason drift has its own vocabulary.
func TestMetadataGroupSplitsIntoUsefulCategories(t *testing.T) {
	tests := map[string]domain.DriftCategory{
		"image.digest":           domain.DriftCategoryImage,
		"process.command":        domain.DriftCategoryProcess,
		"healthCheck.retries":    domain.DriftCategoryHealth,
		"logging.driver":         domain.DriftCategoryLogging,
		"restartPolicy":          domain.DriftCategoryRestart,
		"restartPolicy.maxRetry": domain.DriftCategoryRestart,
		"containerName":          domain.DriftCategoryMetadata,
	}

	for field, want := range tests {
		t.Run(field, func(t *testing.T) {
			got := domain.ClassifyDrift(domain.DiffGroupMetadata, field,
				domain.ChangeModified, "a", "b")
			if got.Category != want {
				t.Errorf("category = %q, want %q", got.Category, want)
			}
		})
	}
}

// Classification must be TOTAL: every group and kind produces a valid verdict.
// A field the classifier has no rule for must still rank, or a new diff key
// would silently produce an invalid record that the CHECK constraint rejects
// at write time.
func TestClassificationIsTotal(t *testing.T) {
	kinds := []domain.ChangeKind{
		domain.ChangeAdded, domain.ChangeRemoved,
		domain.ChangeModified, domain.ChangeUnverifiable,
	}

	for _, group := range domain.DiffGroupNames {
		for _, kind := range kinds {
			for _, field := range []string{"", "unknown", "a.b.c", "sysctl.net.ipv4.ip_forward"} {
				got := domain.ClassifyDrift(group, field, kind, "old", "new")

				if !domain.ValidDriftCategory(string(got.Category)) {
					t.Errorf("group %q field %q kind %q produced invalid category %q",
						group, field, kind, got.Category)
				}
				if !domain.ValidDriftSeverity(string(got.Severity)) {
					t.Errorf("group %q field %q kind %q produced invalid severity %q",
						group, field, kind, got.Severity)
				}
				if strings.TrimSpace(got.Reason) == "" {
					t.Errorf("group %q field %q kind %q produced no reason", group, field, kind)
				}
			}
		}
	}
}

// Classification must be DETERMINISTIC: the same input always ranks the same.
// Two operators looking at the same drift must see the same severity.
func TestClassificationIsDeterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		got := domain.ClassifyDrift(domain.DiffGroupSecurity, "privileged",
			domain.ChangeModified, "false", "true")
		if got.Severity != domain.DriftSeverityCritical {
			t.Fatalf("iteration %d ranked differently: %q", i, got.Severity)
		}
	}
}

// A reason is printed in a UI and written to a log, so it must never carry a
// value. The classifier is given values that would be obvious if echoed.
func TestReasonsNeverEchoValues(t *testing.T) {
	const marker = "SUPER-SECRET-VALUE-9182"

	for _, group := range domain.DiffGroupNames {
		for _, kind := range []domain.ChangeKind{
			domain.ChangeAdded, domain.ChangeRemoved, domain.ChangeModified,
		} {
			got := domain.ClassifyDrift(group, marker, kind, marker, marker)
			if strings.Contains(got.Reason, marker) {
				t.Errorf("group %q kind %q echoed the value into its reason: %q",
					group, kind, got.Reason)
			}
		}
	}

	// The positive control: the sweep can find the marker when it is present.
	if !strings.Contains("prefix "+marker, marker) {
		t.Fatal("the sweep cannot detect the value it looks for")
	}
}

func TestSeverityRankOrdersCorrectly(t *testing.T) {
	ordered := []domain.DriftSeverity{
		domain.DriftSeverityCritical, domain.DriftSeverityHigh,
		domain.DriftSeverityMedium, domain.DriftSeverityLow,
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Rank() <= ordered[i].Rank() {
			t.Errorf("%q must rank above %q", ordered[i-1], ordered[i])
		}
	}
	if domain.DriftSeverity("bogus").Rank() != 0 {
		t.Error("an unknown severity must rank zero rather than sorting among the real ones")
	}
}

// The operator/engine status split is the model's central rule, so it is
// pinned rather than left to the handler.
func TestOperatorCannotBeGivenEngineOwnedStatuses(t *testing.T) {
	for _, status := range []domain.DriftStatus{domain.DriftStatusActive, domain.DriftStatusResolved} {
		if domain.ValidOperatorDriftStatus(string(status)) {
			t.Errorf("%q is engine-owned and must not be settable by an API caller", status)
		}
		if !domain.ValidDriftStatus(string(status)) {
			t.Errorf("%q must still be a valid status", status)
		}
	}

	for _, status := range []domain.DriftStatus{
		domain.DriftStatusAcknowledged, domain.DriftStatusIgnored, domain.DriftStatusExpected,
	} {
		if !domain.ValidOperatorDriftStatus(string(status)) {
			t.Errorf("%q must be settable by an operator", status)
		}
	}

	if domain.ValidOperatorDriftStatus("deleted") {
		t.Error("an unknown status must not be accepted")
	}
}

func TestOpenReportsWhetherTheDifferenceStands(t *testing.T) {
	if domain.DriftStatusResolved.Open() {
		t.Error("resolved is not open")
	}
	for _, status := range []domain.DriftStatus{
		domain.DriftStatusActive, domain.DriftStatusAcknowledged,
		domain.DriftStatusIgnored, domain.DriftStatusExpected,
	} {
		if !status.Open() {
			t.Errorf("%q still describes a difference that stands, so it is open", status)
		}
	}
}
