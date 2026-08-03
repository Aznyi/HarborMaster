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
	Status    OverallStatus `json:"status"`
	Database  Component     `json:"database"`
	Docker    Component     `json:"docker"`
	CheckedAt time.Time     `json:"checkedAt"`
	UptimeSec int64         `json:"uptimeSeconds"`
}
