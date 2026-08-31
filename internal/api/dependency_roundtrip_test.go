package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The four dependency routes, exercised over real HTTP.
//
// # Why these are round trips rather than handler calls
//
// The authorization decision does not live in a handler. It lives in the
// middleware, which reads the route table -- so a test that called
// handleDependencyCreate directly would establish nothing about whether an
// operator can reach it. Everything below goes through Server.ServeHTTP, which
// is the only path that runs the session check, the permission check, the CSRF
// check, and the rate budget in the order production runs them.
//
// # What the negative cases are actually protecting
//
//	anonymous          -> 401, before the handler exists as far as the caller knows
//	viewer / operator  -> 403 on both writes, because DELETE removes a constraint
//	no CSRF header     -> 403, which is what a cross-site form submission gets
//	unknown field      -> 400, so nobody believes an option they sent took effect
//	malformed id       -> 404, the same answer an absent one gets

// fakeDependencies is a DependencyService that answers from fixtures.
//
// Note what it CANNOT be asked to do: there is no method here that pulls,
// recreates, or rolls anything back, because the interface has none. That is the
// point being preserved, and a fake is the cheapest place to notice if the
// interface ever grows one.
type fakeDependencies struct {
	listing    service.DependencyListing
	listErr    error
	forName    service.ContainerDependencies
	forErr     error
	graph      service.DependencyGraphProjection
	graphErr   error
	createErr  error
	created    domain.WorkloadDependency
	deleteErr  error
	deleted    domain.WorkloadDependency
	lastCreate [2]string
	lastDelete string
	requester  domain.Requester

	// The whole-estate picture a listing reads, and how many times it was
	// asked. The count is the N+1 guard: one call per page, never one per row.
	facts          map[string]service.DependencyFacts
	factsErr       error
	attentionCalls int

	operations    []service.DependencyOperationSummary
	operationsErr error
}

func (f *fakeDependencies) List(context.Context) (service.DependencyListing, error) {
	return f.listing, f.listErr
}

func (f *fakeDependencies) ForContainer(_ context.Context, name string) (service.ContainerDependencies, error) {
	if f.forErr != nil {
		return service.ContainerDependencies{}, f.forErr
	}
	result := f.forName
	if result.Container == "" {
		result.Container = name
	}
	return result, nil
}

func (f *fakeDependencies) Graph(context.Context) (service.DependencyGraphProjection, error) {
	return f.graph, f.graphErr
}

func (f *fakeDependencies) AttentionFacts(context.Context) (map[string]service.DependencyFacts, error) {
	f.attentionCalls++
	return f.facts, f.factsErr
}

func (f *fakeDependencies) RecentOperations(
	_ context.Context, _ int,
) ([]service.DependencyOperationSummary, error) {
	return f.operations, f.operationsErr
}

func (f *fakeDependencies) CreateOperatorDependency(
	_ context.Context, dependent, dependency string, requestedBy domain.Requester,
) (domain.WorkloadDependency, error) {
	f.lastCreate = [2]string{dependent, dependency}
	f.requester = requestedBy
	if f.createErr != nil {
		return domain.WorkloadDependency{}, f.createErr
	}
	// The first check the real validator makes, reproduced here so the fake
	// cannot be MORE permissive than the service it stands in for. Without it a
	// round trip carrying two empty names would answer 201, and the test would
	// be describing the fake rather than the endpoint.
	if !domain.ValidContainerName(domain.NormaliseContainerName(dependent)) ||
		!domain.ValidContainerName(domain.NormaliseContainerName(dependency)) {
		return domain.WorkloadDependency{},
			service.ErrDependencyRefused{Refusal: domain.DependencyRefusalMalformed}
	}
	created := f.created
	if created.DependencyID == "" {
		created = domain.WorkloadDependency{
			// The fixture id every case in this file uses: the prefix plus
			// twenty hex characters, well-formed and accepted by the validator.
			//
			// A repeated character rather than a varied run, on purpose.
			// Nothing here tests entropy -- what the id CONTAINS is never
			// asserted, only that requests carrying it are answered
			// identically. One probe below appends an SQL injection payload to
			// this id, and the probe beside it names /etc/passwd; a varied hex
			// run in that position is a secret-scanner keyword and a
			// high-entropy token sitting next to each other, which is the exact
			// shape such a scanner exists to catch. Keeping the id dull keeps
			// the scanner useful and changes nothing these tests prove.
			// See .gitleaksignore for the finding this file used to produce.
			DependencyID: "dep_aaaaaaaaaaaaaaaaaaaa",
			Dependent:    dependent,
			Dependency:   dependency,
			Source:       domain.DependencyOperator,
		}
	}
	return created, nil
}

