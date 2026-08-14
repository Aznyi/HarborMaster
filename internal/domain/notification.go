package domain

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Notifications: telling an operator what happened, without telling anyone
// else.
//
// # Why this subsystem is the riskiest thing in HarborMaster to get wrong
//
// Everything else in the project reads a local socket and writes a local file.
// Image intelligence added outbound HTTPS to hosts derived from image
// references — hosts HarborMaster computed, never ones anybody typed.
//
// This subsystem sends data HarborMaster holds to a URL somebody typed. Both
// halves of that are new, and both are dangerous:
//
//   - The DESTINATION is operator-supplied, which is a server-side request
//     forgery primitive unless every address is checked at dial time.
//   - The PAYLOAD is assembled from records about containers, images, and
//     failures, which is exactly the material that must never leave the host.
//
// So the payload is built from a CLOSED SET of fields, each of which is either
// an identifier HarborMaster generated or a sentence HarborMaster wrote. There
// is no field on Notification that can hold an environment value, a registry
// credential, a session token, or a raw Docker error, and the redaction that
// protects the database protects this too because it happens before storage.
//
// # No templating that can execute anything
//
// A destination may set a title prefix and nothing else. There is no template
// language, no expression evaluation, no field interpolation from user text.
// A notification's shape is decided by the channel's code, from typed fields.

// ------------------------------------------------------------------ events --

// NotificationEvent is the closed vocabulary of things worth telling somebody
// about.
//
// Closed so a rule can select from a list, so a UI can render a label, and so
// adding one is a deliberate decision rather than a string appearing at a call
// site.
type NotificationEvent string

// The events. Grouped by the subsystem that raises them.
const (
	// Update lifecycle.
	EventUpdateDiscovered NotificationEvent = "update.discovered"
	EventApprovalRequired NotificationEvent = "update.approvalRequired"

	EventAcquisitionSucceeded NotificationEvent = "acquisition.succeeded"
	EventAcquisitionFailed    NotificationEvent = "acquisition.failed"

	EventExecutionSucceeded NotificationEvent = "execution.succeeded"
	EventExecutionFailed    NotificationEvent = "execution.failed"

	// Rollback. Started as well as finished, because the interesting minute is
	// the one in between: a container is down and somebody may want to watch.
	EventRollbackStarted   NotificationEvent = "rollback.started"
	EventRollbackSucceeded NotificationEvent = "rollback.succeeded"
	EventRollbackFailed    NotificationEvent = "rollback.failed"

	// Automation.
	EventAutomationPaused NotificationEvent = "automation.paused"
	EventSchedulerError   NotificationEvent = "automation.error"

	// EventRebindFailed reports that a container sharing a replaced provider's
	// namespace could not be reattached to the replacement.
	//
	// # Why this one is worth a message and the other dependency states are not
	//
	// Because of what it means on the host and because nothing will fix it. The
	// dependent may be attached to a namespace that no longer exists — Docker
	// reports nothing, the container keeps running, and its network silently
	// stops working. HarborMaster does not retry a reattachment by itself.
	//
	// A dependency LOOP is a configuration problem that changes nothing about a
	// running container and would be re-raised on every pass. A container
	// WAITING on its dependency is the system working. Neither is news.
	EventRebindFailed NotificationEvent = "dependency.rebindFailed"

	// Observation.
	EventDriftDetected   NotificationEvent = "drift.detected"
	EventPolicyViolation NotificationEvent = "policy.violation"

	// Infrastructure.
	EventRegistryUnavailable NotificationEvent = "registry.unavailable"
	EventBackupFailed        NotificationEvent = "backup.failed"
	EventIntegrityFailed     NotificationEvent = "integrity.failed"

	// EventTest is a delivery an operator asked for, to prove a destination
	// works. Never raised by the system, and deliberately part of the same
	// vocabulary so it travels the same path with the same checks.
	EventTest NotificationEvent = "test"
)

// NotificationEvents lists every event, in the order a rule editor shows them.
var NotificationEvents = []NotificationEvent{
	EventUpdateDiscovered, EventApprovalRequired,
	EventAcquisitionSucceeded, EventAcquisitionFailed,
	EventExecutionSucceeded, EventExecutionFailed,
	EventRollbackStarted, EventRollbackSucceeded, EventRollbackFailed,
	EventAutomationPaused, EventSchedulerError, EventRebindFailed,
	EventDriftDetected, EventPolicyViolation,
	EventRegistryUnavailable, EventBackupFailed, EventIntegrityFailed,
	EventTest,
}

