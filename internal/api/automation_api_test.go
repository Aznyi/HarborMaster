package api

import (
	"context"
	"encoding/json"
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

// Automation API tests.
//
// Automation is the only subsystem that can change the host with nobody
// watching, so the properties worth defending are all about what a caller
// CANNOT reach:
//
//   - No request body in this section names an image, a tag, a digest, or a
//     registry. The tests below try to send each of them and assert the request
//     is REJECTED rather than ignored.
//   - Reading is a permission every role holds; running a pass, approving a
//     decision, and writing a policy are three separate, stronger ones.
//   - A cookie-authenticated write without a CSRF token is refused.
//   - An anonymous caller reaches no service at all.
//   - The engine being disabled yields 503 on the write paths and still serves
//     the reads, because the history is what somebody turned it off to examine.

const (
	sampleRunID          = "run_00112233445566778899"
	sampleUpdatePolicyID = "upd_00112233445566778899"
)

// ------------------------------------------------------------------ fakes --

type fakeAutomation struct {
	mu sync.Mutex

	enabled  bool
	readable bool

	status    domain.AutomationStatus
	runs      []domain.AutomationRun
	decisions []domain.AutomationDecision
	pauses    []domain.PausedContainer

	// The readiness answer, and the candidate the handler assembled to ask
	// for it. Recorded because the whole point of the endpoint is what the
	// SERVER builds from the request body.
	readiness          domain.AutomationReadinessReport
	readinessErr       error
	readinessCandidate *domain.UpdatePolicy

	// What the API asked for. The point of the whole file.
	decisionFilters []store.AutomationDecisionFilter
	runCalls        []bool // dryRun flags
	approveCalls    []struct{ runID, containerName string }
	pauseCalls      []struct{ name, reason string }
	resumeCalls     []string

	runErr     error
	approveErr error
	pauseErr   error
	resumeErr  error
	statusErr  error
}

func (f *fakeAutomation) Enabled() bool  { return f.enabled }
func (f *fakeAutomation) Readable() bool { return f.readable }

func (f *fakeAutomation) Status(context.Context) (domain.AutomationStatus, error) {
	if f.statusErr != nil {
		return domain.AutomationStatus{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeAutomation) Runs(
	_ context.Context, _ store.AutomationRunFilter,
) ([]domain.AutomationRun, int, error) {
	return f.runs, len(f.runs), nil
}

func (f *fakeAutomation) RunDetail(
	_ context.Context, runID string, _ store.Page,
) (domain.AutomationRun, []domain.AutomationDecision, int, error) {
	for _, run := range f.runs {
		if run.RunID == runID {
			return run, f.decisions, len(f.decisions), nil
		}
	}
	return domain.AutomationRun{}, nil, 0, store.ErrNotFound
}

func (f *fakeAutomation) Decisions(
	_ context.Context, filter store.AutomationDecisionFilter,
) ([]domain.AutomationDecision, int, error) {
	f.mu.Lock()
	f.decisionFilters = append(f.decisionFilters, filter)
	f.mu.Unlock()
	return f.decisions, len(f.decisions), nil
}

// lastDecisionFilter is what the handler actually asked the engine for.
func (f *fakeAutomation) lastDecisionFilter(t *testing.T) store.AutomationDecisionFilter {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisionFilters) == 0 {
		t.Fatalf("the engine was never asked for decisions")
	}
	return f.decisionFilters[len(f.decisionFilters)-1]
}

func (f *fakeAutomation) Summary(context.Context) (domain.AutomationRunSummary, error) {
	return domain.AutomationRunSummary{Total: len(f.runs)}, nil
}

func (f *fakeAutomation) Pauses(
	_ context.Context, _ bool, _ store.Page,
) ([]domain.PausedContainer, int, error) {
	return f.pauses, len(f.pauses), nil
}

func (f *fakeAutomation) Upcoming(context.Context) ([]domain.AutomationDecision, error) {
	return f.decisions, nil
}

// Readiness records the candidate it was asked about, so a test can assert
// what the handler assembled from the request body rather than only what came
// back out.
func (f *fakeAutomation) Readiness(
	_ context.Context, candidate *domain.UpdatePolicy,
) (domain.AutomationReadinessReport, []domain.AutomationDecision, error) {
	if candidate != nil {
		copied := *candidate
		f.readinessCandidate = &copied
	}
	if f.readinessErr != nil {
		return domain.AutomationReadinessReport{}, nil, f.readinessErr
	}
	return f.readiness, f.decisions, nil
}

func (f *fakeAutomation) RunNow(
	_ context.Context, dryRun bool, _ domain.Requester,
) (domain.AutomationRun, []domain.AutomationDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, dryRun)
	if f.runErr != nil {
		return domain.AutomationRun{}, nil, f.runErr
	}
	return domain.AutomationRun{RunID: sampleRunID, DryRun: dryRun}, f.decisions, nil
}

func (f *fakeAutomation) Approve(
	_ context.Context, runID, containerName string, _ domain.Requester, _ service.Actor,
) (domain.AutomationDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approveCalls = append(f.approveCalls, struct{ runID, containerName string }{runID, containerName})
	if f.approveErr != nil {
		return domain.AutomationDecision{}, f.approveErr
	}
	return domain.AutomationDecision{RunID: runID, ContainerName: containerName}, nil
}

func (f *fakeAutomation) Resume(
	_ context.Context, containerName string, _ domain.Requester, _ service.Actor,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls = append(f.resumeCalls, containerName)
	return f.resumeErr
}

func (f *fakeAutomation) PauseContainer(
	_ context.Context, containerName, detail string, _ service.Actor,
) (domain.PausedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls = append(f.pauseCalls, struct{ name, reason string }{containerName, detail})
	if f.pauseErr != nil {
		return domain.PausedContainer{}, f.pauseErr
	}
	return domain.PausedContainer{ContainerName: containerName, Detail: detail}, nil
}

func (f *fakeAutomation) reached() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runCalls) + len(f.approveCalls) + len(f.pauseCalls) + len(f.resumeCalls)
}

