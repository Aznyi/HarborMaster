package domain

import (
	"encoding/json"
	"time"
)

// SnapshotTrigger records what caused a capture.
type SnapshotTrigger string

// Snapshot triggers.
//
// The vocabulary is closed and enforced by a CHECK constraint. Notably absent:
// anything implying HarborMaster acted on a container. Capture observes.
const (
	// SnapshotTriggerManual is an operator asking for a capture in the UI.
	SnapshotTriggerManual SnapshotTrigger = "manual"
	// SnapshotTriggerAPI is a direct API call.
	SnapshotTriggerAPI SnapshotTrigger = "api"
	// SnapshotTriggerScheduled is reserved for a future scheduler. Nothing
	// produces it in Phase 3.
	SnapshotTriggerScheduled SnapshotTrigger = "scheduled"
	// SnapshotTriggerPreUpdate is reserved for the future update phase, which
	// will capture a baseline before changing anything. Nothing produces it in
	// Phase 3.
	SnapshotTriggerPreUpdate SnapshotTrigger = "pre_update"
)

// SnapshotTriggers lists every trigger, for filter validation.
var SnapshotTriggers = []SnapshotTrigger{
	SnapshotTriggerManual, SnapshotTriggerAPI,
	SnapshotTriggerScheduled, SnapshotTriggerPreUpdate,
}

// ValidSnapshotTrigger reports whether s names a known trigger.
func ValidSnapshotTrigger(s string) bool {
	for _, trigger := range SnapshotTriggers {
		if string(trigger) == s {
			return true
		}
	}
	return false
}

// Snapshot is an immutable deployment checkpoint.
//
// # Immutability
//
// Everything here except ReadinessStatus and ReadinessEvaluatedAt is fixed at
// capture time and never changes. Those two are a denormalised cache of the
// most recent readiness evaluation, kept so the list endpoint can filter and
// sort without a join; they are derived, not asserted.
//
// The repository has no general-purpose update method, and a test asserts that
// the only UPDATE statement in the package touches those two columns and no
// others. A snapshot is evidence, and evidence that can be edited is not
// evidence.
//
// # Verifiability
//
// SpecJSON is the canonical document and Checksum covers it together with the
// secret digests, so a stored snapshot can be checked without trusting the row
// it came from.
//
// # Secrets
//
// SpecJSON contains no plaintext secret in any encoding. A sensitive value is
// represented by a keyed digest that lives in the environment child rows and
// never reaches this struct.
type Snapshot struct {
	ID            int64  `json:"id"`
	HostID        string `json:"hostId"`
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`

	ImageReference string `json:"imageReference"`
	ImageDigest    string `json:"imageDigest,omitempty"`
	ImageID        string `json:"imageId,omitempty"`

	SpecVersion int             `json:"specVersion"`
	SpecJSON    json.RawMessage `json:"spec,omitempty"`
	Checksum    string          `json:"checksum"`

	HarborMasterVersion string `json:"harbormasterVersion,omitempty"`
	DockerAPIVersion    string `json:"dockerApiVersion,omitempty"`
	DockerEngineVersion string `json:"dockerEngineVersion,omitempty"`

	Trigger SnapshotTrigger `json:"trigger"`
	Reason  string          `json:"reason,omitempty"`

	// InventoryGeneration and EventSequence tie the capture to what
	// HarborMaster had observed at the time, so a later reader can tell how
	// current the underlying data was.
	InventoryGeneration int64 `json:"inventoryGeneration"`
	EventSequence       int64 `json:"eventSequence"`

	Warnings     []InventoryWarning `json:"warnings"`
	WarningCount int                `json:"warningCount"`

	ReadinessStatus      ReadinessStatus `json:"readinessStatus"`
	ReadinessEvaluatedAt *time.Time      `json:"readinessEvaluatedAt,omitempty"`

	// DigestKeyID identifies the HMAC key this snapshot's secret digests were
	// produced under. Digests from different keys are not comparable, and a
	// diff that spans two key IDs reports unverifiable rather than guessing.
	DigestKeyID string `json:"digestKeyId,omitempty"`

	CreatedAt time.Time `json:"createdAt"`

	// Deduplicated reports that Create returned an existing snapshot because
	// the configuration was unchanged. Response-only; never persisted.
	Deduplicated bool `json:"deduplicated,omitempty"`
}

// SnapshotEnvEntry is one captured environment variable or log option.
//
// Value is empty for a sensitive entry, and Digest is `json:"-"` for the same
// reason SecretDigest's is: it cannot reach an API response by accident.
type SnapshotEnvEntry struct {
	Position       int         `json:"position"`
	Key            string      `json:"key"`
	Classification Sensitivity `json:"classification"`
	Present        bool        `json:"present"`
	Value          string      `json:"value,omitempty"`
	Length         int         `json:"length,omitempty"`

	Digest          string          `json:"-"`
	DigestAlgorithm DigestAlgorithm `json:"-"`
	DigestKeyID     string          `json:"-"`
}

// Sensitive reports whether the entry was classified as secret-bearing.
func (e SnapshotEnvEntry) Sensitive() bool { return e.Classification == SensitivitySensitive }

// SecretDigest reassembles the digest for comparison.
func (e SnapshotEnvEntry) SecretDigest() SecretDigest {
	return SecretDigest{
		Present:   e.Present,
		Length:    e.Length,
		Digest:    e.Digest,
		Algorithm: e.DigestAlgorithm,
		KeyID:     e.DigestKeyID,
	}
}

// SnapshotMountRow is one captured mount, as stored relationally.
type SnapshotMountRow struct {
	Destination string    `json:"destination"`
	Type        MountType `json:"type"`
	Source      string    `json:"source,omitempty"`
	ReadOnly    bool      `json:"readOnly"`
	VolumeName  string    `json:"volumeName,omitempty"`
	Driver      string    `json:"driver,omitempty"`
}

// SnapshotNetworkRow is one captured network attachment, as stored
// relationally.
type SnapshotNetworkRow struct {
	NetworkName string   `json:"networkName"`
	Aliases     []string `json:"aliases,omitempty"`
}
