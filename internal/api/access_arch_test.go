package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Architecture tests for the access model.
//
// These are in `package api` rather than in internal/arch because the
// properties they check are about UNEXPORTED structure -- the route table, the
// access policy type, the handler method bodies. A test outside the package
// could only check them by grepping, and a grep cannot tell a route's policy
// from a string that happens to look like one.
//
// What they collectively assert:
//
//  1. Every route declares a policy. Forgetting one is a build failure, not an
//     accidentally public endpoint.
//  2. The public surface is exactly the four routes that have a stated reason.
//  3. Every state-changing route is attributed to an actor.
//  4. No handler makes its own authorization decision.
//
// Together those are what make "default deny, decided in one place, and
// recorded" a checked property.

// TestEveryRouteDeclaresAnAccessPolicy fails on a route with the zero-value
// policy.
//
// The zero value of routeAccess is accessInvalid, which is neither "public" nor
// "authenticated". That is the entire design: a struct literal that omits the
// policy field produces something the runtime refuses and this test rejects,
// rather than something that silently defaults to one answer or the other.
func TestEveryRouteDeclaresAnAccessPolicy(t *testing.T) {
	for _, r := range (&Server{}).routeTable() {
		if r.access.valid() {
			continue
		}
		t.Errorf("route %s %s has no access policy (kind=%q permission=%q)\n"+
			"\tevery route must declare one: public(), bootstrapOnly(), authenticated(), "+
			"duringPasswordChange(), or requires(permission)",
			methodLabel(r.method), r.pattern, r.access.kind, r.access.permission)
	}
}

// TestThePublicSurfaceIsExactlyTheDocumentedRoutes pins the unauthenticated
// surface.
//
// Adding a public route is the single easiest way to undo Phase 9.5, and it
// looks like a one-line change in a large table. This test makes it a two-line
// change, the second of which is here.
//
// Bare method-not-allowed entries count too: a bare entry marked public would
// answer 405 to an anonymous caller and thereby confirm the path exists.
func TestThePublicSurfaceIsExactlyTheDocumentedRoutes(t *testing.T) {
	expected := map[string]bool{
		"GET " + APIPrefix + "/health":      true,
		"* " + APIPrefix + "/health":        true,
		"GET " + APIPrefix + "/version":     true,
		"* " + APIPrefix + "/version":       true,
		"POST " + APIPrefix + "/auth/login": true,
		"* " + APIPrefix + "/auth/login":    true,
		// The bootstrap STATUS is public; the bootstrap POST is not -- it is
		// accessBootstrap, which stops answering once an administrator exists.
		"GET " + APIPrefix + "/auth/bootstrap": true,
		"* " + APIPrefix + "/auth/bootstrap":   true,
	}

	found := map[string]bool{}
	for _, r := range (&Server{}).routeTable() {
		if r.access.kind != accessPublic {
			continue
		}
		key := methodLabel(r.method) + " " + r.pattern
		found[key] = true
		if !expected[key] {
			t.Errorf("route %s is public but is not in the documented public set\n"+
				"\tHarborMaster fronts a root-equivalent socket; an unauthenticated route "+
				"needs a stated reason in routeTable and an entry here",
				key)
		}
	}

	for key := range expected {
		if !found[key] {
			t.Errorf("route %s was expected to be public but is not in the table\n"+
				"\tif it was removed or its policy tightened, remove it from this test too",
				key)
		}
	}
}

// writeMethods are the methods that change state.
var writeMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// auditedElsewhere lists write routes whose audit record is written by the
// SERVICE rather than by the handler, with the reason.
//
// Two groups.
//
// The AUTHENTICATION paths, where the service writes the row because it knows
// things the handler must never see: which of several credential failures
// occurred, whether a session was superseded, how many sessions a password
// change revoked. A handler-side record would either be less accurate or would
// require handing the handler that detail.
//
// The AUTOMATION paths, for the same reason in a different shape. A handler
// cannot know whether an approval was refused because the plan moved on or
// because the container is paused, whether a pass submitted anything, or which
// fields a policy edit actually changed -- and those are exactly what the audit
// reason has to say. Each of these services takes a service.Actor built by
// s.actorFrom(r) and calls RecordAction with it, so the actor still comes from
// the request.
// readOnlyWrites lists routes that use a write METHOD and change no state.
//
// A third category, and a deliberately small one. `auditedElsewhere` says "the
// record is written somewhere else"; this says "there is nothing to record",
// which is a stronger claim and therefore has to be declared rather than
// inferred from the absence of an audit call.
//
// A route belongs here only when it needs a request BODY and answers a
// question. It still gets every guard a POST gets -- CSRF, the size bound, the
// strict decoder, the write rate limiter -- because the method is what the
// middleware sees, and because the rate limiter is what bounds the cost of
// asking an expensive question repeatedly.
//
// Not auditing them is a deliberate choice rather than an omission: the policy
// editor asks for a readiness preview on every edit, and a row per keystroke
// would bury the records of things that actually changed the host.
var readOnlyWrites = map[string]string{
	APIPrefix + "/automation/readiness": "previews a policy configuration against the current estate; " +
		"writes no run, decision, policy, acquisition or execution, and makes no Docker call",
}

