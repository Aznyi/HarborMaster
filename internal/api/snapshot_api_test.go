package api

import (
	"context"
	"encoding/json"
	"errors"
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

// The distinctive values used to prove nothing sensitive reaches a response.
const (
	apiSecretValue  = "s3cr3t-api-value"
	apiSecretDigest = "digest-must-never-be-served"
)

// fakeSnapshots is a SnapshotReader double.
type fakeSnapshots struct {
	mu        sync.Mutex
	snapshots map[int64]domain.Snapshot
	env       []domain.SnapshotEnvEntry
	mounts    []domain.SnapshotMountRow
	networks  []domain.SnapshotNetworkRow
	listErr   error
}

func newFakeSnapshots() *fakeSnapshots {
	spec := domain.SnapshotSpec{
		SpecVersion: domain.SnapshotSpecVersion,
		Identity:    domain.SpecIdentity{ContainerID: "c1", ContainerName: "web"},
		Image:       domain.SpecImage{Reference: "nginx:1.27", ImageID: "sha256:aaaa"},
		Labels:      []domain.Label{{Key: "app", Value: "web"}},
	}
	blob, _ := json.Marshal(spec)

	return &fakeSnapshots{
		snapshots: map[int64]domain.Snapshot{
			1: {
				ID: 1, ContainerID: "c1", ContainerName: "web",
				ImageReference: "nginx:1.27", SpecVersion: domain.SnapshotSpecVersion,
				SpecJSON: blob, Checksum: strings.Repeat("a", 64),
				Trigger: domain.SnapshotTriggerManual, DigestKeyID: "key1",
				ReadinessStatus: domain.ReadinessUnknown,
				CreatedAt:       time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				Warnings:        []domain.InventoryWarning{},
			},
			2: {
				ID: 2, ContainerID: "c1", ContainerName: "web",
				SpecVersion: domain.SnapshotSpecVersion, SpecJSON: blob,
				Checksum: strings.Repeat("b", 64), Trigger: domain.SnapshotTriggerAPI,
				DigestKeyID: "key1", ReadinessStatus: domain.ReadinessUnknown,
				CreatedAt: time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
				Warnings:  []domain.InventoryWarning{},
			},
		},
		env: []domain.SnapshotEnvEntry{
			{Position: 0, Key: "PATH", Classification: domain.SensitivityNormal, Present: true, Value: "/usr/bin"},
			{
				Position: 1, Key: "DB_PASSWORD", Classification: domain.SensitivitySensitive,
				Present: true, Length: 16, Digest: apiSecretDigest,
				DigestAlgorithm: domain.DigestHMACSHA256, DigestKeyID: "key1",
			},
		},
		mounts:   []domain.SnapshotMountRow{{Destination: "/data", Type: domain.MountTypeVolume, VolumeName: "web-data"}},
		networks: []domain.SnapshotNetworkRow{{NetworkName: "bridge", Aliases: []string{"web"}}},
	}
}

func (f *fakeSnapshots) List(_ context.Context, filter store.SnapshotFilter) ([]domain.Snapshot, int, error) {
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.Snapshot, 0, len(f.snapshots))
	for _, s := range f.snapshots {
		if filter.ContainerID != "" && s.ContainerID != filter.ContainerID {
			continue
		}
		out = append(out, s)
	}
	return out, len(out), nil
}

func (f *fakeSnapshots) Get(_ context.Context, id int64) (domain.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snapshots[id]
	if !ok {
		return domain.Snapshot{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeSnapshots) Environment(context.Context, int64) ([]domain.SnapshotEnvEntry, error) {
	return f.env, nil
}
func (f *fakeSnapshots) Mounts(context.Context, int64) ([]domain.SnapshotMountRow, error) {
	return f.mounts, nil
}
func (f *fakeSnapshots) Networks(context.Context, int64) ([]domain.SnapshotNetworkRow, error) {
	return f.networks, nil
}

// fakeCapture is a SnapshotCapturer double.
type fakeCapture struct {
	enabled bool
	err     error
	calls   int
	dedup   bool
	mu      sync.Mutex
}

func (f *fakeCapture) Enabled() bool { return f.enabled }

func (f *fakeCapture) Capture(_ context.Context, req service.CaptureRequest) (domain.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return domain.Snapshot{}, f.err
	}
	return domain.Snapshot{
		ID: 99, ContainerID: req.ContainerID, Reason: req.Reason,
		Trigger: req.Trigger, Deduplicated: f.dedup,
		Warnings: []domain.InventoryWarning{},
	}, nil
}

func (f *fakeCapture) CaptureStartedAt(string) (time.Time, bool) {
	return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), true
}

