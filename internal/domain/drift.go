package domain

import "time"

// Configuration drift.
//
// # What drift is here
//
// A drift record is one field that differs between a container's BASELINE
// snapshot and its configuration right now. It is an observation, never an
// instruction: HarborMaster reports that something moved and cannot move it
// back. There is no remediation, no rollback, and no Docker mutation anywhere
// behind this model.
//
// # Where the comparison comes from
//
// Nothing in this file compares anything. Drift is produced by running the
// Phase 3 diff engine between the baseline snapshot and the container's
// current configuration, then CLASSIFYING each resulting DiffEntry. That
// separation is deliberate: a second comparison implementation would drift from
// the first, and the first is the one whose determinism the snapshot checksum
// depends on.
//
// # Secrets
//
// A record for a sensitive field carries no values, ever. The diff engine
// withholds them, this model has nowhere to put them, the repository blanks
// them again, and a CHECK constraint rejects a row that carries one. Four
// layers, because a leaked credential is the one mistake that cannot be undone.

// DriftCategory groups a drift record by the part of the configuration that
// moved.
//
// A closed vocabulary. It is finer-grained than the diff engine's groups
// because "metadata" is where the diff engine puts image identity, the process
// command, the restart policy, the health check, and logging -- five things an
// operator thinks about separately and would want to filter on separately.
type DriftCategory string

// Drift categories.
const (
	DriftCategoryImage       DriftCategory = "image"
	DriftCategoryProcess     DriftCategory = "process"
	DriftCategoryEnvironment DriftCategory = "environment"
	DriftCategoryLabels      DriftCategory = "labels"
	DriftCategoryPorts       DriftCategory = "ports"
	DriftCategoryMounts      DriftCategory = "mounts"
	DriftCategoryNetworks    DriftCategory = "networks"
	DriftCategoryRestart     DriftCategory = "restart"
	DriftCategoryHealth      DriftCategory = "health"
	DriftCategoryLogging     DriftCategory = "logging"
	DriftCategoryResources   DriftCategory = "resources"
	DriftCategorySecurity    DriftCategory = "security"
	DriftCategoryCompose     DriftCategory = "compose"
	// DriftCategoryMetadata catches identity fields that belong to no other
	// category, such as the container name.
	DriftCategoryMetadata DriftCategory = "metadata"
)

// DriftCategories lists every category, in report order: roughly most to least
// security-relevant, which is the order a dashboard should show them in.
var DriftCategories = []DriftCategory{
	DriftCategorySecurity, DriftCategoryImage, DriftCategoryMounts,
	DriftCategoryPorts, DriftCategoryNetworks, DriftCategoryProcess,
	DriftCategoryHealth, DriftCategoryEnvironment, DriftCategoryResources,
	DriftCategoryRestart, DriftCategoryLogging, DriftCategoryLabels,
	DriftCategoryCompose, DriftCategoryMetadata,
}

// ValidDriftCategory reports whether name is a known category.
func ValidDriftCategory(name string) bool {
	for _, category := range DriftCategories {
		if string(category) == name {
			return true
		}
	}
	return false
}

// DriftSeverity ranks how much a drift record matters.
type DriftSeverity string

// Drift severities.
const (
	DriftSeverityCritical DriftSeverity = "critical"
	DriftSeverityHigh     DriftSeverity = "high"
	DriftSeverityMedium   DriftSeverity = "medium"
	DriftSeverityLow      DriftSeverity = "low"
)

// DriftSeverities lists every severity, most severe first.
var DriftSeverities = []DriftSeverity{
	DriftSeverityCritical, DriftSeverityHigh, DriftSeverityMedium, DriftSeverityLow,
}

// ValidDriftSeverity reports whether name is a known severity.
func ValidDriftSeverity(name string) bool {
	for _, severity := range DriftSeverities {
		if string(severity) == name {
			return true
		}
	}
	return false
}

// Rank returns a sortable rank, higher being more severe.
//
// Used for ordering in SQL through a CASE expression built from these
// constants, never from caller input.
func (s DriftSeverity) Rank() int {
	switch s {
	case DriftSeverityCritical:
		return 4
	case DriftSeverityHigh:
		return 3
	case DriftSeverityMedium:
		return 2
	case DriftSeverityLow:
		return 1
	default:
		return 0
	}
}