type fakeUpdatePolicies struct {
	mu sync.Mutex

	items   []domain.UpdatePolicy
	created []domain.UpdatePolicy
	updated []store.UpdatePolicyChange
	deleted []string

	createErr error
	updateErr error
	getErr    error
}

func (f *fakeUpdatePolicies) Create(
	_ context.Context, policy domain.UpdatePolicy, _ service.Actor,
) (service.UpdatePolicyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, policy)
	if f.createErr != nil {
		return service.UpdatePolicyResult{}, f.createErr
	}
	policy.PolicyID = sampleUpdatePolicyID
	return service.UpdatePolicyResult{Policy: policy, Warnings: policy.Warnings()}, nil
}

func (f *fakeUpdatePolicies) Update(
	_ context.Context, policyID string, change store.UpdatePolicyChange, _ service.Actor,
) (service.UpdatePolicyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, change)
	if f.updateErr != nil {
		return service.UpdatePolicyResult{}, f.updateErr
	}
	return service.UpdatePolicyResult{Policy: domain.UpdatePolicy{PolicyID: policyID}}, nil
}

func (f *fakeUpdatePolicies) Archive(_ context.Context, policyID string, _ service.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, policyID)
	return nil
}

func (f *fakeUpdatePolicies) Get(_ context.Context, policyID string) (service.UpdatePolicyResult, error) {
	if f.getErr != nil {
		return service.UpdatePolicyResult{}, f.getErr
	}
	return service.UpdatePolicyResult{Policy: domain.UpdatePolicy{PolicyID: policyID}}, nil
}

func (f *fakeUpdatePolicies) List(
	_ context.Context, _ store.UpdatePolicyFilter,
) ([]domain.UpdatePolicy, int, error) {
	return f.items, len(f.items), nil
}

// The automatic-updates switch. Recorded like every other write so a test can
// assert that reading the switch mutates nothing.
func (f *fakeUpdatePolicies) SimpleUpdates(_ context.Context) (service.SimpleUpdatesState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.items {
		if domain.IsSimpleUpdatesPolicy(p.PolicyID) {
			return service.SimpleUpdatesState{
				Enabled: p.Enabled && !p.Archived, Configured: true, Policy: &p,
			}, nil
		}
	}
	return service.SimpleUpdatesState{}, nil
}

func (f *fakeUpdatePolicies) EnableSimpleUpdates(
	_ context.Context, _ service.Actor,
) (service.UpdatePolicyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	policy := domain.SimpleUpdatesPolicy()
	f.created = append(f.created, policy)
	return service.UpdatePolicyResult{Policy: policy}, nil
}

func (f *fakeUpdatePolicies) DisableSimpleUpdates(
	_ context.Context, _ service.Actor,
) (service.UpdatePolicyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	policy := domain.SimpleUpdatesPolicy()
	policy.Enabled = false
	disabled := false
	f.updated = append(f.updated, store.UpdatePolicyChange{Enabled: &disabled})
	return service.UpdatePolicyResult{Policy: policy}, nil
}