// ValidNotificationEvent reports whether value names an event.
func ValidNotificationEvent(value string) bool {
	for _, event := range NotificationEvents {
		if string(event) == value {
			return true
		}
	}
	return false
}

// DefaultSeverity is the severity an event carries unless the raiser says
// otherwise.
//
// A property of the EVENT rather than of the notification, so a rule's
// threshold means the same thing whoever raised it. A failed recreation is a
// warning wherever it came from.
func (e NotificationEvent) DefaultSeverity() NotificationSeverity {
	switch e {
	case EventExecutionFailed, EventRollbackFailed, EventAutomationPaused,
		EventBackupFailed, EventIntegrityFailed,
		// Critical rather than warning: the container may be attached to a
		// namespace that no longer exists, Docker reports nothing about that,
		// and HarborMaster will not retry. It is a workload that may be down
		// with no other signal.
		EventRebindFailed:
		return NotifyCritical
	case EventAcquisitionFailed, EventRollbackStarted, EventSchedulerError,
		EventRegistryUnavailable, EventPolicyViolation:
		return NotifyWarning
	default:
		return NotifyInfo
	}
}

// Describe renders an event in operator-facing words.
func (e NotificationEvent) Describe() string {
	switch e {
	case EventUpdateDiscovered:
		return "a newer image is available for a container"
	case EventApprovalRequired:
		return "an automated update is waiting for a person to release it"
	case EventRebindFailed:
		return "a container sharing a replaced container's namespace could not be reattached"
	case EventAcquisitionSucceeded:
		return "an image was downloaded and its digest confirmed"
	case EventAcquisitionFailed:
		return "an image could not be downloaded"
	case EventExecutionSucceeded:
		return "a container was recreated on a new image and proved"
	case EventExecutionFailed:
		return "a container recreation did not succeed"
	case EventRollbackStarted:
		return "a container is being rolled back, and is briefly unavailable"
	case EventRollbackSucceeded:
		return "a container was returned to its previous image"
	case EventRollbackFailed:
		return "a rollback did not finish, and containers may need attention"
	case EventAutomationPaused:
		return "automation stopped touching a container"
	case EventSchedulerError:
		return "an automation pass could not complete"
	case EventDriftDetected:
		return "a container's configuration moved away from its baseline"
	case EventPolicyViolation:
		return "a container stopped complying with a policy"
	case EventRegistryUnavailable:
		return "a registry could not be reached"
	case EventBackupFailed:
		return "a database backup did not complete"
	case EventIntegrityFailed:
		return "a database integrity check found a problem"
	case EventTest:
		return "a test delivery, sent by an operator"
	default:
		return string(e)
	}
}

// --------------------------------------------------------------- severity --

// NotificationSeverity is how much an event matters.
//
// Three levels, deliberately. A fourth would be a level nobody could define
// consistently, and a threshold nobody could set with confidence.
type NotificationSeverity string

const (
	// NotifyInfo is something that happened and went well.
	//
	// Named apart from the EventSeverity constants in event.go: the two
	// vocabularies overlap in wording and mean different things -- one grades
	// an entry in HarborMaster's own activity log, the other decides whether a
	// message leaves the host -- and a shared name would let one be passed
	// where the other belongs.
	NotifyInfo NotificationSeverity = "info"
	// NotifyWarning is something that needs looking at, eventually.
	NotifyWarning NotificationSeverity = "warning"
	// NotifyCritical is something that needs looking at now: a container may
	// be down, or the record of what happened may be incomplete.
	NotifyCritical NotificationSeverity = "critical"
)

// NotificationSeverities lists every severity, least to most urgent.
var NotificationSeverities = []NotificationSeverity{
	NotifyInfo, NotifyWarning, NotifyCritical,
}

// ValidNotificationSeverity reports whether value names a severity.
func ValidNotificationSeverity(value string) bool {
	for _, severity := range NotificationSeverities {
		if string(severity) == value {
			return true
		}
	}
	return false
}

// rank orders severities for the threshold comparison.
//
// An unknown severity ranks BELOW info, so a value this build does not
// understand never satisfies a threshold. Fails towards "not sent", which is
// the safe direction for a subsystem that talks to the internet.
func (s NotificationSeverity) rank() int {
	switch s {
	case NotifyInfo:
		return 1
	case NotifyWarning:
		return 2
	case NotifyCritical:
		return 3
	default:
		return 0
	}
}

