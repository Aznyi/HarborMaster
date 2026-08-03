package docker

// This file is the boundary. Every Docker SDK type that HarborMaster reads is
// converted to a HarborMaster domain type here, and nowhere else. Nothing
// outside package docker imports github.com/docker/docker.

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-connections/nat"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// maxHealthLogEntries bounds the healthcheck history carried per container.
// The daemon keeps a handful; this caps it regardless of what it returns.
const maxHealthLogEntries = 5

// normalizeSummary converts a list entry.
//
// Summary data alone is enough to record a container: if inspection later
// fails, this is what gets persisted, so the container still appears in the
// inventory with a warning attached rather than vanishing from it.
func normalizeSummary(summary container.Summary) domain.ContainerSummary {
	labels := summary.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	out := domain.ContainerSummary{
		HostID:       domain.LocalHostID,
		ID:           summary.ID,
		ShortID:      domain.ShortenID(summary.ID),
		Name:         normalizeContainerName(summary.Names),
		Image:        domain.ParseImageRef(summary.Image),
		ImageID:      summary.ImageID,
		State:        domain.ParseContainerState(string(summary.State)),
		Status:       summary.Status,
		Health:       healthFromStatusText(summary.Status),
		Compose:      domain.ParseComposeMetadata(labels),
		HarborMaster: domain.ParseHarborMasterMetadata(labels),
		Ports:        normalizeSummaryPorts(summary.Ports),
		Present:      true,
	}

	if summary.Created > 0 {
		out.CreatedAt = time.Unix(summary.Created, 0).UTC()
	}
	return out
}

// normalizeInspection converts a full inspection into normalized sections.
func (c *Client) normalizeInspection(inspected container.InspectResponse, raw []byte) *Inspection {
	result := &Inspection{}

	// Identity is read defensively BEFORE anything else. ID and Name are
	// promoted through the embedded *ContainerJSONBase, so touching them when
	// that pointer is nil panics -- and the whole point of this function is to
	// survive a partial record.
	var containerID, containerName string
	if inspected.ContainerJSONBase != nil {
		containerID = inspected.ID
		containerName = strings.TrimPrefix(inspected.Name, "/")
	}

	warn := func(code domain.WarningCode, message string) {
		result.Warnings = append(result.Warnings, domain.InventoryWarning{
			ContainerID:   containerID,
			ContainerName: containerName,
			Code:          code,
			Message:       message,
			OccurredAt:    time.Now().UTC(),
		})
	}

	// The SDK models these as pointers, and a daemon that returns a partial
	// record must degrade to a warning rather than panic mid-refresh.
	if inspected.ContainerJSONBase == nil {
		warn(domain.WarningIncompleteData, "runtime returned a container record without base fields")
		result.Detail = domain.ContainerDetail{}
		result.RawJSON = redactRawInspection(raw, c.masker)
		return result
	}
	config := inspected.Config
	if config == nil {
		warn(domain.WarningIncompleteData, "runtime returned a container record without configuration")
		config = &container.Config{}
	}
	hostConfig := inspected.HostConfig
	if hostConfig == nil {
		warn(domain.WarningIncompleteData, "runtime returned a container record without host configuration")
		hostConfig = &container.HostConfig{}
	}

	labels := config.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	state := normalizeState(inspected.State, inspected.RestartCount)

	overview := domain.ContainerSummary{
		HostID:        domain.LocalHostID,
		ID:            containerID,
		ShortID:       domain.ShortenID(containerID),
		Name:          containerName,
		Image:         domain.ParseImageRef(config.Image),
		ImageID:       inspected.Image,
		State:         state.State,
		Status:        state.Status,
		Health:        state.Health,
		StartedAt:     state.StartedAt,
		FinishedAt:    state.FinishedAt,
		ExitCode:      state.ExitCode,
		RestartCount:  inspected.RestartCount,
		RestartPolicy: normalizeRestartPolicy(hostConfig.RestartPolicy),
		Compose:       domain.ParseComposeMetadata(labels),
		HarborMaster:  domain.ParseHarborMasterMetadata(labels),
		Ports:         normalizeInspectedPorts(config.ExposedPorts, inspected.NetworkSettings),
		Present:       true,
	}
	if created, ok := parseDockerTime(inspected.Created); ok {
		overview.CreatedAt = created
	}

	result.Detail = domain.ContainerDetail{
		Overview:     overview,
		State:        state,
		Process:      normalizeProcess(config),
		HealthCheck:  normalizeHealthCheck(config.Healthcheck),
		Environment:  c.masker.ClassifyEnvironment(config.Env),
		Labels:       normalizeLabels(labels),
		Ports:        overview.Ports,
		Mounts:       normalizeMounts(inspected.Mounts, hostConfig),
		Networks:     normalizeNetworkAttachments(inspected.NetworkSettings),
		Resources:    normalizeResources(hostConfig),
		Security:     normalizeSecurity(inspected.ContainerJSONBase, hostConfig),
		Logging:      c.normalizeLogging(hostConfig.LogConfig),
		Compose:      overview.Compose,
		HarborMaster: overview.HarborMaster,
		Warnings:     result.Warnings,
	}
	result.RawJSON = redactRawInspection(raw, c.masker)
	return result
}