func (f *fakeDependencies) DeleteOperatorDependency(
	_ context.Context, dependencyID string,
) (domain.WorkloadDependency, error) {
	f.lastDelete = dependencyID
	if f.deleteErr != nil {
		return domain.WorkloadDependency{}, f.deleteErr
	}
	removed := f.deleted
	if removed.DependencyID == "" {
		removed = operatorEdgeFixture()
	}
	return removed, nil
}

// errorParts reads the code and message out of an error response.
//
// Deliberately not the whole body: every response carries its own request id,
// which differs by design and would make two identical refusals compare unequal.
func errorParts(t *testing.T, rec *httptest.ResponseRecorder) (ErrorCode, string) {
	t.Helper()

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return ErrorCode(body.Error.Code), body.Error.Message
}

// dependencyServer builds a server wired for the dependency routes.
func dependencyServer(role domain.Role, deps *fakeDependencies) (*Server, *stubAudit) {
	srv, _, audit := asRole(Options{
		Health:       &fakeHealth{},
		Config:       config.Server{MaxRequestBytes: 4096},
		Assets:       testAssets(),
		Dependencies: deps,
		Containers: &fakeContainers{
			detail: &domain.ContainerDetail{
				Overview: domain.ContainerSummary{ID: "abcdef0123456789", Name: "sonarr"},
			},
		},
	}, role)
	return srv, audit
}

// populatedDependencies is the estate most of these tests read.
func populatedDependencies() *fakeDependencies {
	return &fakeDependencies{
		listing: service.DependencyListing{
			Edges: []domain.WorkloadDependency{
				namespaceEdgeFixture(),
				operatorEdgeFixture(),
			},
		},
		graph: service.DependencyGraphProjection{
			Stages: [][]string{{"gluetun", "postgres"}, {"api", "sonarr"}},
			Edges: []domain.WorkloadDependency{
				namespaceEdgeFixture(),
				operatorEdgeFixture(),
			},
		},
		forName: service.ContainerDependencies{
			Container: "sonarr",
			DependsOn: []domain.WorkloadDependency{namespaceEdgeFixture()},
			State:     domain.DependencySatisfied,
			Detail:    domain.DependencySatisfied.Explain(),
		},
	}
}

// ------------------------------------------------------------------ reads --

// Every role may read every dependency projection.
func TestDependencyReadsSucceedForEveryRole(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		APIPrefix + "/dependencies",
		APIPrefix + "/dependencies/graph",
		APIPrefix + "/dependencies/container/abcdef0123456789",
	} {
		for _, role := range domain.Roles {
			srv, _ := dependencyServer(role, populatedDependencies())
			rec := send(srv, authed(request(http.MethodGet, target, "")))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s as %s = %d, want 200", target, role, rec.Code)
			}
		}
	}
}