func (f *fakeUpdatePolicies) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created) + len(f.updated) + len(f.deleted)
}

// ---------------------------------------------------------------- server --

func newAutomationServer(
	t *testing.T,
	automation *fakeAutomation,
	policies *fakeUpdatePolicies,
) *Server {
	t.Helper()

	var engine AutomationService
	if automation != nil {
		engine = automation
	}
	var rules UpdatePolicyService
	if policies != nil {
		rules = policies
	}

	return newAuthedServer(Options{
		Health:         &fakeHealth{},
		Automation:     engine,
		UpdatePolicies: rules,
		Logger:         discardLogger(),
		Config:         config.Server{MaxRequestBytes: 8192},
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		PolicyConfig:   config.Policy{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})
}

func liveAutomation() *fakeAutomation {
	return &fakeAutomation{
		enabled:  true,
		readable: true,
		status:   domain.AutomationStatus{Enabled: true, Policies: 2, EnabledPolicies: 1},
		runs: []domain.AutomationRun{{
			RunID:     sampleRunID,
			Trigger:   domain.AutoTriggerSchedule,
			State:     domain.RunCompleted,
			StartedAt: time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC),
		}},
		decisions: []domain.AutomationDecision{{
			RunID:         sampleRunID,
			ContainerName: "web",
			Verdict:       domain.VerdictUpdate,
			Reason:        domain.ReasonEligible,
			DecidedAt:     time.Date(2026, 8, 6, 2, 0, 1, 0, time.UTC),
		}},
	}
}

const validUpdatePolicyBody = `{"name":"Nightly patches",` +
	`"selector":{"include":["web"]},"strategy":"patch","mode":"observe"}`

// ----------------------------------------------------------------- reads --

func TestAutomationStatusIsServed(t *testing.T) {
	srv := newAutomationServer(t, liveAutomation(), nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/automation", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response automationStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.Status.Enabled || response.Status.Policies != 2 {
		t.Fatalf("unexpected status: %+v", response.Status)
	}
}

