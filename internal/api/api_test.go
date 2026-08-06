package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// fakeHealth is a HealthChecker double.
type fakeHealth struct {
	report domain.HealthReport
	calls  int
}

func (f *fakeHealth) Check(context.Context) domain.HealthReport {
	f.calls++
	return f.report
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            {Data: []byte("<!doctype html><title>HarborMaster</title>")},
		"assets/app-abc123.js":  {Data: []byte("console.log('hm')")},
		"assets/app-abc123.css": {Data: []byte(".hm{color:red}")},
	}
}

func newTestServer(t *testing.T, health HealthChecker) *Server {
	t.Helper()
	return newAuthedServer(Options{
		Health: health,
		Logger: discardLogger(),
		Config: config.Server{MaxRequestBytes: 1024},
		Assets: testAssets(),
	})
}

func do(t *testing.T, srv *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(httptest.NewRequest(method, target, body)))
	return rec
}

func TestHealthEndpointReturnsReport(t *testing.T) {
	health := &fakeHealth{report: domain.HealthReport{
		Status:   domain.StatusDegraded,
		Database: domain.Component{Status: domain.StatusUp},
		Docker:   domain.Component{Status: domain.StatusDown, Detail: "docker engine unreachable"},
	}}
	srv := newTestServer(t, health)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var got domain.HealthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Status != domain.StatusDegraded {
		t.Errorf("status = %q, want %q", got.Status, domain.StatusDegraded)
	}
	if got.Docker.Status != domain.StatusDown {
		t.Errorf("docker = %q, want %q", got.Docker.Status, domain.StatusDown)
	}
	if health.calls != 1 {
		t.Errorf("Check calls = %d, want 1", health.calls)
	}
}

// The endpoint answering at all is the liveness signal; the body carries
// readiness. A 503 here would stop the UI distinguishing "HarborMaster down"
// from "Docker down".
func TestHealthEndpointStays200WhenDegraded(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{report: domain.HealthReport{Status: domain.StatusUnhealthy}})

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/health", nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even when unhealthy", rec.Code)
	}
}

func TestVersionEndpoint(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/version", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"version", "commit", "buildDate", "goVersion", "platform"} {
		if got[key] == "" {
			t.Errorf("field %q is empty", key)
		}
	}
}

func TestUnknownAPIPathReturnsJSONNotHTML(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/no-such-collection", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var got ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unknown API paths must return JSON, got %q: %v", rec.Body.String(), err)
	}
	if got.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", got.Error.Code, CodeNotFound)
	}
	if got.RequestID == "" {
		t.Error("error envelope should carry the request ID")
	}
}

func TestWriteMethodsOnHealthAreRejected(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(t, srv, method, APIPrefix+"/health", strings.NewReader("{}"))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /health status = %d, want 405", method, rec.Code)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/health", nil)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}

func TestRequestIDIsGeneratedAndNotTakenFromTheClient(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/health", nil)
	req.Header.Set(RequestIDHeader, "attacker-supplied")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))

	got := rec.Header().Get(RequestIDHeader)
	if got == "" {
		t.Fatal("expected a generated request ID")
	}
	if got == "attacker-supplied" {
		t.Error("client-supplied request IDs must not be echoed back")
	}
}

func TestOversizedRequestIsRejected(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{}) // limit is 1024 bytes

	rec := do(t, srv, http.MethodPost, APIPrefix+"/health", strings.NewReader(strings.Repeat("a", 4096)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	var got ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Error.Code != CodePayloadTooLarge {
		t.Errorf("code = %q, want %q", got.Error.Code, CodePayloadTooLarge)
	}
}

// A panic must produce a generic 500. Leaking a stack trace would disclose
// source paths and internal structure to anyone who can reach the API.
func TestPanicIsRecoveredWithoutLeakingAStackTrace(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret internal failure at /srv/harbormaster/internal/api")
	})
	handler := chain(panicking, withRequestID, withRecovery(discardLogger()))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	body := rec.Body.String()
	for _, leak := range []string{"secret internal failure", "goroutine", "/srv/harbormaster"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaked %q: %s", leak, body)
		}
	}

	var got ErrorResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("panic response must be JSON: %v", err)
	}
	if got.Error.Code != CodeInternal {
		t.Errorf("code = %q, want %q", got.Error.Code, CodeInternal)
	}
}

func TestStaticAssetIsServed(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	rec := do(t, srv, http.MethodGet, "/assets/app-abc123.js", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "console.log") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	// Hashed filenames are safe to cache forever.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want an immutable policy for hashed assets", cc)
	}
}

// Client-side routes must survive a page reload.
func TestUnknownPathFallsBackToIndex(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	for _, path := range []string{"/", "/containers", "/snapshots/42"} {
		rec := do(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "HarborMaster") {
			t.Errorf("GET %s did not return the SPA shell: %s", path, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

// A binary built without a frontend bundle must still serve the API.
func TestServerWithoutAssetsStillServesAPI(t *testing.T) {
	srv := newAuthedServer(Options{
		Health: &fakeHealth{},
		Logger: discardLogger(),
		Config: config.Server{MaxRequestBytes: 1024},
		Assets: nil,
	})

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/health", nil); rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("root status = %d, want 404 when no bundle is embedded", rec.Code)
	}
}

func TestHTTPServerAppliesConfiguredTimeouts(t *testing.T) {
	cfg := config.Server{
		Addr:              "127.0.0.1:8080",
		MaxRequestBytes:   1024,
		ReadHeaderTimeout: config.DefaultReadHeaderTimeout,
		ReadTimeout:       config.DefaultReadTimeout,
		WriteTimeout:      config.DefaultWriteTimeout,
		IdleTimeout:       config.DefaultIdleTimeout,
	}
	srv := newAuthedServer(Options{Health: &fakeHealth{}, Logger: discardLogger(), Config: cfg})

	httpServer := srv.HTTPServer()

	if httpServer.Addr != cfg.Addr {
		t.Errorf("Addr = %q, want %q", httpServer.Addr, cfg.Addr)
	}
	// A server with any unset deadline can be held open by a slow client.
	if httpServer.ReadHeaderTimeout <= 0 || httpServer.ReadTimeout <= 0 ||
		httpServer.WriteTimeout <= 0 || httpServer.IdleTimeout <= 0 {
		t.Errorf("every timeout must be set: %+v", httpServer)
	}
	if httpServer.MaxHeaderBytes <= 0 {
		t.Error("MaxHeaderBytes must be bounded")
	}
}
