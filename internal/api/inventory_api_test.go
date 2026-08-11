package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// ------------------------------------------------------------- test doubles --

type fakeInventory struct {
	enabled     bool
	status      domain.InventoryStatus
	statusErr   error
	accept      bool
	activeSince time.Time
	runtimeErr  error
	triggers    []domain.RefreshTrigger
}

func (f *fakeInventory) Enabled() bool { return f.enabled }

func (f *fakeInventory) Status(context.Context) (domain.InventoryStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeInventory) TriggerAsync(trigger domain.RefreshTrigger) (bool, time.Time) {
	f.triggers = append(f.triggers, trigger)
	return f.accept, f.activeSince
}

func (f *fakeInventory) CheckRuntime(context.Context) error { return f.runtimeErr }

type fakeContainers struct {
	summaries  []domain.ContainerSummary
	total      int
	detail     *domain.ContainerDetail
	raw        []byte
	resolveTo  string
	resolveErr error
	getErr     error
	listErr    error
	lastFilter store.ContainerFilter

	// What the handler asked about, and what to answer. Recorded because the
	// property under test is that ONE lookup covers the whole page.
	evidence          map[string]domain.ContainerEvidence
	attentionCalls    int
	attentionKeyCount int
	attentionErr      error
}

func (f *fakeContainers) List(_ context.Context, filter store.ContainerFilter) ([]domain.ContainerSummary, int, error) {
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.summaries, f.total, nil
}

func (f *fakeContainers) Attention(
	_ context.Context, keys []store.ContainerKey,
) (map[string]domain.ContainerEvidence, error) {
	f.attentionCalls++
	f.attentionKeyCount += len(keys)
	if f.attentionErr != nil {
		return nil, f.attentionErr
	}
	if f.evidence != nil {
		return f.evidence, nil
	}
	return map[string]domain.ContainerEvidence{}, nil
}

func (f *fakeContainers) Get(context.Context, string) (*domain.ContainerDetail, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.detail, nil
}

func (f *fakeContainers) ResolveID(_ context.Context, reference string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if f.resolveTo != "" {
		return f.resolveTo, nil
	}
	return reference, nil
}

func (f *fakeContainers) RawInspection(context.Context, string) ([]byte, error) {
	if f.raw == nil {
		return nil, store.ErrNotFound
	}
	return f.raw, nil
}

func (f *fakeContainers) DistinctComposeProjects(context.Context) ([]string, error) {
	return []string{"shop", "blog"}, nil
}

func (f *fakeContainers) DistinctImages(context.Context) ([]string, error) {
	return []string{"nginx", "redis"}, nil
}

type fakeImages struct {
	usages []store.ImageUsage
	total  int
	getErr error
}

func (f *fakeImages) List(context.Context, store.Page) ([]store.ImageUsage, int, error) {
	return f.usages, f.total, nil
}

func (f *fakeImages) Get(context.Context, string) (*store.ImageUsage, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &f.usages[0], nil
}

// --------------------------------------------------------------- helpers --

func sampleSummary(id, name string) domain.ContainerSummary {
	return domain.ContainerSummary{
		HostID: domain.LocalHostID, ID: id, ShortID: domain.ShortenID(id), Name: name,
		Image: domain.ParseImageRef("nginx:1.27"), State: domain.StateRunning,
		Health: domain.HealthHealthy, Present: true, Ports: []domain.Port{},
	}
}

func inventoryServer(t *testing.T, opts Options) *Server {
	t.Helper()

	if opts.Health == nil {
		opts.Health = &fakeHealth{}
	}
	opts.Logger = discardLogger()
	opts.Config = config.Server{MaxRequestBytes: 1 << 20}
	return newAuthedServer(opts)
}

func decodeBody[T any](t *testing.T, body []byte) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return decoded
}

// -------------------------------------------------------------- inventory --

func TestInventoryEndpointReturnsStatus(t *testing.T) {
	inventory := &fakeInventory{
		enabled: true,
		status: domain.InventoryStatus{
			Enabled: true, Runtime: domain.RuntimeDocker,
			Docker:     domain.Component{Status: domain.StatusUp, Version: "1.51"},
			State:      domain.RefreshSucceeded,
			Generation: 7, Checksum: "abc123",
			Counts:   domain.InventoryCounts{Containers: 12, Running: 9, Stopped: 3},
			Warnings: []domain.InventoryWarning{},
		},
	}
	srv := inventoryServer(t, Options{Inventory: inventory})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/inventory", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeBody[domain.InventoryStatus](t, rec.Body.Bytes())
	if got.Generation != 7 || got.Checksum != "abc123" {
		t.Errorf("generation/checksum = %d/%q", got.Generation, got.Checksum)
	}
	if got.Counts.Containers != 12 {
		t.Errorf("counts = %+v", got.Counts)
	}
	if got.Docker.Status != domain.StatusUp {
		t.Errorf("docker = %+v", got.Docker)
	}
}

