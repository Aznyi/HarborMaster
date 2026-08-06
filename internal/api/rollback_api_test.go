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

// Manual rollback API tests.
//
// Rollback is the highest-risk capability in the project, so the properties
// worth defending are all about what a caller CANNOT reach:
//
//   - The body has two fields, and neither names a container. Every identity
//     comes from HarborMaster's own execution record.
//   - An unknown field is REJECTED rather than ignored, so no Docker parameter
//     can be smuggled in.
//   - Reading needs a permission, requesting needs a stronger one, and an
//     anonymous caller gets neither.
//   - A cookie-authenticated write without a CSRF token is refused.
//   - A refusal names the specific check that said no, so a client branches on
//     a closed vocabulary rather than on prose.
//   - There is no rollback delete, no rollback edit, and no route that takes a
//     container id.

const sampleRollbackID = "rbk_00112233445566778899"

// ------------------------------------------------------------------ fake --

type fakeRollbacks struct {
	mu sync.Mutex

	items   []domain.Rollback
	events  []domain.RollbackEvent
	summary domain.RollbackSummary

	created  domain.Rollback
	requests []service.RollbackRequest

	requestErr error
	cancelErr  error
	listErr    error
	getErr     error

	cancelled  []string
	enabled    bool
	lastFilter store.RollbackFilter
}

func (f *fakeRollbacks) Request(
	_ context.Context, request service.RollbackRequest,
) (domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.requestErr != nil {
		return domain.Rollback{}, f.requestErr
	}
	return f.created, nil
}

func (f *fakeRollbacks) Cancel(_ context.Context, id string) (domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	if f.cancelErr != nil {
		return domain.Rollback{}, f.cancelErr
	}
	cancelled := f.created
	cancelled.State = domain.RollbackCancelled
	return cancelled, nil
}

func (f *fakeRollbacks) Get(
	_ context.Context, id string,
) (domain.Rollback, []domain.RollbackEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return domain.Rollback{}, nil, f.getErr
	}
	for _, item := range f.items {
		if item.RollbackID == id {
			return item, f.events, nil
		}
	}
	return domain.Rollback{}, nil, store.ErrNotFound
}

func (f *fakeRollbacks) List(
	_ context.Context, filter store.RollbackFilter,
) ([]domain.Rollback, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.items, len(f.items), nil
}

func (f *fakeRollbacks) Summary(context.Context) (domain.RollbackSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, nil
}

func (f *fakeRollbacks) Eligible(
	context.Context, string,
) (domain.RollbackEligibility, error) {
	return domain.RollbackEligibility{Eligible: true}, nil
}

func (f *fakeRollbacks) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

func (f *fakeRollbacks) filter() store.RollbackFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

func (f *fakeRollbacks) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeRollbacks) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancelled)
}

// ------------------------------------------------------------- harnesses --

func sampleRollback() domain.Rollback {
	at := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	return domain.Rollback{
		RollbackID:       sampleRollbackID,
		ExecutionID:      sampleExecutionID,
		ContainerName:    "web",
		OriginalID:       strings.Repeat("a", 64),
		ParkedName:       "web" + domain.ParkedNameSuffix + sampleExecutionID,
		ReplacementID:    strings.Repeat("b", 64),
		OriginalImage:    "nginx:1.27.0",
		OriginalImageID:  "sha256:" + strings.Repeat("c", 64),
		ReplacementImage: "nginx:1.27.1",
		State:            domain.RollbackQueued,
		Verification: domain.RollbackVerification{
			Health:       domain.VerificationUnknown,
			Image:        domain.VerificationUnknown,
			Preservation: domain.VerificationUnknown,
			Network:      domain.VerificationUnknown,
		},
		RequestedAt: at,
		ExpiresAt:   at.Add(10 * time.Minute),
	}
}

func newRollbackServer(t *testing.T, rollbacks *fakeRollbacks) *Server {
	t.Helper()

	var capability RollbackService
	if rollbacks != nil {
		capability = rollbacks
	}

	return newAuthedServer(Options{
		Health:         &fakeHealth{},
		Rollbacks:      capability,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})
}

const validRollbackBody = `{"executionId":"` + sampleExecutionID + `"}`

// ----------------------------------------------------------------- reads --

