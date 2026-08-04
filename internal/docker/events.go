package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Docker event subscription.
//
// This file is the ONLY place that knows what a Docker SDK event looks like.
// Everything it hands out is a domain.DockerEvent: a HarborMaster-owned,
// already-redacted record. No events.Message, no filters.Args, and no raw
// SDK channel crosses this package boundary, so the rest of the application
// cannot come to depend on the SDK's shape.
//
// The adapter reads and converts. It does not decide what an event MEANS for
// the inventory -- that classification lives in the service layer, because it
// is application policy rather than protocol translation.

// EventSubscription is a live view of the Docker event stream.
//
// Ownership: the caller owns the subscription and must ensure the context it
// was created with is cancelled, which is what closes the underlying HTTP
// response and lets the SDK's reader goroutine exit. Both channels are
// receive-only; nothing outside this package may close them.
//
// Errors is buffered and receives exactly one terminal error, after which the
// subscription is finished and must be recreated. Events is closed when the
// converter goroutine exits.
type EventSubscription struct {
	// Events carries normalized, redacted events in the order the daemon sent
	// them on this connection. Docker guarantees ordering only within a single
	// connection; nothing is guaranteed across a reconnect.
	Events <-chan domain.DockerEvent
	// Errors carries the single terminal error that ended the stream. An
	// io.EOF or a context cancellation both arrive here.
	Errors <-chan error
}

// subscribedTypes are the event types HarborMaster asks the daemon for.
//
// Filtering server-side keeps builder, plugin, service, node, secret, and
// config events off the wire entirely: HarborMaster models none of them, and
// on a busy build host they would dominate the stream.
//
// Actions are deliberately NOT filtered. Daemons differ by version in which
// actions they emit and in how they spell them, so an action allowlist sent to
// the daemon would silently drop events on some hosts. Classification happens
// locally instead, where an unrecognised action is visible rather than absent.
var subscribedTypes = []string{
	string(events.ContainerEventType),
	string(events.ImageEventType),
	string(events.NetworkEventType),
	string(events.VolumeEventType),
	string(events.DaemonEventType),
}

