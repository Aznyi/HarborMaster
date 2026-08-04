package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// ------------------------------------------------------------ test doubles --

type fakeDockerEvents struct {
	mu sync.Mutex

	events     []domain.DockerEvent
	total      int
	listErr    error
	getErr     error
	get        *domain.DockerEvent
	replay     []domain.DockerEvent
	replayTot  int
	projects   []string
	actions    []string
	lastFilter store.DockerEventFilter
	lastAfter  int64
	lastLimit  int
}

func (f *fakeDockerEvents) List(_ context.Context, filter store.DockerEventFilter) ([]domain.DockerEvent, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	total := f.total
	if total == 0 {
		total = len(f.events)
	}
	return f.events, total, nil
}

func (f *fakeDockerEvents) Get(context.Context, int64) (*domain.DockerEvent, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.get, nil
}

func (f *fakeDockerEvents) Since(_ context.Context, after int64, limit int) ([]domain.DockerEvent, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastAfter = after
	f.lastLimit = limit

	total := f.replayTot
	if total == 0 {
		total = len(f.replay)
	}
	if limit < len(f.replay) {
		return f.replay[:limit], total, nil
	}
	return f.replay, total, nil
}

func (f *fakeDockerEvents) DistinctEventProjects(context.Context) ([]string, error) {
	return f.projects, nil
}

func (f *fakeDockerEvents) DistinctEventActions(context.Context) ([]string, error) {
	return f.actions, nil
}

func (f *fakeDockerEvents) filter() store.DockerEventFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

// fakeEngine is an EventEngineReader double backed by a real broadcaster, so
// subscription and limit behaviour is the production code rather than a mock.
type fakeEngine struct {
	enabled     bool
	status      domain.EventEngineStatus
	replayLimit int
	heartbeat   time.Duration
	hint        time.Duration

	mu   sync.Mutex
	subs []chan domain.DockerEvent
	// limit caps subscriptions; zero means unlimited.
	limit int
}

func (f *fakeEngine) Enabled() bool { return f.enabled }

func (f *fakeEngine) Status(context.Context) domain.EventEngineStatus { return f.status }

func (f *fakeEngine) Subscribe() (*service.StreamSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.limit > 0 && len(f.subs) >= f.limit {
		return nil, service.ErrTooManySubscribers
	}
	channel := make(chan domain.DockerEvent, 16)
	f.subs = append(f.subs, channel)
	return service.NewStreamSubscription(channel, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, existing := range f.subs {
			if existing == channel {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				close(channel)
				return
			}
		}
	}), nil
}

func (f *fakeEngine) ReplayLimit() int { return f.replayLimit }

func (f *fakeEngine) HeartbeatInterval() time.Duration {
	if f.heartbeat <= 0 {
		return time.Hour
	}
	return f.heartbeat
}

func (f *fakeEngine) ReconnectHint() time.Duration { return f.hint }

// publish pushes an event to every subscriber.
func (f *fakeEngine) publish(event domain.DockerEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, channel := range f.subs {
		select {
		case channel <- event:
		default:
		}
	}
}

func (f *fakeEngine) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// ------------------------------------------------------------------ setup --

func newEventServer(t *testing.T, events *fakeDockerEvents, engine *fakeEngine) *Server {
	t.Helper()

	opts := Options{
		Health: &fakeHealth{},
		Logger: discardLogger(),
		Config: config.Server{MaxRequestBytes: 1024},
		Assets: testAssets(),
	}
	if events != nil {
		opts.DockerEvents = events
	}
	if engine != nil {
		opts.EventEngine = engine
	}
	return NewServer(opts)
}

func sampleEvent(sequence int64) domain.DockerEvent {
	at := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	return domain.DockerEvent{
		Sequence:    sequence,
		Fingerprint: fmt.Sprintf("fp-%d", sequence),
		HostID:      domain.LocalHostID,
		Type:        domain.EventTypeContainer,
		Action:      domain.ActionStart,
		ActorID:     "b8f1c0d2e3a4b5c6d7e8f9a0b1c2d3e4",
		ActorName:   "shop-web-1",
		Scope:       "local",
		Attributes: map[string]string{
			"name": "shop-web-1",
			// Already masked, as everything reaching this layer must be.
			"DB_PASSWORD": domain.MaskedValue,
		},
		ComposeProject:   "shop",
		ComposeService:   "web",
		DockerTime:       at,
		ObservedAt:       at,
		Result:           domain.ResultProcessed,
		RefreshRequested: domain.RefreshContainer,
	}
}

// ------------------------------------------------------------ event list --