func TestRollbacksListWithTheirSummary(t *testing.T) {
	rollbacks := &fakeRollbacks{
		items: []domain.Rollback{sampleRollback()},
		summary: domain.RollbackSummary{
			Total: 4, Active: 1, Succeeded: 2, Failed: 1, NeedsAttention: 1,
		},
		enabled: true,
	}
	srv := newRollbackServer(t, rollbacks)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/rollbacks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response rollbackListResponse
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

func TestRollbackListFiltersAreValidatedAgainstAClosedVocabulary(t *testing.T) {
	rollbacks := &fakeRollbacks{enabled: true}
	srv := newRollbackServer(t, rollbacks)

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/rollbacks?state=validating&failure=unhealthy&needsAttention=true"+
			"&executionId="+sampleExecutionID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	filter := rollbacks.filter()
	if len(filter.States) != 1 || filter.States[0] != domain.RollbackValidating {
		t.Errorf("states = %v", filter.States)
	}
	if len(filter.Failures) != 1 || filter.Failures[0] != domain.RollbackFailureUnhealthy {
		t.Errorf("failures = %v", filter.Failures)
	}
	if !filter.NeedsAttention {
		t.Error("needsAttention was not passed through")
	}
	if filter.ExecutionID != sampleExecutionID {
		t.Errorf("executionId = %q", filter.ExecutionID)
	}

	// Anything else is refused rather than ignored: an ignored filter is how a
	// caller ends up believing they are looking at a subset.
	for _, query := range []string{
		"state=exploded",
		"failure=" + url.QueryEscape("' OR 1=1--"),
		"needsAttention=yes",
		"activeOnly=1",
		"executionId=" + url.QueryEscape("exec_00112233445566778899; DROP TABLE rollbacks"),
		"executionId=rbk_00112233445566778899",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/rollbacks?"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

func TestRollbackDetailReturnsItsCheckpointTrail(t *testing.T) {
	rollbacks := &fakeRollbacks{
		items: []domain.Rollback{sampleRollback()},
		events: []domain.RollbackEvent{
			{State: domain.RollbackQueued, Detail: "rollback requested"},
			{
				State:      domain.RollbackStoppingReplacement,
				Checkpoint: domain.RollbackCheckpointReplacementStopped,
				Detail:     "the replacement container is stopped",
			},
		},
		enabled: true,
	}
	srv := newRollbackServer(t, rollbacks)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/rollbacks/"+sampleRollbackID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response rollbackDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Rollback.RollbackID != sampleRollbackID {
		t.Errorf("rollbackId = %q", response.Rollback.RollbackID)
	}
	if len(response.Events) != 2 {
		t.Errorf("got %d events, want 2", len(response.Events))
	}
	if response.Events[1].Checkpoint != domain.RollbackCheckpointReplacementStopped {
		t.Errorf("the checkpoint did not survive the response: %+v", response.Events[1])
	}
}

func TestAMalformedRollbackIDIsRefusedBeforeTheDatabase(t *testing.T) {
	rollbacks := &fakeRollbacks{enabled: true}
	srv := newRollbackServer(t, rollbacks)

	for _, id := range []string{
		"1", "rbk_", "rbk_zzzzzzzzzzzzzzzzzzzz", "exec_00112233445566778899",
		"rbk_00112233445566778899x", "..%2f..%2fetc",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/rollbacks/"+id, nil)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 400 or 404", id, rec.Code)
		}
	}
}

// TestAnUnknownRollbackIsNotFoundRatherThanEnumerable.
func TestAnUnknownRollbackIsNotFoundRatherThanEnumerable(t *testing.T) {
	rollbacks := &fakeRollbacks{enabled: true}
	srv := newRollbackServer(t, rollbacks)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/rollbacks/rbk_ffffffffffffffffffff", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "rbk_ffffffffffffffffffff") {
		t.Error("the response echoes the requested id back")
	}
}

// ---------------------------------------------------------------- writes --

func TestRollbackCreateAcceptsOnlyAnExecutionID(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv := newRollbackServer(t, rollbacks)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", validRollbackBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if rollbacks.requestCount() != 1 {
		t.Fatalf("the service was asked %d times", rollbacks.requestCount())
	}
	if got := rollbacks.requests[0].ExecutionID; got != sampleExecutionID {
		t.Errorf("executionId = %q", got)
	}
}

