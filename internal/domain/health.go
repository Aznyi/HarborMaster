package domain

import "time"

// ComponentStatus is the reachability verdict for one dependency.
type ComponentStatus string

// Component statuses.
const (
	// StatusUp means the dependency responded successfully.
	StatusUp ComponentStatus = "up"
	// StatusDown means the dependency did not respond. For Docker this is a
	// degraded but valid operating mode, not a fatal error.
	StatusDown ComponentStatus = "down"
)

// Component is the health of a single dependency.
type Component struct {
	Status ComponentStatus `json:"status"`
	// Detail is a short, operator-safe explanation. It must never carry a raw
	// error chain, a file path from a privileged socket, or credentials.
	Detail string `json:"detail,omitempty"`
	// LatencyMS is the round-trip time of the probe, when one was performed.
	LatencyMS int64 `json:"latencyMs,omitempty"`
	// Version is the dependency's reported version, when available.
	Version string `json:"version,omitempty"`
}

// OverallStatus summarises a HealthReport.
type OverallStatus string

// Overall statuses.
const (
	// StatusHealthy means every dependency is up.
	StatusHealthy OverallStatus = "healthy"
	// StatusDegraded means HarborMaster is serving, but at least one
	// non-fatal dependency is unreachable.
	StatusDegraded OverallStatus = "degraded"
	// StatusUnhealthy means a required dependency is unreachable.
	StatusUnhealthy OverallStatus = "unhealthy"
)

// HealthReport is the payload of GET /api/v1/health.
//
// Docker being down yields StatusDegraded rather than StatusUnhealthy: the API
// still serves, and the frontend renders a disconnected state. Only the
// database, which HarborMaster cannot operate without, yields StatusUnhealthy.
type HealthReport struct {
	Status   OverallStatus `json:"status"`
	Database Component     `json:"database"`
	Docker   Component     `json:"docker"`
	// Events reports the Docker event engine. A pointer, and omitted when nil,
	// so a deployment with no event engine configured says nothing about it
	// rather than reporting a fabricated status.
	//
	// A disconnected event stream yields StatusDegraded overall, never
	// StatusUnhealthy: periodic reconciliation still keeps the inventory
	// correct, and a transient reconnect must not fail the container health
	// check and trigger a restart loop.
	Events    *Component `json:"events,omitempty"`
	CheckedAt time.Time  `json:"checkedAt"`
	UptimeSec int64      `json:"uptimeSeconds"`

	// Features is which capabilities this deployment has, and is served only
	// to an authenticated caller.
	//
	// Reported so "is that feature off, or broken" is answerable from the
	// interface. Every one of these is off by default and turning one on is a
	// deliberate act, which means an operator looking at an empty page has two
	// explanations and no way to choose between them.
	Features *Features `json:"features,omitempty"`
}

// Features is which capabilities a deployment turned on.
//
// # What this is not
//
// It is not configuration. No VALUE appears here — no path, no address, no
// interval, and certainly no credential. Each field is a boolean saying whether
// a capability exists in this process, which is exactly what an operator needs
// to distinguish "switched off" from "not working" and is the least that
// answers it.
//
// # Why the three mutations are grouped and named for what they DO
//
// Because "acquisition" means nothing to somebody reading a settings page at
// two in the morning. What matters is that one downloads, one stops and
// replaces a running container, and one puts a container back.
type Features struct {
	// The read-only engines.
	Inventory  bool `json:"inventory"`
	Events     bool `json:"events"`
	Snapshots  bool `json:"snapshots"`
	Drift      bool `json:"drift"`
	Policy     bool `json:"policy"`
	Planner    bool `json:"planner"`
	ImageIntel bool `json:"imageIntel"`

	// The Docker mutations, off by default and each a separate capability.
	//
	// Acquisition downloads an approved, digest-pinned image and touches no
	// container. Execution STOPS A RUNNING CONTAINER and replaces it. Rollback
	// stops the replacement and starts the original.
	Acquisition bool `json:"acquisition"`
	Execution   bool `json:"execution"`
	Rollback    bool `json:"rollback"`

	// Automation changes containers with nobody watching. It holds no Docker
	// capability of its own; it submits the same requests an operator would.
	Automation bool `json:"automation"`

	// Notifications is HarborMaster's second outbound egress.
	Notifications bool `json:"notifications"`
	// NotificationsAllowPrivate reports the one relaxation of the address
	// guard. Surfaced because it is the setting an operator most needs to be
	// reminded they turned on.
	NotificationsAllowPrivate bool `json:"notificationsAllowPrivate"`
}
