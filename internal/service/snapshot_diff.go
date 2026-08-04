package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ErrDiffBusy reports that the concurrency ceiling is reached.
//
// Refused rather than queued: a queue converts a load spike into unbounded
// memory and latency, and this endpoint is reachable without authentication.
var ErrDiffBusy = errors.New("too many diff operations in progress")

// maxDiffValueBytes bounds one rendered value in the output.
//
// A label or environment value can be arbitrarily long, and a diff of two
// containers each carrying a megabyte-long value would otherwise produce a
// two-megabyte response from a single request.
const maxDiffValueBytes = 4096

// DiffInput is one side of a comparison.
type DiffInput struct {
	SnapshotID int64
	Spec       domain.SnapshotSpec
	Env        []domain.SnapshotEnvEntry
}

// DiffEngine compares two snapshots.
//
// # Bounded by construction
//
// A diff is the most expensive read in the API: it decodes two documents and
// cross-products two configuration sets, and an unauthenticated caller can ask
// for one repeatedly. Every dimension is bounded -- concurrency, wall time,
// entries compared, entries returned, and the size of any single value.
//
// # No interpreters
//
// The engine accepts exactly two inputs and nothing else. There are no
// user-defined comparison expressions, no JSONPath, no field selectors, no
// regular expressions, and no templates. Every one of those is an interpreter,
// and an interpreter reachable from an unauthenticated endpoint is both a
// denial-of-service surface (catastrophic backtracking, unbounded recursion)
// and an injection surface. Narrowing output is done by choosing a group name
// from domain.DiffGroupNames.
type DiffEngine struct {
	cfg config.Snapshots
	// slots is a counting semaphore. Acquired without blocking, so an
	// over-capacity request is refused immediately rather than waiting.
	slots chan struct{}
}

// NewDiffEngine builds a DiffEngine.
func NewDiffEngine(cfg config.Snapshots) *DiffEngine {
	if cfg.MaxConcurrentDiffs < 1 {
		cfg.MaxConcurrentDiffs = config.DefaultSnapshotMaxConcurrentDiffs
	}
	if cfg.DiffTimeout <= 0 {
		cfg.DiffTimeout = config.DefaultSnapshotDiffTimeout
	}
	if cfg.MaxDiffEntries < 1 {
		cfg.MaxDiffEntries = config.DefaultSnapshotMaxDiffEntries
	}
	if cfg.MaxGroupEntries < 1 {
		cfg.MaxGroupEntries = config.DefaultSnapshotMaxGroupEntries
	}

	return &DiffEngine{
		cfg:   cfg,
		slots: make(chan struct{}, cfg.MaxConcurrentDiffs),
	}
}

// DiffOptions narrows a comparison.
type DiffOptions struct {
	// Groups restricts output to these groups. Empty means all of them. Every
	// name is validated against domain.ValidDiffGroup by the caller.
	Groups []domain.DiffGroupName
	// IncludeUnchanged emits unchanged entries too. Off by default so a
	// response is proportional to the change rather than to the configuration.
	IncludeUnchanged bool
	// AgainstCurrent records that the target is live configuration.
	AgainstCurrent bool
}

// Diff compares two configurations.
//
// Returns ErrDiffBusy when the concurrency ceiling is reached.
func (e *DiffEngine) Diff(ctx context.Context, from, to DiffInput, opts DiffOptions) (domain.SnapshotDiff, error) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return domain.SnapshotDiff{}, ErrDiffBusy
	}

	ctx, cancel := context.WithTimeout(ctx, e.cfg.DiffTimeout)
	defer cancel()

	diff := domain.SnapshotDiff{
		FromSnapshotID: from.SnapshotID,
		ToSnapshotID:   to.SnapshotID,
		AgainstCurrent: opts.AgainstCurrent,
		Groups:         make([]domain.DiffGroup, 0, len(domain.DiffGroupNames)),
	}

	wanted := requestedGroups(opts.Groups)
	budget := e.cfg.MaxDiffEntries

	for _, name := range domain.DiffGroupNames {
		if ctx.Err() != nil {
			diff.Truncated = true
			diff.TruncationReason = "the comparison exceeded its time budget"
			break
		}
		if !wanted[name] {
			continue
		}

		group := e.diffGroup(name, from, to, opts, &budget)
		if group.Truncated {
			diff.Truncated = true
			if diff.TruncationReason == "" {
				diff.TruncationReason = fmt.Sprintf(
					"the %s group exceeded a size limit; some entries were not returned", name)
			}
		}
		diff.Groups = append(diff.Groups, group)

		diff.AddedCount += group.Added
		diff.RemovedCount += group.Removed
		diff.ModifiedCount += group.Modified
		diff.UnchangedCount += group.Unchanged
	}

	diff.ChangedCount = diff.AddedCount + diff.RemovedCount + diff.ModifiedCount
	// Only meaningful when nothing was truncated: a truncated comparison has
	// not established that the two are identical.
	diff.Identical = diff.ChangedCount == 0 && !diff.Truncated
	return diff, nil
}