// TestNoDockerParameterCanBeSmuggledIntoARollback is the containment test.
//
// Every one of these fields would, if accepted, let a caller aim the rollback
// at a container of their choosing -- or weaken the checks that decide whether
// it happens at all. Unknown fields are rejected, so none of them can arrive.
func TestNoDockerParameterCanBeSmuggledIntoARollback(t *testing.T) {
	forbidden := []string{
		`{"executionId":"` + sampleExecutionID + `","containerId":"other"}`,
		`{"executionId":"` + sampleExecutionID + `","originalId":"` + strings.Repeat("f", 64) + `"}`,
		`{"executionId":"` + sampleExecutionID + `","replacementId":"` + strings.Repeat("f", 64) + `"}`,
		`{"executionId":"` + sampleExecutionID + `","containerName":"other"}`,
		`{"executionId":"` + sampleExecutionID + `","image":"evil:latest"}`,
		`{"executionId":"` + sampleExecutionID + `","snapshotId":7}`,
		`{"executionId":"` + sampleExecutionID + `","force":true}`,
		`{"executionId":"` + sampleExecutionID + `","skipVerification":true}`,
		`{"executionId":"` + sampleExecutionID + `","removeReplacement":true}`,
		`{"executionId":"` + sampleExecutionID + `","hostConfig":{"Privileged":true}}`,
		`{"executionId":"` + sampleExecutionID + `","binds":["/:/host"]}`,
		`{"executionId":"` + sampleExecutionID + `","env":["DB_PASSWORD=hunter2"]}`,
	}

	for _, body := range forbidden {
		rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
		srv := newRollbackServer(t, rollbacks)

		rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if rollbacks.requestCount() != 0 {
			t.Errorf("%s: reached the service", body)
		}
	}
}

func TestARollbackRequestNeedsAWellFormedExecutionID(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"executionId":""}`,
		`{"executionId":"exec_"}`,
		`{"executionId":"exec_zzzzzzzzzzzzzzzzzzzz"}`,
		`{"executionId":"rbk_00112233445566778899"}`,
		`{"executionId":"../../etc/passwd"}`,
		`{"executionId":"exec_00112233445566778899 OR 1=1"}`,
	} {
		rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
		srv := newRollbackServer(t, rollbacks)

		rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if rollbacks.requestCount() != 0 {
			t.Errorf("%s: reached the service", body)
		}
	}
}

func TestTheRequestKeyIsBounded(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv := newRollbackServer(t, rollbacks)

	body := `{"executionId":"` + sampleExecutionID + `","requestKey":"` +
		strings.Repeat("k", 500) + `"}`
	rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if rollbacks.requestCount() != 0 {
		t.Error("an oversized request key reached the service")
	}
}

// TestARollbackRefusalNamesTheCheckThatSaidNo.
func TestARollbackRefusalNamesTheCheckThatSaidNo(t *testing.T) {
	cases := []struct {
		refusal domain.RollbackRefusal
		status  int
	}{
		{domain.RollbackRefusalOriginalRemoved, http.StatusConflict},
		{domain.RollbackRefusalAlreadyRolledBack, http.StatusConflict},
		{domain.RollbackRefusalConflict, http.StatusConflict},
		{domain.RollbackRefusalCheckpointUncertain, http.StatusConflict},
		{domain.RollbackRefusalExecutionMissing, http.StatusNotFound},
		{domain.RollbackRefusalDockerUnavailable, http.StatusServiceUnavailable},
		{domain.RollbackRefusalDisabled, http.StatusServiceUnavailable},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.refusal), func(t *testing.T) {
			rollbacks := &fakeRollbacks{
				enabled:    true,
				requestErr: service.RollbackRefusedError{Refusal: testCase.refusal},
			}
			srv := newRollbackServer(t, rollbacks)

			rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", validRollbackBody)
			if rec.Code != testCase.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, testCase.status, rec.Body.String())
			}

			var response rollbackRefusalResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if response.Refusal != testCase.refusal {
				t.Errorf("refusal = %q, want %q", response.Refusal, testCase.refusal)
			}
			if response.Error.Message == "" {
				t.Error("a refusal with no operator-facing message")
			}
		})
	}
}

// TestAnInternalRollbackFailureSaysNothingAboutTheHost.
func TestAnInternalRollbackFailureSaysNothingAboutTheHost(t *testing.T) {
	leak := "dial unix /var/run/docker.sock: permission denied (DB_PASSWORD=hunter2)"
	rollbacks := &fakeRollbacks{enabled: true, requestErr: &opaqueError{message: leak}}
	srv := newRollbackServer(t, rollbacks)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", validRollbackBody)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	for _, fragment := range []string{"hunter2", "/var/run/docker.sock", "permission denied"} {
		if strings.Contains(rec.Body.String(), fragment) {
			t.Errorf("the response leaks %q", fragment)
		}
	}
}

// opaqueError is an error whose text must never reach a response.
type opaqueError struct{ message string }

func (e *opaqueError) Error() string { return e.message }

// ---------------------------------------------------------- cancellation --

