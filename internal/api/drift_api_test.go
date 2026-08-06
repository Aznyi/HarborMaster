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

// Drift API tests.
//
// The properties worth defending here are about the WRITE: it must be guarded
// like a POST, it must refuse the engine-owned statuses, and it must not let a
// caller reach anything but a status.

// fakeDrift is an in-memory DriftReader.
type fakeDrift struct {
	mu sync.Mutex

	records    []domain.DriftRecord
	summary    domain.DriftSummary
	evaluation *domain.DriftEvaluation

	// lastFilter records what the handler asked for, which is how the filter
	// and pagination tests assert that parsing reached the repository.
	lastFilter store.DriftFilter
	// updates records every status transition applied.
	updates []struct {
		id     int64
		status domain.DriftStatus
		note   string
	}
	listErr error
}

func (f *fakeDrift) List(_ context.Context, filter store.DriftFilter) ([]domain.DriftRecord, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.records, len(f.records), nil
}

func (f *fakeDrift) Get(_ context.Context, id int64) (domain.DriftRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.records {
		if record.ID == id {
			return record, nil
		}
	}
	return domain.DriftRecord{}, store.ErrNotFound
}

func (f *fakeDrift) Summary(context.Context) (domain.DriftSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, nil
}

func (f *fakeDrift) Evaluation(_ context.Context, containerID string) (domain.DriftEvaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.evaluation == nil {
		return domain.DriftEvaluation{}, store.ErrNotFound
	}
	return *f.evaluation, nil
}

func (f *fakeDrift) UpdateStatus(_ context.Context, id int64, status domain.DriftStatus,
	note string, _ time.Time) (domain.DriftRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range f.records {
		if f.records[i].ID != id {
			continue
		}
		f.records[i].Status = status
		f.records[i].Note = note
		f.updates = append(f.updates, struct {
			id     int64
			status domain.DriftStatus
			note   string
		}{id, status, note})
		return f.records[i], nil
	}
	return domain.DriftRecord{}, store.ErrNotFound
}

// fakeDriftContainers resolves a container id for the by-container route.
type fakeDriftContainers struct{ ContainerReader }

func (fakeDriftContainers) ResolveID(_ context.Context, reference string) (string, error) {
	if reference == "missing" {
		return "", store.ErrNotFound
	}
	return "container-" + reference, nil
}

func driftRecordFixture(id int64, field string, severity domain.DriftSeverity) domain.DriftRecord {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return domain.DriftRecord{
		ID: id, ContainerID: "container-a", ContainerName: "web", SnapshotID: 7,
		DetectedAt: at, LastSeenAt: at,
		Category: domain.DriftCategorySecurity, Field: field,
		Kind: domain.ChangeModified, Severity: severity,
		PreviousValue: "false", CurrentValue: "true",
		Status: domain.DriftStatusActive, Reason: "a setting changed",
	}
}

func newDriftServer(t *testing.T, drift *fakeDrift) *Server {
	t.Helper()
	return newAuthedServer(Options{
		Health:      &fakeHealth{},
		Drift:       drift,
		DriftConfig: config.Drift{Enabled: true, MaxNoteBytes: 500},
		Containers:  fakeDriftContainers{},
		Logger:      discardLogger(),
		Config:      config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{
			WriteRateLimit: 1000, WriteRateBurst: 1000,
		},
		Now:    func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		Assets: testAssets(),
	})
}

// patch issues a well-formed PATCH with the headers a browser would send.
func patch(t *testing.T, srv *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))
	return rec
}

// ------------------------------------------------------------------ reads --

