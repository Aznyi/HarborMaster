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

// Change plan API tests.
//
// The property worth defending above all others: NOTHING HERE APPLIES A PLAN.
// The write endpoint generates HarborMaster's own analysis of HarborMaster's own
// database; there is no route that pulls, recreates, restores, or schedules any
// of that, and the tests below pin both the absence of such a route and the
// immutability of what is stored.
//
// The second property: every filter is a CLOSED VOCABULARY. Nothing a caller
// sends becomes SQL text, and anything outside the vocabulary is a 400 rather
// than a silently ignored parameter.

// ------------------------------------------------------------------ fakes --

type fakePlans struct {
	mu sync.Mutex

	plans   []domain.ChangePlan
	history []domain.ChangePlan
	summary domain.ChangePlanSummary

	lastFilter store.PlanFilter
	lastPage   store.Page

	listErr    error
	summaryErr error
	currentErr error
}

func (f *fakePlans) List(_ context.Context, filter store.PlanFilter) ([]domain.ChangePlan, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.plans, len(f.plans), nil
}

func (f *fakePlans) Get(_ context.Context, planID string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, plan := range f.plans {
		if plan.PlanID == planID {
			return plan, nil
		}
	}
	return domain.ChangePlan{}, store.ErrNotFound
}

func (f *fakePlans) Current(_ context.Context, containerID string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentErr != nil {
		return domain.ChangePlan{}, f.currentErr
	}
	for _, plan := range f.plans {
		if plan.ContainerID == containerID {
			return plan, nil
		}
	}
	return domain.ChangePlan{}, store.ErrNotFound
}

func (f *fakePlans) History(
	_ context.Context, _ string, page store.Page,
) ([]domain.ChangePlan, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPage = page
	return f.history, len(f.history), nil
}

func (f *fakePlans) Summary(context.Context) (domain.ChangePlanSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.summaryErr != nil {
		return domain.ChangePlanSummary{}, f.summaryErr
	}
	return f.summary, nil
}

func (f *fakePlans) filter() store.PlanFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

// fakePlanner records generation requests without doing any work.
type fakePlanner struct {
	mu       sync.Mutex
	requests int
}

func (f *fakePlanner) RequestGeneration() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
}

func (f *fakePlanner) Status() domain.PlannerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.PlannerStatus{
		Enabled:        true,
		PlannerVersion: domain.PlannerVersion,
		Pending:        f.requests > 0,
	}
}

