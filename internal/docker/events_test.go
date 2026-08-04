package docker

// In-package, for the same reason normalize_test.go is: event conversion is the
// boundary where Docker SDK types become HarborMaster types, so exercising it
// means constructing events.Message directly.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// testClient is shared with normalize_test.go: a Client carrying only the
// fields normalization needs, which never contacts a daemon.

func TestConvertEventNormalizesAContainerStart(t *testing.T) {
	at := time.Date(2026, 8, 3, 9, 20, 11, 482000000, time.UTC)

	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor: events.Actor{
			ID: "b8f1c0d2e3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0",
			Attributes: map[string]string{
				"name":                       "shop-web-1",
				"image":                      "nginx:1.27",
				"com.docker.compose.project": "shop",
				"com.docker.compose.service": "web",
				"io.harbormaster.channel":    "stable",
			},
		},
		Scope:    "local",
		Time:     at.Unix(),
		TimeNano: at.UnixNano(),
	})

	if event.Type != domain.EventTypeContainer {
		t.Errorf("type = %q, want container", event.Type)
	}
	if event.Action != domain.ActionStart {
		t.Errorf("action = %q, want start", event.Action)
	}
	if event.ActorName != "shop-web-1" {
		t.Errorf("actorName = %q, want shop-web-1", event.ActorName)
	}
	if event.ComposeProject != "shop" || event.ComposeService != "web" {
		t.Errorf("compose = %q/%q, want shop/web", event.ComposeProject, event.ComposeService)
	}
	if event.HarborMasterLabels["channel"] != "stable" {
		t.Errorf("harbormaster labels = %v, want the prefix stripped", event.HarborMasterLabels)
	}
	if !event.DockerTime.Equal(at) {
		t.Errorf("dockerTime = %s, want %s", event.DockerTime, at)
	}
	if event.DockerTimeNano != at.UnixNano() {
		t.Errorf("dockerTimeNano = %d, want %d", event.DockerTimeNano, at.UnixNano())
	}
	if event.Fingerprint == "" {
		t.Error("every event must carry a fingerprint")
	}
	if event.HostID != domain.LocalHostID {
		t.Errorf("hostId = %q, want %q", event.HostID, domain.LocalHostID)
	}
}

// An event with no actor attributes at all must still normalize. Some daemon
// versions send a bare message, and dropping it would make the history lie.
func TestConvertEventToleratesMissingActorAttributes(t *testing.T) {
	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionDie,
		Actor:  events.Actor{ID: "abc123def456"},
		Time:   1000,
	})

	if event.Attributes == nil {
		t.Fatal("attributes must never be nil; callers index into it directly")
	}
	if len(event.Attributes) != 0 {
		t.Errorf("attributes = %v, want empty", event.Attributes)
	}
	if event.ComposeProject != "" || event.ComposeService != "" {
		t.Error("absent compose labels must yield empty strings, not a guess")
	}
	// With no name attribute, the actor ID is shortened for display.
	if event.ActorName != domain.ShortenID("abc123def456") {
		t.Errorf("actorName = %q, want the shortened id", event.ActorName)
	}
}

// A type HarborMaster does not model becomes "other" rather than passing
// through: an unconstrained value would reach the database CHECK constraint.
func TestConvertEventMapsUnknownTypeToOther(t *testing.T) {
	event := testClient().convertEvent(events.Message{
		Type:   events.Type("quantum-widget"),
		Action: events.Action("entangle"),
		Actor:  events.Actor{ID: "x"},
	})

	if event.Type != domain.EventTypeOther {
		t.Errorf("type = %q, want other", event.Type)
	}
	// The action is preserved verbatim: it is data, not a closed vocabulary.
	if event.Action != domain.DockerEventAction("entangle") {
		t.Errorf("action = %q, want entangle preserved", event.Action)
	}
}

func TestConvertEventPreservesUnknownActions(t *testing.T) {
	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.Action("SOMETHING_NEW"),
		Actor:  events.Actor{ID: "x"},
	})

	// Lower-cased so filtering is case-insensitive in one place rather than at
	// every query site.
	if event.Action != domain.DockerEventAction("something_new") {
		t.Errorf("action = %q, want something_new", event.Action)
	}
}

