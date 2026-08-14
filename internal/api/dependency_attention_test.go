package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The dependency picture as it reaches a container list, and the operation
// summary endpoint.
//
// # The property that matters most here is a COUNT
//
// Rendering a page must not become a reason to issue a query per row. The
// dependency read is asked for ONCE per listing whatever the page size, and
// that is asserted rather than inferred: an N+1 over a small fixture is fast,
// so a timing would never catch it.
//
// # And the second: silence when nothing was established
//
// A deployment without dependency tracking, or one whose graph could not be
// built, must produce exactly the rows it produced before this existed. Not a
// fleet of containers claiming their dependencies are satisfied.

// attentionServerWith builds a server whose listing carries dependency facts.
func attentionServerWith(
	containers *fakeContainers,
	deps *fakeDependencies,
) *Server {
	options := Options{
		Health:     &fakeHealth{},
		Config:     config.Server{MaxRequestBytes: 4096},
		Assets:     testAssets(),
		Containers: containers,
	}
	if deps != nil {
		options.Dependencies = deps
	}
	srv, _, _ := asRole(options, domain.RoleAdministrator)
	return srv
}

// listRows reads the attention block off every row of a container listing.
func listRows(t *testing.T, srv *Server) []domain.ContainerAttention {
	t.Helper()

	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/containers", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /containers = %d (%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Items []struct {
			Name      string                    `json:"name"`
			Attention domain.ContainerAttention `json:"attention"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	out := make([]domain.ContainerAttention, 0, len(body.Items))
	for _, item := range body.Items {
		out = append(out, item.Attention)
	}
	return out
}

// A page of containers costs ONE dependency read.
func TestAContainerListingReadsDependenciesOnce(t *testing.T) {
	t.Parallel()

	for _, size := range []int{1, 25, 200} {
		containers := &fakeContainers{summaries: pageOf(size), total: size}
		deps := populatedDependencies()
		deps.facts = map[string]service.DependencyFacts{}

		srv := attentionServerWith(containers, deps)
		rows := listRows(t, srv)

		if len(rows) != size {
			t.Fatalf("%d containers produced %d rows", size, len(rows))
		}
		if deps.attentionCalls != 1 {
			t.Fatalf("a page of %d containers cost %d dependency reads, want 1\n"+
				"\trendering a list must not become a query per row",
				size, deps.attentionCalls)
		}
	}
}

// A deployment without dependency tracking asserts nothing.
func TestAListingWithoutDependencyTrackingClaimsNothing(t *testing.T) {
	t.Parallel()

	containers := &fakeContainers{summaries: pageOf(3), total: 3}
	srv := attentionServerWith(containers, nil)

	for _, row := range listRows(t, srv) {
		if row.DependencyKnown {
			t.Error("a row claims the dependency subsystem answered when it is not wired")
		}
		if row.DependencyState != "" {
			t.Errorf("DependencyState = %q, want empty", row.DependencyState)
		}
	}
}

// A dependency read that FAILED is silence, not a claim in either direction.
func TestAFailedDependencyReadLeavesTheListingUnchanged(t *testing.T) {
	t.Parallel()

	containers := &fakeContainers{summaries: pageOf(3), total: 3}
	deps := populatedDependencies()
	deps.factsErr = service.ErrDependencyGraphUnavailable

	srv := attentionServerWith(containers, deps)
	rows := listRows(t, srv)

	if len(rows) != 3 {
		t.Fatalf("the listing returned %d rows; a display read that failed must "+
			"not take the inventory page down", len(rows))
	}
	for _, row := range rows {
		if row.DependencyKnown {
			t.Error("a row claims a dependency answer that never arrived")
		}
	}
}

// The verdicts reach the row.
func TestDependencyFactsReachTheContainerRow(t *testing.T) {
	t.Parallel()

	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	name := containers.summaries[0].Name

	cases := []struct {
		label string
		facts service.DependencyFacts
		want  domain.AttentionState
	}{
		{
			label: "a loop",
			facts: service.DependencyFacts{State: domain.DependencyCycle},
			want:  domain.AttentionDependencyCycle,
		},
		{
			label: "an unresolvable dependency",
			facts: service.DependencyFacts{State: domain.DependencyMissing},
			want:  domain.AttentionDependencyUnresolved,
		},
		{
			label: "a failed reattachment",
			facts: service.DependencyFacts{
				State: domain.DependencySatisfied, RebindFailed: true,
				RebindProvider: "gluetun",
			},
			want: domain.AttentionDependencyFailed,
		},
	}

	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			deps := populatedDependencies()
			deps.facts = map[string]service.DependencyFacts{name: test.facts}

			rows := listRows(t, attentionServerWith(containers, deps))
			if len(rows) != 1 {
				t.Fatalf("rows = %d", len(rows))
			}
			if rows[0].State != test.want {
				t.Fatalf("state = %q, want %q", rows[0].State, test.want)
			}
			if !rows[0].DependencyKnown {
				t.Error("the row does not record that the subsystem answered")
			}
		})
	}
}

// A container waiting on its dependency looks exactly as it did.
func TestWaitingDoesNotChangeARow(t *testing.T) {
	t.Parallel()

	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	name := containers.summaries[0].Name

	deps := populatedDependencies()
	deps.facts = map[string]service.DependencyFacts{
		name: {State: domain.DependencyWaiting, BlockedBy: "postgres"},
	}

	rows := listRows(t, attentionServerWith(containers, deps))
	if rows[0].State == domain.AttentionDependencyBlocked ||
		rows[0].State == domain.AttentionDependencyFailed {
		t.Fatalf("waiting produced %q; waiting for a dependency is the system "+
			"working", rows[0].State)
	}
	// The fact is still carried, so a detail page can explain the delay.
	if rows[0].DependencyState != domain.DependencyWaiting {
		t.Error("the waiting state was not carried through")
	}
	if rows[0].DependencyBlockedBy != "postgres" {
		t.Error("the container being waited on was not carried through")
	}
}

// ------------------------------------------------------------ operations --

func TestDependencyOperationsAreReadableByEveryRole(t *testing.T) {
	t.Parallel()

	for _, role := range domain.Roles {
		deps := populatedDependencies()
		deps.operations = []service.DependencyOperationSummary{{
			Operation: domain.DependencyOperation{
				OperationID: "depop_0123456789abcdef0123",
				Provider:    "gluetun",
				State:       domain.OperationFailed,
				Failure:     domain.OperationFailureRebind,
			},
			ProviderVerified: true,
			NeedsAttention:   true,
		}}

		srv, _ := dependencyServer(role, deps)
		rec := send(srv, authed(request(http.MethodGet,
			APIPrefix+"/dependencies/operations", "")))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /dependencies/operations as %s = %d", role, rec.Code)
		}
	}
}

func TestDependencyOperationsRefuseAnonymousCallers(t *testing.T) {
	t.Parallel()

	srv, _ := dependencyServer(domain.RoleAdministrator, populatedDependencies())
	rec := send(srv, anonymous(request(http.MethodGet,
		APIPrefix+"/dependencies/operations", "")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", rec.Code)
	}
}

// The listing reports its own bound, so truncation is never silent.
func TestDependencyOperationsStateTheirBound(t *testing.T) {
	t.Parallel()

	deps := populatedDependencies()
	srv, _ := dependencyServer(domain.RoleAdministrator, deps)

	rec := send(srv, authed(request(http.MethodGet,
		APIPrefix+"/dependencies/operations", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}

	var body struct {
		Limit int `json:"limit"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Limit <= 0 {
		t.Error("the listing does not state the bound it was truncated at")
	}
}

