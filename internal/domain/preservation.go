package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Configuration preservation.
//
// # The question this answers
//
// A recreation replaces a container. The operator approved a change of IMAGE
// and nothing else, so every other aspect of the container must come out the
// same. "Must" is easy to write and hard to prove, and an unproved claim about
// a privileged operation is worth very little.
//
// So the configuration is projected, twice, through one builder: once from what
// was captured off the original, and once from the replacement after it is
// running. The two projections are compared field by field, and a divergence
// anywhere fails the recreation closed.
//
// # Why a flat field list rather than a struct comparison
//
// Three reasons, and the first is the one that matters:
//
//  1. A struct comparison silently ignores a field nobody thought to compare. A
//     flat list is EXHAUSTIVE BY CONSTRUCTION -- the builder either emits a
//     field or it does not, and the same builder produces both sides, so a
//     field that exists is always compared.
//  2. The result is directly displayable. An operator reading "why did this
//     fail" gets "security.capAdd: expected NET_ADMIN, got nothing".
//  3. It is trivially deterministic, which is what makes the fingerprint
//     meaningful.
//
// # No value in here is a secret
//
// A sensitive environment variable or log option contributes a KEYED DIGEST,
// produced by the same installation key the snapshots use. Two containers with
// the same password produce the same token and neither reveals it. A digest
// under a different key is not comparable, which is why the key id travels with
// the summary.
//
// Everything else in the projection is configuration HarborMaster already
// displays: mount destinations, network names, capability lists, limits.

// PreservationField is one named, canonically rendered aspect of a container's
// configuration.
//
// Value is always safe to store, log, and render. It is never a raw secret and
// never daemon-authored prose.
type PreservationField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PreservationSummary is the value-free projection of one container's
// configuration.
type PreservationSummary struct {
	// ContainerName and ImageReference are recorded for display. Neither is
	// COMPARED: the image is the thing that is meant to change, and the name is
	// checked separately because a rename is part of the pipeline.
	ContainerName  string `json:"containerName,omitempty"`
	ImageReference string `json:"imageReference,omitempty"`

	// Fields are the compared aspects, in a fixed order.
	Fields []PreservationField `json:"fields"`
	// Fingerprint is the SHA-256 over the canonical rendering of Fields. Two
	// identical configurations produce the same fingerprint.
	Fingerprint string `json:"fingerprint"`
	// DigestKeyID identifies the key sensitive values were digested under.
	// Summaries produced under different keys are NOT comparable.
	DigestKeyID string `json:"digestKeyId,omitempty"`
}

// SecretDigester produces a comparison token for a sensitive value.
//
// A function rather than an interface so this package -- which sits below the
// service layer and must not depend on it -- can take the installation's keyed
// hasher without importing it.
type SecretDigester func(value string) string

// FieldValue returns the rendered value of one field.
func (s PreservationSummary) FieldValue(name string) (string, bool) {
	for _, field := range s.Fields {
		if field.Name == name {
			return field.Value, true
		}
	}
	return "", false
}

// PreservationDiffKind classifies one divergence.
type PreservationDiffKind string

// Divergence kinds.
const (
	// PreservationChanged means both sides have the field and they disagree.
	PreservationChanged PreservationDiffKind = "changed"
	// PreservationMissing means the replacement does not have a field the
	// original did.
	PreservationMissing PreservationDiffKind = "missing"
	// PreservationAdded means the replacement has a field the original did not.
	// Rare, and worth reporting: it means the daemon applied something that was
	// not asked for.
	PreservationAdded PreservationDiffKind = "added"
)