// normalizeContainerName picks the primary name and strips Docker's leading
// slash. A container with no name is identified by its short ID.
func normalizeContainerName(names []string) string {
	for _, name := range names {
		trimmed := strings.TrimPrefix(name, "/")
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// healthFromStatusText recovers health from a list-view status string such as
// "Up 2 hours (healthy)".
//
// The list endpoint does not return structured health, and this is the only
// place it can be had without inspecting. Used solely as a fallback when
// inspection fails.
func healthFromStatusText(status string) domain.HealthState {
	switch {
	case strings.Contains(status, "(healthy)"):
		return domain.HealthHealthy
	case strings.Contains(status, "(unhealthy)"):
		return domain.HealthUnhealthy
	case strings.Contains(status, "(health: starting)"):
		return domain.HealthStarting
	default:
		return domain.HealthNone
	}
}

func normalizeState(state *container.State, restartCount int) domain.StateDetail {
	if state == nil {
		return domain.StateDetail{State: domain.StateUnknown, Health: domain.HealthNone, RestartCount: restartCount}
	}

	detail := domain.StateDetail{
		State:        domain.ParseContainerState(string(state.Status)),
		RawState:     string(state.Status),
		Running:      state.Running,
		Paused:       state.Paused,
		Restarting:   state.Restarting,
		Dead:         state.Dead,
		OOMKilled:    state.OOMKilled,
		Error:        state.Error,
		RestartCount: restartCount,
		Health:       domain.HealthNone,
	}

	// An exit code is only meaningful once the container has actually exited;
	// reporting 0 for a running container would read as "exited cleanly".
	if !state.Running && !state.Restarting {
		code := state.ExitCode
		detail.ExitCode = &code
	}
	if started, ok := parseDockerTime(state.StartedAt); ok {
		detail.StartedAt = &started
	}
	if finished, ok := parseDockerTime(state.FinishedAt); ok {
		detail.FinishedAt = &finished
	}

	if state.Health != nil {
		detail.Health = domain.ParseHealthState(string(state.Health.Status))
		detail.HealthFailingStreak = state.Health.FailingStreak
		detail.HealthLog = normalizeHealthLog(state.Health.Log)
	}

	// Status text mirrors what the list view shows, rebuilt from structured
	// fields so it is consistent whichever path produced the record.
	detail.Status = string(state.Status)
	return detail
}

// normalizeHealthLog keeps timings and exit codes only. The probe's output is
// deliberately dropped -- see domain.HealthLogEntry.
func normalizeHealthLog(entries []*container.HealthcheckResult) []domain.HealthLogEntry {
	if len(entries) == 0 {
		return nil
	}

	// Keep the most recent entries; the daemon returns oldest first.
	if len(entries) > maxHealthLogEntries {
		entries = entries[len(entries)-maxHealthLogEntries:]
	}

	log := make([]domain.HealthLogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		log = append(log, domain.HealthLogEntry{
			Start:    entry.Start.UTC(),
			End:      entry.End.UTC(),
			ExitCode: entry.ExitCode,
		})
	}
	return log
}

func normalizeProcess(config *container.Config) domain.Process {
	process := domain.Process{
		Hostname:   config.Hostname,
		Domainname: config.Domainname,
		Entrypoint: []string(config.Entrypoint),
		Command:    []string(config.Cmd),
		User:       config.User,
		WorkingDir: config.WorkingDir,
		StopSignal: config.StopSignal,
		TTY:        config.Tty,
		StdinOpen:  config.OpenStdin,
	}
	if config.StopTimeout != nil {
		timeout := *config.StopTimeout
		process.StopTimeoutSeconds = &timeout
	}
	return process
}

func normalizeHealthCheck(healthcheck *container.HealthConfig) *domain.HealthCheck {
	if healthcheck == nil {
		return nil
	}

	check := &domain.HealthCheck{
		Test:            append([]string(nil), healthcheck.Test...),
		IntervalMS:      healthcheck.Interval.Milliseconds(),
		TimeoutMS:       healthcheck.Timeout.Milliseconds(),
		StartPeriodMS:   healthcheck.StartPeriod.Milliseconds(),
		StartIntervalMS: healthcheck.StartInterval.Milliseconds(),
		Retries:         healthcheck.Retries,
	}
	// {"NONE"} is Docker's way of saying "the image has a healthcheck and this
	// container turns it off", which is not the same as having none.
	check.Disabled = len(check.Test) == 1 && check.Test[0] == "NONE"
	return check
}

func normalizeLabels(labels map[string]string) []domain.Label {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]domain.Label, 0, len(keys))
	for _, key := range keys {
		out = append(out, domain.Label{
			Key:    key,
			Value:  labels[key],
			Source: domain.ClassifyLabel(key),
		})
	}
	return out
}

