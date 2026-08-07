package api

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The authentication and authorization tests.
//
// These exercise the MIDDLEWARE against the real route table. Together with
// TestEveryRouteDeclaresAnAccessPolicy in access_arch_test.go -- which proves
// every route has a policy -- they cover the two halves of the claim: the
// policies exist, and they are enforced.

// authServer builds a server with a full route surface and a stub identity.
func authServer(role domain.Role) (*Server, *stubAuth, *stubAudit) {
	return asRole(Options{
		Health: &fakeHealth{},
		Config: config.Server{MaxRequestBytes: 4096},
		Assets: testAssets(),
	}, role)
}

// request builds a JSON request, authenticated unless anonymously is set.
func request(method, target, body string) *http.Request {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, target, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	return req
}

// send dispatches a request and returns the recorder.
func send(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// errorCode reads the machine-readable code out of an error response.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) ErrorCode {
	t.Helper()

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if body.Error.Code != "" {
		return ErrorCode(body.Error.Code)
	}
	return ErrorCode(body.Code)
}

// ------------------------------------------------------------- the surface --

// TestOnlyTheFourPublicRoutesAnswerWithoutASession is the headline property.
//
// Every route in the table is exercised anonymously. The four documented public
// ones answer; everything else answers 401. This is the test that would fail if
// somebody added an endpoint and forgot its policy, and it walks the SAME table
// the server is built from rather than a hand-maintained list.
func TestOnlyTheFourPublicRoutesAnswerWithoutASession(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	for _, route := range srv.routeTable() {
		if route.method == "" || route.handler == nil {
			continue
		}
		// The bootstrap POST answers 404 on a claimed installation, which is
		// checked separately below.
		if route.access.kind == accessBootstrap {
			continue
		}

		req := anonymous(request(route.method, concreteTarget(route.pattern),
			anonymousBody(route.pattern)))
		rec := send(srv, req)

		if route.access.kind == accessPublic {
			// A public route must not be refused BY THE MIDDLEWARE. Login can
			// still answer 401 on its own terms -- that is the endpoint doing
			// its job -- so the assertion is on the code, not the status.
			if rec.Code == http.StatusUnauthorized &&
				errorCode(t, rec) == CodeUnauthenticated {
				t.Errorf("public route %s %s was refused for want of a session",
					route.method, route.pattern)
			}
			continue
		}

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d to an anonymous caller, want 401\n"+
				"\tHarborMaster fronts a root-equivalent Docker socket; no estate data "+
				"and no mutation may be reachable without a session",
				route.method, route.pattern, rec.Code)
		}
	}
}

// anonymousBody supplies a body that lets a public route reach its handler.
//
// Only login needs one: every other public route is a GET. Credentials that the
// stub accepts, so the sweep measures the MIDDLEWARE rather than the endpoint's
// own answer to a wrong password.
func anonymousBody(pattern string) string {
	if pattern == APIPrefix+"/auth/login" {
		return `{"username":"tester","password":"the correct passphrase"}`
	}
	return "{}"
}

// concreteTarget substitutes a plausible value for each path parameter.
func concreteTarget(pattern string) string {
	target := strings.ReplaceAll(pattern, "{id}", "1")
	return target
}

