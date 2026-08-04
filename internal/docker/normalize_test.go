package docker

// In-package tests: normalization is the boundary where Docker SDK types
// become HarborMaster types, so exercising it means constructing SDK structs
// directly. This is the one place in the codebase that does.

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// mustMAC parses a hardware address for a fixture, panicking on a bad literal
// in the same spirit as network.MustParsePort. The moby API types MacAddress as
// network.HardwareAddr rather than as a string.
func mustMAC(s string) network.HardwareAddr {
	addr, err := net.ParseMAC(s)
	if err != nil {
		panic("fixture has an invalid mac address: " + s)
	}
	return network.HardwareAddr(addr)
}

func testClient() *Client {
	return &Client{timeout: time.Second, masker: domain.NewDefaultMasker()}
}

// runningContainer builds a realistic inspection of a healthy Compose-managed
// container with ports, mounts, networks, limits, and hardening.
func runningContainer() container.InspectResponse {
	stopTimeout := 10
	pidsLimit := int64(100)

	return container.InspectResponse{
		ID:              "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Name:            "/web",
		Created:         "2026-08-01T10:00:00.000000000Z",
		Image:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		RestartCount:    2,
		AppArmorProfile: "docker-default",
		ProcessLabel:    "system_u:system_r:container_t:s0",
		State: &container.State{
			Status:     container.StateRunning,
			Running:    true,
			StartedAt:  "2026-08-02T09:00:00.000000000Z",
			FinishedAt: "0001-01-01T00:00:00Z",
			Health: &container.Health{
				Status:        container.Healthy,
				FailingStreak: 0,
				Log: []*container.HealthcheckResult{
					{
						Start:    time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
						End:      time.Date(2026, 8, 2, 9, 0, 1, 0, time.UTC),
						ExitCode: 0,
						Output:   "SECRET DATABASE PASSWORD hunter2 in probe output",
					},
				},
			},
		},
		HostConfig: &container.HostConfig{
			RestartPolicy:  container.RestartPolicy{Name: "unless-stopped"},
			Privileged:     false,
			ReadonlyRootfs: true,
			CapAdd:         []string{"NET_BIND_SERVICE"},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true", "seccomp=unconfined"},
			IpcMode:        "private",
			PidMode:        "",
			UTSMode:        "",
			CgroupnsMode:   "private",
			Sysctls:        map[string]string{"net.ipv4.ip_forward": "1"},
			GroupAdd:       []string{"999"},
			LogConfig: container.LogConfig{
				Type: "json-file",
				Config: map[string]string{
					"max-size":     "10m",
					"splunk-token": "super-secret-token",
				},
			},
			Resources: container.Resources{
				CPUShares:         512,
				NanoCPUs:          1500000000,
				Memory:            536870912,
				MemoryReservation: 268435456,
				CpusetCpus:        "0-3",
				PidsLimit:         &pidsLimit,
				Ulimits: []*container.Ulimit{
					{Name: "nofile", Soft: 1024, Hard: 2048},
				},
			},
			Mounts: []mount.Mount{
				{
					Target: "/data",
					Type:   mount.TypeVolume,
					VolumeOptions: &mount.VolumeOptions{
						DriverConfig: &mount.Driver{
							Name:    "local",
							Options: map[string]string{"type": "nfs"},
						},
					},
				},
			},
			Tmpfs: map[string]string{"/tmp": "size=16m"},
		},
		Config: &container.Config{
			Hostname:    "web",
			Domainname:  "example.internal",
			User:        "1000:1000",
			Image:       "nginx:1.27",
			WorkingDir:  "/usr/share/nginx",
			StopSignal:  "SIGQUIT",
			StopTimeout: &stopTimeout,
			Tty:         false,
			OpenStdin:   false,
			Entrypoint:  []string{"/docker-entrypoint.sh"},
			Cmd:         []string{"nginx", "-g", "daemon off;"},
			Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin",
				"DB_PASSWORD=hunter2",
				"API_KEY=abc123",
				"NORMAL_SETTING=visible",
				"MALFORMED_ENTRY",
			},
			ExposedPorts: network.PortSet{
				network.MustParsePort("80/tcp"):   {},
				network.MustParsePort("443/tcp"):  {},
				network.MustParsePort("9000/tcp"): {},
			},
			Healthcheck: &dockerspec.HealthcheckConfig{
				Test:          []string{"CMD", "curl", "-f", "http://localhost/"},
				Interval:      30 * time.Second,
				Timeout:       5 * time.Second,
				StartPeriod:   10 * time.Second,
				StartInterval: time.Second,
				Retries:       3,
			},
			Labels: map[string]string{
				"com.docker.compose.project":          "shop",
				"com.docker.compose.service":          "web",
				"com.docker.compose.container-number": "1",
				"com.docker.compose.oneoff":           "False",
				"com.docker.compose.version":          "2.29.0",
				"io.harbormaster.enabled":             "true",
				"io.harbormaster.channel":             "stable",
				"maintainer":                          "ops@example.com",
			},
		},
		Mounts: []container.MountPoint{
			{
				Type:        mount.TypeVolume,
				Name:        "shop_data",
				Source:      "/var/lib/docker/volumes/shop_data/_data",
				Destination: "/data",
				Driver:      "local",
				RW:          true,
				Propagation: "",
			},
			{
				Type:        mount.TypeBind,
				Source:      "/etc/nginx/conf.d",
				Destination: "/etc/nginx/conf.d",
				RW:          false,
				Mode:        "ro",
				Propagation: "rprivate",
			},
		},
		NetworkSettings: &container.NetworkSettings{
			Ports: network.PortMap{
				network.MustParsePort("80/tcp"): []network.PortBinding{
					{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8080"},
				},
				network.MustParsePort("443/tcp"): []network.PortBinding{
					{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8443"},
				},
			},
			Networks: map[string]*network.EndpointSettings{
				"shop_default": {
					NetworkID:         "netid1",
					EndpointID:        "epid1",
					IPAddress:         netip.MustParseAddr("172.20.0.3"),
					GlobalIPv6Address: netip.MustParseAddr("fd00::3"),
					Gateway:           netip.MustParseAddr("172.20.0.1"),
					MacAddress:        mustMAC("02:42:ac:14:00:03"),
					Aliases:           []string{"web", "frontend"},
				},
				"shop_backend": {
					NetworkID:  "netid2",
					EndpointID: "epid2",
					IPAddress:  netip.MustParseAddr("172.21.0.3"),
				},
			},
		},
	}
}

func TestNormalizeRunningContainer(t *testing.T) {
	result := testClient().normalizeInspection(runningContainer(), nil)
	detail := result.Detail

	if detail.Overview.Name != "web" {
		t.Errorf("name = %q, want %q (the leading slash must be stripped)", detail.Overview.Name, "web")
	}
	if detail.Overview.ShortID != "abcdef012345" {
		t.Errorf("shortId = %q", detail.Overview.ShortID)
	}
	if detail.Overview.State != domain.StateRunning {
		t.Errorf("state = %q", detail.Overview.State)
	}
	if detail.Overview.Health != domain.HealthHealthy {
		t.Errorf("health = %q", detail.Overview.Health)
	}
	if detail.Overview.Image.Repository != "nginx" || detail.Overview.Image.Tag != "1.27" {
		t.Errorf("image = %+v", detail.Overview.Image)
	}
	if detail.Overview.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy = %+v", detail.Overview.RestartPolicy)
	}
	// A running container has no meaningful exit code.
	if detail.State.ExitCode != nil {
		t.Errorf("exitCode = %v, want nil while running", *detail.State.ExitCode)
	}
	if detail.Process.Hostname != "web" || detail.Process.Domainname != "example.internal" {
		t.Errorf("process identity = %+v", detail.Process)
	}
	if detail.Process.StopTimeoutSeconds == nil || *detail.Process.StopTimeoutSeconds != 10 {
		t.Errorf("stopTimeout = %v", detail.Process.StopTimeoutSeconds)
	}
}