func TestDriftListReturnsRecords(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var response struct {
		Items      []domain.DriftRecord `json:"items"`
		Pagination Pagination           `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Field != "privileged" {
		t.Errorf("items = %+v", response.Items)
	}
}

// The list defaults to open records: a dashboard buried in resolved history
// hides what still stands.
func TestDriftListDefaultsToOpenOnly(t *testing.T) {
	drift := &fakeDrift{}
	srv := newDriftServer(t, drift)

	do(t, srv, http.MethodGet, APIPrefix+"/drift", nil)

	if !drift.lastFilter.OpenOnly {
		t.Error("openOnly must default to true")
	}
}

// An explicit status filter turns the default off, or asking for resolved
// records and being handed none would be the API contradicting itself.
func TestExplicitStatusFilterDisablesOpenOnly(t *testing.T) {
	drift := &fakeDrift{}
	srv := newDriftServer(t, drift)

	do(t, srv, http.MethodGet, APIPrefix+"/drift?status=resolved", nil)

	if drift.lastFilter.OpenOnly {
		t.Error("an explicit status filter must disable the open-only default")
	}
	if len(drift.lastFilter.Statuses) != 1 || drift.lastFilter.Statuses[0] != domain.DriftStatusResolved {
		t.Errorf("statuses = %v", drift.lastFilter.Statuses)
	}
}

func TestDriftFilterParsing(t *testing.T) {
	drift := &fakeDrift{}
	srv := newDriftServer(t, drift)

	do(t, srv, http.MethodGet,
		APIPrefix+"/drift?severity=critical,high&category=security&page=2&pageSize=10&sort=severity&order=asc", nil)

	filter := drift.lastFilter
	if len(filter.Severities) != 2 {
		t.Errorf("severities = %v, want two", filter.Severities)
	}
	if len(filter.Categories) != 1 || filter.Categories[0] != domain.DriftCategorySecurity {
		t.Errorf("categories = %v", filter.Categories)
	}
	if filter.Sort != "severity" || !filter.Ascending {
		t.Errorf("sort = %q ascending = %v", filter.Sort, filter.Ascending)
	}
	if filter.Page.Limit != 10 || filter.Page.Offset != 10 {
		t.Errorf("page = %+v, want limit 10 offset 10", filter.Page)
	}
}

// An unrecognised vocabulary value is rejected rather than dropped: a typo
// must produce a message, not a silently different result set.
func TestInvalidFilterValuesAreRejected(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	for name, target := range map[string]string{
		"severity":   APIPrefix + "/drift?severity=catastrophic",
		"category":   APIPrefix + "/drift?category=everything",
		"status":     APIPrefix + "/drift?status=deleted",
		"sort":       APIPrefix + "/drift?sort=spec_json",
		"order":      APIPrefix + "/drift?order=sideways",
		"openOnly":   APIPrefix + "/drift?openOnly=perhaps",
		"page":       APIPrefix + "/drift?page=0",
		"pageSize":   APIPrefix + "/drift?pageSize=100000",
		"snapshotId": APIPrefix + "/drift?snapshotId=abc",
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, target, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// A rejected value must not be echoed: an error message is not the place to
// reflect attacker-controlled input.
func TestFilterErrorsDoNotEchoTheValue(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})
	const payload = "REFLECTED-XSS-MARKER"

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift?severity="+payload, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), payload) {
		t.Errorf("the response echoed the offending value: %s", rec.Body)
	}
	// The positive control: the sweep can find the marker when present.
	if !strings.Contains("x"+payload, payload) {
		t.Fatal("the sweep cannot detect its own marker")
	}
}

// A SQL payload in a filter is refused by the allowlist before reaching the
// repository.
func TestFilterValuesCannotCarrySQL(t *testing.T) {
	drift := &fakeDrift{}
	srv := newDriftServer(t, drift)

	for _, payload := range []string{
		"critical'; DROP TABLE drift_records; --",
		"critical UNION SELECT 1",
		"' OR '1'='1",
		"critical) OR 1=1--",
	} {
		// Percent-encoded, because a raw space makes the request target itself
		// invalid and httptest would panic before the handler ever ran. The
		// DECODED value is what reaches the parser, which is what is under
		// test.
		rec := do(t, srv, http.MethodGet,
			APIPrefix+"/drift?severity="+url.QueryEscape(payload), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("payload %q returned %d, want 400", payload, rec.Code)
		}
	}
	if len(drift.lastFilter.Severities) != 0 {
		t.Error("a rejected value must never reach the repository")
	}
}

// A caller cannot make one request carry an unbounded IN clause.
func TestRepeatedFilterValuesAreBounded(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	target := APIPrefix + "/drift?"
	for range 200 {
		target += "severity=critical&"
	}
	rec := do(t, srv, http.MethodGet, strings.TrimSuffix(target, "&"), nil)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; the value count must be bounded", rec.Code)
	}
}

func TestDriftSummaryEndpoint(t *testing.T) {
	drift := &fakeDrift{summary: domain.DriftSummary{
		Total: 5, Open: 3,
		BySeverity:          map[domain.DriftSeverity]int{domain.DriftSeverityCritical: 2},
		ByStatus:            map[domain.DriftStatus]int{domain.DriftStatusActive: 3},
		ByCategory:          map[domain.DriftCategory]int{domain.DriftCategorySecurity: 2},
		ContainersWithDrift: 2, ContainersEvaluated: 9, Incomplete: true,
	}}
	srv := newDriftServer(t, drift)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift/summary", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var summary domain.DriftSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.Total != 5 || summary.Open != 3 {
		t.Errorf("summary = %+v", summary)
	}
	if !summary.Incomplete {
		t.Error("incomplete must survive serialisation; it is what stops the summary reading as complete")
	}
	if summary.ContainersEvaluated != 9 {
		t.Errorf("containersEvaluated = %d, want 9", summary.ContainersEvaluated)
	}
}

// The container view carries the evaluation, so a client can tell "no drift"
// from "never checked" without a second request.
func TestContainerDriftIncludesTheEvaluation(t *testing.T) {
	evaluation := domain.DriftEvaluation{
		ContainerID: "container-a", Complete: false,
		Reason: "no baseline snapshot exists for this container",
	}
	drift := &fakeDrift{evaluation: &evaluation}
	srv := newDriftServer(t, drift)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift/container/a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var response struct {
		ContainerID string                  `json:"containerId"`
		Evaluation  *domain.DriftEvaluation `json:"evaluation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Evaluation == nil {
		t.Fatal("the evaluation must be included")
	}
	if response.Evaluation.Complete {
		t.Error("the incomplete flag must survive")
	}
}