func (f *fakePlanner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// ------------------------------------------------------------- harnesses --

const samplePlanID = "plan_00112233445566778899"

func samplePlan() domain.ChangePlan {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return domain.ChangePlan{
		ID:            1,
		PlanID:        samplePlanID,
		ContainerID:   "container-a",
		ContainerName: "web",

		CurrentImage:   "nginx:1.27.0",
		ProposedImage:  "nginx:1.27.1",
		CurrentDigest:  "sha256:" + strings.Repeat("a", 64),
		ProposedDigest: "sha256:" + strings.Repeat("b", 64),
		UpdateType:     domain.UpdatePatch,

		SnapshotID:        7,
		SnapshotAvailable: true,
		RestoreReadiness:  domain.ReadinessReady,
		RegistryStatus:    domain.CheckOK,

		Risk: domain.RiskAssessment{
			Score:          5,
			Band:           domain.RiskVeryLow,
			Recommendation: domain.RecommendProceed,
			Summary:        "Nothing in the available evidence argues against this change.",
			Factors: []domain.RiskFactor{{
				Rule: domain.RuleUpdateClassification, Points: 5,
				Severity: domain.FactorInfo, Detail: "this is a patch version change",
			}},
		},

		PlanVersion:    domain.PlanSchemaVersion,
		PlannerVersion: domain.PlannerVersion,
		InputDigest:    strings.Repeat("c", 64),
		GeneratedAt:    at,
	}
}

func newPlanServer(t *testing.T, plans *fakePlans, planner *fakePlanner) *Server {
	t.Helper()

	// A nil interface value must stay nil, so the "not configured" path is
	// reachable: a typed nil pointer in an interface is not nil.
	var (
		reader    PlanReader
		generator PlanGenerator
	)
	if plans != nil {
		reader = plans
	}
	if planner != nil {
		generator = planner
	}

	return newAuthedServer(Options{
		Health:         &fakeHealth{},
		Plans:          reader,
		Planner:        generator,
		Containers:     fakePolicyContainerReader{},
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})
}

// ----------------------------------------------------------------- reads --

func TestPlansListWithTheirSummary(t *testing.T) {
	plans := &fakePlans{
		plans: []domain.ChangePlan{samplePlan()},
		summary: domain.ChangePlanSummary{
			Plans: 12, Containers: 12,
			Actionable: 7, NeedsReview: 3, Blocked: 1, Undetermined: 1,
			ByBand:         map[domain.RiskBand]int{domain.RiskHigh: 3},
			PlannerVersion: domain.PlannerVersion,
		},
	}
	srv := newPlanServer(t, plans, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var response planListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].PlanID != samplePlanID {
		t.Errorf("items = %+v", response.Items)
	}
	// The summary travels WITH the list so a dashboard renders in one request.
	if response.Summary.Plans != 12 || response.Summary.Blocked != 1 {
		t.Errorf("summary = %+v", response.Summary)
	}
	if response.Summary.Undetermined != 1 {
		t.Error("undetermined must be reported beside the rest, not absorbed")
	}
	if response.Pagination.TotalItems != 1 {
		t.Errorf("pagination = %+v", response.Pagination)
	}

	// The reasoning reaches the client: a verdict without its factors is a
	// number an operator has to take on trust.
	if len(response.Items[0].Risk.Factors) != 1 {
		t.Errorf("factors did not reach the response: %+v", response.Items[0].Risk)
	}
}

// The listing defaults to CURRENT plans. Superseded ones describe a world that
// has moved on, and listing them beside current ones would double-count.
func TestThePlanListingDefaultsToCurrentPlans(t *testing.T) {
	plans := &fakePlans{}
	srv := newPlanServer(t, plans, &fakePlanner{})

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/plans", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !plans.filter().CurrentOnly {
		t.Error("the default listing should be current plans only")
	}

	if rec := do(t, srv, http.MethodGet,
		APIPrefix+"/plans?currentOnly=false", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if plans.filter().CurrentOnly {
		t.Error("currentOnly=false should include superseded plans")
	}
}

func TestPlanFiltersReachTheRepository(t *testing.T) {
	plans := &fakePlans{}
	srv := newPlanServer(t, plans, &fakePlanner{})

	target := APIPrefix + "/plans?band=high&band=critical" +
		"&recommendation=notRecommended&update=major&minRisk=40&sort=risk&order=asc&pageSize=25"
	if rec := do(t, srv, http.MethodGet, target, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := plans.filter()
	if len(filter.Bands) != 2 || filter.Bands[0] != domain.RiskHigh {
		t.Errorf("bands = %v", filter.Bands)
	}
	if len(filter.Recommendations) != 1 || filter.Recommendations[0] != domain.RecommendAgainst {
		t.Errorf("recommendations = %v", filter.Recommendations)
	}
	if len(filter.Updates) != 1 || filter.Updates[0] != domain.UpdateMajor {
		t.Errorf("updates = %v", filter.Updates)
	}
	if filter.MinRisk != 40 || filter.Sort != "risk" || !filter.Ascending {
		t.Errorf("filter = %+v", filter)
	}
	if filter.Page.Limit != 25 {
		t.Errorf("page = %+v", filter.Page)
	}
}

// Every filter is a closed vocabulary. A value outside it is refused rather
// than ignored: silently dropping it would answer a different question than the
// one asked.
func TestPlanFiltersRefuseValuesOutsideTheirVocabulary(t *testing.T) {
	srv := newPlanServer(t, &fakePlans{}, &fakePlanner{})

	// Encoded rather than concatenated, so the injection attempts arrive as
	// PARAMETER VALUES -- which is the layer under test. A raw space would be
	// refused by the request parser and prove nothing about the handler.
	for _, pair := range [][2]string{
		{"band", "catastrophic"},
		{"band", "high'; DROP TABLE change_plans--"},
		{"recommendation", "justDoIt"},
		{"update", "sideways"},
		{"sort", "risk_score"},
		{"sort", "generated_at; DROP TABLE change_plans"},
		{"sort", "(SELECT 1)"},
		{"sort", "p.risk_score DESC, (SELECT 1)"},
		{"order", "sideways"},
		{"currentOnly", "perhaps"},
		{"minRisk", "-1"},
		{"minRisk", "101"},
		{"minRisk", "NaN"},
		{"pageSize", "0"},
		{"pageSize", "100000"},
		{"page", "-1"},
	} {
		query := url.Values{pair[0]: []string{pair[1]}}.Encode()

		rec := do(t, srv, http.MethodGet, APIPrefix+"/plans?"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s=%q: status = %d, want 400 (%s)",
				pair[0], pair[1], rec.Code, rec.Body.String())
		}
	}
}

func TestPlanDetailReturnsOnePlan(t *testing.T) {
	plans := &fakePlans{plans: []domain.ChangePlan{samplePlan()}}
	srv := newPlanServer(t, plans, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/"+samplePlanID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var plan domain.ChangePlan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plan.PlanID != samplePlanID || plan.Risk.Summary == "" {
		t.Errorf("plan = %+v", plan)
	}
}

// A plan id has exactly one shape, so anything else is refused BEFORE it
// reaches a query. Traversal, injection and oversized input are all the same
// answer: not well formed.
func TestAMalformedPlanIDIsRefused(t *testing.T) {
	plans := &fakePlans{plans: []domain.ChangePlan{samplePlan()}}
	srv := newPlanServer(t, plans, &fakePlanner{})

	for _, id := range []string{
		"plan_",
		"plan_zzzz",
		"plan_00112233445566778899x",
		"plan_0011223344556677889",
		"pol_00112233445566778899",
		"plan_" + strings.Repeat("a", 500),
		url.PathEscape("../../etc/passwd"),
		url.PathEscape("plan_001122' OR '1'='1"),
		url.PathEscape("plan_00112233445566778899 extra"),
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400", id, rec.Code)
		}
	}

	// A well-formed id that does not exist is a 404, which is a different
	// answer and must stay one.
	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/plan_ffffffffffffffffffff", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	// Surrounding whitespace is NORMALISED AWAY rather than carried into the
	// lookup, matching every other id route. What must never happen is the
	// padded form reaching the repository as a distinct identifier.
	padded := do(t, srv, http.MethodGet,
		APIPrefix+"/plans/"+url.PathEscape(" "+samplePlanID+"\n"), nil)
	if padded.Code != http.StatusOK {
		t.Fatalf("a padded id should trim to the real one, got %d", padded.Code)
	}
	var plan domain.ChangePlan
	if err := json.Unmarshal(padded.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plan.PlanID != samplePlanID {
		t.Errorf("padded lookup returned %q", plan.PlanID)
	}
}

// A traversal attempt must never reach the handler as a path, and a redirect
// must not preserve the traversal segments.
func TestPathTraversalNeverReachesThePlanHandler(t *testing.T) {
	srv := newPlanServer(t, &fakePlans{}, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/../../etc/passwd", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("traversal returned 200: %s", rec.Body.String())
	}
	if location := rec.Header().Get("Location"); strings.Contains(location, "..") {
		t.Errorf("a redirect preserved the traversal: %q", location)
	}
}

// The per-container view is the reasoning timeline: the current assessment and
// how HarborMaster arrived at it.
func TestTheContainerViewCarriesCurrentAndHistory(t *testing.T) {
	older := samplePlan()
	older.PlanID = "plan_aabbccddeeff00112233"
	older.Superseded = true

	plans := &fakePlans{
		plans:   []domain.ChangePlan{samplePlan()},
		history: []domain.ChangePlan{samplePlan(), older},
	}
	srv := newPlanServer(t, plans, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/container/container-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response planContainerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.ContainerID != "container-a" {
		t.Errorf("containerId = %q", response.ContainerID)
	}
	if response.Current == nil || response.Current.PlanID != samplePlanID {
		t.Errorf("current = %+v", response.Current)
	}
	if len(response.History) != 2 {
		t.Errorf("history = %d entries, want 2", len(response.History))
	}
	if !response.History[1].Superseded {
		t.Error("the older entry should report itself superseded")
	}
}

// A container with no plan is ABSENT, not invented. "Nothing proposed" and
// "assessed as safe" are different states, and the client must be able to tell
// them apart.
func TestAContainerWithNoPlanReportsNoneRatherThanSafe(t *testing.T) {
	srv := newPlanServer(t, &fakePlans{}, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/container/container-z", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["current"]; present {
		t.Errorf("a container with no plan should omit current, got %s", rec.Body.String())
	}
	if _, present := raw["history"]; !present {
		t.Error("the history field should always be present")
	}
}

func TestAnUnknownContainerIsNotFound(t *testing.T) {
	srv := newPlanServer(t, &fakePlans{}, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/container/missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestThePlanHistoryIsPaginated(t *testing.T) {
	plans := &fakePlans{history: []domain.ChangePlan{samplePlan()}}
	srv := newPlanServer(t, plans, &fakePlanner{})

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/plans/container/container-a?page=3&pageSize=10", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	plans.mu.Lock()
	page := plans.lastPage
	plans.mu.Unlock()

	if page.Limit != 10 || page.Offset != 20 {
		t.Errorf("page = %+v, want limit 10 offset 20", page)
	}
}

// --------------------------------------------------------------- generate --

func TestGenerateSchedulesAPassAndReturnsAccepted(t *testing.T) {
	planner := &fakePlanner{}
	srv := newPlanServer(t, &fakePlans{}, planner)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/plans/generate", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if planner.count() != 1 {
		t.Errorf("requested %d passes, want 1", planner.count())
	}

	var response planGenerateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.Requested {
		t.Error("the response should acknowledge the request")
	}
	if response.Planner.PlannerVersion != domain.PlannerVersion {
		t.Errorf("planner status = %+v", response.Planner)
	}
}

// The request is a fetch-metadata-guarded write. A cross-site form post must
// not be able to schedule work.
func TestGenerateRefusesACrossSiteRequest(t *testing.T) {
	planner := &fakePlanner{}
	srv := newPlanServer(t, &fakePlans{}, planner)

	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/plans/generate", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if planner.count() != 0 {
		t.Error("a cross-site request scheduled work")
	}
}

// Repeated requests are coalesced by the planner rather than queued. The
// endpoint's job is to ask; the planner's job is to run at most one pass.
func TestRepeatedGenerateRequestsAreCheap(t *testing.T) {
	planner := &fakePlanner{}
	srv := newPlanServer(t, &fakePlans{}, planner)

	for attempt := 0; attempt < 20; attempt++ {
		rec := write(t, srv, http.MethodPost, APIPrefix+"/plans/generate", "")
		if rec.Code != http.StatusAccepted && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status = %d", attempt, rec.Code)
		}
	}
}

// ------------------------------------------------------------ method set --

// There is no PATCH, PUT or DELETE. A plan records what was believed at one
// moment; an endpoint that edited one would destroy the property that makes
// plans worth keeping.
func TestPlansExposeNoMutationRoutes(t *testing.T) {
	srv := newPlanServer(t, &fakePlans{plans: []domain.ChangePlan{samplePlan()}}, &fakePlanner{})

	for _, tc := range []struct {
		method, target string
	}{
		{http.MethodPost, APIPrefix + "/plans"},
		{http.MethodPut, APIPrefix + "/plans"},
		{http.MethodDelete, APIPrefix + "/plans"},
		{http.MethodPatch, APIPrefix + "/plans/" + samplePlanID},
		{http.MethodPut, APIPrefix + "/plans/" + samplePlanID},
		{http.MethodDelete, APIPrefix + "/plans/" + samplePlanID},
		{http.MethodPost, APIPrefix + "/plans/container/container-a"},
		{http.MethodDelete, APIPrefix + "/plans/container/container-a"},
	} {
		rec := write(t, srv, tc.method, tc.target, `{}`)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", tc.method, tc.target, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "" && strings.Contains(allow, tc.method) {
			t.Errorf("%s %s: Allow advertises the refused method: %q", tc.method, tc.target, allow)
		}
	}
}