// There is no route that acts on an operation.
//
// Not a claim about this handler: a claim about the ROUTE TABLE. Retrying a
// reattachment goes through the acquisition and recreation services, each of
// which re-runs its own preflight, and there is deliberately no group rollback.
func TestNoRouteActsOnADependencyOperation(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, route := range (&Server{}).routeTable() {
		if !strings.Contains(route.pattern, "/dependencies/operations") {
			continue
		}
		checked++
		if route.method != http.MethodGet && route.method != "" {
			t.Errorf("%s %s exists; the operations surface is read-only",
				route.method, route.pattern)
		}
	}
	if checked == 0 {
		t.Fatal("no operations route was checked; the test would pass vacuously")
	}

	// And no route anywhere names a retry or a group rollback.
	for _, route := range (&Server{}).routeTable() {
		lowered := strings.ToLower(route.pattern)
		for _, forbidden := range []string{"rebind", "reattach", "group-rollback"} {
			if strings.Contains(lowered, forbidden) {
				t.Errorf("the route %s exposes %q; a reattachment is performed by "+
					"the services that own the capability, never by a route of its own",
					route.pattern, forbidden)
			}
		}
	}
}

// The operations listing leaks no storage detail, healthy or failing.
func TestDependencyOperationsLeakNoStorageDetail(t *testing.T) {
	t.Parallel()

	for _, failing := range []bool{false, true} {
		deps := populatedDependencies()
		if failing {
			deps.operationsErr = service.ErrDependencyGraphUnavailable
		}
		srv, _ := dependencyServer(domain.RoleAdministrator, deps)

		rec := send(srv, authed(request(http.MethodGet,
			APIPrefix+"/dependencies/operations", "")))
		lowered := strings.ToLower(rec.Body.String())
		for _, marker := range []string{
			"select ", "dependency_operation_members", "sqlite", "sql:", ".go:",
		} {
			if strings.Contains(lowered, marker) {
				t.Errorf("failing=%v leaked %q: %s", failing, marker, rec.Body.String())
			}
		}
	}
}