func TestRollbackCancelReachesTheService(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv := newRollbackServer(t, rollbacks)

	rec := write(t, srv, http.MethodPost,
		APIPrefix+"/rollbacks/"+sampleRollbackID+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if rollbacks.cancelCount() != 1 {
		t.Fatalf("the service was asked to cancel %d times", rollbacks.cancelCount())
	}
	if rollbacks.cancelled[0] != sampleRollbackID {
		t.Errorf("cancelled %q", rollbacks.cancelled[0])
	}
}

// TestCancellingAfterTheMutationPointIsAConflict.
//
// 409 rather than 400: the request was well formed and would have been honoured
// a moment earlier. What changed is the world.
func TestCancellingAfterTheMutationPointIsAConflict(t *testing.T) {
	rollbacks := &fakeRollbacks{
		enabled:   true,
		cancelErr: service.ErrRollbackNotCancellable,
	}
	srv := newRollbackServer(t, rollbacks)

	rec := write(t, srv, http.MethodPost,
		APIPrefix+"/rollbacks/"+sampleRollbackID+"/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// --------------------------------------------------------- authorization --

// TestEveryRollbackRouteRefusesAnAnonymousCaller.
func TestEveryRollbackRouteRefusesAnAnonymousCaller(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv := newRollbackServer(t, rollbacks)

	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, APIPrefix + "/rollbacks", ""},
		{http.MethodGet, APIPrefix + "/rollbacks/" + sampleRollbackID, ""},
		{http.MethodPost, APIPrefix + "/rollbacks", validRollbackBody},
		{http.MethodPost, APIPrefix + "/rollbacks/" + sampleRollbackID + "/cancel", ""},
	}

	for _, testCase := range cases {
		req := httptest.NewRequest(testCase.method, testCase.target,
			strings.NewReader(testCase.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, anonymous(req))

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401",
				testCase.method, testCase.target, rec.Code)
		}
	}
	if rollbacks.requestCount() != 0 || rollbacks.cancelCount() != 0 {
		t.Error("an anonymous caller reached the rollback service")
	}
}

// TestARollbackNeedsMoreThanReadAccess is the role matrix.
//
// A viewer may READ the rollback history -- an incident is not a reason to hide
// the record of it -- but only an operator or an administrator may cause one.
func TestARollbackNeedsMoreThanReadAccess(t *testing.T) {
	cases := []struct {
		role      domain.Role
		read      int
		request   int
		cancelled int
	}{
		{domain.RoleViewer, http.StatusOK, http.StatusForbidden, http.StatusForbidden},
		{domain.RoleOperator, http.StatusOK, http.StatusAccepted, http.StatusOK},
		{domain.RoleAdministrator, http.StatusOK, http.StatusAccepted, http.StatusOK},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.role), func(t *testing.T) {
			rollbacks := &fakeRollbacks{
				created: sampleRollback(),
				items:   []domain.Rollback{sampleRollback()},
				enabled: true,
			}
			srv, _, _ := asRole(Options{
				Health:         &fakeHealth{},
				Rollbacks:      rollbacks,
				Logger:         discardLogger(),
				Config:         config.Server{MaxRequestBytes: 4096},
				SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
				Assets:         testAssets(),
			}, testCase.role)

			if rec := do(t, srv, http.MethodGet, APIPrefix+"/rollbacks", nil); rec.Code != testCase.read {
				t.Errorf("list: status = %d, want %d", rec.Code, testCase.read)
			}

			rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", validRollbackBody)
			if rec.Code != testCase.request {
				t.Errorf("create: status = %d, want %d: %s",
					rec.Code, testCase.request, rec.Body.String())
			}

			rec = write(t, srv, http.MethodPost,
				APIPrefix+"/rollbacks/"+sampleRollbackID+"/cancel", "")
			if rec.Code != testCase.cancelled {
				t.Errorf("cancel: status = %d, want %d", rec.Code, testCase.cancelled)
			}

			if testCase.role == domain.RoleViewer && rollbacks.requestCount() != 0 {
				t.Error("a viewer reached the rollback service")
			}
		})
	}
}

// TestARollbackWriteWithoutACSRFTokenIsRefused.
func TestARollbackWriteWithoutACSRFTokenIsRefused(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv := newRollbackServer(t, rollbacks)

	for _, target := range []string{
		APIPrefix + "/rollbacks",
		APIPrefix + "/rollbacks/" + sampleRollbackID + "/cancel",
	} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(validRollbackBody))
		req.Header.Set("Content-Type", "application/json")
		// The session cookie, deliberately WITHOUT the header a browser would
		// have to be able to read to set.
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", target, rec.Code)
		}
	}
	if rollbacks.requestCount() != 0 || rollbacks.cancelCount() != 0 {
		t.Error("a request with no CSRF token reached the rollback service")
	}
}