// fakeReadiness is a SnapshotReadinessEvaluator double.
type fakeReadiness struct{ err error }

func (f *fakeReadiness) Evaluate(
	_ context.Context, snapshot domain.Snapshot,
	env []domain.SnapshotEnvEntry, _ []domain.SnapshotMountRow, _ []domain.SnapshotNetworkRow,
) (domain.ReadinessReport, error) {
	if f.err != nil {
		return domain.ReadinessReport{}, f.err
	}
	return domain.ReadinessReport{
		SnapshotID: snapshot.ID,
		Status:     domain.ReadinessWarning,
		Checks: []domain.ReadinessCheck{
			{ID: domain.CheckDaemonReachable, Status: domain.ReadinessReady},
			{ID: domain.CheckSecretsAvailable, Status: domain.ReadinessWarning,
				Detail: "1 sensitive value must be supplied externally"},
		},
		EvaluatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

// fakeContainerReader serves the current configuration for a diff.
type fakeContainerReader struct {
	detail *domain.ContainerDetail
	err    error
}

func (f *fakeContainerReader) List(context.Context, store.ContainerFilter) ([]domain.ContainerSummary, int, error) {
	return nil, 0, nil
}
func (f *fakeContainerReader) Attention(
	context.Context, []store.ContainerKey,
) (map[string]domain.ContainerEvidence, error) {
	return map[string]domain.ContainerEvidence{}, nil
}
func (f *fakeContainerReader) Get(context.Context, string) (*domain.ContainerDetail, error) {
	return f.detail, f.err
}
func (f *fakeContainerReader) ResolveID(_ context.Context, ref string) (string, error) {
	return ref, nil
}
func (f *fakeContainerReader) RawInspection(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (f *fakeContainerReader) DistinctComposeProjects(context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeContainerReader) DistinctImages(context.Context) ([]string, error) { return nil, nil }

// stubInventory is enough of an InventoryReader for the refresh route to be
// live, so the write guard on it can be exercised.
type stubInventory struct{}

func (stubInventory) Enabled() bool { return true }
func (stubInventory) Status(context.Context) (domain.InventoryStatus, error) {
	return domain.InventoryStatus{}, nil
}
func (stubInventory) TriggerAsync(domain.RefreshTrigger) (bool, time.Time) {
	return true, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
}
func (stubInventory) CheckRuntime(context.Context) error { return nil }

type snapshotTestServer struct {
	*Server
	snapshots *fakeSnapshots
	capture   *fakeCapture
}

func newSnapshotServer(t *testing.T) *snapshotTestServer {
	t.Helper()

	snapshots := newFakeSnapshots()
	capture := &fakeCapture{enabled: true}
	detail := domain.ContainerDetail{
		Overview: domain.ContainerSummary{ID: "c1", Name: "web"},
		Labels:   []domain.Label{{Key: "app", Value: "api"}},
	}

	srv := newAuthedServer(Options{
		Health:     &fakeHealth{},
		Inventory:  stubInventory{},
		Containers: &fakeContainerReader{detail: &detail},
		Snapshots:  snapshots,
		Capture:    capture,
		Diffs:      service.NewDiffEngine(config.Snapshots{MaxConcurrentDiffs: 4, DiffTimeout: time.Second}),
		Readiness:  &fakeReadiness{},
		SnapshotSpecBuilder: func(d domain.ContainerDetail) domain.SnapshotSpec {
			return domain.SnapshotSpec{
				SpecVersion: domain.SnapshotSpecVersion,
				Identity:    domain.SpecIdentity{ContainerID: d.Overview.ID, ContainerName: d.Overview.Name},
				Labels:      d.Labels,
			}
		},
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{MaxReasonBytes: 500, WriteRateLimit: 600, WriteRateBurst: 100},
	})

	return &snapshotTestServer{Server: srv, snapshots: snapshots, capture: capture}
}

// postJSON issues a well-formed same-origin JSON POST.
func postJSON(t *testing.T, srv *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))
	return rec
}

// --- the invariant that matters most ---------------------------------------

// HarborMaster must not restore, roll back, or apply a snapshot, and no route
// may exist that looks like it does.
func TestNoRestoreEndpointExists(t *testing.T) {
	srv := newSnapshotServer(t)

	for _, path := range []string{
		"/api/v1/snapshots/1/restore",
		"/api/v1/snapshots/1/rollback",
		"/api/v1/snapshots/1/apply",
		"/api/v1/snapshots/1/recreate",
		"/api/v1/snapshots/1/revert",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, authed(req))

			if rec.Code == http.StatusOK || rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted {
				t.Errorf("%s %s returned %d; HarborMaster must never restore a container",
					method, path, rec.Code)
			}
		}
	}
}

