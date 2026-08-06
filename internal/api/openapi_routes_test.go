package api

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The OpenAPI document and the router must describe the same API.
//
// They are maintained by hand in two files, so the only thing stopping them
// drifting is a test that reads both. A documented path that does not exist
// misleads every client author; an undocumented one is invisible to them.

// specPathPattern matches a top-level path key in the paths: block, which is
// the only place a two-space-indented "/..." key appears.
var specPathPattern = regexp.MustCompile(`(?m)^  (/api/v1[^:]*):$`)

// documentedPaths reads the paths declared in api/openapi.yaml.
//
// A regex rather than a YAML parser: the module has no YAML dependency, adding
// one to read a list of keys would be a poor trade, and the pattern is anchored
// tightly enough that a mismatch fails loudly rather than silently passing.
func documentedPaths(t *testing.T) []string {
	t.Helper()

	source, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	matches := specPathPattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("found no paths in openapi.yaml; the pattern no longer matches the document")
	}

	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		paths = append(paths, match[1])
	}
	sort.Strings(paths)
	return paths
}

// routedPaths is the set of API paths the router serves, kept as a literal so a
// route added without documenting it fails this test rather than passing
// vacuously.
//
// Each entry is exercised below, so a path listed here that the router does not
// actually serve fails too.
var routedPaths = []string{
	APIPrefix + "/containers",
	APIPrefix + "/containers/{id}",
	APIPrefix + "/containers/{id}/raw",
	APIPrefix + "/drift",
	APIPrefix + "/drift/container/{id}",
	APIPrefix + "/drift/summary",
	APIPrefix + "/drift/{id}",
	APIPrefix + "/event-engine",
	APIPrefix + "/event-filters",
	APIPrefix + "/events",
	APIPrefix + "/events/stream",
	APIPrefix + "/events/{id}",
	APIPrefix + "/health",
	APIPrefix + "/images",
	APIPrefix + "/images/{id}",
	APIPrefix + "/images/{id}/history",
	APIPrefix + "/images/refresh",
	APIPrefix + "/images/updates",
	APIPrefix + "/inventory",
	APIPrefix + "/inventory/filters",
	APIPrefix + "/inventory/refresh",
	APIPrefix + "/networks",
	APIPrefix + "/plans",
	APIPrefix + "/plans/container/{id}",
	APIPrefix + "/plans/generate",
	APIPrefix + "/plans/{id}",
	APIPrefix + "/policies",
	APIPrefix + "/policies/{id}",
	APIPrefix + "/policy-rules",
	APIPrefix + "/policy-summary",
	APIPrefix + "/policy-violations",
	APIPrefix + "/policy-violations/container/{id}",
	APIPrefix + "/policy-violations/{id}",
	APIPrefix + "/policy/evaluate",
	APIPrefix + "/snapshots",
	APIPrefix + "/snapshots/{id}",
	APIPrefix + "/snapshots/{id}/diff",
	APIPrefix + "/snapshots/{id}/restore-readiness",
	APIPrefix + "/version",
	APIPrefix + "/volumes",
}

// There is no restore, rollback, or apply path, and this is the list that would
// have to change for one to appear. Phase 3 records configuration and validates
// whether it could be restored; it does not restore.
//
// Nor is there an image PULL, delete, prune, or apply path. Phase 6 reads
// registries to discover that a newer image exists; downloading or applying one
// is an operator's job with their own tooling, and adding a route that did it
// would have to be added to this literal in a diff a reviewer sees.
//
// Nor is there a policy ENFORCE, apply, or remediate path. Phase 5 checks
// configuration against administrator-defined rules and reports what fails; it
// does not change a container to satisfy one, and adding a route that did would
// have to be added to this literal in a diff a reviewer sees.
//
// Nor is there a plan APPLY, execute, approve, or schedule path. POST
// /plans/generate produces HarborMaster's own analysis of HarborMaster's own
// database -- it pulls nothing and changes no container. Phase 7 assesses a
// proposed change; carrying one out is an operator's job with their own
// tooling. There is also no PATCH or DELETE for a plan, because a plan records
// what was believed at one moment and editing one would destroy exactly the
// property that makes it worth keeping.

func TestOpenAPIDocumentsExactlyTheRoutedPaths(t *testing.T) {
	documented := documentedPaths(t)

	expected := append([]string(nil), routedPaths...)
	sort.Strings(expected)

	if len(documented) != len(expected) {
		t.Errorf("openapi documents %d paths, the router serves %d", len(documented), len(expected))
	}

	documentedSet := make(map[string]bool, len(documented))
	for _, path := range documented {
		documentedSet[path] = true
	}
	routedSet := make(map[string]bool, len(expected))
	for _, path := range expected {
		routedSet[path] = true
	}

	for _, path := range expected {
		if !documentedSet[path] {
			t.Errorf("%s is routed but not documented in openapi.yaml", path)
		}
	}
	for _, path := range documented {
		if !routedSet[path] {
			t.Errorf("%s is documented in openapi.yaml but not routed", path)
		}
	}
}

// Every documented path must actually answer, so a path listed above cannot be
// wrong in the other direction.
func TestEveryDocumentedPathIsReachable(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, &fakeEngine{enabled: false})

	for _, pattern := range routedPaths {
		// Substitute a concrete value for the wildcard so the request routes.
		target := strings.ReplaceAll(pattern, "{id}", "1")

		// The two POST-only paths. Everything else answers GET.
		method := http.MethodGet
		switch pattern {
		case APIPrefix + "/inventory/refresh",
			APIPrefix + "/policy/evaluate",
			APIPrefix + "/images/refresh",
			APIPrefix + "/plans/generate":
			method = http.MethodPost
		}

		rec := do(t, srv, method, target, nil)

		// The doubles here are deliberately minimal, so a 503 or a 404 for a
		// missing record is fine. A 405 would mean the method-qualified route
		// is missing, and a 404 with the API's own "endpoint not found" body
		// would mean the path is not registered at all.
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s returned 405; the route is not registered for this method", method, target)
		}
		if rec.Code == http.StatusNotFound &&
			strings.Contains(rec.Body.String(), "endpoint not found") {
			t.Errorf("%s %s is documented but not routed", method, target)
		}
	}
}

// The Phase 2.5 endpoints are read-only. This is asserted here as well as in
// event_api_test.go because it is a property of the whole API surface, not just
// of one handler.
func TestNoEventPathAcceptsAWriteMethod(t *testing.T) {
	srv := newEventServer(t, &fakeDockerEvents{}, &fakeEngine{enabled: true})

	for _, path := range routedPaths {
		if !strings.Contains(path, "event") {
			continue
		}
		target := strings.ReplaceAll(path, "{id}", "1")

		for _, method := range []string{
			http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		} {
			rec := do(t, srv, method, target, nil)
			if rec.Code < 400 {
				t.Errorf("%s %s returned %d; every event endpoint must reject writes",
					method, target, rec.Code)
			}
		}
	}
}
