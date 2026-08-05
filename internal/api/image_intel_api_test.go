package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Image intelligence API tests.
//
// The property worth defending above all others: NO REQUEST A CALLER CAN
// COMPOSE BECOMES A NETWORK DESTINATION. The refresh endpoint takes no target,
// the filters only narrow a database query, and there is no parameter anywhere
// that carries a host, a URL, or a scheme.

// ------------------------------------------------------------------ fakes --

type fakeImageIntel struct {
	mu sync.Mutex

	records []domain.ImageIntel
	events  []domain.ImageUpdateEvent
	summary domain.ImageIntelSummary

	lastFilter store.ImageIntelFilter
	lastPage   store.Page
	listErr    error
}

func (f *fakeImageIntel) List(
	_ context.Context, filter store.ImageIntelFilter,
) ([]domain.ImageIntel, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.records, len(f.records), nil
}

func (f *fakeImageIntel) Get(_ context.Context, reference string) (domain.ImageIntel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.records {
		if record.Reference == reference {
			return record, nil
		}
	}
	return domain.ImageIntel{}, store.ErrNotFound
}

func (f *fakeImageIntel) ForImageID(_ context.Context, _ string) ([]domain.ImageIntel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.records, nil
}

func (f *fakeImageIntel) History(
	_ context.Context, _ string, page store.Page,
) ([]domain.ImageUpdateEvent, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPage = page
	return f.events, len(f.events), nil
}

func (f *fakeImageIntel) Summary(context.Context) (domain.ImageIntelSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, nil
}

func (f *fakeImageIntel) filter() store.ImageIntelFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

// fakeCollector records collection requests without doing any work.
type fakeCollector struct {
	mu       sync.Mutex
	requests int
}

func (f *fakeCollector) RequestCollection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
}

func (f *fakeCollector) Status(context.Context) domain.ImageIntelEngineStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.ImageIntelEngineStatus{Enabled: true, DueNow: 3}
}

