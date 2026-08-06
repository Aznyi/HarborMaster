package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Container recreation API tests.
//
// These are the only endpoints in HarborMaster that change something RUNNING,
// so the properties worth defending are about what a caller CANNOT reach:
//
//   - No request body field names a container, an image, or any Docker
//     parameter. The only field is an acquisition id.
//   - There is no allowReuse, no force, and no skipVerification -- and an
//     unknown field is REJECTED rather than ignored, so none can be smuggled in.
//   - There is no rollback, restore, or restart route.
//   - Every write is behind the same guard as the rest of the API: fetch
//     metadata, JSON media type, size limit, rate limit.
//   - A refusal names the specific check that said no, so a client can branch
//     without parsing prose.

const sampleExecutionID = "exec_00112233445566778899"

// ------------------------------------------------------------------ fake --

type fakeExecutions struct {
	mu sync.Mutex

	items   []domain.Execution
	events  []domain.ExecutionEvent
	summary domain.ExecutionSummary

	created  domain.Execution
	requests []service.ExecutionRequest

	requestErr error
	cancelErr  error
	listErr    error
	getErr     error

	cancelled  []string
	enabled    bool
	lastFilter store.ExecutionFilter
}

func (f *fakeExecutions) Request(
	_ context.Context, request service.ExecutionRequest,
) (domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.requestErr != nil {
		return domain.Execution{}, f.requestErr
	}
	return f.created, nil
}

func (f *fakeExecutions) Cancel(_ context.Context, id string) (domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	if f.cancelErr != nil {
		return domain.Execution{}, f.cancelErr
	}
	cancelled := f.created
	cancelled.State = domain.ExecutionCancelled
	return cancelled, nil
}

func (f *fakeExecutions) Get(
	_ context.Context, id string,
) (domain.Execution, []domain.ExecutionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return domain.Execution{}, nil, f.getErr
	}
	for _, item := range f.items {
		if item.ExecutionID == id {
			return item, f.events, nil
		}
	}
	return domain.Execution{}, nil, store.ErrNotFound
}

func (f *fakeExecutions) List(
	_ context.Context, filter store.ExecutionFilter,
) ([]domain.Execution, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.items, len(f.items), nil
}

func (f *fakeExecutions) Summary(context.Context) (domain.ExecutionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, nil
}

func (f *fakeExecutions) Eligible(
	context.Context, string,
) (domain.ExecutionTarget, domain.ExecutionRefusal, error) {
	return domain.ExecutionTarget{}, domain.ExecutionRefusalNone, nil
}

func (f *fakeExecutions) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

func (f *fakeExecutions) filter() store.ExecutionFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

func (f *fakeExecutions) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ------------------------------------------------------------- harnesses --

func sampleExecution() domain.Execution {
	at := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	return domain.Execution{
		ExecutionID:   sampleExecutionID,
		AcquisitionID: sampleAcquisitionID,
		PlanID:        samplePlanID,
		ContainerID:   "container-a",
		ContainerName: "web",
		OldImage:      "nginx:1.27.0",
		Target: domain.ExecutionTarget{
			Registry:   "docker.io",
			Repository: "library/nginx",
			Digest:     "sha256:" + strings.Repeat("a", 64),
			Reference:  "nginx:1.27.1",
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State: domain.ExecutionQueued,
		Verification: domain.ExecutionVerification{
			Health:       domain.VerificationUnknown,
			Image:        domain.VerificationUnknown,
			Preservation: domain.VerificationUnknown,
			Network:      domain.VerificationUnknown,
		},
		RequestedAt: at,
		ExpiresAt:   at.Add(15 * time.Minute),
	}
}

func newExecutionServer(t *testing.T, executions *fakeExecutions) *Server {
	t.Helper()

	var capability ExecutionService
	if executions != nil {
		capability = executions
	}

	return newAuthedServer(Options{
		Health:         &fakeHealth{},
		Executions:     capability,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})
}

const validExecutionBody = `{"acquisitionId":"` + sampleAcquisitionID + `"}`

// ----------------------------------------------------------------- reads --

func TestExecutionsListWithTheirSummary(t *testing.T) {
	executions := &fakeExecutions{
		items: []domain.Execution{sampleExecution()},
		summary: domain.ExecutionSummary{
			Total: 5, Active: 1, Succeeded: 3, Failed: 1, NeedsAttention: 1, Enabled: true,
		},
		enabled: true,
	}
	srv := newExecutionServer(t, executions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/executions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response executionListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(response.Items))
	}
	if response.Summary.NeedsAttention != 1 {
		t.Errorf("needsAttention = %d, want 1; it is the number an operator opens this page for",
			response.Summary.NeedsAttention)
	}
}