// TestTheBootstrapEndpointDisappearsOnceClaimed checks the accessBootstrap
// behaviour.
//
// 404 rather than 403: on a claimed installation the endpoint genuinely does
// not exist, and saying "forbidden" would confirm that a bootstrap flow is
// there to be raced.
func TestTheBootstrapEndpointDisappearsOnceClaimed(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/bootstrap",
		`{"token":"the-bootstrap-token","username":"admin","password":"a decent passphrase"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("bootstrap on a claimed installation = %d, want 404", rec.Code)
	}

	// Unclaimed, it works.
	auth.mu.Lock()
	auth.claimed = false
	auth.mu.Unlock()
	srv.claimed.Store(false)

	rec = send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/bootstrap",
		`{"token":"the-bootstrap-token","username":"admin","password":"a decent passphrase"}`)))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap on an unclaimed installation = %d (%s)", rec.Code, rec.Body.String())
	}
	if cookie := rec.Header().Get("Set-Cookie"); !strings.Contains(cookie, SessionCookieName) {
		t.Errorf("bootstrap issued no session cookie (Set-Cookie: %q)", cookie)
	}
}

func TestAWrongBootstrapTokenIsRefused(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.claimed = false
	auth.mu.Unlock()
	srv.claimed.Store(false)

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/bootstrap",
		`{"token":"guessed","username":"admin","password":"a decent passphrase"}`)))
	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatal("a wrong bootstrap token claimed the installation")
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("a wrong bootstrap token = %d, want 401 or 403", rec.Code)
	}
}

// TestAnUnclaimedInstallationSaysSoRatherThanJustRefusing keeps the SPA able
// to route the operator to the bootstrap page.
func TestAnUnclaimedInstallationSaysSoRatherThanJustRefusing(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.claimed = false
	auth.mu.Unlock()
	srv.claimed.Store(false)

	rec := send(srv, anonymous(request(http.MethodGet, APIPrefix+"/inventory", "")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := errorCode(t, rec); code != CodeBootstrapRequired {
		t.Errorf("code = %q, want %q", code, CodeBootstrapRequired)
	}
}

// --------------------------------------------------------------- the health --

// TestAnonymousHealthIsReducedToOneBit is the reconnaissance control.
func TestAnonymousHealthIsReducedToOneBit(t *testing.T) {
	health := &fakeHealth{report: domain.HealthReport{
		Status:   domain.StatusDegraded,
		Database: domain.Component{Status: domain.StatusUp, Detail: "/var/lib/harbormaster/hm.db"},
		Docker:   domain.Component{Status: domain.StatusDown, Detail: "dial unix /var/run/docker.sock"},
	}}
	srv, _, _ := asRole(Options{
		Health: health,
		Config: config.Server{MaxRequestBytes: 4096},
		Assets: testAssets(),
	}, domain.RoleViewer)

	anon := send(srv, anonymous(request(http.MethodGet, APIPrefix+"/health", "")))
	if anon.Code != http.StatusOK {
		t.Fatalf("anonymous health = %d, want 200", anon.Code)
	}
	body := anon.Body.String()
	if !strings.Contains(body, string(domain.StatusDegraded)) {
		t.Errorf("the anonymous body carries no status: %s", body)
	}
	for _, secret := range []string{"harbormaster.db", "docker.sock", "database", "docker"} {
		if strings.Contains(strings.ToLower(body), secret) {
			t.Errorf("the anonymous health body discloses %q: %s", secret, body)
		}
	}

	full := send(srv, authed(request(http.MethodGet, APIPrefix+"/health", "")))
	if !strings.Contains(full.Body.String(), "docker.sock") {
		t.Errorf("an authenticated caller does not get the full report: %s", full.Body.String())
	}
}

// ------------------------------------------------------------ authorization --

// TestEveryPermissionRouteRefusesARoleThatLacksIt walks the table again, this
// time authenticated as each role.
//
// The assertion is exact: a route answers 403 if and only if the role does not
// hold its permission. That covers both halves of an authorization bug -- a
// role that can reach too much, and one that cannot reach enough.
func TestEveryPermissionRouteRefusesARoleThatLacksIt(t *testing.T) {
	for _, role := range []domain.Role{domain.RoleViewer, domain.RoleOperator, domain.RoleAdministrator} {
		t.Run(string(role), func(t *testing.T) {
			srv, _, audit := authServer(role)

			for _, route := range srv.routeTable() {
				if route.method == "" || route.handler == nil {
					continue
				}
				if route.access.kind != accessPermission {
					continue
				}

				rec := send(srv, authed(request(route.method,
					concreteTarget(route.pattern), "{}")))
				forbidden := rec.Code == http.StatusForbidden &&
					errorCode(t, rec) == CodeForbidden

				if allowed := role.Can(route.access.permission); allowed && forbidden {
					t.Errorf("%s %s refused a %s, which holds %q",
						route.method, route.pattern, role, route.access.permission)
				} else if !allowed && !forbidden {
					t.Errorf("%s %s answered %d to a %s, which does NOT hold %q; want 403",
						route.method, route.pattern, rec.Code, role, route.access.permission)
				}
			}

			// Every denial is recorded. A single 403 is a mistake; a pattern of
			// them is somebody looking for a way in, and the pattern is only
			// visible if all of them are written down.
			if role != domain.RoleAdministrator {
				if len(audit.recorded(domain.AuditAuthorizationDenied)) == 0 {
					t.Error("no authorization denial was audited for a role that was refused")
				}
			}
		})
	}
}

// TestADenialAuditRecordsThePermissionNotThePath is the log-injection control.
//
// A permission is a closed vocabulary. A path is request-derived text that
// reaches a page an administrator reads.
func TestADenialAuditRecordsThePermissionNotThePath(t *testing.T) {
	srv, _, audit := authServer(domain.RoleViewer)

	send(srv, authed(request(http.MethodPost, APIPrefix+"/executions",
		`{"acquisitionId":"acq_1"}`)))

	denials := audit.recorded(domain.AuditAuthorizationDenied)
	if len(denials) == 0 {
		t.Fatal("the denial was not audited")
	}
	event := denials[0]
	if event.TargetID != string(domain.PermExecutionCreate) {
		t.Errorf("the denial names %q, want the permission %q",
			event.TargetID, domain.PermExecutionCreate)
	}
	if strings.Contains(event.TargetID, "/") {
		t.Error("the denial records a path; a request-derived value must not reach the audit log")
	}
	if event.ActorUsername != "tester" {
		t.Errorf("the denial is attributed to %q, want the caller", event.ActorUsername)
	}
}

// TestAViewerCannotReachEitherSocketTouchingRoute is the one worth spelling out
// separately.
func TestAViewerCannotReachEitherSocketTouchingRoute(t *testing.T) {
	srv, _, _ := authServer(domain.RoleViewer)

	for _, target := range []string{
		APIPrefix + "/acquisitions",
		APIPrefix + "/executions",
	} {
		rec := send(srv, authed(request(http.MethodPost, target, `{"acquisitionId":"acq_1"}`)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as a viewer = %d, want 403", target, rec.Code)
		}
	}
}

// TestAnOperatorCannotEditAPolicyOrAnAccount is the operator ceiling.
func TestAnOperatorCannotEditAPolicyOrAnAccount(t *testing.T) {
	srv, _, _ := authServer(domain.RoleOperator)

	cases := []struct {
		method, target, body string
	}{
		{http.MethodPost, APIPrefix + "/policies", `{"name":"x"}`},
		{http.MethodPatch, APIPrefix + "/policies/pol_1", `{"name":"x"}`},
		{http.MethodDelete, APIPrefix + "/policies/pol_1", ""},
		{http.MethodGet, APIPrefix + "/users", ""},
		{http.MethodPost, APIPrefix + "/users", `{"username":"x","role":"viewer"}`},
		{http.MethodGet, APIPrefix + "/audit", ""},
	}
	for _, testCase := range cases {
		rec := send(srv, authed(request(testCase.method, testCase.target, testCase.body)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as an operator = %d, want 403\n"+
				"\ta policy is what BLOCKS an operator's action, and the audit log is "+
				"what records it; neither may be editable or readable by them",
				testCase.method, testCase.target, rec.Code)
		}
	}
}

// ---------------------------------------------------------------- the CSRF --

func TestEveryAuthenticatedWriteNeedsTheCSRFHeader(t *testing.T) {
	srv, auth, audit := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.acceptAnyCSRF = false
	auth.mu.Unlock()

	checked := 0
	for _, route := range srv.routeTable() {
		if route.handler == nil || !writeMethods[route.method] {
			continue
		}
		if route.access.skipCSRF {
			continue
		}

		// The cookie but NOT the header, which is exactly what a cross-site
		// form submission can produce.
		req := request(route.method, concreteTarget(route.pattern), "{}")
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})
		rec := send(srv, req)
		checked++

		if rec.Code != http.StatusForbidden || errorCode(t, rec) != CodeCSRF {
			t.Errorf("%s %s without a CSRF header = %d (%s), want 403/%s",
				route.method, route.pattern, rec.Code, errorCode(t, rec), CodeCSRF)
		}
	}
	if checked == 0 {
		t.Fatal("no write route was checked; the test would pass vacuously")
	}
	if len(audit.recorded(domain.AuditCSRFRejected)) == 0 {
		t.Error("a rejected state-changing request was not audited")
	}
}

func TestAWrongCSRFTokenIsRefused(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.acceptAnyCSRF = false
	auth.mu.Unlock()

	req := request(http.MethodPost, APIPrefix+"/inventory/refresh", "{}")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})
	req.Header.Set(CSRFHeader, "not-the-token")

	if rec := send(srv, req); rec.Code != http.StatusForbidden {
		t.Errorf("a wrong CSRF token = %d, want 403", rec.Code)
	}
}

// TestReadsDoNotNeedACSRFToken keeps the control proportionate: a GET changes
// nothing, and requiring a header on it would break every link.
func TestReadsDoNotNeedACSRFToken(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.acceptAnyCSRF = false
	auth.mu.Unlock()

	req := request(http.MethodGet, APIPrefix+"/version", "")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})

	if rec := send(srv, req); rec.Code != http.StatusOK {
		t.Errorf("an authenticated GET without a CSRF header = %d, want 200", rec.Code)
	}
}

// ------------------------------------------------------------- the cookie --

func TestTheSessionCookieIsHttpOnlyAndSameSite(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/login",
		`{"username":"tester","password":"the correct passphrase"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", rec.Code, rec.Body.String())
	}

	cookie := rec.Header().Get("Set-Cookie")
	for _, attribute := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, attribute) {
			t.Errorf("the session cookie is missing %s: %q", attribute, cookie)
		}
	}
	if strings.Contains(cookie, "Domain=") {
		t.Errorf("the session cookie sets a Domain, which widens it to subdomains: %q", cookie)
	}
}