// The generate route answers POST only. A GET that scheduled work would be
// reachable from an image tag.
func TestGenerateIsPostOnly(t *testing.T) {
	planner := &fakePlanner{}
	srv := newPlanServer(t, &fakePlans{}, planner)

	// A bare GET to the generate path falls through to the {id} route, which
	// refuses it as a malformed plan id rather than scheduling anything. Either
	// answer is acceptable; scheduling is not.
	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans/generate", nil)
	if rec.Code == http.StatusAccepted {
		t.Errorf("a GET scheduled a pass: %s", rec.Body.String())
	}
	if planner.count() != 0 {
		t.Error("a GET scheduled work")
	}
}

// --------------------------------------------------------------- disabled --

// A deployment with planning switched off answers 503 rather than a broken
// route or an empty list that looks like "no risk".
func TestPlanEndpointsReportWhenPlanningIsNotConfigured(t *testing.T) {
	srv := newPlanServer(t, nil, nil)

	for _, target := range []string{
		APIPrefix + "/plans",
		APIPrefix + "/plans/" + samplePlanID,
		APIPrefix + "/plans/container/container-a",
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", target, rec.Code)
		}
	}

	rec := write(t, srv, http.MethodPost, APIPrefix+"/plans/generate", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("generate: status = %d, want 503", rec.Code)
	}
}