func TestExecutionListFiltersAreValidatedAgainstAClosedVocabulary(t *testing.T) {
	executions := &fakeExecutions{enabled: true}
	srv := newExecutionServer(t, executions)

	// Accepted values reach the repository filter.
	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/executions?state=creating&failure=unhealthy&needsAttention=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	filter := executions.filter()
	if len(filter.States) != 1 || filter.States[0] != domain.ExecutionCreating {
		t.Errorf("states = %v", filter.States)
	}
	if len(filter.Failures) != 1 || filter.Failures[0] != domain.ExecutionFailureUnhealthy {
		t.Errorf("failures = %v", filter.Failures)
	}
	if !filter.NeedsAttention {
		t.Error("needsAttention was not passed through")
	}

	// Anything else is refused rather than ignored. An ignored filter is how a
	// caller ends up believing they are looking at a subset.
	//
	// An empty value is deliberately absent from this list: the shared query
	// parser skips it, which means "no filter" rather than "a filter that
	// matches nothing". That is consistent across every endpoint in this API.
	for _, query := range []string{
		"state=exploded",
		"failure=" + url.QueryEscape("' OR 1=1--"),
		"needsAttention=yes",
		"activeOnly=1",
		"sort=" + url.QueryEscape("container_name;DROP TABLE executions"),
		"order=sideways",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/executions?"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

func TestExecutionDetailReturnsItsAuditTrail(t *testing.T) {
	executions := &fakeExecutions{
		items:   []domain.Execution{sampleExecution()},
		events:  []domain.ExecutionEvent{{State: domain.ExecutionQueued, Detail: "requested"}},
		enabled: true,
	}
	srv := newExecutionServer(t, executions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/executions/"+sampleExecutionID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response executionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Execution.ExecutionID != sampleExecutionID {
		t.Errorf("executionId = %q", response.Execution.ExecutionID)
	}
	if len(response.Events) != 1 {
		t.Errorf("got %d events, want 1", len(response.Events))
	}
}

func TestAMalformedExecutionIDIsRefusedBeforeTheDatabase(t *testing.T) {
	executions := &fakeExecutions{enabled: true}
	srv := newExecutionServer(t, executions)

	for _, id := range []string{
		"1", "exec_", "exec_zzzzzzzzzzzzzzzzzzzz", "acq_00112233445566778899",
		"exec_00112233445566778899x", "..%2f..%2fetc",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/executions/"+id, nil)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 400 or 404", id, rec.Code)
		}
	}
}

// ---------------------------------------------------------------- writes --

func TestExecutionCreateAcceptsOnlyAnAcquisitionID(t *testing.T) {
	executions := &fakeExecutions{created: sampleExecution(), enabled: true}
	srv := newExecutionServer(t, executions)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/executions", validExecutionBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if executions.requestCount() != 1 {
		t.Fatalf("the service was asked %d times", executions.requestCount())
	}
	if got := executions.requests[0].AcquisitionID; got != sampleAcquisitionID {
		t.Errorf("acquisitionId = %q", got)
	}
}

