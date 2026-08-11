package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The policy scope over HTTP.
//
// Three questions, and the third is the one an API client depends on:
//
//  1. Can a caller ask for the broad scope without typing a selector?
//  2. Is a body that says two contradictory things refused rather than
//     reconciled?
//  3. Does a client written before the field existed still work unchanged?

func TestCreatingABroadPolicyNeedsNoSelector(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	body := `{"name":"everything","scope":"allEligible","strategy":"patch","mode":"observe"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if len(policies.created) != 1 {
		t.Fatalf("created = %d, want 1", len(policies.created))
	}

	created := policies.created[0]
	if created.Scope != domain.ScopeAllEligible {
		t.Fatalf("scope = %q, want allEligible", created.Scope)
	}
	if !created.Selector.Empty() {
		t.Fatalf("the handler invented a selector: %+v", created.Selector)
	}
}

// The exclusion list is the one selector clause the broad scope accepts, and it
// has to reach the service intact -- it is the only way an operator narrows a
// broad policy.
func TestABroadPolicyKeepsItsExclusions(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	body := `{"name":"everything","scope":"allEligible",` +
		`"selector":{"exclude":["database"]},"strategy":"patch","mode":"observe"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	created := policies.created[0]
	if len(created.Selector.Exclude) != 1 || created.Selector.Exclude[0] != "database" {
		t.Fatalf("exclusions = %v, want the one the caller sent", created.Selector.Exclude)
	}
}

// A body that names a scope AND an inclusion clause says two things. The
// service refuses it; this proves the handler passes it through rather than
// quietly dropping one of them, which would store a policy nobody asked for.
func TestContradictoryScopeAndSelectorReachTheValidator(t *testing.T) {
	for _, body := range []string{
		`{"name":"x","scope":"allEligible","selector":{"include":["web"]},"strategy":"patch","mode":"observe"}`,
		`{"name":"x","scope":"allEligible","selector":{"images":["nginx:*"]},"strategy":"patch","mode":"observe"}`,
		`{"name":"x","scope":"allEligible","selector":{"labels":{"tier":"front"}},"strategy":"patch","mode":"observe"}`,
	} {
		policies := &fakeUpdatePolicies{}
		srv := newAutomationServer(t, liveAutomation(), policies)

		rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
		if rec.Code != http.StatusCreated {
			// The fake service does not validate, so a 201 here means the
			// handler passed the contradiction along -- which is what we want.
			t.Errorf("%s: status = %d", body, rec.Code)
			continue
		}
		created := policies.created[0]
		if created.Scope != domain.ScopeAllEligible {
			t.Errorf("%s: the handler changed the scope to %q", body, created.Scope)
		}
		if created.Selector.Empty() {
			t.Errorf("%s: the handler silently dropped the inclusion clause\n"+
				"\tit must reach the validator, which refuses the combination; "+
				"dropping it here would store a policy the caller did not ask for", body)
		}
	}
}

// An unknown scope is refused by the validator, not translated into a known
// one. Reaching the service is the handler's whole job here.
func TestAnUnknownScopeIsNotTranslated(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	body := `{"name":"x","scope":"everything","selector":{"include":["web"]},` +
		`"strategy":"patch","mode":"observe"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := policies.created[0].Scope; got != "everything" {
		t.Fatalf("scope reached the service as %q; the handler must not reinterpret it", got)
	}
}

// ---------------------------------------------- backward compatibility --

// A body with no scope is what every client sent before this field existed. It
// must still create the same policy it always did.
func TestAPolicyBodyWithoutAScopeIsNarrow(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", validUpdatePolicyBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := policies.created[0].Scope; got != domain.ScopeSelector {
		t.Fatalf("scope = %q, want selector\n"+
			"\ta client that does not know about scopes must keep getting the "+
			"behaviour it already had", got)
	}
}

// And a create with no scope and no selector is still refused, because the
// default scope is the one that needs one.
func TestACreateWithNeitherScopeNorSelectorIsRefused(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	body := `{"name":"x","strategy":"patch","mode":"observe"}`
	rec := doJSON(t, srv, http.MethodPost, APIPrefix+"/update-policies", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "selector is required") {
		t.Fatalf("the refusal does not say what is missing: %s", rec.Body.String())
	}
	if policies.writes() != 0 {
		t.Error("an incomplete policy reached the service")
	}
}

// An EDIT that does not mention the scope must not carry one. A partial edit
// that silently set a breadth would be the worst kind of surprise: invisible in
// the request and permanent in the policy.
func TestAnEditWithoutAScopeSendsNoScope(t *testing.T) {
	policies := &fakeUpdatePolicies{}
	srv := newAutomationServer(t, liveAutomation(), policies)

	rec := doJSON(t, srv, http.MethodPatch,
		APIPrefix+"/update-policies/"+sampleUpdatePolicyID, `{"name":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(policies.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(policies.updated))
	}
	if policies.updated[0].Scope != nil {
		t.Fatalf("the handler sent a scope (%q) on an edit that did not name one",
			*policies.updated[0].Scope)
	}
}