// A snapshot may not be deleted or edited through the API either: it is
// evidence, and retention is the only thing that removes one.
func TestSnapshotsCannotBeMutatedThroughTheAPI(t *testing.T) {
	srv := newSnapshotServer(t)

	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := do(t, srv.Server, method, "/api/v1/snapshots/1", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/snapshots/1 returned %d, want 405", method, rec.Code)
		}
	}
}

// --- list -------------------------------------------------------------------

func TestSnapshotListReturnsPagedEnvelope(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body listResponse[domain.Snapshot]
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Errorf("items = %d, want 2", len(body.Items))
	}
	if body.Pagination.TotalItems != 2 {
		t.Errorf("totalItems = %d, want 2", body.Pagination.TotalItems)
	}
}

// The list must not carry every snapshot's full configuration document: a page
// of fifty would otherwise ship fifty complete configurations to render a table
// that shows none of them.
func TestSnapshotListOmitsTheDocument(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots", nil)

	body := rec.Body.String()
	// "specVersion" is a scalar column on every row and is expected. What must
	// be absent is the document itself, whose presence shows as the "spec" key
	// and the sections inside it.
	if strings.Contains(body, `"spec":`) {
		t.Error("the list response carried a canonical document; it should be detail-only")
	}
	if strings.Contains(body, `"identity"`) {
		t.Error("document sections leaked into the list response")
	}

	// The detail endpoint DOES carry it, or the omission above would just be a
	// missing feature.
	detail := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1", nil)
	if !strings.Contains(detail.Body.String(), `"identity"`) {
		t.Error("the detail response is missing the canonical document")
	}
}

func TestSnapshotListRejectsBadFilters(t *testing.T) {
	srv := newSnapshotServer(t)

	for _, target := range []string{
		"/api/v1/snapshots?page=0",
		"/api/v1/snapshots?page=-1",
		"/api/v1/snapshots?pageSize=0",
		"/api/v1/snapshots?pageSize=10000",
		"/api/v1/snapshots?trigger=restore",
		"/api/v1/snapshots?readiness=definitely",
		"/api/v1/snapshots?sort=spec_json",
		// Percent-encoded, so the semicolon survives net/url's parsing and
		// actually reaches the allowlist rather than being dropped upstream.
		"/api/v1/snapshots?sort=created_at%3BDROP%20TABLE%20snapshots",
		"/api/v1/snapshots?sort=id)%20--",
		"/api/v1/snapshots?direction=sideways",
		"/api/v1/snapshots?since=yesterday",
		"/api/v1/snapshots?checksum=nothex!!",
		"/api/v1/snapshots?since=2026-05-02T00:00:00Z&until=2026-05-01T00:00:00Z",
	} {
		rec := do(t, srv.Server, http.MethodGet, target, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s returned %d, want 400", target, rec.Code)
		}
	}
}

// A validation error must never echo the offending value back.
func TestFilterErrorsDoNotEchoInput(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet,
		"/api/v1/snapshots?trigger=%3Cscript%3Ealert(1)%3C%2Fscript%3E", nil)

	if strings.Contains(rec.Body.String(), "script") {
		t.Errorf("the error echoed caller input: %s", rec.Body)
	}
}