// normalizeSummaryPorts converts the list view's flat port list.
func normalizeSummaryPorts(ports []container.Port) []domain.Port {
	out := make([]domain.Port, 0, len(ports))
	for _, port := range ports {
		out = append(out, domain.Port{
			ContainerPort: port.PrivatePort,
			Protocol:      defaultProtocol(port.Type),
			HostIP:        port.IP,
			HostPort:      port.PublicPort,
			Published:     port.PublicPort != 0,
		})
	}
	domain.SortPorts(out)
	return out
}

// normalizeInspectedPorts merges exposed-but-unpublished ports with published
// bindings, so the inventory shows both what the image offers and what is
// actually reachable.
func normalizeInspectedPorts(exposed nat.PortSet, settings *container.NetworkSettings) []domain.Port {
	out := make([]domain.Port, 0, len(exposed))
	bound := make(map[string]struct{})

	if settings != nil {
		for port, bindings := range settings.Ports {
			containerPort, protocol := splitNatPort(port)
			if len(bindings) == 0 {
				// Exposed via the port map but not bound to the host.
				continue
			}
			bound[string(port)] = struct{}{}
			for _, binding := range bindings {
				hostPort, _ := strconv.ParseUint(binding.HostPort, 10, 16)
				out = append(out, domain.Port{
					ContainerPort: containerPort,
					Protocol:      protocol,
					HostIP:        binding.HostIP,
					HostPort:      uint16(hostPort),
					Published:     true,
				})
			}
		}
	}

	for port := range exposed {
		if _, published := bound[string(port)]; published {
			continue
		}
		containerPort, protocol := splitNatPort(port)
		out = append(out, domain.Port{
			ContainerPort: containerPort,
			Protocol:      protocol,
			Published:     false,
		})
	}

	domain.SortPorts(out)
	return out
}

// splitNatPort turns "8080/tcp" into (8080, "tcp").
func splitNatPort(port nat.Port) (uint16, string) {
	number, err := strconv.ParseUint(port.Port(), 10, 16)
	if err != nil {
		return 0, defaultProtocol(port.Proto())
	}
	return uint16(number), defaultProtocol(port.Proto())
}

func defaultProtocol(protocol string) string {
	if protocol == "" {
		return "tcp"
	}
	return strings.ToLower(protocol)
}