func TestNormalizeComposeAndHarborMasterMetadata(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	compose := detail.Compose
	if !compose.Managed || compose.Project != "shop" || compose.Service != "web" {
		t.Errorf("compose = %+v", compose)
	}
	if compose.ContainerNumber != 1 {
		t.Errorf("container number = %d", compose.ContainerNumber)
	}
	if compose.OneOff {
		t.Error("oneOff should be false for a `compose up` container")
	}

	hm := detail.HarborMaster
	if hm.Enabled == nil || !*hm.Enabled {
		t.Errorf("harbormaster enabled = %v, want true", hm.Enabled)
	}
	if hm.Labels["channel"] != "stable" {
		t.Errorf("harbormaster labels = %+v (prefix should be stripped)", hm.Labels)
	}

	// Labels are tagged by convention so the UI can group them.
	sources := map[string]domain.LabelSource{}
	for _, label := range detail.Labels {
		sources[label.Key] = label.Source
	}
	if sources["com.docker.compose.project"] != domain.LabelSourceCompose {
		t.Error("compose label not classified as compose")
	}
	if sources["io.harbormaster.enabled"] != domain.LabelSourceHarborMaster {
		t.Error("harbormaster label not classified")
	}
	if sources["maintainer"] != domain.LabelSourceUser {
		t.Error("application label not classified as user")
	}
}

