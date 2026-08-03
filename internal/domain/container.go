package domain

import "time"

// ContainerState is the lifecycle state reported by the Docker Engine.
type ContainerState string

// Container states as reported by the Engine.
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

// HealthState is the Docker healthcheck verdict for a container. Containers
// without a healthcheck report HealthNone.
type HealthState string

// Health states as reported by the Engine.
const (
	HealthNone      HealthState = "none"
	HealthStarting  HealthState = "starting"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
)

// Port is a published port mapping.
type Port struct {
	// HostIP is the interface the port is published on; empty when unpublished.
	HostIP string `json:"hostIp,omitempty"`
	// HostPort is 0 when the port is exposed but not published.
	HostPort      uint16 `json:"hostPort,omitempty"`
	ContainerPort uint16 `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

// Mount is a bind mount or volume attached to a container.
type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"readOnly"`
}

// Container is HarborMaster's view of a Docker container.
//
// It is intentionally a subset of the Engine's inspect payload: only fields
// HarborMaster reasons about are modelled, so the Engine's schema does not leak
// into the API or the database.
type Container struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	ImageID     string            `json:"imageId"`
	State       ContainerState    `json:"state"`
	Health      HealthState       `json:"health"`
	Status      string            `json:"status"`
	CreatedAt   time.Time         `json:"createdAt"`
	StartedAt   *time.Time        `json:"startedAt,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Ports       []Port            `json:"ports,omitempty"`
	Networks    []string          `json:"networks,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Managed     bool              `json:"managed"`
	ComposeName string            `json:"composeProject,omitempty"`
}

// ParseContainerState maps an Engine state string onto a known state,
// falling back to StateUnknown rather than trusting arbitrary input.
func ParseContainerState(s string) ContainerState {
	switch ContainerState(s) {
	case StateCreated, StateRunning, StatePaused, StateRestarting,
		StateRemoving, StateExited, StateDead:
		return ContainerState(s)
	default:
		return StateUnknown
	}
}

// ParseHealthState maps an Engine health string onto a known health state.
// An empty string means the container declares no healthcheck.
func ParseHealthState(s string) HealthState {
	switch HealthState(s) {
	case HealthStarting, HealthHealthy, HealthUnhealthy:
		return HealthState(s)
	default:
		return HealthNone
	}
}
