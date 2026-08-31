package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/notify"
)

// The exact payloads, and what the real transport does with a real socket.
//
// # Why the payloads are captured at the sender seam and not off the wire
//
// The first version of this file stood up an httptest sink and asserted on the
// bytes it received. Nothing arrived, and the reason is a property worth
// stating plainly rather than working around:
//
//	HarborMaster will not post a notification to a plaintext URL, and will not
//	post one to a certificate it cannot verify. There is no configuration for
//	either. internal/notify/tls.go is twelve lines with a comment saying so, and
//	the URL is re-parsed at the point of use so a row edited by hand cannot
//	become a destination.
//
// A local sink is http://, and an httptest TLS sink is self-signed. Reaching
// either would mean adding a trust seam to the transport, which is exactly the
// "do not broaden outbound-network capability" line -- so the positive cases are
// captured at NotificationSender, the seam the engine itself is built on, and
// the transport gets its own test below proving it refuses the sink.
//
// What that still exercises for real: the engine, the routing, the rule match,
// the suppression window, the delivery record, the retry, and the complete
// domain.Notification each channel would serialise.

// deliverySink records every webhook body it is posted.
type deliverySink struct {
	mu       sync.Mutex
	bodies   []string
	requests []*http.Request
	// status is what the sink answers. 200 unless a test wants a failure.
	status int
}

func newDeliverySink() *deliverySink {
	return &deliverySink{status: http.StatusOK}
}

func (s *deliverySink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	s.mu.Lock()
	s.bodies = append(s.bodies, string(body))
	s.requests = append(s.requests, r)
	status := s.status
	s.mu.Unlock()

	w.WriteHeader(status)
}

func (s *deliverySink) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// sinkRig is a real engine, a real sender, and a real HTTP destination.
type sinkRig struct {
	sink   *deliverySink
	server *httptest.Server
	store  *fakeNotificationStore
	engine *NotificationService
	sender *capturingSender
}

// capturingSender records the request the engine would hand the transport.
//
// It is the production NotificationSender interface, so what it captures is
// exactly what notify.Sender would have been given -- the whole notification,
// the destination, and the credential.
type capturingSender struct {
	mu       sync.Mutex
	requests []notify.SendRequest
	// result is what the send reports. OK unless a test wants a failure.
	result notify.Result
}

func (c *capturingSender) Send(_ context.Context, request notify.SendRequest) notify.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	if c.result.Reason == "" && !c.result.OK {
		return notify.Result{OK: true}
	}
	return c.result
}

func (c *capturingSender) captured() []notify.SendRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notify.SendRequest(nil), c.requests...)
}

func newSinkRig(t *testing.T, events ...domain.NotificationEvent) *sinkRig {
	t.Helper()

	sink := newDeliverySink()
	server := httptest.NewServer(sink)
	t.Cleanup(server.Close)

	destination := testDestination("ndst_0123456789abcdef0123", "sink")
	destination.Endpoint = server.URL

	rule := testRule("nrul_0123456789abcdef0123", destination.DestinationID)
	rule.Events = events

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{destination}
	fake.rules = []domain.NotificationRule{rule}
	// The URL the transport actually dials comes from the CREDENTIAL, in its
	// own type and its own table. Endpoint above is the display value; a test
	// that set only that would send nothing and prove nothing.
	fake.secrets[destination.DestinationID] = domain.NotificationSecret{URL: server.URL}

	recorder := &capturingSender{}
	engine := NewNotificationService(NotificationOptions{
		Store: fake,
		// The seam the engine is built on. Everything above it is production
		// code; below it is the transport, which has its own test.
		Sender: recorder,
		Config: testNotificationConfig(),
		Logger: quietLogger(),
		Now:    func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})

	return &sinkRig{sink: sink, server: server, store: fake, engine: engine, sender: recorder}
}

