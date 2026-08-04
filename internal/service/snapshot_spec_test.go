package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The distinctive value used to prove no secret reaches the document.
const specSecretValue = "s3cr3t-value-do-not-persist"

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt(i int) *int              { return &i }

// fixtureDetail builds a fully populated container with at least two entries in
// every collection, so ordering bugs have somewhere to show up.
func fixtureDetail() domain.ContainerDetail {
	created := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			HostID:  domain.LocalHostID,
			ID:      "c0ffee0000000000",
			ShortID: "c0ffee000000",
			Name:    "web",
			Image: domain.ImageRef{
				Raw: "nginx:1.27", Repository: "nginx", Tag: "1.27",
			},
			ImageID:       "sha256:aaaa",
			State:         domain.StateRunning,
			Status:        "Up 2 minutes",
			Health:        domain.HealthHealthy,
			CreatedAt:     created,
			StartedAt:     ptrTime(created.Add(time.Minute)),
			RestartCount:  3,
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
			Generation:    42,
			LastSeenAt:    created.Add(time.Hour),
			FirstSeenAt:   created,
			Present:       true,
		},
		State: domain.StateDetail{
			State: domain.StateRunning, Running: true,
			StartedAt: ptrTime(created.Add(time.Minute)),
		},
		Process: domain.Process{
			Hostname: "web-1", Domainname: "example.internal",
			Entrypoint: []string{"/docker-entrypoint.sh"},
			Command:    []string{"nginx", "-g", "daemon off;"},
			User:       "101:101", WorkingDir: "/usr/share/nginx",
			StopSignal: "SIGQUIT", StopTimeoutSeconds: ptrInt(10),
		},
		HealthCheck: &domain.HealthCheck{
			Test:       []string{"CMD", "curl", "-f", "http://localhost/"},
			IntervalMS: 30000, TimeoutMS: 5000, Retries: 3,
		},
		Environment: []domain.EnvVar{
			{Name: "PATH", Value: "/usr/bin", Sensitivity: domain.SensitivityNormal, RawValue: "/usr/bin"},
			{Name: "NGINX_PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
			{
				Name: "DB_PASSWORD", Value: domain.MaskedValue,
				Sensitivity: domain.SensitivitySensitive, RawValue: specSecretValue,
			},
		},
		Labels: []domain.Label{
			{Key: "com.docker.compose.project", Value: "shop", Source: domain.LabelSourceCompose},
			{Key: "app", Value: "web", Source: domain.LabelSourceUser},
			{Key: "tier", Value: "frontend", Source: domain.LabelSourceUser},
		},
		Ports: []domain.Port{
			{ContainerPort: 443, Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8443, Published: true},
			{ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 8080, Published: true},
		},
		Mounts: []domain.Mount{
			{Type: domain.MountTypeVolume, VolumeName: "web-data", Destination: "/data", Driver: "local"},
			{Type: domain.MountTypeBind, Source: "/srv/conf", Destination: "/etc/nginx/conf.d", ReadOnly: true},
		},
		Networks: []domain.NetworkAttachment{
			{
				NetworkName: "shop_default", NetworkID: "net-b",
				Aliases: []string{"web", "frontend"}, IPv4Address: "172.18.0.3",
				Gateway: "172.18.0.1", MACAddress: "02:42:ac:12:00:03", EndpointID: "ep-b",
			},
			{
				NetworkName: "edge", NetworkID: "net-a",
				Aliases: []string{"proxy"}, IPv4Address: "172.19.0.2", EndpointID: "ep-a",
			},
		},
		Resources: domain.Resources{
			MemoryBytes: 536870912, NanoCPUs: 1500000000,
			Ulimits: []domain.Ulimit{
				{Name: "nproc", Soft: 1024, Hard: 2048},
				{Name: "nofile", Soft: 1024, Hard: 4096},
			},
		},
		Security: domain.Security{
			ReadonlyRootfs: true, NoNewPrivileges: true,
			CapDrop:  []string{"NET_RAW", "ALL"},
			CapAdd:   []string{"NET_BIND_SERVICE"},
			GroupAdd: []string{"web", "app"},
			Sysctls:  map[string]string{"net.ipv4.ip_forward": "0", "net.core.somaxconn": "1024"},
		},
		Logging: domain.Logging{
			Driver: "json-file",
			Options: []domain.EnvVar{
				{Name: "max-size", Value: "10m", Sensitivity: domain.SensitivityNormal, RawValue: "10m"},
				{
					Name: "splunk-token", Value: domain.MaskedValue,
					Sensitivity: domain.SensitivitySensitive, RawValue: "tok_" + specSecretValue,
				},
			},
		},
		Compose:  domain.ComposeMetadata{Managed: true, Project: "shop", Service: "web", ContainerNumber: 1},
		Warnings: []domain.InventoryWarning{},
	}
}

