package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The container list's attention projection.
//
// A container row now carries what HarborMaster knows about the container, and
// the properties worth pinning are the ones an operator would be misled by:
//
//   - the whole page costs ONE evidence lookup, not one per row
//   - a container nothing has assessed says so, rather than "up to date"
//   - the containers HarborMaster parked aside are out of the default view,
//     reachable with one parameter, and never deleted
//   - the row still carries every field it carried before, so nothing that
//     read this endpoint has to change

// attentionServer builds a server over a container fake.
func attentionServer(t *testing.T, containers *fakeContainers) *Server {
	t.Helper()
	return newAuthedServer(Options{
		Health:     &fakeHealth{},
		Containers: containers,
		Logger:     discardLogger(),
		Assets:     testAssets(),
	})
}

// pageOf builds n container summaries.
func pageOf(n int) []domain.ContainerSummary {
	summaries := make([]domain.ContainerSummary, 0, n)
	for i := 0; i < n; i++ {
		summaries = append(summaries, domain.ContainerSummary{
			HostID:  domain.LocalHostID,
			ID:      string(rune('a'+i)) + "0000000000000000",
			ShortID: string(rune('a'+i)) + "00000000000",
			Name:    "svc-" + string(rune('a'+i)),
			Image:   domain.ParseImageRef("nginx:1.27"),
			State:   domain.StateRunning,
			Health:  domain.HealthHealthy,
			Present: true,
		})
	}
	return summaries
}

// listItems decodes the rows out of a container list response.
func listItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var response struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return response.Items
}

func TestOnePageCostsOneEvidenceLookup(t *testing.T) {
	// The headline property, and the reason the store method takes a slice.
	// An implementation that asked per row would return exactly the same JSON,
	// so nothing but this test would catch it.
	containers := &fakeContainers{summaries: pageOf(20), total: 20}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if containers.attentionCalls != 1 {
		t.Fatalf("the handler made %d evidence lookups for one page of 20",
			containers.attentionCalls)
	}
	if containers.attentionKeyCount != 20 {
		t.Fatalf("the lookup covered %d containers, want all 20",
			containers.attentionKeyCount)
	}
}

func TestAnEmptyPageAsksNothing(t *testing.T) {
	containers := &fakeContainers{summaries: nil, total: 0}
	srv := attentionServer(t, containers)

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if containers.attentionCalls != 0 {
		t.Fatalf("an empty page made %d lookups", containers.attentionCalls)
	}
}

func TestAContainerWithNoEvidenceIsNotReportedAsCurrent(t *testing.T) {
	// The fake answers with no evidence at all, which is what a fresh install
	// looks like before the planner has run.
	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	items := listItems(t, rec.Body.Bytes())
	attention, ok := items[0]["attention"].(map[string]any)
	if !ok {
		t.Fatalf("the row carries no attention object: %v", items[0])
	}
	if attention["state"] != string(domain.AttentionNotChecked) {
		t.Fatalf("state = %v, want %q", attention["state"], domain.AttentionNotChecked)
	}
}

func TestTheInventoryRowIsTheAuthorityOnItsOwnHealth(t *testing.T) {
	// Health comes from the container record the refresh wrote, not from the
	// evidence tables -- which do not carry it and must not be able to
	// contradict it.
	summaries := pageOf(1)
	summaries[0].Health = domain.HealthUnhealthy

	containers := &fakeContainers{
		summaries: summaries,
		total:     1,
		evidence: map[string]domain.ContainerEvidence{
			summaries[0].ID: {
				Health:         domain.HealthHealthy, // stale, and ignored
				PlanKnown:      true,
				UpdateType:     domain.UpdateNone,
				Recommendation: domain.RecommendProceed,
				LineageKnown:   true,
				Tracked:        true,
			},
		},
	}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil)
	items := listItems(t, rec.Body.Bytes())
	attention := items[0]["attention"].(map[string]any)

	if attention["state"] != string(domain.AttentionUnhealthy) {
		t.Fatalf("state = %v, want %q", attention["state"], domain.AttentionUnhealthy)
	}
}

func TestTheRowStillCarriesEveryFieldItCarriedBefore(t *testing.T) {
	// The summary is embedded, so this is an ADDITIVE change: a client reading
	// `name` or `state` today keeps reading them.
	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil)
	items := listItems(t, rec.Body.Bytes())

	for _, field := range []string{
		"id", "shortId", "name", "image", "state", "health", "present",
		"restartCount", "restartPolicy", "compose", "ports",
	} {
		if _, present := items[0][field]; !present {
			t.Errorf("the row lost %q", field)
		}
	}
}

// -------------------------------------------------------------- preserved --

func TestPreservedContainersAreOutOfTheDefaultView(t *testing.T) {
	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	srv := attentionServer(t, containers)

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !containers.lastFilter.ExcludePreserved {
		t.Fatal("the default listing must exclude the containers HarborMaster parked")
	}
}

func TestPreservedContainersAreOneParameterAway(t *testing.T) {
	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/containers?includePreserved=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if containers.lastFilter.ExcludePreserved {
		t.Fatal("includePreserved=true must widen the listing")
	}
}

func TestAMalformedIncludePreservedIsRefused(t *testing.T) {
	// Not silently treated as false. A caller asking to see the preserved
	// containers and being handed the narrow list without being told would
	// conclude they are gone.
	containers := &fakeContainers{summaries: pageOf(1), total: 1}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/containers?includePreserved=maybe", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------- failures --

func TestAFailedEvidenceLookupDoesNotServeAMisleadingPage(t *testing.T) {
	// The one direction that matters: rather than rendering every row as "not
	// checked" because a query failed, the request fails. A page that silently
	// downgraded its own evidence would be indistinguishable from a fresh
	// install with nothing assessed.
	containers := &fakeContainers{
		summaries:    pageOf(3),
		total:        3,
		attentionErr: store.ErrTooManyAttentionKeys,
	}
	srv := attentionServer(t, containers)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/containers", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	// And it says nothing about what failed.
	if body := rec.Body.String(); len(body) > 0 &&
		(contains(body, "attention") || contains(body, "sql")) {
		t.Fatalf("the error response leaks internals: %s", body)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