// TestNoTokenReachesAResponseBody is the disclosure sweep on the login
// response.
func TestNoTokenReachesAResponseBody(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/login",
		`{"username":"tester","password":"the correct passphrase"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, testSessionToken) {
		t.Error("the login response body contains the session token; it belongs in " +
			"an HttpOnly cookie and nowhere a script can read it")
	}
	if strings.Contains(body, "the correct passphrase") {
		t.Error("the login response echoes the password")
	}
	// The CSRF token DOES belong in the body: a script has to send it back in
	// a header, and it is useless without the cookie a script cannot read.
	if !strings.Contains(body, testCSRFToken) {
		t.Error("the login response carries no CSRF token, so a client could not " +
			"make a write request")
	}
}

func TestALoginFailureSaysNothingSpecific(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/login",
		`{"username":"tester","password":"the wrong passphrase"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a failed login = %d, want 401", rec.Code)
	}

	body := strings.ToLower(rec.Body.String())
	for _, disclosure := range []string{"unknown", "disabled", "locked", "no such user", "not found"} {
		if strings.Contains(body, disclosure) {
			t.Errorf("the failure response contains %q: %s", disclosure, rec.Body.String())
		}
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("a failed login set a cookie")
	}
}

// TestAnOversizedCredentialIsRefusedLikeAWrongOne bounds the one expensive
// operation offered to anonymous callers.
func TestAnOversizedCredentialIsRefusedLikeAWrongOne(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)

	body, err := json.Marshal(map[string]string{
		"username": "tester",
		"password": strings.Repeat("x", domain.MaxPasswordBytes+1),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := send(srv, anonymous(request(http.MethodPost, APIPrefix+"/auth/login", string(body))))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest &&
		rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized password = %d, want a refusal", rec.Code)
	}

	auth.mu.Lock()
	calls := auth.loginCalls
	auth.mu.Unlock()
	if calls != 0 {
		t.Error("an oversized password reached the hashing path; the bound exists " +
			"so an anonymous caller cannot make the server hash unbounded input")
	}
}