// PreservationDifference is one field that did not survive the recreation.
type PreservationDifference struct {
	Field string               `json:"field"`
	Kind  PreservationDiffKind `json:"kind"`
	// Expected is the original's rendering and Actual the replacement's. Both
	// are bounded before they are stored: a rendering is built from
	// configuration, and configuration can be long.
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// PreservationReport is the outcome of comparing two summaries.
type PreservationReport struct {
	Status VerificationResult `json:"status"`
	// Checked is how many distinct fields were considered, and Matched how many
	// agreed.
	Checked int `json:"checked"`
	Matched int `json:"matched"`

	// Differences are the fields that did not agree, bounded.
	Differences []PreservationDifference `json:"differences,omitempty"`
	// Truncated reports that more differences existed than are listed. A
	// wholesale mismatch produces hundreds, and the first few are enough to act
	// on.
	Truncated bool `json:"truncated,omitempty"`

	ExpectedFingerprint string `json:"expectedFingerprint,omitempty"`
	ActualFingerprint   string `json:"actualFingerprint,omitempty"`

	// Unverifiable reports that the comparison could not be PERFORMED -- most
	// often because the two summaries were digested under different keys. It is
	// never a pass: a check that could not run establishes nothing.
	Unverifiable bool `json:"unverifiable,omitempty"`
	// Reason is HarborMaster's own sentence about a non-pass.
	Reason string `json:"reason,omitempty"`
}

// Bounds on a stored report. Configuration can be long and a wholesale
// mismatch can produce one difference per field, so both dimensions are capped.
const (
	// MaxPreservationDifferences bounds how many divergences are recorded.
	MaxPreservationDifferences = 25
	// MaxPreservationValueBytes bounds one rendered value in a difference.
	MaxPreservationValueBytes = 240
)

// ComparePreservation compares a replacement's configuration against the
// original's.
//
// Fails closed in every direction: a nil digester, a key mismatch, or an empty
// projection all produce an unverifiable report rather than a pass.
func ComparePreservation(expected, actual PreservationSummary) PreservationReport {
	report := PreservationReport{
		Status:              VerificationUnknown,
		ExpectedFingerprint: expected.Fingerprint,
		ActualFingerprint:   actual.Fingerprint,
	}

	// Digests under different keys are not comparable, and treating them as
	// unequal would be as wrong as treating them as equal. Say so instead.
	if expected.DigestKeyID != actual.DigestKeyID {
		report.Unverifiable = true
		report.Reason = "the two configuration projections were produced under different installation keys, so they cannot be compared"
		return report
	}
	if len(expected.Fields) == 0 || len(actual.Fields) == 0 {
		report.Unverifiable = true
		report.Reason = "a configuration projection was empty, so nothing could be compared"
		return report
	}

	actualByName := make(map[string]string, len(actual.Fields))
	for _, field := range actual.Fields {
		actualByName[field.Name] = field.Value
	}
	expectedByName := make(map[string]string, len(expected.Fields))
	for _, field := range expected.Fields {
		expectedByName[field.Name] = field.Value
	}

	// Iterated over the EXPECTED list rather than over a map, so the order of
	// reported differences is the fixed builder order rather than a map's
	// randomised one.
	for _, field := range expected.Fields {
		report.Checked++

		got, present := actualByName[field.Name]
		switch {
		case !present:
			report.add(PreservationDifference{
				Field: field.Name, Kind: PreservationMissing,
				Expected: boundValue(field.Value),
			})
		case got == field.Value:
			report.Matched++
		default:
			report.add(PreservationDifference{
				Field: field.Name, Kind: PreservationChanged,
				Expected: boundValue(field.Value), Actual: boundValue(got),
			})
		}
	}

	for _, field := range actual.Fields {
		if _, known := expectedByName[field.Name]; known {
			continue
		}
		report.Checked++
		report.add(PreservationDifference{
			Field: field.Name, Kind: PreservationAdded,
			Actual: boundValue(field.Value),
		})
	}

	if report.Matched == report.Checked {
		report.Status = VerificationPassed
		return report
	}
	report.Status = VerificationFailed
	report.Reason = "the replacement container's configuration does not match the original's"
	return report
}

// add records one difference, within the bound.
func (r *PreservationReport) add(difference PreservationDifference) {
	if len(r.Differences) >= MaxPreservationDifferences {
		r.Truncated = true
		return
	}
	r.Differences = append(r.Differences, difference)
}

// boundValue makes a rendered value fit to store and to read.
//
// The separators are control characters, chosen so nothing inside an element
// can forge a boundary. SanitiseDisplayText would strip them, which would turn
// two genuinely different renderings into the same displayed string -- so they
// are TRANSLATED into visible markers first, and only then sanitised and
// bounded.
func boundValue(value string) string {
	readable := strings.NewReplacer(
		recordSeparator, ", ",
		unitSeparator, "=",
	).Replace(value)
	return SanitiseDisplayText(readable, MaxPreservationValueBytes)
}

// ------------------------------------------------------------ the builder --

// BuildPreservationSummary projects a container's configuration.
//
// # One builder, both sides
//
// The original and the replacement go through THIS function and no other. That
// is what makes the comparison meaningful: a field that is rendered leniently
// is rendered leniently on both sides, and a field that is forgotten is
// forgotten on both sides and therefore cannot produce a false pass on a real
// difference -- it simply is not claimed to have been checked.
//
// # What is deliberately absent
//
// Runtime state, timestamps, restart counts, health results, IP addresses,
// gateways, MAC addresses, and endpoint ids do not appear. The daemon assigns
// them, so including them would make every recreation report a difference.
//
// The IMAGE does not appear either. It is the one thing that is meant to
// change, and it is verified separately, against the approved digest.
//
// # The hostname special case
//
// When a container does not set a hostname, the daemon uses its short id. The
// replacement is a different container and therefore gets a different one, so a
// hostname that equals the container's own short id is treated as UNSET and not
// compared. A hostname the operator actually chose is compared, and preserved.
func BuildPreservationSummary(detail ContainerDetail, digest SecretDigester) PreservationSummary {
	summary := PreservationSummary{
		ContainerName:  NormaliseContainerName(detail.Overview.Name),
		ImageReference: detail.Overview.Image.Raw,
		Fields:         make([]PreservationField, 0, 48),
	}

	add := func(name, value string) {
		summary.Fields = append(summary.Fields, PreservationField{Name: name, Value: value})
	}

	// ---- process ---------------------------------------------------------

	process := detail.Process
	add("process.entrypoint", joinList(process.Entrypoint))
	add("process.command", joinList(process.Command))
	add("process.user", process.User)
	add("process.workingDir", process.WorkingDir)
	add("process.stopSignal", process.StopSignal)
	add("process.stopTimeout", optionalInt(process.StopTimeoutSeconds))
	add("process.tty", strconv.FormatBool(process.TTY))
	add("process.stdinOpen", strconv.FormatBool(process.StdinOpen))
	add("process.hostname", explicitHostname(detail))
	add("process.domainname", process.Domainname)

	// ---- environment -----------------------------------------------------
	//
	// Order is PRESERVED rather than sorted. Environment order is semantically
	// meaningful to some programs, so a reordering is a real configuration
	// change and must be visible as one.
	add("environment", renderEnv(detail.Environment, digest))

	// ---- labels ----------------------------------------------------------
	//
	// HarborMaster's own lineage label is excluded -- see renderLabels.

	add("labels", renderLabels(detail.Labels))

	// ---- ports -----------------------------------------------------------

	add("ports", renderPorts(detail.Ports))

	// ---- mounts ----------------------------------------------------------

	add("mounts", renderMounts(detail.Mounts))

	// ---- networks --------------------------------------------------------

	add("networks", renderNetworks(detail.Networks))

	// ---- restart policy --------------------------------------------------

	add("restartPolicy", renderRestartPolicy(detail.Overview.RestartPolicy))

	// ---- logging ---------------------------------------------------------

	add("logging.driver", detail.Logging.Driver)
	add("logging.options", renderEnv(detail.Logging.Options, digest))

	// ---- health check ----------------------------------------------------

	add("healthCheck", renderHealthCheck(detail.HealthCheck))

	// ---- resources -------------------------------------------------------

	resources := detail.Resources
	add("resources.cpuShares", strconv.FormatInt(resources.CPUShares, 10))
	add("resources.cpuQuota", strconv.FormatInt(resources.CPUQuota, 10))
	add("resources.cpuPeriod", strconv.FormatInt(resources.CPUPeriod, 10))
	add("resources.nanoCpus", strconv.FormatInt(resources.NanoCPUs, 10))
	add("resources.cpusetCpus", resources.CpusetCPUs)
	add("resources.cpusetMems", resources.CpusetMems)
	add("resources.memoryBytes", strconv.FormatInt(resources.MemoryBytes, 10))
	add("resources.memoryReservationBytes", strconv.FormatInt(resources.MemoryReservationBytes, 10))
	add("resources.memorySwapBytes", strconv.FormatInt(resources.MemorySwapBytes, 10))
	add("resources.memorySwappiness", optionalInt64(resources.MemorySwappiness))
	add("resources.pidsLimit", optionalInt64(resources.PidsLimit))
	add("resources.blkioWeight", strconv.FormatUint(uint64(resources.BlkioWeight), 10))
	add("resources.shmSizeBytes", strconv.FormatInt(resources.ShmSizeBytes, 10))
	add("resources.oomScoreAdj", strconv.Itoa(resources.OomScoreAdj))
	add("resources.oomKillDisable", optionalBool(resources.OomKillDisable))
	add("resources.ulimits", renderUlimits(resources.Ulimits))

	// ---- security --------------------------------------------------------
	//
	// The section that matters most. A recreation that quietly dropped a
	// capability restriction, a seccomp profile, or a read-only root filesystem
	// would be a privilege escalation performed by the tool that exists to
	// prevent them.
	security := detail.Security
	add("security.privileged", strconv.FormatBool(security.Privileged))
	add("security.readonlyRootfs", strconv.FormatBool(security.ReadonlyRootfs))
	add("security.noNewPrivileges", strconv.FormatBool(security.NoNewPrivileges))
	add("security.capAdd", joinSorted(security.CapAdd))
	add("security.capDrop", joinSorted(security.CapDrop))
	add("security.securityOpt", joinSorted(security.SecurityOpt))
	add("security.apparmorProfile", security.AppArmorProfile)
	add("security.seccompProfile", security.SeccompProfile)
	add("security.selinuxLabel", security.SELinuxLabel)
	// The network mode, alongside its two siblings.
	//
	// # Why this was missing, and what its absence hid
	//
	// Every other namespace mode was compared and this one never was. There is
	// no normalisation behind that and no comment claiming one: `networks`
	// above records ATTACHMENTS, and a container on `none`, on `host`, or
	// sharing another container's namespace has none of those -- so all three
	// rendered identically and were indistinguishable from each other.
	//
	// A replacement that came back on `host` when the original was on `none`
	// therefore passed preservation, as would one attached to the wrong
	// provider entirely. Stage 5b found the omission while diagnosing why the
	// IPC and PID rebinds failed and the network rebind did not: the network
	// case was passing because nothing looked.
	//
	// The intended change a REBIND makes is expressed by rewriting the expected
	// configuration before the comparison, in the package that owns the capture,
	// rather than by leaving the field uncompared here. An exception that broad
	// is not a way to permit one approved change.
	add("security.networkMode", security.NetworkMode)
	add("security.ipcMode", security.IPCMode)
	add("security.pidMode", security.PIDMode)
	add("security.utsMode", security.UTSMode)
	add("security.usernsMode", security.UsernsMode)
	add("security.cgroupnsMode", security.CgroupnsMode)
	add("security.groupAdd", joinSorted(security.GroupAdd))
	add("security.sysctls", renderStringMap(security.Sysctls))
	add("security.deviceCgroupRules", joinSorted(security.DeviceCgroupRules))
	add("security.devices", renderDevices(security.Devices))
	add("security.deviceRequests", renderDeviceRequests(security.DeviceRequests))

	summary.Fingerprint = fingerprintFields(summary.Fields)
	return summary
}

// explicitHostname returns the hostname only when the operator chose one.
//
// See the note on BuildPreservationSummary: an unset hostname is filled in by
// the daemon with the container's own short id, and comparing that across two
// different containers would fail every recreation.
//
// # A shared network namespace has no hostname of its own
//
// Docker refuses `--hostname` together with `--network container:<id>` --
// always, not only when the two disagree -- so a container in that mode cannot
// have had one set, and what inspection reports is the PROVIDER's short id.
//
// That is the value a REBIND changes by construction: reattaching a dependent
// from an old provider to a new one moves it from one short id to another. This
// used to recognise only the container's OWN id as daemon-derived, so every
// successful reattachment would have been reported as a configuration change.
//
// Decided by the mode rather than by the value's shape, because the mode is the
// fact. IPC and PID sharing carry no such rule: a container may share either
// while holding its own network namespace, and then its hostname is its own
// configuration and is compared.
func explicitHostname(detail ContainerDetail) string {
	hostname := detail.Process.Hostname
	if hostname == "" {
		return ""
	}
	if SharesNamespace(detail.Security.NetworkMode) {
		return ""
	}
	id := detail.Overview.ID
	if len(id) >= 12 && hostname == id[:12] {
		return ""
	}
	// ShortID is how the rest of HarborMaster spells the same thing; checked
	// too, so a differently-truncated id does not slip through.
	if detail.Overview.ShortID != "" && hostname == detail.Overview.ShortID {
		return ""
	}
	return hostname
}

// ------------------------------------------------------------- renderers --
//
// Every renderer is total, deterministic, and injective enough for comparison:
// two configurations that differ must render differently. Separators are chosen
// so a value containing one cannot forge a boundary that changes the meaning --
// the unit separator and record separator are control characters that
// SanitiseDisplayText strips from anything operator-facing, so they cannot
// appear inside a rendered element.

const (
	// unitSeparator joins the parts of one element.
	unitSeparator = "\x1f"
	// recordSeparator joins elements of a list.
	recordSeparator = "\x1e"
)

func joinList(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.Join(values, recordSeparator)
}

func joinSorted(values []string) string {
	if len(values) == 0 {
		return ""
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return strings.Join(sorted, recordSeparator)
}

func optionalInt(value *int) string {
	if value == nil {
		return "unset"
	}
	return strconv.Itoa(*value)
}

func optionalInt64(value *int64) string {
	if value == nil {
		return "unset"
	}
	return strconv.FormatInt(*value, 10)
}

func optionalBool(value *bool) string {
	if value == nil {
		return "unset"
	}
	return strconv.FormatBool(*value)
}

// renderEnv projects an environment or log-option list.
//
// A sensitive entry contributes a KEYED DIGEST rather than its value. A nil
// digester yields a token that can never match anything, so a summary built
// without a key fails the comparison closed rather than matching vacuously.
func renderEnv(vars []EnvVar, digest SecretDigester) string {
	if len(vars) == 0 {
		return ""
	}

	parts := make([]string, 0, len(vars))
	for _, v := range vars {
		if !v.Sensitive() {
			parts = append(parts, v.Name+unitSeparator+v.Value)
			continue
		}
		if digest == nil {
			// Fails closed. Two summaries built without a key produce the same
			// token and would match, so the token embeds the variable name to
			// make even that comparison meaningless rather than reassuring.
			parts = append(parts, v.Name+unitSeparator+"unverifiable")
			continue
		}
		parts = append(parts, v.Name+unitSeparator+"digest:"+digest(v.RawValue))
	}
	return strings.Join(parts, recordSeparator)
}

// renderLabels projects a container's labels, excluding the one HarborMaster
// writes itself.
//
// # Why the lineage label is excluded
//
// Preservation answers one question: did the OPERATOR's configuration survive
// the recreation? LineageLabel is not the operator's configuration. HarborMaster
// stamps it onto every replacement it creates for a tracked workload, so it is
// present on the replacement and absent from the original that was captured --
// and comparing it would make every tracked recreation report a difference it
// caused itself, fail verification, and roll back a replacement that was
// correct. That is not a hypothetical: it is what happened the first time a
// recreation ran with lineage enabled.
//
// It is dropped from BOTH sides rather than only from the replacement, and that
// symmetry is the point. The second update of the same workload captures an
// original that ALREADY carries the label from the first, so a one-sided filter
// would compare "absent" against "present" in one direction and "present"
// against "changed" in the other. Dropping it everywhere means the field simply
// is not part of the comparison, whichever side carries it.
//
// Nothing else is filtered. Every operator label is still compared exactly as
// before, so a label the recreation genuinely lost is still caught -- and the
// value of this one is not a safety property: it is re-derived from the plan on
// every recreation and re-validated by LineageFromLabel before it is ever
// believed.
func renderLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		if label.Key == LineageLabel {
			continue
		}
		parts = append(parts, label.Key+unitSeparator+label.Value)
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return strings.Join(parts, recordSeparator)
}

func renderPorts(ports []Port) string {
	if len(ports) == 0 {
		return ""
	}
	sorted := append([]Port(nil), ports...)
	SortPorts(sorted)

	parts := make([]string, 0, len(sorted))
	for _, port := range sorted {
		parts = append(parts, strings.Join([]string{
			strconv.Itoa(int(port.ContainerPort)),
			port.Protocol,
			port.HostIP,
			strconv.Itoa(int(port.HostPort)),
			strconv.FormatBool(port.Published),
		}, unitSeparator))
	}
	return strings.Join(parts, recordSeparator)
}

func renderMounts(mounts []Mount) string {
	if len(mounts) == 0 {
		return ""
	}
	sorted := append([]Mount(nil), mounts...)
	SortMounts(sorted)

	parts := make([]string, 0, len(sorted))
	for _, mount := range sorted {
		parts = append(parts, strings.Join([]string{
			string(mount.Type), mount.Source, mount.Destination,
			strconv.FormatBool(mount.ReadOnly), mount.Propagation,
			mount.VolumeName, mount.Driver, mount.TmpfsOptions,
		}, unitSeparator))
	}
	return strings.Join(parts, recordSeparator)
}

// renderNetworks projects the attachments, stripped of runtime addressing.
//
// IP addresses, gateway, MAC address, and endpoint id are assigned by the
// daemon at start, so including them would make every recreation look like a
// configuration change. Names, aliases, and links are what was configured.
func renderNetworks(networks []NetworkAttachment) string {
	if len(networks) == 0 {
		return ""
	}
	sorted := append([]NetworkAttachment(nil), networks...)
	SortNetworkAttachments(sorted)

	parts := make([]string, 0, len(sorted))
	for _, network := range sorted {
		parts = append(parts, strings.Join([]string{
			network.NetworkName,
			joinSorted(network.Aliases),
			joinSorted(network.Links),
		}, unitSeparator))
	}
	return strings.Join(parts, recordSeparator)
}

func renderRestartPolicy(policy RestartPolicy) string {
	name := policy.Name
	if name == "" {
		name = "no"
	}
	return name + unitSeparator + strconv.Itoa(policy.MaximumRetryCount)
}

func renderHealthCheck(check *HealthCheck) string {
	if check == nil {
		return "none"
	}
	return strings.Join([]string{
		joinList(check.Test),
		strconv.FormatInt(check.IntervalMS, 10),
		strconv.FormatInt(check.TimeoutMS, 10),
		strconv.FormatInt(check.StartPeriodMS, 10),
		strconv.FormatInt(check.StartIntervalMS, 10),
		strconv.Itoa(check.Retries),
		strconv.FormatBool(check.Disabled),
	}, unitSeparator)
}

func renderUlimits(ulimits []Ulimit) string {
	if len(ulimits) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ulimits))
	for _, ulimit := range ulimits {
		parts = append(parts, strings.Join([]string{
			ulimit.Name,
			strconv.FormatInt(ulimit.Soft, 10),
			strconv.FormatInt(ulimit.Hard, 10),
		}, unitSeparator))
	}
	sort.Strings(parts)
	return strings.Join(parts, recordSeparator)
}

