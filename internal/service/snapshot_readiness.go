package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// HostValidationProvider answers questions about the host filesystem.
//
// Phase 3 ships only nullHostValidation, which answers "unverifiable" to
// everything and touches no filesystem at all.
//
// That is a deliberate refusal rather than a missing feature. Calling stat on a
// path that originated in container configuration is exactly the path-traversal
// shape the security requirements forbid: the string is attacker-influenced
// (anyone who can create a container chooses it), and HarborMaster runs with a
// privileged socket. Nor would it work -- HarborMaster in a container cannot see
// host paths, so the answer would be a confident, wrong "absent".
//
// The interface exists so a later phase can add a local, SSH, or agent-backed
// provider behind explicit configuration without reshaping the snapshot schema
// or the readiness API.
type HostValidationProvider interface {
	PathExists(ctx context.Context, path string) (domain.HostPathResult, error)
}

// nullHostValidation is the Phase 3 provider. It inspects nothing.
type nullHostValidation struct{}

func (nullHostValidation) PathExists(context.Context, string) (domain.HostPathResult, error) {
	return domain.HostPathResult{
		Status: domain.HostPathUnverifiable,
		Detail: "HarborMaster does not inspect the host filesystem",
	}, nil
}

// NewNullHostValidation returns the provider Phase 3 uses.
func NewNullHostValidation() HostValidationProvider { return nullHostValidation{} }

// ReadinessInventory is the inventory state a readiness evaluation reads.
//
// A narrow interface rather than the concrete repositories, so the engine can
// be tested against fixtures with no database.
type ReadinessInventory interface {
	// CurrentGeneration reports the newest committed inventory generation.
	CurrentGeneration(ctx context.Context) (int64, string, error)
	// LastSuccessfulRefresh returns the most recent SUCCESSFUL refresh, which
	// is what inventory age must be measured from. A failed attempt does not
	// make the data any fresher.
	LastSuccessfulRefresh(ctx context.Context) (*domain.RefreshRecord, error)
	// ImageExists reports whether an image is still present locally.
	ImageExists(ctx context.Context, imageID, reference string) (bool, error)
	// NetworkNames and VolumeNames return the whole current set, so a
	// container with several attachments costs one query rather than several.
	NetworkNames(ctx context.Context) (map[string]struct{}, error)
	VolumeNames(ctx context.Context) (map[string]struct{}, error)
}

// ReadinessPinger reports whether the container runtime is reachable.
type ReadinessPinger interface {
	CheckRuntime(ctx context.Context) error
}

// ReadinessEngine evaluates whether a snapshot could be restored.
//
// It does NOT restore anything, and Phase 3 has no code that could. The report
// exists so that the future phase which can restore has an honest baseline, and
// so an operator can see today whether that baseline is complete.
type ReadinessEngine struct {
	inventory ReadinessInventory
	pinger    ReadinessPinger
	host      HostValidationProvider
	cfg       config.Snapshots
	now       func() time.Time
}

// ReadinessOptions configures a ReadinessEngine.
type ReadinessOptions struct {
	Inventory ReadinessInventory
	Pinger    ReadinessPinger
	Host      HostValidationProvider
	Config    config.Snapshots
	Now       func() time.Time
}

// NewReadinessEngine builds a ReadinessEngine.
func NewReadinessEngine(opts ReadinessOptions) *ReadinessEngine {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	host := opts.Host
	if host == nil {
		host = NewNullHostValidation()
	}
	if opts.Config.MaxInventoryAge <= 0 {
		opts.Config.MaxInventoryAge = config.DefaultSnapshotMaxInventoryAge
	}

	return &ReadinessEngine{
		inventory: opts.Inventory,
		pinger:    opts.Pinger,
		host:      host,
		cfg:       opts.Config,
		now:       now,
	}
}