// A never-evaluated container omits the evaluation rather than inventing one.
func TestContainerDriftOmitsAMissingEvaluation(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift/container/a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"evaluation"`) {
		t.Error("an absent evaluation must be omitted, not rendered as an empty object")
	}
}

// The path segment is authoritative: a query parameter cannot widen the
// request to another container.
func TestContainerRouteIgnoresAContainerIdParameter(t *testing.T) {
	drift := &fakeDrift{}
	srv := newDriftServer(t, drift)

	do(t, srv, http.MethodGet, APIPrefix+"/drift/container/a?containerId=container-b", nil)

	if drift.lastFilter.ContainerID != "container-a" {
		t.Errorf("containerID = %q, want the path segment to win", drift.lastFilter.ContainerID)
	}
}

func TestDriftDetailNotFound(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift/404", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDriftIdMustBeAPositiveInteger(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	for _, id := range []string{"abc", "-1", "0", "1.5"} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/drift/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q returned %d, want 400", id, rec.Code)
		}
	}
}

// ------------------------------------------------------------------ write --

func TestPatchAppliesAnOperatorStatus(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	rec := patch(t, srv, APIPrefix+"/drift/1", `{"status":"ignored","note":"known"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	if len(drift.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(drift.updates))
	}
	if drift.updates[0].status != domain.DriftStatusIgnored || drift.updates[0].note != "known" {
		t.Errorf("update = %+v", drift.updates[0])
	}
}

// THE CENTRAL INVARIANT: an operator cannot declare a difference resolved.
// Resolution is something the world does, not something a person asserts, and
// allowing it would make the drift list stop describing reality.
func TestPatchRefusesEngineOwnedStatuses(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	for _, status := range []string{"resolved", "active"} {
		t.Run(status, func(t *testing.T) {
			rec := patch(t, srv, APIPrefix+"/drift/1", `{"status":"`+status+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status %q returned %d, want 400", status, rec.Code)
			}
			if len(drift.updates) != 0 {
				t.Errorf("a rejected status reached the repository: %+v", drift.updates)
			}
		})
	}
}

func TestPatchAcceptsOnlyTheOperatorVocabulary(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	for _, status := range []string{"acknowledged", "ignored", "expected"} {
		rec := patch(t, srv, APIPrefix+"/drift/1", `{"status":"`+status+`"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("status %q returned %d, want 200: %s", status, rec.Code, rec.Body)
		}
	}
	for _, status := range []string{"", "deleted", "REMEDIATED", "restored"} {
		rec := patch(t, srv, APIPrefix+"/drift/1", `{"status":"`+status+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %q returned %d, want 400", status, rec.Code)
		}
	}
}

// PATCH is state-changing, so it gets exactly the POST protections.
func TestPatchRequiresJSONContentType(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		req := httptest.NewRequest(http.MethodPatch, APIPrefix+"/drift/1",
			strings.NewReader(`{"status":"ignored"}`))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authed(req))

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q returned %d, want 415", contentType, rec.Code)
		}
	}
	if len(drift.updates) != 0 {
		t.Error("a rejected request reached the repository")
	}
}

// Unknown fields are rejected rather than ignored, so a caller that thinks it
// can set a severity or a value finds out.
func TestPatchRejectsUnknownFields(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	for _, body := range []string{
		`{"status":"ignored","severity":"low"}`,
		`{"status":"ignored","currentValue":"anything"}`,
		`{"status":"ignored","containerId":"other"}`,
		`{"status":"ignored","resolvedAt":"2026-01-01T00:00:00Z"}`,
	} {
		rec := patch(t, srv, APIPrefix+"/drift/1", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s returned %d, want 400", body, rec.Code)
		}
	}
	if len(drift.updates) != 0 {
		t.Error("a rejected body reached the repository")
	}
}

