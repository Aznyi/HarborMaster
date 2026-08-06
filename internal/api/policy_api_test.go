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

// Policy API tests.
//
// This is the first phase to add a create/update/delete surface, so the
// properties worth defending are about that surface: every write must be
// guarded exactly like a POST, a caller must not be able to choose or change a
// policy id, DELETE must archive rather than destroy history, the manual
// evaluate endpoint must not run work synchronously, and the violation PATCH
// must refuse the engine-owned statuses.

// ------------------------------------------------------------------ fakes --

type fakePolicies struct {
	mu sync.Mutex

	policies   []domain.PolicyDefinition
	violations []domain.PolicyViolation
	summary    domain.PolicySummary
	evaluation *domain.PolicyEvaluation

	// lastPolicyFilter and lastViolationFilter record what the handlers asked
	// for, which is how the filter tests assert that parsing reached the
	// repository.
	lastPolicyFilter    store.PolicyFilter
	lastViolationFilter store.PolicyViolationFilter

	created  []domain.PolicyDefinition
	updates  []store.PolicyUpdate
	archived []string
	statuses []struct {
		id     int64
		status domain.PolicyViolationStatus
		note   string
	}

	createErr error
	listErr   error
}

func (f *fakePolicies) ListPolicies(
	_ context.Context, filter store.PolicyFilter,
) ([]domain.PolicyDefinition, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPolicyFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.policies, len(f.policies), nil
}

func (f *fakePolicies) GetPolicy(_ context.Context, policyID string) (domain.PolicyDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, policy := range f.policies {
		if policy.PolicyID == policyID {
			return policy, nil
		}
	}
	return domain.PolicyDefinition{}, store.ErrNotFound
}

func (f *fakePolicies) CreatePolicy(
	_ context.Context, policy domain.PolicyDefinition, now time.Time,
) (domain.PolicyDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return domain.PolicyDefinition{}, f.createErr
	}
	policy.ID = int64(len(f.policies) + 1)
	policy.CreatedAt, policy.UpdatedAt = now, now
	f.created = append(f.created, policy)
	f.policies = append(f.policies, policy)
	return policy, nil
}

func (f *fakePolicies) UpdatePolicy(
	_ context.Context, policyID string, update store.PolicyUpdate, now time.Time,
) (domain.PolicyDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, update)

	for index, policy := range f.policies {
		if policy.PolicyID != policyID {
			continue
		}
		if update.Name != nil {
			policy.Name = *update.Name
		}
		if update.Enabled != nil {
			policy.Enabled = *update.Enabled
		}
		if update.Rules != nil {
			policy.Rules = *update.Rules
		}
		policy.UpdatedAt = now
		f.policies[index] = policy
		return policy, nil
	}
	return domain.PolicyDefinition{}, store.ErrNotFound
}

func (f *fakePolicies) ArchivePolicy(_ context.Context, policyID string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index, policy := range f.policies {
		if policy.PolicyID == policyID {
			f.archived = append(f.archived, policyID)
			f.policies[index].Archived = true
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakePolicies) ListViolations(
	_ context.Context, filter store.PolicyViolationFilter,
) ([]domain.PolicyViolation, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastViolationFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.violations, len(f.violations), nil
}

func (f *fakePolicies) GetViolation(_ context.Context, id int64) (domain.PolicyViolation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, violation := range f.violations {
		if violation.ID == id {
			return violation, nil
		}
	}
	return domain.PolicyViolation{}, store.ErrNotFound
}

func (f *fakePolicies) UpdateViolationStatus(
	_ context.Context, id int64, status domain.PolicyViolationStatus, note string, _ time.Time,
) (domain.PolicyViolation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index, violation := range f.violations {
		if violation.ID != id {
			continue
		}
		f.statuses = append(f.statuses, struct {
			id     int64
			status domain.PolicyViolationStatus
			note   string
		}{id, status, note})
		violation.Status = status
		violation.Note = note
		f.violations[index] = violation
		return violation, nil
	}
	return domain.PolicyViolation{}, store.ErrNotFound
}

func (f *fakePolicies) PolicySummary(context.Context) (domain.PolicySummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, nil
}

func (f *fakePolicies) PolicyEvaluation(_ context.Context, _ string) (domain.PolicyEvaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.evaluation == nil {
		return domain.PolicyEvaluation{}, store.ErrNotFound
	}
	return *f.evaluation, nil
}

// fakePolicyEngine records sweep requests without doing any work.
type fakePolicyEngine struct {
	mu     sync.Mutex
	sweeps int
}

func (f *fakePolicyEngine) RequestSweep() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
}

func (f *fakePolicyEngine) Status() domain.PolicyEngineStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.PolicyEngineStatus{Enabled: true, SweepPending: f.sweeps > 0}
}

