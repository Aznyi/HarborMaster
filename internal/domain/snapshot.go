package domain

import "time"

// SnapshotSource records why a snapshot was captured.
type SnapshotSource string

// Snapshot sources. Only SnapshotSourceManual is produced today, since nothing
// automatic mutates containers yet.
const (
	SnapshotSourceManual    SnapshotSource = "manual"
	SnapshotSourceScheduled SnapshotSource = "scheduled"
	SnapshotSourcePreUpdate SnapshotSource = "pre_update"
)

// Snapshot is a point-in-time capture of a container's configuration.
//
// Snapshots are the foundation for later rollback work: they must be a
// faithful, immutable record. Spec holds the normalised configuration as JSON
// and Checksum lets HarborMaster detect drift without re-parsing it.
type Snapshot struct {
	ID            int64          `json:"id"`
	ContainerID   string         `json:"containerId"`
	ContainerName string         `json:"containerName"`
	Source        SnapshotSource `json:"source"`
	Image         string         `json:"image"`
	ImageID       string         `json:"imageId"`
	// Spec is the normalised container configuration, encoded as JSON.
	Spec []byte `json:"spec"`
	// Checksum is a hex-encoded SHA-256 of Spec.
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"createdAt"`
	Note      string    `json:"note,omitempty"`
}