func (f *fakeCollector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// -------------------------------------------------------------- harnesses --

func sampleIntel() domain.ImageIntel {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return domain.ImageIntel{
		ID:             1,
		Reference:      "docker.io/library/nginx:1.25",
		Familiar:       "nginx:1.25",
		Kind:           domain.RegistryDockerHub,
		Registry:       "docker.io",
		Namespace:      "library",
		Repository:     "library/nginx",
		Tag:            "1.25",
		LocalDigest:    "sha256:" + strings.Repeat("a", 64),
		RemoteDigest:   "sha256:" + strings.Repeat("b", 64),
		Platform:       domain.Platform{OS: "linux", Architecture: "amd64"},
		ImageID:        "sha256:" + strings.Repeat("1", 64),
		ContainerCount: 3,
		Update:         domain.UpdateMinor,
		LatestTag:      "1.26",
		UpdateReason:   "a newer tag is published in the same series",
		Status:         domain.CheckOK,
		FirstSeenAt:    at,
		LastCheckedAt:  &at,
		// Deliberately populated: it must NOT appear in the JSON.
		ETag: `"cached-validator"`,
	}
}

func newImageServer(t *testing.T, intel *fakeImageIntel, collector *fakeCollector) *Server {
	t.Helper()
	return NewServer(Options{
		Health: &fakeHealth{},
		Images: &fakeImages{usages: []store.ImageUsage{{
			Image:          domain.Image{ID: "sha256:" + strings.Repeat("1", 64)},
			ContainerCount: 3,
		}}, total: 1},
		ImageIntel:     intel,
		ImageCollector: collector,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})
}

// ----------------------------------------------------------------- reads --

func TestImageUpdatesListsAndSummarises(t *testing.T) {
	intel := &fakeImageIntel{
		records: []domain.ImageIntel{sampleIntel()},
		summary: domain.ImageIntelSummary{
			Images: 10, Checked: 8, Pending: 2, UpdatesAvailable: 3,
			Containers: 20, ContainersAffected: 5,
			ByUpdate: map[domain.UpdateType]int{domain.UpdateMinor: 3},
		},
	}
	srv := newImageServer(t, intel, &fakeCollector{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/images/updates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var response imageUpdatesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Familiar != "nginx:1.25" {
		t.Errorf("items = %+v", response.Items)
	}
	// The summary travels WITH the list so a dashboard renders in one request,
	// and so coverage is always beside the update count.
	if response.Summary.Checked != 8 || response.Summary.Pending != 2 {
		t.Errorf("summary = %+v", response.Summary)
	}
}

// The cached ETag is a registry-supplied cache detail with no meaning to a
// client. It must not be serialised.
func TestTheCachedValidatorIsNeverSerialised(t *testing.T) {
	intel := &fakeImageIntel{records: []domain.ImageIntel{sampleIntel()}}
	srv := newImageServer(t, intel, &fakeCollector{})

	for _, target := range []string{
		APIPrefix + "/images/updates",
		APIPrefix + "/images/sha256:" + strings.Repeat("1", 64),
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "cached-validator") || strings.Contains(body, `"etag"`) {
			t.Errorf("%s leaked the cache validator: %s", target, body)
		}
	}
}

// The image detail response is a strict SUPERSET of the Phase 2 one: an
// endpoint that already had consumers is extended, not replaced.
func TestImageDetailRemainsBackwardCompatible(t *testing.T) {
	intel := &fakeImageIntel{records: []domain.ImageIntel{sampleIntel()}}
	srv := newImageServer(t, intel, &fakeCollector{})

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/images/sha256:"+strings.Repeat("1", 64), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The Phase 2 fields, in the same places.
	for _, key := range []string{"image", "containerCount"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the response dropped the pre-existing %q field", key)
		}
	}
	// And the new one.
	if _, ok := raw["intel"]; !ok {
		t.Error("the response carries no intel section")
	}
}

func TestImageHistory(t *testing.T) {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	intel := &fakeImageIntel{
		records: []domain.ImageIntel{sampleIntel()},
		events: []domain.ImageUpdateEvent{{
			ID: 1, Reference: "docker.io/library/nginx:1.25",
			ObservedAt: at, Kind: domain.ImageEventDigestChanged,
			Status: domain.CheckOK, Detail: "the registry now serves a different digest",
		}},
	}
	srv := newImageServer(t, intel, &fakeCollector{})

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/images/sha256:"+strings.Repeat("1", 64)+"/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response imageHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Kind != domain.ImageEventDigestChanged {
		t.Errorf("items = %+v", response.Items)
	}
	// The merged history is attributable to the references that produced it.
	if len(response.References) != 1 {
		t.Errorf("references = %v", response.References)
	}
}

// -------------------------------------------------------------- filtering --

func TestImageFiltersReachTheRepository(t *testing.T) {
	intel := &fakeImageIntel{}
	srv := newImageServer(t, intel, &fakeCollector{})

	target := APIPrefix + "/images/updates?update=major,minor&status=ok" +
		"&registry=docker.io&updatesOnly=true&inUseOnly=true" +
		"&sort=lastChecked&order=asc&page=1&pageSize=10"
	rec := do(t, srv, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := intel.filter()
	if len(filter.Updates) != 2 || len(filter.Statuses) != 1 {
		t.Errorf("vocabularies = %+v / %+v", filter.Updates, filter.Statuses)
	}
	if len(filter.Registries) != 1 || filter.Registries[0] != "docker.io" {
		t.Errorf("registries = %v", filter.Registries)
	}
	if !filter.UpdatesOnly || !filter.InUseOnly {
		t.Errorf("flags = %v / %v", filter.UpdatesOnly, filter.InUseOnly)
	}
	if filter.Sort != "lastChecked" || !filter.Ascending {
		t.Errorf("sort = %q asc=%v", filter.Sort, filter.Ascending)
	}
}

func TestImageQueryRejections(t *testing.T) {
	srv := newImageServer(t, &fakeImageIntel{}, &fakeCollector{})

	for _, target := range []string{
		APIPrefix + "/images/updates?sort=update_type",
		APIPrefix + "/images/updates?sort=" + url.QueryEscape("reference; DROP TABLE image_intel"),
		APIPrefix + "/images/updates?order=sideways",
		APIPrefix + "/images/updates?update=catastrophic",
		APIPrefix + "/images/updates?status=exploded",
		APIPrefix + "/images/updates?updatesOnly=perhaps",
		APIPrefix + "/images/updates?inUseOnly=perhaps",
		APIPrefix + "/images/updates?search=" + url.QueryEscape(strings.Repeat("a", 600)),
		// A registry filter is validated by SHAPE. None of these could reach a
		// query, and none of them could ever become a destination either.
		APIPrefix + "/images/updates?registry=" + url.QueryEscape("http://evil.example.com"),
		APIPrefix + "/images/updates?registry=" + url.QueryEscape("evil.example.com/path"),
		APIPrefix + "/images/updates?registry=" + url.QueryEscape("' OR 1=1 --"),
		APIPrefix + "/images/updates?" + strings.Repeat("registry=a.example.com&", 40),
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "DROP TABLE") ||
			strings.Contains(rec.Body.String(), "evil.example.com") {
			t.Errorf("%s echoed the caller's input: %s", target, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------- refresh --

// The refresh endpoint SCHEDULES. It must not run work inline, must take no
// target, and must be guarded like every other write.
func TestImageRefreshSchedulesAndTakesNoTarget(t *testing.T) {
	collector := &fakeCollector{}
	srv := newImageServer(t, &fakeImageIntel{}, collector)

	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/images/refresh", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	var response imageRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.Requested || !response.Engine.Enabled {
		t.Errorf("response = %+v", response)
	}
	if collector.count() != 1 {
		t.Errorf("collections requested = %d, want 1", collector.count())
	}
}

// THE SSRF ASSERTION AT THE API BOUNDARY. Whatever a caller attaches to the
// refresh request, no destination reaches the engine -- because the engine's
// interface has nowhere to put one.
func TestNoRefreshParameterCanSupplyADestination(t *testing.T) {
	collector := &fakeCollector{}
	srv := newImageServer(t, &fakeImageIntel{}, collector)

	for _, target := range []string{
		APIPrefix + "/images/refresh?registry=http://169.254.169.254/",
		APIPrefix + "/images/refresh?host=169.254.169.254",
		APIPrefix + "/images/refresh?url=" + url.QueryEscape("http://127.0.0.1:2375/containers/json"),
		APIPrefix + "/images/refresh?reference=" + url.QueryEscape("127.0.0.1:5000/x:1"),
	} {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		// Accepted and ignored: the parameters are not read at all, which is a
		// stronger property than rejecting them. There is no field on
		// ImageIntelCollector that could carry one.
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s returned %d, want 202", target, rec.Code)
		}
	}

	// Every request scheduled a pass over the EXISTING inventory and nothing
	// else. The interface is the guarantee: RequestCollection takes no
	// arguments.
	if collector.count() != 4 {
		t.Errorf("collections = %d, want one per request", collector.count())
	}
}

// The refresh is a write, so it gets the write guard.
func TestImageRefreshIsGuarded(t *testing.T) {
	collector := &fakeCollector{}
	srv := newImageServer(t, &fakeImageIntel{}, collector)

	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/images/refresh", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site refresh returned %d, want 403", rec.Code)
	}
	if collector.count() != 0 {
		t.Error("a cross-site request scheduled a collection")
	}
}

// ---------------------------------------------------------------- methods --

func TestImageMethodsAreConstrained(t *testing.T) {
	srv := newImageServer(t, &fakeImageIntel{}, &fakeCollector{})

	cases := []struct {
		method string
		target string
	}{
		{http.MethodDelete, APIPrefix + "/images"},
		{http.MethodPost, APIPrefix + "/images/updates"},
		{http.MethodPut, APIPrefix + "/images/sha256:abc"},
		{http.MethodDelete, APIPrefix + "/images/sha256:abc"},
		{http.MethodPost, APIPrefix + "/images/sha256:abc/history"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s returned %d, want 405", tc.method, tc.target, rec.Code)
		}
	}
}

// GET /images/refresh is not a refresh.
//
// It resolves to the image-detail route with an id of "refresh", which answers
// 404 in production. A bare "/images/refresh" companion would give a tidier 405,
// but it neither contains nor is contained by "GET /images/{id}", so ServeMux
// would panic on the pair at startup -- the same constraint that shapes
// /events/stream and /drift/summary.
//
// What actually matters is that it does not SCHEDULE anything, which is what
// this asserts.
func TestAGetToTheRefreshPathSchedulesNothing(t *testing.T) {
	collector := &fakeCollector{}
	srv := newImageServer(t, &fakeImageIntel{}, collector)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/images/refresh", nil)

	if rec.Code == http.StatusAccepted {
		t.Error("a GET was treated as a refresh request")
	}
	if collector.count() != 0 {
		t.Errorf("collections = %d, want 0 for a GET", collector.count())
	}
}

// There must be no route that pulls, deletes, or prunes an image. Asserted
// directly rather than trusting that nobody added one.
func TestThereIsNoImageMutationRoute(t *testing.T) {
	srv := newImageServer(t, &fakeImageIntel{}, &fakeCollector{})

	for _, target := range []string{
		APIPrefix + "/images/pull",
		APIPrefix + "/images/prune",
		APIPrefix + "/images/sha256:abc/pull",
		APIPrefix + "/images/sha256:abc/update",
		APIPrefix + "/images/sha256:abc/delete",
		APIPrefix + "/images/sha256:abc/apply",
	} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d; there must be no image mutation route", target, rec.Code)
		}
	}
}

