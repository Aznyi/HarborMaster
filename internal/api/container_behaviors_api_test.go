package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The saved-behaviour summary endpoint (C2.2).
//
// # The two things this endpoint must not become
//
// A CONTROL SURFACE. The automation workspace shows which containers carry a
// saved update behaviour; the container's own page remains the only place one
// is chosen. So this is a GET, and the tests below assert that no other method
// reaches a handler and that reading it writes nothing.
//
// A REQUEST EXPLOSION. One bounded request answers the whole question. A page
// that listed preferences and then asked about each container would turn an
// overview into an estate evaluation.

// stubBehaviorSummary serves a fixed summary and records what was called.
type stubBehaviorSummary struct {
	summary service.ContainerBehaviorSummary
	err     error
	reads   int
	// wrote records any mutation attempt, which must stay zero.
	wrote int
}

func (s *stubBehaviorSummary) Behavior(context.Context, string) (service.ContainerUpdateBehavior, error) {
	return service.ContainerUpdateBehavior{}, errors.New("the summary must not read per container")
}

func (s *stubBehaviorSummary) BehaviorSummary(context.Context) (service.ContainerBehaviorSummary, error) {
	s.reads++
	return s.summary, s.err
}

func (s *stubBehaviorSummary) SetBehavior(context.Context, string, domain.UpdateBehavior,
	service.Actor,
) (service.ContainerUpdateBehavior, error) {
	s.wrote++
	return service.ContainerUpdateBehavior{}, errors.New("not for this test")
}

func (s *stubBehaviorSummary) ClearBehavior(context.Context, string,
	service.Actor,
) (service.ContainerUpdateBehavior, error) {
	s.wrote++
	return service.ContainerUpdateBehavior{}, errors.New("not for this test")
}

func behaviorSummaryFixture() service.ContainerBehaviorSummary {
	return service.ContainerBehaviorSummary{
		Items: []service.ContainerBehaviorItem{
			{ContainerName: "grafana", Behavior: domain.BehaviorAutomatic, Present: true, ContainerID: "grafana-id"},
			{ContainerName: "old-thing", Behavior: domain.BehaviorReviewFirst, Present: false},
		},
		Counts: map[domain.UpdateBehavior]int{
			domain.BehaviorAutomatic:   1,
			domain.BehaviorReviewFirst: 0,
			domain.BehaviorMonitorOnly: 0,
		},
		Total: 1,
		Stale: 1,
	}
}

func behaviorSummaryServer(t *testing.T, stub *stubBehaviorSummary, role domain.Role) *Server {
	t.Helper()
	return newAuthedServer(Options{
		Auth:                 newStubAuth(role),
		ContainerPreferences: stub,
	})
}

const behaviorsPath = APIPrefix + "/containers/update-behaviors"

func getBehaviors(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, behaviorsPath, nil)))
	return rec
}