// TestNoDockerParameterCanBeSmuggledIntoARecreation is the containment test.
//
// Every one of these fields would, if accepted, let an unauthenticated caller
// aim the recreation at something the plan never approved -- or weaken the
// checks that decide whether it happens at all.
func TestNoDockerParameterCanBeSmuggledIntoARecreation(t *testing.T) {
	forbidden := []string{
		`{"acquisitionId":"` + sampleAcquisitionID + `","containerId":"other"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","image":"evil:latest"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","digest":"sha256:0"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","command":["sh","-c","id"]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","entrypoint":["/bin/sh"]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","binds":["/:/host"]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","mounts":[{"source":"/","target":"/host"}]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","privileged":true}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","capAdd":["SYS_ADMIN"]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","securityOpt":["seccomp=unconfined"]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","user":"root"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","network":"host"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","env":["X=1"]}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","force":true}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","allowReuse":true}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","skipVerification":true}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","removeVolumes":true}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","startupTimeout":"1ms"}`,
	}

	for _, body := range forbidden {
		executions := &fakeExecutions{created: sampleExecution(), enabled: true}
		srv := newExecutionServer(t, executions)

		rec := write(t, srv, http.MethodPost, APIPrefix+"/executions", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
		if executions.requestCount() != 0 {
			t.Errorf("body %s reached the service", body)
		}
	}
}

func TestAMalformedAcquisitionIDIsRefusedBeforeTheService(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"acquisitionId":""}`,
		`{"acquisitionId":"nonsense"}`,
		`{"acquisitionId":"exec_00112233445566778899"}`,
		`{"acquisitionId":"acq_ZZZZZZZZZZZZZZZZZZZZ"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","requestKey":"` + strings.Repeat("k", 200) + `"}`,
		`{"acquisitionId":"` + sampleAcquisitionID + `","requestKey":"has space"}`,
	} {
		executions := &fakeExecutions{created: sampleExecution(), enabled: true}
		srv := newExecutionServer(t, executions)

		rec := write(t, srv, http.MethodPost, APIPrefix+"/executions", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
		if executions.requestCount() != 0 {
			t.Errorf("body %s reached the service", body)
		}
	}
}

// TestEveryRefusalReachesTheClientWithItsName.
func TestEveryRefusalReachesTheClientWithItsName(t *testing.T) {
	cases := []struct {
		refusal domain.ExecutionRefusal
		status  int
	}{
		{domain.ExecutionRefusalDisabled, http.StatusServiceUnavailable},
		{domain.ExecutionRefusalAcquisitionMissing, http.StatusNotFound},
		{domain.ExecutionRefusalPlanMissing, http.StatusNotFound},
		{domain.ExecutionRefusalContainerMissing, http.StatusNotFound},
		{domain.ExecutionRefusalLimit, http.StatusTooManyRequests},
		{domain.ExecutionRefusalAcquisitionConsumed, http.StatusConflict},
		{domain.ExecutionRefusalPlanSuperseded, http.StatusConflict},
		{domain.ExecutionRefusalConflict, http.StatusConflict},
		{domain.ExecutionRefusalDigestMismatch, http.StatusConflict},
		{domain.ExecutionRefusalDockerUnavailable, http.StatusConflict},
	}

	for _, tc := range cases {
		executions := &fakeExecutions{
			enabled:    true,
			requestErr: service.ErrExecutionRefused{Refusal: tc.refusal},
		}
		srv := newExecutionServer(t, executions)

		rec := write(t, srv, http.MethodPost, APIPrefix+"/executions", validExecutionBody)
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.refusal, rec.Code, tc.status)
		}

		var response executionRefusalResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("%s: decode: %v", tc.refusal, err)
		}
		if response.Refusal != tc.refusal {
			t.Errorf("%s: refusal field = %q", tc.refusal, response.Refusal)
		}
		if response.Error.Message != tc.refusal.Explain() {
			t.Errorf("%s: message = %q, want HarborMaster's own phrase",
				tc.refusal, response.Error.Message)
		}
	}
}