// A standalone container carries no Compose labels, and must not be reported
// as Compose-managed.
func TestNormalizeStandaloneContainer(t *testing.T) {
	inspected := runningContainer()
	inspected.Config.Labels = map[string]string{"app": "standalone"}

	detail := testClient().normalizeInspection(inspected, nil).Detail

	if detail.Compose.Managed {
		t.Error("a container without compose labels must not be reported as managed")
	}
	if detail.HarborMaster.Enabled != nil {
		t.Error("absent harbormaster label must stay nil, not default to false")
	}
}

func TestNormalizeEnvironmentMasking(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	byName := map[string]domain.EnvVar{}
	for _, env := range detail.Environment {
		byName[env.Name] = env
	}

	for _, name := range []string{"DB_PASSWORD", "API_KEY"} {
		env, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing from environment", name)
		}
		if env.Value != domain.MaskedValue {
			t.Errorf("%s value = %q, want it masked", name, env.Value)
		}
		if !env.Sensitive() {
			t.Errorf("%s should be classified sensitive", name)
		}
		if env.RawValue == "" {
			t.Errorf("%s raw value should be retained in memory for checksumming", name)
		}
	}

	normal := byName["NORMAL_SETTING"]
	if normal.Value != "visible" || normal.Sensitive() {
		t.Errorf("non-sensitive variable was altered: %+v", normal)
	}

	// An entry with no "=" is still recorded: the runtime accepted it.
	malformed, ok := byName["MALFORMED_ENTRY"]
	if !ok {
		t.Fatal("an entry without '=' should still be recorded")
	}
	if malformed.Value != "" {
		t.Errorf("malformed entry value = %q, want empty", malformed.Value)
	}
}

// Log driver options carry credentials as often as the environment does.
func TestNormalizeLoggingOptionsAreMasked(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	if detail.Logging.Driver != "json-file" {
		t.Errorf("driver = %q", detail.Logging.Driver)
	}

	byName := map[string]domain.EnvVar{}
	for _, option := range detail.Logging.Options {
		byName[option.Name] = option
	}
	if byName["splunk-token"].Value != domain.MaskedValue {
		t.Errorf("splunk-token = %q, want masked", byName["splunk-token"].Value)
	}
	if byName["max-size"].Value != "10m" {
		t.Errorf("max-size = %q, want the real value", byName["max-size"].Value)
	}
}

func TestNormalizePortsMergesExposedAndPublished(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	type key struct {
		port      uint16
		published bool
	}
	seen := map[key]domain.Port{}
	for _, port := range detail.Ports {
		seen[key{port.ContainerPort, port.Published}] = port
	}

	published80, ok := seen[key{80, true}]
	if !ok {
		t.Fatal("published port 80 missing")
	}
	if published80.HostPort != 8080 || published80.HostIP != "127.0.0.1" {
		t.Errorf("port 80 binding = %+v", published80)
	}

	// 9000 is exposed by the image but never bound, which is a meaningful
	// distinction: it is not reachable.
	if _, ok := seen[key{9000, false}]; !ok {
		t.Error("exposed-but-unpublished port 9000 missing")
	}

	// Deterministic ordering.
	for i := 1; i < len(detail.Ports); i++ {
		if detail.Ports[i-1].ContainerPort > detail.Ports[i].ContainerPort {
			t.Fatalf("ports are not sorted: %+v", detail.Ports)
		}
	}
}