func normalizeMounts(mounts []container.MountPoint, hostConfig *container.HostConfig) []domain.Mount {
	out := make([]domain.Mount, 0, len(mounts))

	// Driver options live on the host config's mount specs, not on the
	// reported mount points, so index them by destination to merge.
	specByTarget := make(map[string]mount.Mount, len(hostConfig.Mounts))
	for _, spec := range hostConfig.Mounts {
		specByTarget[spec.Target] = spec
	}

	for _, point := range mounts {
		normalized := domain.Mount{
			Type:        normalizeMountType(point.Type),
			Source:      point.Source,
			Destination: point.Destination,
			ReadOnly:    !point.RW,
			Propagation: string(point.Propagation),
			Consistency: point.Mode,
			VolumeName:  point.Name,
			Driver:      point.Driver,
		}

		if spec, ok := specByTarget[point.Destination]; ok {
			if spec.VolumeOptions != nil && spec.VolumeOptions.DriverConfig != nil {
				normalized.DriverOptions = copyStringMap(spec.VolumeOptions.DriverConfig.Options)
				if normalized.Driver == "" {
					normalized.Driver = spec.VolumeOptions.DriverConfig.Name
				}
			}
			if spec.TmpfsOptions != nil {
				normalized.TmpfsOptions = formatTmpfsOptions(
					spec.TmpfsOptions.SizeBytes, uint32(spec.TmpfsOptions.Mode))
			}
		}
		out = append(out, normalized)
	}

	// tmpfs mounts declared via HostConfig.Tmpfs are not reported as mount
	// points at all, so they would otherwise be invisible in the inventory.
	targets := make(map[string]struct{}, len(out))
	for _, existing := range out {
		targets[existing.Destination] = struct{}{}
	}
	for target, options := range hostConfig.Tmpfs {
		if _, exists := targets[target]; exists {
			continue
		}
		out = append(out, domain.Mount{
			Type:         domain.MountTypeTmpfs,
			Destination:  target,
			TmpfsOptions: options,
		})
	}

	domain.SortMounts(out)
	return out
}

func normalizeMountType(mountType mount.Type) domain.MountType {
	switch mountType {
	case mount.TypeBind:
		return domain.MountTypeBind
	case mount.TypeVolume:
		return domain.MountTypeVolume
	case mount.TypeTmpfs:
		return domain.MountTypeTmpfs
	case mount.TypeNamedPipe:
		return domain.MountTypeNamedPipe
	case mount.TypeCluster:
		return domain.MountTypeCluster
	default:
		return domain.MountTypeUnknown
	}
}

func formatTmpfsOptions(sizeBytes int64, mode uint32) string {
	parts := make([]string, 0, 2)
	if sizeBytes > 0 {
		parts = append(parts, "size="+strconv.FormatInt(sizeBytes, 10))
	}
	if mode != 0 {
		parts = append(parts, "mode="+strconv.FormatUint(uint64(mode), 8))
	}
	return strings.Join(parts, ",")
}

func normalizeNetworkAttachments(settings *container.NetworkSettings) []domain.NetworkAttachment {
	if settings == nil || len(settings.Networks) == 0 {
		return []domain.NetworkAttachment{}
	}

	out := make([]domain.NetworkAttachment, 0, len(settings.Networks))
	for name, endpoint := range settings.Networks {
		if endpoint == nil {
			out = append(out, domain.NetworkAttachment{NetworkName: name})
			continue
		}
		out = append(out, domain.NetworkAttachment{
			NetworkID:   endpoint.NetworkID,
			NetworkName: name,
			Aliases:     append([]string(nil), endpoint.Aliases...),
			IPv4Address: endpoint.IPAddress,
			IPv6Address: endpoint.GlobalIPv6Address,
			Gateway:     endpoint.Gateway,
			// MacAddress moved to the endpoint in API 1.44; Config.MacAddress
			// is deprecated and not read.
			MACAddress: endpoint.MacAddress,
			EndpointID: endpoint.EndpointID,
			Links:      append([]string(nil), endpoint.Links...),
		})
	}
	domain.SortNetworkAttachments(out)
	return out
}

func normalizeRestartPolicy(policy container.RestartPolicy) domain.RestartPolicy {
	name := string(policy.Name)
	if name == "" {
		// An empty policy means "no", which is what the daemon applies.
		name = "no"
	}
	return domain.RestartPolicy{Name: name, MaximumRetryCount: policy.MaximumRetryCount}
}

