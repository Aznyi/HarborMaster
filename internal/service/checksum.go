package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// InventoryChecksum is the deterministic fingerprint of a completed inventory.
//
// # What it is for
//
// Two refreshes of an unchanged host produce the same checksum, so a client can
// tell "nothing changed" from "changed" without diffing the whole inventory.
//
// # What is included
//
// Per container, sorted by container ID: identity (ID, name), image reference
// and image ID, normalized state, health, exit code, restart count and policy,
// Compose and HarborMaster metadata, ports, labels, mounts, network
// attachments, process configuration, healthcheck configuration, resources,
// security posture, logging configuration, and the environment. Then the set
// of image IDs, network IDs, and volume names.
//
// # What is excluded, and why
//
//   - Refresh metadata: timestamps, duration, generation, trigger. These
//     describe the observation, not the thing observed.
//   - Container timestamps: created, started, finished, first/last seen. A
//     restart is visible through state and restart count; the exact instant is
//     not part of the configuration.
//   - Status text ("Up 3 minutes"). It re-renders on every refresh by design.
//   - Healthcheck result timings. Same reason.
//
// # Environment values
//
// A sensitive value contributes SHA-256(value) rather than the value itself, so
// that changing a password changes the checksum without the checksum
// containing, or being derived recoverably from, the password. Individual
// hashes are never persisted or exposed -- they are folded into one aggregate
// digest.
//
// # Relationship to configuration snapshots
//
// This is NOT the snapshot checksum from migration 0001, and the two must not
// be conflated. A snapshot checksum fingerprints one container's captured
// configuration for rollback. This fingerprints the whole current inventory for
// change detection. Different scope, different lifetime, different purpose.
func InventoryChecksum(
	containers []domain.ContainerDetail,
	images []domain.Image,
	networks []domain.Network,
	volumes []domain.Volume,
) string {
	hash := sha256.New()

	// Sorted by ID so concurrent inspection order cannot affect the result.
	ordered := make([]domain.ContainerDetail, len(containers))
	copy(ordered, containers)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Overview.ID < ordered[j].Overview.ID
	})

	for _, container := range ordered {
		writeContainerCanonical(hash, container)
	}

	writeSection(hash, "images")
	imageIDs := make([]string, 0, len(images))
	for _, image := range images {
		imageIDs = append(imageIDs, image.ID)
	}
	sort.Strings(imageIDs)
	for _, id := range imageIDs {
		writeField(hash, "image", id)
	}

	writeSection(hash, "networks")
	networkIDs := make([]string, 0, len(networks))
	for _, network := range networks {
		networkIDs = append(networkIDs, network.ID+"|"+network.Name)
	}
	sort.Strings(networkIDs)
	for _, id := range networkIDs {
		writeField(hash, "network", id)
	}

	writeSection(hash, "volumes")
	volumeNames := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		volumeNames = append(volumeNames, volume.Name)
	}
	sort.Strings(volumeNames)
	for _, name := range volumeNames {
		writeField(hash, "volume", name)
	}

	return hex.EncodeToString(hash.Sum(nil))
}