func buildTestSpec(t *testing.T, detail domain.ContainerDetail) domain.SnapshotSpec {
	t.Helper()
	return BuildSpec(detail, nil, newTestHasher(t))
}

func marshalTestSpec(t *testing.T, spec domain.SnapshotSpec) string {
	t.Helper()
	blob, _, err := MarshalSpec(spec)
	if err != nil {
		t.Fatalf("MarshalSpec: %v", err)
	}
	return string(blob)
}

func checksumTestSpec(t *testing.T, spec domain.SnapshotSpec) string {
	t.Helper()
	_, checksum, err := MarshalSpec(spec)
	if err != nil {
		t.Fatalf("MarshalSpec: %v", err)
	}
	return checksum
}

func TestSpecIsDeterministicAcrossRepeatedBuilds(t *testing.T) {
	first := marshalTestSpec(t, buildTestSpec(t, fixtureDetail()))
	firstSum := checksumTestSpec(t, buildTestSpec(t, fixtureDetail()))

	// Repeated because Go randomises map iteration order per range statement:
	// a map-ordering bug shows up probabilistically, not on the first try.
	for i := 0; i < 50; i++ {
		spec := buildTestSpec(t, fixtureDetail())
		if got := marshalTestSpec(t, spec); got != first {
			t.Fatalf("document is not deterministic on iteration %d", i)
		}
		if got := checksumTestSpec(t, spec); got != firstSum {
			t.Fatalf("checksum is not deterministic on iteration %d", i)
		}
	}
}

func TestSpecIgnoresOrderingOfSetLikeFields(t *testing.T) {
	a := fixtureDetail()

	b := fixtureDetail()
	reverseSlice(b.Mounts)
	reverseSlice(b.Ports)
	reverseSlice(b.Networks)
	reverseSlice(b.Labels)
	reverseSlice(b.Security.CapDrop)
	reverseSlice(b.Security.GroupAdd)
	reverseSlice(b.Resources.Ulimits)
	reverseSlice(b.Networks[0].Aliases)

	if checksumTestSpec(t, buildTestSpec(t, a)) != checksumTestSpec(t, buildTestSpec(t, b)) {
		t.Error("reordering set-like fields changed the checksum; ordering noise must be normalised")
	}
}

// Environment order IS meaningful to some programs, so a reordering is a real
// configuration change and must be visible as one.
func TestSpecPreservesEnvironmentOrder(t *testing.T) {
	a := fixtureDetail()
	b := fixtureDetail()
	b.Environment[0], b.Environment[1] = b.Environment[1], b.Environment[0]

	if checksumTestSpec(t, buildTestSpec(t, a)) == checksumTestSpec(t, buildTestSpec(t, b)) {
		t.Error("reordering the environment must change the checksum")
	}
}

func TestSpecExcludesVolatileFields(t *testing.T) {
	a := fixtureDetail()

	b := fixtureDetail()
	b.Overview.Status = "Up 4 hours"
	b.Overview.RestartCount = 99
	b.Overview.Generation = 12345
	b.Overview.LastSeenAt = b.Overview.LastSeenAt.Add(time.Hour)
	b.Overview.StartedAt = ptrTime(time.Now())
	b.State.StartedAt = ptrTime(time.Now())
	b.State.Status = "different"
	b.Networks[0].IPv4Address = "10.9.9.9"
	b.Networks[0].Gateway = "10.9.9.1"
	b.Networks[0].MACAddress = "02:42:ff:ff:ff:ff"
	b.Networks[0].EndpointID = "totally-different"
	b.Networks[0].NetworkID = "changed"

	if checksumTestSpec(t, buildTestSpec(t, a)) != checksumTestSpec(t, buildTestSpec(t, b)) {
		t.Error("a volatile field changed the checksum; volatile fields must not be captured")
	}
}

