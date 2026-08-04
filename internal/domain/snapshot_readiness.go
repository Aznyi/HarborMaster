package domain

import "time"

// ReadinessStatus is the verdict of one check or of a whole report.
type ReadinessStatus string

// Readiness statuses.
const (
	// ReadinessUnknown means no evaluation has run. It is the stored default on
	// a freshly captured snapshot and a valid filter value; it is never
	// produced BY an evaluation.
	ReadinessUnknown ReadinessStatus = "unknown"

	// ReadinessReady means the check established that restoration would
	// succeed on this point.
	ReadinessReady ReadinessStatus = "ready"

	// ReadinessWarning means restoration would probably succeed but something
	// needs an operator's attention.
	ReadinessWarning ReadinessStatus = "warning"

	// ReadinessNotReady means restoration would fail.
	ReadinessNotReady ReadinessStatus = "not_ready"

	// ReadinessUnverifiable means HarborMaster could not establish the answer
	// at all.
	//
	// Distinct from a warning, and load-bearing: it caps the overall verdict at
	// ReadinessWarning, so a report can never claim "ready" on the strength of
	// something HarborMaster did not actually check. This is the fail-closed
	// rule applied to readiness.
	ReadinessUnverifiable ReadinessStatus = "unverifiable"
)

// ReadinessStatuses lists the values a REPORT may carry, for filter validation.
// ReadinessUnverifiable is a per-check outcome only: it degrades the overall
// verdict rather than becoming it.
var ReadinessStatuses = []ReadinessStatus{
	ReadinessUnknown, ReadinessReady, ReadinessWarning, ReadinessNotReady,
}

// ValidReadinessStatus reports whether s names a status a report may carry.
func ValidReadinessStatus(s string) bool {
	for _, status := range ReadinessStatuses {
		if string(status) == s {
			return true
		}
	}
	return false
}

// ReadinessCheckID identifies one restore-readiness check.
type ReadinessCheckID string

// The readiness checks.
const (
	CheckDaemonReachable    ReadinessCheckID = "daemon_reachable"
	CheckInventoryFresh     ReadinessCheckID = "inventory_fresh"
	CheckImageAvailable     ReadinessCheckID = "image_available"
	CheckImageDigestKnown   ReadinessCheckID = "image_digest_known"
	CheckNamedVolumes       ReadinessCheckID = "named_volumes_present"
	CheckMountSources       ReadinessCheckID = "mount_sources"
	CheckNetworksPresent    ReadinessCheckID = "networks_present"
	CheckRestartPolicyValid ReadinessCheckID = "restart_policy_valid"
	CheckComposeMetadata    ReadinessCheckID = "compose_metadata_complete"
	CheckSecretsAvailable   ReadinessCheckID = "secrets_available"
	CheckConfigConsistent   ReadinessCheckID = "config_consistent"
	CheckRuntimeFeatures    ReadinessCheckID = "runtime_features"
)

// ReadinessCheckIDs lists every check, in report order.
var ReadinessCheckIDs = []ReadinessCheckID{
	CheckDaemonReachable,
	CheckInventoryFresh,
	CheckImageAvailable,
	CheckImageDigestKnown,
	CheckNamedVolumes,
	CheckMountSources,
	CheckNetworksPresent,
	CheckRestartPolicyValid,
	CheckComposeMetadata,
	CheckSecretsAvailable,
	CheckConfigConsistent,
	CheckRuntimeFeatures,
}

// ReadinessCheck is one check's outcome.
//
// Detail is operator-facing prose. It never contains a secret value, a raw
// daemon error, or a path the snapshot does not already record.
type ReadinessCheck struct {
	ID     ReadinessCheckID `json:"id"`
	Status ReadinessStatus  `json:"status"`
	Detail string           `json:"detail,omitempty"`
}

// ReadinessReport is a complete restore-readiness evaluation.
//
// # It is informational
//
// HarborMaster cannot restore a container and this report does not imply
// otherwise. It answers "could restoration succeed", so that the future phase
// that can restore has an honest baseline.
//
// # Provenance
//
// Most checks answer from HarborMaster's inventory rather than from the daemon,
// so the report carries how current that inventory was. A verdict derived from
// a six-hour-old reading states history as though it were fact, and the
// freshness fields are what let a reader tell the difference.
type ReadinessReport struct {
	SnapshotID int64            `json:"snapshotId"`
	Status     ReadinessStatus  `json:"status"`
	Checks     []ReadinessCheck `json:"checks"`

	// EvaluatedAt is when this report was produced.
	EvaluatedAt time.Time `json:"evaluatedAt"`
	// DaemonCheckedAt is when the reachability ping in this evaluation ran.
	DaemonCheckedAt *time.Time `json:"daemonCheckedAt,omitempty"`

	// InventoryGeneration and InventoryCompletedAt describe the inventory the
	// evaluation read; InventoryAgeSeconds is its age at evaluation time.
	InventoryGeneration  int64      `json:"inventoryGeneration"`
	InventoryCompletedAt *time.Time `json:"inventoryCompletedAt,omitempty"`
	InventoryAgeSeconds  int64      `json:"inventoryAgeSeconds"`
	// InventoryStale reports that the age exceeded the configured threshold,
	// which caps Status at ReadinessWarning.
	InventoryStale bool `json:"inventoryStale"`

	// Summary counts, so a UI can render a distribution without walking Checks.
	ReadyCount        int `json:"readyCount"`
	WarningCount      int `json:"warningCount"`
	NotReadyCount     int `json:"notReadyCount"`
	UnverifiableCount int `json:"unverifiableCount"`
}

// WorstStatus folds a set of check statuses into an overall verdict.
//
// The fold is deliberately pessimistic:
//
//   - not_ready dominates everything.
//   - unverifiable caps the result at warning. HarborMaster does not claim
//     "ready" on a fact it failed to establish.
//   - an empty set is not_ready: a report with no checks has established
//     nothing, and reporting "ready" for it would be the worst possible
//     default.
func WorstStatus(statuses []ReadinessStatus) ReadinessStatus {
	if len(statuses) == 0 {
		return ReadinessNotReady
	}

	worst := ReadinessReady
	for _, status := range statuses {
		switch status {
		case ReadinessNotReady:
			return ReadinessNotReady
		case ReadinessUnverifiable, ReadinessWarning:
			worst = ReadinessWarning
		}
	}
	return worst
}

// HostPathStatus is what a HostValidationProvider concluded about a path.
type HostPathStatus string

// Host path statuses.
const (
	HostPathExists HostPathStatus = "exists"
	HostPathAbsent HostPathStatus = "absent"
	// HostPathUnverifiable is the only value Phase 3 ever produces: no
	// filesystem is inspected at all.
	HostPathUnverifiable HostPathStatus = "unverifiable"
)

// HostPathResult is a host filesystem answer.
type HostPathResult struct {
	Status HostPathStatus `json:"status"`
	// Detail explains an unverifiable result to an operator.
	Detail string `json:"detail,omitempty"`
}