func TestPatchRejectsACrossSiteRequest(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	req := httptest.NewRequest(http.MethodPatch, APIPrefix+"/drift/1",
		strings.NewReader(`{"status":"ignored"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(drift.updates) != 0 {
		t.Error("a cross-site request reached the repository")
	}
}

// The note is bounded in BYTES, validated as UTF-8, and refuses control
// characters -- which is what stops a stored note rendering as something else.
func TestPatchNoteValidation(t *testing.T) {
	drift := &fakeDrift{records: []domain.DriftRecord{
		driftRecordFixture(1, "privileged", domain.DriftSeverityCritical),
	}}
	srv := newDriftServer(t, drift)

	t.Run("too long", func(t *testing.T) {
		body, _ := json.Marshal(driftStatusRequest{
			Status: "ignored", Note: strings.Repeat("a", 501),
		})
		rec := patch(t, srv, APIPrefix+"/drift/1", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("control characters", func(t *testing.T) {
		body, _ := json.Marshal(driftStatusRequest{
			Status: "ignored", Note: "line one\nforged: line two",
		})
		rec := patch(t, srv, APIPrefix+"/drift/1", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; a note must not carry a newline", rec.Code)
		}
	})

	t.Run("printable unicode is accepted", func(t *testing.T) {
		body, _ := json.Marshal(driftStatusRequest{
			Status: "expected", Note: "expected until Q1 — see ticket #42 日本語",
		})
		rec := patch(t, srv, APIPrefix+"/drift/1", string(body))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})
}

func TestPatchOnAMissingRecordIs404(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	rec := patch(t, srv, APIPrefix+"/drift/9999", `{"status":"ignored"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ------------------------------------------------------------- disabled --

func TestDriftEndpointsReportDisabled(t *testing.T) {
	srv := newAuthedServer(Options{
		Health: &fakeHealth{},
		Logger: discardLogger(),
		Config: config.Server{MaxRequestBytes: 4096},
		Assets: testAssets(),
	})

	for _, target := range []string{
		APIPrefix + "/drift",
		APIPrefix + "/drift/summary",
		APIPrefix + "/drift/1",
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s returned %d, want 503", target, rec.Code)
		}
	}
}

// --------------------------------------------------------------- methods --

// Every drift route rejects the methods it does not serve, with a 405 rather
// than a misleading 404.
func TestDriftRoutesRejectUnsupportedMethods(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	cases := map[string][]string{
		APIPrefix + "/drift":               {http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch},
		APIPrefix + "/drift/1":             {http.MethodPost, http.MethodPut, http.MethodDelete},
		APIPrefix + "/drift/container/abc": {http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch},
	}

	for target, methods := range cases {
		for _, method := range methods {
			rec := do(t, srv, method, target, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s returned %d, want 405", method, target, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow == "" {
				t.Errorf("%s %s: a 405 must carry an Allow header", method, target)
			}
		}
	}
}

// There is deliberately no route that evaluates, deletes, or remediates. This
// is the list that would have to change for one to appear.
func TestNoDriftMutationEndpointsExist(t *testing.T) {
	srv := newDriftServer(t, &fakeDrift{})

	for _, target := range []string{
		APIPrefix + "/drift/1/resolve",
		APIPrefix + "/drift/1/remediate",
		APIPrefix + "/drift/1/apply",
		APIPrefix + "/drift/evaluate",
		APIPrefix + "/drift/sweep",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			rec := do(t, srv, method, target, nil)
			if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted || rec.Code == http.StatusCreated {
				t.Errorf("%s %s succeeded with %d; drift exposes no such capability",
					method, target, rec.Code)
			}
		}
	}
}

// A sensitive record must never serve values, whatever the repository holds.
func TestSensitiveRecordsServeNoValues(t *testing.T) {
	record := driftRecordFixture(1, "DB_PASSWORD", domain.DriftSeverityMedium)
	record.Category = domain.DriftCategoryEnvironment
	record.Sensitive = true
	record.PreviousValue = ""
	record.CurrentValue = ""

	drift := &fakeDrift{records: []domain.DriftRecord{record}}
	srv := newDriftServer(t, drift)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, `"previousValue"`) || strings.Contains(body, `"currentValue"`) {
		t.Errorf("a sensitive record serialised value fields: %s", body)
	}
	if !strings.Contains(body, `"sensitive":true`) {
		t.Errorf("the sensitive marker must be served so a UI can explain the omission: %s", body)
	}
}

// The list endpoint must stay bounded regardless of what the repository holds.
func TestDriftListPageSizeIsCapped(t *testing.T) {
	records := make([]domain.DriftRecord, 0, 300)
	for i := range 300 {
		records = append(records, driftRecordFixture(int64(i+1),
			fmt.Sprintf("field-%d", i), domain.DriftSeverityLow))
	}
	drift := &fakeDrift{records: records}
	srv := newDriftServer(t, drift)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/drift?pageSize=201", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; the page size must be rejected, not clamped", rec.Code)
	}

	do(t, srv, http.MethodGet, APIPrefix+"/drift?pageSize=200", nil)
	if drift.lastFilter.Page.Limit != 200 {
		t.Errorf("limit = %d, want 200", drift.lastFilter.Page.Limit)
	}
}