func TestNormalizeMounts(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	byDestination := map[string]domain.Mount{}
	for _, mount := range detail.Mounts {
		byDestination[mount.Destination] = mount
	}

	dataMount, ok := byDestination["/data"]
	if !ok {
		t.Fatal("/data mount missing")
	}
	if dataMount.Type != domain.MountTypeVolume || dataMount.VolumeName != "shop_data" {
		t.Errorf("/data = %+v", dataMount)
	}
	if dataMount.DriverOptions["type"] != "nfs" {
		t.Errorf("driver options not merged from the host config spec: %+v", dataMount.DriverOptions)
	}

	confMount, ok := byDestination["/etc/nginx/conf.d"]
	if !ok {
		t.Fatal("bind mount missing")
	}
	if !confMount.ReadOnly {
		t.Error("read-only bind mount reported as writable")
	}

	// A tmpfs declared only in HostConfig.Tmpfs is not a reported mount point
	// and would otherwise be invisible.
	tmp, ok := byDestination["/tmp"]
	if !ok {
		t.Fatal("tmpfs mount from HostConfig.Tmpfs missing")
	}
	if tmp.Type != domain.MountTypeTmpfs || tmp.TmpfsOptions != "size=16m" {
		t.Errorf("/tmp = %+v", tmp)
	}
}

func TestNormalizeMultipleNetworks(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	if len(detail.Networks) != 2 {
		t.Fatalf("networks = %d, want 2", len(detail.Networks))
	}
	// Sorted by name for determinism: map iteration order is random.
	if detail.Networks[0].NetworkName != "shop_backend" {
		t.Errorf("networks not sorted: %+v", detail.Networks)
	}

	var frontend domain.NetworkAttachment
	for _, attachment := range detail.Networks {
		if attachment.NetworkName == "shop_default" {
			frontend = attachment
		}
	}
	if frontend.IPv4Address != "172.20.0.3" || frontend.IPv6Address != "fd00::3" {
		t.Errorf("addresses = %+v", frontend)
	}
	if frontend.MACAddress != "02:42:ac:14:00:03" {
		t.Errorf("mac from the endpoint (not the deprecated Config field) = %q", frontend.MACAddress)
	}
	if len(frontend.Aliases) != 2 {
		t.Errorf("aliases = %v", frontend.Aliases)
	}
}

func TestNormalizeResourceLimits(t *testing.T) {
	resources := testClient().normalizeInspection(runningContainer(), nil).Detail.Resources

	if resources.CPUShares != 512 || resources.NanoCPUs != 1500000000 {
		t.Errorf("cpu = %+v", resources)
	}
	if resources.MemoryBytes != 536870912 || resources.MemoryReservationBytes != 268435456 {
		t.Errorf("memory = %+v", resources)
	}
	if resources.CpusetCPUs != "0-3" {
		t.Errorf("cpuset = %q", resources.CpusetCPUs)
	}
	if resources.PidsLimit == nil || *resources.PidsLimit != 100 {
		t.Errorf("pidsLimit = %v", resources.PidsLimit)
	}
	if len(resources.Ulimits) != 1 || resources.Ulimits[0].Name != "nofile" {
		t.Errorf("ulimits = %+v", resources.Ulimits)
	}
}

// Unset and zero are different configurations and must stay distinguishable.
func TestNormalizeUnsetPointerLimitsStayNil(t *testing.T) {
	inspected := runningContainer()
	inspected.HostConfig.PidsLimit = nil
	inspected.HostConfig.OomKillDisable = nil
	inspected.HostConfig.MemorySwappiness = nil

	resources := testClient().normalizeInspection(inspected, nil).Detail.Resources

	if resources.PidsLimit != nil {
		t.Error("an unset PidsLimit must stay nil, not become 0")
	}
	if resources.OomKillDisable != nil || resources.MemorySwappiness != nil {
		t.Error("unset pointer limits must stay nil")
	}
}