func (f *fakePolicyEngine) sweepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

// fakePolicyContainerReader resolves a container id for the per-container
// route. Only ResolveID is exercised; the embedded interface supplies the rest
// and panics if anything else is ever called, which is the honest behaviour for
// a double that does not implement it.
type fakePolicyContainerReader struct{ ContainerReader }

func (fakePolicyContainerReader) ResolveID(_ context.Context, reference string) (string, error) {
	if reference == "missing" {
		return "", store.ErrNotFound
	}
	return reference, nil
}

// -------------------------------------------------------------- harnesses --

func samplePolicy() domain.PolicyDefinition {
	return domain.PolicyDefinition{
		PolicyID: "pol_00112233445566778899",
		Name:     "Container hardening",
		Severity: domain.PolicySeverityHigh,
		Enabled:  true,
		Rules: []domain.PolicyRule{
			{Type: domain.RulePrivilegedForbidden},
		},
	}
}

func sampleViolation() domain.PolicyViolation {
	return domain.PolicyViolation{
		ID:            1,
		PolicyID:      "pol_00112233445566778899",
		PolicyName:    "Container hardening",
		ContainerID:   "abc123",
		ContainerName: "web",
		RuleType:      domain.RulePrivilegedForbidden,
		Severity:      domain.PolicySeverityCritical,
		Observed:      "privileged=true",
		Expected:      "privileged=false",
		Reason:        "the container runs privileged",
		Status:        domain.PolicyViolationActive,
	}
}

func newPolicyServer(t *testing.T, policies *fakePolicies, engine *fakePolicyEngine) *Server {
	t.Helper()
	return newAuthedServer(Options{
		Health:       &fakeHealth{},
		Policies:     policies,
		PolicyEngine: engine,
		PolicyConfig: config.Policy{
			Enabled:             true,
			MaxRulesPerPolicy:   32,
			MaxValuesPerRule:    32,
			MaxNameBytes:        120,
			MaxDescriptionBytes: 1000,
			MaxNoteBytes:        500,
			WriteRateLimit:      10000,
			WriteRateBurst:      10000,
		},
		Containers: fakePolicyContainerReader{},
		Logger:     discardLogger(),
		Config:     config.Server{MaxRequestBytes: 16384},
		SnapshotConfig: config.Snapshots{
			WriteRateLimit: 10000, WriteRateBurst: 10000,
		},
		Now:    func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		Assets: testAssets(),
	})
}

// write issues a body-carrying request with the headers a browser would send.
func write(t *testing.T, srv *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(req))
	return rec
}

const validPolicyBody = `{
	"name": "Container hardening",
	"severity": "high",
	"rules": [{"type": "privilegedForbidden"}]
}`

// ----------------------------------------------------------------- reads --

func TestPolicyListAndDetail(t *testing.T) {
	policies := &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/policies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}

	var list listResponse[domain.PolicyDefinition]
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].PolicyID != samplePolicy().PolicyID {
		t.Errorf("items = %+v", list.Items)
	}
	// Archived policies are excluded by default.
	if policies.lastPolicyFilter.IncludeArchived {
		t.Error("the default listing included archived policies")
	}

	rec = do(t, srv, http.MethodGet, APIPrefix+"/policies/"+samplePolicy().PolicyID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", rec.Code)
	}

	rec = do(t, srv, http.MethodGet, APIPrefix+"/policies/pol_ffffffffffffffffffff", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing policy status = %d, want 404", rec.Code)
	}
}