func TestInventoryEndpointWithoutServiceReturns503(t *testing.T) {
	srv := inventoryServer(t, Options{})

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/inventory", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// The manual refresh is asynchronous, so 202 rather than 200: the work is
// accepted, not finished.
func TestRefreshTriggerReturns202(t *testing.T) {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	inventory := &fakeInventory{enabled: true, accept: true, activeSince: started}
	srv := inventoryServer(t, Options{Inventory: inventory})

	rec := do(t, srv, http.MethodPost, APIPrefix+"/inventory/refresh", nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	got := decodeBody[refreshAccepted](t, rec.Body.Bytes())
	if !got.Accepted || got.Trigger != string(domain.TriggerManual) {
		t.Errorf("body = %+v", got)
	}
	if len(inventory.triggers) != 1 || inventory.triggers[0] != domain.TriggerManual {
		t.Errorf("triggers = %v", inventory.triggers)
	}
}

// An already-running refresh is a deterministic 409 that also reports the
// active refresh, so the caller learns something actionable.
func TestRefreshWhileRunningReturns409WithActiveStatus(t *testing.T) {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	inventory := &fakeInventory{enabled: true, accept: false, activeSince: started}
	srv := inventoryServer(t, Options{Inventory: inventory})

	rec := do(t, srv, http.MethodPost, APIPrefix+"/inventory/refresh", nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	got := decodeBody[refreshConflict](t, rec.Body.Bytes())
	if got.Error.Code != CodeConflict {
		t.Errorf("code = %q", got.Error.Code)
	}
	if !got.Active.InProgress || !got.Active.StartedAt.Equal(started) {
		t.Errorf("active = %+v", got.Active)
	}
}

func TestRefreshWithUnreachableDockerReturns503(t *testing.T) {
	inventory := &fakeInventory{enabled: true, accept: true, runtimeErr: errors.New("unreachable")}
	srv := inventoryServer(t, Options{Inventory: inventory})

	rec := do(t, srv, http.MethodPost, APIPrefix+"/inventory/refresh", nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	got := decodeBody[ErrorResponse](t, rec.Body.Bytes())
	if got.Error.Code != CodeUnavailable {
		t.Errorf("code = %q", got.Error.Code)
	}
	// No refresh should have been started.
	if len(inventory.triggers) != 0 {
		t.Error("a refresh was triggered despite an unreachable runtime")
	}
}

func TestRefreshWhenDisabledReturns503(t *testing.T) {
	srv := inventoryServer(t, Options{Inventory: &fakeInventory{enabled: false}})

	rec := do(t, srv, http.MethodPost, APIPrefix+"/inventory/refresh", nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// GET on the refresh endpoint is a 405, not a silent refresh: read methods
// must never trigger work.
func TestRefreshEndpointRejectsGet(t *testing.T) {
	srv := inventoryServer(t, Options{Inventory: &fakeInventory{enabled: true, accept: true}})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/inventory/refresh", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// -------------------------------------------------------------- containers --

func TestContainerListPagination(t *testing.T) {
	containers := &fakeContainers{
		summaries: []domain.ContainerSummary{sampleSummary("c1", "alpha"), sampleSummary("c2", "bravo")},
		total:     57,
	}
	srv := inventoryServer(t, Options{Containers: containers})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers?page=2&pageSize=25", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeBody[listResponse[domain.ContainerSummary]](t, rec.Body.Bytes())

	if got.Pagination.Page != 2 || got.Pagination.PageSize != 25 {
		t.Errorf("pagination = %+v", got.Pagination)
	}
	if got.Pagination.TotalItems != 57 || got.Pagination.TotalPages != 3 {
		t.Errorf("totals = %+v", got.Pagination)
	}
	if !got.Pagination.HasNext || !got.Pagination.HasPrev {
		t.Errorf("navigation flags = %+v", got.Pagination)
	}
	// Offset must be derived correctly, or page 2 would repeat page 1.
	if containers.lastFilter.Page.Offset != 25 {
		t.Errorf("offset = %d, want 25", containers.lastFilter.Page.Offset)
	}
}

func TestContainerListRejectsInvalidPagination(t *testing.T) {
	srv := inventoryServer(t, Options{Containers: &fakeContainers{}})

	tests := []string{
		"?page=0", "?page=-1", "?page=abc",
		"?pageSize=0", "?pageSize=-5", "?pageSize=nonsense",
		fmt.Sprintf("?pageSize=%d", MaxPageSize+1),
	}

	for _, query := range tests {
		t.Run(query, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, APIPrefix+"/containers"+query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			got := decodeBody[ErrorResponse](t, rec.Body.Bytes())
			if got.Error.Code != CodeInvalidRequest {
				t.Errorf("code = %q", got.Error.Code)
			}
		})
	}
}

func TestContainerListFiltersReachTheRepository(t *testing.T) {
	containers := &fakeContainers{}
	srv := inventoryServer(t, Options{Containers: containers})

	query := url.Values{}
	query.Set("search", "web")
	query.Set("state", "running,paused")
	query.Set("health", "unhealthy")
	query.Set("project", "shop")
	query.Set("service", "api")
	query.Set("image", "nginx")
	query.Set("restartPolicy", "always")
	query.Set("labelKey", "tier")
	query.Set("labelValue", "front")
	query.Set("harbormasterEnabled", "true")
	query.Set("sort", "state")
	query.Set("direction", "desc")

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers?"+query.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := containers.lastFilter
	if filter.Search != "web" || filter.ComposeProject != "shop" || filter.ComposeService != "api" {
		t.Errorf("filter = %+v", filter)
	}
	if len(filter.States) != 2 {
		t.Errorf("states = %v (comma-separated values should expand)", filter.States)
	}
	if len(filter.Health) != 1 || filter.Health[0] != domain.HealthUnhealthy {
		t.Errorf("health = %v", filter.Health)
	}
	if filter.LabelKey != "tier" || filter.LabelValue != "front" {
		t.Errorf("labels = %q/%q", filter.LabelKey, filter.LabelValue)
	}
	if filter.HarborMasterEnabled == nil || !*filter.HarborMasterEnabled {
		t.Errorf("harbormasterEnabled = %v", filter.HarborMasterEnabled)
	}
	if filter.Sort != "state" || filter.Direction != store.SortDesc {
		t.Errorf("sort = %q %q", filter.Sort, filter.Direction)
	}
}

func TestContainerListRejectsInvalidFilters(t *testing.T) {
	srv := inventoryServer(t, Options{Containers: &fakeContainers{}})

	tests := map[string]string{
		"unknown state":     "?state=teleporting",
		"unknown health":    "?health=greenish",
		"unknown sort":      "?sort=hostname",
		"qualified column":  "?sort=containers.name",
		"quoted sort":       "?sort=name%27--",
		"union attempt":     "?sort=name%20UNION%20SELECT%201",
		"bad direction":     "?direction=sideways",
		"bad bool":          "?harbormasterEnabled=perhaps",
		"value without key": "?labelValue=front",
	}

	for name, query := range tests {
		t.Run(name, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, APIPrefix+"/containers"+query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// A query containing a semicolon is discarded wholesale by Go's parser, so the
// parameter never reaches the handler and the default ordering applies. Worth
// pinning: it means such a request is safe but returns 200, not 400.
func TestSemicolonBearingQueryIsDroppedNotHonoured(t *testing.T) {
	containers := &fakeContainers{}
	srv := inventoryServer(t, Options{Containers: containers})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers?sort=name;DROP+TABLE+containers", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if containers.lastFilter.Sort != "name" {
		t.Errorf("sort = %q, want the default; the malformed parameter must not survive",
			containers.lastFilter.Sort)
	}
}

// A rejected sort field must not be echoed back; it is caller-controlled text.
func TestInvalidFilterErrorsDoNotEchoInput(t *testing.T) {
	srv := inventoryServer(t, Options{Containers: &fakeContainers{}})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers?state=%3Cscript%3Ealert(1)%3C/script%3E", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "script") {
		t.Errorf("the error echoed caller input: %s", rec.Body.String())
	}
}

func TestContainerDetailReturnsSections(t *testing.T) {
	detail := &domain.ContainerDetail{
		Overview: sampleSummary("c1", "web"),
		Environment: []domain.EnvVar{
			{Name: "DB_PASSWORD", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2"},
			{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
		},
		Labels:   []domain.Label{{Key: "app", Value: "web", Source: domain.LabelSourceUser}},
		Mounts:   []domain.Mount{{Destination: "/data", Type: domain.MountTypeVolume}},
		Networks: []domain.NetworkAttachment{{NetworkName: "bridge"}},
		Warnings: []domain.InventoryWarning{},
	}
	srv := inventoryServer(t, Options{Containers: &fakeContainers{detail: detail}})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers/c1", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// Every documented section must be present, so the UI can rely on shape.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, section := range []string{
		"overview", "state", "process", "environment", "labels", "ports",
		"mounts", "networks", "resources", "security", "logging", "compose",
		"harbormaster", "warnings",
	} {
		if _, ok := raw[section]; !ok {
			t.Errorf("section %q missing from the detail response", section)
		}
	}
}

// The central guarantee of the masking design, asserted at the boundary that
// actually matters: the wire.
func TestContainerDetailMasksSensitiveEnvironmentValues(t *testing.T) {
	detail := &domain.ContainerDetail{
		Overview: sampleSummary("c1", "web"),
		Environment: []domain.EnvVar{
			{Name: "DB_PASSWORD", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2"},
			{Name: "API_TOKEN", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "tok_live_123"},
			{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
		},
		Logging: domain.Logging{
			Driver: "splunk",
			Options: []domain.EnvVar{
				{Name: "splunk-token", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "secret-token"},
			},
		},
	}
	srv := inventoryServer(t, Options{Containers: &fakeContainers{detail: detail}})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers/c1", nil)
	body := rec.Body.String()

	for _, secret := range []string{"hunter2", "tok_live_123", "secret-token"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaked the secret %q", secret)
		}
	}
	// Names and non-sensitive values still come through.
	for _, expected := range []string{"DB_PASSWORD", "API_TOKEN", "8080", domain.MaskedValue} {
		if !strings.Contains(body, expected) {
			t.Errorf("response is missing %q", expected)
		}
	}
}

// There must be no way to ask the API for the real values.
func TestNoQueryParameterRevealsRawEnvironmentValues(t *testing.T) {
	detail := &domain.ContainerDetail{
		Overview: sampleSummary("c1", "web"),
		Environment: []domain.EnvVar{
			{Name: "DB_PASSWORD", Value: domain.MaskedValue, Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2"},
		},
	}
	srv := inventoryServer(t, Options{Containers: &fakeContainers{detail: detail}})

	for _, query := range []string{
		"?reveal=true", "?unmask=1", "?raw=true", "?showSecrets=true", "?includeSensitive=true",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/containers/c1"+query, nil)
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatalf("%s revealed a raw secret value", query)
		}
	}
}

func TestContainerNotFoundReturns404(t *testing.T) {
	srv := inventoryServer(t, Options{
		Containers: &fakeContainers{resolveErr: store.ErrNotFound},
	})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers/missing", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	got := decodeBody[ErrorResponse](t, rec.Body.Bytes())
	if got.Error.Code != CodeNotFound {
		t.Errorf("code = %q", got.Error.Code)
	}
}

// An ambiguous prefix is a conflict, not a not-found: the resource exists more
// than once, and guessing would be worse than refusing.
func TestAmbiguousContainerPrefixReturns409(t *testing.T) {
	srv := inventoryServer(t, Options{
		Containers: &fakeContainers{resolveErr: store.ErrAmbiguousID},
	})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers/abc", nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	got := decodeBody[ErrorResponse](t, rec.Body.Bytes())
	if got.Error.Code != CodeAmbiguousID {
		t.Errorf("code = %q", got.Error.Code)
	}
	if !strings.Contains(got.Error.Message, "more characters") {
		t.Errorf("message should say how to fix it: %q", got.Error.Message)
	}
}

// ------------------------------------------------------------------- raw --

func TestRawInspectionIsASeparateEndpoint(t *testing.T) {
	containers := &fakeContainers{
		detail: &domain.ContainerDetail{Overview: sampleSummary("c1", "web")},
		raw:    []byte(`{"Id":"c1","Config":{"Env":["DB_PASSWORD=********"]}}`),
	}
	srv := inventoryServer(t, Options{Containers: containers})

	// Not in the default detail response...
	detailRec := do(t, srv, http.MethodGet, APIPrefix+"/containers/c1", nil)
	if strings.Contains(detailRec.Body.String(), "inspection") {
		t.Error("raw inspection data leaked into the default container response")
	}

	// ...but available from its own endpoint, clearly labelled.
	rawRec := do(t, srv, http.MethodGet, APIPrefix+"/containers/c1/raw", nil)
	if rawRec.Code != http.StatusOK {
		t.Fatalf("status = %d", rawRec.Code)
	}

	body := decodeBody[map[string]any](t, rawRec.Body.Bytes())
	if body["redacted"] != true {
		t.Error("the raw response must declare that it is redacted")
	}
	notice, _ := body["notice"].(string)
	if !strings.Contains(notice, "cannot be used to recreate") {
		t.Errorf("notice should not claim restoration fidelity: %q", notice)
	}
	if body["inspection"] == nil {
		t.Error("inspection payload missing")
	}
}

func TestRawInspectionMissingReturns404(t *testing.T) {
	srv := inventoryServer(t, Options{
		Containers: &fakeContainers{detail: &domain.ContainerDetail{}},
	})

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/containers/c1/raw", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------- images --

func TestImageEndpoints(t *testing.T) {
	images := &fakeImages{
		usages: []store.ImageUsage{{
			Image: domain.Image{
				ID: "sha256:abc", ShortID: "abc", RepoTags: []string{"nginx:1.27"},
				Size: 1024, Architecture: "amd64", OS: "linux",
			},
			ContainerCount: 3,
		}},
		total: 1,
	}
	srv := inventoryServer(t, Options{Images: images})

	listRec := do(t, srv, http.MethodGet, APIPrefix+"/images", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d", listRec.Code)
	}
	list := decodeBody[listResponse[store.ImageUsage]](t, listRec.Body.Bytes())
	if len(list.Items) != 1 || list.Items[0].ContainerCount != 3 {
		t.Errorf("items = %+v", list.Items)
	}

	detailRec := do(t, srv, http.MethodGet, APIPrefix+"/images/sha256:abc", nil)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status = %d", detailRec.Code)
	}

	images.getErr = store.ErrNotFound
	if rec := do(t, srv, http.MethodGet, APIPrefix+"/images/missing", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing image status = %d, want 404", rec.Code)
	}
}

// ------------------------------------------------------------- read-only --

// The whole surface must refuse writes. Only the refresh endpoint accepts a
// POST, and it changes HarborMaster's records, never Docker.
func TestEveryInventoryEndpointRejectsWrites(t *testing.T) {
	srv := inventoryServer(t, Options{
		Inventory:  &fakeInventory{enabled: true, accept: true},
		Containers: &fakeContainers{detail: &domain.ContainerDetail{}},
		Images:     &fakeImages{usages: []store.ImageUsage{{}}, total: 1},
		Networks:   nil,
		Volumes:    nil,
	})

	paths := []string{
		APIPrefix + "/inventory",
		APIPrefix + "/inventory/filters",
		APIPrefix + "/containers",
		APIPrefix + "/containers/c1",
		APIPrefix + "/containers/c1/raw",
		APIPrefix + "/images",
		APIPrefix + "/images/sha256:abc",
		APIPrefix + "/networks",
		APIPrefix + "/volumes",
	}

	for _, path := range paths {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			rec := do(t, srv, method, path, strings.NewReader("{}"))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
			}
		}
	}
}

func TestContainerFiltersEndpoint(t *testing.T) {
	srv := inventoryServer(t, Options{Containers: &fakeContainers{}})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/inventory/filters", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeBody[map[string][]string](t, rec.Body.Bytes())

	if len(body["states"]) != len(domain.ContainerStates) {
		t.Errorf("states = %v", body["states"])
	}
	if len(body["projects"]) != 2 {
		t.Errorf("projects = %v", body["projects"])
	}
	if len(body["sortFields"]) == 0 {
		t.Error("sortFields should be advertised so a client can build a selector")
	}
}

// A repository failure is a generic 500; it must not surface SQL or paths.
func TestRepositoryFailuresReturnGeneric500(t *testing.T) {
	srv := inventoryServer(t, Options{
		Containers: &fakeContainers{
			listErr: errors.New("SQL logic error near \"SELECT\": /srv/secret/hm.db"),
		},
	})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"SQL", "SELECT", "/srv/secret"} {
		if strings.Contains(body, leak) {
			t.Errorf("500 response leaked %q: %s", leak, body)
		}
	}
}