var auditedElsewhere = map[string]string{
	APIPrefix + "/auth/login":                "AuthService.Login records success, failure, and the reason",
	APIPrefix + "/auth/logout":               "AuthService.Logout records the revocation",
	APIPrefix + "/auth/bootstrap":            "AuthService.Bootstrap records both completion and rejection",
	APIPrefix + "/auth/password":             "AuthService.ChangePassword records the session revocations",
	APIPrefix + "/auth/sessions/{id}/revoke": "AuthService.RevokeSession records who revoked whose",
	APIPrefix + "/users":                     "UserService.Create records the account and its role",
	APIPrefix + "/users/{id}":                "UserService.SetRole and SetStatus record the change",
	APIPrefix + "/users/{id}/password-reset": "UserService.ResetPassword records the reset",

	APIPrefix + "/update-policies":      "UpdatePolicyService.Create records the mode and strategy the rule was created with",
	APIPrefix + "/update-policies/{id}": "UpdatePolicyService.Update and Archive record which fields moved",
	APIPrefix + "/automation/run":       "AutomationService records the pass, and its counters, once it knows them",
	APIPrefix + "/automation/approve":   "AutomationService.Approve records the release, and the refusal when the plan moved on",
	APIPrefix + "/automation/pause":     "AutomationService.PauseContainer records the pause",
	APIPrefix + "/automation/resume":    "AutomationService.Resume records who cleared it",
	APIPrefix + "/containers/{id}/update-behavior": "ContainerPreferenceService.SetBehavior and ClearBehavior record " +
		"which behaviour was chosen for which container, in the operator-facing words",

	APIPrefix + "/automation/simple-updates": "UpdatePolicyService.EnableSimpleUpdates and DisableSimpleUpdates record " +
		"the switch, and what the managed policy covers, through the same recorder every policy write uses",

	APIPrefix + "/plan-approvals/{id}": "PlanApprovalService records the approval and the withdrawal, " +
		"and the refusal when the plan is superseded or does not ask for review",

	APIPrefix + "/notifications/destinations": "NotificationAdminService.CreateDestination records the channel, never the URL",
	APIPrefix + "/notifications/destinations/{id}": "NotificationAdminService.UpdateDestination and ArchiveDestination record the edit, " +
		"and say explicitly when a credential was replaced",
	APIPrefix + "/notifications/destinations/{id}/test": "NotificationAdminService.TestDestination records the outbound request somebody caused",
	APIPrefix + "/notifications/rules":                  "NotificationAdminService.CreateRule records the rule",
	APIPrefix + "/notifications/rules/{id}":             "NotificationAdminService.UpdateRule and ArchiveRule record the change",
}

