package domain

import (
	"sort"
	"strconv"
	"strings"
)

// Process is the container's process configuration.
type Process struct {
	Hostname   string   `json:"hostname,omitempty"`
	Domainname string   `json:"domainname,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Command    []string `json:"command,omitempty"`
	// User is the configured user, in "user" or "user:group" form. Empty means
	// the image's default, which is frequently root.
	User       string `json:"user,omitempty"`
	WorkingDir string `json:"workingDir,omitempty"`
	StopSignal string `json:"stopSignal,omitempty"`
	// StopTimeoutSeconds is nil when the container inherits the daemon default.
	StopTimeoutSeconds *int `json:"stopTimeoutSeconds,omitempty"`
	TTY                bool `json:"tty"`
	StdinOpen          bool `json:"stdinOpen"`
}

// Sensitivity classifies an environment variable.
type Sensitivity string

// Sensitivity levels.
const (
	// SensitivityNormal means the value is displayed as-is.
	SensitivityNormal Sensitivity = "normal"
	// SensitivitySensitive means the name matched a masking pattern and the
	// value is replaced before it leaves the process.
	SensitivitySensitive Sensitivity = "sensitive"
)

// MaskedValue is what replaces a sensitive value in any output.
const MaskedValue = "********"

// EnvVar is one normalized environment variable.
//
// RawValue is tagged `json:"-"`, which is the enforcement point for the whole
// masking design: even if an EnvVar is handed to a JSON encoder by mistake --
// in an API response, an error payload, or a log record -- the raw value
// cannot be serialised. Reaching it requires naming the field deliberately,
// which makes every such use greppable and reviewable.
type EnvVar struct {
	Name string `json:"name"`
	// Value is the display-safe value: the real value when the variable is not
	// sensitive, MaskedValue when it is.
	Value       string      `json:"value"`
	Sensitivity Sensitivity `json:"sensitivity"`
	// RawValue is the unmasked value. Never serialised. Persisted so a future
	// phase can recreate a container faithfully.
	RawValue string `json:"-"`

	// Digest is the keyed comparison evidence for a SENSITIVE value, and the
	// only thing about that value which survives persistence.
	//
	// # Why it is computed here rather than where it is used
	//
	// RawValue is `json:"-"`, so it exists only between the daemon read and the
	// first time the detail is encoded. Everything downstream -- a snapshot, a
	// dedup checksum -- runs AFTER that boundary and sees an empty RawValue.
	// Digesting there produced one identical token for every secret on the host.
	//
	// So the digest is taken at classification, the moment the value arrives and
	// the only moment it is both present and known to be sensitive, and is
	// carried forward in its place. The value itself still goes nowhere.
	//
	// Empty for a non-sensitive variable, whose real Value is already stored,
	// and empty when no digester was configured -- which readers must treat as
	// "cannot be compared", never as "unchanged".
	Digest          string          `json:"digest,omitempty"`
	DigestAlgorithm DigestAlgorithm `json:"digestAlgorithm,omitempty"`
	DigestKeyID     string          `json:"digestKeyId,omitempty"`
	// ValueLength is the byte length of the raw value, carried for the same
	// reason and with the same lifetime. Already part of the recorded design:
	// SecretDigest.Length is serialised, and the snapshot's environment rows
	// have always had a length column.
	ValueLength int `json:"valueLength,omitempty"`
}

// SecretEvidence projects the carried comparison evidence.
//
// Present mirrors the variable being set at all, which is what the snapshot's
// own "present" flag means; a variable set to the empty string still has
// evidence, and one that was never set does not.
func (e EnvVar) SecretEvidence() SecretDigest {
	return SecretDigest{
		Present:   true,
		Length:    e.ValueLength,
		Digest:    e.Digest,
		Algorithm: e.DigestAlgorithm,
		KeyID:     e.DigestKeyID,
	}
}

// Sensitive reports whether the variable was classified as secret-bearing.
func (e EnvVar) Sensitive() bool { return e.Sensitivity == SensitivitySensitive }

// Label is one container label, tagged with the namespace it belongs to.
type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Source records which convention the key belongs to, so the UI can group
	// Compose and HarborMaster labels away from application labels.
	Source LabelSource `json:"source"`
}

// LabelSource identifies the convention a label key belongs to.
type LabelSource string

// Label sources.
const (
	LabelSourceUser         LabelSource = "user"
	LabelSourceCompose      LabelSource = "compose"
	LabelSourceHarborMaster LabelSource = "harbormaster"
)

// Label key prefixes HarborMaster recognises.
const (
	ComposeLabelPrefix      = "com.docker.compose."
	HarborMasterLabelPrefix = "io.harbormaster."
)

// Standard Compose label keys.
const (
	LabelComposeProject           = "com.docker.compose.project"
	LabelComposeService           = "com.docker.compose.service"
	LabelComposeContainerNumber   = "com.docker.compose.container-number"
	LabelComposeProjectWorkingDir = "com.docker.compose.project.working_dir"
	LabelComposeProjectConfigDirs = "com.docker.compose.project.config_files"
	LabelComposeVersion           = "com.docker.compose.version"
	LabelComposeOneOff            = "com.docker.compose.oneoff"
)

// HarborMaster's own label keys.
const (
	LabelHarborMasterEnabled = "io.harbormaster.enabled"
)

// ClassifyLabel reports which convention a label key belongs to.
func ClassifyLabel(key string) LabelSource {
	switch {
	case strings.HasPrefix(key, ComposeLabelPrefix):
		return LabelSourceCompose
	case strings.HasPrefix(key, HarborMasterLabelPrefix):
		return LabelSourceHarborMaster
	default:
		return LabelSourceUser
	}
}

// ComposeMetadata is the Docker Compose provenance of a container.
type ComposeMetadata struct {
	// Managed is true when the container carries a Compose project label.
	Managed         bool   `json:"managed"`
	Project         string `json:"project,omitempty"`
	Service         string `json:"service,omitempty"`
	ContainerNumber int    `json:"containerNumber,omitempty"`
	WorkingDir      string `json:"workingDir,omitempty"`
	ConfigFiles     string `json:"configFiles,omitempty"`
	Version         string `json:"version,omitempty"`
	// OneOff marks a container started by `compose run` rather than `compose up`.
	OneOff bool `json:"oneOff"`
}

// HarborMasterMetadata is HarborMaster's own labelled configuration.
type HarborMasterMetadata struct {
	// Enabled is nil when the container carries no io.harbormaster.enabled
	// label. Nil, true, and false are three distinct answers, and collapsing
	// them would silently opt containers in or out of future automation.
	Enabled *bool `json:"enabled,omitempty"`
	// Labels holds every io.harbormaster.* label with the prefix stripped.
	Labels map[string]string `json:"labels,omitempty"`
}

// ParseComposeMetadata extracts Compose provenance from a label set.
func ParseComposeMetadata(labels map[string]string) ComposeMetadata {
	meta := ComposeMetadata{
		Project:     labels[LabelComposeProject],
		Service:     labels[LabelComposeService],
		WorkingDir:  labels[LabelComposeProjectWorkingDir],
		ConfigFiles: labels[LabelComposeProjectConfigDirs],
		Version:     labels[LabelComposeVersion],
	}
	meta.Managed = meta.Project != ""

	if n, err := strconv.Atoi(labels[LabelComposeContainerNumber]); err == nil {
		meta.ContainerNumber = n
	}
	// Compose writes "True"/"False"; ParseBool handles both cases.
	if oneOff, err := strconv.ParseBool(labels[LabelComposeOneOff]); err == nil {
		meta.OneOff = oneOff
	}
	return meta
}

// ParseHarborMasterMetadata extracts HarborMaster's own labels.
func ParseHarborMasterMetadata(labels map[string]string) HarborMasterMetadata {
	meta := HarborMasterMetadata{}

	for key, value := range labels {
		if !strings.HasPrefix(key, HarborMasterLabelPrefix) {
			continue
		}
		if meta.Labels == nil {
			meta.Labels = make(map[string]string)
		}
		meta.Labels[strings.TrimPrefix(key, HarborMasterLabelPrefix)] = value
	}

	if raw, ok := labels[LabelHarborMasterEnabled]; ok {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			meta.Enabled = &enabled
		}
	}
	return meta
}

// Port is one exposed or published port mapping.
type Port struct {
	ContainerPort uint16 `json:"containerPort"`
	Protocol      string `json:"protocol"`
	// HostIP and HostPort are set only when the port is published.
	HostIP   string `json:"hostIp,omitempty"`
	HostPort uint16 `json:"hostPort,omitempty"`
	// Published distinguishes a port merely exposed by the image from one
	// actually bound on the host. Only published ports are reachable.
	Published bool `json:"published"`
}

// SortPorts orders ports deterministically: container port, then protocol,
// then host binding. Inventory output must not shuffle between refreshes.
func SortPorts(ports []Port) {
	sort.SliceStable(ports, func(i, j int) bool {
		a, b := ports[i], ports[j]
		if a.ContainerPort != b.ContainerPort {
			return a.ContainerPort < b.ContainerPort
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.HostIP != b.HostIP {
			return a.HostIP < b.HostIP
		}
		return a.HostPort < b.HostPort
	})
}

// MountType is the normalized kind of a mount.
type MountType string

// Mount types.
const (
	MountTypeBind      MountType = "bind"
	MountTypeVolume    MountType = "volume"
	MountTypeTmpfs     MountType = "tmpfs"
	MountTypeNamedPipe MountType = "npipe"
	MountTypeCluster   MountType = "cluster"
	MountTypeUnknown   MountType = "unknown"
)

// Mount is one filesystem mount attached to a container.
//
// HarborMaster records where a mount points, never what it contains.
type Mount struct {
	Type        MountType `json:"type"`
	Source      string    `json:"source,omitempty"`
	Destination string    `json:"destination"`
	ReadOnly    bool      `json:"readOnly"`
	Propagation string    `json:"propagation,omitempty"`
	// Consistency is the mode string the operator supplied, e.g. "z" or "ro".
	Consistency string `json:"consistency,omitempty"`
	// VolumeName and Driver are set for volume mounts.
	VolumeName    string            `json:"volumeName,omitempty"`
	Driver        string            `json:"driver,omitempty"`
	DriverOptions map[string]string `json:"driverOptions,omitempty"`
	// TmpfsOptions is the raw option string for a tmpfs mount, e.g. "size=16m".
	TmpfsOptions string `json:"tmpfsOptions,omitempty"`
}

// SortMounts orders mounts by destination, which is unique per container.
func SortMounts(mounts []Mount) {
	sort.SliceStable(mounts, func(i, j int) bool {
		if mounts[i].Destination != mounts[j].Destination {
			return mounts[i].Destination < mounts[j].Destination
		}
		return mounts[i].Source < mounts[j].Source
	})
}

// NetworkAttachment is a container's endpoint on one network.
type NetworkAttachment struct {
	NetworkID   string   `json:"networkId,omitempty"`
	NetworkName string   `json:"networkName"`
	Driver      string   `json:"driver,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	IPv4Address string   `json:"ipv4Address,omitempty"`
	IPv6Address string   `json:"ipv6Address,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	MACAddress  string   `json:"macAddress,omitempty"`
	EndpointID  string   `json:"endpointId,omitempty"`
	Links       []string `json:"links,omitempty"`
}

// SortNetworkAttachments orders attachments by network name.
func SortNetworkAttachments(attachments []NetworkAttachment) {
	sort.SliceStable(attachments, func(i, j int) bool {
		return attachments[i].NetworkName < attachments[j].NetworkName
	})
}

// RestartPolicy is a container's restart configuration.
type RestartPolicy struct {
	// Name is one of "", "no", "always", "on-failure", "unless-stopped".
	Name string `json:"name"`
	// MaximumRetryCount applies to the on-failure policy only.
	MaximumRetryCount int `json:"maximumRetryCount,omitempty"`
}

// HealthCheck is a container's healthcheck configuration, as configured rather
// than as currently evaluated. Durations are milliseconds, matching the
// existing API convention (latencyMs on the health endpoint).
type HealthCheck struct {
	Test            []string `json:"test,omitempty"`
	IntervalMS      int64    `json:"intervalMs,omitempty"`
	TimeoutMS       int64    `json:"timeoutMs,omitempty"`
	StartPeriodMS   int64    `json:"startPeriodMs,omitempty"`
	StartIntervalMS int64    `json:"startIntervalMs,omitempty"`
	Retries         int      `json:"retries,omitempty"`
	// Disabled is true when the container explicitly turns the image's
	// healthcheck off, which is different from having none configured.
	Disabled bool `json:"disabled"`
}

// Resources is a container's normalized resource configuration.
//
// Pointer fields distinguish "not set" from "set to zero". The difference is
// load-bearing: a PidsLimit of 0 means unlimited, while an absent PidsLimit
// means the daemon default applies, and a future recreate must not confuse
// the two.
type Resources struct {
	CPUShares  int64  `json:"cpuShares,omitempty"`
	CPUQuota   int64  `json:"cpuQuota,omitempty"`
	CPUPeriod  int64  `json:"cpuPeriod,omitempty"`
	NanoCPUs   int64  `json:"nanoCpus,omitempty"`
	CpusetCPUs string `json:"cpusetCpus,omitempty"`
	CpusetMems string `json:"cpusetMems,omitempty"`

	MemoryBytes            int64  `json:"memoryBytes,omitempty"`
	MemoryReservationBytes int64  `json:"memoryReservationBytes,omitempty"`
	MemorySwapBytes        int64  `json:"memorySwapBytes,omitempty"`
	MemorySwappiness       *int64 `json:"memorySwappiness,omitempty"`
	// KernelMemoryBytes is deprecated in the Engine API and is reported only
	// when a daemon still populates it.
	KernelMemoryBytes int64 `json:"kernelMemoryBytes,omitempty"`

	PidsLimit      *int64 `json:"pidsLimit,omitempty"`
	BlkioWeight    uint16 `json:"blkioWeight,omitempty"`
	ShmSizeBytes   int64  `json:"shmSizeBytes,omitempty"`
	OomScoreAdj    int    `json:"oomScoreAdj,omitempty"`
	OomKillDisable *bool  `json:"oomKillDisable,omitempty"`

	Ulimits []Ulimit `json:"ulimits,omitempty"`
}

// Ulimit is one resource limit.
type Ulimit struct {
	Name string `json:"name"`
	Soft int64  `json:"soft"`
	Hard int64  `json:"hard"`
}

// Security is a container's normalized security posture.
//
// This section is the reason the inventory exists: it is what an operator
// checks before and after a change.
type Security struct {
	Privileged     bool `json:"privileged"`
	ReadonlyRootfs bool `json:"readonlyRootfs"`

	CapAdd  []string `json:"capAdd,omitempty"`
	CapDrop []string `json:"capDrop,omitempty"`

	// SecurityOpt is the raw option list; the parsed fields below are derived
	// from it for display.
	SecurityOpt     []string `json:"securityOpt,omitempty"`
	AppArmorProfile string   `json:"apparmorProfile,omitempty"`
	SELinuxLabel    string   `json:"selinuxLabel,omitempty"`
	SeccompProfile  string   `json:"seccompProfile,omitempty"`
	NoNewPrivileges bool     `json:"noNewPrivileges"`

	Devices           []Device        `json:"devices,omitempty"`
	DeviceCgroupRules []string        `json:"deviceCgroupRules,omitempty"`
	DeviceRequests    []DeviceRequest `json:"deviceRequests,omitempty"`

	// NetworkMode is the container's network namespace declaration: "bridge",
	// "host", "none", a network name, or `container:<id>` for a namespace shared
	// with another container.
	//
	// A security fact in its own right -- `host` means the container has the
	// host's network stack -- and the input the dependency subsystem derives a
	// hard runtime relationship from. It sits beside the other namespace modes
	// because it is one, and because an operator checking this section before
	// and after a change needs all of them in one place.
	NetworkMode  string `json:"networkMode,omitempty"`
	IPCMode      string `json:"ipcMode,omitempty"`
	PIDMode      string `json:"pidMode,omitempty"`
	UTSMode      string `json:"utsMode,omitempty"`
	UsernsMode   string `json:"usernsMode,omitempty"`
	CgroupnsMode string `json:"cgroupnsMode,omitempty"`

	Sysctls map[string]string `json:"sysctls,omitempty"`
	// GroupAdd lists supplementary groups. Notable because adding the docker
	// group is how a container gains control of the host.
	GroupAdd []string `json:"groupAdd,omitempty"`
}

// Device is one host device exposed to a container.
type Device struct {
	PathOnHost        string `json:"pathOnHost"`
	PathInContainer   string `json:"pathInContainer"`
	CgroupPermissions string `json:"cgroupPermissions,omitempty"`
}

// DeviceRequest is a driver-mediated device request, as used for GPUs.
type DeviceRequest struct {
	Driver       string            `json:"driver,omitempty"`
	Count        int               `json:"count,omitempty"`
	DeviceIDs    []string          `json:"deviceIds,omitempty"`
	Capabilities [][]string        `json:"capabilities,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
}

// Logging is a container's log driver configuration.
//
// Options are filtered through the same masking rules as environment
// variables: log driver configuration routinely carries endpoint credentials
// (a Splunk token, a Loki basic-auth URL, a Fluentd shared key).
type Logging struct {
	Driver  string   `json:"driver,omitempty"`
	Options []EnvVar `json:"options,omitempty"`
}