// Evaluate produces a readiness report for a snapshot.
//
// Evaluation NEVER triggers an inventory refresh. It reports that the inventory
// is stale and leaves refreshing to the operator or the scheduler, because a
// read endpoint that can drive privileged socket traffic is an amplifier: one
// request would become a full host sweep, repeatable at will by anyone who can
// reach the API.
func (e *ReadinessEngine) Evaluate(
	ctx context.Context,
	snapshot domain.Snapshot,
	env []domain.SnapshotEnvEntry,
	mounts []domain.SnapshotMountRow,
	networks []domain.SnapshotNetworkRow,
) (domain.ReadinessReport, error) {
	report := domain.ReadinessReport{
		SnapshotID:  snapshot.ID,
		EvaluatedAt: e.now(),
		Checks:      make([]domain.ReadinessCheck, 0, len(domain.ReadinessCheckIDs)),
	}

	var spec domain.SnapshotSpec
	if len(snapshot.SpecJSON) > 0 {
		// A document that will not decode is a broken snapshot, not a reason to
		// fail the request: the report says so and every dependent check
		// degrades.
		_ = json.Unmarshal(snapshot.SpecJSON, &spec)
	}

	add := func(id domain.ReadinessCheckID, status domain.ReadinessStatus, detail string) {
		report.Checks = append(report.Checks, domain.ReadinessCheck{ID: id, Status: status, Detail: detail})
	}

	// --- daemon reachability -------------------------------------------------
	daemonCheckedAt := e.now()
	report.DaemonCheckedAt = &daemonCheckedAt
	if e.pinger == nil {
		add(domain.CheckDaemonReachable, domain.ReadinessUnverifiable,
			"no runtime is configured, so reachability could not be established")
	} else if err := e.pinger.CheckRuntime(ctx); err != nil {
		// The raw error can name the socket path, so it never reaches the
		// report.
		add(domain.CheckDaemonReachable, domain.ReadinessNotReady,
			"the container runtime is unreachable")
	} else {
		add(domain.CheckDaemonReachable, domain.ReadinessReady, "the container runtime responded")
	}

	// --- inventory freshness -------------------------------------------------
	e.evaluateFreshness(ctx, &report, add)

	// --- image ---------------------------------------------------------------
	e.evaluateImage(ctx, &report, spec, snapshot, add)

	// --- volumes and mounts --------------------------------------------------
	e.evaluateStorage(ctx, mounts, add)

	// --- networks ------------------------------------------------------------
	e.evaluateNetworks(ctx, networks, add)

	// --- configuration -------------------------------------------------------
	evaluateRestartPolicy(spec, add)
	evaluateCompose(spec, add)
	evaluateSecrets(env, add)
	evaluateConsistency(spec, snapshot, mounts, add)
	evaluateRuntimeFeatures(spec, add)

	e.summarise(&report)
	return report, nil
}

// evaluateFreshness records how current the inventory was.
//
// Nine of the twelve checks answer from the inventory rather than from the
// daemon, so a "ready" derived from a six-hour-old reading is stating history
// as though it were fact.
func (e *ReadinessEngine) evaluateFreshness(
	ctx context.Context,
	report *domain.ReadinessReport,
	add func(domain.ReadinessCheckID, domain.ReadinessStatus, string),
) {
	if e.inventory == nil {
		add(domain.CheckInventoryFresh, domain.ReadinessUnverifiable,
			"no inventory is configured")
		return
	}

	generation, _, err := e.inventory.CurrentGeneration(ctx)
	if err == nil {
		report.InventoryGeneration = generation
	}

	last, err := e.inventory.LastSuccessfulRefresh(ctx)
	if err != nil || last == nil || last.FinishedAt == nil {
		// No successful refresh has ever completed, so there is no basis for
		// any inventory-derived check at all.
		add(domain.CheckInventoryFresh, domain.ReadinessNotReady,
			"no successful inventory refresh has completed, so nothing can be verified against it")
		report.InventoryStale = true
		return
	}

	completedAt := last.FinishedAt.UTC()
	report.InventoryCompletedAt = &completedAt

	// Measured from the last SUCCESS, not the last attempt: a failed refresh
	// does not make the data any fresher.
	age := e.now().Sub(completedAt)
	if age < 0 {
		age = 0
	}
	report.InventoryAgeSeconds = int64(age.Seconds())

	if age > e.cfg.MaxInventoryAge {
		report.InventoryStale = true
		add(domain.CheckInventoryFresh, domain.ReadinessWarning, fmt.Sprintf(
			"the inventory is %s old, beyond the %s freshness threshold; refresh before relying on this report",
			age.Round(time.Second), e.cfg.MaxInventoryAge))
		return
	}
	add(domain.CheckInventoryFresh, domain.ReadinessReady, fmt.Sprintf(
		"the inventory is %s old", age.Round(time.Second)))
}

