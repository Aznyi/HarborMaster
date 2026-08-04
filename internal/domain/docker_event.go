package domain

import "time"

// Docker events, as HarborMaster models them.
//
// A Docker event is a HINT that something on the host may have changed. It is
// not the authoritative record of what is there. The daemon makes no durability
// promise about its event stream: events are dropped while nothing is
// listening, they are not replayed across a daemon restart, and their ordering
// is only per-connection. Docker inspection and HarborMaster's own persistence
// remain authoritative, and periodic full reconciliation is what restores
// accuracy after anything is missed.
//
// Everything here is HarborMaster's own vocabulary. No Docker SDK type appears
// in this package, and none ever should: internal/docker is the only place
// allowed to know what an events.Message looks like.

// DockerEventType is the kind of object an event describes.
//
// A closed vocabulary with an explicit "other" member: Docker adds event types
// over time, and an unknown one must be recorded and ignored rather than
// dropped silently or crash-mapped onto something it is not.
type DockerEventType string

// Docker event types HarborMaster subscribes to.
const (
	EventTypeContainer DockerEventType = "container"
	EventTypeImage     DockerEventType = "image"
	EventTypeNetwork   DockerEventType = "network"
	EventTypeVolume    DockerEventType = "volume"
	EventTypeDaemon    DockerEventType = "daemon"
	// EventTypeOther covers anything the daemon emits that HarborMaster does
	// not model. Recorded, never acted on.
	EventTypeOther DockerEventType = "other"
)

// DockerEventTypes lists the subscribable types, in stable order. Used to build
// the daemon-side filter and to validate API query parameters.
var DockerEventTypes = []DockerEventType{
	EventTypeContainer,
	EventTypeImage,
	EventTypeNetwork,
	EventTypeVolume,
	EventTypeDaemon,
	EventTypeOther,
}

// ValidDockerEventType reports whether a string names a known event type.
func ValidDockerEventType(value string) bool {
	for _, candidate := range DockerEventTypes {
		if string(candidate) == value {
			return true
		}
	}
	return false
}

// DockerEventAction is the verb of an event, normalized to lower case.
//
// It is deliberately an open string rather than an enum. Docker daemons differ
// by version in which actions they emit, and some actions carry a suffix (a
// health-check status, an exec command line) that is split off during
// normalization. Treating the action as data rather than as a closed type is
// what lets an unknown one be stored and displayed without special handling.
type DockerEventAction string

// Container actions HarborMaster acts on. Not every daemon emits every one.
const (
	ActionCreate       DockerEventAction = "create"
	ActionStart        DockerEventAction = "start"
	ActionStop         DockerEventAction = "stop"
	ActionDie          DockerEventAction = "die"
	ActionDestroy      DockerEventAction = "destroy"
	ActionKill         DockerEventAction = "kill"
	ActionPause        DockerEventAction = "pause"
	ActionUnpause      DockerEventAction = "unpause"
	ActionRestart      DockerEventAction = "restart"
	ActionRename       DockerEventAction = "rename"
	ActionUpdate       DockerEventAction = "update"
	ActionHealthStatus DockerEventAction = "health_status"
	ActionOOM          DockerEventAction = "oom"
	ActionAttach       DockerEventAction = "attach"
	ActionDetach       DockerEventAction = "detach"
	ActionConnect      DockerEventAction = "connect"
	ActionDisconnect   DockerEventAction = "disconnect"
	// ActionRemove is what some daemon versions emit where others emit
	// destroy, notably for networks and volumes. Both are handled.
	ActionRemove DockerEventAction = "remove"
)

// Image, network, and volume actions.
const (
	ActionPull    DockerEventAction = "pull"
	ActionTag     DockerEventAction = "tag"
	ActionUntag   DockerEventAction = "untag"
	ActionDelete  DockerEventAction = "delete"
	ActionPrune   DockerEventAction = "prune"
	ActionMount   DockerEventAction = "mount"
	ActionUnmount DockerEventAction = "unmount"
	// ActionReload is emitted by the daemon on SIGHUP configuration reload.
	// HarborMaster treats it as "assume everything moved".
	ActionReload DockerEventAction = "reload"
)

// EventProcessingResult records what HarborMaster did with an event.
type EventProcessingResult string

// Processing results.
const (
	// ResultProcessed means the event was recorded and, where applicable,
	// triggered a refresh.
	ResultProcessed EventProcessingResult = "processed"
	// ResultDeduplicated means an identical event was already seen inside the
	// deduplication window. It is counted but does no refresh work.
	ResultDeduplicated EventProcessingResult = "deduplicated"
	// ResultIgnored means the event was recorded but carries no inventory
	// consequence, such as an exec or an unknown type.
	ResultIgnored EventProcessingResult = "ignored"
	// ResultWarning means the event was recorded but could not be mapped onto a
	// resource, so a full reconciliation was requested instead.
	ResultWarning EventProcessingResult = "warning"
	// ResultFailed means processing raised an error. The event is still stored;
	// Error carries a sanitised summary.
	ResultFailed EventProcessingResult = "failed"
)