// The editor is built from the catalogue the validator uses, so the endpoint
// must serve every rule type and the bounds the server enforces.
func TestTheRuleCatalogueIsServed(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{}, &fakePolicyEngine{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/policy-rules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var catalogue policyRuleCatalogueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &catalogue); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(catalogue.Rules) != len(domain.PolicyRuleTypes) {
		t.Errorf("rules = %d, want %d", len(catalogue.Rules), len(domain.PolicyRuleTypes))
	}
	if len(catalogue.Severities) != 4 || len(catalogue.RestartPolicy) != 4 {
		t.Errorf("vocabularies = %v / %v", catalogue.Severities, catalogue.RestartPolicy)
	}
	if catalogue.Limits.MaxRules != 32 {
		t.Errorf("limits = %+v, want the configured bounds", catalogue.Limits)
	}
}

func TestViolationListFiltersReachTheRepository(t *testing.T) {
	policies := &fakePolicies{violations: []domain.PolicyViolation{sampleViolation()}}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	target := APIPrefix + "/policy-violations?severity=critical,high&rule=privilegedForbidden" +
		"&containerId=abc123&sort=severity&order=asc&page=1&pageSize=10"
	rec := do(t, srv, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	filter := policies.lastViolationFilter
	if len(filter.Severities) != 2 || len(filter.RuleTypes) != 1 {
		t.Errorf("filter = %+v", filter)
	}
	if filter.ContainerID != "abc123" || filter.Sort != "severity" || !filter.Ascending {
		t.Errorf("filter = %+v", filter)
	}
	// openOnly defaults to true, so the dashboard shows what still fails.
	if !filter.OpenOnly {
		t.Error("openOnly did not default to true")
	}
}

// An explicit status filter must turn the openOnly default off, or asking for
// resolved violations would return none.
func TestAnExplicitStatusFilterDisablesTheOpenOnlyDefault(t *testing.T) {
	policies := &fakePolicies{}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/policy-violations?status=resolved", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if policies.lastViolationFilter.OpenOnly {
		t.Error("an explicit status filter left openOnly on")
	}
}

func TestPolicyQueryRejections(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{}, &fakePolicyEngine{})

	for _, target := range []string{
		APIPrefix + "/policies?sort=rules_json",
		APIPrefix + "/policies?sort=" + url.QueryEscape("name; DROP TABLE policy_definitions"),
		APIPrefix + "/policies?order=sideways",
		APIPrefix + "/policies?enabled=perhaps",
		APIPrefix + "/policies?search=" + url.QueryEscape(strings.Repeat("a", 300)),
		APIPrefix + "/policy-violations?severity=catastrophic",
		APIPrefix + "/policy-violations?rule=runArbitraryCode",
		APIPrefix + "/policy-violations?status=deleted",
		APIPrefix + "/policy-violations?sort=detected_at",
		APIPrefix + "/policy-violations?policyId=" + url.QueryEscape("'; DROP TABLE x; --"),
		APIPrefix + "/policy-violations?" + strings.Repeat("severity=high&", 40),
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", target, rec.Code)
		}
		// An error message must never reflect the caller's value.
		if strings.Contains(rec.Body.String(), "DROP TABLE") {
			t.Errorf("%s echoed the caller's input: %s", target, rec.Body.String())
		}
	}
}

// A policy id is validated by SHAPE before it reaches a query.
func TestMalformedPolicyIdsAreRefused(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}}, &fakePolicyEngine{})

	for _, id := range []string{
		"pol_short",
		"pol_00112233445566778899x",
		"pol_ZZ112233445566778899",
		"abc_00112233445566778899",
		url.QueryEscape("' OR 1=1 --"),
		url.QueryEscape("../../etc/passwd"),
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/policies/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET /policies/%s returned %d, want 400", id, rec.Code)
		}
	}

	// An UNESCAPED traversal never reaches the handler at all: ServeMux
	// normalises the path and redirects first. Asserted rather than assumed,
	// because "the handler rejected it" and "the handler never saw it" are
	// different guarantees and only one of them is true here.
	rec := do(t, srv, http.MethodGet, APIPrefix+"/policies/../../etc/passwd", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("an unescaped traversal returned 200")
	}
	if location := rec.Header().Get("Location"); location != "" && strings.Contains(location, "..") {
		t.Errorf("the redirect preserved the traversal: %q", location)
	}
}

