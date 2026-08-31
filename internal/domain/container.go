package domain

import "time"

// ContainerState is the normalized lifecycle state of a container.
//
// The vocabulary is fixed and closed. An unrecognised runtime state maps to
// StateUnknown rather than passing through, so nothing downstream -- SQL
// filters, UI badges, API consumers -- ever has to handle an open-ended set.
type ContainerState string

// Container states.
const (
	StateCreated    ContainerState = "created"
	StateRunning    ContainerState = "running"
	StatePaused     ContainerState = "paused"
	StateRestarting ContainerState = "restarting"
	StateRemoving   ContainerState = "removing"
	StateExited     ContainerState = "exited"
	StateDead       ContainerState = "dead"
	StateUnknown    ContainerState = "unknown"
)

// ContainerStates lists every state, in lifecycle order. Used to validate API
// filters and to render the UI's state selector.
var ContainerStates = []ContainerState{
	StateCreated, StateRunning, StatePaused, StateRestarting,
	StateRemoving, StateExited, StateDead, StateUnknown,
}

// HealthState is the normalized health verdict for a container.
// A container with no healthcheck reports HealthNone.
type HealthState string

// Health states.
const (
	HealthNone      HealthState = "none"
	HealthStarting  HealthState = "starting"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
)

// HealthStates lists every health state, for filter validation.
var HealthStates = []HealthState{HealthNone, HealthStarting, HealthHealthy, HealthUnhealthy}

// ParseContainerState maps a runtime state string onto a known state, falling
// back to StateUnknown rather than trusting arbitrary input.
func ParseContainerState(s string) ContainerState {
	switch ContainerState(s) {
	case StateCreated, StateRunning, StatePaused, StateRestarting,
		StateRemoving, StateExited, StateDead:
		return ContainerState(s)
	default:
		return StateUnknown
	}
}

// ParseHealthState maps a runtime health string onto a known health state.
// An empty string means the container declares no healthcheck.
func ParseHealthState(s string) HealthState {
	switch HealthState(s) {
	case HealthStarting, HealthHealthy, HealthUnhealthy:
		return HealthState(s)
	default:
		return HealthNone
	}
}

// ValidContainerState reports whether s names a known state.
func ValidContainerState(s string) bool {
	for _, state := range ContainerStates {
		if string(state) == s {
			return true
		}
	}
	return false
}

// ValidHealthState reports whether s names a known health state.
func ValidHealthState(s string) bool {
	for _, health := range HealthStates {
		if string(health) == s {
			return true
		}
	}
	return false
}

// ContainerSummary is the list-row projection of a container.
//
// It carries what the containers table and the dashboard need, and nothing
// else: the expensive normalized configuration lives in ContainerDetail and is
// loaded only when a single container is requested.
type ContainerSummary struct {
	HostID  string `json:"hostId"`
	ID      string `json:"id"`
	ShortID string `json:"shortId"`
	Name    string `json:"name"`

	Image   ImageRef `json:"image"`
	ImageID string   `json:"imageId,omitempty"`

	State  ContainerState `json:"state"`
	Status string         `json:"status,omitempty"`
	Health HealthState    `json:"health"`

	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	ExitCode      *int          `json:"exitCode,omitempty"`
	RestartCount  int           `json:"restartCount"`
	RestartPolicy RestartPolicy `json:"restartPolicy"`

	Compose      ComposeMetadata      `json:"compose"`
	HarborMaster HarborMasterMetadata `json:"harbormaster"`

	Ports []Port `json:"ports"`

	// Present is false for a container that earlier refreshes saw but the most
	// recent one did not. The row is retained rather than deleted so history
	// and warnings survive a container being removed out from under
	// HarborMaster.
	Present bool `json:"present"`
	// FirstSeenAt and LastSeenAt bound the container's observed lifetime.
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	// Generation is the inventory generation that last observed this container.
	Generation int64 `json:"generation"`
	// WarningCount is how many non-fatal problems the last refresh recorded.
	WarningCount int `json:"warningCount"`
}