func TestEventListReturnsAPage(t *testing.T) {
	events := &fakeDockerEvents{
		events: []domain.DockerEvent{sampleEvent(3), sampleEvent(2), sampleEvent(1)},
		total:  3,
	}
	srv := newEventServer(t, events, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body struct {
		Items      []domain.DockerEvent `json:"items"`
		Pagination Pagination           `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 3 {
		t.Errorf("items = %d, want 3", len(body.Items))
	}
	if body.Pagination.TotalItems != 3 {
		t.Errorf("totalItems = %d, want 3", body.Pagination.TotalItems)
	}
	// Newest first is what an event log is read as.
	if events.filter().Direction != store.SortDesc {
		t.Errorf("direction = %q, want desc by default", events.filter().Direction)
	}
}

func TestEventListAppliesFilters(t *testing.T) {
	events := &fakeDockerEvents{}
	srv := newEventServer(t, events, nil)

	target := APIPrefix + "/events?type=container,image&action=start&result=processed" +
		"&project=shop&service=web&actorId=abc123&search=nginx" +
		"&since=2026-08-03T00:00:00Z&until=2026-08-04T00:00:00Z&sort=observed&direction=asc"

	rec := do(t, srv, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	filter := events.filter()
	if len(filter.Types) != 2 {
		t.Errorf("types = %v, want two", filter.Types)
	}
	if len(filter.Actions) != 1 || filter.Actions[0] != "start" {
		t.Errorf("actions = %v, want [start]", filter.Actions)
	}
	if len(filter.Results) != 1 {
		t.Errorf("results = %v, want one", filter.Results)
	}
	if filter.ComposeProject != "shop" || filter.ComposeService != "web" {
		t.Errorf("compose filters = %q/%q", filter.ComposeProject, filter.ComposeService)
	}
	if filter.ActorID != "abc123" || filter.Search != "nginx" {
		t.Errorf("actorId/search = %q/%q", filter.ActorID, filter.Search)
	}
	if filter.Since == nil || filter.Until == nil {
		t.Fatal("the time range must be parsed")
	}
	if filter.Sort != "observed" || filter.Direction != store.SortAsc {
		t.Errorf("sort = %q %q", filter.Sort, filter.Direction)
	}
}

func TestEventListRejectsInvalidFilters(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, nil)

	tests := []struct {
		name  string
		query string
	}{
		{"unknown type", "?type=quantum"},
		{"unknown result", "?result=maybe"},
		{"bad sort field", "?sort=attributes"},
		{"bad direction", "?direction=sideways"},
		{"bad since", "?since=yesterday"},
		{"bad until", "?until=2026-13-45"},
		{"inverted range", "?since=2026-08-04T00:00:00Z&until=2026-08-03T00:00:00Z"},
		{"page zero", "?page=0"},
		{"negative page", "?page=-1"},
		{"page size too large", "?pageSize=5000"},
		{"non-numeric page", "?page=abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, APIPrefix+"/events"+tc.query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}

			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Code != CodeInvalidRequest {
				t.Errorf("code = %q, want invalid_request", body.Error.Code)
			}
			// The message names the parameter, never the value supplied.
			if strings.Contains(body.Error.Message, "quantum") ||
				strings.Contains(body.Error.Message, "yesterday") {
				t.Errorf("message echoed the offending value: %q", body.Error.Message)
			}
		})
	}
}

func TestEventListWithoutTheEngineIsUnavailable(t *testing.T) {
	srv := newEventServer(t, nil, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// ---------------------------------------------------------- event detail --

func TestEventDetailReturnsLinksAndRedactionNotice(t *testing.T) {
	event := sampleEvent(7)
	srv := newEventServer(t, &fakeDockerEvents{get: &event}, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events/7", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body struct {
		Sequence   int64             `json:"sequence"`
		Attributes map[string]string `json:"attributes"`
		Redacted   bool              `json:"redacted"`
		Result     string            `json:"result"`
		Refresh    string            `json:"refreshRequested"`
		DockerTime time.Time         `json:"dockerTime"`
		ObservedAt time.Time         `json:"observedAt"`
		Links      struct {
			Container string `json:"container"`
		} `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Sequence != 7 {
		t.Errorf("sequence = %d, want 7", body.Sequence)
	}
	if !body.Redacted {
		t.Error("the payload must state that it is redacted")
	}
	if body.Result != string(domain.ResultProcessed) {
		t.Errorf("result = %q", body.Result)
	}
	if body.Refresh != string(domain.RefreshContainer) {
		t.Errorf("refreshRequested = %q", body.Refresh)
	}
	if body.DockerTime.IsZero() || body.ObservedAt.IsZero() {
		t.Error("both timestamps must be present; they are different facts")
	}
	if !strings.HasSuffix(body.Links.Container, event.ActorID) {
		t.Errorf("container link = %q, want it to name the actor", body.Links.Container)
	}
}

// Everything reaching this layer has already been redacted. The endpoint is
// unauthenticated, so this asserts nothing un-redacts it on the way out.
func TestEventDetailKeepsSensitiveAttributesMasked(t *testing.T) {
	event := sampleEvent(1)
	srv := newEventServer(t, &fakeDockerEvents{get: &event}, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events/1", nil)

	var body struct {
		Attributes map[string]string `json:"attributes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Attributes["DB_PASSWORD"] != domain.MaskedValue {
		t.Errorf("DB_PASSWORD = %q, want it masked", body.Attributes["DB_PASSWORD"])
	}
	if body.Attributes["name"] != "shop-web-1" {
		t.Error("structural metadata needed for correlation must survive")
	}
}

func TestEventDetailRejectsANonNumericId(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, nil)

	for _, id := range []string{"abc", "0", "-4"} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/events/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400", id, rec.Code)
		}
	}
}

func TestEventDetailNotFound(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{getErr: store.ErrNotFound}, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events/999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------- engine status --

func TestEventEngineStatus(t *testing.T) {
	connected := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	engine := &fakeEngine{
		enabled: true,
		status: domain.EventEngineStatus{
			Enabled:          true,
			State:            domain.ConnStateConnected,
			ConnectedSince:   &connected,
			QueueDepth:       2,
			QueueCapacity:    1024,
			Subscribers:      1,
			SubscriberLimit:  16,
			Counters:         domain.EventEngineCounters{Received: 42, Persisted: 40, Dropped: 1},
			Retention:        domain.EventRetentionPolicy{MaxAgeSeconds: 604800, MaxCount: 50000},
			CurrentBackoffMS: 0,
		},
	}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/event-engine", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body domain.EventEngineStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Enabled || body.State != domain.ConnStateConnected {
		t.Errorf("enabled/state = %v/%q", body.Enabled, body.State)
	}
	if body.QueueCapacity != 1024 || body.QueueDepth != 2 {
		t.Errorf("queue = %d/%d", body.QueueDepth, body.QueueCapacity)
	}
	if body.Counters.Received != 42 || body.Counters.Dropped != 1 {
		t.Errorf("counters = %+v", body.Counters)
	}
	if body.Retention.MaxCount != 50000 {
		t.Errorf("retention = %+v", body.Retention)
	}
}

// A deployment with no engine configured must render the same "off" state as a
// disabled one, not an unexplained error.
func TestEventEngineStatusWithoutAnEngineReportsDisabled(t *testing.T) {
	srv := newEventServer(t, nil, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/event-engine", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body domain.EventEngineStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Enabled || body.State != domain.ConnStateDisabled {
		t.Errorf("status = %+v, want disabled", body)
	}
}

func TestEventFilters(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{
		projects: []string{"shop", "blog"},
		actions:  []string{"start", "die"},
	}, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/event-filters", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body struct {
		Types      []string `json:"types"`
		Actions    []string `json:"actions"`
		Results    []string `json:"results"`
		Projects   []string `json:"projects"`
		SortFields []string `json:"sortFields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Types) == 0 || len(body.Results) == 0 || len(body.SortFields) == 0 {
		t.Errorf("vocabularies = %+v, want all populated", body)
	}
	if len(body.Projects) != 2 || len(body.Actions) != 2 {
		t.Errorf("projects/actions = %v/%v", body.Projects, body.Actions)
	}
}

// ------------------------------------------------------ unsupported writes --

// The event API is read-only. Every write method must be an honest 405, not a
// 404 that hides the fact the path exists.
func TestEventEndpointsRejectWrites(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, &fakeEngine{enabled: true})

	paths := []string{
		APIPrefix + "/events",
		APIPrefix + "/events/1",
		APIPrefix + "/events/stream",
		APIPrefix + "/event-engine",
		APIPrefix + "/event-filters",
	}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, path := range paths {
		for _, method := range methods {
			rec := do(t, srv, method, path, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status = %d, want 405", method, path, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow == "" {
				t.Errorf("%s %s: an Allow header must name the permitted methods", method, path)
			}
		}
	}
}

// There must be no endpoint that deletes event history in this phase.
func TestNoEventPruneEndpointExists(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, &fakeEngine{enabled: true})

	for _, path := range []string{
		APIPrefix + "/events/prune",
		APIPrefix + "/event-engine/prune",
		APIPrefix + "/event-engine/reconnect",
	} {
		rec := do(t, srv, http.MethodPost, path, nil)
		if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
			t.Errorf("%s returned %d; no destructive event endpoint may exist", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------- streaming --

// streamRequest runs the SSE handler against a cancellable context and returns
// the body once the handler exits.
func streamRequest(t *testing.T, srv *Server, target string, header http.Header, drive func(cancel context.CancelFunc)) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, req)
	}()

	drive(cancel)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sse handler did not return after cancellation; it leaks a goroutine")
	}
	return rec.Body.String()
}