func (e *ReadinessEngine) evaluateImage(
	ctx context.Context,
	report *domain.ReadinessReport,
	spec domain.SnapshotSpec,
	snapshot domain.Snapshot,
	add func(domain.ReadinessCheckID, domain.ReadinessStatus, string),
) {
	_ = report

	reference := spec.Image.Reference
	if reference == "" {
		reference = snapshot.ImageReference
	}
	imageID := spec.Image.ImageID
	if imageID == "" {
		imageID = snapshot.ImageID
	}

	switch {
	case e.inventory == nil:
		add(domain.CheckImageAvailable, domain.ReadinessUnverifiable, "no inventory is configured")
	case imageID == "" && reference == "":
		add(domain.CheckImageAvailable, domain.ReadinessNotReady,
			"the snapshot records no image reference or ID")
	default:
		present, err := e.inventory.ImageExists(ctx, imageID, reference)
		switch {
		case err != nil:
			add(domain.CheckImageAvailable, domain.ReadinessUnverifiable,
				"the image could not be looked up in the inventory")
		case present:
			add(domain.CheckImageAvailable, domain.ReadinessReady,
				"the image is present locally")
		default:
			// Not "not_ready": a missing image is recoverable by pulling it,
			// which a future phase would do. It is an obstacle, not a wall.
			add(domain.CheckImageAvailable, domain.ReadinessWarning,
				"the image is not present locally and would have to be pulled")
		}
	}

	digest := spec.Image.Digest
	if digest == "" {
		digest = snapshot.ImageDigest
	}
	switch {
	case digest != "" || len(spec.Image.RepoDigests) > 0:
		add(domain.CheckImageDigestKnown, domain.ReadinessReady,
			"the image is identified by digest")
	default:
		add(domain.CheckImageDigestKnown, domain.ReadinessWarning,
			"no image digest was recorded, so the restore target is a mutable tag rather than exact content")
	}
}

func (e *ReadinessEngine) evaluateStorage(
	ctx context.Context,
	mounts []domain.SnapshotMountRow,
	add func(domain.ReadinessCheckID, domain.ReadinessStatus, string),
) {
	var (
		named       []string
		binds       int
		tmpfs       int
		missing     []string
		lookupError bool
	)

	volumes := map[string]struct{}{}
	if e.inventory != nil {
		found, err := e.inventory.VolumeNames(ctx)
		if err != nil {
			lookupError = true
		} else {
			volumes = found
		}
	} else {
		lookupError = true
	}

	for _, mount := range mounts {
		switch mount.Type {
		case domain.MountTypeVolume:
			if mount.VolumeName == "" {
				// An anonymous volume is recreated rather than reattached.
				continue
			}
			named = append(named, mount.VolumeName)
			if _, present := volumes[mount.VolumeName]; !present && !lookupError {
				missing = append(missing, mount.VolumeName)
			}
		case domain.MountTypeBind:
			binds++
		case domain.MountTypeTmpfs:
			tmpfs++
		}
	}

	switch {
	case len(named) == 0:
		add(domain.CheckNamedVolumes, domain.ReadinessReady, "the container uses no named volumes")
	case lookupError:
		add(domain.CheckNamedVolumes, domain.ReadinessUnverifiable,
			"named volumes could not be checked against the inventory")
	case len(missing) > 0:
		add(domain.CheckNamedVolumes, domain.ReadinessNotReady, fmt.Sprintf(
			"%d of %d named volumes are missing: %s",
			len(missing), len(named), joinLimited(missing, 5)))
	default:
		add(domain.CheckNamedVolumes, domain.ReadinessReady, fmt.Sprintf(
			"all %d named volumes are present", len(named)))
	}

	// Bind sources are never checked. See HostValidationProvider.
	switch {
	case binds == 0:
		add(domain.CheckMountSources, domain.ReadinessReady, "the container uses no bind mounts")
	default:
		result, _ := e.host.PathExists(ctx, "")
		add(domain.CheckMountSources, domain.ReadinessUnverifiable, fmt.Sprintf(
			"%d bind mount source(s) cannot be verified: %s", binds, result.Detail))
	}
}

