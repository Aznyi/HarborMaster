package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The readiness endpoint, from the outside.
//
// It is a POST that changes nothing, which is an unusual enough shape to be
// worth defending explicitly. Two properties matter more than the rest:
//
//   - a caller may describe a POLICY and nothing else. Every computed fact --
//     the recommendation, the verdict, the update type, the eligibility, the
//     dependency state, the snapshot status, the risk score -- is refused by
//     name rather than ignored, so a client cannot manufacture the answer;
//   - it is reachable with a READ permission, because an operator who may not
//     write a policy still has to be able to find out why automation is not
//     touching their containers.

const readinessPath = APIPrefix + "/automation/readiness"

// A minimal valid candidate: the two fields the handler requires.
const readinessBody = `{"strategy":"digestOnly","mode":"automatic","scope":"allEligible"}`

func TestReadinessPreviewsACandidatePolicy(t *testing.T) {
	automation := liveAutomation()
	automation.readiness = domain.AutomationReadinessReport{
		Considered: 12,
		Governed:   5,
		Eligible:   3,
		Groups: []domain.AutomationReadinessGroup{{
			Reason:      domain.ReasonNoUpdate,
			Explanation: domain.ReasonNoUpdate.Explain(),
			Count:       2,
		}},
	}
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, readinessPath, readinessBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Readiness     domain.AutomationReadinessReport `json:"readiness"`
		EngineEnabled bool                             `json:"engineEnabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Readiness.Eligible != 3 {
		t.Fatalf("eligible = %d, want 3", body.Readiness.Eligible)
	}
	if !body.EngineEnabled {
		t.Fatal("the engine state must be reported alongside the count")
	}

	// The handler assembled the candidate, and gave it the sentinel identifier
	// rather than a real one.
	if automation.readinessCandidate == nil {
		t.Fatal("the engine was never asked")
	}
	if got := automation.readinessCandidate.PolicyID; got != domain.AutomationReadinessCandidatePolicyID {
		t.Fatalf("candidate policy id = %q, want the preview sentinel", got)
	}
	if domain.ValidUpdatePolicyID(automation.readinessCandidate.PolicyID) {
		t.Fatal("the preview sentinel must not be a storable identifier")
	}
}

// TestReadinessRefusesComputedFacts is the §7 boundary.
//
// Each body below tries to tell HarborMaster something only HarborMaster may
// establish. All are refused as unknown fields rather than ignored: ignoring
// them would let a caller believe they had been honoured.
func TestReadinessRefusesComputedFacts(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	for _, body := range []string{
		`{"strategy":"digestOnly","mode":"automatic","recommendation":"proceed"}`,
		`{"strategy":"digestOnly","mode":"automatic","verdict":"update"}`,
		`{"strategy":"digestOnly","mode":"automatic","updateType":"digest"}`,
		`{"strategy":"digestOnly","mode":"automatic","eligible":true}`,
		`{"strategy":"digestOnly","mode":"automatic","dependencyState":"dependencySatisfied"}`,
		`{"strategy":"digestOnly","mode":"automatic","snapshotAvailable":true}`,
		`{"strategy":"digestOnly","mode":"automatic","riskScore":0}`,
		`{"strategy":"digestOnly","mode":"automatic","containerId":"0123456789ab"}`,
		`{"strategy":"digestOnly","mode":"automatic","image":"evil.example.com/x:latest"}`,
		`{"strategy":"digestOnly","mode":"automatic","digest":"sha256:0000"}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, readinessPath, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestReadinessRequiresTheFieldsThatDecideBehaviour mirrors the create path.
//
// Defaulting a mode or a ceiling would mean HarborMaster choosing how far an
// unattended change may go, and then reporting a count for a policy the
// operator never described.
func TestReadinessRequiresTheFieldsThatDecideBehaviour(t *testing.T) {
	srv := newAutomationServer(t, liveAutomation(), nil)

	for _, body := range []string{
		`{"mode":"automatic","scope":"allEligible"}`,
		`{"strategy":"digestOnly","scope":"allEligible"}`,
		`{}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, readinessPath, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestReadinessRefusesAPolicyItCouldNotStore keeps preview and persistence
// honest about each other.
//
// A configuration the create endpoint would refuse must not produce a count
// here: an operator would read a number for a policy they cannot save.
func TestReadinessRefusesAPolicyItCouldNotStore(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	for _, body := range []string{
		// A bare `*` image pattern is refused BY NAME. Note the field: the
		// refusal is on `selector.images`, which is a glob list. An entry in
		// `selector.include` is a literal container name, where `*` names a
		// container called "*" and matches nothing -- a different thing, and
		// not an error.
		`{"strategy":"digestOnly","mode":"automatic","selector":{"images":["*"]}}`,
		// A ceiling that is not in the vocabulary.
		`{"strategy":"everything","mode":"automatic","scope":"allEligible"}`,
		// A floor no policy may automate.
		`{"strategy":"digestOnly","mode":"automatic","scope":"allEligible",` +
			`"minimumRecommendation":"manualReview"}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, readinessPath, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if automation.readinessCandidate != nil {
		t.Fatal("an invalid configuration must not reach the engine")
	}
}

// TestReadinessRefusesAnUnparseablePolicyIdentifier stops a caller naming
// something that is not a policy.
func TestReadinessRefusesAnUnparseablePolicyIdentifier(t *testing.T) {
	srv := newAutomationServer(t, liveAutomation(), nil)

	rec := doJSON(t, srv, http.MethodPost, readinessPath,
		`{"strategy":"digestOnly","mode":"automatic","scope":"allEligible","policyId":"../../etc/passwd"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestReadinessCarriesAStoredPolicyIdentifierThrough is the edit path.
func TestReadinessCarriesAStoredPolicyIdentifierThrough(t *testing.T) {
	automation := liveAutomation()
	srv := newAutomationServer(t, automation, nil)

	rec := doJSON(t, srv, http.MethodPost, readinessPath,
		`{"strategy":"digestOnly","mode":"automatic","scope":"allEligible",`+
			`"policyId":"`+sampleUpdatePolicyID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := automation.readinessCandidate.PolicyID; got != sampleUpdatePolicyID {
		t.Fatalf("candidate policy id = %q, want the stored one", got)
	}
}

// TestReadinessIsReachableWithAReadPermission is the authorization boundary.
//
// Asking why automation is not touching a container is a question every role
// that can see automation may ask. Requiring automation:manage would put the
// answer behind the permission for CHANGING the rules.
func TestReadinessIsReachableWithAReadPermission(t *testing.T) {
	for _, route := range (&Server{}).routeTable() {
		if route.pattern != readinessPath || route.method != http.MethodPost {
			continue
		}
		if got := route.access.permission; got != domain.PermAutomationRead {
			t.Fatalf("readiness requires %q, want %q", got, domain.PermAutomationRead)
		}
		return
	}
	t.Fatal("the readiness route is not in the table")
}

// TestReadinessIsUnavailableWithoutTheEngine keeps the answer honest.
//
// A deployment with no automation service must say so rather than answer zero,
// which would read as "nothing is eligible".
func TestReadinessIsUnavailableWithoutTheEngine(t *testing.T) {
	srv := newAutomationServer(t, nil, nil)

	rec := doJSON(t, srv, http.MethodPost, readinessPath, readinessBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