// -------------------------------------------------------------- disabled --

// A deployment with the engine off yields a 503 rather than a broken route or a
// misleading empty list. The image endpoints that predate this phase keep
// working.
func TestImageIntelEndpointsReportWhenDisabled(t *testing.T) {
	srv := NewServer(Options{
		Health: &fakeHealth{},
		Images: &fakeImages{usages: []store.ImageUsage{{
			Image: domain.Image{ID: "sha256:abc"},
		}}, total: 1},
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 1000, WriteRateBurst: 1000},
		Assets:         testAssets(),
	})

	for _, target := range []string{
		APIPrefix + "/images/updates",
		APIPrefix + "/images/sha256:abc/history",
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s returned %d, want 503", target, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/images/refresh", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /images/refresh returned %d, want 503", rec.Code)
	}

	// The pre-existing endpoint still works, without an empty intel section
	// implying the registry said nothing.
	detail := do(t, srv, http.MethodGet, APIPrefix+"/images/sha256:abc", nil)
	if detail.Code != http.StatusOK {
		t.Errorf("GET /images/{id} returned %d with intel disabled, want 200", detail.Code)
	}
}

// A repository failure must not reach the client.
func TestImageRepositoryFailureIsNotLeaked(t *testing.T) {
	intel := &fakeImageIntel{listErr: fmt.Errorf("query image_intel: no such table: image_intel")}
	srv := newImageServer(t, intel, &fakeCollector{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/images/updates", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "image_intel") ||
		strings.Contains(rec.Body.String(), "no such table") {
		t.Errorf("the internal error leaked: %s", rec.Body.String())
	}
}
