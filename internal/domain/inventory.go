package domain

import "time"

// RefreshState is the lifecycle state of an inventory refresh.
type RefreshState string

// Refresh states.
const (
	// RefreshIdle means no refresh has ever run in this process.
	RefreshIdle RefreshState = "idle"
	// RefreshRunning means a refresh is in flight.
	RefreshRunning RefreshState = "running"
	// RefreshSucceeded means the last refresh completed and was persisted.
	RefreshSucceeded RefreshState = "succeeded"
	// RefreshFailed means the last refresh could not complete. The previously
	// persisted inventory is still intact and still served.
	RefreshFailed RefreshState = "failed"
)

// RefreshTrigger records what caused a refresh.
type RefreshTrigger string

// Refresh triggers.
const (
	TriggerStartup  RefreshTrigger = "startup"
	TriggerPeriodic RefreshTrigger = "periodic"
	TriggerManual   RefreshTrigger = "manual"
)

// WarningCode classifies a non-fatal problem encountered during a refresh.
//
// A closed vocabulary, so the UI can group warnings and an operator can tell
// routine churn from a real problem without parsing prose.
type WarningCode string

// Warning codes.
const (
	// WarningContainerVanished means the container was listed but had gone by
	// the time it was inspected. This is expected churn on a busy host, not a
	// fault.
	WarningContainerVanished WarningCode = "container_vanished"
	// WarningInspectFailed means inspection failed for some other reason. The
	// container is recorded from its summary data alone.
	WarningInspectFailed WarningCode = "inspect_failed"
	// WarningImageUnavailable means the container's image could not be
	// inspected, usually because it was removed while still in use.
	WarningImageUnavailable WarningCode = "image_unavailable"
	// WarningIncompleteData means the runtime returned a record missing fields
	// HarborMaster expected.
	WarningIncompleteData WarningCode = "incomplete_data"
)

// InventoryWarning is one non-fatal problem from a refresh.
//
// Message is operator-facing prose and must never carry a raw runtime error,
// a socket path, or an environment value.
type InventoryWarning struct {
	ID            int64       `json:"id,omitempty"`
	Generation    int64       `json:"generation"`
	ContainerID   string      `json:"containerId,omitempty"`
	ContainerName string      `json:"containerName,omitempty"`
	Code          WarningCode `json:"code"`
	Message       string      `json:"message"`
	OccurredAt    time.Time   `json:"occurredAt"`
}

// RefreshRecord is the persisted outcome of one refresh attempt.
type RefreshRecord struct {
	ID         int64          `json:"id,omitempty"`
	Generation int64          `json:"generation"`
	Trigger    RefreshTrigger `json:"trigger"`
	State      RefreshState   `json:"state"`

	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMS int64      `json:"durationMs"`

	ContainersListed    int `json:"containersListed"`
	ContainersInspected int `json:"containersInspected"`
	ContainersFailed    int `json:"containersFailed"`
	ImagesInspected     int `json:"imagesInspected"`
	NetworksListed      int `json:"networksListed"`
	VolumesListed       int `json:"volumesListed"`
	WarningCount        int `json:"warningCount"`

	// Checksum is the deterministic fingerprint of the resulting inventory.
	// Empty for a failed refresh, which produces no inventory.
	Checksum string `json:"checksum,omitempty"`
	// Error is a sanitised reason for a failed refresh. Never a raw runtime
	// error.
	Error string `json:"error,omitempty"`
}

// InventoryCounts is the headline tally of a completed inventory.
type InventoryCounts struct {
	// Containers counts every container present in the last refresh.
	Containers int `json:"containers"`
	// Absent counts containers retained from an earlier refresh that the last
	// refresh no longer saw.
	Absent int `json:"absent"`

	Running    int `json:"running"`
	Stopped    int `json:"stopped"`
	Paused     int `json:"paused"`
	Restarting int `json:"restarting"`

	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`

	Images   int `json:"images"`
	Networks int `json:"networks"`
	Volumes  int `json:"volumes"`
	Warnings int `json:"warnings"`

	// ByState is the exact per-state breakdown. Stopped above is the sum of
	// exited and dead, which is what an operator means by "stopped"; ByState
	// keeps the precise numbers available without overloading that word.
	ByState map[ContainerState]int `json:"byState"`
}

// InventoryStatus is the payload of GET /api/v1/inventory.
type InventoryStatus struct {
	// Enabled reflects configuration. When false, no refresh ever runs and the
	// counts describe whatever was last persisted.
	Enabled bool   `json:"enabled"`
	Runtime string `json:"runtime"`

	// Docker reports connectivity, reusing the health endpoint's component
	// shape so a client has one thing to learn.
	Docker Component `json:"docker"`

	// State is the current refresh state, and InProgress is true only while a
	// refresh is actually running.
	State      RefreshState `json:"state"`
	InProgress bool         `json:"inProgress"`

	// Generation increments only when a refresh completes and is persisted, so
	// a client can tell whether the inventory it holds is current.
	Generation int64  `json:"generation"`
	Checksum   string `json:"checksum,omitempty"`

	LastAttempt *RefreshRecord `json:"lastAttempt,omitempty"`
	LastSuccess *RefreshRecord `json:"lastSuccess,omitempty"`

	Counts   InventoryCounts    `json:"counts"`
	Warnings []InventoryWarning `json:"warnings"`
}