// An unauthenticated caller learns nothing, on any of them.
func TestDependencyReadsRefuseAnonymousCallers(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		APIPrefix + "/dependencies",
		APIPrefix + "/dependencies/graph",
		APIPrefix + "/dependencies/container/abcdef0123456789",
	} {
		srv, _ := dependencyServer(domain.RoleAdministrator, populatedDependencies())
		rec := send(srv, anonymous(request(http.MethodGet, target, "")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", target, rec.Code)
		}
		// And no relationship leaked into the refusal.
		if strings.Contains(rec.Body.String(), "gluetun") {
			t.Errorf("anonymous GET %s leaked a container name: %s", target, rec.Body.String())
		}
	}
}

// The listing renders operator-facing words, and marks only the stored row
// deletable.
func TestDependencyListingRendersOriginAndDeletability(t *testing.T) {
	t.Parallel()

	srv, _ := dependencyServer(domain.RoleAdministrator, populatedDependencies())
	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/dependencies", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dependencies = %d", rec.Code)
	}

	var body struct {
		Items []dependencyResponse `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 {
		t.Fatalf("total = %d, want 2", body.Total)
	}

	var detected, configured int
	for _, item := range body.Items {
		switch item.Source {
		case domain.DependencyOperator:
			configured++
			if !item.Deletable || item.DependencyID == "" {
				t.Error("a configured relationship is not removable through the API")
			}
		default:
			detected++
			if item.Deletable {
				t.Error("a detected relationship was reported as removable")
			}
			if item.DependencyID != "" {
				t.Error("a detected relationship carries an id a caller could DELETE")
			}
		}
		if item.Kind == "" || item.Origin == "" {
			t.Errorf("relationship %s->%s has no operator-facing wording",
				item.Dependent, item.Dependency)
		}
	}
	if detected != 1 || configured != 1 {
		t.Fatalf("detected=%d configured=%d, want 1 and 1", detected, configured)
	}
}

// The graph is a projection: stages and edges, and nothing that could act.
func TestDependencyGraphReturnsTheOrder(t *testing.T) {
	t.Parallel()

	srv, _ := dependencyServer(domain.RoleViewer, populatedDependencies())
	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/dependencies/graph", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dependencies/graph = %d", rec.Code)
	}

	var body struct {
		Stages [][]string `json:"stages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Stages) != 2 || len(body.Stages[0]) != 2 {
		t.Fatalf("stages = %v", body.Stages)
	}
}

// A graph that could not be built is 503, and says so without a stack trace.
func TestDependencyGraphFailureIsUnavailableNotEmpty(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	deps.graphErr = service.ErrDependencyGraphUnavailable

	srv, _ := dependencyServer(domain.RoleAdministrator, deps)
	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/dependencies/graph", "")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unbuildable graph = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != CodeUnavailable {
		t.Errorf("code = %q, want %q", code, CodeUnavailable)
	}
	// 503 rather than 200-with-no-stages is the whole point: an empty order
	// would read as "nothing constrains anything", which is the opposite of
	// what happened.
	if strings.Contains(rec.Body.String(), `"stages"`) {
		t.Error("a failed graph answered with a stages field")
	}
}