// ---------------------------------------------------------------- writes --

func TestCreatingAPolicy(t *testing.T) {
	policies := &fakePolicies{}
	engine := &fakePolicyEngine{}
	srv := newPolicyServer(t, policies, engine)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", validPolicyBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var created domain.PolicyDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !validPolicyID(created.PolicyID) {
		t.Errorf("policy id %q does not have the generated shape", created.PolicyID)
	}
	if !created.Enabled {
		t.Error("a new policy defaulted to disabled")
	}
	if location := rec.Header().Get("Location"); location != APIPrefix+"/policies/"+created.PolicyID {
		t.Errorf("Location = %q", location)
	}
	// A definition change re-evaluates the estate rather than waiting for the
	// next scheduled pass.
	if engine.sweepCount() != 1 {
		t.Errorf("sweeps = %d, want 1 after a create", engine.sweepCount())
	}
}

// The identifier is server-generated. A caller must not be able to choose one,
// and the strict decoder is what makes the attempt visible rather than ignored.
func TestACallerCannotChooseAPolicyID(t *testing.T) {
	policies := &fakePolicies{}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	body := `{"policyId":"pol_aaaaaaaaaaaaaaaaaaaa","name":"Mine","severity":"low",` +
		`"rules":[{"type":"privilegedForbidden"}]}`
	rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
	if len(policies.created) != 0 {
		t.Error("a policy was created despite the rejected body")
	}
}

func TestCreateValidationRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no name", `{"severity":"high","rules":[{"type":"privilegedForbidden"}]}`},
		{"no rules", `{"name":"X","severity":"high"}`},
		{"empty rules", `{"name":"X","severity":"high","rules":[]}`},
		{"unknown severity", `{"name":"X","severity":"nuclear","rules":[{"type":"privilegedForbidden"}]}`},
		{"unknown rule type", `{"name":"X","severity":"high","rules":[{"type":"execShell"}]}`},
		{
			"duplicate rule types",
			`{"name":"X","severity":"high","rules":[{"type":"userNotRoot"},{"type":"userNotRoot"}]}`,
		},
		{
			"values on a parameterless rule",
			`{"name":"X","severity":"high","rules":[{"type":"userNotRoot","values":["yes"]}]}`,
		},
		{
			"a rule needing values with none",
			`{"name":"X","severity":"high","rules":[{"type":"imageAllowlist"}]}`,
		},
		{
			"a control character in the name",
			`{"name":"line\nbreak","severity":"high","rules":[{"type":"userNotRoot"}]}`,
		},
		{
			"an unknown field",
			`{"name":"X","severity":"high","rules":[{"type":"userNotRoot"}],"enforce":true}`,
		},
		{"not an object", `[]`},
		{"two objects", `{"name":"X","severity":"high","rules":[]}{"name":"Y"}`},
		{"empty body", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policies := &fakePolicies{}
			srv := newPolicyServer(t, policies, &fakePolicyEngine{})

			rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(policies.created) != 0 {
				t.Error("an invalid policy was stored")
			}
		})
	}
}