// requestedGroups builds the set of groups to compare.
func requestedGroups(groups []domain.DiffGroupName) map[domain.DiffGroupName]bool {
	wanted := make(map[domain.DiffGroupName]bool, len(domain.DiffGroupNames))
	if len(groups) == 0 {
		for _, name := range domain.DiffGroupNames {
			wanted[name] = true
		}
		return wanted
	}
	for _, name := range groups {
		wanted[name] = true
	}
	return wanted
}

// diffGroup compares one section.
//
// Every group reduces to two keyed maps compared in a single pass: O(n+m), no
// nesting, no backtracking, no recursion. There is no code path here whose cost
// is superlinear in the input.
func (e *DiffEngine) diffGroup(
	name domain.DiffGroupName,
	from, to DiffInput,
	opts DiffOptions,
	budget *int,
) domain.DiffGroup {
	switch name {
	case domain.DiffGroupEnvironment:
		return e.diffEnvironment(from, to, opts, budget)
	case domain.DiffGroupLabels:
		return e.diffKeyed(name, labelMap(from.Spec.Labels), labelMap(to.Spec.Labels), opts, budget)
	case domain.DiffGroupPorts:
		return e.diffKeyed(name, portMap(from.Spec.Ports), portMap(to.Spec.Ports), opts, budget)
	case domain.DiffGroupNetworks:
		return e.diffKeyed(name, networkMap(from.Spec.Networks), networkMap(to.Spec.Networks), opts, budget)
	case domain.DiffGroupMounts:
		return e.diffKeyed(name, mountMap(from.Spec.Mounts), mountMap(to.Spec.Mounts), opts, budget)
	case domain.DiffGroupResources:
		return e.diffKeyed(name, resourceMap(from.Spec.Resources), resourceMap(to.Spec.Resources), opts, budget)
	case domain.DiffGroupSecurity:
		return e.diffKeyed(name, securityMap(from.Spec.Security), securityMap(to.Spec.Security), opts, budget)
	case domain.DiffGroupCompose:
		return e.diffKeyed(name, composeMap(from.Spec.Compose), composeMap(to.Spec.Compose), opts, budget)
	case domain.DiffGroupMetadata:
		return e.diffKeyed(name, metadataMap(from.Spec), metadataMap(to.Spec), opts, budget)
	default:
		return domain.DiffGroup{Name: name, Entries: []domain.DiffEntry{}}
	}
}