func normalizeResources(hostConfig *container.HostConfig) domain.Resources {
	resources := domain.Resources{
		CPUShares:              hostConfig.CPUShares,
		CPUQuota:               hostConfig.CPUQuota,
		CPUPeriod:              hostConfig.CPUPeriod,
		NanoCPUs:               hostConfig.NanoCPUs,
		CpusetCPUs:             hostConfig.CpusetCpus,
		CpusetMems:             hostConfig.CpusetMems,
		MemoryBytes:            hostConfig.Memory,
		MemoryReservationBytes: hostConfig.MemoryReservation,
		MemorySwapBytes:        hostConfig.MemorySwap,
		KernelMemoryBytes:      hostConfig.KernelMemory, //nolint:staticcheck // reported only if a daemon still sets it
		BlkioWeight:            hostConfig.BlkioWeight,
		ShmSizeBytes:           hostConfig.ShmSize,
		OomScoreAdj:            hostConfig.OomScoreAdj,
	}

	// Pointers preserved: "unset" and "zero" mean different things.
	if hostConfig.MemorySwappiness != nil {
		swappiness := *hostConfig.MemorySwappiness
		resources.MemorySwappiness = &swappiness
	}
	if hostConfig.PidsLimit != nil {
		limit := *hostConfig.PidsLimit
		resources.PidsLimit = &limit
	}
	if hostConfig.OomKillDisable != nil {
		disable := *hostConfig.OomKillDisable
		resources.OomKillDisable = &disable
	}

	for _, ulimit := range hostConfig.Ulimits {
		if ulimit == nil {
			continue
		}
		resources.Ulimits = append(resources.Ulimits, domain.Ulimit{
			Name: ulimit.Name,
			Soft: ulimit.Soft,
			Hard: ulimit.Hard,
		})
	}
	sort.SliceStable(resources.Ulimits, func(i, j int) bool {
		return resources.Ulimits[i].Name < resources.Ulimits[j].Name
	})
	return resources
}

func normalizeSecurity(base *container.ContainerJSONBase, hostConfig *container.HostConfig) domain.Security {
	security := domain.Security{
		Privileged:        hostConfig.Privileged,
		ReadonlyRootfs:    hostConfig.ReadonlyRootfs,
		CapAdd:            []string(hostConfig.CapAdd),
		CapDrop:           []string(hostConfig.CapDrop),
		SecurityOpt:       append([]string(nil), hostConfig.SecurityOpt...),
		DeviceCgroupRules: append([]string(nil), hostConfig.DeviceCgroupRules...),
		IPCMode:           string(hostConfig.IpcMode),
		PIDMode:           string(hostConfig.PidMode),
		UTSMode:           string(hostConfig.UTSMode),
		UsernsMode:        string(hostConfig.UsernsMode),
		CgroupnsMode:      string(hostConfig.CgroupnsMode),
		Sysctls:           copyStringMap(hostConfig.Sysctls),
		GroupAdd:          append([]string(nil), hostConfig.GroupAdd...),
		AppArmorProfile:   base.AppArmorProfile,
		SELinuxLabel:      base.ProcessLabel,
	}

	for _, device := range hostConfig.Devices {
		security.Devices = append(security.Devices, domain.Device{
			PathOnHost:        device.PathOnHost,
			PathInContainer:   device.PathInContainer,
			CgroupPermissions: device.CgroupPermissions,
		})
	}
	for _, request := range hostConfig.DeviceRequests {
		security.DeviceRequests = append(security.DeviceRequests, domain.DeviceRequest{
			Driver:       request.Driver,
			Count:        request.Count,
			DeviceIDs:    append([]string(nil), request.DeviceIDs...),
			Capabilities: request.Capabilities,
			Options:      copyStringMap(request.Options),
		})
	}

	applySecurityOptions(&security, hostConfig.SecurityOpt)
	return security
}

// applySecurityOptions parses the free-form SecurityOpt list into named
// fields. Docker accepts both "key=value" and the older "key:value".
func applySecurityOptions(security *domain.Security, options []string) {
	for _, option := range options {
		key, value := splitSecurityOption(option)
		switch strings.ToLower(key) {
		case "apparmor":
			if security.AppArmorProfile == "" {
				security.AppArmorProfile = value
			}
		case "seccomp":
			security.SeccompProfile = value
		case "label":
			if security.SELinuxLabel == "" {
				security.SELinuxLabel = value
			}
		case "no-new-privileges":
			// Bare "no-new-privileges" means enabled.
			security.NoNewPrivileges = value == "" || strings.EqualFold(value, "true")
		}
	}
}