func TestNormalizeSecurity(t *testing.T) {
	security := testClient().normalizeInspection(runningContainer(), nil).Detail.Security

	if security.Privileged {
		t.Error("privileged should be false")
	}
	if !security.ReadonlyRootfs {
		t.Error("readonlyRootfs should be true")
	}
	if !security.NoNewPrivileges {
		t.Error("no-new-privileges:true was not parsed from SecurityOpt")
	}
	if security.SeccompProfile != "unconfined" {
		t.Errorf("seccomp = %q", security.SeccompProfile)
	}
	if security.AppArmorProfile != "docker-default" {
		t.Errorf("apparmor = %q", security.AppArmorProfile)
	}
	if security.SELinuxLabel == "" {
		t.Error("SELinux process label not captured")
	}
	if len(security.CapDrop) != 1 || security.CapDrop[0] != "ALL" {
		t.Errorf("capDrop = %v", security.CapDrop)
	}
	if security.Sysctls["net.ipv4.ip_forward"] != "1" {
		t.Errorf("sysctls = %+v", security.Sysctls)
	}
	if len(security.GroupAdd) != 1 {
		t.Errorf("groupAdd = %v", security.GroupAdd)
	}
}

func TestNormalizeHealthCheckConfiguration(t *testing.T) {
	check := testClient().normalizeInspection(runningContainer(), nil).Detail.HealthCheck

	if check == nil {
		t.Fatal("healthcheck configuration missing")
	}
	if check.IntervalMS != 30000 || check.TimeoutMS != 5000 {
		t.Errorf("durations = %+v", check)
	}
	if check.StartPeriodMS != 10000 || check.StartIntervalMS != 1000 {
		t.Errorf("start timings = %+v", check)
	}
	if check.Retries != 3 {
		t.Errorf("retries = %d", check.Retries)
	}
	if check.Disabled {
		t.Error("a configured healthcheck is not disabled")
	}
}

// {"NONE"} means the container turns the image's healthcheck off, which is not
// the same as having none.
func TestNormalizeDisabledHealthCheck(t *testing.T) {
	inspected := runningContainer()
	inspected.Config.Healthcheck = &dockerspec.HealthcheckConfig{Test: []string{"NONE"}}

	check := testClient().normalizeInspection(inspected, nil).Detail.HealthCheck

	if check == nil || !check.Disabled {
		t.Errorf("healthcheck = %+v, want disabled", check)
	}
}

func TestNormalizeContainerWithoutHealthCheck(t *testing.T) {
	inspected := runningContainer()
	inspected.Config.Healthcheck = nil
	inspected.State.Health = nil

	detail := testClient().normalizeInspection(inspected, nil).Detail

	if detail.HealthCheck != nil {
		t.Error("a container with no healthcheck should have no configuration")
	}
	if detail.Overview.Health != domain.HealthNone {
		t.Errorf("health = %q, want none", detail.Overview.Health)
	}
}

func TestNormalizeUnhealthyContainer(t *testing.T) {
	inspected := runningContainer()
	inspected.State.Health = &container.Health{
		Status:        container.Unhealthy,
		FailingStreak: 4,
	}

	detail := testClient().normalizeInspection(inspected, nil).Detail

	if detail.Overview.Health != domain.HealthUnhealthy {
		t.Errorf("health = %q", detail.Overview.Health)
	}
	if detail.State.HealthFailingStreak != 4 {
		t.Errorf("failing streak = %d", detail.State.HealthFailingStreak)
	}
}

// Healthcheck output is arbitrary and routinely contains credentials, so it
// must never reach the inventory.
func TestNormalizeHealthLogOmitsProbeOutput(t *testing.T) {
	detail := testClient().normalizeInspection(runningContainer(), nil).Detail

	if len(detail.State.HealthLog) != 1 {
		t.Fatalf("health log entries = %d", len(detail.State.HealthLog))
	}
	entry := detail.State.HealthLog[0]
	if entry.ExitCode != 0 || entry.Start.IsZero() || entry.End.IsZero() {
		t.Errorf("entry = %+v", entry)
	}

	// The struct has no output field at all, which is the real guarantee; this
	// asserts the whole detail carries no trace of it.
	rendered := renderDetail(t, detail)
	for _, secret := range []string{"hunter2", "SECRET DATABASE PASSWORD"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("probe output leaked into the inventory: %q", secret)
		}
	}
}