// The single most important test in the package.
func TestSpecNeverContainsSensitiveValues(t *testing.T) {
	spec := buildTestSpec(t, fixtureDetail())
	blob := marshalTestSpec(t, spec)

	for _, needle := range []string{specSecretValue, "tok_" + specSecretValue} {
		if strings.Contains(blob, needle) {
			t.Fatalf("canonical document contains a plaintext secret: %q", needle)
		}
	}

	// The NAME survives, because knowing a variable exists is the point.
	if !strings.Contains(blob, "DB_PASSWORD") {
		t.Error("document lost the sensitive variable's name")
	}
	// And the digest does NOT reach the document bytes.
	var env struct {
		Environment []map[string]any `json:"environment"`
	}
	if err := json.Unmarshal([]byte(blob), &env); err != nil {
		t.Fatal(err)
	}
	for _, entry := range env.Environment {
		if _, found := entry["digest"]; found {
			t.Error("document serialised a digest field")
		}
		if entry["name"] == "DB_PASSWORD" {
			if v, ok := entry["value"]; ok && v != "" {
				t.Errorf("sensitive variable carried a value: %v", v)
			}
		}
	}
}

// Changing a secret must change the checksum, or drift detection is blind to
// exactly the change that matters most.
func TestChecksumChangesWhenASecretChanges(t *testing.T) {
	a := fixtureDetail()
	b := fixtureDetail()
	b.Environment[2].RawValue = "a-completely-different-secret"

	if checksumTestSpec(t, buildTestSpec(t, a)) == checksumTestSpec(t, buildTestSpec(t, b)) {
		t.Error("changing a secret did not change the checksum")
	}
}

// The checksum notices a changed secret while the document bytes do not, which
// is the whole design in one assertion.
//
// The two values are the same LENGTH deliberately. Length is a captured field
// (SpecEnvVar.Length), so a length change legitimately alters the document --
// see TestSecretLengthIsObservableByDesign. Holding length constant isolates
// the property under test: the value itself contributes nothing to the bytes.
func TestSecretValueDoesNotReachDocumentBytes(t *testing.T) {
	a := fixtureDetail()
	a.Environment[2].RawValue = "AAAAAAAAAAAAAAAA"

	b := fixtureDetail()
	b.Environment[2].RawValue = "BBBBBBBBBBBBBBBB"

	if marshalTestSpec(t, buildTestSpec(t, a)) != marshalTestSpec(t, buildTestSpec(t, b)) {
		t.Error("two same-length secrets produced different documents; the value is leaking into the bytes")
	}
	if checksumTestSpec(t, buildTestSpec(t, a)) == checksumTestSpec(t, buildTestSpec(t, b)) {
		t.Error("two different secrets produced the same checksum; drift detection is blind")
	}
}

// A sensitive value's LENGTH is recorded and is therefore observable through
// the API, while the value is not.
//
// This is a deliberate trade, not an oversight: an operator preparing a restore
// needs to know a variable is set and roughly what it is, and length is the
// cheapest such signal. It does disclose the length of a password to anyone who
// can read the snapshot, which is a small but real information leak, and it is
// documented as a known limitation rather than hidden.
func TestSecretLengthIsObservableByDesign(t *testing.T) {
	detail := fixtureDetail()
	detail.Environment[2].RawValue = "short"

	spec := buildTestSpec(t, detail)
	for _, entry := range spec.Environment {
		if entry.Name != "DB_PASSWORD" {
			continue
		}
		if entry.Length != len("short") {
			t.Errorf("Length = %d, want %d", entry.Length, len("short"))
		}
		if entry.Value != "" {
			t.Errorf("sensitive entry carried a value: %q", entry.Value)
		}
		return
	}
	t.Fatal("sensitive variable not found in the document")
}