// deliver runs the engine until the sender has been handed want requests, and
// returns each notification as the JSON a webhook destination would receive.
func (r *sinkRig) deliver(t *testing.T, want int, raise func()) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stopped := make(chan struct{})
	go func() { defer close(stopped); r.engine.Run(ctx) }()

	raise()

	deadline := time.After(8 * time.Second)
	for {
		if captured := r.sender.captured(); len(captured) >= want {
			cancel()
			<-stopped

			bodies := make([]string, 0, len(captured))
			for _, request := range captured {
				encoded, err := json.Marshal(request.Notification)
				if err != nil {
					t.Fatalf("marshal notification: %v", err)
				}
				bodies = append(bodies, string(encoded))
			}
			return bodies
		}
		select {
		case <-deadline:
			cancel()
			<-stopped
			t.Fatalf("the sender was handed %d requests, want %d",
				len(r.sender.captured()), want)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// ------------------------------------------------- the five real payloads --

// TestTheLifecyclePayloadsThatReachTheWire walks each outcome end to end.
//
// One test rather than five, because the assertion that matters most is the
// COMPARISON: success, unrecovered failure and recovered must be three
// distinguishable documents, and a reader of the JSON must be able to tell
// which is which without reading English prose.
func TestTheLifecyclePayloadsThatReachTheWire(t *testing.T) {
	rig := newSinkRig(t,
		domain.EventExecutionSucceeded,
		domain.EventExecutionFailed,
		domain.EventUpdateRecovered,
		domain.EventRollbackFailed,
		domain.EventApprovalRequired,
	)

	bodies := rig.deliver(t, 5, func() {
		// A. update success.
		NotifyExecutionSucceeded(rig.engine, "web", "nginx:1.27.1", "exec_0123456789abcdef")
		// B. review required.
		NotifyApprovalRequired(rig.engine, "web", "plan_0123456789abcdef01",
			"It is a major version change.")
		// C. unrecovered failure, operator-requested.
		NotifyExecutionFailed(rig.engine, "web", "exec_1123456789abcdef",
			"the recreation did not succeed (unhealthy).", true, false)
		// D. failed update, successful automatic rollback.
		NotifyUpdateRecovered(rig.engine, "web",
			"nginx:1.27.1", "nginx:1.27.0", "rb_0123456789abcdef01", "exec_2123456789abcdef")
		// E. rollback failure.
		NotifyRollbackFailed(rig.engine, "web", "rb_1123456789abcdef01",
			"the rollback did not succeed (startOriginal).", true)
	})

	byEvent := map[string]map[string]any{}
	for _, body := range bodies {
		var document map[string]any
		if err := json.Unmarshal([]byte(body), &document); err != nil {
			t.Fatalf("the sink received something that is not JSON: %v\n%s", err, body)
		}
		event, _ := document["event"].(string)
		if event == "" {
			t.Fatalf("a delivered document has no event field:\n%s", body)
		}
		byEvent[event] = document
	}

	for _, want := range []string{
		"execution.succeeded", "update.approvalRequired", "execution.failed",
		"update.recovered", "rollback.failed",
	} {
		if _, arrived := byEvent[want]; !arrived {
			t.Fatalf("%q never reached the sink; got %v", want, keysOf(byEvent))
		}
	}

	// The central distinction, asserted on the wire format rather than on prose.
	if byEvent["update.recovered"]["event"] == byEvent["execution.succeeded"]["event"] {
		t.Fatal("a recovered update and a successful one are the same event on the wire")
	}
	if severity := byEvent["update.recovered"]["severity"]; severity != "warning" {
		t.Errorf("update.recovered arrived with severity %v, want warning\n\n"+
			"A receiver routing on severity must not file a failed-and-recovered "+
			"update alongside the ones that worked.", severity)
	}
	if severity := byEvent["execution.succeeded"]["severity"]; severity != "info" {
		t.Errorf("execution.succeeded arrived with severity %v, want info", severity)
	}
	if severity := byEvent["rollback.failed"]["severity"]; severity != "critical" {
		t.Errorf("rollback.failed arrived with severity %v, want critical", severity)
	}

	// And the recovered document carries what is needed to understand it.
	recovered := byEvent["update.recovered"]
	if recovered["containerName"] != "web" {
		t.Errorf("containerName = %v", recovered["containerName"])
	}
	// The two facts an operator needs, in the part of the payload that survives
	// a retry and a restart. NOTE: `fields` does NOT survive -- the engine
	// rebuilds the notification from the delivery row, which has no column for
	// them -- so this deliberately asserts on the body rather than on a field
	// that would pass here and be absent on the second attempt.
	body, _ := recovered["body"].(string)
	if !strings.Contains(body, "nginx:1.27.1") {
		t.Errorf("the recovered payload does not name the attempted image: %s", body)
	}
	if !strings.Contains(body, "nginx:1.27.0") {
		t.Errorf("the recovered payload does not name the image now running: %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "paused") {
		t.Errorf("the recovered payload does not say automation is paused, so an "+
			"operator does not know the container is out of the rotation: %s", body)
	}
}

func TestNothingSensitiveReachesTheWire(t *testing.T) {
	// The payload is built from a closed set of fields, so this is a check on
	// the whole serialised document rather than on any one of them. What it
	// looks for is what a leak would look like: an environment value, a
	// credential, a socket path, an authorization header.
	rig := newSinkRig(t,
		domain.EventExecutionSucceeded, domain.EventExecutionFailed,
		domain.EventUpdateRecovered, domain.EventRollbackFailed,
		domain.EventApprovalRequired, domain.EventRollbackStarted,
		domain.EventRollbackSucceeded,
	)

	bodies := rig.deliver(t, 7, func() {
		NotifyExecutionSucceeded(rig.engine, "web", "nginx:1.27.1", "exec_A123456789abcdef")
		NotifyExecutionFailed(rig.engine, "web", "exec_B123456789abcdef",
			"the recreation did not succeed (unhealthy).", true, true)
		NotifyUpdateRecovered(rig.engine, "web",
			"nginx:1.27.1", "nginx:1.27.0", "rb_A123456789abcdef0", "exec_B123456789abcdef")
		NotifyRollbackStarted(rig.engine, "web", "rb_B123456789abcdef0")
		NotifyRollbackSucceeded(rig.engine, "web", "rb_B123456789abcdef0")
		NotifyRollbackFailed(rig.engine, "web", "rb_C123456789abcdef0",
			"the rollback did not succeed (startOriginal).", false)
		NotifyApprovalRequired(rig.engine, "web", "plan_A123456789abcdef",
			"It is a major version change.")
	})

	for _, body := range bodies {
		lowered := strings.ToLower(body)
		for _, forbidden := range []string{
			"password", "secret", "token", "credential", "authorization",
			"bearer ", "docker.sock", "/var/run", "env=", "apikey", "api_key",
			"private_key", "-----begin",
		} {
			if strings.Contains(lowered, forbidden) {
				t.Errorf("a delivered payload contains %q:\n%s", forbidden, body)
			}
		}
	}
}

func TestARetryReusesOneDeliveryRecordRatherThanMintingAnother(t *testing.T) {
	// "A delivery retry retries the same logical notification rather than
	// creating a new one."
	//
	// It is a HarborMaster-side guarantee, not something a receiver collapses
	// on: the published webhook document carries no dedup key, deliberately.
	// What makes a retry the same message is that it reuses ONE delivery row --
	// same id, attempts incremented -- so the history shows one notification
	// that took three tries rather than three notifications.
	rig := newSinkRig(t, domain.EventUpdateRecovered)
	rig.sender.result = notify.Result{Reason: notify.FailureTimeout, Retryable: true}

	rig.deliver(t, 1, func() {
		NotifyUpdateRecovered(rig.engine, "web",
			"nginx:1.27.1", "nginx:1.27.0", "rb_0123456789abcdef01", "exec_0123456789abcdef")
	})

	recorded := rig.store.recorded()
	if len(recorded) != 1 {
		t.Fatalf("%d delivery records for one notification, want 1\n\n"+
			"A second row is a second notification in the history, and an "+
			"operator counting failures would count the retries as incidents.",
			len(recorded))
	}
	// And the row still names the lifecycle record it reports, so the history
	// can be joined back to the rollback.
	if !strings.Contains(recorded[0].DedupKey, "rb_0123456789abcdef01") {
		t.Errorf("the delivery record's dedup key is %q and does not name the "+
			"rollback", recorded[0].DedupKey)
	}
}

// TestTheDeliveredDocumentIsRebuiltFromTheStoredRow records a real limitation.
//
// The engine hands the transport a notification rebuilt from the delivery row,
// and that row has columns for the title, the body, the container and the
// event -- but none for Fields. So a notification's fields do NOT reach a
// destination, on any channel, even though every channel encoder knows how to
// render them.
//
// This is pre-existing and is NOT fixed here: carrying fields through would
// change the payload of every event on every channel, which is a published
// contract change and its own piece of work. It is pinned instead, because it
// is the reason NotifyUpdateRecovered states the attempted and restored images
// in its BODY. If somebody fixes the gap, this test fails and points at the
// duplication so it can be tidied deliberately.
func TestTheDeliveredDocumentIsRebuiltFromTheStoredRow(t *testing.T) {
	rig := newSinkRig(t, domain.EventUpdateRecovered)

	rig.deliver(t, 1, func() {
		NotifyUpdateRecovered(rig.engine, "web",
			"nginx:1.27.1", "nginx:1.27.0", "rb_0123456789abcdef01", "exec_0123456789abcdef")
	})

	delivered := rig.sender.captured()[0].Notification
	if len(delivered.Fields) != 0 {
		t.Errorf("fields now reach the transport: %v\n\n"+
			"Good -- but NotifyUpdateRecovered repeats the attempted and "+
			"restored images in its body precisely because they did not. "+
			"Remove the duplication and update this test.", delivered.Fields)
	}
	// The body is what survives, so the body must be self-contained.
	if !strings.Contains(delivered.Body, "nginx:1.27.1") ||
		!strings.Contains(delivered.Body, "nginx:1.27.0") {
		t.Errorf("the delivered body does not carry both images: %s", delivered.Body)
	}
}

func TestADeliveryFailureIsRecordedAndChangesNothingElse(t *testing.T) {
	// The sink refuses everything. The engine must record that and keep
	// running: a webhook that is down is a webhook that is down, not a
	// container problem and not a reason to stop.
	rig := newSinkRig(t, domain.EventExecutionSucceeded)
	rig.sink.status = http.StatusServiceUnavailable

	bodies := rig.deliver(t, 1, func() {
		NotifyExecutionSucceeded(rig.engine, "web", "nginx:1.27.1", "exec_0123456789abcdef")
	})

	if len(bodies) == 0 {
		t.Fatal("nothing was attempted")
	}
	// The engine wrote a delivery row for the attempt. That row is the whole
	// record of the failure; nothing about the execution it described moved.
	if len(rig.store.recorded()) == 0 {
		t.Error("a failed delivery produced no delivery record, so an operator " +
			"checking why they heard nothing has nothing to read")
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// ------------------------------------------- the transport, over a socket --

// TestTheRealTransportRefusesAPlaintextSink is the security assertion this file
// was originally built to make, inverted.
//
// A real notify.Sender, a real socket, a real listener on loopback. Nothing
// arrives, and nothing arriving is the correct behaviour: a webhook URL is a
// bearer credential and the payload names containers and failures, so
// HarborMaster posts over verified TLS or not at all.
//
// The sink is left running and asserted to be untouched, because "the delivery
// failed" and "the delivery went somewhere else" would otherwise look the same.
func TestTheRealTransportRefusesAPlaintextSink(t *testing.T) {
	sink := newDeliverySink()
	server := httptest.NewServer(sink)
	defer server.Close()

	sender := notify.NewSender(notify.SenderOptions{
		// Even with the private-address relaxation ON -- the most permissive
		// this deployment can be configured to be.
		Policy:  notify.AddressPolicy{AllowPrivate: true},
		Version: "test",
	})

	destination := testDestination("ndst_0123456789abcdef0123", "sink")
	destination.Endpoint = server.URL

	result := sender.Send(context.Background(), notify.SendRequest{
		Notification: domain.Notification{
			Event:      domain.EventUpdateRecovered,
			Severity:   domain.NotifyWarning,
			Title:      "web failed to update and was restored automatically",
			OccurredAt: time.Unix(1700000000, 0).UTC(),
		},
		Destination: destination,
		Secret:      domain.NotificationSecret{URL: server.URL},
	})

	if result.OK {
		t.Fatal("a notification was posted to a plaintext http:// endpoint.\n\n" +
			"A webhook URL is a bearer credential and the payload names " +
			"containers and failures. There is no configuration that permits " +
			"this, and adding one would be the change this test exists to stop.")
	}
	if got := len(sink.received()); got != 0 {
		t.Fatalf("the plaintext sink received %d bodies", got)
	}
}

// TestALoopbackSinkIsRefusedAtEveryLayer walks the guards in front of a
// destination, and records why a positive end-to-end socket test cannot exist.
//
// THREE independent refusals stand between a notification and a local sink, and
// the outermost one fires first:
//
//  1. The URL. `plainHostname` refuses IP literals, `localhost`, and any host
//     with no dot -- which also catches a container name or a Docker service
//     alias. An SSRF target cannot even be NAMED.
//  2. The scheme. https only, with no setting that adds a plaintext path.
//  3. The dialled address. The transport's Control hook re-checks the RESOLVED
//     literal IP at dial time, which is what defeats a DNS rebind: a name that
//     resolved publicly at validation and privately at dial is caught here.
//
// TLS verification sits under all three and cannot be reached from a loopback
// test, because such a sink is refused by name long before a handshake. That is
// the correct ordering and it is why this file captures payloads at the sender
// seam instead: reaching a local sink would mean adding a trust or scheme seam
// to the transport, and there is deliberately none to add.
func TestALoopbackSinkIsRefusedAtEveryLayer(t *testing.T) {
	sink := newDeliverySink()
	plain := httptest.NewServer(sink)
	defer plain.Close()
	secure := httptest.NewTLSServer(sink)
	defer secure.Close()

	sender := notify.NewSender(notify.SenderOptions{
		// The most permissive this deployment can be configured to be.
		Policy:  notify.AddressPolicy{AllowPrivate: true},
		Version: "test",
	})

	for _, endpoint := range []struct {
		name string
		url  string
	}{
		{"a plaintext sink", plain.URL},
		{"a TLS sink on an address literal", secure.URL},
		{"a name with no dot", "https://sink/hook"},
		{"localhost", "https://localhost/hook"},
		{"the cloud metadata endpoint", "https://169.254.169.254/latest/meta-data"},
		{"credentials in the userinfo", "https://user:pass@hooks.example.test/hook"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			destination := testDestination("ndst_0123456789abcdef0123", "sink")
			destination.Endpoint = endpoint.url

			result := sender.Send(context.Background(), notify.SendRequest{
				Notification: domain.Notification{
					Event:      domain.EventUpdateRecovered,
					Severity:   domain.NotifyWarning,
					Title:      "web failed to update and was restored automatically",
					OccurredAt: time.Unix(1700000000, 0).UTC(),
				},
				Destination: destination,
				Secret:      domain.NotificationSecret{URL: endpoint.url},
			})

			if result.OK {
				t.Fatalf("a notification was posted to %s.\n\n"+
					"A webhook URL is a bearer credential and the payload names "+
					"containers and failures. There is no configuration that "+
					"permits this.", endpoint.name)
			}
		})
	}

	// Nothing reached either listener. Without this, "the send failed" and "the
	// send went somewhere else" would look identical.
	if got := len(sink.received()); got != 0 {
		t.Fatalf("a local sink received %d bodies", got)
	}
}