func TestAutomationReadsSurviveTheEngineBeingOff(t *testing.T) {
	// The history is exactly what somebody who turned automation off wants to
	// examine. Refusing to show it would hide that.
	automation := liveAutomation()
	automation.enabled = false

	srv := newAutomationServer(t, automation, nil)

	for _, path := range []string{
		APIPrefix + "/automation",
		APIPrefix + "/automation/runs",
		APIPrefix + "/automation/runs/" + sampleRunID,
		APIPrefix + "/automation/paused",
		APIPrefix + "/automation/upcoming",
	} {
		if rec := do(t, srv, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAutomationWritesAreRefusedWhenTheEngineIsOff(t *testing.T) {
	automation := liveAutomation()
	automation.enabled = false
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/run", `{"dryRun":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if automation.reached() != 0 {
		t.Error("a disabled engine was asked to run a pass")
	}
}

func TestAutomationRoutesAre503WithoutTheService(t *testing.T) {
	srv := newAutomationServer(t, nil, nil)

	for _, path := range []string{
		APIPrefix + "/automation",
		APIPrefix + "/automation/runs",
		APIPrefix + "/automation/paused",
		APIPrefix + "/update-policies",
	} {
		if rec := do(t, srv, http.MethodGet, path, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", path, rec.Code)
		}
	}
}

func TestAutomationRunDetailRejectsAMalformedID(t *testing.T) {
	srv := newAutomationServer(t, liveAutomation(), nil)

	for _, id := range []string{
		"nonsense",
		"run_short",
		"run_" + strings.Repeat("z", 20),
		// The classic. It never reaches a query: the shape check refuses first.
		"run_00112233445566778899%27%20OR%201=1",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/runs/"+id, nil)
		// 404, not 400: a malformed id must not be distinguishable from an
		// absent one.
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", id, rec.Code)
		}
	}
}

func TestAutomationRunFiltersRejectUnknownValues(t *testing.T) {
	srv := newAutomationServer(t, liveAutomation(), nil)

	for _, query := range []string{
		"state=exploded",
		"trigger=cron",
		"acted=perhaps",
		"state=completed,exploded",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/runs?"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}

	// A repeated value is DEDUPLICATED rather than rejected, so a long
	// parameter cannot build a long IN clause. The vocabularies themselves are
	// short, so after deduplication the count bound is unreachable -- which is
	// the point: the bound is a backstop under a vocabulary that might grow.
	long := "state=" + strings.Repeat("completed,", 200) + "completed"
	if rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/runs?"+long, nil); rec.Code != http.StatusOK {
		t.Errorf("a repeated value must deduplicate, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ----------------------------------------------------------- run and dry run --

func TestRunningAPassPassesOnlyTheDryRunFlag(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/run", `{"dryRun":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(automation.runCalls) != 1 || !automation.runCalls[0] {
		t.Fatalf("run calls = %+v, want one dry run", automation.runCalls)
	}
}

func TestRunningAPassRejectsAnythingThatLooksLikeATarget(t *testing.T) {
	// The rule that matters most, tested by trying to break it. Unknown fields
	// are REJECTED rather than ignored, so no Docker parameter can be smuggled
	// into a request that will cause a container to be replaced.
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	for _, body := range []string{
		`{"dryRun":false,"containerId":"0123456789abcdef"}`,
		`{"dryRun":false,"image":"evil.example.com/x:latest"}`,
		`{"dryRun":false,"digest":"sha256:0000"}`,
		`{"dryRun":false,"registry":"evil.example.com"}`,
		`{"dryRun":false,"tag":"latest"}`,
		`{"dryRun":false,"containerName":"web"}`,
		`{"dryRun":false,"planId":"plan_00112233445566778899"}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/run", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
	if len(automation.runCalls) != 0 {
		t.Error("a request carrying an unknown field reached the engine")
	}
}

func TestAConcurrentPassAnswers409(t *testing.T) {
	automation := liveAutomation()
	automation.runErr = service.ErrAutomationBusy
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/run", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------- approval --

func TestApprovingSelectsAHeldDecision(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	body := `{"runId":"` + sampleRunID + `","containerName":"web"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/approve", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(automation.approveCalls) != 1 {
		t.Fatalf("approve calls = %d, want 1", len(automation.approveCalls))
	}
	if automation.approveCalls[0].runID != sampleRunID ||
		automation.approveCalls[0].containerName != "web" {
		t.Fatalf("unexpected approval: %+v", automation.approveCalls[0])
	}
}

func TestApprovingRejectsAMalformedSelection(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	for _, body := range []string{
		`{"runId":"nonsense","containerName":"web"}`,
		`{"runId":"` + sampleRunID + `","containerName":""}`,
		`{"runId":"` + sampleRunID + `","containerName":"a name with spaces"}`,
		`{"runId":"` + sampleRunID + `","containerName":"../../etc/passwd"}`,
		// No way to name what to update instead.
		`{"runId":"` + sampleRunID + `","containerName":"web","image":"evil:latest"}`,
		`{"runId":"` + sampleRunID + `","containerName":"web","planId":"plan_1"}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/approve", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
	if len(automation.approveCalls) != 0 {
		t.Error("a malformed approval reached the engine")
	}
}

func TestApprovingAStaleDecisionAnswers409(t *testing.T) {
	automation := liveAutomation()
	automation.approveErr = service.ErrDecisionNotApprovable
	srv := newAutomationServer(t, automation, nil)

	body := `{"runId":"` + sampleRunID + `","containerName":"web"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/approve", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// ----------------------------------------------------------------- pauses --

func TestPausingNamesAContainerAndNothingElse(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/pause",
		`{"containerName":"web","reason":"investigating a crash loop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(automation.pauseCalls) != 1 || automation.pauseCalls[0].name != "web" {
		t.Fatalf("unexpected pause: %+v", automation.pauseCalls)
	}
}

func TestPausingSanitisesTheOperatorsNote(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/pause",
		`{"containerName":"web","reason":"line one\u0000line two\u001b[31m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	stored := automation.pauseCalls[0].reason
	if strings.ContainsRune(stored, 0) || strings.ContainsRune(stored, 0x1b) {
		t.Fatalf("control characters reached the record: %q", stored)
	}
}

func TestPausingRejectsAnOversizedNote(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	body := `{"containerName":"web","reason":"` + strings.Repeat("a", 600) + `"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/pause", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(automation.pauseCalls) != 0 {
		t.Error("an oversized note reached the engine")
	}
}

func TestPausingAnUnknownContainerAnswers404(t *testing.T) {
	automation := liveAutomation()
	automation.pauseErr = store.ErrNotFound
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/pause",
		`{"containerName":"ghost"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestResumingClearsAPause(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/resume",
		`{"containerName":"web"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if len(automation.resumeCalls) != 1 || automation.resumeCalls[0] != "web" {
		t.Fatalf("unexpected resume: %+v", automation.resumeCalls)
	}
}

// ------------------------------------------------------- update policies --

func TestCreatingAnUpdatePolicyRequiresTheDecidingFields(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	for _, body := range []string{
		`{"selector":{"include":["web"]},"strategy":"patch","mode":"observe"}`,
		`{"name":"x","strategy":"patch","mode":"observe"}`,
		`{"name":"x","selector":{"include":["web"]},"mode":"observe"}`,
		`{"name":"x","selector":{"include":["web"]},"strategy":"patch"}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
	if policies.writes() != 0 {
		t.Error("an incomplete policy reached the service")
	}
}

func TestAnUpdatePolicyCannotNameAnImageToPullTo(t *testing.T) {
	// A policy says WHICH containers and HOW FAR. What image a matched
	// container moves to is the planner's, and there is no field for it.
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	for _, body := range []string{
		`{"name":"x","selector":{"include":["web"]},"strategy":"patch","mode":"observe","targetImage":"evil:latest"}`,
		`{"name":"x","selector":{"include":["web"]},"strategy":"patch","mode":"observe","digest":"sha256:00"}`,
		`{"name":"x","selector":{"include":["web"]},"strategy":"patch","mode":"observe","registry":"evil.example.com"}`,
		`{"name":"x","selector":{"include":["web"]},"strategy":"patch","mode":"observe","pullOptions":{}}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
	if policies.writes() != 0 {
		t.Error("a policy carrying an unknown field reached the service")
	}
}

func TestCreatingAnUpdatePolicyDefaultsToTheSaferSetting(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", validUpdatePolicyBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(policies.created) != 1 {
		t.Fatalf("created = %d, want 1", len(policies.created))
	}
	created := policies.created[0]
	if created.MinimumRecommendation != domain.RecommendProceed {
		t.Fatalf("minimumRecommendation = %q, want the stricter default",
			created.MinimumRecommendation)
	}
	// The policy id is generated by the SERVICE, never taken from the body.
	if created.PolicyID != "" {
		t.Fatalf("the handler set a policy id (%q); it must be generated server-side",
			created.PolicyID)
	}
}

func TestUpdatePolicyValidationNamesTheFieldNotTheValue(t *testing.T) {
	const marker = "PoIsOnEdVaLuE"
	policies := &fakeUpdatePolicies{
		createErr: domain.PolicyValidationError{Field: "strategy", Message: "must be one of ..."},
	}
	srv := newAutomationServer(t, liveAutomation(), policies)

	body := `{"name":"` + marker + `","selector":{"include":["web"]},` +
		`"strategy":"` + marker + `","mode":"observe"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("the response echoed the request: %s", rec.Body.String())
	}
}

func TestDeletingAnUpdatePolicyArchivesIt(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	rec := do(t, srv, http.MethodDelete, APIPrefix+"/update-policies/"+sampleUpdatePolicyID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if len(policies.deleted) != 1 || policies.deleted[0] != sampleUpdatePolicyID {
		t.Fatalf("archived = %+v", policies.deleted)
	}
}

func TestUpdatePolicyPathRejectsAMalformedID(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	for _, id := range []string{"nonsense", "pol_00112233445566778899", "upd_short"} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			rec := do(t, srv, method, APIPrefix+"/update-policies/"+id, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404", method, id, rec.Code)
			}
		}
	}
	if policies.writes() != 0 {
		t.Error("a malformed id reached the service")
	}
}

// -------------------------------------------------------- authorization --

func TestAnonymousCallersReachNoAutomationEndpoint(t *testing.T) {
	automation := liveAutomation()
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, automation, policies)

	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, APIPrefix + "/automation", ""},
		{http.MethodGet, APIPrefix + "/automation/runs", ""},
		{http.MethodGet, APIPrefix + "/automation/upcoming", ""},
		{http.MethodGet, APIPrefix + "/automation/paused", ""},
		{http.MethodGet, APIPrefix + "/automation/approvals", ""},
		{http.MethodPost, APIPrefix + "/automation/run", `{"dryRun":true}`},
		{http.MethodPost, APIPrefix + "/automation/approve",
			`{"runId":"` + sampleRunID + `","containerName":"web"}`},
		{http.MethodPost, APIPrefix + "/automation/pause", `{"containerName":"web"}`},
		{http.MethodPost, APIPrefix + "/automation/resume", `{"containerName":"web"}`},
		{http.MethodGet, APIPrefix + "/update-policies", ""},
		{http.MethodPost, APIPrefix + "/update-policies", validUpdatePolicyBody},
		{http.MethodDelete, APIPrefix + "/update-policies/" + sampleUpdatePolicyID, ""},
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
	if automation.reached() != 0 || policies.writes() != 0 {
		t.Error("an anonymous caller reached the automation subsystem")
	}
}

func TestTheAutomationRoleMatrix(t *testing.T) {
	// A viewer READS everything: automation changes the host without being
	// asked, and a viewer who cannot see what it decided cannot answer the
	// question their role exists for.
	//
	// An operator RUNS, APPROVES, and PAUSES. An administrator additionally
	// WRITES POLICIES, because a policy is a standing grant of the operator's
	// most dangerous permission.
	cases := []struct {
		role        domain.Role
		read        int
		run         int
		approve     int
		pause       int
		writePolicy int
	}{
		{domain.RoleViewer, http.StatusOK, http.StatusForbidden,
			http.StatusForbidden, http.StatusForbidden, http.StatusForbidden},
		{domain.RoleOperator, http.StatusOK, http.StatusOK,
			http.StatusAccepted, http.StatusOK, http.StatusForbidden},
		{domain.RoleAdministrator, http.StatusOK, http.StatusOK,
			http.StatusAccepted, http.StatusOK, http.StatusCreated},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.role), func(t *testing.T) {
			automation := liveAutomation()
			policies := &fakeUpdatePolicies{}
			srv, _, _ := asRole(Options{
				Health:         &fakeHealth{},
				Automation:     automation,
				UpdatePolicies: policies,
				Logger:         discardLogger(),
				Config:         config.Server{MaxRequestBytes: 8192},
				SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
				PolicyConfig:   config.Policy{WriteRateLimit: 10000, WriteRateBurst: 10000},
				Assets:         testAssets(),
			}, testCase.role)

			if rec := do(t, srv, http.MethodGet, APIPrefix+"/automation", nil); rec.Code != testCase.read {
				t.Errorf("read = %d, want %d", rec.Code, testCase.read)
			}
			// The approvals queue is a READ. A viewer holds it for the same
			// reason they hold every other automation read: automation changes
			// the host unasked, and somebody has to be able to see that a
			// change is being held even if they cannot release it.
			if rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/approvals",
				nil); rec.Code != testCase.read {
				t.Errorf("approvals read = %d, want %d", rec.Code, testCase.read)
			}
			if rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/run",
				`{"dryRun":true}`); rec.Code != testCase.run {
				t.Errorf("run = %d, want %d", rec.Code, testCase.run)
			}
			if rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/approve",
				`{"runId":"`+sampleRunID+`","containerName":"web"}`); rec.Code != testCase.approve {
				t.Errorf("approve = %d, want %d", rec.Code, testCase.approve)
			}
			if rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/automation/pause",
				`{"containerName":"web"}`); rec.Code != testCase.pause {
				t.Errorf("pause = %d, want %d", rec.Code, testCase.pause)
			}
			if rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies",
				validUpdatePolicyBody); rec.Code != testCase.writePolicy {
				t.Errorf("policy write = %d, want %d", rec.Code, testCase.writePolicy)
			}
		})
	}
}

func TestAutomationWritesRequireACSRFToken(t *testing.T) {
	automation := liveAutomation()
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, automation, policies)

	targets := []struct {
		target string
		body   string
	}{
		{APIPrefix + "/automation/run", `{"dryRun":true}`},
		{APIPrefix + "/automation/approve", `{"runId":"` + sampleRunID + `","containerName":"web"}`},
		{APIPrefix + "/automation/pause", `{"containerName":"web"}`},
		{APIPrefix + "/automation/resume", `{"containerName":"web"}`},
		{APIPrefix + "/update-policies", validUpdatePolicyBody},
	}

	for _, entry := range targets {
		req := httptest.NewRequest(http.MethodPost, entry.target, strings.NewReader(entry.body))
		req.Header.Set("Content-Type", "application/json")
		// The session cookie, but no CSRF header: what a cross-site form post
		// looks like.
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", entry.target, rec.Code)
		}
	}
	if automation.reached() != 0 || policies.writes() != 0 {
		t.Error("a request with no CSRF token reached the automation subsystem")
	}
}

// doJSON issues an authenticated JSON write.
func doJSON(t *testing.T, srv *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))
	return rec
}