func TestExecutionCancelStopsWorkThatHasNotStarted(t *testing.T) {
	executions := &fakeExecutions{created: sampleExecution(), enabled: true}
	srv := newExecutionServer(t, executions)

	rec := do(t, srv, http.MethodPost,
		APIPrefix+"/executions/"+sampleExecutionID+"/cancel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(executions.cancelled) != 1 || executions.cancelled[0] != sampleExecutionID {
		t.Errorf("cancelled = %v", executions.cancelled)
	}
}

// TestCancellingAMutatingRecreationIsAConflictThatExplainsItself.
func TestCancellingAMutatingRecreationIsAConflictThatExplainsItself(t *testing.T) {
	executions := &fakeExecutions{
		enabled:   true,
		cancelErr: service.ErrExecutionRefused{Refusal: domain.ExecutionRefusalNone},
	}
	srv := newExecutionServer(t, executions)

	rec := do(t, srv, http.MethodPost,
		APIPrefix+"/executions/"+sampleExecutionID+"/cancel", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	// The message must say WHY, not merely refuse: "you cannot cancel this"
	// invites an operator to try harder.
	if !strings.Contains(rec.Body.String(), "passed the point") {
		t.Errorf("the 409 does not explain itself: %s", rec.Body.String())
	}
}

// ------------------------------------------------------- the write guard --

func TestExecutionWritesGoThroughTheSameGuardAsEveryOtherWrite(t *testing.T) {
	executions := &fakeExecutions{created: sampleExecution(), enabled: true}
	srv := newExecutionServer(t, executions)

	// A cross-site fetch is refused.
	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/executions", strings.NewReader(validExecutionBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: status = %d, want 403", rec.Code)
	}

	// A non-JSON media type is refused, which is also what forces a CORS
	// preflight for a cross-origin caller.
	req = httptest.NewRequest(http.MethodPost, APIPrefix+"/executions", strings.NewReader(validExecutionBody))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST: status = %d, want 415", rec.Code)
	}

	// An oversized body is refused.
	rec = write(t, srv, http.MethodPost, APIPrefix+"/executions",
		`{"acquisitionId":"`+sampleAcquisitionID+`","requestKey":"`+strings.Repeat("x", 8192)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("oversized POST: status = %d, want 413 or 400", rec.Code)
	}

	if executions.requestCount() != 0 {
		t.Errorf("%d guarded requests reached the service", executions.requestCount())
	}
}

func TestExecutionWritesAreRateLimited(t *testing.T) {
	executions := &fakeExecutions{created: sampleExecution(), enabled: true}

	srv := newAuthedServer(Options{
		Health:         &fakeHealth{},
		Executions:     executions,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 1, WriteRateBurst: 1},
		Assets:         testAssets(),
	})

	if rec := write(t, srv, http.MethodPost, APIPrefix+"/executions",
		validExecutionBody); rec.Code != http.StatusAccepted {
		t.Fatalf("first request: status = %d", rec.Code)
	}
	rec := write(t, srv, http.MethodPost, APIPrefix+"/executions", validExecutionBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 carried no Retry-After")
	}
}

// ------------------------------------------------------------- disabled --

func TestARecreationEndpointIsUnavailableWhenNotWired(t *testing.T) {
	srv := newExecutionServer(t, nil)

	for _, probe := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, APIPrefix + "/executions"},
		{http.MethodGet, APIPrefix + "/executions/" + sampleExecutionID},
		{http.MethodPost, APIPrefix + "/executions"},
		{http.MethodPost, APIPrefix + "/executions/" + sampleExecutionID + "/cancel"},
	} {
		var rec = do(t, srv, probe.method, probe.path, nil)
		if probe.method == http.MethodPost && probe.path == APIPrefix+"/executions" {
			rec = write(t, srv, probe.method, probe.path, validExecutionBody)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", probe.method, probe.path, rec.Code)
		}
	}
}

// ------------------------------------------------------------- no routes --

