package api

import (
	"context"
	"encoding/json"
	"errors"
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

// Image acquisition API tests.
//
// These are the only endpoints in HarborMaster that can change the Docker host,
// so the properties worth defending are about what a caller CANNOT reach:
//
//   - No request body field names an image. There is no registry, repository,
//     digest, tag, or pull option a caller can supply -- the target comes from
//     the plan.
//   - There is no route that applies an image, changes a container, or removes
//     one, and no unknown field can smuggle a parameter in.
//   - Every write is behind the same guard as the rest of the API: fetch
//     metadata, JSON media type, size limit, rate limit.

const sampleAcquisitionID = "acq_00112233445566778899"

// ------------------------------------------------------------------ fake --

type fakeAcquisitions struct {
	mu sync.Mutex

	items   []domain.Acquisition
	events  []domain.AcquisitionEvent
	summary domain.AcquisitionSummary

	created  domain.Acquisition
	requests []service.AcquisitionRequest

	requestErr error
	cancelErr  error
	listErr    error
	getErr     error

	cancelled  []string
	enabled    bool
	lastFilter store.AcquisitionFilter
}

func (f *fakeAcquisitions) Request(
	_ context.Context, request service.AcquisitionRequest,
) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.requestErr != nil {
		return domain.Acquisition{}, f.requestErr
	}
	return f.created, nil
}

func (f *fakeAcquisitions) Cancel(_ context.Context, id string) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	if f.cancelErr != nil {
		return domain.Acquisition{}, f.cancelErr
	}
	cancelled := f.created
	cancelled.State = domain.AcquisitionCancelled
	return cancelled, nil
}

func (f *fakeAcquisitions) Get(
	_ context.Context, id string,
) (domain.Acquisition, []domain.AcquisitionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return domain.Acquisition{}, nil, f.getErr
	}
	for _, item := range f.items {
		if item.AcquisitionID == id {
			return item, f.events, nil
		}
	}
	return domain.Acquisition{}, nil, store.ErrNotFound
}

func (f *fakeAcquisitions) List(
	_ context.Context, filter store.AcquisitionFilter,
) ([]domain.Acquisition, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.items, len(f.items), nil
}

func (f *fakeAcquisitions) Summary(context.Context) (domain.AcquisitionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, nil
}

func (f *fakeAcquisitions) Eligible(
	context.Context, string,
) (domain.AcquisitionTarget, domain.AcquisitionRefusal, error) {
	return domain.AcquisitionTarget{}, domain.AcquisitionRefusalNone, nil
}

func (f *fakeAcquisitions) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

func (f *fakeAcquisitions) filter() store.AcquisitionFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

func (f *fakeAcquisitions) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ------------------------------------------------------------- harnesses --