// AtLeast reports whether this severity meets a threshold.
func (s NotificationSeverity) AtLeast(threshold NotificationSeverity) bool {
	return s.rank() > 0 && s.rank() >= threshold.rank()
}

// ---------------------------------------------------------- the payload --

// Notification is one thing worth telling somebody about.
//
// # Every field is HarborMaster's own
//
// There is nowhere on this type to put an environment value, a registry
// credential, a session token, or a raw Docker error. Identifiers are ones
// HarborMaster generated; Title and Body are sentences HarborMaster wrote from
// a fixed vocabulary; Fields is a bounded list of label/value pairs the raiser
// chose from its own records.
//
// That is the whole secret control for this subsystem. It is structural: a
// caller cannot leak a secret through a type with no field for one.
type Notification struct {
	Event    NotificationEvent    `json:"event"`
	Severity NotificationSeverity `json:"severity"`

	// Title is one line. Body is at most a short paragraph. Both are written by
	// HarborMaster.
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`

	// ContainerName is the container this concerns, when it concerns one. A
	// NAME rather than an id: it is what an operator recognises, and an id
	// changes on recreation.
	ContainerName string `json:"containerName,omitempty"`

	// Fields are additional label/value pairs, bounded in count and length.
	// Every value is an identifier or a fixed phrase.
	Fields []NotificationField `json:"fields,omitempty"`

	// DedupKey identifies "the same thing happening again". Two notifications
	// with one key inside a rule's window produce one delivery. Derived by the
	// raiser from its own records -- never from time, or every one would be
	// unique and deduplication would do nothing.
	DedupKey string `json:"dedupKey,omitempty"`

	// OccurredAt is when the thing happened, which is not necessarily when the
	// notification is delivered.
	OccurredAt time.Time `json:"occurredAt"`
}

// NotificationField is one label/value pair on a notification.
type NotificationField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Bounds on a notification's own content.
//
// Applied by Sanitise before anything is stored or sent. Every one of these is
// reached only by a programming error -- HarborMaster writes these strings --
// and every one exists because a bound that is never reached costs nothing and
// a missing one is discovered by the payload that reaches it.
const (
	MaxNotificationTitleBytes = 200
	MaxNotificationBodyBytes  = 2000
	MaxNotificationFields     = 12
	MaxNotificationFieldLabel = 60
	MaxNotificationFieldValue = 300
	MaxDedupKeyBytes          = 200
)

// Sanitise bounds and cleans a notification before it is stored or sent.
//
// The last line of defence rather than the first: every caller builds these
// from its own records, and this exists so that a caller which did not is
// still safe. Control characters are stripped, lengths are bounded, and empty
// fields are dropped.
func (n Notification) Sanitise() Notification {
	n.Title = SanitiseDisplayText(n.Title, MaxNotificationTitleBytes)
	n.Body = SanitiseDisplayText(n.Body, MaxNotificationBodyBytes)
	n.ContainerName = SanitiseDisplayText(n.ContainerName, MaxContainerNameBytes)
	n.DedupKey = SanitiseDisplayText(n.DedupKey, MaxDedupKeyBytes)

	if !ValidNotificationSeverity(string(n.Severity)) {
		n.Severity = n.Event.DefaultSeverity()
	}
	if n.OccurredAt.IsZero() {
		// Left to the caller to set; a zero here would render as year one.
		n.OccurredAt = time.Time{}
	}

	fields := make([]NotificationField, 0, len(n.Fields))
	for _, field := range n.Fields {
		label := SanitiseDisplayText(field.Label, MaxNotificationFieldLabel)
		value := SanitiseDisplayText(field.Value, MaxNotificationFieldValue)
		if label == "" || value == "" {
			continue
		}
		fields = append(fields, NotificationField{Label: label, Value: value})
		if len(fields) == MaxNotificationFields {
			break
		}
	}
	n.Fields = fields
	return n
}

// ------------------------------------------------------------ destinations --

// NotificationChannel names how a destination is reached.
type NotificationChannel string

const (
	// ChannelWebhook posts HarborMaster's own JSON document to a URL.
	ChannelWebhook NotificationChannel = "webhook"
	// ChannelDiscord, ChannelSlack, and ChannelTeams post the shape each of
	// those services expects, to that service's incoming-webhook URL.
	ChannelDiscord NotificationChannel = "discord"
	ChannelSlack   NotificationChannel = "slack"
	ChannelTeams   NotificationChannel = "teams"
	// ChannelEmail sends over SMTP.
	ChannelEmail NotificationChannel = "email"
)

// NotificationChannels lists every channel.
var NotificationChannels = []NotificationChannel{
	ChannelWebhook, ChannelDiscord, ChannelSlack, ChannelTeams, ChannelEmail,
}

// ValidNotificationChannel reports whether value names a channel.
func ValidNotificationChannel(value string) bool {
	for _, channel := range NotificationChannels {
		if string(channel) == value {
			return true
		}
	}
	return false
}

// WebhookBased reports whether a channel is reached by posting to a URL.
func (c NotificationChannel) WebhookBased() bool {
	switch c {
	case ChannelWebhook, ChannelDiscord, ChannelSlack, ChannelTeams:
		return true
	default:
		return false
	}
}

// NotificationDestinationIDPrefix and its length shape a destination id.
const (
	NotificationDestinationIDPrefix    = "ndst_"
	NotificationDestinationIDHexLength = 20
)

// NewNotificationDestinationID generates a destination identifier.
func NewNotificationDestinationID() string {
	return prefixedRandomID(NotificationDestinationIDPrefix, NotificationDestinationIDHexLength)
}

// ValidNotificationDestinationID reports whether id has the generated shape.
func ValidNotificationDestinationID(id string) bool {
	return validPrefixedID(id, NotificationDestinationIDPrefix, NotificationDestinationIDHexLength)
}

// NotificationDestination is somewhere notifications are sent.
//
// # What is NOT on this type
//
// The webhook URL and the SMTP password are not fields here. They live in
// NotificationSecret, which the API never returns and the repository stores
// separately — see notification_secret.go. This type is the part that is safe
// to render, log, and serve, and it is the part every read path uses.
type NotificationDestination struct {
	ID            int64  `json:"-"`
	DestinationID string `json:"destinationId"`

	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Channel     NotificationChannel `json:"channel"`
	Enabled     bool                `json:"enabled"`

	// Endpoint is the SAFE rendering of where this goes: a scheme and host for
	// a webhook, an address for email. Never the full URL, because a webhook
	// URL's path is the credential for Slack, Discord, and Teams alike.
	Endpoint string `json:"endpoint"`

	// TitlePrefix is the one piece of operator text that reaches a delivered
	// message. Bounded, sanitised, and inserted as TEXT -- never as markup, and
	// never interpolated into anything a receiver would evaluate.
	TitlePrefix string `json:"titlePrefix,omitempty"`

	// EmailTo is the recipient list for an email destination, bounded in count.
	// Empty for every webhook channel.
	EmailTo []string `json:"emailTo,omitempty"`
	// EmailFrom is the envelope sender.
	EmailFrom string `json:"emailFrom,omitempty"`

	// Health is what happened the last time HarborMaster tried.
	LastResult    DeliveryResult `json:"lastResult,omitempty"`
	LastAttemptAt *time.Time     `json:"lastAttemptAt,omitempty"`
	LastError     string         `json:"lastError,omitempty"`
	// ConsecutiveFailures counts failures since the last success, which is what
	// the UI turns into "this destination is not working".
	ConsecutiveFailures int `json:"consecutiveFailures"`

	Archived  bool   `json:"archived"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Bounds on a destination.
const (
	MaxDestinationNameBytes        = 120
	MaxDestinationDescriptionBytes = 500
	MaxTitlePrefixBytes            = 60
	MaxEmailRecipients             = 20
	MaxEmailAddressBytes           = 320
	MaxEndpointBytes               = 300
)

// ------------------------------------------------------------------ rules --

// NotificationRuleIDPrefix and its length shape a rule id.
const (
	NotificationRuleIDPrefix    = "nrul_"
	NotificationRuleIDHexLength = 20
)

// NewNotificationRuleID generates a rule identifier.
func NewNotificationRuleID() string {
	return prefixedRandomID(NotificationRuleIDPrefix, NotificationRuleIDHexLength)
}

// ValidNotificationRuleID reports whether id has the generated shape.
func ValidNotificationRuleID(id string) bool {
	return validPrefixedID(id, NotificationRuleIDPrefix, NotificationRuleIDHexLength)
}

// NotificationRule decides which events reach which destinations.
//
// # Deliberately not a compliance policy and not an update policy
//
// Three subsystems in HarborMaster now have something called a policy, and they
// do entirely different things: one REPORTS, one ACTS, and this one ROUTES.
// Sharing a type between any two of them would mean an edit to one could change
// the behaviour of another.
type NotificationRule struct {
	ID     int64  `json:"-"`
	RuleID string `json:"ruleId"`

	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	// Events this rule matches. EMPTY MEANS EVERY EVENT, which is the useful
	// default for a first rule -- unlike an update policy's selector, where
	// empty means nothing, because the cost of being wrong here is an extra
	// message rather than an unintended container change.
	Events []NotificationEvent `json:"events,omitempty"`

	// MinimumSeverity is the threshold. An event below it does not match.
	MinimumSeverity NotificationSeverity `json:"minimumSeverity"`

	// Destinations this rule delivers to. A rule with none delivers nothing,
	// and validation refuses one.
	Destinations []string `json:"destinations"`

	// CooldownSeconds suppresses a repeat of the same DedupKey within the
	// window. Zero disables suppression, which is honest rather than
	// convenient: an operator who wants every occurrence should get every one.
	CooldownSeconds int `json:"cooldownSeconds"`

	Archived  bool   `json:"archived"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Bounds on a rule.
const (
	MaxRuleNameBytes      = 120
	MaxRuleDestinations   = 20
	MaxRuleEvents         = 32
	MaxRuleCooldownSecond = 24 * 60 * 60
)

// Matches reports whether a rule routes a notification.
//
// Pure, and the only place the routing decision is made. A disabled or archived
// rule matches nothing; an event below the threshold matches nothing; an empty
// event list matches every event.
func (r NotificationRule) Matches(notification Notification) bool {
	if !r.Enabled || r.Archived {
		return false
	}
	if !notification.Severity.AtLeast(r.MinimumSeverity) {
		return false
	}
	if len(r.Events) == 0 {
		return true
	}
	for _, event := range r.Events {
		if event == notification.Event {
			return true
		}
	}
	return false
}

// Cooldown returns the suppression window as a duration.
func (r NotificationRule) Cooldown() time.Duration {
	if r.CooldownSeconds <= 0 {
		return 0
	}
	return time.Duration(r.CooldownSeconds) * time.Second
}

// --------------------------------------------------------------- delivery --

// NotificationDeliveryIDPrefix and its length shape a delivery id.
const (
	NotificationDeliveryIDPrefix    = "ndlv_"
	NotificationDeliveryIDHexLength = 20
)

// NewNotificationDeliveryID generates a delivery identifier.
func NewNotificationDeliveryID() string {
	return prefixedRandomID(NotificationDeliveryIDPrefix, NotificationDeliveryIDHexLength)
}

// ValidNotificationDeliveryID reports whether id has the generated shape.
func ValidNotificationDeliveryID(id string) bool {
	return validPrefixedID(id, NotificationDeliveryIDPrefix, NotificationDeliveryIDHexLength)
}

// DeliveryResult is how one attempt ended.
type DeliveryResult string

const (
	// DeliveryPending means queued and not yet attempted.
	DeliveryPending DeliveryResult = "pending"
	// DeliverySucceeded means the destination accepted it.
	DeliverySucceeded DeliveryResult = "succeeded"
	// DeliveryFailed means every attempt failed and no more will be made. The
	// dead letter.
	DeliveryFailed DeliveryResult = "failed"
	// DeliveryRetrying means an attempt failed and another is scheduled.
	DeliveryRetrying DeliveryResult = "retrying"
	// DeliverySuppressed means a rule's cooldown swallowed it. Recorded rather
	// than dropped: an operator asking "why did I not get told" needs to see
	// that HarborMaster decided not to, and why.
	DeliverySuppressed DeliveryResult = "suppressed"
	// DeliveryDropped means the queue was full. The one outcome that means
	// HarborMaster lost something, and it is recorded as loudly as it can be.
	DeliveryDropped DeliveryResult = "dropped"
)

// DeliveryResults lists every result.
var DeliveryResults = []DeliveryResult{
	DeliveryPending, DeliverySucceeded, DeliveryFailed,
	DeliveryRetrying, DeliverySuppressed, DeliveryDropped,
}

// ValidDeliveryResult reports whether value names a result.
func ValidDeliveryResult(value string) bool {
	for _, result := range DeliveryResults {
		if string(result) == value {
			return true
		}
	}
	return false
}

// Terminal reports whether no further attempt will be made.
func (r DeliveryResult) Terminal() bool {
	switch r {
	case DeliverySucceeded, DeliveryFailed, DeliverySuppressed, DeliveryDropped:
		return true
	default:
		return false
	}
}

// Explain renders a result in operator-facing words.
func (r DeliveryResult) Explain() string {
	switch r {
	case DeliveryPending:
		return "queued, not yet attempted"
	case DeliverySucceeded:
		return "the destination accepted it"
	case DeliveryFailed:
		return "every attempt failed; no more will be made"
	case DeliveryRetrying:
		return "an attempt failed and another is scheduled"
	case DeliverySuppressed:
		return "a rule's cooldown suppressed a repeat of the same event"
	case DeliveryDropped:
		return "the delivery queue was full and this was not sent"
	default:
		return string(r)
	}
}

// NotificationDelivery is the record of one notification going to one
// destination.
//
// # Why the whole payload is stored
//
// So "what did HarborMaster actually send" is answerable, and so a failed
// delivery can be understood without reproducing the conditions. The payload is
// the sanitised notification, which by construction contains nothing sensitive.
//
// # What is never stored
//
// The destination URL, the SMTP password, and the response body. The first two
// are credentials; the third is third-party text that would carry into a page
// an operator reads. What IS stored is the status code and HarborMaster's own
// sentence about it.
type NotificationDelivery struct {
	ID         int64  `json:"-"`
	DeliveryID string `json:"deliveryId"`

	DestinationID   string              `json:"destinationId"`
	DestinationName string              `json:"destinationName"`
	Channel         NotificationChannel `json:"channel"`
	RuleID          string              `json:"ruleId,omitempty"`
	RuleName        string              `json:"ruleName,omitempty"`

	Event    NotificationEvent    `json:"event"`
	Severity NotificationSeverity `json:"severity"`
	Title    string               `json:"title"`
	Body     string               `json:"body,omitempty"`
	// ContainerName is denormalised so the history can be filtered without
	// decoding every payload.
	ContainerName string `json:"containerName,omitempty"`

	Result   DeliveryResult `json:"result"`
	Attempts int            `json:"attempts"`
	// StatusCode is the HTTP status a webhook returned, when there was one.
	// Zero for email and for a failure that never reached a response.
	StatusCode int `json:"statusCode,omitempty"`
	// Error is HarborMaster's own sentence about the failure, from a fixed
	// vocabulary. NEVER the transport's error text: that carries hostnames,
	// addresses, and occasionally the URL itself.
	Error string `json:"error,omitempty"`

	// DedupKey is what suppression compared on.
	DedupKey string `json:"dedupKey,omitempty"`

	QueuedAt    time.Time  `json:"queuedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	// NextAttemptAt is when a retrying delivery will be tried again.
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	DurationMs    int64      `json:"durationMs,omitempty"`
}

// NotificationSummary is the aggregate the dashboard and the page header show.
type NotificationSummary struct {
	Enabled      bool `json:"enabled"`
	Destinations int  `json:"destinations"`
	Rules        int  `json:"rules"`
	// Failing counts destinations whose last attempt failed. The one number an
	// operator needs: a notification subsystem nobody notices has broken is
	// worse than none at all.
	Failing int `json:"failing"`

	Delivered  int `json:"delivered"`
	Failed     int `json:"failed"`
	Suppressed int `json:"suppressed"`
	Dropped    int `json:"dropped"`
	Pending    int `json:"pending"`
}

// ------------------------------------------------------------- identifiers --

// prefixedRandomID generates an identifier of the project's standard shape.
//
// Shared by the three notification id kinds rather than written three times.
// The entropy source is the system's; a failure to read it panics, because a
// predictable identifier is worse than a crash and there is no sensible way to
// continue without one.
func prefixedRandomID(prefix string, hexLength int) string {
	raw := make([]byte, hexLength/2)
	if _, err := rand.Read(raw); err != nil {
		panic("notification id: " + err.Error())
	}
	return prefix + hex.EncodeToString(raw)
}

// validPrefixedID reports whether id has the generated shape.
func validPrefixedID(id, prefix string, hexLength int) bool {
	if len(id) != len(prefix)+hexLength {
		return false
	}
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	for _, r := range id[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// SortNotificationEvents orders events for display, deterministically.
func SortNotificationEvents(events []NotificationEvent) {
	sort.Slice(events, func(i, j int) bool { return events[i] < events[j] })
}
