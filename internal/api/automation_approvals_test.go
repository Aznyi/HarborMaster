package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The approvals queue.
//
// GET /automation/approvals exists so an operator can find what is waiting for
// them without opening an archived pass and reading its whole table. It is a
// READ over decisions HarborMaster already recorded, and the properties worth
// pinning are all about what a caller cannot make it do:
//
//   - The verdict is fixed in code. A caller cannot widen the queue into the
//     full decision history by asking for another verdict, and cannot use this
//     route to enumerate the passes' skip reasons.
//   - The optional container narrowing is validated by SHAPE and refused when
//     it is not a name HarborMaster would have recorded, so a malformed value
//     never reaches the repository.
//   - Nothing here approves anything. The fake would record an Approve call,
//     and no test in this file produces one.

// approvalQueue is an engine holding one decision that needs a person, among
// decisions that do not.
func approvalQueue() *fakeAutomation {
	automation := liveAutomation()
	automation.decisions = append(automation.decisions, domain.AutomationDecision{
		RunID:         sampleRunID,
		ContainerName: "web",
		Verdict:       domain.VerdictAwaitingApproval,
		Reason:        domain.ReasonNeedApproval,
	})
	return automation
}

func TestApprovalQueueFixesTheVerdictInCode(t *testing.T) {
	// The headline property. A caller supplying its own verdict must not be
	// able to turn the approvals queue into a general decision search.
	automation := approvalQueue()
	srv := newAutomationServer(t, automation, nil)

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/automation/approvals?verdict=skip&verdicts=update", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	filter := automation.lastDecisionFilter(t)
	if len(filter.Verdicts) != 1 || filter.Verdicts[0] != domain.VerdictAwaitingApproval {
		t.Fatalf("the handler asked for verdicts %v, want exactly [%s]",
			filter.Verdicts, domain.VerdictAwaitingApproval)
	}
}

func TestApprovalQueueAsksTheSameQuestionTheCounterAnswers(t *testing.T) {
	// Found against a live host: every scheduler pass re-asks the same question
	// and records its own held decision, so an unrestricted listing showed one
	// row per pass per container -- twenty-eight rows for two outstanding
	// approvals, under a dashboard number that said two.
	automation := approvalQueue()
	srv := newAutomationServer(t, automation, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/approvals", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if !automation.lastDecisionFilter(t).LatestRunOnly {
		t.Fatal("the queue must be restricted to the pass the dashboard counts")
	}
}

func TestApprovalQueueNarrowsToOneContainer(t *testing.T) {
	// What the container detail page uses: a yes/no question about one
	// container, answered exactly rather than by scanning a page of the queue.
	automation := approvalQueue()
	srv := newAutomationServer(t, automation, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/approvals?container=web", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	filter := automation.lastDecisionFilter(t)
	if filter.ContainerName != "web" {
		t.Fatalf("the handler asked for container %q, want %q", filter.ContainerName, "web")
	}
	if len(filter.Verdicts) != 1 || filter.Verdicts[0] != domain.VerdictAwaitingApproval {
		t.Fatalf("narrowing lost the fixed verdict: %v", filter.Verdicts)
	}
}

func TestApprovalQueueRefusesAContainerNameItWouldNotHaveWritten(t *testing.T) {
	// Fail closed on shape. A value HarborMaster would never have recorded
	// cannot match a decision, so it is refused rather than passed down to the
	// repository to return nothing.
	cases := map[string]string{
		"a wildcard":        "web%25",
		"a path traversal":  "..%2F..%2Fetc",
		"a quote":           "web%27",
		"a semicolon":       "web%3Bdrop",
		"a space":           "web+server",
		"an over-long name": "w" + strings.Repeat("e", 512),
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			automation := approvalQueue()
			srv := newAutomationServer(t, automation, nil)

			rec := do(t, srv, http.MethodGet,
				APIPrefix+"/automation/approvals?container="+value, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			automation.mu.Lock()
			asked := len(automation.decisionFilters)
			automation.mu.Unlock()
			if asked != 0 {
				t.Fatalf("a refused name still reached the engine %d times", asked)
			}
		})
	}
}

func TestApprovalQueueIsBounded(t *testing.T) {
	// A host with hundreds of containers can produce a long queue. The browser
	// must not be handed all of it, and a caller must not be able to ask.
	automation := approvalQueue()
	srv := newAutomationServer(t, automation, nil)

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/automation/approvals?pageSize=100000", nil)
	if rec.Code == http.StatusOK {
		filter := automation.lastDecisionFilter(t)
		if filter.Page.Limit > 200 {
			t.Fatalf("page limit %d is not bounded", filter.Page.Limit)
		}
		return
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 200 with a bounded limit or 400: %s",
			rec.Code, rec.Body.String())
	}
}

func TestApprovalQueueApprovesNothing(t *testing.T) {
	// Reading the queue is not acting on it. Every guard on the approve path
	// stays where it is, and this route never crosses it.
	automation := approvalQueue()
	srv := newAutomationServer(t, automation, nil)

	for _, target := range []string{
		APIPrefix + "/automation/approvals",
		APIPrefix + "/automation/approvals?container=web",
		APIPrefix + "/automation/approvals?page=2&pageSize=5",
	} {
		if rec := do(t, srv, http.MethodGet, target, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
	}

	if automation.reached() != 0 {
		t.Fatalf("reading the queue reached a mutating engine method")
	}
}

func TestApprovalQueueRefusesEveryMethodButGet(t *testing.T) {
	srv := newAutomationServer(t, approvalQueue(), nil)

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		rec := doJSON(t, srv, method, APIPrefix+"/automation/approvals", `{}`)
		if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
			t.Errorf("%s /automation/approvals = %d, want a refusal", method, rec.Code)
		}
	}
}

func TestApprovalQueueSurvivesTheEngineBeingOff(t *testing.T) {
	// Consistent with every other automation read: somebody who turned the
	// engine off is exactly the person who wants to see what it was holding.
	automation := approvalQueue()
	automation.enabled = false
	srv := newAutomationServer(t, automation, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/approvals", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestApprovalQueueIsUnavailableWithoutTheEngine(t *testing.T) {
	srv := newAutomationServer(t, nil, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/approvals", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestApprovalQueueDisclosesNoSecret(t *testing.T) {
	// The decision record carries the container's identity and the proposed
	// image. It must not start carrying anything else because a new response
	// type was written around it.
	automation := approvalQueue()
	srv := newAutomationServer(t, automation, nil)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/automation/approvals", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response automationApprovalListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Pagination.PageSize == 0 {
		t.Fatalf("the response carries no pagination: %s", rec.Body.String())
	}

	body := rec.Body.String()
	for _, forbidden := range []string{
		"password", "token", "secret", "Authorization", "registryCredential",
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("the approvals response mentions %q: %s", forbidden, body)
		}
	}
}