func sampleAcquisition() domain.Acquisition {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return domain.Acquisition{
		AcquisitionID: sampleAcquisitionID,
		PlanID:        samplePlanID,
		ContainerID:   "container-a",
		ContainerName: "web",
		Target: domain.AcquisitionTarget{
			Registry:   "docker.io",
			Repository: "library/nginx",
			Digest:     "sha256:" + strings.Repeat("a", 64),
			Reference:  "nginx:1.27.1",
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State:       domain.AcquisitionQueued,
		RequestedAt: at,
		ExpiresAt:   at.Add(time.Hour),
	}
}

func newAcquisitionServer(t *testing.T, acquisitions *fakeAcquisitions) *Server {
	t.Helper()

	var capability AcquisitionService
	if acquisitions != nil {
		capability = acquisitions
	}

	return newAuthedServer(Options{
		Health:         &fakeHealth{},
		Acquisitions:   capability,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})
}

const validAcquisitionBody = `{"planId":"` + samplePlanID + `"}`

// ----------------------------------------------------------------- reads --

func TestAcquisitionsListWithTheirSummary(t *testing.T) {
	acquisitions := &fakeAcquisitions{
		items: []domain.Acquisition{sampleAcquisition()},
		summary: domain.AcquisitionSummary{
			Total: 5, Active: 1, Succeeded: 3, Failed: 1, Enabled: true,
		},
		enabled: true,
	}
	srv := newAcquisitionServer(t, acquisitions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response acquisitionListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].AcquisitionID != sampleAcquisitionID {
		t.Errorf("items = %+v", response.Items)
	}
	if response.Summary.Total != 5 || !response.Summary.Enabled {
		t.Errorf("summary = %+v", response.Summary)
	}
	// The target reaches the client digest-pinned, because a confirmation that
	// showed a tag would be asking someone to approve a name.
	if !strings.HasPrefix(response.Items[0].Target.Digest, "sha256:") {
		t.Errorf("target = %+v", response.Items[0].Target)
	}
}

func TestAcquisitionDetailCarriesTheAuditTrail(t *testing.T) {
	acquisitions := &fakeAcquisitions{
		items: []domain.Acquisition{sampleAcquisition()},
		events: []domain.AcquisitionEvent{
			{State: domain.AcquisitionQueued, Detail: "requested by an operator"},
			{State: domain.AcquisitionPulling, Detail: "downloading"},
		},
		enabled: true,
	}
	srv := newAcquisitionServer(t, acquisitions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions/"+sampleAcquisitionID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response acquisitionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Acquisition.AcquisitionID != sampleAcquisitionID {
		t.Errorf("acquisition = %+v", response.Acquisition)
	}
	if len(response.Events) != 2 {
		t.Errorf("events = %d, want 2", len(response.Events))
	}
}

// An acquisition id has exactly one shape, so anything else is refused BEFORE
// it reaches a query.
func TestAMalformedAcquisitionIDIsRefused(t *testing.T) {
	acquisitions := &fakeAcquisitions{items: []domain.Acquisition{sampleAcquisition()}, enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	for _, id := range []string{
		"acq_", "acq_zzzz", "acq_00112233445566778899x", "plan_00112233445566778899",
		"acq_" + strings.Repeat("a", 500),
		url.PathEscape("../../etc/passwd"),
		url.PathEscape("acq_001122' OR '1'='1"),
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400", id, rec.Code)
		}
	}

	rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions/acq_ffffffffffffffffffff", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAcquisitionFiltersRefuseValuesOutsideTheirVocabulary(t *testing.T) {
	srv := newAcquisitionServer(t, &fakeAcquisitions{enabled: true})

	// An EMPTY value is deliberately absent from this table. Across every filter
	// in this API an empty parameter means "not specified" and is skipped, and
	// that convention is worth more than a special case here.
	for _, pair := range [][2]string{
		{"state", "downloading"},
		{"state", "queued'; DROP TABLE acquisitions--"},
		{"failure", "somethingElse"},
		{"sort", "target_digest"},
		{"sort", "requested_at; DROP TABLE acquisitions"},
		{"order", "sideways"},
		{"activeOnly", "perhaps"},
		{"pageSize", "0"},
		{"pageSize", "100000"},
	} {
		query := url.Values{pair[0]: []string{pair[1]}}.Encode()

		rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions?"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s=%q: status = %d, want 400 (%s)",
				pair[0], pair[1], rec.Code, rec.Body.String())
		}
	}
}

func TestAcquisitionFiltersReachTheRepository(t *testing.T) {
	acquisitions := &fakeAcquisitions{enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	target := APIPrefix + "/acquisitions?state=pulling&state=queued" +
		"&failure=digestMismatch&activeOnly=true&sort=state&order=asc&pageSize=10"
	if rec := do(t, srv, http.MethodGet, target, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := acquisitions.filter()
	if len(filter.States) != 2 || filter.States[0] != domain.AcquisitionPulling {
		t.Errorf("states = %v", filter.States)
	}
	if len(filter.Failures) != 1 || filter.Failures[0] != domain.AcquisitionFailureDigestMismatch {
		t.Errorf("failures = %v", filter.Failures)
	}
	if !filter.ActiveOnly || filter.Sort != "state" || !filter.Ascending {
		t.Errorf("filter = %+v", filter)
	}
}

// ---------------------------------------------------------------- create --

func TestRequestingAnAcquisitionReturnsAccepted(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", validAcquisitionBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	var acquisition domain.Acquisition
	if err := json.Unmarshal(rec.Body.Bytes(), &acquisition); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if acquisition.AcquisitionID != sampleAcquisitionID {
		t.Errorf("acquisition = %+v", acquisition)
	}
	if acquisitions.requestCount() != 1 {
		t.Errorf("the service was asked %d times, want 1", acquisitions.requestCount())
	}
}

// THE property of this endpoint: a caller cannot name an image. Every attempt
// to smuggle a target in is refused by strict decoding.
func TestNoRequestFieldCanNameAnImage(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	for _, body := range []string{
		`{"planId":"` + samplePlanID + `","registry":"evil.example"}`,
		`{"planId":"` + samplePlanID + `","repository":"evil/payload"}`,
		`{"planId":"` + samplePlanID + `","digest":"sha256:` + strings.Repeat("b", 64) + `"}`,
		`{"planId":"` + samplePlanID + `","image":"evil.example/payload:latest"}`,
		`{"planId":"` + samplePlanID + `","reference":"evil.example/payload"}`,
		`{"planId":"` + samplePlanID + `","tag":"latest"}`,
		`{"planId":"` + samplePlanID + `","platform":"linux/arm64"}`,
		`{"planId":"` + samplePlanID + `","pullOptions":{"all":true}}`,
		`{"planId":"` + samplePlanID + `","registryAuth":"aGVsbG8="}`,
	} {
		rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 -- an unknown field must be refused, "+
				"not ignored", body, rec.Code)
		}
	}

	if acquisitions.requestCount() != 0 {
		t.Errorf("%d requests reached the service despite being malformed",
			acquisitions.requestCount())
	}
}