// StreamEvents subscribes to the Docker event stream.
//
// `since` resumes from a point in time where the daemon still has history.
// Pass the zero time to start from now. Resumption is best-effort and is not a
// durability guarantee: the daemon keeps only a bounded, in-memory ring of
// recent events, and nothing survives a daemon restart. That is precisely why
// HarborMaster runs a periodic full reconciliation regardless.
//
// The returned subscription lives until ctx is cancelled or the stream fails.
// This call does not block waiting for the daemon; an unreachable socket
// surfaces on the Errors channel, so a Docker that is down at startup is a
// runtime condition rather than a startup failure.
func (c *Client) StreamEvents(ctx context.Context, since time.Time) (*EventSubscription, error) {
	// client.Filters replaces filters.Args. Its zero value is documented as
	// read-only, so the map is made before Add is called; Add returns the map
	// it mutated, which is why the result is reassigned.
	args := make(client.Filters).Add("type", subscribedTypes...)

	options := client.EventsListOptions{Filters: args}
	if !since.IsZero() {
		// Nanosecond precision so a resume cannot replay the event it resumed
		// from, and cannot skip one that shared its second.
		options.Since = strconv.FormatInt(since.Unix(), 10) + "." +
			fmt.Sprintf("%09d", since.Nanosecond())
	}

	// streamAPI, not api: the ordinary client carries a request timeout, and
	// an HTTP client timeout applies to the whole exchange including the body.
	// Using it here would tear the event stream down every DOCKER_TIMEOUT
	// seconds, which would look exactly like a flapping daemon.
	// Events now returns a single result struct rather than two bare channels.
	// The channels inside it carry the same semantics as before.
	stream := c.streamAPI.Events(ctx, options)
	messages, errs := stream.Messages, stream.Err

	converted := make(chan domain.DockerEvent)

	// One goroutine, owned by this subscription, converting SDK messages into
	// domain events. It exits when the SDK closes `messages` or when ctx is
	// cancelled, and it closes `converted` on the way out -- it is the only
	// writer, so there is exactly one closer and no send-on-closed race.
	go func() {
		defer close(converted)
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-messages:
				if !ok {
					return
				}
				select {
				case converted <- c.convertEvent(message):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return &EventSubscription{Events: converted, Errors: errs}, nil
}

// convertEvent normalizes one SDK message into a domain event.
//
// It never fails. A message with an unknown type, an unknown action, no actor,
// or no timestamp still produces a record: an event HarborMaster cannot
// interpret is information about the host, and dropping it silently would make
// the history lie about what was observed.
func (c *Client) convertEvent(message events.Message) domain.DockerEvent {
	eventType := normalizeEventType(message.Type)
	action, actionDetail := splitAction(message.Action)

	attributes := c.redactAttributes(message.Actor.Attributes)

	// The action suffix (a health status, an exec command line) is preserved as
	// an attribute rather than folded into the action, so filtering by action
	// stays possible while the detail is not lost.
	if actionDetail != "" {
		attributes["harbormaster.actionDetail"] = actionDetail
	}

	dockerTime, timeNano := eventTimestamp(message)

	event := domain.DockerEvent{
		HostID:             domain.LocalHostID,
		Type:               eventType,
		Action:             action,
		ActorID:            strings.TrimSpace(actorID(message)),
		Scope:              scopeOrLocal(message.Scope),
		Attributes:         attributes,
		DockerTime:         dockerTime,
		DockerTimeNano:     timeNano,
		ComposeProject:     attributes[domain.LabelComposeProject],
		ComposeService:     attributes[domain.LabelComposeService],
		HarborMasterLabels: harborMasterAttributes(attributes),
	}
	event.ActorName = resourceName(eventType, event.ActorID, attributes)
	event.Fingerprint = EventFingerprint(event)
	return event
}

// actorID resolves the actor ID.
//
// The v28 SDK also carried a deprecated top-level Message.ID, which this
// function used as a fallback for daemons old enough to leave Actor.ID empty.
// The moby API module removed that field, so the fallback is gone and cannot be
// reinstated from the typed message. Actor.ID has been populated by every
// daemon since API 1.22, which is far below the versions this client can
// negotiate, so the practical effect is nil -- but an empty actor is still
// tolerated here rather than treated as an error.
func actorID(message events.Message) string {
	return message.Actor.ID
}

// eventTimestamp resolves the event time and its nanosecond form.
//
// Some daemons send only whole seconds. When TimeNano is absent the returned
// nanosecond value is 0, which is the signal EventFingerprint uses to widen the
// identity so two events in the same second are still told apart.
func eventTimestamp(message events.Message) (time.Time, int64) {
	switch {
	case message.TimeNano > 0:
		return time.Unix(0, message.TimeNano).UTC(), message.TimeNano
	case message.Time > 0:
		return time.Unix(message.Time, 0).UTC(), 0
	default:
		// A daemon that sent no timestamp at all. Recording the zero time would
		// put the event at the start of the epoch in every ordering; the
		// observation time is assigned by the service layer instead.
		return time.Time{}, 0
	}
}

func scopeOrLocal(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return "local"
	}
	return scope
}

// normalizeEventType maps an SDK event type onto HarborMaster's vocabulary.
//
// Anything HarborMaster does not model becomes EventTypeOther rather than
// being passed through: an open string here would let an unexpected daemon
// value reach a database CHECK constraint and fail an insert.
func normalizeEventType(eventType events.Type) domain.DockerEventType {
	switch domain.DockerEventType(strings.ToLower(strings.TrimSpace(string(eventType)))) {
	case domain.EventTypeContainer:
		return domain.EventTypeContainer
	case domain.EventTypeImage:
		return domain.EventTypeImage
	case domain.EventTypeNetwork:
		return domain.EventTypeNetwork
	case domain.EventTypeVolume:
		return domain.EventTypeVolume
	case domain.EventTypeDaemon:
		return domain.EventTypeDaemon
	default:
		return domain.EventTypeOther
	}
}

// splitAction separates an action from its free-form suffix.
//
// Docker overloads the action field for two cases, both documented in the SDK
// as compromises for backwards compatibility:
//
//	health_status: healthy
//	exec_create: /bin/sh -c 'echo hi'
//
// Splitting on the first colon recovers a filterable action and keeps the
// detail. An exec command line is returned as detail and, being potentially
// sensitive, is redacted by the caller along with every other attribute.
func splitAction(action events.Action) (domain.DockerEventAction, string) {
	raw := strings.TrimSpace(string(action))
	if raw == "" {
		return "", ""
	}

	verb, detail, found := strings.Cut(raw, ":")
	verb = strings.ToLower(strings.TrimSpace(verb))
	if !found {
		return domain.DockerEventAction(verb), ""
	}
	return domain.DockerEventAction(verb), strings.TrimSpace(detail)
}

// redactAttributes copies actor attributes, masking every value whose KEY
// matches a sensitive-name pattern.
//
// Actor attributes are the resource's labels plus a handful of daemon-supplied
// fields, and a label is an arbitrary operator-supplied key/value pair -- so
// they can and do carry credentials. Redaction happens here, at the adapter
// boundary, because every downstream consumer (logs, SQLite, the REST API, the
// unauthenticated SSE stream) would otherwise each need to remember to do it.
//
// The returned map is always non-nil, so callers never have to guard a lookup.
func (c *Client) redactAttributes(attributes map[string]string) map[string]string {
	out := make(map[string]string, len(attributes))
	for key, value := range attributes {
		if c.masker.IsSensitive(key) {
			out[key] = domain.MaskedValue
			continue
		}
		out[key] = value
	}
	return out
}

// resourceName resolves the human-readable name for the event's subject.
//
// Docker is inconsistent about where the name lives: containers and networks
// carry "name", images carry a reference in "name", and volumes use the volume
// name as the actor ID itself. Falling back to a shortened actor ID keeps every
// event renderable rather than showing a blank cell.
func resourceName(eventType domain.DockerEventType, actor string, attributes map[string]string) string {
	if name := strings.TrimSpace(attributes["name"]); name != "" {
		return name
	}
	if eventType == domain.EventTypeImage {
		// Image events name the reference in "image" on some daemon versions.
		if ref := strings.TrimSpace(attributes["image"]); ref != "" {
			return ref
		}
	}
	if actor == "" {
		return ""
	}
	// A volume's actor ID is its name, and an image event's actor may be a
	// reference: neither should be shortened as though it were a hex digest.
	if eventType == domain.EventTypeVolume || !looksLikeID(actor) {
		return actor
	}
	return domain.ShortenID(actor)
}

// looksLikeID reports whether a string is a Docker content hash.
func looksLikeID(value string) bool {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) < 12 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// harborMasterAttributes extracts io.harbormaster.* attributes with the prefix
// stripped. Returns nil when there are none, so the JSON field is omitted.
func harborMasterAttributes(attributes map[string]string) map[string]string {
	var out map[string]string
	for key, value := range attributes {
		if !strings.HasPrefix(key, domain.HarborMasterLabelPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[strings.TrimPrefix(key, domain.HarborMasterLabelPrefix)] = value
	}
	return out
}

// fingerprintAttributes are the attribute keys folded into an event's identity.
//
// A short, fixed list rather than every attribute: a container's attributes
// include its whole label set, so hashing all of them would make the identity
// depend on data unrelated to the event, and a label edit between two otherwise
// identical events would defeat deduplication. These keys are the ones that
// distinguish two events that would otherwise collide -- a rename's old and new
// names, a health transition's status, a network attach's container.
var fingerprintAttributes = []string{
	"name",
	"oldName",
	"image",
	"container",
	"health_status",
	"exitCode",
	"signal",
	"harbormaster.actionDetail",
}

// EventFingerprint derives the deterministic deduplication identity of an
// event.
//
// The identity is built from stable fields only -- host, type, action, actor,
// and the daemon's own nanosecond timestamp -- so the same event seen twice
// (a duplicate delivery, or a replay after a reconnect that overlapped) hashes
// the same, while a genuinely repeated action at a different instant does not.
// That distinction is the whole point: suppressing a real second `start`
// because it matched the first would lose a state transition.
//
// When the daemon supplied no nanosecond timestamp, whole seconds alone are too
// coarse to separate a burst, so the identity widens to include every actor
// attribute in sorted order. That is more sensitive to noise but never merges
// two distinct events, which is the safer failure direction.
//
// Exported so tests can assert the identity is stable for a given input, and so
// the property "same input, same fingerprint" is checkable outside this file.
func EventFingerprint(event domain.DockerEvent) string {
	hash := sha256.New()

	write := func(parts ...string) {
		for _, part := range parts {
			// The separator is a byte that cannot occur in any of these fields,
			// so "ab"+"c" and "a"+"bc" cannot collide.
			_, _ = hash.Write([]byte(part))
			_, _ = hash.Write([]byte{0})
		}
	}

	write(
		event.HostID,
		string(event.Type),
		string(event.Action),
		event.ActorID,
		event.Scope,
	)

	if event.DockerTimeNano > 0 {
		write(strconv.FormatInt(event.DockerTimeNano, 10))
		for _, key := range fingerprintAttributes {
			write(key, event.Attributes[key])
		}
	} else {
		write("seconds", strconv.FormatInt(event.DockerTime.Unix(), 10))
		keys := make([]string, 0, len(event.Attributes))
		for key := range event.Attributes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			write(key, event.Attributes[key])
		}
	}

	return hex.EncodeToString(hash.Sum(nil))
}