// --- detail -----------------------------------------------------------------

func TestSnapshotDetailReturnsSections(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body SnapshotDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Environment) != 2 || len(body.Mounts) != 1 || len(body.Networks) != 1 {
		t.Errorf("sections not returned: %+v", body)
	}
}

func TestSnapshotDetailNotFound(t *testing.T) {
	srv := newSnapshotServer(t)
	if rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/404", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSnapshotDetailRejectsMalformedID(t *testing.T) {
	srv := newSnapshotServer(t)
	for _, id := range []string{"abc", "0", "-1", "1.5"} {
		rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q returned %d, want 400", id, rec.Code)
		}
	}
}

// --- the secret guarantee ---------------------------------------------------

// No endpoint may return a secret value OR a digest.
func TestSnapshotResponsesNeverContainSecrets(t *testing.T) {
	srv := newSnapshotServer(t)

	for _, target := range []string{
		"/api/v1/snapshots",
		"/api/v1/snapshots/1",
		"/api/v1/snapshots/1/diff?against=2",
		"/api/v1/snapshots/1/diff?against=current",
		"/api/v1/snapshots/1/restore-readiness",
	} {
		rec := do(t, srv.Server, http.MethodGet, target, nil)
		body := rec.Body.String()

		for _, needle := range []string{apiSecretValue, apiSecretDigest} {
			if strings.Contains(body, needle) {
				t.Errorf("GET %s leaked %q", target, needle)
			}
		}
		// The variable NAME is fine and expected; the value is not.
		if strings.Contains(body, `"value":"`+apiSecretValue) {
			t.Errorf("GET %s served a sensitive value", target)
		}
	}
}

// --- capture ----------------------------------------------------------------

func TestSnapshotCreateReturns201(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := postJSON(t, srv.Server, "/api/v1/snapshots", `{"containerId":"c1","reason":"before upgrade"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
}

// An unchanged configuration creates nothing, so it is 200 rather than 201.
func TestSnapshotCreateReturns200WhenDeduplicated(t *testing.T) {
	srv := newSnapshotServer(t)
	srv.capture.dedup = true

	rec := postJSON(t, srv.Server, "/api/v1/snapshots", `{"containerId":"c1"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a deduplicated capture", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"deduplicated":true`) {
		t.Error("the response should say the capture was a no-op")
	}
}

func TestSnapshotCreateRejectsMalformedBodies(t *testing.T) {
	cases := map[string]struct {
		body string
		want int
	}{
		"not json":            {`not json at all`, http.StatusBadRequest},
		"empty body":          {``, http.StatusBadRequest},
		"unknown field":       {`{"containerId":"c1","surprise":true}`, http.StatusBadRequest},
		"two objects":         {`{"containerId":"c1"}{"containerId":"c2"}`, http.StatusBadRequest},
		"trailing content":    {`{"containerId":"c1"} trailing`, http.StatusBadRequest},
		"json array":          {`[{"containerId":"c1"}]`, http.StatusBadRequest},
		"missing containerId": {`{}`, http.StatusBadRequest},
		"wrong type":          {`{"containerId":123}`, http.StatusBadRequest},
		"invalid utf8":        {"{\"containerId\":\"c1\",\"reason\":\"\xff\xfe\"}", http.StatusBadRequest},
		"control characters":  {`{"containerId":"c1","reason":"line1\nline2"}`, http.StatusBadRequest},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newSnapshotServer(t)
			rec := postJSON(t, srv.Server, "/api/v1/snapshots", tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestSnapshotCreateRejectsOversizedReason(t *testing.T) {
	srv := newSnapshotServer(t)
	body := `{"containerId":"c1","reason":"` + strings.Repeat("x", 600) + `"}`

	if rec := postJSON(t, srv.Server, "/api/v1/snapshots", body); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized reason", rec.Code)
	}
}

func TestSnapshotCreateRejectsOversizedBody(t *testing.T) {
	srv := newSnapshotServer(t)
	body := `{"containerId":"` + strings.Repeat("x", 8192) + `"}`

	rec := postJSON(t, srv.Server, "/api/v1/snapshots", body)
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 413 or 400 for an oversized body", rec.Code)
	}
}

func TestSnapshotCreateRequiresJSONContentType(t *testing.T) {
	srv := newSnapshotServer(t)

	for _, contentType := range []string{
		"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data",
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots",
			strings.NewReader(`{"containerId":"c1"}`))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authed(req))

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q returned %d, want 415", contentType, rec.Code)
		}
	}
}