func (e *ReadinessEngine) evaluateNetworks(
	ctx context.Context,
	networks []domain.SnapshotNetworkRow,
	add func(domain.ReadinessCheckID, domain.ReadinessStatus, string),
) {
	if len(networks) == 0 {
		add(domain.CheckNetworksPresent, domain.ReadinessReady, "the container declares no network attachments")
		return
	}
	if e.inventory == nil {
		add(domain.CheckNetworksPresent, domain.ReadinessUnverifiable, "no inventory is configured")
		return
	}

	present, err := e.inventory.NetworkNames(ctx)
	if err != nil {
		add(domain.CheckNetworksPresent, domain.ReadinessUnverifiable,
			"networks could not be checked against the inventory")
		return
	}

	var missing []string
	for _, network := range networks {
		// The predefined networks always exist on a working daemon.
		switch network.NetworkName {
		case "bridge", "host", "none":
			continue
		}
		if _, found := present[network.NetworkName]; !found {
			missing = append(missing, network.NetworkName)
		}
	}

	if len(missing) > 0 {
		add(domain.CheckNetworksPresent, domain.ReadinessNotReady, fmt.Sprintf(
			"%d network(s) are missing: %s", len(missing), joinLimited(missing, 5)))
		return
	}
	add(domain.CheckNetworksPresent, domain.ReadinessReady, fmt.Sprintf(
		"all %d network attachments resolve", len(networks)))
}

// validRestartPolicies is the closed set Docker accepts.
var validRestartPolicies = map[string]struct{}{
	"":               {},
	"no":             {},
	"always":         {},
	"on-failure":     {},
	"unless-stopped": {},
}

func evaluateRestartPolicy(spec domain.SnapshotSpec, add func(domain.ReadinessCheckID, domain.ReadinessStatus, string)) {
	policy := spec.RestartPolicy
	if _, ok := validRestartPolicies[policy.Name]; !ok {
		add(domain.CheckRestartPolicyValid, domain.ReadinessNotReady,
			"the recorded restart policy is not one Docker accepts")
		return
	}
	// A retry count only means anything for on-failure, and a non-zero one
	// elsewhere is a configuration HarborMaster would not be able to reproduce
	// faithfully.
	if policy.Name != "on-failure" && policy.MaximumRetryCount != 0 {
		add(domain.CheckRestartPolicyValid, domain.ReadinessWarning,
			"a maximum retry count is set on a policy that does not use one")
		return
	}
	add(domain.CheckRestartPolicyValid, domain.ReadinessReady, "the restart policy is valid")
}

func evaluateCompose(spec domain.SnapshotSpec, add func(domain.ReadinessCheckID, domain.ReadinessStatus, string)) {
	compose := spec.Compose
	if !compose.Managed {
		add(domain.CheckComposeMetadata, domain.ReadinessReady, "the container is not Compose-managed")
		return
	}

	var missing []string
	if compose.Project == "" {
		missing = append(missing, "project")
	}
	if compose.Service == "" {
		missing = append(missing, "service")
	}
	if compose.ConfigFiles == "" {
		missing = append(missing, "config files")
	}

	if len(missing) > 0 {
		add(domain.CheckComposeMetadata, domain.ReadinessWarning, fmt.Sprintf(
			"Compose metadata is incomplete, missing: %s", joinLimited(missing, 5)))
		return
	}
	add(domain.CheckComposeMetadata, domain.ReadinessReady, "Compose metadata is complete")
}

// evaluateSecrets reports the gap HarborMaster deliberately created.
//
// Because no sensitive value is ever stored, a container that depends on one
// cannot be recreated from a snapshot alone. This check exists so that fact is
// visible in the product rather than discovered during an incident.
func evaluateSecrets(env []domain.SnapshotEnvEntry, add func(domain.ReadinessCheckID, domain.ReadinessStatus, string)) {
	sensitive := 0
	for _, entry := range env {
		if entry.Sensitive() {
			sensitive++
		}
	}

	if sensitive == 0 {
		add(domain.CheckSecretsAvailable, domain.ReadinessReady,
			"the container declares no sensitive values")
		return
	}
	// Never "ready", and never "not_ready" either: restoration is possible, but
	// only with values HarborMaster does not hold and will not store.
	add(domain.CheckSecretsAvailable, domain.ReadinessWarning, fmt.Sprintf(
		"%d sensitive value(s) are recorded by digest only and must be supplied externally at restore time; "+
			"HarborMaster never stores secret values", sensitive))
}