// EventProcessingResults lists the results, in stable order.
var EventProcessingResults = []EventProcessingResult{
	ResultProcessed,
	ResultDeduplicated,
	ResultIgnored,
	ResultWarning,
	ResultFailed,
}

// ValidEventResult reports whether a string names a known processing result.
func ValidEventResult(value string) bool {
	for _, candidate := range EventProcessingResults {
		if string(candidate) == value {
			return true
		}
	}
	return false
}

// RefreshRequest is the synchronization an event asked for.
//
// Events never write normalized container state themselves. They request one of
// these, and the inventory service performs the actual read and persist, so
// there is exactly one normalization path regardless of what triggered it.
type RefreshRequest string

// Refresh request kinds.
const (
	// RefreshNone means the event needs no inventory work.
	RefreshNone RefreshRequest = "none"
	// RefreshContainer re-inspects one container by full ID.
	RefreshContainer RefreshRequest = "container"
	// RefreshContainerAbsent marks a confirmed-destroyed container absent.
	RefreshContainerAbsent RefreshRequest = "container_absent"
	// RefreshImage re-inspects one image by ID or reference.
	RefreshImage RefreshRequest = "image"
	// RefreshImageCatalog re-reads image metadata for referenced images. Used
	// when an image event names something HarborMaster cannot resolve to a
	// single image, such as a prune.
	RefreshImageCatalog RefreshRequest = "image_catalog"
	// RefreshNetworks re-reads the network catalog.
	RefreshNetworks RefreshRequest = "networks"
	// RefreshVolumes re-reads the volume catalog.
	RefreshVolumes RefreshRequest = "volumes"
	// RefreshFull runs a whole inventory reconciliation through the Phase 2
	// pipeline. It always wins over any pending targeted request.
	RefreshFull RefreshRequest = "full"
)