// DriftStatus is the lifecycle state of a drift record.
//
// # Who owns which value
//
// This split is the model's central rule, and the API enforces it:
//
//   - ACTIVE and RESOLVED are owned by the ENGINE. They are facts about the
//     world -- the field still differs, or it no longer does -- and are set
//     only by an evaluation that observed the difference or its absence.
//   - ACKNOWLEDGED, IGNORED and EXPECTED are owned by the OPERATOR. They are
//     statements of intent about a difference that still exists.
//
// An operator therefore cannot mark a record resolved. Resolution is something
// the world does, not something a person asserts, and letting a person assert
// it would turn the drift list into a to-do list that lies.
type DriftStatus string

// Drift statuses.
const (
	// DriftStatusActive means the difference is present and unreviewed.
	DriftStatusActive DriftStatus = "active"
	// DriftStatusResolved means a later evaluation no longer saw the
	// difference. Engine-owned.
	DriftStatusResolved DriftStatus = "resolved"
	// DriftStatusAcknowledged means an operator has seen it and left it in
	// place.
	DriftStatusAcknowledged DriftStatus = "acknowledged"
	// DriftStatusIgnored means an operator does not want to be told about it.
	DriftStatusIgnored DriftStatus = "ignored"
	// DriftStatusExpected means the difference is intended -- the baseline is
	// what is stale, not the container.
	DriftStatusExpected DriftStatus = "expected"
)

// DriftStatuses lists every status.
var DriftStatuses = []DriftStatus{
	DriftStatusActive, DriftStatusResolved, DriftStatusAcknowledged,
	DriftStatusIgnored, DriftStatusExpected,
}

// OperatorDriftStatuses are the transitions an API caller may request.
//
// Deliberately excludes active and resolved: see DriftStatus. This slice is
// what the PATCH handler validates against, so widening the API surface means
// editing this line, in a diff a reviewer sees.
var OperatorDriftStatuses = []DriftStatus{
	DriftStatusAcknowledged, DriftStatusIgnored, DriftStatusExpected,
}

// ValidDriftStatus reports whether name is a known status.
func ValidDriftStatus(name string) bool {
	for _, status := range DriftStatuses {
		if string(status) == name {
			return true
		}
	}
	return false
}

// ValidOperatorDriftStatus reports whether name is a transition an API caller
// may request.
func ValidOperatorDriftStatus(name string) bool {
	for _, status := range OperatorDriftStatuses {
		if string(status) == name {
			return true
		}
	}
	return false
}

// Open reports whether a status means the difference still stands.
//
// Everything except resolved: an acknowledged or ignored difference has not
// gone away, it has been read.
func (s DriftStatus) Open() bool { return s != DriftStatusResolved }

// DriftRecord is one field that differs from the baseline.
type DriftRecord struct {
	ID          int64  `json:"id"`
	ContainerID string `json:"containerId"`
	// ContainerName is denormalised so a list response does not need a join
	// per row, and so a record stays readable after the container is gone.
	ContainerName string `json:"containerName"`
	// SnapshotID is the baseline this was measured against.
	SnapshotID int64 `json:"snapshotId"`

	// DetectedAt is when the difference was FIRST seen. It does not move when
	// a later evaluation sees the same difference again, so the age of a drift
	// record is the age of the drift.
	DetectedAt time.Time `json:"detectedAt"`
	// LastSeenAt is the most recent evaluation that still saw it.
	LastSeenAt time.Time `json:"lastSeenAt"`
	// ResolvedAt is set when an evaluation stopped seeing it.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
	// InventoryGeneration is the inventory the comparison read, so a record
	// can be tied back to the exact observation that produced it.
	InventoryGeneration int64 `json:"inventoryGeneration"`

	Category DriftCategory `json:"category"`
	// Field is the key the diff engine produced, such as "privileged" or
	// "image.digest". Engine-generated, never caller text.
	Field    string        `json:"field"`
	Kind     ChangeKind    `json:"kind"`
	Severity DriftSeverity `json:"severity"`

	// PreviousValue and CurrentValue are ALWAYS empty when Sensitive is true.
	// See the package comment: four independent layers keep it that way.
	PreviousValue string `json:"previousValue,omitempty"`
	CurrentValue  string `json:"currentValue,omitempty"`
	// Sensitive marks a record whose values are withheld, so a UI can explain
	// that something changed without showing what.
	Sensitive bool `json:"sensitive,omitempty"`

	Status DriftStatus `json:"status"`
	// Reason explains the classification, or why a comparison could not be
	// made. Engine-generated prose from a fixed set of phrases; never an error
	// string and never caller input.
	Reason string `json:"reason,omitempty"`

	// Note carries an operator's comment from a status change. Length-bounded
	// and UTF-8 validated before it reaches the database.
	Note string `json:"note,omitempty"`
	// StatusChangedAt is when an operator last moved the status.
	StatusChangedAt *time.Time `json:"statusChangedAt,omitempty"`
}