// TestThereIsNoPerExecutionMutationRoute.
//
// The list of paths this feature does NOT serve, asserted rather than assumed.
// Each would be a reasonable thing to want and none exists.
//
// Manual rollback DOES exist, as its own resource at `/rollbacks`, and it is
// still not here. A sub-resource under an execution would make "undo this" read
// like a property of the recreation; it is a separate, separately authorised,
// separately audited operation with its own record, and the URL says so.
func TestThereIsNoPerExecutionMutationRoute(t *testing.T) {
	srv := newExecutionServer(t, &fakeExecutions{enabled: true})

	for _, path := range []string{
		APIPrefix + "/executions/" + sampleExecutionID + "/rollback",
		APIPrefix + "/executions/" + sampleExecutionID + "/restore",
		APIPrefix + "/executions/" + sampleExecutionID + "/retry",
		APIPrefix + "/executions/" + sampleExecutionID + "/recover",
		APIPrefix + "/executions/" + sampleExecutionID + "/apply",
		APIPrefix + "/executions/" + sampleExecutionID + "/force",
		APIPrefix + "/containers/container-a/recreate",
		APIPrefix + "/containers/container-a/restart",
		APIPrefix + "/containers/container-a/stop",
	} {
		rec := write(t, srv, http.MethodPost, path, `{}`)
		if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
			t.Errorf("%s answered %d; that capability must not exist", path, rec.Code)
		}
	}
}

// TestExecutionsRejectUnsupportedMethods.
func TestExecutionsRejectUnsupportedMethods(t *testing.T) {
	srv := newExecutionServer(t, &fakeExecutions{enabled: true})

	for _, probe := range []struct {
		method string
		path   string
	}{
		{http.MethodDelete, APIPrefix + "/executions"},
		{http.MethodPut, APIPrefix + "/executions"},
		{http.MethodPatch, APIPrefix + "/executions/" + sampleExecutionID},
		{http.MethodDelete, APIPrefix + "/executions/" + sampleExecutionID},
		{http.MethodGet, APIPrefix + "/executions/" + sampleExecutionID + "/cancel"},
	} {
		rec := do(t, srv, probe.method, probe.path, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", probe.method, probe.path, rec.Code)
		}
	}
}

// ---------------------------------------------------------- disclosure --

// TestARecreationResponseCarriesNoSecret.
//
// The record travels with a preservation report, whose values are keyed digests
// rather than configuration values. This asserts the response for the shape of
// one.
func TestARecreationResponseCarriesNoSecret(t *testing.T) {
	execution := sampleExecution()
	execution.State = domain.ExecutionFailed
	execution.Failure = domain.ExecutionFailurePreservation
	execution.Checkpoint = domain.CheckpointReplacementQuarantined
	execution.Verification.Preservation = domain.VerificationFailed
	execution.Verification.Report = &domain.PreservationReport{
		Status: domain.VerificationFailed, Checked: 40, Matched: 39,
		Differences: []domain.PreservationDifference{
			{
				Field: "environment", Kind: domain.PreservationChanged,
				Expected: "DB_PASSWORD=digest:aaaa", Actual: "DB_PASSWORD=digest:bbbb",
			},
		},
	}
	execution.Recovery = domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:    sampleExecutionID,
		ContainerName:  "web",
		OriginalID:     strings.Repeat("a", 64),
		ParkedName:     "web.hm-old-" + sampleExecutionID,
		ReplacementID:  strings.Repeat("b", 64),
		QuarantineName: "web.hm-failed-" + sampleExecutionID,
		Checkpoint:     domain.CheckpointReplacementQuarantined,
	})

	executions := &fakeExecutions{items: []domain.Execution{execution}, enabled: true}
	srv := newExecutionServer(t, executions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/executions/"+sampleExecutionID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, "hunter2") {
		t.Fatal("the response carried a secret value")
	}
	// The recovery plan must reach the operator: it is the whole point of
	// failing this way rather than rolling back.
	if !strings.Contains(body, "hm-old-") {
		t.Error("the response does not name the parked container")
	}
	if !strings.Contains(body, "recovery") {
		t.Error("the response carries no recovery plan")
	}
}