func writeContainerCanonical(hash io.Writer, container domain.ContainerDetail) {
	overview := container.Overview

	writeSection(hash, "container")
	writeField(hash, "id", overview.ID)
	writeField(hash, "name", overview.Name)
	writeField(hash, "image", overview.Image.Raw)
	writeField(hash, "imageDigest", overview.Image.Digest)
	writeField(hash, "imageId", overview.ImageID)
	writeField(hash, "state", string(overview.State))
	writeField(hash, "health", string(overview.Health))
	writeField(hash, "restartCount", strconv.Itoa(overview.RestartCount))
	writeField(hash, "restartPolicy", overview.RestartPolicy.Name)
	writeField(hash, "restartPolicyMax", strconv.Itoa(overview.RestartPolicy.MaximumRetryCount))
	writeField(hash, "present", strconv.FormatBool(overview.Present))

	if overview.ExitCode != nil {
		writeField(hash, "exitCode", strconv.Itoa(*overview.ExitCode))
	}

	writeField(hash, "composeProject", overview.Compose.Project)
	writeField(hash, "composeService", overview.Compose.Service)
	writeField(hash, "composeNumber", strconv.Itoa(overview.Compose.ContainerNumber))
	writeField(hash, "composeOneOff", strconv.FormatBool(overview.Compose.OneOff))
	if overview.HarborMaster.Enabled != nil {
		writeField(hash, "hmEnabled", strconv.FormatBool(*overview.HarborMaster.Enabled))
	}

	// Ports: order is not semantically meaningful, so it is normalized.
	ports := append([]domain.Port(nil), container.Ports...)
	domain.SortPorts(ports)
	for _, port := range ports {
		writeField(hash, "port", fmt.Sprintf("%d/%s/%s/%d/%t",
			port.ContainerPort, port.Protocol, port.HostIP, port.HostPort, port.Published))
	}

	// Labels: a map, so sorted by key.
	labels := append([]domain.Label(nil), container.Labels...)
	sort.SliceStable(labels, func(i, j int) bool { return labels[i].Key < labels[j].Key })
	for _, label := range labels {
		writeField(hash, "label", label.Key+"="+label.Value)
	}

	mounts := append([]domain.Mount(nil), container.Mounts...)
	domain.SortMounts(mounts)
	for _, mount := range mounts {
		writeField(hash, "mount", fmt.Sprintf("%s|%s|%s|%t|%s|%s",
			mount.Type, mount.Source, mount.Destination, mount.ReadOnly,
			mount.VolumeName, mount.TmpfsOptions))
	}

	attachments := append([]domain.NetworkAttachment(nil), container.Networks...)
	domain.SortNetworkAttachments(attachments)
	for _, attachment := range attachments {
		aliases := append([]string(nil), attachment.Aliases...)
		sort.Strings(aliases)
		writeField(hash, "network", fmt.Sprintf("%s|%s|%s|%s|%v",
			attachment.NetworkName, attachment.IPv4Address, attachment.IPv6Address,
			attachment.MACAddress, aliases))
	}

	process := container.Process
	writeField(hash, "hostname", process.Hostname)
	writeField(hash, "domainname", process.Domainname)
	writeField(hash, "entrypoint", fmt.Sprintf("%v", process.Entrypoint))
	writeField(hash, "command", fmt.Sprintf("%v", process.Command))
	writeField(hash, "user", process.User)
	writeField(hash, "workingDir", process.WorkingDir)
	writeField(hash, "stopSignal", process.StopSignal)
	writeField(hash, "tty", strconv.FormatBool(process.TTY))
	writeField(hash, "stdinOpen", strconv.FormatBool(process.StdinOpen))
	if process.StopTimeoutSeconds != nil {
		writeField(hash, "stopTimeout", strconv.Itoa(*process.StopTimeoutSeconds))
	}

	if check := container.HealthCheck; check != nil {
		writeField(hash, "healthcheck", fmt.Sprintf("%v|%d|%d|%d|%d|%d|%t",
			check.Test, check.IntervalMS, check.TimeoutMS, check.StartPeriodMS,
			check.StartIntervalMS, check.Retries, check.Disabled))
	}

	// Environment order is preserved: it is meaningful to some programs, so a
	// reordering is a real change.
	for _, env := range container.Environment {
		if env.Sensitive() {
			digest := sha256.Sum256([]byte(env.RawValue))
			writeField(hash, "env", env.Name+"=sha256:"+hex.EncodeToString(digest[:]))
			continue
		}
		writeField(hash, "env", env.Name+"="+env.Value)
	}

	writeResourcesCanonical(hash, container.Resources)
	writeSecurityCanonical(hash, container.Security)

	writeField(hash, "logDriver", container.Logging.Driver)
	for _, option := range container.Logging.Options {
		if option.Sensitive() {
			digest := sha256.Sum256([]byte(option.RawValue))
			writeField(hash, "logOption", option.Name+"=sha256:"+hex.EncodeToString(digest[:]))
			continue
		}
		writeField(hash, "logOption", option.Name+"="+option.Value)
	}
}