// DriftIdentity is what makes two observations the same drift.
//
// A drift record is identified by the container, the baseline it was measured
// against, and the field -- NOT by its values. That is what makes a field
// whose value keeps changing one long-lived record rather than a new record
// per evaluation, and it is what lets an operator's "ignored" survive the next
// evaluation.
type DriftIdentity struct {
	ContainerID string
	SnapshotID  int64
	Category    DriftCategory
	Field       string
}

// Identity returns the record's identity.
func (d DriftRecord) Identity() DriftIdentity {
	return DriftIdentity{
		ContainerID: d.ContainerID,
		SnapshotID:  d.SnapshotID,
		Category:    d.Category,
		Field:       d.Field,
	}
}

// DriftSummary is the aggregate view the dashboard renders.
//
// Computed by ONE aggregate query rather than by counting a list in memory:
// the summary must stay cheap on a host with a thousand containers, and
// fetching every record to count it would be the N+1 pattern in its most
// expensive form.
type DriftSummary struct {
	// Total counts every record in any status.
	Total int `json:"total"`
	// Open counts every record that is not resolved.
	Open int `json:"open"`

	BySeverity map[DriftSeverity]int `json:"bySeverity"`
	ByStatus   map[DriftStatus]int   `json:"byStatus"`
	ByCategory map[DriftCategory]int `json:"byCategory"`

	// ContainersWithDrift counts distinct containers holding at least one open
	// record.
	ContainersWithDrift int `json:"containersWithDrift"`
	// ContainersEvaluated counts containers a comparison has been attempted
	// for, so a UI can say "12 of 40 containers have drift" honestly rather
	// than implying the other 28 were checked when they were not.
	ContainersEvaluated int `json:"containersEvaluated"`

	// LastEvaluatedAt is the most recent evaluation of any container.
	LastEvaluatedAt *time.Time `json:"lastEvaluatedAt,omitempty"`
	// Incomplete reports that at least one container's most recent evaluation
	// could not compare everything -- a truncated diff, or a missing baseline.
	// A summary that hid this would read as "these are all the differences",
	// which is exactly the wrong thing to tell someone reviewing posture.
	Incomplete bool `json:"incomplete"`
}

// DriftEngineStatus reports the evaluation queue's state.
//
// Surfaced through the health endpoint for the same reason the event engine's
// queue depth is: an overflowed queue means the estate is being evaluated by
// sweep rather than by event, which is correct but slower, and an operator
// should be able to see that rather than infer it from stale timestamps.
type DriftEngineStatus struct {
	Enabled bool `json:"enabled"`
	// PendingEvaluations is how many containers are waiting out their debounce
	// window.
	PendingEvaluations int `json:"pendingEvaluations"`
	// SweepPending reports that a full sweep is owed.
	SweepPending bool `json:"sweepPending"`
	// Overflowed reports that the queue hit its cap and escalated. Cleared
	// when the resulting sweep completes.
	Overflowed bool `json:"overflowed"`
}

// DriftEvaluation records one comparison attempt for one container.
//
// Kept separate from the records themselves because an evaluation that found
// NOTHING is still a fact worth storing: it is the difference between "this
// container has no drift" and "this container has never been checked".
type DriftEvaluation struct {
	ContainerID   string    `json:"containerId"`
	ContainerName string    `json:"containerName"`
	SnapshotID    int64     `json:"snapshotId"`
	EvaluatedAt   time.Time `json:"evaluatedAt"`

	InventoryGeneration int64 `json:"inventoryGeneration"`
	// DriftCount is how many differences the evaluation recorded.
	DriftCount int `json:"driftCount"`

	// Complete reports that the comparison examined everything. False when the
	// diff was truncated or no baseline existed, in which case DriftCount is a
	// floor rather than a total.
	Complete bool `json:"complete"`
	// Reason explains an incomplete evaluation, from a fixed set of phrases.
	Reason string `json:"reason,omitempty"`
}
