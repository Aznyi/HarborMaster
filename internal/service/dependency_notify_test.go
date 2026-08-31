package service_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The one dependency notification, and what it may say.
//
// # Why only one event
//
// A failed reattachment is the only dependency condition that can leave a
// workload broken with no other signal: the container is attached to a
// namespace that no longer exists, Docker reports nothing, and HarborMaster
// does not retry. A loop changes nothing about a running container and would be
// re-raised every pass. A wait is the system working. A block is HarborMaster
// declining, which is the safe direction.
//
// # And what it may carry
//
// Two container names and one HarborMaster-generated identifier. The tests
// below walk the whole notification for anything that could have come from a
// daemon, a registry, or an environment.

type recordingNotifier struct {
	mu   sync.Mutex
	sent []domain.Notification
	// explode makes every Raise panic, modelling a notification path that is
	// itself broken rather than a destination that is merely unreachable. The
	// second is the engine's business; the first is the one that could take a
	// container pipeline down with it.
	explode bool
}

func (n *recordingNotifier) Raise(notification domain.Notification) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.explode {
		panic("the notification path is broken")
	}
	n.sent = append(n.sent, notification)
}

func (n *recordingNotifier) all() []domain.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]domain.Notification(nil), n.sent...)
}

func TestAFailedReattachmentNotifiesOncePerContainer(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyRebindFailed(notifier, "sonarr", "gluetun", "depop_0123456789abcdef0123")
	service.NotifyRebindFailed(notifier, "radarr", "gluetun", "depop_0123456789abcdef0123")

	sent := notifier.all()
	if len(sent) != 2 {
		t.Fatalf("%d notifications, want one per container", len(sent))
	}

	// Distinct dedup keys, so an operator watching a five-dependent provider
	// fail gets five messages rather than one.
	if sent[0].DedupKey == sent[1].DedupKey {
		t.Errorf("both notifications share the dedup key %q", sent[0].DedupKey)
	}
	for _, notification := range sent {
		if notification.Event != domain.EventRebindFailed {
			t.Errorf("event = %q", notification.Event)
		}
		if notification.Severity != domain.NotifyCritical {
			t.Errorf("severity = %q; a container that may have no network is critical",
				notification.Severity)
		}
	}
}

// The sentence names the two containers and says what did NOT happen.
func TestTheReattachmentNotificationSaysWhatDidNotHappen(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyRebindFailed(notifier, "sonarr", "gluetun", "depop_0123456789abcdef0123")

	sent := notifier.all()[0]
	if !strings.Contains(sent.Title, "sonarr") {
		t.Errorf("the title does not name the container: %q", sent.Title)
	}
	for _, phrase := range []string{
		"gluetun",
		"does not retry a reattachment by itself",
		"no image version was changed",
	} {
		if !strings.Contains(strings.ToLower(sent.Body), strings.ToLower(phrase)) {
			t.Errorf("the body does not say %q: %q", phrase, sent.Body)
		}
	}
	// A reattachment must never be described as an update.
	lowered := strings.ToLower(sent.Title + " " + sent.Body)
	for _, forbidden := range []string{"updated", "upgrade", "newer image"} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("the notification describes a reattachment as %q", forbidden)
		}
	}
}

// It carries nothing a daemon, a registry, or an environment could have written.
func TestTheReattachmentNotificationCarriesOnlyHarborMastersOwnWords(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyRebindFailed(notifier, "sonarr", "gluetun", "depop_0123456789abcdef0123")

	sent := notifier.all()[0]
	whole := sent.Title + " " + sent.Body
	for _, field := range sent.Fields {
		whole += " " + field.Label + " " + field.Value
	}

	for _, forbidden := range []string{
		"sha256:", "http://", "https://", "/var/run", "/etc/", "docker.sock",
		"password", "token", "secret", "Authorization",
		// A format verb would mean the sentence is assembled from something
		// else at some other site, which is the thing the one-file rule stops.
		"%s", "%v", "%q", "%w",
	} {
		if strings.Contains(whole, forbidden) {
			t.Errorf("the notification carries %q: %q", forbidden, whole)
		}
	}
}

// Nothing is raised when notifications are off.
func TestNoNotifierMeansNoNotification(t *testing.T) {
	t.Parallel()

	// The default deployment. This must not panic and must not require a
	// deployment to opt out of anything.
	service.NotifyRebindFailed(nil, "sonarr", "gluetun", "depop_0123456789abcdef0123")
}

// The dependency subsystem raises exactly one event, and it is this one.
//
// A guard against the vocabulary quietly growing: every additional dependency
// event is a message an operator did not ask for, and the three states that are
// deliberately silent are silent for reasons written down in the domain.
func TestOnlyOneDependencyEventExists(t *testing.T) {
	t.Parallel()

	var dependencyEvents []domain.NotificationEvent
	for _, event := range domain.NotificationEvents {
		if strings.HasPrefix(string(event), "dependency.") {
			dependencyEvents = append(dependencyEvents, event)
		}
	}
	if len(dependencyEvents) != 1 || dependencyEvents[0] != domain.EventRebindFailed {
		t.Fatalf("dependency events = %v, want exactly [%s]\n"+
			"\ta loop, a wait, and a block are all real states and none of them "+
			"can leave a workload broken with no other signal; adding an event "+
			"for one is a message nobody asked for",
			dependencyEvents, domain.EventRebindFailed)
	}
}