// A pattern that would make matching expensive must be refused at write time,
// so it never reaches the database and never reaches the matcher.
func TestExpensivePatternsAreRefusedAtWriteTime(t *testing.T) {
	policies := &fakePolicies{}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	pathological := strings.Repeat("*a", domain.MaxPolicyWildcards+2)
	body := fmt.Sprintf(`{"name":"X","severity":"high","rules":[{"type":"imageAllowlist","values":[%q]}]}`,
		pathological)

	rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(policies.created) != 0 {
		t.Error("a pathological pattern was stored")
	}

	oversize := strings.Repeat("a", domain.MaxPolicyPatternBytes+1)
	body = fmt.Sprintf(`{"name":"X","severity":"high","rules":[{"type":"imageAllowlist","values":[%q]}]}`,
		oversize)
	if rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", body); rec.Code != http.StatusBadRequest {
		t.Errorf("an oversized pattern returned %d, want 400", rec.Code)
	}
}

func TestDuplicateNamesConflict(t *testing.T) {
	policies := &fakePolicies{createErr: store.ErrPolicyNameTaken}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", validPolicyBody)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestUpdatingAPolicy(t *testing.T) {
	policies := &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}}
	engine := &fakePolicyEngine{}
	srv := newPolicyServer(t, policies, engine)

	rec := write(t, srv, http.MethodPatch,
		APIPrefix+"/policies/"+samplePolicy().PolicyID, `{"name":"Renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if len(policies.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(policies.updates))
	}
	update := policies.updates[0]
	if update.Name == nil || *update.Name != "Renamed" {
		t.Errorf("name = %v, want Renamed", update.Name)
	}
	// An omitted field must not be sent, or a PATCH would silently overwrite
	// everything it did not mention.
	if update.Enabled != nil || update.Rules != nil || update.Severity != nil {
		t.Errorf("omitted fields were included: %+v", update)
	}
	if engine.sweepCount() != 1 {
		t.Errorf("sweeps = %d, want 1 after an update", engine.sweepCount())
	}
}

// A partial update is validated as the WHOLE policy it will produce, so a
// change that is legal in isolation but illegal in context is refused.
func TestAPartialUpdateIsValidatedAgainstTheMergedPolicy(t *testing.T) {
	policies := &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	// An empty name is invalid, even though the request changes nothing else.
	rec := write(t, srv, http.MethodPatch,
		APIPrefix+"/policies/"+samplePolicy().PolicyID, `{"name":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}

	// Replacing the rules with an empty list leaves a policy that can never
	// pass or fail, so it is refused too.
	rec = write(t, srv, http.MethodPatch,
		APIPrefix+"/policies/"+samplePolicy().PolicyID, `{"rules":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty rules returned %d, want 400", rec.Code)
	}

	if len(policies.updates) != 0 {
		t.Error("an invalid update reached the repository")
	}
}

// DELETE archives. The endpoint must succeed, and the interface it goes through
// has no method that could destroy the row.
func TestDeletingAPolicyArchivesIt(t *testing.T) {
	policies := &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := write(t, srv, http.MethodDelete, APIPrefix+"/policies/"+samplePolicy().PolicyID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if len(policies.archived) != 1 {
		t.Fatalf("archived = %v, want the one policy", policies.archived)
	}
	// The definition is still there, marked archived.
	if len(policies.policies) != 1 || !policies.policies[0].Archived {
		t.Error("the policy was removed rather than archived")
	}

	rec = write(t, srv, http.MethodDelete, APIPrefix+"/policies/pol_ffffffffffffffffffff", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleting a missing policy returned %d, want 404", rec.Code)
	}
}

// -------------------------------------------------------------- the guard --

// Every write gets the POST guard. A cross-site request must be refused before
// it reaches any handler logic.
func TestEveryPolicyWriteIsGuarded(t *testing.T) {
	writes := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPost, APIPrefix + "/policies", validPolicyBody},
		{http.MethodPatch, APIPrefix + "/policies/" + samplePolicy().PolicyID, `{"name":"X"}`},
		{http.MethodDelete, APIPrefix + "/policies/" + samplePolicy().PolicyID, ""},
		{http.MethodPatch, APIPrefix + "/policy-violations/1", `{"status":"acknowledged"}`},
		{http.MethodPost, APIPrefix + "/policy/evaluate", `{}`},
	}

	for _, tc := range writes {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			policies := &fakePolicies{
				policies:   []domain.PolicyDefinition{samplePolicy()},
				violations: []domain.PolicyViolation{sampleViolation()},
			}
			srv := newPolicyServer(t, policies, &fakePolicyEngine{})

			// Cross-site, as a hostile page's fetch would be.
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, authed(req))

			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-site request returned %d, want 403", rec.Code)
			}
			if len(policies.created) != 0 || len(policies.updates) != 0 ||
				len(policies.archived) != 0 || len(policies.statuses) != 0 {
				t.Error("a cross-site request reached the repository")
			}
		})
	}
}

// A body-carrying write must require the JSON media type, which is what forces
// a CORS preflight that a simple form POST cannot satisfy.
func TestPolicyWritesRequireTheJSONMediaType(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}},
		&fakePolicyEngine{})

	for _, contentType := range []string{
		"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data",
	} {
		req := httptest.NewRequest(http.MethodPost, APIPrefix+"/policies",
			strings.NewReader(validPolicyBody))
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

func TestPolicyMethodsAreConstrained(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{}, &fakePolicyEngine{})

	cases := []struct {
		method string
		target string
	}{
		{http.MethodDelete, APIPrefix + "/policies"},
		{http.MethodPut, APIPrefix + "/policies/" + samplePolicy().PolicyID},
		{http.MethodPost, APIPrefix + "/policy-violations"},
		{http.MethodDelete, APIPrefix + "/policy-violations/1"},
		{http.MethodPost, APIPrefix + "/policy-summary"},
		{http.MethodPost, APIPrefix + "/policy-rules"},
		{http.MethodGet, APIPrefix + "/policy/evaluate"},
	}

	for _, tc := range cases {
		rec := write(t, srv, tc.method, tc.target, `{}`)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s returned %d, want 405", tc.method, tc.target, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("%s %s carries no Allow header", tc.method, tc.target)
		}
	}
}

// There must be no route that applies, enforces, or remediates a policy. This
// asserts the absence directly rather than trusting that nobody added one.
func TestThereIsNoEnforcementRoute(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{}, &fakePolicyEngine{})

	for _, target := range []string{
		APIPrefix + "/policies/" + samplePolicy().PolicyID + "/enforce",
		APIPrefix + "/policies/" + samplePolicy().PolicyID + "/apply",
		APIPrefix + "/policy/enforce",
		APIPrefix + "/policy/remediate",
		APIPrefix + "/policy-violations/1/fix",
		APIPrefix + "/policy-violations/1/remediate",
	} {
		rec := write(t, srv, http.MethodPost, target, `{}`)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d; there must be no enforcement route", target, rec.Code)
		}
	}
}

// ------------------------------------------------------ violation patches --

func TestAcknowledgingAViolation(t *testing.T) {
	policies := &fakePolicies{violations: []domain.PolicyViolation{sampleViolation()}}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := write(t, srv, http.MethodPatch, APIPrefix+"/policy-violations/1",
		`{"status":"acknowledged","note":"accepted until the next release"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if len(policies.statuses) != 1 {
		t.Fatalf("status changes = %d, want 1", len(policies.statuses))
	}
	if policies.statuses[0].status != domain.PolicyViolationAcknowledged {
		t.Errorf("status = %q", policies.statuses[0].status)
	}
	if policies.statuses[0].note != "accepted until the next release" {
		t.Errorf("note = %q", policies.statuses[0].note)
	}
}

// The engine owns active and resolved. A caller asserting either would be
// asserting a fact about the world rather than an intent.
func TestTheEngineOwnedStatusesAreRefused(t *testing.T) {
	for _, status := range []string{"active", "resolved", "", "deleted", "ACKNOWLEDGED"} {
		policies := &fakePolicies{violations: []domain.PolicyViolation{sampleViolation()}}
		srv := newPolicyServer(t, policies, &fakePolicyEngine{})

		rec := write(t, srv, http.MethodPatch, APIPrefix+"/policy-violations/1",
			fmt.Sprintf(`{"status":%q}`, status))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %q returned %d, want 400", status, rec.Code)
		}
		if len(policies.statuses) != 0 {
			t.Errorf("status %q reached the repository", status)
		}
	}
}

func TestViolationNoteValidation(t *testing.T) {
	cases := []struct {
		name string
		note string
	}{
		{"too long", strings.Repeat("a", 600)},
		{"a newline", "first\nsecond"},
		{"a NUL", "before\x00after"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policies := &fakePolicies{violations: []domain.PolicyViolation{sampleViolation()}}
			srv := newPolicyServer(t, policies, &fakePolicyEngine{})

			body, err := json.Marshal(map[string]string{"status": "acknowledged", "note": tc.note})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rec := write(t, srv, http.MethodPatch, APIPrefix+"/policy-violations/1", string(body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if len(policies.statuses) != 0 {
				t.Error("an invalid note reached the repository")
			}
		})
	}
}

func TestViolationIdsMustBePositiveIntegers(t *testing.T) {
	srv := newPolicyServer(t, &fakePolicies{}, &fakePolicyEngine{})

	for _, id := range []string{"0", "-1", "abc", "1.5", url.QueryEscape("1 OR 1=1")} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/policy-violations/"+id, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q returned %d, want 400", id, rec.Code)
		}
	}
}