func writeResourcesCanonical(hash io.Writer, resources domain.Resources) {
	writeField(hash, "cpuShares", strconv.FormatInt(resources.CPUShares, 10))
	writeField(hash, "cpuQuota", strconv.FormatInt(resources.CPUQuota, 10))
	writeField(hash, "cpuPeriod", strconv.FormatInt(resources.CPUPeriod, 10))
	writeField(hash, "nanoCpus", strconv.FormatInt(resources.NanoCPUs, 10))
	writeField(hash, "cpusetCpus", resources.CpusetCPUs)
	writeField(hash, "cpusetMems", resources.CpusetMems)
	writeField(hash, "memory", strconv.FormatInt(resources.MemoryBytes, 10))
	writeField(hash, "memoryReservation", strconv.FormatInt(resources.MemoryReservationBytes, 10))
	writeField(hash, "memorySwap", strconv.FormatInt(resources.MemorySwapBytes, 10))
	writeField(hash, "kernelMemory", strconv.FormatInt(resources.KernelMemoryBytes, 10))
	writeField(hash, "blkioWeight", strconv.FormatUint(uint64(resources.BlkioWeight), 10))
	writeField(hash, "shmSize", strconv.FormatInt(resources.ShmSizeBytes, 10))
	writeField(hash, "oomScoreAdj", strconv.Itoa(resources.OomScoreAdj))

	// Nil and zero are different configurations, so they hash differently.
	if resources.PidsLimit != nil {
		writeField(hash, "pidsLimit", strconv.FormatInt(*resources.PidsLimit, 10))
	}
	if resources.MemorySwappiness != nil {
		writeField(hash, "memorySwappiness", strconv.FormatInt(*resources.MemorySwappiness, 10))
	}
	if resources.OomKillDisable != nil {
		writeField(hash, "oomKillDisable", strconv.FormatBool(*resources.OomKillDisable))
	}

	ulimits := append([]domain.Ulimit(nil), resources.Ulimits...)
	sort.SliceStable(ulimits, func(i, j int) bool { return ulimits[i].Name < ulimits[j].Name })
	for _, ulimit := range ulimits {
		writeField(hash, "ulimit", fmt.Sprintf("%s|%d|%d", ulimit.Name, ulimit.Soft, ulimit.Hard))
	}
}

func writeSecurityCanonical(hash io.Writer, security domain.Security) {
	writeField(hash, "privileged", strconv.FormatBool(security.Privileged))
	writeField(hash, "readonlyRootfs", strconv.FormatBool(security.ReadonlyRootfs))
	writeField(hash, "noNewPrivileges", strconv.FormatBool(security.NoNewPrivileges))
	writeField(hash, "apparmor", security.AppArmorProfile)
	writeField(hash, "selinux", security.SELinuxLabel)
	writeField(hash, "seccomp", security.SeccompProfile)
	writeField(hash, "ipcMode", security.IPCMode)
	writeField(hash, "pidMode", security.PIDMode)
	writeField(hash, "utsMode", security.UTSMode)
	writeField(hash, "usernsMode", security.UsernsMode)
	writeField(hash, "cgroupnsMode", security.CgroupnsMode)

	// Capability and option lists are sets, so they are sorted before hashing.
	writeSortedStrings(hash, "capAdd", security.CapAdd)
	writeSortedStrings(hash, "capDrop", security.CapDrop)
	writeSortedStrings(hash, "securityOpt", security.SecurityOpt)
	writeSortedStrings(hash, "deviceCgroupRule", security.DeviceCgroupRules)
	writeSortedStrings(hash, "groupAdd", security.GroupAdd)

	devices := make([]string, 0, len(security.Devices))
	for _, device := range security.Devices {
		devices = append(devices, fmt.Sprintf("%s|%s|%s",
			device.PathOnHost, device.PathInContainer, device.CgroupPermissions))
	}
	writeSortedStrings(hash, "device", devices)

	requests := make([]string, 0, len(security.DeviceRequests))
	for _, request := range security.DeviceRequests {
		requests = append(requests, fmt.Sprintf("%s|%d|%v|%v",
			request.Driver, request.Count, request.DeviceIDs, request.Capabilities))
	}
	writeSortedStrings(hash, "deviceRequest", requests)

	sysctlKeys := make([]string, 0, len(security.Sysctls))
	for key := range security.Sysctls {
		sysctlKeys = append(sysctlKeys, key)
	}
	sort.Strings(sysctlKeys)
	for _, key := range sysctlKeys {
		writeField(hash, "sysctl", key+"="+security.Sysctls[key])
	}
}

func writeSortedStrings(hash io.Writer, field string, values []string) {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	for _, value := range sorted {
		writeField(hash, field, value)
	}
}

// writeField writes a length-prefixed key/value pair.
//
// Length prefixing prevents ambiguity: without it, {name:"ab", value:"c"} and
// {name:"a", value:"bc"} would hash identically.
func writeField(hash io.Writer, name, value string) {
	_, _ = fmt.Fprintf(hash, "%d:%s=%d:%s\n", len(name), name, len(value), value)
}

func writeSection(hash io.Writer, name string) {
	_, _ = fmt.Fprintf(hash, "--%d:%s--\n", len(name), name)
}