// An unknown container is 404, not an empty relationship set.
func TestDependenciesForUnknownContainerAre404(t *testing.T) {
	t.Parallel()

	srv, _, _ := asRole(Options{
		Health:       &fakeHealth{},
		Config:       config.Server{MaxRequestBytes: 4096},
		Assets:       testAssets(),
		Dependencies: populatedDependencies(),
		Containers:   &fakeContainers{getErr: store.ErrNotFound},
	}, domain.RoleAdministrator)

	rec := send(srv, authed(request(http.MethodGet,
		APIPrefix+"/dependencies/container/abcdef0123456789", "")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown container = %d, want 404", rec.Code)
	}
}

// ----------------------------------------------------------------- create --

// An administrator records an ordering, and the write is audited.
func TestDependencyCreateSucceedsForAnAdministrator(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	srv, audit := dependencyServer(domain.RoleAdministrator, deps)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/dependencies",
		`{"dependent":"api","dependency":"postgres"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /dependencies = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	if deps.lastCreate != [2]string{"api", "postgres"} {
		t.Errorf("the service received %v", deps.lastCreate)
	}
	if deps.requester.UserID == "" {
		t.Error("the write did not carry who performed it")
	}
	if len(audit.recorded(domain.AuditDependencyCreated)) == 0 {
		t.Error("a recorded ordering was not audited")
	}

	var body dependencyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Deletable || body.Origin != "Configured by you" {
		t.Errorf("the created relationship rendered as %+v", body)
	}
}

// A viewer and an operator are refused, and nothing reaches the service.
func TestDependencyWritesAreRefusedBelowAdministrator(t *testing.T) {
	t.Parallel()

	for _, role := range []domain.Role{domain.RoleViewer, domain.RoleOperator} {
		for _, call := range []struct {
			method, target, body string
		}{
			{http.MethodPost, APIPrefix + "/dependencies",
				`{"dependent":"api","dependency":"postgres"}`},
			{http.MethodDelete, APIPrefix + "/dependencies/dep_aaaaaaaaaaaaaaaaaaaa", ""},
		} {
			deps := populatedDependencies()
			srv, _ := dependencyServer(role, deps)

			rec := send(srv, authed(request(call.method, call.target, call.body)))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d, want 403", call.method, call.target, role, rec.Code)
			}
			// The refusal happened in the middleware, so the service was never
			// consulted. A 403 produced INSIDE a handler would mean the handler
			// ran with the caller's input.
			if deps.lastCreate != [2]string{} || deps.lastDelete != "" {
				t.Errorf("%s reached the dependency service as %s", call.method, role)
			}
		}
	}
}

// Anonymous writes are 401, and reach nothing.
func TestDependencyWritesRefuseAnonymousCallers(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	srv, _ := dependencyServer(domain.RoleAdministrator, deps)

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/dependencies",
		`{"dependent":"api","dependency":"postgres"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d, want 401", rec.Code)
	}
	rec = send(srv, anonymous(request(http.MethodDelete,
		APIPrefix+"/dependencies/dep_aaaaaaaaaaaaaaaaaaaa", "")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE = %d, want 401", rec.Code)
	}
	if deps.lastCreate != [2]string{} || deps.lastDelete != "" {
		t.Error("an anonymous request reached the dependency service")
	}
}

// A session cookie without the CSRF header is exactly a cross-site submission.
func TestDependencyWritesRequireTheCSRFHeader(t *testing.T) {
	t.Parallel()

	for _, call := range []struct{ method, target, body string }{
		{http.MethodPost, APIPrefix + "/dependencies",
			`{"dependent":"api","dependency":"postgres"}`},
		{http.MethodDelete, APIPrefix + "/dependencies/dep_aaaaaaaaaaaaaaaaaaaa", ""},
	} {
		deps := populatedDependencies()
		srv, auth, _ := asRole(Options{
			Health:       &fakeHealth{},
			Config:       config.Server{MaxRequestBytes: 4096},
			Assets:       testAssets(),
			Dependencies: deps,
			Containers:   &fakeContainers{},
		}, domain.RoleAdministrator)
		auth.mu.Lock()
		auth.acceptAnyCSRF = false
		auth.mu.Unlock()

		req := request(call.method, call.target, call.body)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})

		rec := send(srv, req)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != CodeCSRF {
			t.Errorf("%s without a CSRF header = %d (%s), want 403/%s",
				call.method, rec.Code, errorCode(t, rec), CodeCSRF)
		}
		if deps.lastCreate != [2]string{} || deps.lastDelete != "" {
			t.Errorf("%s reached the service without a CSRF header", call.method)
		}
	}
}