func TestNormalizeStoppedContainer(t *testing.T) {
	inspected := runningContainer()
	inspected.State = &container.State{
		Status:     container.StateExited,
		Running:    false,
		ExitCode:   137,
		OOMKilled:  true,
		Error:      "",
		StartedAt:  "2026-08-02T09:00:00.000000000Z",
		FinishedAt: "2026-08-02T10:00:00.000000000Z",
	}

	detail := testClient().normalizeInspection(inspected, nil).Detail

	if detail.Overview.State != domain.StateExited {
		t.Errorf("state = %q", detail.Overview.State)
	}
	if detail.State.ExitCode == nil || *detail.State.ExitCode != 137 {
		t.Errorf("exitCode = %v, want 137", detail.State.ExitCode)
	}
	if !detail.State.OOMKilled {
		t.Error("oomKilled not captured")
	}
	if detail.State.FinishedAt == nil {
		t.Error("finishedAt should be set for an exited container")
	}
}

// The daemon uses the zero time to mean "never". A 1-year-1 timestamp in the
// API would be worse than an absent one.
func TestNormalizeZeroTimestampsBecomeAbsent(t *testing.T) {
	inspected := runningContainer()
	inspected.State.StartedAt = "0001-01-01T00:00:00Z"
	inspected.State.FinishedAt = "0001-01-01T00:00:00Z"

	detail := testClient().normalizeInspection(inspected, nil).Detail

	if detail.State.StartedAt != nil || detail.State.FinishedAt != nil {
		t.Errorf("zero timestamps should be nil, got %v / %v",
			detail.State.StartedAt, detail.State.FinishedAt)
	}
}

// A daemon returning a partial record must degrade to warnings, not panic
// mid-refresh.
func TestNormalizeIncompleteRecords(t *testing.T) {
	// The moby API flattened ContainerJSONBase into InspectResponse, so "no
	// base fields" is now expressed as a record carrying no identity at all.
	noConfig := runningContainer()
	noConfig.Config = nil

	tests := map[string]container.InspectResponse{
		"no base":   {},
		"no config": noConfig,
		"no host config": {
			ID:     "x",
			Name:   "/x",
			State:  &container.State{Status: container.StateRunning},
			Config: &container.Config{},
		},
	}

	for name, inspected := range tests {
		t.Run(name, func(t *testing.T) {
			result := testClient().normalizeInspection(inspected, nil)
			if len(result.Warnings) == 0 {
				t.Error("an incomplete record should produce a warning")
			}
			for _, warning := range result.Warnings {
				if warning.Code != domain.WarningIncompleteData {
					t.Errorf("warning code = %q", warning.Code)
				}
			}
		})
	}
}

func TestNormalizeIsDeterministic(t *testing.T) {
	client := testClient()

	// Maps iterate randomly, so repeated normalization is the test that
	// ordering has actually been imposed.
	first := renderDetail(t, client.normalizeInspection(runningContainer(), nil).Detail)
	for i := 0; i < 20; i++ {
		next := renderDetail(t, client.normalizeInspection(runningContainer(), nil).Detail)
		if next != first {
			t.Fatal("normalization is not deterministic across runs")
		}
	}
}