// ContainerDetail is the full normalized view of one container.
//
// The sections mirror the detail API response and the UI's tabs one-for-one,
// so adding a field means touching one section rather than reshaping a
// flattened struct.
//
// Identity fields are split across two sections by intent: the stable
// identifiers and timestamps live in Overview, where the list view also needs
// them, while Hostname and Domainname live in Process because they are
// container configuration rather than identity HarborMaster assigns.
type ContainerDetail struct {
	Overview     ContainerSummary     `json:"overview"`
	State        StateDetail          `json:"state"`
	Image        *Image               `json:"image,omitempty"`
	Process      Process              `json:"process"`
	HealthCheck  *HealthCheck         `json:"healthCheck,omitempty"`
	Environment  []EnvVar             `json:"environment"`
	Labels       []Label              `json:"labels"`
	Ports        []Port               `json:"ports"`
	Mounts       []Mount              `json:"mounts"`
	Networks     []NetworkAttachment  `json:"networks"`
	Resources    Resources            `json:"resources"`
	Security     Security             `json:"security"`
	Logging      Logging              `json:"logging"`
	Compose      ComposeMetadata      `json:"compose"`
	HarborMaster HarborMasterMetadata `json:"harbormaster"`
	Warnings     []InventoryWarning   `json:"warnings"`

	// ImageLineage distinguishes what this container RUNS from what
	// HarborMaster FOLLOWS for it. A managed container that has been updated
	// runs an immutable digest while tracking a mutable tag, and presenting the
	// tag as the artefact executing would be a lie about what is on the host.
	//
	// Nil when HarborMaster holds no lineage for this container, which is
	// honest rather than empty: it means nothing is being followed.
	ImageLineage *ImageLineage `json:"imageLineage,omitempty"`

	// RunningDigest is the manifest digest this container was running as of the
	// inventory record carrying it, resolved by RunningDigestFor.
	//
	// It is only a claim about NOW when the record is a present one. This
	// struct is returned for departed containers too -- that is what a detail
	// page and an Activity entry render -- and on such a record this is the
	// digest the container was running when it left. A caller reasoning about
	// what is running must establish presence first; see
	// ContainerRepository.GetPresent.
	//
	// Distinct from Overview.Image.Digest, which is only ever the digest the
	// declared REFERENCE carries and is therefore empty for every container
	// created from a tag. Conflating the two is what left a recreation with no
	// record of what it replaced, and so left a rollback unable to put lineage
	// back.
	//
	// Empty means UNESTABLISHED -- a locally built image, or RepoDigests too
	// ambiguous to choose between. It never means "no digest is running".
	RunningDigest string `json:"runningDigest,omitempty"`
}

// StateDetail is the expanded runtime state of a container.
type StateDetail struct {
	// State is the normalized state; RawState is what the runtime reported,
	// kept so an unmapped value is still visible to an operator.
	State    ContainerState `json:"state"`
	RawState string         `json:"rawState,omitempty"`
	Status   string         `json:"status,omitempty"`

	Running    bool `json:"running"`
	Paused     bool `json:"paused"`
	Restarting bool `json:"restarting"`
	Dead       bool `json:"dead"`
	OOMKilled  bool `json:"oomKilled"`

	ExitCode     *int   `json:"exitCode,omitempty"`
	Error        string `json:"error,omitempty"`
	RestartCount int    `json:"restartCount"`

	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	Health              HealthState      `json:"health"`
	HealthFailingStreak int              `json:"healthFailingStreak,omitempty"`
	HealthLog           []HealthLogEntry `json:"healthLog,omitempty"`
}

// HealthLogEntry summarises one healthcheck run.
//
// The probe's stdout/stderr is deliberately NOT carried. A healthcheck command
// is arbitrary and its output routinely contains connection strings, tokens,
// or query results. Timing and exit code answer "is it failing, and since
// when" without turning the inventory into an exfiltration channel.
type HealthLogEntry struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	ExitCode int       `json:"exitCode"`
}