func TestAMalformedPlanIDIsRefusedBeforeTheService(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	for _, body := range []string{
		`{}`,
		`{"planId":""}`,
		`{"planId":"nonsense"}`,
		`{"planId":"acq_00112233445566778899"}`,
		`{"planId":"plan_00112233445566778899x"}`,
		`{"planId":"../../etc/passwd"}`,
		`{"planId":"plan_001122' OR '1'='1"}`,
	} {
		rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}

	if acquisitions.requestCount() != 0 {
		t.Error("a malformed plan id reached the service")
	}
}

func TestAnOversizedRequestKeyIsRefused(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	for _, key := range []string{
		strings.Repeat("k", 200),
		"key with spaces",
		"key\nwith\nnewlines",
		"key\x00null",
	} {
		body, err := json.Marshal(acquisitionRequestBody{PlanID: samplePlanID, RequestKey: key})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("key %q: status = %d, want 400", key, rec.Code)
		}
	}

	// A reasonable key is accepted.
	body := `{"planId":"` + samplePlanID + `","requestKey":"operator-click-1"}`
	if rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", body); rec.Code != http.StatusAccepted {
		t.Errorf("a valid key was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// A refusal reports WHICH check said no, so a client can tell "the digest
// moved" from "the daemon is down" without parsing prose.
func TestARefusalReportsWhichCheckSaidNo(t *testing.T) {
	for refusal, wantStatus := range map[domain.AcquisitionRefusal]int{
		domain.AcquisitionRefusalDigestChanged:     http.StatusConflict,
		domain.AcquisitionRefusalPlanStale:         http.StatusConflict,
		domain.AcquisitionRefusalDuplicate:         http.StatusConflict,
		domain.AcquisitionRefusalDockerUnavailable: http.StatusConflict,
		domain.AcquisitionRefusalPlanMissing:       http.StatusNotFound,
		domain.AcquisitionRefusalContainerMissing:  http.StatusNotFound,
		domain.AcquisitionRefusalLimit:             http.StatusTooManyRequests,
		domain.AcquisitionRefusalDisabled:          http.StatusServiceUnavailable,
	} {
		t.Run(string(refusal), func(t *testing.T) {
			acquisitions := &fakeAcquisitions{
				requestErr: service.ErrAcquisitionRefused{Refusal: refusal},
				enabled:    true,
			}
			srv := newAcquisitionServer(t, acquisitions)

			rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", validAcquisitionBody)
			if rec.Code != wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body.String())
			}

			var response acquisitionRefusalResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if response.Refusal != refusal {
				t.Errorf("refusal = %q, want %q", response.Refusal, refusal)
			}
			if response.Error.Message == "" {
				t.Error("a refusal should explain itself")
			}
		})
	}
}

// ---------------------------------------------------------------- cancel --