func TestTheBehaviorSummaryIsServed(t *testing.T) {
	stub := &stubBehaviorSummary{summary: behaviorSummaryFixture()}
	rec := getBehaviors(t, behaviorSummaryServer(t, stub, domain.RoleAdministrator))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Items []struct {
			ContainerName string `json:"containerName"`
			Behavior      string `json:"behavior"`
			Present       bool   `json:"present"`
			ContainerID   string `json:"containerId"`
		} `json:"items"`
		Counts map[string]int `json:"counts"`
		Total  int            `json:"total"`
		Stale  int            `json:"stale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v -- %s", err, rec.Body.String())
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Total != 1 || body.Stale != 1 {
		t.Errorf("total/stale = %d/%d, want 1/1", body.Total, body.Stale)
	}
	// Every behaviour carries a key, so a client renders a real zero.
	for _, behavior := range domain.UpdateBehaviors {
		if _, ok := body.Counts[string(behavior)]; !ok {
			t.Errorf("counts is missing %q", behavior)
		}
	}
	// The stale row is present and says so, and offers no id to follow.
	if body.Items[1].Present || body.Items[1].ContainerID != "" {
		t.Errorf("the stale row = %+v", body.Items[1])
	}
	// One bounded read answered the whole page.
	if stub.reads != 1 {
		t.Errorf("the endpoint made %d reads, want 1", stub.reads)
	}
}

func TestTheBehaviorSummaryOmitsNoAlwaysPresentField(t *testing.T) {
	// The C2.1 lesson: a field the schema declares `required` must actually be
	// in the payload, on an EMPTY estate as much as a populated one. A summary
	// that omitted `items` or `counts` would hand a client `undefined` where it
	// promised a list.
	stub := &stubBehaviorSummary{summary: service.ContainerBehaviorSummary{
		Items:  []service.ContainerBehaviorItem{},
		Counts: map[domain.UpdateBehavior]int{},
	}}
	rec := getBehaviors(t, behaviorSummaryServer(t, stub, domain.RoleAdministrator))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"items", "counts", "total", "stale"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("%q is declared required and was not sent", field)
		}
	}
	// And `items` is a list, never null.
	if string(raw["items"]) != "[]" {
		t.Errorf("items = %s, want []", raw["items"])
	}
}

func TestAViewerMaySeeSavedBehaviours(t *testing.T) {
	// `inventory:read`, not `automation:manage`. Seeing which containers deviate
	// is not a management action, and a viewer can already read the same fact
	// one container at a time.
	stub := &stubBehaviorSummary{summary: behaviorSummaryFixture()}
	rec := getBehaviors(t, behaviorSummaryServer(t, stub, domain.RoleViewer))
	if rec.Code != http.StatusOK {
		t.Fatalf("a viewer got %d; the summary requires inventory:read", rec.Code)
	}
}

func TestTheBehaviorSummaryNeedsASession(t *testing.T) {
	stub := &stubBehaviorSummary{summary: behaviorSummaryFixture()}
	srv := behaviorSummaryServer(t, stub, domain.RoleAdministrator)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, behaviorsPath, nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous request got %d, want 401", rec.Code)
	}
	if stub.reads != 0 {
		t.Errorf("the store was read for an unauthenticated caller")
	}
}

func TestTheBehaviorSummaryIsReadOnly(t *testing.T) {
	// It is a second VIEW of per-container behaviour, never a second editor.
	// Nothing here may change one, so no write method reaches a handler.
	stub := &stubBehaviorSummary{summary: behaviorSummaryFixture()}
	srv := behaviorSummaryServer(t, stub, domain.RoleAdministrator)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authed(httptest.NewRequest(method, behaviorsPath, nil)))
		if rec.Code == http.StatusOK {
			t.Errorf("%s reached a handler and succeeded", method)
		}
	}
	if stub.wrote != 0 {
		t.Errorf("a write was attempted %d times through a read-only endpoint", stub.wrote)
	}
}

func TestReadingTheSummaryChangesNothing(t *testing.T) {
	// The orphaned rows in the fixture are exactly what a well-meaning cleanup
	// would delete. A read must not: a name that comes back should find its
	// setting waiting, and this endpoint holds no capability to remove one.
	stub := &stubBehaviorSummary{summary: behaviorSummaryFixture()}
	srv := behaviorSummaryServer(t, stub, domain.RoleAdministrator)

	for i := 0; i < 3; i++ {
		if rec := getBehaviors(t, srv); rec.Code != http.StatusOK {
			t.Fatalf("read %d: status %d", i, rec.Code)
		}
	}
	if stub.wrote != 0 {
		t.Errorf("reading the summary attempted %d writes", stub.wrote)
	}
}

func TestTheBehaviorSummaryFailsClosed(t *testing.T) {
	// A read that could not be performed establishes nothing. Reporting it as
	// an empty estate would claim no container has an override.
	stub := &stubBehaviorSummary{err: errors.New("database is gone")}
	rec := getBehaviors(t, behaviorSummaryServer(t, stub, domain.RoleAdministrator))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// And the failure says nothing about the host.
	for _, leak := range []string{"database is gone", "sqlite", "SELECT"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the error response leaked %q: %s", leak, rec.Body.String())
		}
	}
}

func TestTheBehaviorSummaryIsUnavailableWhenTheFeatureIsNotConfigured(t *testing.T) {
	// The service is optional. Absent, the endpoint says so rather than
	// answering with an empty summary that would read as "nothing is set".
	srv := newAuthedServer(Options{Auth: newStubAuth(domain.RoleAdministrator)})

	rec := getBehaviors(t, srv)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