// diffKeyed compares two key/value maps.
func (e *DiffEngine) diffKeyed(
	name domain.DiffGroupName,
	from, to map[string]string,
	opts DiffOptions,
	budget *int,
) domain.DiffGroup {
	group := domain.DiffGroup{Name: name, Entries: []domain.DiffEntry{}}

	keys := unionKeys(from, to)
	if len(keys) > e.cfg.MaxGroupEntries {
		keys = keys[:e.cfg.MaxGroupEntries]
		group.Truncated = true
	}

	for _, key := range keys {
		oldValue, inFrom := from[key]
		newValue, inTo := to[key]

		var entry domain.DiffEntry
		switch {
		case inFrom && !inTo:
			entry = domain.DiffEntry{Key: key, Kind: domain.ChangeRemoved, Old: truncateValue(oldValue)}
			group.Removed++
		case !inFrom && inTo:
			entry = domain.DiffEntry{Key: key, Kind: domain.ChangeAdded, New: truncateValue(newValue)}
			group.Added++
		case oldValue != newValue:
			entry = domain.DiffEntry{
				Key: key, Kind: domain.ChangeModified,
				Old: truncateValue(oldValue), New: truncateValue(newValue),
			}
			group.Modified++
		default:
			group.Unchanged++
			if !opts.IncludeUnchanged {
				continue
			}
			entry = domain.DiffEntry{Key: key, Kind: domain.ChangeUnchanged, Old: truncateValue(oldValue)}
		}

		group.Total++
		if *budget <= 0 {
			group.Truncated = true
			continue
		}
		group.Entries = append(group.Entries, entry)
		group.Returned++
		*budget--
	}
	return group
}

// diffEnvironment compares environment entries.
//
// Separate from diffKeyed because a sensitive variable is compared by digest
// and reports only THAT it changed, never what it changed to.
func (e *DiffEngine) diffEnvironment(
	from, to DiffInput,
	opts DiffOptions,
	budget *int,
) domain.DiffGroup {
	group := domain.DiffGroup{Name: domain.DiffGroupEnvironment, Entries: []domain.DiffEntry{}}

	fromEnv := envEntryMap(from.Env, from.Spec)
	toEnv := envEntryMap(to.Env, to.Spec)

	keys := unionKeys(fromEnv, toEnv)
	if len(keys) > e.cfg.MaxGroupEntries {
		keys = keys[:e.cfg.MaxGroupEntries]
		group.Truncated = true
	}

	for _, key := range keys {
		oldEntry, inFrom := fromEnv[key]
		newEntry, inTo := toEnv[key]

		sensitive := (inFrom && oldEntry.Sensitive()) || (inTo && newEntry.Sensitive())

		entry := domain.DiffEntry{Key: key, Sensitive: sensitive}
		switch {
		case inFrom && !inTo:
			entry.Kind = domain.ChangeRemoved
			if !sensitive {
				entry.Old = truncateValue(oldEntry.Value)
			}
			group.Removed++

		case !inFrom && inTo:
			entry.Kind = domain.ChangeAdded
			if !sensitive {
				entry.New = truncateValue(newEntry.Value)
			}
			group.Added++

		case sensitive:
			oldDigest := oldEntry.SecretDigest()
			newDigest := newEntry.SecretDigest()
			switch {
			case !oldDigest.Comparable(newDigest):
				// Different HMAC keys, or a digest missing entirely. Reporting
				// "modified" here would tell an operator every secret changed
				// after a key rotation.
				entry.Kind = domain.ChangeUnverifiable
				entry.Note = "the two digests were produced under different keys and cannot be compared"
			case oldDigest.Equal(newDigest):
				group.Unchanged++
				if !opts.IncludeUnchanged {
					continue
				}
				entry.Kind = domain.ChangeUnchanged
			default:
				entry.Kind = domain.ChangeModified
				entry.Note = "the value changed; HarborMaster does not store secret values"
				group.Modified++
			}

		case oldEntry.Value != newEntry.Value:
			entry.Kind = domain.ChangeModified
			entry.Old = truncateValue(oldEntry.Value)
			entry.New = truncateValue(newEntry.Value)
			group.Modified++

		default:
			group.Unchanged++
			if !opts.IncludeUnchanged {
				continue
			}
			entry.Kind = domain.ChangeUnchanged
			entry.Old = truncateValue(oldEntry.Value)
		}

		group.Total++
		if *budget <= 0 {
			group.Truncated = true
			continue
		}
		group.Entries = append(group.Entries, entry)
		group.Returned++
		*budget--
	}
	return group
}