func TestChecksumChangesForEveryCapturedSection(t *testing.T) {
	base := checksumTestSpec(t, buildTestSpec(t, fixtureDetail()))

	mutations := map[string]func(*domain.ContainerDetail){
		"image":         func(d *domain.ContainerDetail) { d.Overview.Image.Raw = "nginx:1.28" },
		"imageId":       func(d *domain.ContainerDetail) { d.Overview.ImageID = "sha256:bbbb" },
		"name":          func(d *domain.ContainerDetail) { d.Overview.Name = "web-2" },
		"command":       func(d *domain.ContainerDetail) { d.Process.Command = []string{"nginx"} },
		"user":          func(d *domain.ContainerDetail) { d.Process.User = "0:0" },
		"workingDir":    func(d *domain.ContainerDetail) { d.Process.WorkingDir = "/tmp" },
		"hostname":      func(d *domain.ContainerDetail) { d.Process.Hostname = "other" },
		"envValue":      func(d *domain.ContainerDetail) { d.Environment[0].Value = "/bin" },
		"label":         func(d *domain.ContainerDetail) { d.Labels[1].Value = "api" },
		"port":          func(d *domain.ContainerDetail) { d.Ports[0].HostPort = 9443 },
		"mount":         func(d *domain.ContainerDetail) { d.Mounts[0].ReadOnly = true },
		"network":       func(d *domain.ContainerDetail) { d.Networks[0].NetworkName = "other" },
		"alias":         func(d *domain.ContainerDetail) { d.Networks[1].Aliases = []string{"gone"} },
		"restartPolicy": func(d *domain.ContainerDetail) { d.Overview.RestartPolicy.Name = "always" },
		"healthCheck":   func(d *domain.ContainerDetail) { d.HealthCheck.Retries = 9 },
		"memory":        func(d *domain.ContainerDetail) { d.Resources.MemoryBytes = 1 },
		"ulimit":        func(d *domain.ContainerDetail) { d.Resources.Ulimits[0].Hard = 9999 },
		"privileged":    func(d *domain.ContainerDetail) { d.Security.Privileged = true },
		"capAdd":        func(d *domain.ContainerDetail) { d.Security.CapAdd = []string{"SYS_ADMIN"} },
		"readonlyRoot":  func(d *domain.ContainerDetail) { d.Security.ReadonlyRootfs = false },
		"sysctl":        func(d *domain.ContainerDetail) { d.Security.Sysctls["net.ipv4.ip_forward"] = "1" },
		"logDriver":     func(d *domain.ContainerDetail) { d.Logging.Driver = "local" },
		"logOption":     func(d *domain.ContainerDetail) { d.Logging.Options[0].Value = "50m" },
		"compose":       func(d *domain.ContainerDetail) { d.Compose.Service = "api" },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			detail := fixtureDetail()
			mutate(&detail)
			if checksumTestSpec(t, buildTestSpec(t, detail)) == base {
				t.Errorf("changing %s did not change the checksum", name)
			}
		})
	}
}

func TestVerifyChecksumDetectsTampering(t *testing.T) {
	spec := buildTestSpec(t, fixtureDetail())
	blob, checksum, err := MarshalSpec(spec)
	if err != nil {
		t.Fatal(err)
	}

	if !VerifyChecksum(blob, spec, checksum) {
		t.Fatal("VerifyChecksum rejected an untampered document")
	}

	tampered := strings.Replace(string(blob), `"web"`, `"hacked"`, 1)
	if VerifyChecksum([]byte(tampered), spec, checksum) {
		t.Error("VerifyChecksum accepted a tampered document")
	}
}

func TestSpecVersionIsRecorded(t *testing.T) {
	if got := buildTestSpec(t, fixtureDetail()).SpecVersion; got != domain.SnapshotSpecVersion {
		t.Errorf("SpecVersion = %d, want %d", got, domain.SnapshotSpecVersion)
	}
}

func TestSpecCarriesImageMetadataWhenAvailable(t *testing.T) {
	image := &domain.Image{
		ID:           "sha256:aaaa",
		RepoDigests:  []string{"nginx@sha256:zzz", "nginx@sha256:aaa"},
		Architecture: "amd64", OS: "linux",
	}
	spec := BuildSpec(fixtureDetail(), image, newTestHasher(t))

	if spec.Image.Architecture != "amd64" || spec.Image.OS != "linux" {
		t.Errorf("image metadata not captured: %+v", spec.Image)
	}
	// Sorted, so a runtime that reports digests in a different order does not
	// read as a configuration change.
	if spec.Image.RepoDigests[0] != "nginx@sha256:aaa" {
		t.Errorf("RepoDigests not sorted: %v", spec.Image.RepoDigests)
	}
}

// reverseSlice reverses any slice in place.
func reverseSlice[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