// TestTheRollbackRoutesRefuseTheMethodsTheyDoNotOffer.
//
// Notably: there is no DELETE and no PATCH. A rollback record is immutable, and
// a rollback cannot be un-run.
func TestTheRollbackRoutesRefuseTheMethodsTheyDoNotOffer(t *testing.T) {
	rollbacks := &fakeRollbacks{items: []domain.Rollback{sampleRollback()}, enabled: true}
	srv := newRollbackServer(t, rollbacks)

	cases := []struct {
		method string
		target string
	}{
		{http.MethodDelete, APIPrefix + "/rollbacks"},
		{http.MethodPut, APIPrefix + "/rollbacks"},
		{http.MethodPatch, APIPrefix + "/rollbacks"},
		{http.MethodDelete, APIPrefix + "/rollbacks/" + sampleRollbackID},
		{http.MethodPut, APIPrefix + "/rollbacks/" + sampleRollbackID},
		{http.MethodPatch, APIPrefix + "/rollbacks/" + sampleRollbackID},
		{http.MethodDelete, APIPrefix + "/rollbacks/" + sampleRollbackID + "/cancel"},
		{http.MethodGet, APIPrefix + "/rollbacks/" + sampleRollbackID + "/cancel"},
	}

	for _, testCase := range cases {
		rec := write(t, srv, testCase.method, testCase.target, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405",
				testCase.method, testCase.target, rec.Code)
		}
	}
}

// ------------------------------------------------------------- disabled --

// TestEveryRollbackRouteIsUnavailableWhenTheCapabilityIsAbsent.
//
// A deployment built without the rollback capability answers 503 on every
// route rather than 404. The distinction matters: 404 would say the feature
// does not exist, and an operator would go looking for the wrong thing.
func TestEveryRollbackRouteIsUnavailableWhenTheCapabilityIsAbsent(t *testing.T) {
	srv := newRollbackServer(t, nil)

	reads := []string{
		APIPrefix + "/rollbacks",
		APIPrefix + "/rollbacks/" + sampleRollbackID,
	}
	for _, target := range reads {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s: status = %d, want 503", target, rec.Code)
		}
	}

	writes := []struct {
		target string
		body   string
	}{
		{APIPrefix + "/rollbacks", validRollbackBody},
		{APIPrefix + "/rollbacks/" + sampleRollbackID + "/cancel", ""},
	}
	for _, testCase := range writes {
		rec := write(t, srv, http.MethodPost, testCase.target, testCase.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s: status = %d, want 503", testCase.target, rec.Code)
		}
	}
}

// -------------------------------------------------------------- audit --

// TestARollbackRequestIsAttributedToTheAccountThatMadeIt.
func TestARollbackRequestIsAttributedToTheAccountThatMadeIt(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv, _, audit := asRole(Options{
		Health:         &fakeHealth{},
		Rollbacks:      rollbacks,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Assets:         testAssets(),
	}, domain.RoleOperator)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", validRollbackBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := audit.recorded(domain.AuditRollbackRequested)
	if len(events) != 1 {
		t.Fatalf("%d rollback.requested events, want 1", len(events))
	}
	event := events[0]
	if event.ActorUsername == "" || event.ActorUserID == "" {
		t.Errorf("the audit record does not name the requester: %+v", event)
	}
	if event.TargetType != domain.AuditTargetRollback {
		t.Errorf("targetType = %q", event.TargetType)
	}
	if event.TargetID != sampleRollbackID {
		t.Errorf("targetId = %q", event.TargetID)
	}

	// And the requester reaches the service, so the OUTCOME can be attributed
	// minutes later by a worker with no request and no session.
	if rollbacks.requests[0].RequestedBy.Username == "" {
		t.Error("the requester was not carried onto the rollback record")
	}
}

// TestNoRollbackAuditRecordCarriesTheRequestBody.
func TestNoRollbackAuditRecordCarriesTheRequestBody(t *testing.T) {
	rollbacks := &fakeRollbacks{created: sampleRollback(), enabled: true}
	srv, _, audit := asRole(Options{
		Health:         &fakeHealth{},
		Rollbacks:      rollbacks,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Assets:         testAssets(),
	}, domain.RoleOperator)

	body := `{"executionId":"` + sampleExecutionID + `","requestKey":"secret-key-value"}`
	if rec := write(t, srv, http.MethodPost, APIPrefix+"/rollbacks", body); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	encoded, err := json.Marshal(audit.recorded(domain.AuditRollbackRequested))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"secret-key-value", testSessionToken, testCSRFToken} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the audit record contains %q", forbidden)
		}
	}
}