// DockerEvent is one normalized, redacted observation from the Docker event
// stream.
//
// Attributes have already been through redaction: any key matching the
// configured sensitive-name patterns carries MaskedValue rather than its real
// value. Nothing in this struct may be populated from a source that has not
// been redacted, because every field here reaches persistence, the REST API,
// and the SSE stream, none of which is authenticated.
type DockerEvent struct {
	// Sequence is HarborMaster's monotonic local ordering, assigned on
	// persistence. It is the SSE event ID and the deterministic tiebreak for
	// every list query. Zero until the event has been stored.
	//
	// Docker guarantees no global ordering across a reconnect, so this records
	// the order HarborMaster OBSERVED events in, which is the only order it can
	// honestly claim.
	Sequence int64 `json:"sequence"`

	// Fingerprint is the deterministic deduplication identity. See
	// docker.EventFingerprint for how it is derived.
	Fingerprint string `json:"fingerprint"`

	HostID string            `json:"hostId"`
	Type   DockerEventType   `json:"type"`
	Action DockerEventAction `json:"action"`

	// ActorID is the container ID, image ID or reference, network ID, or volume
	// name the event concerns. Empty when the daemon did not supply one.
	ActorID string `json:"actorId,omitempty"`
	// ActorName is the human-readable resource name, resolved from the actor
	// attributes where the daemon provides one.
	ActorName string `json:"actorName,omitempty"`
	// Scope is "local" for engine events and "swarm" for cluster events.
	Scope string `json:"scope,omitempty"`

	// Attributes are the actor attributes AFTER redaction.
	Attributes map[string]string `json:"attributes"`

	ComposeProject string `json:"composeProject,omitempty"`
	ComposeService string `json:"composeService,omitempty"`
	// HarborMasterLabels holds io.harbormaster.* attributes with the prefix
	// stripped.
	HarborMasterLabels map[string]string `json:"harbormasterLabels,omitempty"`

	// DockerTime is when the daemon says the event happened, and DockerTimeNano
	// is the same instant in nanoseconds when the daemon supplied one (0
	// otherwise). Both are recorded because older daemons report only seconds,
	// and a second is not enough to order a burst.
	DockerTime     time.Time `json:"dockerTime"`
	DockerTimeNano int64     `json:"dockerTimeNano,omitempty"`
	// ObservedAt is when HarborMaster read the event off the stream. It differs
	// from DockerTime by the stream latency, and by much more after a
	// reconnect, which is exactly why both are kept.
	ObservedAt time.Time `json:"observedAt"`

	Result           EventProcessingResult `json:"result"`
	RefreshRequested RefreshRequest        `json:"refreshRequested"`
	// Error is a sanitised processing failure summary. Never a raw Docker
	// error, which can name the socket path.
	Error string `json:"error,omitempty"`
	// ConnectionState is the event engine's connection state at the moment this
	// event was processed.
	ConnectionState EventConnectionState `json:"connectionState,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

// EventConnectionState is the event stream's connection state.
type EventConnectionState string

// Connection states.
const (
	// ConnStateDisabled means the engine is switched off by configuration. This
	// is a deliberate operating mode, not a fault, and does not degrade health.
	ConnStateDisabled EventConnectionState = "disabled"
	// ConnStateConnecting means a connection attempt is in flight.
	ConnStateConnecting EventConnectionState = "connecting"
	// ConnStateConnected means the stream is open and being read.
	ConnStateConnected EventConnectionState = "connected"
	// ConnStateReconnecting means the stream dropped and a backoff delay is
	// being waited out.
	ConnStateReconnecting EventConnectionState = "reconnecting"
	// ConnStateStopped means the engine has shut down.
	ConnStateStopped EventConnectionState = "stopped"
)

// EventEngineCounters are the engine's lifetime tallies.
//
// Plain counters rather than a metrics framework: HarborMaster has no metrics
// dependency, and adding one to publish eleven integers would be a poor trade.
// They are exposed through the status endpoint and the logs.
type EventEngineCounters struct {
	// Received counts events read off the Docker stream.
	Received int64 `json:"eventsReceived"`
	// Persisted counts events written to the event table.
	Persisted int64 `json:"eventsPersisted"`
	// Deduplicated counts events suppressed by the in-memory window or by the
	// table's unique fingerprint constraint.
	Deduplicated int64 `json:"eventsDeduplicated"`
	// Dropped counts events discarded because the processing queue was full.
	// Every drop also requests a full reconciliation.
	Dropped int64 `json:"eventsDropped"`
	// TargetedRefreshes counts targeted refresh operations that ran.
	TargetedRefreshes int64 `json:"targetedRefreshes"`
	// FullReconciliations counts full reconciliations requested.
	FullReconciliations int64 `json:"fullReconciliations"`
	// Reconnects counts stream reconnection attempts after the first connect.
	Reconnects int64 `json:"reconnectCount"`
	// RefreshFailures counts targeted refresh operations that failed.
	RefreshFailures int64 `json:"refreshFailures"`
	// Pruned counts event rows removed by retention.
	Pruned int64 `json:"eventsPruned"`
}

// EventRetentionPolicy describes how long event history is kept.
type EventRetentionPolicy struct {
	// MaxAgeSeconds is the age past which events are pruned. Zero disables
	// age-based pruning.
	MaxAgeSeconds int64 `json:"maxAgeSeconds"`
	// MaxCount is the row ceiling; the oldest rows above it are pruned. Zero
	// disables count-based pruning.
	MaxCount int64 `json:"maxCount"`
	// IntervalSeconds is how often pruning runs.
	IntervalSeconds int64 `json:"intervalSeconds"`
}

// EventEngineStatus is the payload of GET /api/v1/event-engine.
type EventEngineStatus struct {
	// Enabled reflects configuration. When false nothing connects, and the
	// application is NOT degraded: running without live events is a supported
	// mode in which periodic reconciliation carries the inventory alone.
	Enabled bool                 `json:"enabled"`
	State   EventConnectionState `json:"state"`

	ConnectedSince   *time.Time `json:"connectedSince,omitempty"`
	LastConnectedAt  *time.Time `json:"lastConnectedAt,omitempty"`
	LastDisconnected *time.Time `json:"lastDisconnectedAt,omitempty"`
	LastEventAt      *time.Time `json:"lastEventAt,omitempty"`
	LastReconcileAt  *time.Time `json:"lastReconciliationAt,omitempty"`

	// CurrentBackoffMS is the delay before the next connection attempt. Zero
	// while connected.
	CurrentBackoffMS int64 `json:"currentBackoffMs"`

	// QueueDepth and QueueCapacity describe the processing queue. A depth
	// approaching capacity is the signal that events are arriving faster than
	// they can be persisted.
	QueueDepth    int `json:"queueDepth"`
	QueueCapacity int `json:"queueCapacity"`
	// PendingRefreshes is how many distinct resources are waiting for a
	// debounced refresh.
	PendingRefreshes int `json:"pendingRefreshes"`
	// OverflowPending is true when a queue overflow has forced a full
	// reconciliation that has not completed yet. The application reports
	// degraded until it does.
	OverflowPending bool `json:"overflowPending"`

	// Subscribers and SubscriberLimit describe the SSE stream.
	Subscribers     int `json:"subscribers"`
	SubscriberLimit int `json:"subscriberLimit"`

	Counters EventEngineCounters `json:"counters"`
	// LastError is a sanitised summary of the most recent failure. It never
	// carries a socket path, a daemon internal, or a credential.
	LastError string `json:"lastError,omitempty"`

	Retention EventRetentionPolicy `json:"retention"`
	// StoredEvents is how many events are currently held.
	StoredEvents int64 `json:"storedEvents"`
}