// envEntryMap keys environment entries by name.
//
// Prefers the relational rows, which carry the digests, and falls back to the
// document for a comparison against live configuration where no rows exist.
func envEntryMap(rows []domain.SnapshotEnvEntry, spec domain.SnapshotSpec) map[string]domain.SnapshotEnvEntry {
	out := make(map[string]domain.SnapshotEnvEntry, len(rows))
	for _, row := range rows {
		out[row.Key] = row
	}
	if len(out) > 0 {
		return out
	}

	for _, entry := range spec.Environment {
		out[entry.Name] = domain.SnapshotEnvEntry{
			Key:             entry.Name,
			Classification:  entry.Sensitivity,
			Present:         entry.Present,
			Value:           entry.Value,
			Length:          entry.Length,
			Digest:          entry.Digest,
			DigestAlgorithm: entry.DigestAlgorithm,
			DigestKeyID:     entry.DigestKeyID,
		}
	}
	return out
}

// --- keyed projections ------------------------------------------------------
//
// Each renders one configuration section as a flat key/value map. Flattening
// here is what makes every comparison a single pass over two maps, and what
// makes ordering noise disappear: a map has no order to differ in.

func labelMap(labels []domain.Label) map[string]string {
	out := make(map[string]string, len(labels))
	for _, label := range labels {
		out[label.Key] = label.Value
	}
	return out
}

func portMap(ports []domain.Port) map[string]string {
	out := make(map[string]string, len(ports))
	for _, port := range ports {
		key := strconv.Itoa(int(port.ContainerPort)) + "/" + port.Protocol
		value := "exposed"
		if port.Published {
			value = "published " + port.HostIP + ":" + strconv.Itoa(int(port.HostPort))
		}
		out[key] = value
	}
	return out
}

func networkMap(networks []domain.SpecNetwork) map[string]string {
	out := make(map[string]string, len(networks))
	for _, network := range networks {
		aliases := append([]string(nil), network.Aliases...)
		sort.Strings(aliases)
		out[network.NetworkName] = "aliases=" + strings.Join(aliases, ",")
	}
	return out
}

func mountMap(mounts []domain.SpecMount) map[string]string {
	out := make(map[string]string, len(mounts))
	for _, mount := range mounts {
		value := string(mount.Type) + " source=" + mount.Source
		if mount.VolumeName != "" {
			value = string(mount.Type) + " volume=" + mount.VolumeName
		}
		if mount.ReadOnly {
			value += " ro"
		}
		out[mount.Destination] = value
	}
	return out
}

func resourceMap(r domain.Resources) map[string]string {
	out := map[string]string{
		"cpuShares":         strconv.FormatInt(r.CPUShares, 10),
		"cpuQuota":          strconv.FormatInt(r.CPUQuota, 10),
		"cpuPeriod":         strconv.FormatInt(r.CPUPeriod, 10),
		"nanoCpus":          strconv.FormatInt(r.NanoCPUs, 10),
		"cpusetCpus":        r.CpusetCPUs,
		"cpusetMems":        r.CpusetMems,
		"memoryBytes":       strconv.FormatInt(r.MemoryBytes, 10),
		"memoryReservation": strconv.FormatInt(r.MemoryReservationBytes, 10),
		"memorySwap":        strconv.FormatInt(r.MemorySwapBytes, 10),
		"blkioWeight":       strconv.FormatUint(uint64(r.BlkioWeight), 10),
		"shmSizeBytes":      strconv.FormatInt(r.ShmSizeBytes, 10),
		"oomScoreAdj":       strconv.Itoa(r.OomScoreAdj),
	}
	// Nil and zero are different configurations, so an unset pointer is absent
	// from the map rather than rendered as "0".
	if r.PidsLimit != nil {
		out["pidsLimit"] = strconv.FormatInt(*r.PidsLimit, 10)
	}
	if r.MemorySwappiness != nil {
		out["memorySwappiness"] = strconv.FormatInt(*r.MemorySwappiness, 10)
	}
	if r.OomKillDisable != nil {
		out["oomKillDisable"] = strconv.FormatBool(*r.OomKillDisable)
	}
	for _, ulimit := range r.Ulimits {
		out["ulimit."+ulimit.Name] = strconv.FormatInt(ulimit.Soft, 10) + ":" + strconv.FormatInt(ulimit.Hard, 10)
	}
	return out
}