// ---------------------------------------------------------- the evaluator --

// The manual pass must be ASYNCHRONOUS. A synchronous one would let an
// unauthenticated caller hold a request open across a whole-estate sweep.
func TestManualEvaluationIsAsynchronousAndCoalesced(t *testing.T) {
	engine := &fakePolicyEngine{}
	srv := newPolicyServer(t, &fakePolicies{}, engine)

	for i := 0; i < 5; i++ {
		rec := write(t, srv, http.MethodPost, APIPrefix+"/policy/evaluate", `{}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
		}

		var response policyEvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !response.Requested {
			t.Error("the response does not report the request")
		}
	}

	// Each call schedules; the ENGINE coalesces. What matters here is that the
	// handler returned immediately every time rather than running a pass.
	if engine.sweepCount() != 5 {
		t.Errorf("sweeps requested = %d, want 5", engine.sweepCount())
	}
}

// ----------------------------------------------------------- per-container --

func TestContainerPolicyViewDistinguishesCompliantFromUnchecked(t *testing.T) {
	// Never evaluated: no evaluation is invented.
	policies := &fakePolicies{}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/policy-violations/container/abc123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var response policyContainerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Evaluation != nil {
		t.Error("an evaluation was invented for a container that has never been checked")
	}

	// Evaluated and compliant.
	policies.evaluation = &domain.PolicyEvaluation{
		ContainerID: "abc123", Compliant: true, Complete: true, PoliciesEvaluated: 2,
	}
	rec = do(t, srv, http.MethodGet, APIPrefix+"/policy-violations/container/abc123", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Evaluation == nil || !response.Evaluation.Compliant {
		t.Error("a compliant evaluation was not reported")
	}
}

// The path segment is authoritative: a containerId parameter must not widen the
// request to another container.
func TestTheContainerPathSegmentWins(t *testing.T) {
	policies := &fakePolicies{}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/policy-violations/container/abc123?containerId=somethingelse", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if policies.lastViolationFilter.ContainerID != "abc123" {
		t.Errorf("container = %q, want the path segment to win",
			policies.lastViolationFilter.ContainerID)
	}
}

// ------------------------------------------------------------- disabled --

// A deployment with the engine switched off yields a 503 rather than a broken
// route or a misleading empty list.
func TestPolicyEndpointsReportWhenDisabled(t *testing.T) {
	srv := newAuthedServer(Options{
		Health: &fakeHealth{},
		Logger: discardLogger(),
		Config: config.Server{MaxRequestBytes: 4096},
		SnapshotConfig: config.Snapshots{
			WriteRateLimit: 1000, WriteRateBurst: 1000,
		},
		Assets: testAssets(),
	})

	for _, target := range []string{
		APIPrefix + "/policies",
		APIPrefix + "/policies/" + samplePolicy().PolicyID,
		APIPrefix + "/policy-rules",
		APIPrefix + "/policy-summary",
		APIPrefix + "/policy-violations",
		APIPrefix + "/policy-violations/1",
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s returned %d, want 503", target, rec.Code)
		}
	}

	if rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", validPolicyBody); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /policies returned %d, want 503", rec.Code)
	}
	if rec := write(t, srv, http.MethodPost, APIPrefix+"/policy/evaluate", `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /policy/evaluate returned %d, want 503", rec.Code)
	}
}

// The summary is what a dashboard polls, so it must serve the aggregate rather
// than a list the client counts.
func TestPolicySummaryIsServed(t *testing.T) {
	policies := &fakePolicies{summary: domain.PolicySummary{
		Policies: 3, PoliciesTotal: 4, Open: 7, Total: 12,
		ContainersEvaluated: 10, ContainersCompliant: 6, ContainersNonCompliant: 4,
		BySeverity: map[domain.PolicySeverity]int{domain.PolicySeverityCritical: 2},
	}}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	rec := do(t, srv, http.MethodGet, APIPrefix+"/policy-summary", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var summary domain.PolicySummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.Open != 7 || summary.Policies != 3 || summary.ContainersCompliant != 6 {
		t.Errorf("summary = %+v", summary)
	}
}

// The policy writes use their OWN rate limit, separate from the snapshot and
// refresh bucket. Asserted directly, because "we added a second limiter" is
// only useful if the policy handlers actually consult it.
func TestPolicyWritesUseTheirOwnRateLimit(t *testing.T) {
	policies := &fakePolicies{policies: []domain.PolicyDefinition{samplePolicy()}}

	srv := newAuthedServer(Options{
		Health:       &fakeHealth{},
		Policies:     policies,
		PolicyEngine: &fakePolicyEngine{},
		PolicyConfig: config.Policy{
			Enabled:             true,
			MaxRulesPerPolicy:   32,
			MaxValuesPerRule:    32,
			MaxNameBytes:        120,
			MaxDescriptionBytes: 1000,
			MaxNoteBytes:        500,
			// One request, then nothing for a minute.
			WriteRateLimit: 1,
			WriteRateBurst: 1,
		},
		Containers: fakePolicyContainerReader{},
		Logger:     discardLogger(),
		Config:     config.Server{MaxRequestBytes: 16384},
		// The shared bucket is generous, so a 429 below can only have come from
		// the policy one.
		SnapshotConfig: config.Snapshots{WriteRateLimit: 10000, WriteRateBurst: 10000},
		Now:            func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		Assets:         testAssets(),
	})

	if rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", validPolicyBody); rec.Code != http.StatusCreated {
		t.Fatalf("first write returned %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rec := write(t, srv, http.MethodPost, APIPrefix+"/policies", validPolicyBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second write returned %d, want 429", rec.Code)
	}
	// A client that honours Retry-After must be told how long to wait.
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the rate-limited response carries no Retry-After")
	}

	// The snapshot bucket is untouched, so the two limits are genuinely
	// independent rather than one shared counter.
	if rec := do(t, srv, http.MethodPost, APIPrefix+"/inventory/refresh", nil); rec.Code == http.StatusTooManyRequests {
		t.Error("the policy limit consumed the shared write budget")
	}
}

// A repository failure must not reach the client. The message is generic and
// the internal error text stays in the log, where it can name a table or a
// path without handing that to an anonymous caller.
func TestARepositoryFailureIsNotLeakedToTheClient(t *testing.T) {
	policies := &fakePolicies{
		listErr: fmt.Errorf("query policies: no such table: policy_definitions"),
	}
	srv := newPolicyServer(t, policies, &fakePolicyEngine{})

	for _, target := range []string{
		APIPrefix + "/policies",
		APIPrefix + "/policy-violations",
	} {
		rec := do(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s returned %d, want 500", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "policy_definitions") ||
			strings.Contains(rec.Body.String(), "no such table") {
			t.Errorf("%s leaked the internal error: %s", target, rec.Body.String())
		}
	}
}