// ---------------------------------------------------------------- errors --

// A repository failure is an internal error with no detail. The message a
// caller sees must not describe the database.
func TestARepositoryFailureLeaksNothing(t *testing.T) {
	for name, plans := range map[string]*fakePlans{
		"list":    {listErr: fmt.Errorf("no such table: change_plans")},
		"summary": {summaryErr: fmt.Errorf("no such column: risk_band")},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newPlanServer(t, plans, &fakePlanner{})

			rec := do(t, srv, http.MethodGet, APIPrefix+"/plans", nil)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, "table") || strings.Contains(body, "column") {
				t.Errorf("the response described the database: %s", body)
			}
		})
	}
}

// --------------------------------------------------------------- rendering --

// A plan is rendered as data, never as markup. Every string on it originates in
// HarborMaster's own vocabulary, and the JSON encoder escapes what reaches a
// browser -- but the container NAME comes from Docker, so it is the one worth
// pinning.
func TestPlanTextIsEscapedInTheResponse(t *testing.T) {
	plan := samplePlan()
	plan.ContainerName = `<script>alert(1)</script>`
	plan.RegistryDetail = `</script><img src=x onerror=alert(1)>`

	srv := newPlanServer(t, &fakePlans{plans: []domain.ChangePlan{plan}}, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans", nil)
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

// A plan carries no secret, and cannot: PlanInputs has no field for one. This
// pins the boundary anyway, because a future field would show up here.
func TestNoPlanFieldCarriesASecret(t *testing.T) {
	plan := samplePlan()
	srv := newPlanServer(t, &fakePlans{plans: []domain.ChangePlan{plan}}, &fakePlanner{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/plans", nil)
	body := strings.ToLower(rec.Body.String())

	for _, forbidden := range []string{
		"password", "secret", "token", "authorization", "credential", "apikey", "api_key",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the plan response mentions %q: %s", forbidden, rec.Body.String())
		}
	}
}