// A body that is not one JSON object is refused, and never partially applied.
func TestDependencyCreateRefusesMalformedBodies(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"notJSON":       `dependent=api`,
		"truncated":     `{"dependent":"api",`,
		"array":         `[{"dependent":"api","dependency":"postgres"}]`,
		"twoObjects":    `{"dependent":"api","dependency":"postgres"}{"dependent":"x","dependency":"y"}`,
		"wrongType":     `{"dependent":123,"dependency":"postgres"}`,
		"unknownField":  `{"dependent":"api","dependency":"postgres","source":"dockerNetworkNamespace"}`,
		"unknownField2": `{"dependent":"api","dependency":"postgres","image":"nginx:1.27"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			deps := populatedDependencies()
			srv, _ := dependencyServer(domain.RoleAdministrator, deps)

			rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/dependencies", body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d (%s), want 400", name, rec.Code, rec.Body.String())
			}
			if deps.lastCreate != [2]string{} {
				t.Error("a malformed body reached the dependency service")
			}
		})
	}
}

// A request with no body at all is 415, not 400.
//
// Its own test because the answer is different and correct: a POST with no
// Content-Type is refused by the media-type guard before the decoder is
// reached, so the caller is told what is wrong with the REQUEST rather than
// what is wrong with a body they did not send.
func TestDependencyCreateRefusesARequestWithNoBody(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	srv, _ := dependencyServer(domain.RoleAdministrator, deps)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/dependencies", "")))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("a bodyless POST = %d, want 415", rec.Code)
	}
	if deps.lastCreate != [2]string{} {
		t.Error("a bodyless request reached the dependency service")
	}
}

// A name that is too long, or carries a control character, is refused.
//
// The control character is the one that matters: it is what would let a name
// forge a line in the audit log or the application log.
func TestDependencyCreateRefusesUnacceptableNames(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"tooLong": `{"dependent":"` + strings.Repeat("a", 300) + `","dependency":"postgres"}`,
		"newline": `{"dependent":"api\nlevel=error msg=owned","dependency":"postgres"}`,
		// Written as JSON escapes rather than as literal bytes: what matters is
		// that the DECODED name carries a control character.
		"nullByte":     `{"dependent":"api\u0000","dependency":"postgres"}`,
		"escapeCode":   `{"dependent":"api\u001b[31m","dependency":"postgres"}`,
		"emptyBothEnd": `{"dependent":"","dependency":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			deps := populatedDependencies()
			srv, _ := dependencyServer(domain.RoleAdministrator, deps)

			rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/dependencies", body)))
			// Either the text guard refuses it (400) or the estate does (409).
			// What must not happen is a 2xx.
			if rec.Code < 400 {
				t.Fatalf("%s = %d, want a refusal", name, rec.Code)
			}
		})
	}
}

// Every refusal the estate can produce comes back as a conflict carrying
// HarborMaster's own sentence.
func TestDependencyCreateRefusalsAreConflicts(t *testing.T) {
	t.Parallel()

	for _, refusal := range domain.DependencyRefusals {
		t.Run(string(refusal), func(t *testing.T) {
			t.Parallel()

			deps := populatedDependencies()
			deps.createErr = service.ErrDependencyRefused{Refusal: refusal}
			srv, audit := dependencyServer(domain.RoleAdministrator, deps)

			rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/dependencies",
				`{"dependent":"api","dependency":"postgres"}`)))
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s = %d, want 409", refusal, rec.Code)
			}
			if code := errorCode(t, rec); code != CodeConflict {
				t.Errorf("code = %q, want %q", code, CodeConflict)
			}
			// The message is HarborMaster's, and names WHICH check said no.
			if !strings.Contains(rec.Body.String(), refusal.Explain()) {
				t.Errorf("the response does not carry the refusal's own words: %s",
					rec.Body.String())
			}
			// A refused write is audited: enumeration looks exactly like this.
			if len(audit.recorded(domain.AuditDependencyCreated)) == 0 {
				t.Error("a refused write was not audited")
			}
		})
	}
}

// ----------------------------------------------------------------- delete --