func TestNormalizeSummary(t *testing.T) {
	summary := normalizeSummary(container.Summary{
		ID:      "aaaabbbbccccdddd",
		Names:   []string{"/api"},
		Image:   "registry.example.com:5000/team/app:v2",
		ImageID: "sha256:deadbeef",
		State:   container.StateRunning,
		Status:  "Up 3 minutes (healthy)",
		Created: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix(),
		Labels:  map[string]string{"com.docker.compose.project": "team"},
		Ports: []container.PortSummary{
			{PrivatePort: 80, PublicPort: 8080, Type: "tcp", IP: netip.MustParseAddr("0.0.0.0")},
		},
	})

	if summary.Name != "api" {
		t.Errorf("name = %q", summary.Name)
	}
	// A registry port must not be mistaken for a tag separator.
	if summary.Image.Repository != "registry.example.com:5000/team/app" || summary.Image.Tag != "v2" {
		t.Errorf("image = %+v", summary.Image)
	}
	// Health is only available in the status text at list time.
	if summary.Health != domain.HealthHealthy {
		t.Errorf("health = %q, want it recovered from the status text", summary.Health)
	}
	if !summary.Compose.Managed {
		t.Error("compose project label not detected")
	}
	if len(summary.Ports) != 1 || !summary.Ports[0].Published {
		t.Errorf("ports = %+v", summary.Ports)
	}
}

func TestNormalizeImage(t *testing.T) {
	normalized := normalizeImage(image.InspectResponse{
		ID:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		RepoTags:     []string{"nginx:1.27", "nginx:latest"},
		RepoDigests:  []string{"nginx@sha256:2222"},
		Created:      "2026-07-01T00:00:00.000000000Z",
		Architecture: "amd64",
		Os:           "linux",
		Size:         187000000,
	})

	if normalized.ShortID != "111111111111" {
		t.Errorf("shortId = %q", normalized.ShortID)
	}
	if len(normalized.RepoTags) != 2 || normalized.RepoTags[0] != "nginx:1.27" {
		t.Errorf("repoTags = %v (should be sorted)", normalized.RepoTags)
	}
	if normalized.CreatedAt.IsZero() {
		t.Error("created timestamp not parsed")
	}
	if normalized.Size != 187000000 {
		t.Errorf("size = %d", normalized.Size)
	}
}

func TestNormalizeNetworkAndVolume(t *testing.T) {
	// network.Summary now embeds network.Network rather than declaring the
	// fields itself, and IPAMConfig.Subnet is a netip.Prefix.
	summary := normalizeNetwork(network.Summary{
		Network: network.Network{
			ID:         "netid",
			Name:       "bridge",
			Driver:     "bridge",
			Scope:      "local",
			EnableIPv6: false,
			Created:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			IPAM: network.IPAM{
				Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix("172.17.0.0/16")}},
			},
		},
	})
	if summary.Name != "bridge" || len(summary.Subnets) != 1 {
		t.Errorf("network = %+v", summary)
	}
	if summary.Subnets[0] != "172.17.0.0/16" {
		t.Errorf("subnet = %q, want the prefix rendered back to text", summary.Subnets[0])
	}

	vol := normalizeVolume(volume.Volume{
		Name:       "shop_data",
		Driver:     "local",
		Scope:      "local",
		Mountpoint: "/var/lib/docker/volumes/shop_data/_data",
		CreatedAt:  "2026-07-01T00:00:00Z",
		Options:    map[string]string{"type": "nfs"},
	})
	if vol.Name != "shop_data" || vol.Options["type"] != "nfs" {
		t.Errorf("volume = %+v", vol)
	}
	if vol.CreatedAt.IsZero() {
		t.Error("volume created timestamp not parsed")
	}
}

func TestParseImageRefHandlesAwkwardReferences(t *testing.T) {
	tests := map[string]domain.ImageRef{
		"nginx":                        {Repository: "nginx", Tag: "latest"},
		"nginx:1.27":                   {Repository: "nginx", Tag: "1.27"},
		"registry:5000/app":            {Repository: "registry:5000/app", Tag: "latest"},
		"registry:5000/app:v1":         {Repository: "registry:5000/app", Tag: "v1"},
		"nginx@sha256:abc":             {Repository: "nginx", Digest: "sha256:abc"},
		"registry:5000/a/b@sha256:abc": {Repository: "registry:5000/a/b", Digest: "sha256:abc"},
		"":                             {},
	}

	for raw, want := range tests {
		got := domain.ParseImageRef(raw)
		if got.Repository != want.Repository || got.Tag != want.Tag || got.Digest != want.Digest {
			t.Errorf("ParseImageRef(%q) = %+v, want %+v", raw, got, want)
		}
		if got.Raw != raw {
			t.Errorf("raw reference not preserved for %q", raw)
		}
	}
}
