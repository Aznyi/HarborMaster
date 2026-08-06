package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The event stream's session lifetime.
//
// # What the audit found
//
// Every other route re-reads the account on each request, which is what makes a
// disablement, a demotion, or a password change take effect immediately. A
// stream makes ONE request and then runs for as long as the client holds the
// connection -- up to the session's absolute lifetime, seven days by default.
//
// Authorized only at connect, it was the one place in HarborMaster where
// revoking a session did not stop the flow of estate data. These tests pin the
// fix: the session is re-checked on every heartbeat, and the stream ends the
// moment it stops being valid.

// streamServer builds a server whose event engine is enabled and whose
// heartbeat fires fast enough for a test to observe.
func streamServer(t *testing.T, role domain.Role) (*Server, *stubAuth, *fakeEngine) {
	t.Helper()

	engine := &fakeEngine{enabled: true, heartbeat: 20 * time.Millisecond}
	srv, auth, _ := asRole(Options{
		Health:      &fakeHealth{},
		EventEngine: engine,
		Config:      config.Server{MaxRequestBytes: 4096},
		Assets:      testAssets(),
	}, role)
	return srv, auth, engine
}

// syncRecorder is a ResponseWriter a test can read WHILE the handler writes.
//
// httptest.ResponseRecorder is not safe for concurrent use, and a stream test
// is concurrent by nature: the handler runs until something ends it, and the
// assertion is about what it wrote in the meantime. Reading the recorder's
// buffer from the test goroutine is a genuine data race, and `-race` says so.
//
// Every method takes the same mutex, including text(), so a read never observes
// a partially written frame.
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
	code   int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{header: make(http.Header), code: http.StatusOK}
}

func (r *syncRecorder) Header() http.Header { return r.header }

func (r *syncRecorder) Write(payload []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(payload)
}

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

// Flush satisfies http.Flusher, which http.NewResponseController needs for the
// stream's per-frame flush. A recorder has nothing to flush.
func (r *syncRecorder) Flush() {}

// text returns everything written so far.
func (r *syncRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// openStream starts a stream and returns a function that stops it, plus the
// recorder the frames land in.
func openStream(t *testing.T, srv *Server) (*syncRecorder, context.CancelFunc, chan struct{}) {
	t.Helper()

	request := authed(httptest.NewRequest(http.MethodGet, APIPrefix+"/events/stream", nil))
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)

	recorder := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(recorder, request)
	}()
	return recorder, cancel, done
}

// awaitStreamEnd waits for the handler to return.
func awaitStreamEnd(t *testing.T, done chan struct{}, why string) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("the stream did not end after %s", why)
	}
}

// TestARevokedSessionEndsAnOpenStream is the headline property.
func TestARevokedSessionEndsAnOpenStream(t *testing.T) {
	srv, auth, _ := streamServer(t, domain.RoleViewer)

	recorder, cancel, done := openStream(t, srv)
	defer cancel()

	// The stream is live: the ready frame has been written.
	waitForBody(t, recorder, "event: ready")

	// Revoke, exactly as a sign-out, a password change, a disablement, or an
	// administrator ending the session would.
	auth.mu.Lock()
	auth.rejectToken = true
	auth.mu.Unlock()

	awaitStreamEnd(t, done, "its session was revoked")

	body := recorder.text()
	if !strings.Contains(body, "event: closed") {
		t.Errorf("the stream ended without telling the client why:\n%s", body)
	}
	if !strings.Contains(body, "no longer valid") {
		t.Errorf("the closing frame does not say the session ended:\n%s", body)
	}
}

