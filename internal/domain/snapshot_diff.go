package domain

// ChangeKind classifies one difference between two snapshots.
type ChangeKind string

// Change kinds.
const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
	// ChangeUnchanged entries are omitted unless explicitly requested, so a
	// response is proportional to the change rather than to the configuration.
	ChangeUnchanged ChangeKind = "unchanged"
	// ChangeUnverifiable means the two values cannot be compared at all --
	// specifically, two secret digests produced under different HMAC keys.
	// Reporting "modified" there would tell an operator every secret changed
	// after a key rotation, which is a false alarm indistinguishable from a
	// real breach.
	ChangeUnverifiable ChangeKind = "unverifiable"
)

// DiffGroupName identifies a section of the configuration.
//
// A closed, code-defined vocabulary. Narrowing a diff means choosing a name
// from this list, validated against an allowlist -- there are no user-defined
// selectors, expressions, or paths anywhere in the diff API.
type DiffGroupName string

// Diff groups.
const (
	DiffGroupEnvironment DiffGroupName = "environment"
	DiffGroupLabels      DiffGroupName = "labels"
	DiffGroupPorts       DiffGroupName = "ports"
	DiffGroupNetworks    DiffGroupName = "networks"
	DiffGroupMounts      DiffGroupName = "mounts"
	DiffGroupResources   DiffGroupName = "resources"
	DiffGroupSecurity    DiffGroupName = "security"
	DiffGroupCompose     DiffGroupName = "compose"
	DiffGroupMetadata    DiffGroupName = "metadata"
)

// DiffGroupNames lists every group, in report order.
var DiffGroupNames = []DiffGroupName{
	DiffGroupEnvironment, DiffGroupLabels, DiffGroupPorts, DiffGroupNetworks,
	DiffGroupMounts, DiffGroupResources, DiffGroupSecurity, DiffGroupCompose,
	DiffGroupMetadata,
}

// ValidDiffGroup reports whether name identifies a known group.
func ValidDiffGroup(name string) bool {
	for _, group := range DiffGroupNames {
		if string(group) == name {
			return true
		}
	}
	return false
}

// DiffEntry is one difference.
//
// Old and New are display values. For a sensitive entry they are ALWAYS empty:
// what changed is reported through Kind alone, derived from digest comparison,
// and neither the values nor the digests are ever serialised.
type DiffEntry struct {
	Key  string     `json:"key"`
	Kind ChangeKind `json:"kind"`
	Old  string     `json:"old,omitempty"`
	New  string     `json:"new,omitempty"`
	// Sensitive marks an entry whose values are withheld, so a UI can explain
	// why it shows a change without showing what changed.
	Sensitive bool `json:"sensitive,omitempty"`
	// Note carries an explanation, such as why an entry is unverifiable.
	Note string `json:"note,omitempty"`
}

// DiffGroup is one section's differences.
type DiffGroup struct {
	Name    DiffGroupName `json:"name"`
	Entries []DiffEntry   `json:"entries"`

	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Modified  int `json:"modified"`
	Unchanged int `json:"unchanged"`

	// Truncated reports that this group stopped short of comparing or
	// returning everything. Never silent: a truncated group that looked
	// complete would read as "these configurations are identical", which is
	// exactly the wrong conclusion to hand an operator preparing a restore.
	Truncated bool `json:"truncated,omitempty"`
	// Returned and Total let a client tell "no further changes" from "we
	// stopped looking".
	Returned int `json:"returned"`
	Total    int `json:"total"`
}

// SnapshotDiff compares two configurations.
type SnapshotDiff struct {
	// FromSnapshotID is the baseline. ToSnapshotID is zero when the comparison
	// target is the container's CURRENT configuration rather than a stored
	// snapshot.
	FromSnapshotID int64 `json:"fromSnapshotId"`
	ToSnapshotID   int64 `json:"toSnapshotId,omitempty"`
	// AgainstCurrent reports that the comparison target was live configuration.
	AgainstCurrent bool `json:"againstCurrent"`

	Groups []DiffGroup `json:"groups"`

	// Identical is true when nothing differs, which is the question most
	// callers are actually asking.
	Identical bool `json:"identical"`

	AddedCount     int `json:"addedCount"`
	RemovedCount   int `json:"removedCount"`
	ModifiedCount  int `json:"modifiedCount"`
	ChangedCount   int `json:"changedCount"`
	UnchangedCount int `json:"unchangedCount"`

	// Truncated and TruncationReason apply to the diff as a whole.
	Truncated        bool   `json:"truncated,omitempty"`
	TruncationReason string `json:"truncationReason,omitempty"`
}