// health_status and exec actions carry a colon-separated suffix. The verb must
// stay filterable and the detail must not be lost.
func TestConvertEventSplitsSuffixedActions(t *testing.T) {
	tests := []struct {
		name       string
		raw        events.Action
		wantAction domain.DockerEventAction
		wantDetail string
	}{
		{"health status", events.ActionHealthStatusHealthy, domain.ActionHealthStatus, "healthy"},
		{"unhealthy", events.ActionHealthStatusUnhealthy, domain.ActionHealthStatus, "unhealthy"},
		{"exec create", events.Action("exec_create: /bin/sh -c 'echo hi'"), "exec_create", "/bin/sh -c 'echo hi'"},
		{"plain action", events.ActionStart, domain.ActionStart, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := testClient().convertEvent(events.Message{
				Type:   events.ContainerEventType,
				Action: tc.raw,
				Actor:  events.Actor{ID: "x"},
			})

			if event.Action != tc.wantAction {
				t.Errorf("action = %q, want %q", event.Action, tc.wantAction)
			}
			detail := event.Attributes["harbormaster.actionDetail"]
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

// Older daemons send whole seconds only. The nanosecond field must be zero
// rather than fabricated, because the fingerprint branches on it.
func TestConvertEventHandlesSecondsOnlyTimestamps(t *testing.T) {
	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStop,
		Actor:  events.Actor{ID: "x"},
		Time:   1785000000,
	})

	if event.DockerTimeNano != 0 {
		t.Errorf("dockerTimeNano = %d, want 0 when the daemon sent no nanoseconds", event.DockerTimeNano)
	}
	if event.DockerTime.Unix() != 1785000000 {
		t.Errorf("dockerTime = %s, want the second-precision value", event.DockerTime)
	}
}

func TestConvertEventFallsBackToDeprecatedActorFields(t *testing.T) {
	// Older daemons populate the top-level ID and leave Actor.ID empty.
	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		ID:     "legacy-container-id",
	})

	if event.ActorID != "legacy-container-id" {
		t.Errorf("actorId = %q, want the deprecated top-level id", event.ActorID)
	}
}

func TestConvertEventDefaultsScopeToLocal(t *testing.T) {
	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "x"},
	})

	if event.Scope != "local" {
		t.Errorf("scope = %q, want local", event.Scope)
	}
}

// Actor attributes are a resource's labels, which are arbitrary operator
// key/value pairs and absolutely can carry credentials. Every one of these
// reaches SQLite, the REST API, and an unauthenticated SSE stream.
func TestConvertEventRedactsSensitiveAttributes(t *testing.T) {
	const secret = "hunter2-do-not-leak"

	event := testClient().convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionCreate,
		Actor: events.Actor{
			ID: "x",
			Attributes: map[string]string{
				"name":            "api",
				"DB_PASSWORD":     secret,
				"api_key":         secret,
				"AUTHORIZATION":   secret,
				"registry_token":  secret,
				"harmless_config": "keep-me",
			},
		},
	})

	for _, key := range []string{"DB_PASSWORD", "api_key", "AUTHORIZATION", "registry_token"} {
		if got := event.Attributes[key]; got != domain.MaskedValue {
			t.Errorf("attribute %q = %q, want it masked", key, got)
		}
	}
	// Over-masking is the intended bias, but structural metadata needed for
	// correlation must survive.
	if event.Attributes["name"] != "api" {
		t.Error("the resource name must not be masked; inventory correlation needs it")
	}
	if event.Attributes["harmless_config"] != "keep-me" {
		t.Error("a non-sensitive attribute must be preserved verbatim")
	}

	for key, value := range event.Attributes {
		if strings.Contains(value, secret) {
			t.Fatalf("attribute %q leaked the secret value", key)
		}
	}
}

// The identity must be stable: the same input always produces the same
// fingerprint, or deduplication does nothing.
func TestEventFingerprintIsDeterministic(t *testing.T) {
	message := events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor: events.Actor{
			ID:         "abc",
			Attributes: map[string]string{"name": "web", "other": "x"},
		},
		TimeNano: 1785000000123456789,
	}

	first := testClient().convertEvent(message)
	second := testClient().convertEvent(message)

	if first.Fingerprint != second.Fingerprint {
		t.Errorf("the same event produced two fingerprints: %s vs %s",
			first.Fingerprint, second.Fingerprint)
	}
}

// A genuinely repeated action at a different instant is NOT the same event.
// Merging them would lose a real state transition.
func TestEventFingerprintDistinguishesRepeatedActions(t *testing.T) {
	base := events.Message{
		Type:     events.ContainerEventType,
		Action:   events.ActionStart,
		Actor:    events.Actor{ID: "abc", Attributes: map[string]string{"name": "web"}},
		TimeNano: 1785000000000000000,
	}
	later := base
	later.TimeNano = 1785000005000000000

	client := testClient()
	if client.convertEvent(base).Fingerprint == client.convertEvent(later).Fingerprint {
		t.Error("two starts at different instants must have different fingerprints")
	}
}