// TestADemotionEndsAnOpenStream covers the case a session check alone misses.
//
// A demotion from operator to viewer leaves a VALID session that no longer
// holds `event:read`. Re-checking only the session would keep the stream open
// for an account that may no longer read events at all.
func TestADemotionEndsAnOpenStream(t *testing.T) {
	srv, auth, _ := streamServer(t, domain.RoleOperator)

	recorder, cancel, done := openStream(t, srv)
	defer cancel()
	waitForBody(t, recorder, "event: ready")

	// A role that holds no permissions at all -- the shape a demotion to an
	// unrecognised role takes, and the strictest form of the check.
	auth.mu.Lock()
	auth.user.Role = domain.Role("revoked")
	auth.mu.Unlock()

	awaitStreamEnd(t, done, "the account lost event:read")
}

// TestAStreamSurvivesWhileItsSessionIsValid is the other half.
//
// A re-authorization that ended healthy streams would be a worse bug than the
// one it fixed, so the test that the stream KEEPS running matters as much.
func TestAStreamSurvivesWhileItsSessionIsValid(t *testing.T) {
	srv, _, _ := streamServer(t, domain.RoleViewer)

	recorder, cancel, done := openStream(t, srv)
	waitForBody(t, recorder, "event: ready")

	// Several heartbeats' worth of time, each of which re-checks.
	time.Sleep(150 * time.Millisecond)

	select {
	case <-done:
		t.Fatalf("the stream ended while its session was still valid:\n%s", recorder.text())
	default:
	}
	if !strings.Contains(recorder.text(), "heartbeat") {
		t.Error("no heartbeat was written, so the re-authorization path never ran")
	}

	cancel()
	awaitStreamEnd(t, done, "the client disconnected")
}

// TestAStreamEndsWhenTheSessionCannotBeChecked is the fail-closed direction.
//
// A stream is a standing grant. A grant that cannot be reconfirmed is one that
// should stop, not one that should continue on the strength of a lookup that
// did not answer.
func TestAStreamEndsWhenTheSessionCannotBeChecked(t *testing.T) {
	srv, auth, _ := streamServer(t, domain.RoleViewer)

	recorder, cancel, done := openStream(t, srv)
	defer cancel()
	waitForBody(t, recorder, "event: ready")

	// The identity resolves to a DIFFERENT session, which is what a replaced
	// row looks like from here.
	auth.mu.Lock()
	auth.user.UserID = "usr_somebodyelse00000000"
	auth.mu.Unlock()

	awaitStreamEnd(t, done, "the token resolved to another account")
}

// TestAnAnonymousStreamIsRefusedBeforeItOpens keeps the connect-time check
// honest as well as the running one.
func TestAnAnonymousStreamIsRefusedBeforeItOpens(t *testing.T) {
	srv, _, _ := streamServer(t, domain.RoleViewer)

	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, anonymous(
		httptest.NewRequest(http.MethodGet, APIPrefix+"/events/stream", nil)))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous stream = %d, want 401", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "event:") {
		t.Error("an anonymous request received stream frames")
	}
}

// TestAStreamWithoutTheEventPermissionIsRefused pins the connect-time
// authorization, so the re-check is not the only thing standing between a
// viewer-less role and the estate.
func TestAStreamWithoutTheEventPermissionIsRefused(t *testing.T) {
	auth := newStubAuth(domain.Role("nothing"))
	srv := newAuthedServer(Options{
		Health:      &fakeHealth{},
		EventEngine: &fakeEngine{enabled: true, heartbeat: time.Second},
		Auth:        auth,
		Config:      config.Server{MaxRequestBytes: 4096},
		Assets:      testAssets(),
	})

	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, authed(
		httptest.NewRequest(http.MethodGet, APIPrefix+"/events/stream", nil)))

	if recorder.Code != http.StatusForbidden {
		t.Errorf("a stream without event:read = %d, want 403", recorder.Code)
	}
}

// waitForBody blocks until the recorder contains a marker.
func waitForBody(t *testing.T, recorder *syncRecorder, marker string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(recorder.text(), marker) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the stream never wrote %q:\n%s", marker, recorder.text())
}

// streamIdentity is the identity a stream is opened under, used above to build
// the "resolved to another account" case.
var _ = service.Authenticated{}