func splitSecurityOption(option string) (string, string) {
	if key, value, found := strings.Cut(option, "="); found {
		return key, value
	}
	if key, value, found := strings.Cut(option, ":"); found {
		return key, value
	}
	return option, ""
}

// normalizeLogging masks credential-bearing log driver options. A Splunk
// token or a Loki basic-auth URL lives here just as often as in the
// environment.
func (c *Client) normalizeLogging(config container.LogConfig) domain.Logging {
	return domain.Logging{
		Driver:  config.Type,
		Options: c.masker.ClassifyMap(config.Config),
	}
}

func normalizeImage(inspected image.InspectResponse) domain.Image {
	normalized := domain.Image{
		ID:           inspected.ID,
		ShortID:      domain.ShortenID(inspected.ID),
		RepoTags:     append([]string(nil), inspected.RepoTags...),
		RepoDigests:  append([]string(nil), inspected.RepoDigests...),
		Architecture: inspected.Architecture,
		OS:           inspected.Os,
		OSVersion:    inspected.OsVersion,
		Variant:      inspected.Variant,
		Size:         inspected.Size,
		Author:       inspected.Author,
		Comment:      inspected.Comment,
	}
	if created, ok := parseDockerTime(inspected.Created); ok {
		normalized.CreatedAt = created
	}
	if inspected.Config != nil {
		normalized.Labels = copyStringMap(inspected.Config.Labels)
	}

	sort.Strings(normalized.RepoTags)
	sort.Strings(normalized.RepoDigests)
	return normalized
}

func normalizeNetwork(summary network.Summary) domain.Network {
	normalized := domain.Network{
		ID:         summary.ID,
		Name:       summary.Name,
		Driver:     summary.Driver,
		Scope:      summary.Scope,
		Internal:   summary.Internal,
		Attachable: summary.Attachable,
		IPv6:       summary.EnableIPv6,
		CreatedAt:  summary.Created.UTC(),
		Labels:     copyStringMap(summary.Labels),
	}
	for _, config := range summary.IPAM.Config {
		if config.Subnet != "" {
			normalized.Subnets = append(normalized.Subnets, config.Subnet)
		}
	}
	sort.Strings(normalized.Subnets)
	return normalized
}

func normalizeVolume(vol volume.Volume) domain.Volume {
	normalized := domain.Volume{
		Name:       vol.Name,
		Driver:     vol.Driver,
		Scope:      vol.Scope,
		Mountpoint: vol.Mountpoint,
		Labels:     copyStringMap(vol.Labels),
		Options:    copyStringMap(vol.Options),
	}
	if created, ok := parseDockerTime(vol.CreatedAt); ok {
		normalized.CreatedAt = created
	}
	return normalized
}

// parseDockerTime accepts the RFC3339 forms the Engine emits and reports
// whether the result is a real timestamp.
//
// The daemon uses the zero time ("0001-01-01T00:00:00Z") to mean "never", for
// example FinishedAt on a container that has not exited. Returning ok=false
// for that keeps a meaningless 1-year-1 timestamp out of the API.
func parseDockerTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	if parsed.IsZero() || parsed.Year() <= 1 {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func copyStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// Deterministic ordering. The refresh inspects containers concurrently, so
// results must be re-ordered before they are hashed or persisted.
func sortContainerSummaries(summaries []domain.ContainerSummary) {
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].Name != summaries[j].Name {
			return summaries[i].Name < summaries[j].Name
		}
		return summaries[i].ID < summaries[j].ID
	})
}

func sortNetworks(networks []domain.Network) {
	sort.SliceStable(networks, func(i, j int) bool {
		if networks[i].Name != networks[j].Name {
			return networks[i].Name < networks[j].Name
		}
		return networks[i].ID < networks[j].ID
	})
}

func sortVolumes(volumes []domain.Volume) {
	sort.SliceStable(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
}