// ------------------------------------------------------- forced password change --

// TestAnAccountThatMustChangeItsPasswordCanDoNothingElse.
//
// Three routes stay reachable -- read the session, change the password, sign
// out -- because without them the account could not recover. Everything else is
// refused with a code the SPA can act on.
func TestAnAccountThatMustChangeItsPasswordCanDoNothingElse(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.user.MustChangePassword = true
	auth.mu.Unlock()

	allowed := map[string]bool{
		APIPrefix + "/auth/session":  true,
		APIPrefix + "/auth/password": true,
		APIPrefix + "/auth/logout":   true,
	}

	for _, route := range srv.routeTable() {
		if route.method == "" || route.handler == nil {
			continue
		}
		if route.access.kind == accessPublic || route.access.kind == accessBootstrap {
			continue
		}

		rec := send(srv, authed(request(route.method, concreteTarget(route.pattern), "{}")))
		blocked := rec.Code == http.StatusForbidden &&
			errorCode(t, rec) == CodePasswordChangeRequired

		if allowed[route.pattern] {
			if blocked {
				t.Errorf("%s %s is blocked during a forced password change; without it "+
					"the account could not recover", route.method, route.pattern)
			}
			continue
		}
		if !blocked {
			t.Errorf("%s %s answered %d during a forced password change, want 403/%s",
				route.method, route.pattern, rec.Code, CodePasswordChangeRequired)
		}
	}
}

