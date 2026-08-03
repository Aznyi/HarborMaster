package domain

import "time"

// EventSeverity classifies an event for display and, later, notification
// routing.
type EventSeverity string

// Event severities.
const (
	SeverityInfo    EventSeverity = "info"
	SeverityWarning EventSeverity = "warning"
	SeverityError   EventSeverity = "error"
)

// EventType identifies what happened. Only observational events are recorded
// today; action events will join them once HarborMaster can act.
type EventType string

// Event types recorded today.
const (
	EventServerStarted      EventType = "server.started"
	EventServerStopped      EventType = "server.stopped"
	EventDockerConnected    EventType = "docker.connected"
	EventDockerDisconnected EventType = "docker.disconnected"
	EventSnapshotCaptured   EventType = "snapshot.captured"
)

// Event is an audit record of something HarborMaster observed or did.
//
// Events are append-only. Message is operator-facing prose and must never
// contain credentials, environment-variable values, or raw error chains from
// privileged interfaces.
type Event struct {
	ID          int64         `json:"id"`
	Type        EventType     `json:"type"`
	Severity    EventSeverity `json:"severity"`
	ContainerID string        `json:"containerId,omitempty"`
	Message     string        `json:"message"`
	// Details is optional structured context, encoded as JSON.
	Details    []byte    `json:"details,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}