func TestSnapshotCreateConflictWhenCaptureInProgress(t *testing.T) {
	srv := newSnapshotServer(t)
	srv.capture.err = service.ErrCaptureInProgress

	rec := postJSON(t, srv.Server, "/api/v1/snapshots", `{"containerId":"c1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"inProgress":true`) {
		t.Error("the 409 body should describe the in-flight capture")
	}
}

func TestSnapshotCreateNotFound(t *testing.T) {
	srv := newSnapshotServer(t)
	srv.capture.err = store.ErrNotFound

	if rec := postJSON(t, srv.Server, "/api/v1/snapshots", `{"containerId":"nope"}`); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSnapshotCreateUnavailableWhenDisabled(t *testing.T) {
	srv := newSnapshotServer(t)
	srv.capture.enabled = false

	if rec := postJSON(t, srv.Server, "/api/v1/snapshots", `{"containerId":"c1"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// --- write-endpoint hardening ------------------------------------------------

func TestCrossSiteFetchMetadataIsRejected(t *testing.T) {
	for _, target := range []string{"/api/v1/snapshots", "/api/v1/inventory/refresh"} {
		t.Run(target, func(t *testing.T) {
			srv := newSnapshotServer(t)

			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"containerId":"c1"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, authed(req))

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestCrossOriginIsRejected(t *testing.T) {
	srv := newSnapshotServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots", strings.NewReader(`{"containerId":"c1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestSameOriginIsAccepted(t *testing.T) {
	srv := newSnapshotServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots", strings.NewReader(`{"containerId":"c1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
}

// THE test for the layering claim: Fetch Metadata is defence in depth, so every
// real control must still hold with all of it stripped.
func TestValidationHoldsWithoutFetchMetadata(t *testing.T) {
	bodies := []string{
		`not json`,
		`{"unknown":1}`,
		`{}{}`,
		`{"containerId":"c1"} trailing`,
		`{"containerId":""}`,
		`{"containerId":"c1","reason":"` + strings.Repeat("x", 600) + `"}`,
	}

	for _, body := range bodies {
		srv := newSnapshotServer(t)

		// No Origin, no Sec-Fetch-*: exactly what curl sends.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authed(req))

		if rec.Code < 400 {
			t.Errorf("body %q accepted with no Fetch Metadata headers: %d", body, rec.Code)
		}
	}
}

func TestRateLimitHoldsWithoutFetchMetadata(t *testing.T) {
	srv := newAuthedServer(Options{
		Health:    &fakeHealth{},
		Snapshots: newFakeSnapshots(),
		Capture:   &fakeCapture{enabled: true},
		Logger:    discardLogger(),
		Config:    config.Server{MaxRequestBytes: 4096},
		// Two requests, then refuse.
		SnapshotConfig: config.Snapshots{MaxReasonBytes: 500, WriteRateLimit: 1, WriteRateBurst: 2},
	})

	var limited bool
	for range 6 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots", strings.NewReader(`{"containerId":"c1"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authed(req))

		if rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 must carry Retry-After")
			}
			break
		}
	}
	if !limited {
		t.Error("the rate limit never engaged; it is the control that bounds sustained abuse")
	}
}

// --- diff -------------------------------------------------------------------

func TestSnapshotDiffAgainstAnotherSnapshot(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1/diff?against=2", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var diff domain.SnapshotDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if diff.FromSnapshotID != 1 || diff.ToSnapshotID != 2 {
		t.Errorf("wrong sides: %+v", diff)
	}
}