func TestEventStreamSetsStreamingHeaders(t *testing.T) {
	engine := &fakeEngine{enabled: true, hint: 2 * time.Second}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, req)
	}()

	// Wait until the handler has subscribed, which is after the headers are set.
	deadline := time.Now().Add(5 * time.Second)
	for engine.subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	// Without this, a proxy buffers the response and the stream stops being live.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	// The security headers from the shared middleware must still apply.
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the shared security headers must apply to the stream")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "retry: 2000") {
		t.Errorf("the stream must send a reconnect hint; body = %q", body)
	}
	if !strings.Contains(body, "event: ready") {
		t.Errorf("the stream must open with a ready frame; body = %q", body)
	}
}

func TestEventStreamDeliversLiveEvents(t *testing.T) {
	engine := &fakeEngine{enabled: true}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	body := streamRequest(t, srv, APIPrefix+"/events/stream", nil, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		engine.publish(sampleEvent(11))
		// Let the handler write the frame before ending the response.
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if !strings.Contains(body, "event: docker-event") {
		t.Errorf("the live event was not framed; body = %q", body)
	}
	// The SSE id is the local sequence, which is what the browser echoes back.
	if !strings.Contains(body, "id: 11") {
		t.Errorf("the event id must be the local sequence; body = %q", body)
	}
	if !strings.Contains(body, `"fingerprint":"fp-11"`) {
		t.Errorf("the payload must carry the event; body = %q", body)
	}
}

// The stream is unauthenticated. Nothing sensitive may reach it, even though
// everything at this layer has already been redacted upstream.
func TestEventStreamEmitsOnlyRedactedValues(t *testing.T) {
	engine := &fakeEngine{enabled: true}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	body := streamRequest(t, srv, APIPrefix+"/events/stream", nil, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		engine.publish(sampleEvent(1))
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if !strings.Contains(body, domain.MaskedValue) {
		t.Error("the masked attribute did not survive to the wire")
	}
	if !strings.Contains(body, `"redacted":true`) {
		t.Error("the ready frame must state the redaction guarantee")
	}
}

func TestEventStreamSendsHeartbeats(t *testing.T) {
	engine := &fakeEngine{enabled: true, heartbeat: 10 * time.Millisecond}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	body := streamRequest(t, srv, APIPrefix+"/events/stream", nil, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(60 * time.Millisecond)
		cancel()
	})

	if !strings.Contains(body, ": heartbeat") {
		t.Errorf("an idle stream must send comment heartbeats; body = %q", body)
	}
}

func TestEventStreamReplaysFromLastEventId(t *testing.T) {
	events := &fakeDockerEvents{
		replay: []domain.DockerEvent{sampleEvent(6), sampleEvent(7)},
	}
	engine := &fakeEngine{enabled: true, replayLimit: 50}
	srv := newEventServer(t, events, engine)

	header := http.Header{"Last-Event-ID": []string{"5"}}
	body := streamRequest(t, srv, APIPrefix+"/events/stream", header, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if events.lastAfter != 5 {
		t.Errorf("replay asked for events after %d, want 5", events.lastAfter)
	}
	if !strings.Contains(body, "id: 6") || !strings.Contains(body, "id: 7") {
		t.Errorf("both replayed events must be sent; body = %q", body)
	}
	if !strings.Contains(body, `"replayed":2`) {
		t.Errorf("the ready frame must report the replay count; body = %q", body)
	}
}

// EventSource cannot set a header on its first connection, so a client
// resuming a persisted session has only the query parameter.
func TestEventStreamAcceptsLastEventIdAsAQueryParameter(t *testing.T) {
	events := &fakeDockerEvents{replay: []domain.DockerEvent{sampleEvent(9)}}
	engine := &fakeEngine{enabled: true, replayLimit: 50}
	srv := newEventServer(t, events, engine)

	streamRequest(t, srv, APIPrefix+"/events/stream?lastEventId=8", nil, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if events.lastAfter != 8 {
		t.Errorf("replay asked for events after %d, want 8", events.lastAfter)
	}
}

// A client that fell a long way behind must be told, not handed a silent hole.
func TestEventStreamCapsAndReportsReplayTruncation(t *testing.T) {
	events := &fakeDockerEvents{
		replay:    []domain.DockerEvent{sampleEvent(2), sampleEvent(3), sampleEvent(4)},
		replayTot: 500,
	}
	engine := &fakeEngine{enabled: true, replayLimit: 2}
	srv := newEventServer(t, events, engine)

	header := http.Header{"Last-Event-ID": []string{"1"}}
	body := streamRequest(t, srv, APIPrefix+"/events/stream", header, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if events.lastLimit != 2 {
		t.Errorf("replay limit passed through as %d, want 2", events.lastLimit)
	}
	if !strings.Contains(body, "event: replay-truncated") {
		t.Errorf("a capped replay must warn the client; body = %q", body)
	}
	if !strings.Contains(body, `"skipped":498`) {
		t.Errorf("the truncation notice must say how much was skipped; body = %q", body)
	}
}

// A malformed resume point must degrade to a live stream, not fail the request:
// the client is mid-reconnect and would otherwise be left with nothing.
func TestEventStreamIgnoresAMalformedLastEventId(t *testing.T) {
	events := &fakeDockerEvents{}
	engine := &fakeEngine{enabled: true, replayLimit: 50}
	srv := newEventServer(t, events, engine)

	header := http.Header{"Last-Event-ID": []string{"not-a-number"}}
	body := streamRequest(t, srv, APIPrefix+"/events/stream", header, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	})

	if !strings.Contains(body, "event: ready") {
		t.Errorf("the stream must open normally; body = %q", body)
	}
}

func TestEventStreamEnforcesTheSubscriberLimit(t *testing.T) {
	engine := &fakeEngine{enabled: true, limit: 1}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	// Occupy the only slot.
	if _, err := engine.Subscribe(); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events/stream", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 at the subscriber limit", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a refused subscriber must be told when to try again")
	}

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != CodeUnavailable {
		t.Errorf("code = %q, want service_unavailable", body.Error.Code)
	}
}

func TestEventStreamIsUnavailableWhenTheEngineIsDisabled(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, &fakeEngine{enabled: false})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/events/stream", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != CodeDisabled {
		t.Errorf("code = %q, want feature_disabled", body.Error.Code)
	}
}