func TestCancellingAnAcquisition(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	rec := write(t, srv, http.MethodPost,
		APIPrefix+"/acquisitions/"+sampleAcquisitionID+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var acquisition domain.Acquisition
	if err := json.Unmarshal(rec.Body.Bytes(), &acquisition); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if acquisition.State != domain.AcquisitionCancelled {
		t.Errorf("state = %q, want cancelled", acquisition.State)
	}

	acquisitions.mu.Lock()
	cancelled := append([]string(nil), acquisitions.cancelled...)
	acquisitions.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != sampleAcquisitionID {
		t.Errorf("cancelled = %v", cancelled)
	}
}

func TestCancellingSomethingThatHasFinishedIsAConflict(t *testing.T) {
	acquisitions := &fakeAcquisitions{
		cancelErr: service.ErrAcquisitionRefused{Refusal: domain.AcquisitionRefusalNone},
		enabled:   true,
	}
	srv := newAcquisitionServer(t, acquisitions)

	rec := write(t, srv, http.MethodPost,
		APIPrefix+"/acquisitions/"+sampleAcquisitionID+"/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestCancellingAnUnknownAcquisitionIsNotFound(t *testing.T) {
	acquisitions := &fakeAcquisitions{cancelErr: store.ErrNotFound, enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	rec := write(t, srv, http.MethodPost,
		APIPrefix+"/acquisitions/"+sampleAcquisitionID+"/cancel", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ----------------------------------------------------------- write guards --

// Both writes are behind the same guard as every other state-changing endpoint.
// A cross-site form post must not be able to start a download.
func TestAcquisitionWritesRefuseCrossSiteRequests(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	for _, target := range []string{
		APIPrefix + "/acquisitions",
		APIPrefix + "/acquisitions/" + sampleAcquisitionID + "/cancel",
	} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(validAcquisitionBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authed(req))

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", target, rec.Code)
		}
	}

	if acquisitions.requestCount() != 0 {
		t.Error("a cross-site request reached the service")
	}
}

// A body without a JSON media type is refused, which is also what forces a CORS
// preflight for any cross-origin caller.
func TestAcquisitionCreateRequiresJSON(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/acquisitions",
		strings.NewReader(validAcquisitionBody))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
	if acquisitions.requestCount() != 0 {
		t.Error("a non-JSON request reached the service")
	}
}

func TestAnOversizedAcquisitionBodyIsRefused(t *testing.T) {
	acquisitions := &fakeAcquisitions{created: sampleAcquisition(), enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	body := `{"planId":"` + samplePlanID + `","requestKey":"` + strings.Repeat("k", 100_000) + `"}`
	rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", body)

	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the body to be refused", rec.Code)
	}
	if acquisitions.requestCount() != 0 {
		t.Error("an oversized body reached the service")
	}
}

// ------------------------------------------------------------ method set --

// There is no route that applies an image, changes a container, or removes an
// acquisition. This is the list that would have to change for one to appear.
func TestAcquisitionsExposeNoOtherMutation(t *testing.T) {
	acquisitions := &fakeAcquisitions{items: []domain.Acquisition{sampleAcquisition()}, enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	for _, tc := range []struct{ method, target string }{
		{http.MethodPut, APIPrefix + "/acquisitions"},
		{http.MethodPatch, APIPrefix + "/acquisitions"},
		{http.MethodDelete, APIPrefix + "/acquisitions"},
		{http.MethodPut, APIPrefix + "/acquisitions/" + sampleAcquisitionID},
		{http.MethodPatch, APIPrefix + "/acquisitions/" + sampleAcquisitionID},
		{http.MethodDelete, APIPrefix + "/acquisitions/" + sampleAcquisitionID},
		{http.MethodGet, APIPrefix + "/acquisitions/" + sampleAcquisitionID + "/cancel"},
		{http.MethodDelete, APIPrefix + "/acquisitions/" + sampleAcquisitionID + "/cancel"},
	} {
		rec := write(t, srv, tc.method, tc.target, `{}`)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", tc.method, tc.target, rec.Code)
		}
	}
}

// --------------------------------------------------------------- disabled --

// A deployment that has not opted in answers 503 rather than a broken route.
func TestAcquisitionEndpointsReportWhenNotConfigured(t *testing.T) {
	srv := newAcquisitionServer(t, nil)

	for _, target := range []string{
		APIPrefix + "/acquisitions",
		APIPrefix + "/acquisitions/" + sampleAcquisitionID,
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", target, rec.Code)
		}
	}

	rec := write(t, srv, http.MethodPost, APIPrefix+"/acquisitions", validAcquisitionBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create: status = %d, want 503", rec.Code)
	}

	rec = write(t, srv, http.MethodPost,
		APIPrefix+"/acquisitions/"+sampleAcquisitionID+"/cancel", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("cancel: status = %d, want 503", rec.Code)
	}
}

// ---------------------------------------------------------------- errors --

// A repository failure is an internal error with no detail. The message a
// caller sees must not describe the database or the daemon.
func TestAnAcquisitionFailureLeaksNothing(t *testing.T) {
	acquisitions := &fakeAcquisitions{
		listErr: errors.New("no such table: acquisitions"),
		enabled: true,
	}
	srv := newAcquisitionServer(t, acquisitions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"table", "acquisitions.db", "sqlite"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the response described the storage layer: %s", body)
		}
	}
}

// No acquisition field can carry a credential, and none does. This pins the
// boundary anyway, because a future field would show up here.
func TestNoAcquisitionFieldCarriesASecret(t *testing.T) {
	acquisition := sampleAcquisition()
	acquisition.Message = "the transfer did not complete"

	acquisitions := &fakeAcquisitions{items: []domain.Acquisition{acquisition}, enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions", nil)
	body := strings.ToLower(rec.Body.String())

	for _, forbidden := range []string{
		"password", "secret", "bearer", "authorization", "credential",
		"apikey", "api_key", "registryauth",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the acquisition response mentions %q: %s", forbidden, rec.Body.String())
		}
	}
}

// Text on an acquisition originates in HarborMaster's own vocabulary, but the
// container NAME comes from Docker. It is rendered as data, never as markup.
func TestAcquisitionTextIsEscapedInTheResponse(t *testing.T) {
	acquisition := sampleAcquisition()
	acquisition.ContainerName = `<script>alert(1)</script>`
	acquisition.Progress = `</script><img src=x onerror=alert(1)>`

	acquisitions := &fakeAcquisitions{items: []domain.Acquisition{acquisition}, enabled: true}
	srv := newAcquisitionServer(t, acquisitions)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/acquisitions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("unescaped markup reached the response: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content type = %q", got)
	}
}
