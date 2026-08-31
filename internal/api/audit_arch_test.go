package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Architecture tests added by the Phase 9.5 security audit.
//
// Each one exists because the audit found the property was true by care rather
// than by construction, and a property held by care is one a future change
// breaks silently.

// TestEveryPermissionGuardsARoute fails on a permission that grants nothing.
//
// # Why a dead permission is a real problem, not tidiness
//
// `GET /roles` publishes each role's permission list, and a client builds its
// interface from it. A permission that no route requires is a capability the
// API advertises and does not have: a UI generated from the catalogue offers a
// control that cannot exist, and an administrator granting the role believes
// they granted something.
//
// The audit found exactly one -- `settings:manage` -- which was declared,
// granted to administrators, published in the catalogue, and guarded nothing.
// This test is what stops the next one.
func TestEveryPermissionGuardsARoute(t *testing.T) {
	guarded := map[domain.Permission]int{}
	for _, r := range (&Server{}).routeTable() {
		if r.access.kind != accessPermission {
			continue
		}
		guarded[r.access.permission]++
	}

	for _, permission := range domain.AllPermissions {
		if guarded[permission] == 0 {
			t.Errorf("permission %q guards no route\n"+
				"\tGET /roles publishes it, so a client is told the capability exists; "+
				"either give it a route or remove it",
				permission)
		}
	}
}

// TestEveryGuardedPermissionIsReachableFromARole is the other direction.
//
// A route guarded by a permission no role holds is a route nobody can reach --
// an endpoint that answers 403 to everyone, including the administrator, which
// is a lockout rather than a policy.
func TestEveryGuardedPermissionIsReachableFromARole(t *testing.T) {
	roles := []domain.Role{
		domain.RoleViewer, domain.RoleOperator, domain.RoleAdministrator,
	}

	for _, r := range (&Server{}).routeTable() {
		if r.access.kind != accessPermission {
			continue
		}

		reachable := false
		for _, role := range roles {
			if role.Can(r.access.permission) {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("route %s %s requires %q, which no role holds\n"+
				"\tthe endpoint would answer 403 to every account, including an "+
				"administrator",
				methodLabel(r.method), r.pattern, r.access.permission)
		}
	}
}

// TestTheHostChangingActionsAreAuditedByOutcomeNotRequest pins what the
// privileged counter counts.
//
// # The distinction the audit found missing
//
// Before this pass, `Privileged()` was true for `acquisition.requested` and
// `execution.requested`. A request is an INTENTION: it can be refused by the
// second preflight, cancelled before the first mutation, expire in the queue,
// or fail partway. Counting requests as host changes over-reports, and a
// security counter that over-reports is one an administrator learns to ignore.
//
// The completions are what changed the host, so they are what count.
func TestTheHostChangingActionsAreAuditedByOutcomeNotRequest(t *testing.T) {
	privileged := map[domain.AuditAction]bool{
		domain.AuditAcquisitionCompleted: true,
		domain.AuditExecutionCompleted:   true,
		// A completed rollback stopped the container that was serving and
		// started another in its place. It changed the host.
		domain.AuditRollbackCompleted: true,
		// A removed image is a host change that cannot be undone, made on a
		// timer with nobody watching. If anything belongs in a counter an
		// administrator scans, it is this.
		//
		// The recorder pairs Privileged() with a SUCCEEDED outcome, so a
		// removal the daemon refused -- which changed nothing -- does not
		// inflate the count. That is the same rule the other three follow.
		domain.AuditImageRemoved: true,
	}

	for _, action := range domain.AuditActions {
		if got := action.Privileged(); got != privileged[action] {
			t.Errorf("audit action %q privileged=%v, want %v", action, got, privileged[action])
		}
	}

	// The requests must still be recorded -- they are just not host changes.
	for _, action := range []domain.AuditAction{
		domain.AuditAcquisitionRequested, domain.AuditExecutionRequested,
		domain.AuditAcquisitionCompleted, domain.AuditAcquisitionFailed,
		domain.AuditExecutionCompleted, domain.AuditExecutionFailed,
		domain.AuditRollbackRequested, domain.AuditRollbackCancelled,
		domain.AuditRollbackCompleted, domain.AuditRollbackFailed,
		domain.AuditImageRemoved,
	} {
		if !domain.ValidAuditAction(string(action)) {
			t.Errorf("audit action %q is not in the recognised vocabulary", action)
		}
	}
}

// TestEveryDocumentedAuditFilterIsActuallyApplied fails on a filter that is
// accepted and then ignored.
//
// # Why this is a security property and not a usability one
//
// The audit found `targetId` and `since` documented in the OpenAPI schema, sent
// by the shipped client, accepted by the server, and dropped on the floor. An
// administrator narrowing an investigation to "the last hour" got the whole log
// and had no way to know. A filter that silently does nothing is worse than one
// that errors: it produces a confident wrong answer.
func TestEveryDocumentedAuditFilterIsActuallyApplied(t *testing.T) {
	cases := map[string]struct {
		query  string
		assert func(t *testing.T, applied bool)
	}{
		"targetId": {
			query: "targetId=exec_0123456789abcdef",
			assert: func(t *testing.T, applied bool) {
				if !applied {
					t.Error("targetId was accepted but never reached the store filter")
				}
			},
		},
		"since": {
			query: "since=2026-01-01T00:00:00Z",
			assert: func(t *testing.T, applied bool) {
				if !applied {
					t.Error("since was accepted but never reached the store filter")
				}
			},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			parsed, err := parseAuditQuery(mustQuery(t, testCase.query))
			if err != nil {
				t.Fatalf("parse %s: %v", testCase.query, err)
			}
			filter := parsed.filter()

			applied := filter.TargetID != "" || !filter.Since.IsZero()
			testCase.assert(t, applied)
		})
	}

	// A malformed value is refused rather than ignored, so a client that sends
	// the wrong shape learns about it. Built as decoded values rather than as a
	// query string, because a control character cannot survive url.Parse.
	for name, values := range map[string]map[string][]string{
		"a timestamp that is not one": {"since": {"not-a-timestamp"}},
		"a bare year":                 {"since": {"2026"}},
		"a control character in the target id": {
			"targetId": {"exec" + string(rune(0x07)) + "1234"},
		},
		"an oversized target id": {"targetId": {strings.Repeat("a", 4096)}},
	} {
		if _, err := parseAuditQuery(values); err == nil {
			t.Errorf("parseAuditQuery accepted %s", name)
		}
	}
}

// mustQuery parses a query string for a test.
func mustQuery(t *testing.T, raw string) map[string][]string {
	t.Helper()

	request := httptestRequest(t, "/?"+raw)
	return request.URL.Query()
}

// httptestRequest builds a GET request for a target.
func httptestRequest(t *testing.T, target string) *http.Request {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request for %q: %v", target, err)
	}
	return request
}