// Without nanoseconds, whole seconds are too coarse to separate a burst, so the
// identity widens to include every attribute.
func TestEventFingerprintWidensWithoutNanoseconds(t *testing.T) {
	client := testClient()

	first := client.convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "abc", Attributes: map[string]string{"name": "web-1"}},
		Time:   1785000000,
	})
	second := client.convertEvent(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "abc", Attributes: map[string]string{"name": "web-2"}},
		Time:   1785000000,
	})

	if first.Fingerprint == second.Fingerprint {
		t.Error("two same-second events with different attributes must not collide")
	}
}

// A container's attributes include its whole label set. Hashing all of them
// would mean an unrelated label edit defeats deduplication.
func TestEventFingerprintIgnoresUnrelatedLabelsWhenNanosecondsExist(t *testing.T) {
	client := testClient()

	withLabel := func(extra map[string]string) string {
		attributes := map[string]string{"name": "web"}
		for key, value := range extra {
			attributes[key] = value
		}
		return client.convertEvent(events.Message{
			Type:     events.ContainerEventType,
			Action:   events.ActionStart,
			Actor:    events.Actor{ID: "abc", Attributes: attributes},
			TimeNano: 1785000000123456789,
		}).Fingerprint
	}

	if withLabel(nil) != withLabel(map[string]string{"team": "platform"}) {
		t.Error("an unrelated label must not change the event identity")
	}
}

// A rename's old and new names are what tell two renames apart, so they must be
// inside the identity.
func TestEventFingerprintSeparatesRenames(t *testing.T) {
	client := testClient()

	rename := func(oldName, newName string) string {
		return client.convertEvent(events.Message{
			Type:   events.ContainerEventType,
			Action: events.ActionRename,
			Actor: events.Actor{
				ID:         "abc",
				Attributes: map[string]string{"name": newName, "oldName": oldName},
			},
			TimeNano: 1785000000123456789,
		}).Fingerprint
	}

	if rename("a", "b") == rename("b", "c") {
		t.Error("two renames at the same instant must have different identities")
	}
}

// A daemon that is unreachable must surface on the error channel rather than
// panicking or blocking, so the engine can back off and retry.
func TestStreamEventsAgainstAnUnreachableDaemon(t *testing.T) {
	client, err := New(Options{
		Host:    "unix:///nonexistent/harbormaster-events-test.sock",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subscription, err := client.StreamEvents(ctx, time.Time{})
	if err != nil {
		// Some platforms fail at dial time, which is equally acceptable.
		return
	}

	select {
	case streamErr := <-subscription.Errors:
		if streamErr == nil {
			t.Fatal("an unreachable daemon must produce an error")
		}
	case <-ctx.Done():
		t.Fatal("an unreachable daemon must fail promptly rather than hang")
	}
}

// Cancelling the context must close the event channel, or every reconnect would
// leak a goroutine and a socket.
func TestStreamEventsClosesOnCancellation(t *testing.T) {
	client, err := New(Options{
		Host:    "unix:///nonexistent/harbormaster-events-cancel.sock",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	subscription, err := client.StreamEvents(ctx, time.Time{})
	if err != nil {
		cancel()
		return
	}
	cancel()

	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("the event channel must not deliver after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event channel must close when the context is cancelled")
	}
}

// The `since` argument must be nanosecond-precise, so a resume neither replays
// the event it resumed from nor skips one that shared its second.
func TestStreamEventsAcceptsANanosecondResumePoint(t *testing.T) {
	client, err := New(Options{
		Host:    "unix:///nonexistent/harbormaster-events-since.sock",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The daemon is unreachable, so this only proves the option is accepted and
	// the call returns without panicking on a sub-second timestamp.
	if _, err := client.StreamEvents(ctx, time.Unix(1785000000, 123456789)); err != nil {
		t.Fatalf("StreamEvents with a nanosecond resume point: %v", err)
	}
}

func TestLooksLikeID(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"b8f1c0d2e3a4b5c6d7e8f9a0b1c2d3e4", true},
		{"sha256:b8f1c0d2e3a4b5c6d7e8f9a0", true},
		{"nginx:1.27", false},
		{"my-volume", false},
		{"abc", false},
	}

	for _, tc := range tests {
		if got := looksLikeID(tc.value); got != tc.want {
			t.Errorf("looksLikeID(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// A volume's actor ID is its name, so it must never be shortened as though it
// were a digest.
func TestResourceNameDoesNotShortenVolumeNames(t *testing.T) {
	event := testClient().convertEvent(events.Message{
		Type:   events.VolumeEventType,
		Action: events.ActionCreate,
		Actor:  events.Actor{ID: "a-long-descriptive-volume-name"},
	})

	if event.ActorName != "a-long-descriptive-volume-name" {
		t.Errorf("actorName = %q, want the full volume name", event.ActorName)
	}
}