// A client that disconnects must release its subscriber slot, or the limit
// would be consumed permanently by clients that are long gone.
func TestEventStreamReleasesTheSlotOnDisconnect(t *testing.T) {
	engine := &fakeEngine{enabled: true, limit: 2}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	streamRequest(t, srv, APIPrefix+"/events/stream", nil, func(cancel context.CancelFunc) {
		deadline := time.Now().Add(5 * time.Second)
		for engine.subscribers() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	})

	deadline := time.Now().Add(2 * time.Second)
	for engine.subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := engine.subscribers(); got != 0 {
		t.Errorf("%d subscribers still registered after a disconnect", got)
	}
}

// The engine shutting down must end the response rather than leave the handler
// blocked, or the HTTP server's drain would hang on it.
func TestEventStreamEndsWhenTheEngineStops(t *testing.T) {
	engine := &fakeEngine{enabled: true}
	srv := newEventServer(t, &fakeDockerEvents{}, engine)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, req)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for engine.subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Close every subscriber channel, as the broadcaster does on shutdown.
	engine.mu.Lock()
	for _, channel := range engine.subs {
		close(channel)
	}
	engine.subs = nil
	engine.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return when the engine stopped")
	}
}

// ------------------------------------------------------------- routing --

// GET /events/{id} and a bare /events/stream would be an ambiguous pair that
// ServeMux panics on. This asserts the router builds and both paths resolve to
// the handler intended.
func TestEventRoutesDoNotConflict(t *testing.T) {
	events := &fakeDockerEvents{get: func() *domain.DockerEvent { e := sampleEvent(3); return &e }()}
	engine := &fakeEngine{enabled: false}
	srv := newEventServer(t, events, engine)

	// The stream path must reach the stream handler, not the detail handler --
	// a disabled engine's 503 is the proof it did.
	rec := do(t, srv, http.MethodGet, APIPrefix+"/events/stream", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/events/stream reached the wrong handler: status = %d", rec.Code)
	}

	// A numeric path must reach the detail handler.
	rec = do(t, srv, http.MethodGet, APIPrefix+"/events/3", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("/events/3 status = %d, want 200: %s", rec.Code, rec.Body)
	}
}