func TestSnapshotDiffAgainstCurrent(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1/diff", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var diff domain.SnapshotDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if !diff.AgainstCurrent {
		t.Error("AgainstCurrent not set")
	}
	// The fixture's current labels differ from the snapshot's, so this must
	// register as a change rather than silently comparing nothing.
	if diff.Identical {
		t.Error("the diff reported identical against a container with different labels")
	}
}

func TestSnapshotDiffRejectsBadParameters(t *testing.T) {
	srv := newSnapshotServer(t)

	for _, target := range []string{
		"/api/v1/snapshots/1/diff?against=abc",
		"/api/v1/snapshots/1/diff?against=0",
		"/api/v1/snapshots/1/diff?against=-1",
		"/api/v1/snapshots/1/diff?group=spec_json",
		"/api/v1/snapshots/1/diff?group=$.environment",
		"/api/v1/snapshots/1/diff?includeUnchanged=maybe",
	} {
		if rec := do(t, srv.Server, http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s returned %d, want 400", target, rec.Code)
		}
	}
}

func TestSnapshotDiffAgainstMissingSnapshot(t *testing.T) {
	srv := newSnapshotServer(t)
	if rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1/diff?against=404", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSnapshotDiffGroupSelection(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1/diff?against=2&group=labels", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var diff domain.SnapshotDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.Groups) != 1 || diff.Groups[0].Name != domain.DiffGroupLabels {
		t.Errorf("group selection ignored: %+v", diff.Groups)
	}
}

func TestSnapshotDiffBusyReturns429(t *testing.T) {
	engine := service.NewDiffEngine(config.Snapshots{MaxConcurrentDiffs: 1, DiffTimeout: time.Second})

	srv := newAuthedServer(Options{
		Health:    &fakeHealth{},
		Snapshots: newFakeSnapshots(),
		Diffs:     &busyDiffer{},
		Logger:    discardLogger(),
		Config:    config.Server{MaxRequestBytes: 4096},
	})
	_ = engine

	rec := do(t, srv, http.MethodGet, "/api/v1/snapshots/1/diff?against=2", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 must carry Retry-After")
	}
}

// busyDiffer always reports the concurrency ceiling.
type busyDiffer struct{}

func (busyDiffer) Diff(context.Context, service.DiffInput, service.DiffInput, service.DiffOptions) (domain.SnapshotDiff, error) {
	return domain.SnapshotDiff{}, service.ErrDiffBusy
}

// --- readiness --------------------------------------------------------------

func TestSnapshotReadinessReturnsReport(t *testing.T) {
	srv := newSnapshotServer(t)
	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/1/restore-readiness", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var report domain.ReadinessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != domain.ReadinessWarning {
		t.Errorf("Status = %q", report.Status)
	}
	if len(report.Checks) != 2 {
		t.Errorf("checks = %d", len(report.Checks))
	}
}

func TestSnapshotReadinessNotFound(t *testing.T) {
	srv := newSnapshotServer(t)
	if rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots/404/restore-readiness", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- unconfigured deployment ------------------------------------------------

func TestSnapshotEndpointsReport503WhenUnconfigured(t *testing.T) {
	srv := newAuthedServer(Options{
		Health: &fakeHealth{},
		Logger: discardLogger(),
		Config: config.Server{MaxRequestBytes: 4096},
	})

	for _, target := range []string{
		"/api/v1/snapshots",
		"/api/v1/snapshots/1",
		"/api/v1/snapshots/1/diff",
		"/api/v1/snapshots/1/restore-readiness",
	} {
		if rec := do(t, srv, http.MethodGet, target, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s returned %d, want 503", target, rec.Code)
		}
	}

	if rec := postJSON(t, srv, "/api/v1/snapshots", `{"containerId":"c1"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST returned %d, want 503", rec.Code)
	}
}

func TestSnapshotListInternalErrorIsOpaque(t *testing.T) {
	srv := newSnapshotServer(t)
	srv.snapshots.listErr = errors.New("database is locked: /data/harbormaster.db")

	rec := do(t, srv.Server, http.MethodGet, "/api/v1/snapshots", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The internal error names a filesystem path and must not cross the
	// boundary.
	if strings.Contains(rec.Body.String(), "harbormaster.db") {
		t.Errorf("the 500 leaked internal detail: %s", rec.Body)
	}
}