func securityMap(s domain.Security) map[string]string {
	out := map[string]string{
		"privileged":      strconv.FormatBool(s.Privileged),
		"readonlyRootfs":  strconv.FormatBool(s.ReadonlyRootfs),
		"noNewPrivileges": strconv.FormatBool(s.NoNewPrivileges),
		"apparmorProfile": s.AppArmorProfile,
		"selinuxLabel":    s.SELinuxLabel,
		"seccompProfile":  s.SeccompProfile,
		"ipcMode":         s.IPCMode,
		"pidMode":         s.PIDMode,
		"utsMode":         s.UTSMode,
		"usernsMode":      s.UsernsMode,
		"cgroupnsMode":    s.CgroupnsMode,
		// Sorted, so a reordering is not a change in posture.
		"capAdd":      sortedJoin(s.CapAdd),
		"capDrop":     sortedJoin(s.CapDrop),
		"securityOpt": sortedJoin(s.SecurityOpt),
		"groupAdd":    sortedJoin(s.GroupAdd),
	}
	for key, value := range s.Sysctls {
		out["sysctl."+key] = value
	}
	for _, device := range s.Devices {
		out["device."+device.PathInContainer] = device.PathOnHost + ":" + device.CgroupPermissions
	}
	return out
}

func composeMap(c domain.ComposeMetadata) map[string]string {
	return map[string]string{
		"managed":         strconv.FormatBool(c.Managed),
		"project":         c.Project,
		"service":         c.Service,
		"containerNumber": strconv.Itoa(c.ContainerNumber),
		"workingDir":      c.WorkingDir,
		"configFiles":     c.ConfigFiles,
		"oneOff":          strconv.FormatBool(c.OneOff),
	}
}

// metadataMap covers identity, image, process, restart policy, healthcheck, and
// logging: the fields that belong to no larger section.
func metadataMap(spec domain.SnapshotSpec) map[string]string {
	out := map[string]string{
		"containerName":          spec.Identity.ContainerName,
		"image.reference":        spec.Image.Reference,
		"image.digest":           spec.Image.Digest,
		"image.id":               spec.Image.ImageID,
		"process.command":        strings.Join(spec.Process.Command, " "),
		"process.entrypoint":     strings.Join(spec.Process.Entrypoint, " "),
		"process.user":           spec.Process.User,
		"process.workingDir":     spec.Process.WorkingDir,
		"process.hostname":       spec.Process.Hostname,
		"process.domainname":     spec.Process.Domainname,
		"process.stopSignal":     spec.Process.StopSignal,
		"process.tty":            strconv.FormatBool(spec.Process.TTY),
		"restartPolicy":          spec.RestartPolicy.Name,
		"restartPolicy.maxRetry": strconv.Itoa(spec.RestartPolicy.MaximumRetryCount),
		"logging.driver":         spec.Logging.Driver,
	}

	if check := spec.HealthCheck; check != nil {
		out["healthCheck.test"] = strings.Join(check.Test, " ")
		out["healthCheck.intervalMs"] = strconv.FormatInt(check.IntervalMS, 10)
		out["healthCheck.timeoutMs"] = strconv.FormatInt(check.TimeoutMS, 10)
		out["healthCheck.retries"] = strconv.Itoa(check.Retries)
		out["healthCheck.disabled"] = strconv.FormatBool(check.Disabled)
	}

	// Log options follow the same masking rule as the environment: a sensitive
	// option's value never appears.
	for _, option := range spec.Logging.Options {
		if option.Sensitive() {
			continue
		}
		out["logging.option."+option.Name] = option.Value
	}
	return out
}

func sortedJoin(values []string) string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

// unionKeys returns every key in either map, sorted so output is deterministic.
func unionKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	keys := make([]string, 0, len(a)+len(b))

	for key := range a {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range b {
		if _, ok := seen[key]; ok {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// truncateValue bounds one rendered value.
//
// Explicit, so a reader can tell a truncated value from a short one.
func truncateValue(value string) string {
	if len(value) <= maxDiffValueBytes {
		return value
	}
	return value[:maxDiffValueBytes] + fmt.Sprintf("… (truncated, %d bytes total)", len(value))
}