// TestEveryWriteRouteIsAudited fails if a state-changing route neither calls
// auditWrite nor is listed as audited in its service.
//
// # Why this test exists
//
// Before Phase 9.5 HarborMaster recorded WHAT happened and never WHO. Closing
// that gap was one call per handler, and a gap reopened the same way -- a new
// write endpoint whose author did not know the convention -- would be invisible
// in review. This test is the thing that notices.
//
// It resolves each handler to its source method by name and reads the method's
// body, so it cannot be satisfied by a call in a neighbouring function.
func TestEveryWriteRouteIsAudited(t *testing.T) {
	bodies := handlerBodies(t)

	for _, r := range (&Server{}).routeTable() {
		if !writeMethods[r.method] || r.handler == nil {
			continue
		}
		if reason, ok := auditedElsewhere[r.pattern]; ok {
			if reason == "" {
				t.Errorf("route %s %s is exempted without a reason", r.method, r.pattern)
			}
			continue
		}
		if reason, ok := readOnlyWrites[r.pattern]; ok {
			if reason == "" {
				t.Errorf("route %s %s is declared read-only without a reason", r.method, r.pattern)
			}
			// Declared, and then CHECKED: a route that claims to change nothing
			// must not call a write path. Cheap to state and easy to violate
			// later, which is exactly what makes it worth asserting.
			name := handlerName(r.handler)
			if body, found := bodies[name]; found {
				for _, forbidden := range []string{
					"s.auditWrite(", "Create(", "Update(", "Archive(", "Delete(",
					"RunNow(", "Approve(", "Resume(", "PauseContainer(",
				} {
					if strings.Contains(body, forbidden) {
						t.Errorf("route %s %s is declared read-only but its handler calls %s",
							r.method, r.pattern, forbidden)
					}
				}
			}
			continue
		}

		name := handlerName(r.handler)
		body, ok := bodies[name]
		if !ok {
			t.Errorf("route %s %s resolves to handler %q, whose source could not be found\n"+
				"\tthe test locates the method by name; an anonymous or wrapped handler "+
				"cannot be checked and must not serve a write route",
				r.method, r.pattern, name)
			continue
		}
		if !strings.Contains(body, "s.auditWrite(") {
			t.Errorf("route %s %s (%s) changes state but records no actor\n"+
				"\tcall s.auditWrite(r, action, targetType, targetID, targetName, reason) "+
				"at the point the write succeeds, or add the route to auditedElsewhere "+
				"with the service method that records it",
				r.method, r.pattern, name)
		}
	}
}

// roleConstants are the identifiers a handler must not name.
//
// Naming one means an authorization decision has moved out of the route table
// and into a handler, where the next reader will not find it and the
// default-deny test cannot see it.
var roleConstants = []string{
	"RoleAdministrator", "RoleOperator", "RoleViewer",
}

// TestNoHandlerChecksARoleDirectly fails if a handler file names a role.
//
// # The rule, stated precisely
//
// Authorization is decided once, by the middleware, from the route table. A
// handler receives an already-authorized request. The identity it can read is
// for ATTRIBUTION -- writing the actor into an audit row -- and for the two
// self-reference checks that are not authorization at all (an administrator may
// not change their own role, and a session list is the caller's own).
//
// A handler that compares a role is a second decision in a second place, and
// the two will disagree eventually. This test forbids it structurally.
//
// The route table and the middleware are exempt: the table names PERMISSIONS,
// not roles, and access.go is where the identity plumbing lives.
func TestNoHandlerChecksARoleDirectly(t *testing.T) {
	exempt := map[string]bool{
		// The middleware resolves the identity; it must be able to speak about
		// what a role holds.
		"auth_middleware.go": true,
		// The user administration handlers PARSE a role out of a request body.
		// That is data validation, not an authorization decision, and it goes
		// through domain.ValidRole rather than through a comparison -- which
		// this test still verifies below.
		"user_handlers.go": true,
	}

	for _, file := range packageFiles(t) {
		base := filepath.Base(file)
		if !strings.HasSuffix(base, "_handlers.go") || exempt[base] {
			continue
		}

		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", base, err)
		}
		text := string(source)

		for _, constant := range roleConstants {
			if !strings.Contains(text, constant) {
				continue
			}
			t.Errorf("%s names domain.%s\n"+
				"\tauthorization is decided in routes.go and enforced by the middleware; "+
				"a role comparison in a handler is a second policy in a second place",
				base, constant)
		}
	}
}

// TestUserHandlersDoNotCompareRoles narrows the exemption above.
//
// user_handlers.go is allowed to MENTION roles because it parses one out of a
// request body. It is not allowed to branch on one: that would be an
// authorization decision wearing validation's clothes.
func TestUserHandlersDoNotCompareRoles(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(".", "user_handlers.go"))
	if err != nil {
		t.Fatalf("read user_handlers.go: %v", err)
	}

	forbidden := []string{
		"== domain.RoleAdministrator", "!= domain.RoleAdministrator",
		"== domain.RoleOperator", "!= domain.RoleOperator",
		"== domain.RoleViewer", "!= domain.RoleViewer",
		".Role.Can(", ".Can(domain.Perm",
	}
	text := string(source)
	for _, pattern := range forbidden {
		if strings.Contains(text, pattern) {
			t.Errorf("user_handlers.go contains %q\n"+
				"\tit may parse and validate a role from a request body, but it must not "+
				"decide access from one; that decision belongs to the route table",
				pattern)
		}
	}
}