// An administrator removes a configured ordering, and it is audited.
func TestDependencyDeleteSucceedsForAnAdministrator(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	srv, audit := dependencyServer(domain.RoleAdministrator, deps)

	rec := send(srv, authed(request(http.MethodDelete,
		APIPrefix+"/dependencies/dep_aaaaaaaaaaaaaaaaaaaa", "")))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if deps.lastDelete != "dep_aaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("the service received %q", deps.lastDelete)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 carried a body: %q", rec.Body.String())
	}
	if len(audit.recorded(domain.AuditDependencyDeleted)) == 0 {
		t.Error("a removed ordering was not audited")
	}
}

// An id that names nothing is 404 -- and so is one that is not an id at all.
//
// The two must be indistinguishable, or the endpoint becomes an oracle for
// which relationship ids exist.
func TestDependencyDeleteAnswersNotFoundIdentically(t *testing.T) {
	t.Parallel()

	// A well-formed id the store does not have.
	deps := populatedDependencies()
	deps.deleteErr = store.ErrNotFound
	srv, _ := dependencyServer(domain.RoleAdministrator, deps)

	absent := send(srv, authed(request(http.MethodDelete,
		APIPrefix+"/dependencies/dep_ffffffffffffffffffff", "")))
	if absent.Code != http.StatusNotFound {
		t.Fatalf("an absent id = %d, want 404", absent.Code)
	}
	// The code and the message are what a caller can compare. The request id
	// differs on every response by design, so it is not part of the answer.
	absentCode, absentMessage := errorParts(t, absent)

	// A string that is not an id at all, including the shapes an attacker
	// reaches for first.
	for _, bad := range []string{
		"not-an-id",
		"dep_",
		"..%2F..%2Fetc%2Fpasswd",
		"dep_aaaaaaaaaaaaaaaaaaaa'%20OR%20'1'='1",
		"%3Cscript%3Ealert(1)%3C%2Fscript%3E",
		strings.Repeat("a", 400),
	} {
		fresh := populatedDependencies()
		srv, _ := dependencyServer(domain.RoleAdministrator, fresh)

		rec := send(srv, authed(request(http.MethodDelete,
			APIPrefix+"/dependencies/"+bad, "")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("DELETE %q = %d, want 404", bad, rec.Code)
		}
		code, message := errorParts(t, rec)
		if code != absentCode || message != absentMessage {
			t.Errorf("DELETE %q answered %s/%q, but an absent id answers %s/%q\n"+
				"\tthe two must be indistinguishable, or the endpoint tells a "+
				"caller which ids exist", bad, code, message, absentCode, absentMessage)
		}
		// A malformed id is refused before the service is consulted.
		if fresh.lastDelete != "" {
			t.Errorf("the malformed id %q reached the dependency service", bad)
		}
	}
}

// A detected relationship cannot be named in a DELETE, because it has no id.
//
// Structural rather than checked: discovery derives these on every read and
// stores no row, so the listing offers no id for a caller to send.
func TestDetectedRelationshipsOfferNoIdToDelete(t *testing.T) {
	t.Parallel()

	srv, _ := dependencyServer(domain.RoleAdministrator, populatedDependencies())
	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/dependencies", "")))

	var body struct {
		Items []dependencyResponse `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	checked := 0
	for _, item := range body.Items {
		if item.Source == domain.DependencyOperator {
			continue
		}
		checked++
		if item.DependencyID != "" {
			t.Errorf("the detected relationship %s->%s exposes id %q",
				item.Dependent, item.Dependency, item.DependencyID)
		}
	}
	if checked == 0 {
		t.Fatal("no detected relationship was checked; the test would pass vacuously")
	}
}

// ------------------------------------------------------------- disclosure --

// No response carries a query, a table name, or a driver error.
func TestDependencyResponsesLeakNoStorageDetail(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	deps.listErr = nil

	cases := []struct{ method, target, body string }{
		{http.MethodGet, APIPrefix + "/dependencies", ""},
		{http.MethodGet, APIPrefix + "/dependencies/graph", ""},
		{http.MethodGet, APIPrefix + "/dependencies/container/abcdef0123456789", ""},
		{http.MethodPost, APIPrefix + "/dependencies", `{"dependent":"api","dependency":"postgres"}`},
		{http.MethodDelete, APIPrefix + "/dependencies/dep_aaaaaaaaaaaaaaaaaaaa", ""},
	}

	// Once healthy, once with the store failing underneath: an error path is
	// where a driver message would escape.
	for _, failing := range []bool{false, true} {
		for _, call := range cases {
			fresh := populatedDependencies()
			if failing {
				fresh.listErr = store.ErrNotFound
				fresh.graphErr = service.ErrDependencyGraphUnavailable
				fresh.forErr = store.ErrNotFound
				fresh.createErr = store.ErrNotFound
				fresh.deleteErr = store.ErrNotFound
			}
			srv, _ := dependencyServer(domain.RoleAdministrator, fresh)
			rec := send(srv, authed(request(call.method, call.target, call.body)))

			lowered := strings.ToLower(rec.Body.String())
			for _, marker := range []string{
				"select ", "insert ", "update ", "delete from", "sqlite",
				"workload_dependencies", "dependency_operations", "constraint",
				"goroutine", ".go:", "sql:",
			} {
				if strings.Contains(lowered, marker) {
					t.Errorf("%s %s (failing=%v) leaked %q: %s",
						call.method, call.target, failing, marker, rec.Body.String())
				}
			}
		}
	}
}

// A container name is returned as data, never as a rendered fragment.
//
// The API is JSON, so the guarantee is that the encoder escapes: a name that
// looked like markup must come back escaped, and must not appear raw.
func TestDependencyNamesAreReturnedAsEncodedData(t *testing.T) {
	t.Parallel()

	// Names like this cannot be created -- ValidContainerName's allowlist
	// refuses them -- but Docker is not the only thing that ever wrote a row, so
	// the RENDERING must be safe regardless of how one got there.
	hostile := `<img src=x onerror=alert(1)>`
	deps := populatedDependencies()
	deps.listing = service.DependencyListing{
		Edges: []domain.WorkloadDependency{{
			Dependent:  hostile,
			Dependency: "gluetun",
			Source:     domain.DependencyNetworkNamespace,
		}},
	}

	srv, _ := dependencyServer(domain.RoleAdministrator, deps)
	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/dependencies", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<img") {
		t.Errorf("a name was returned without escaping: %s", rec.Body.String())
	}
	if content := rec.Header().Get("Content-Type"); !strings.HasPrefix(content, "application/json") {
		t.Errorf("Content-Type = %q; a JSON body served as anything else can be "+
			"rendered by a browser", content)
	}
}

// The subsystem being absent is 503 on every route, never a 500 or an empty
// success.
func TestDependencyRoutesReportBeingUnwired(t *testing.T) {
	t.Parallel()

	srv, _, _ := asRole(Options{
		Health:     &fakeHealth{},
		Config:     config.Server{MaxRequestBytes: 4096},
		Assets:     testAssets(),
		Containers: &fakeContainers{},
	}, domain.RoleAdministrator)

	for _, call := range []struct{ method, target, body string }{
		{http.MethodGet, APIPrefix + "/dependencies", ""},
		{http.MethodGet, APIPrefix + "/dependencies/graph", ""},
		{http.MethodGet, APIPrefix + "/dependencies/container/abcdef0123456789", ""},
		{http.MethodPost, APIPrefix + "/dependencies", `{"dependent":"api","dependency":"postgres"}`},
		{http.MethodDelete, APIPrefix + "/dependencies/dep_aaaaaaaaaaaaaaaaaaaa", ""},
	} {
		rec := send(srv, authed(request(call.method, call.target, call.body)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with no dependency service = %d, want 503",
				call.method, call.target, rec.Code)
		}
	}
}