// TestChangingThePasswordRotatesTheSession.
func TestChangingThePasswordRotatesTheSession(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/auth/password",
		`{"currentPassword":"the correct passphrase","newPassword":"a brand new passphrase"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("change password = %d (%s)", rec.Code, rec.Body.String())
	}
	if cookie := rec.Header().Get("Set-Cookie"); !strings.Contains(cookie, SessionCookieName) {
		t.Errorf("the password change issued no replacement cookie: %q", cookie)
	}
	if !strings.Contains(rec.Body.String(), testCSRFToken) {
		t.Error("the password change returned no CSRF token; the derived token changes " +
			"with the session, so a client that kept the old one would be refused")
	}
	if strings.Contains(rec.Body.String(), "the correct passphrase") ||
		strings.Contains(rec.Body.String(), "a brand new passphrase") {
		t.Error("the password change response echoes a password")
	}
}

func TestChangingThePasswordNeedsTheCurrentOne(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/auth/password",
		`{"currentPassword":"wrong","newPassword":"a brand new passphrase"}`)))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("a wrong current password = %d, want a refusal", rec.Code)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("a refused password change rotated the session anyway")
	}
}

// ------------------------------------------------------- session management --

func TestLogoutClearsTheCookie(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/auth/logout", "{}")))
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("logout = %d (%s)", rec.Code, rec.Body.String())
	}

	cookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, SessionCookieName) {
		t.Fatalf("logout set no cookie header: %q", cookie)
	}
	if !strings.Contains(cookie, "Max-Age=0") && !strings.Contains(cookie, "Expires=") {
		t.Errorf("logout did not expire the cookie: %q", cookie)
	}
}

// Signing out over HTTPS deletes the plain-name cookie with Secure set.
//
// # Why the deletion's Secure attribute matters at all
//
// A cookie's identity is its name, domain, and path; the Secure attribute is a
// property of the cookie being WRITTEN, not part of what it matches. So a
// Secure deletion still removes a non-Secure cookie of the same name — which
// means there is no reason to send the deletion insecurely when the connection
// is secure, and one reason not to.
//
// This is also what removes the last constant-false Secure attribute in the
// codebase. `go/cookie-secure-not-set` reported the old hardcoded one.
func TestSigningOutOverTLSDeletesThePlainCookieSecurely(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	req := authed(request(http.MethodPost, APIPrefix+"/auth/logout", "{}"))
	// What a TLS-terminating server sets, and what makes requestIsSecure true.
	req.TLS = &tls.ConnectionState{}

	rec := send(srv, req)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("logout = %d (%s)", rec.Code, rec.Body.String())
	}

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != SessionCookieName && cookie.Name != SecureSessionCookieName {
			continue
		}
		if !cookie.Secure {
			t.Errorf("signing out over TLS wrote %s without Secure; the deletion "+
				"can then be replayed over a downgraded request, and nothing is "+
				"gained by omitting it", cookie.Name)
		}
		if cookie.Value != "" {
			t.Errorf("the deletion for %s carries a value: %q", cookie.Name, cookie.Value)
		}
		if cookie.MaxAge >= 0 {
			t.Errorf("the deletion for %s does not expire: MaxAge=%d",
				cookie.Name, cookie.MaxAge)
		}
	}
}

// Signing out over PLAIN HTTP still deletes the plain-name cookie.
//
// The one case that writes a cookie without Secure, and the reason the
// attribute cannot simply be hardcoded true: a browser rejects a Secure cookie
// set from an insecure origin, so the deletion would be silently dropped and
// the dead token would stay in the browser.
func TestSigningOutOverPlainHTTPStillDeletesThePlainCookie(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	req := authed(request(http.MethodPost, APIPrefix+"/auth/logout", "{}"))
	// httptest.NewRequest's default peer is 192.0.2.1, which is NOT loopback --
	// and `requestIsSecure` treats any non-loopback peer as secure, because a
	// peer that is not on this machine is one whose traffic left it. Set the
	// peer that the plain-HTTP deployment actually has.
	req.RemoteAddr = "127.0.0.1:54321"

	rec := send(srv, req)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("logout = %d (%s)", rec.Code, rec.Body.String())
	}

	var deleted bool
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != SessionCookieName {
			continue
		}
		deleted = true
		if cookie.Secure {
			t.Error("signing out over plain HTTP wrote the plain cookie with " +
				"Secure; a browser rejects that from an insecure origin, so the " +
				"session token would stay in the browser")
		}
		if cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Errorf("the plain cookie was not deleted: value=%q maxAge=%d",
				cookie.Value, cookie.MaxAge)
		}
	}
	if !deleted {
		t.Fatal("signing out over plain HTTP did not delete the plain cookie at all")
	}

	// And the __Host- name is still cleared, with Secure, because it can only
	// ever be replaced by a Secure cookie. The browser rejects it here, which
	// costs nothing: a __Host- cookie could never have been set on this origin.
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == SecureSessionCookieName && !cookie.Secure {
			t.Error("the __Host- deletion was written without Secure, which no " +
				"browser will accept for that prefix")
		}
	}
}

// TestADeadSessionClearsTheCookieSoTheBrowserStopsSendingIt turns a permanent
// 401 loop into one redirect to the login page.
func TestADeadSessionClearsTheCookieSoTheBrowserStopsSendingIt(t *testing.T) {
	srv, auth, _ := authServer(domain.RoleAdministrator)
	auth.mu.Lock()
	auth.rejectToken = true
	auth.mu.Unlock()

	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/inventory", "")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a dead session = %d, want 401", rec.Code)
	}
	if cookie := rec.Header().Get("Set-Cookie"); !strings.Contains(cookie, "Max-Age=0") {
		t.Errorf("a dead session did not clear the cookie: %q", cookie)
	}
}

// ---------------------------------------------------- user administration --

func TestCreatingAnAccountReturnsItsPasswordOnceAndNeverAgain(t *testing.T) {
	srv, _, audit := authServer(domain.RoleAdministrator)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/users",
		`{"username":"newoperator","role":"operator"}`)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create user = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a-generated-temporary-password") {
		t.Error("the create response does not return the temporary password, so " +
			"nobody could sign in as the new account")
	}

	// Reading the account back must not return it.
	list := send(srv, authed(request(http.MethodGet, APIPrefix+"/users", "")))
	if strings.Contains(list.Body.String(), "a-generated-temporary-password") {
		t.Error("the user list contains a password")
	}
	for _, event := range audit.recorded(domain.AuditUserCreated) {
		if strings.Contains(event.Reason, "a-generated-temporary-password") {
			t.Error("an audit reason contains a password")
		}
	}
}

func TestAnInvalidRoleIsRefusedRatherThanDefaulted(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	for _, body := range []string{
		`{"username":"someone","role":"root"}`,
		`{"username":"someone","role":""}`,
		`{"username":"someone","role":"Administrator"}`,
		`{"username":"Bad Name","role":"viewer"}`,
	} {
		rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/users", body)))
		if rec.Code < 400 {
			t.Errorf("create with %s = %d; an unrecognised role must be refused, "+
				"never defaulted to the least privileged one", body, rec.Code)
		}
	}
}

// TestAnAdministratorCannotChangeTheirOwnRoleOrStatus.
func TestAnAdministratorCannotChangeTheirOwnRoleOrStatus(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	for _, body := range []string{`{"role":"viewer"}`, `{"status":"disabled"}`} {
		rec := send(srv, authed(request(http.MethodPatch,
			APIPrefix+"/users/usr_test0000000000000000", body)))
		if rec.Code < 400 {
			t.Errorf("self-modification with %s = %d, want a refusal", body, rec.Code)
		}
	}
}

// TestTheRoleCatalogueComesFromTheAuthorizationModel keeps the UI's picker and
// the middleware reading the same source of truth.
func TestTheRoleCatalogueComesFromTheAuthorizationModel(t *testing.T) {
	srv, _, _ := authServer(domain.RoleAdministrator)

	rec := send(srv, authed(request(http.MethodGet, APIPrefix+"/roles", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("roles = %d (%s)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, role := range []domain.Role{domain.RoleViewer, domain.RoleOperator, domain.RoleAdministrator} {
		if !strings.Contains(body, string(role)) {
			t.Errorf("the catalogue omits %q", role)
		}
	}
	if !strings.Contains(body, string(domain.PermExecutionCreate)) {
		t.Error("the catalogue omits the permissions each role holds, so a UI could " +
			"not explain what it is granting")
	}
}

// ------------------------------------------------------------- attribution --

// TestAWriteIsAttributedToItsCaller is the phase's headline change, checked
// end to end through the HTTP layer.
func TestAWriteIsAttributedToItsCaller(t *testing.T) {
	inventory := &fakeInventory{enabled: true, accept: true}
	srv, _, audit := asRole(Options{
		Health:    &fakeHealth{},
		Inventory: inventory,
		Config:    config.Server{MaxRequestBytes: 4096},
		Assets:    testAssets(),
	}, domain.RoleOperator)

	rec := send(srv, authed(request(http.MethodPost, APIPrefix+"/inventory/refresh", "{}")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("refresh = %d (%s)", rec.Code, rec.Body.String())
	}

	events := audit.recorded(domain.AuditInventoryRefreshed)
	if len(events) != 1 {
		t.Fatalf("%d inventory-refresh audit events, want 1", len(events))
	}
	event := events[0]
	if event.ActorUsername != "tester" || event.ActorRole != domain.RoleOperator {
		t.Errorf("the write is attributed to %q/%q, want tester/operator",
			event.ActorUsername, event.ActorRole)
	}
	if event.ActorSessionID == "" {
		t.Error("the audit row does not name the session the write came from")
	}
	if event.RequestID == "" {
		t.Error("the audit row carries no request id, so it cannot be correlated " +
			"with the access log")
	}
	if event.Outcome != domain.AuditSucceeded {
		t.Errorf("outcome = %q, want succeeded", event.Outcome)
	}
}

// TestTheClientAddressIsTheTransportPeerWithoutATrustedProxy is the spoofing
// control.
func TestTheClientAddressIsTheTransportPeerWithoutATrustedProxy(t *testing.T) {
	inventory := &fakeInventory{enabled: true, accept: true}
	srv, _, audit := asRole(Options{
		Health:    &fakeHealth{},
		Inventory: inventory,
		Config:    config.Server{MaxRequestBytes: 4096},
		Assets:    testAssets(),
	}, domain.RoleOperator)

	req := authed(request(http.MethodPost, APIPrefix+"/inventory/refresh", "{}"))
	req.RemoteAddr = "192.0.2.50:41234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Forwarded", "for=203.0.113.9")

	if rec := send(srv, req); rec.Code != http.StatusAccepted {
		t.Fatalf("refresh = %d (%s)", rec.Code, rec.Body.String())
	}

	events := audit.recorded(domain.AuditInventoryRefreshed)
	if len(events) != 1 {
		t.Fatalf("%d audit events, want 1", len(events))
	}
	if events[0].ClientAddr != "192.0.2.50" {
		t.Errorf("client address = %q, want the transport peer 192.0.2.50\n"+
			"\ta forwarding header is attacker-controlled text and must not be "+
			"believed without a trusted-proxy configuration",
			events[0].ClientAddr)
	}
}

// TestATrustedProxyIsBelievedAndAnUntrustedOneIsNot.
func TestATrustedProxyIsBelievedAndAnUntrustedOneIsNot(t *testing.T) {
	build := func(trusted []string) (*Server, *stubAudit) {
		cfg := testAuthConfig()
		cfg.TrustedProxies = trusted
		srv, _, audit := asRole(Options{
			Health:     &fakeHealth{},
			Inventory:  &fakeInventory{enabled: true, accept: true},
			Config:     config.Server{MaxRequestBytes: 4096},
			AuthConfig: cfg,
			Assets:     testAssets(),
		}, domain.RoleOperator)
		return srv, audit
	}

	cases := map[string]struct {
		trusted []string
		want    string
	}{
		"a trusted peer is believed":   {[]string{"192.0.2.0/24"}, "203.0.113.9"},
		"an untrusted peer is not":     {[]string{"198.51.100.0/24"}, "192.0.2.50"},
		"no configuration trusts none": {nil, "192.0.2.50"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			srv, audit := build(testCase.trusted)

			req := authed(request(http.MethodPost, APIPrefix+"/inventory/refresh", "{}"))
			req.RemoteAddr = "192.0.2.50:41234"
			req.Header.Set("X-Forwarded-For", "203.0.113.9")

			if rec := send(srv, req); rec.Code != http.StatusAccepted {
				t.Fatalf("refresh = %d (%s)", rec.Code, rec.Body.String())
			}
			events := audit.recorded(domain.AuditInventoryRefreshed)
			if len(events) != 1 {
				t.Fatalf("%d audit events, want 1", len(events))
			}
			if events[0].ClientAddr != testCase.want {
				t.Errorf("client address = %q, want %q", events[0].ClientAddr, testCase.want)
			}
		})
	}
}

// TestAForgedForwardingChainCannotHideTheRealPeer.
//
// The walk is right to left and stops at the first untrusted hop, so an
// attacker prepending entries to X-Forwarded-For moves nothing.
func TestAForgedForwardingChainCannotHideTheRealPeer(t *testing.T) {
	cfg := testAuthConfig()
	cfg.TrustedProxies = []string{"192.0.2.0/24"}
	srv, _, audit := asRole(Options{
		Health:     &fakeHealth{},
		Inventory:  &fakeInventory{enabled: true, accept: true},
		Config:     config.Server{MaxRequestBytes: 4096},
		AuthConfig: cfg,
		Assets:     testAssets(),
	}, domain.RoleOperator)

	req := authed(request(http.MethodPost, APIPrefix+"/inventory/refresh", "{}"))
	req.RemoteAddr = "192.0.2.50:41234"
	// The attacker controls everything left of their own entry.
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 127.0.0.1, 203.0.113.9")

	if rec := send(srv, req); rec.Code != http.StatusAccepted {
		t.Fatalf("refresh = %d (%s)", rec.Code, rec.Body.String())
	}
	events := audit.recorded(domain.AuditInventoryRefreshed)
	if len(events) != 1 {
		t.Fatalf("%d audit events, want 1", len(events))
	}
	if events[0].ClientAddr != "203.0.113.9" {
		t.Errorf("client address = %q, want 203.0.113.9 -- the rightmost entry that "+
			"a trusted proxy actually added", events[0].ClientAddr)
	}
}

// ------------------------------------------------------------------- SSE --

// TestTheEventStreamRequiresASession.
//
// EventSource cannot set headers, so the stream is authenticated by the cookie
// alone -- which is exactly why it must still be authenticated. An open stream
// would be a live feed of the estate to anyone who can reach the port.
func TestTheEventStreamRequiresASession(t *testing.T) {
	srv, _, _ := authServer(domain.RoleViewer)

	rec := send(srv, anonymous(request(http.MethodGet, APIPrefix+"/events/stream", "")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous event stream = %d, want 401", rec.Code)
	}
}

// TestTheAuditEndpointsNeedTheAuditPermission.
func TestTheAuditEndpointsNeedTheAuditPermission(t *testing.T) {
	for role, want := range map[domain.Role]int{
		domain.RoleViewer:        http.StatusForbidden,
		domain.RoleOperator:      http.StatusForbidden,
		domain.RoleAdministrator: http.StatusOK,
	} {
		srv, _, _ := authServer(role)
		for _, target := range []string{APIPrefix + "/audit", APIPrefix + "/audit/summary"} {
			rec := send(srv, authed(request(http.MethodGet, target, "")))
			if rec.Code != want {
				t.Errorf("GET %s as %s = %d, want %d", target, role, rec.Code, want)
			}
		}
	}
}

// TestTheSPAShellIsServedWithoutASession.
//
// It contains no data, and requiring a session to fetch it would mean there was
// no page to log in from.
func TestTheSPAShellIsServedWithoutASession(t *testing.T) {
	srv, _, _ := authServer(domain.RoleViewer)

	rec := send(srv, anonymous(request(http.MethodGet, "/", "")))
	if rec.Code != http.StatusOK {
		t.Errorf("the SPA shell = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "container") {
		t.Error("the shell contains estate data")
	}
}
