package domain

// SnapshotSpecVersion is the current canonical-document shape.
//
// A capture-shape change increments this. Old documents are never rewritten:
// they are evidence, and re-encoding evidence under a new shape would change
// its checksum and destroy the property that makes it verifiable.
const SnapshotSpecVersion = 1

// SnapshotSpec is the canonical, immutable record of one container's
// configuration.
//
// # What it is
//
// The single source of truth for a snapshot. Every relational child row is
// derived from this document, written in the same transaction, and is never
// independently authoritative.
//
// # Determinism
//
// json.Marshal over this type is byte-deterministic: struct fields emit in
// declaration order, map keys are sorted by the encoder, and there is no
// floating-point field anywhere in the model. Set-like slices are sorted before
// the document is built. That determinism is what makes the checksum
// meaningful -- identical logical configuration produces identical bytes.
//
// # What is absent, and why absent rather than ignored
//
// Timestamps, status prose, inventory generation, restart counts, runtime IP
// addresses, gateways, MAC addresses, endpoint IDs, and health results do not
// appear here at all. Excluding them by omission rather than by filtering means
// no future edit can accidentally let one into the checksum: there is no field
// to populate.
//
// # Secrets
//
// A sensitive environment variable or log option carries no value here. Its
// digest travels in SnapshotEnvVar's unserialised fields to the child table and
// contributes to the checksum, so changing a password changes the checksum
// without the document ever containing the password.
type SnapshotSpec struct {
	SpecVersion int `json:"specVersion"`

	Identity SpecIdentity `json:"identity"`
	Image    SpecImage    `json:"image"`
	Process  Process      `json:"process"`

	Environment []SpecEnvVar  `json:"environment"`
	Labels      []Label       `json:"labels"`
	Ports       []Port        `json:"ports"`
	Networks    []SpecNetwork `json:"networks"`
	Mounts      []SpecMount   `json:"mounts"`

	RestartPolicy RestartPolicy `json:"restartPolicy"`
	HealthCheck   *HealthCheck  `json:"healthCheck,omitempty"`
	Resources     Resources     `json:"resources"`
	Security      Security      `json:"security"`
	Logging       SpecLogging   `json:"logging"`

	Compose      ComposeMetadata      `json:"compose"`
	HarborMaster HarborMasterMetadata `json:"harbormaster"`
}

// SpecIdentity is the container's stable identity.
//
// The container ID is included: a snapshot describes one specific container,
// and a recreated container with the same name is a different thing.
type SpecIdentity struct {
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
}

// SpecImage is the image the container was created from.
//
// Both the reference as written and the resolved content-addressable ID, since
// a tag can be repointed while an ID cannot.
type SpecImage struct {
	Reference  string `json:"reference"`
	Repository string `json:"repository,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	ImageID    string `json:"imageId,omitempty"`
	// RepoDigests are the registry-resolvable manifest references known for the
	// image, sorted. Their presence is what makes a restore target unambiguous.
	RepoDigests  []string `json:"repoDigests,omitempty"`
	Architecture string   `json:"architecture,omitempty"`
	OS           string   `json:"os,omitempty"`
}

// SpecEnvVar is one environment variable as captured.
//
// Value is empty and omitted for a sensitive variable. Digest, DigestAlgorithm,
// and DigestKeyID are `json:"-"`: they contribute to the checksum through the
// canonical writer and are persisted to the child table, but they are never
// part of the document's bytes and can never reach an API response.
type SpecEnvVar struct {
	Name        string      `json:"name"`
	Value       string      `json:"value,omitempty"`
	Sensitivity Sensitivity `json:"sensitivity"`
	// Present distinguishes an unset variable from one set to the empty string.
	Present bool `json:"present"`
	// Length is the byte length of the original value, for sensitive variables
	// where Value is withheld.
	Length int `json:"length,omitempty"`

	Digest          string          `json:"-"`
	DigestAlgorithm DigestAlgorithm `json:"-"`
	DigestKeyID     string          `json:"-"`
}

// SecretDigest reassembles the digest fields for comparison.
func (e SpecEnvVar) SecretDigest() SecretDigest {
	return SecretDigest{
		Present:   e.Present,
		Length:    e.Length,
		Digest:    e.Digest,
		Algorithm: e.DigestAlgorithm,
		KeyID:     e.DigestKeyID,
	}
}

// Sensitive reports whether the variable was classified as secret-bearing.
func (e SpecEnvVar) Sensitive() bool { return e.Sensitivity == SensitivitySensitive }

// SpecNetwork is one network attachment, stripped of runtime addressing.
//
// IP addresses, gateway, MAC address, and endpoint ID are deliberately absent:
// the daemon assigns them at start, so including them would make every restart
// look like a configuration change.
type SpecNetwork struct {
	NetworkName string   `json:"networkName"`
	Aliases     []string `json:"aliases,omitempty"`
	Links       []string `json:"links,omitempty"`
}

// SpecMount is one mount as configured.
type SpecMount struct {
	Type        MountType `json:"type"`
	Source      string    `json:"source,omitempty"`
	Destination string    `json:"destination"`
	ReadOnly    bool      `json:"readOnly"`
	Propagation string    `json:"propagation,omitempty"`
	VolumeName  string    `json:"volumeName,omitempty"`
	Driver      string    `json:"driver,omitempty"`
	// TmpfsOptions is the raw option string for a tmpfs mount, e.g. "size=16m".
	TmpfsOptions string `json:"tmpfsOptions,omitempty"`
}

// Tmpfs reports whether the mount is an in-memory filesystem, which a restore
// recreates rather than reattaching.
func (m SpecMount) Tmpfs() bool { return m.Type == MountTypeTmpfs }

// SpecLogging is the log driver configuration.
//
// Options run through the same classification as environment variables: log
// driver configuration routinely carries endpoint credentials -- a Splunk
// token, a Loki basic-auth URL, a Fluentd shared key.
type SpecLogging struct {
	Driver  string       `json:"driver,omitempty"`
	Options []SpecEnvVar `json:"options,omitempty"`
}