func renderStringMap(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, key+unitSeparator+value)
	}
	sort.Strings(parts)
	return strings.Join(parts, recordSeparator)
}

func renderDevices(devices []Device) string {
	if len(devices) == 0 {
		return ""
	}
	parts := make([]string, 0, len(devices))
	for _, device := range devices {
		parts = append(parts, strings.Join([]string{
			device.PathOnHost, device.PathInContainer, device.CgroupPermissions,
		}, unitSeparator))
	}
	sort.Strings(parts)
	return strings.Join(parts, recordSeparator)
}

func renderDeviceRequests(requests []DeviceRequest) string {
	if len(requests) == 0 {
		return ""
	}
	parts := make([]string, 0, len(requests))
	for _, request := range requests {
		capabilities := make([]string, 0, len(request.Capabilities))
		for _, set := range request.Capabilities {
			capabilities = append(capabilities, joinSorted(set))
		}
		sort.Strings(capabilities)

		parts = append(parts, strings.Join([]string{
			request.Driver,
			strconv.Itoa(request.Count),
			joinSorted(request.DeviceIDs),
			strings.Join(capabilities, ","),
			renderStringMap(request.Options),
		}, unitSeparator))
	}
	sort.Strings(parts)
	return strings.Join(parts, recordSeparator)
}

// fingerprintFields hashes the canonical rendering.
//
// SHA-256 over name and value with an explicit length prefix on each, so no
// combination of field names and values can produce the same byte stream as a
// different combination.
func fingerprintFields(fields []PreservationField) string {
	hash := sha256.New()
	for _, field := range fields {
		_, _ = hash.Write([]byte(strconv.Itoa(len(field.Name))))
		_, _ = hash.Write([]byte(":"))
		_, _ = hash.Write([]byte(field.Name))
		_, _ = hash.Write([]byte(strconv.Itoa(len(field.Value))))
		_, _ = hash.Write([]byte(":"))
		_, _ = hash.Write([]byte(field.Value))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}