// TestPrivilegedRoutesRequireAPermission fails if either socket-touching
// operation is reachable without one.
//
// A belt-and-braces check over the default-deny test. Those two routes are the
// ones that pull an image and replace a running container; if the table is ever
// restructured, these must not be the entries that quietly become
// authenticated() rather than requires().
func TestPrivilegedRoutesRequireAPermission(t *testing.T) {
	privileged := map[string]domain.Permission{
		"POST " + APIPrefix + "/acquisitions": domain.PermAcquisitionCreate,
		"POST " + APIPrefix + "/executions":   domain.PermExecutionCreate,
	}

	seen := map[string]bool{}
	for _, r := range (&Server{}).routeTable() {
		key := methodLabel(r.method) + " " + r.pattern
		want, ok := privileged[key]
		if !ok {
			continue
		}
		seen[key] = true

		if r.access.kind != accessPermission || r.access.permission != want {
			t.Errorf("route %s is served under %q/%q, expected the %q permission\n"+
				"\tthis route reaches a root-equivalent Docker socket",
				key, r.access.kind, r.access.permission, want)
		}
		if !want.Privileged() {
			t.Errorf("permission %q is not marked privileged; it should be, so its use "+
				"is logged at a level a default configuration shows", want)
		}
	}

	for key := range privileged {
		if !seen[key] {
			t.Errorf("privileged route %s is not in the route table; if it was renamed, "+
				"update this test rather than deleting the check", key)
		}
	}
}

// TestCSRFExemptionsAreOnlyTheSessionlessRoutes pins which writes may skip the
// CSRF header.
//
// A CSRF token is derived from the session token, so a route reached without a
// session cannot present one. That is a real constraint and it applies to
// exactly two routes. Anywhere else, .noCSRF() would be removing a control
// rather than acknowledging one that cannot apply.
func TestCSRFExemptionsAreOnlyTheSessionlessRoutes(t *testing.T) {
	allowed := map[string]bool{
		APIPrefix + "/auth/login":     true,
		APIPrefix + "/auth/bootstrap": true,
	}

	for _, r := range (&Server{}).routeTable() {
		if !writeMethods[r.method] || !r.access.skipCSRF {
			continue
		}
		if allowed[r.pattern] {
			continue
		}
		t.Errorf("route %s %s is CSRF-exempt\n"+
			"\tonly login and bootstrap may be, because they have no session to derive "+
			"a token from; every other write must require the %s header",
			r.method, r.pattern, CSRFHeader)
	}
}

// ------------------------------------------------------------- plumbing --

// methodLabel renders a route's method, including the bare entry.
func methodLabel(method string) string {
	if method == "" {
		return "*"
	}
	return method
}

// handlerName resolves a bound method value to its source method name.
//
// A method value's runtime name is "pkg.(*Server).handleThing-fm"; the last
// element after the final dot, minus the "-fm" suffix, is the method.
func handlerName(handler http.HandlerFunc) string {
	full := runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name()
	if index := strings.LastIndex(full, "."); index >= 0 {
		full = full[index+1:]
	}
	return strings.TrimSuffix(full, "-fm")
}

// handlerBodies returns the source text of every method in the package, keyed
// by method name.
//
// Source text rather than an AST walk for the call expression: the check is
// "does this method call auditWrite", and a textual search of the method's own
// byte range answers it without needing to model every way a call can be
// spelled. The range is the AST's, so a call in an adjacent function is not
// included.
func handlerBodies(t *testing.T) map[string]string {
	t.Helper()

	bodies := map[string]string{}
	fileSet := token.NewFileSet()

	for _, path := range packageFiles(t) {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, err := parser.ParseFile(fileSet, path, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		base := fileSet.File(parsed.Pos()).Base()
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil {
				continue
			}
			start := int(fn.Body.Pos()) - base
			end := int(fn.Body.End()) - base
			if start < 0 || end > len(source) || start >= end {
				continue
			}
			bodies[fn.Name.Name] = string(source[start:end])
		}
	}

	if len(bodies) == 0 {
		t.Fatal("no methods parsed; the test is not reading the package source")
	}
	return bodies
}

// packageFiles lists the package's non-test Go files.
func packageFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(".", name))
	}
	return files
}