func evaluateConsistency(
	spec domain.SnapshotSpec,
	snapshot domain.Snapshot,
	mounts []domain.SnapshotMountRow,
	add func(domain.ReadinessCheckID, domain.ReadinessStatus, string),
) {
	var problems []string

	if len(snapshot.SpecJSON) == 0 {
		problems = append(problems, "the snapshot has no configuration document")
	} else if spec.SpecVersion == 0 {
		problems = append(problems, "the configuration document could not be decoded")
	}
	if spec.SpecVersion > domain.SnapshotSpecVersion {
		problems = append(problems, "the document was written by a newer HarborMaster")
	}
	if spec.Identity.ContainerName == "" && snapshot.ContainerName == "" {
		problems = append(problems, "no container name is recorded")
	}

	for _, mount := range mounts {
		if mount.Destination == "" {
			problems = append(problems, "a mount has no destination")
			break
		}
	}
	for _, port := range spec.Ports {
		if port.Published && port.HostPort == 0 {
			problems = append(problems, "a port is marked published but records no host port")
			break
		}
	}
	// Memory below the reservation is a configuration the daemon would refuse.
	if spec.Resources.MemoryBytes > 0 &&
		spec.Resources.MemoryReservationBytes > spec.Resources.MemoryBytes {
		problems = append(problems, "the memory reservation exceeds the memory limit")
	}

	if len(problems) > 0 {
		add(domain.CheckConfigConsistent, domain.ReadinessNotReady, joinLimited(problems, 5))
		return
	}
	add(domain.CheckConfigConsistent, domain.ReadinessReady, "the configuration is internally consistent")
}

// evaluateRuntimeFeatures flags configuration that needs daemon support beyond
// the baseline.
func evaluateRuntimeFeatures(spec domain.SnapshotSpec, add func(domain.ReadinessCheckID, domain.ReadinessStatus, string)) {
	var notable []string

	if len(spec.Security.DeviceRequests) > 0 {
		notable = append(notable, strconv.Itoa(len(spec.Security.DeviceRequests))+" device request(s), which need a device plugin such as the NVIDIA runtime")
	}
	if len(spec.Security.Devices) > 0 {
		notable = append(notable, strconv.Itoa(len(spec.Security.Devices))+" host device mapping(s)")
	}
	if spec.Security.CgroupnsMode != "" && spec.Security.CgroupnsMode != "host" && spec.Security.CgroupnsMode != "private" {
		notable = append(notable, "an unrecognised cgroup namespace mode")
	}
	if spec.Security.Privileged {
		notable = append(notable, "privileged mode, which grants the container root-equivalent access to the host")
	}
	if spec.Security.UsernsMode != "" {
		notable = append(notable, "a user namespace mode that the target daemon must also be configured for")
	}

	if len(notable) > 0 {
		add(domain.CheckRuntimeFeatures, domain.ReadinessWarning,
			"the configuration depends on runtime features that must be present on the target host: "+
				joinLimited(notable, 5))
		return
	}
	add(domain.CheckRuntimeFeatures, domain.ReadinessReady,
		"the configuration uses no special runtime features")
}

// summarise folds the checks into an overall verdict and the counts.
func (e *ReadinessEngine) summarise(report *domain.ReadinessReport) {
	statuses := make([]domain.ReadinessStatus, 0, len(report.Checks))
	for _, check := range report.Checks {
		statuses = append(statuses, check.Status)
		switch check.Status {
		case domain.ReadinessReady:
			report.ReadyCount++
		case domain.ReadinessWarning:
			report.WarningCount++
		case domain.ReadinessNotReady:
			report.NotReadyCount++
		case domain.ReadinessUnverifiable:
			report.UnverifiableCount++
		}
	}

	report.Status = domain.WorstStatus(statuses)

	// A stale inventory caps the verdict independently of the individual
	// checks: most of them read from that inventory, so their confidence is
	// bounded by its age.
	if report.InventoryStale && report.Status == domain.ReadinessReady {
		report.Status = domain.ReadinessWarning
	}
}

// joinLimited renders a bounded, comma-separated list.
//
// Bounded because these strings reach an API response and a UI: a container
// with two hundred missing volumes must not produce a two-hundred-item error
// message.
func joinLimited(values []string, limit int) string {
	if len(values) == 0 {
		return ""
	}
	if len(values) <= limit {
		out := values[0]
		for _, value := range values[1:] {
			out += ", " + value
		}
		return out
	}

	out := values[0]
	for _, value := range values[1:limit] {
		out += ", " + value
	}
	return out + fmt.Sprintf(", and %d more", len(values)-limit)
}
