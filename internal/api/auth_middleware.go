package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/logging"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Authentication, authorization, and CSRF, as one wrapper per route.
//
// # Why per-route rather than one matcher in front of the mux
//
// A middleware in front of the mux would have to decide which route a request
// is going to hit BEFORE the mux does -- which means a second copy of the
// routing rules, kept in step by hand. Two matchers that disagree is exactly
// how an endpoint ends up unprotected.
//
// So each handler is wrapped with its own policy at registration. The mux does
// the matching, once, and the policy travels with the handler it belongs to.
// The cost is that a route must be registered through the table; the benefit is
// that it CANNOT be registered without a policy, because the registration
// function takes one.
//
// # The order of the checks
//
//  1. Bootstrap gating -- an unclaimed installation serves almost nothing.
//  2. Authentication -- is there a live session.
//  3. Password change -- an account with a temporary credential goes nowhere
//     else first.
//  4. Authorization -- does the identity hold the permission.
//  5. Write guard -- fetch metadata, origin, rate limit.
//  6. CSRF -- the header, compared in constant time.
//
// Authentication before authorization so an anonymous request gets 401 rather
// than 403, and the CSRF check last so a request that was going to be refused
// anyway is not told whether its token was right.

// Authentication error codes. Distinct from the generic ones so a client can
// branch on "log in again" versus "you may not do this".
const (
	// CodeUnauthenticated marks a request with no usable session.
	CodeUnauthenticated ErrorCode = "unauthenticated"
	// CodeForbidden marks an authenticated request lacking a permission.
	CodeForbidden ErrorCode = "forbidden"
	// CodeCSRF marks a state-changing request with a missing or wrong token.
	CodeCSRF ErrorCode = "csrf_required"
	// CodePasswordChangeRequired marks a session whose account must set a new
	// password before doing anything else.
	CodePasswordChangeRequired ErrorCode = "password_change_required"
	// CodeBootstrapRequired marks a request to an unclaimed installation.
	CodeBootstrapRequired ErrorCode = "bootstrap_required"
)

// CSRFHeader carries the per-session CSRF token on state-changing requests.
//
// A CUSTOM header, deliberately. A cross-origin form or a "simple" fetch cannot
// set one without triggering a preflight, and no CORS headers are served, so
// the preflight fails. That property is what makes the header meaningful over
// and above the token it carries.
const CSRFHeader = "X-HarborMaster-CSRF"

// SessionCookieName is the session cookie.
//
// The `__Host-` prefix is a browser-enforced guarantee, not a naming
// convention: a cookie with it MUST be Secure, MUST have Path=/, and MUST NOT
// have a Domain. That stops a sibling subdomain -- or a network attacker who
// can spoof one over plain HTTP -- from overwriting the session cookie, which
// is the standard session-fixation route.
//
// It only works over HTTPS. A plain-HTTP deployment gets the unprefixed name,
// chosen at write time by sessionCookieName.
const (
	SessionCookieName       = "harbormaster_session"
	SecureSessionCookieName = "__Host-harbormaster_session"
)

// sessionCookieName picks the cookie name for the connection's security.
func sessionCookieName(secure bool) string {
	if secure {
		return SecureSessionCookieName
	}
	return SessionCookieName
}

// readSessionToken pulls the session token out of a request.
//
// Both names are read, because a deployment can move between HTTP and HTTPS and
// a browser holding the other cookie should not be silently signed out. The
// secure name wins when both are present.
//
// Deliberately NOT read from a query parameter, a form field, or an
// Authorization header. A token in a URL ends up in access logs, browser
// history, and Referer headers -- which is why the requirement says so.
func readSessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(SecureSessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	return ""
}

// guard wraps a handler with its access policy.
//
// The ONLY way a route reaches the mux. Registration goes through this, so a
// route without a policy cannot be served -- there is no code path that
// registers a bare handler.
func (s *Server) guard(access routeAccess, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A policy that was never declared denies. This is the runtime half of
		// default-deny; the test half fails the build before it can happen.
		if !access.valid() {
			s.logger.ErrorContext(r.Context(), "route reached with no access policy",
				logging.SafeAttr("path", r.URL.Path))
			writeError(w, r, s.logger, http.StatusForbidden, CodeForbidden,
				"this endpoint has no access policy and is refused")
			return
		}

		authorized, ok := s.enforce(w, r, access)
		if !ok {
			return
		}
		// The AUGMENTED request is what reaches the handler: it carries the
		// resolved identity in its context. Returning it rather than mutating
		// in place is the only correct way -- http.Request's context is
		// immutable by design, and WithContext returns a shallow copy.
		handler(w, authorized)
	}
}

// enforce runs the policy.
//
// Returns the request the handler should receive -- which for an authenticated
// route carries the identity in its context -- and whether it may proceed at
// all. A false second return means a response has already been written.
func (s *Server) enforce(
	w http.ResponseWriter,
	r *http.Request,
	access routeAccess,
) (*http.Request, bool) {
	// ---- the authentication subsystem must exist ---------------------------
	//
	// A server built without it serves nothing but the public routes. That is
	// the fail-closed reading: a misconfiguration must not silently restore
	// anonymous access to a Docker mutation.
	if s.auth == nil {
		if access.kind == accessPublic {
			return r, true
		}
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"authentication is not configured on this server")
		return nil, false
	}

	claimed := s.installationClaimed(r)

	// ---- bootstrap gating --------------------------------------------------

	switch access.kind {
	case accessBootstrap:
		// Reachable only while unclaimed. Once an administrator exists this
		// answers 404 rather than 403: the endpoint genuinely no longer exists
		// for this installation, and saying so reveals nothing.
		if claimed {
			writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "endpoint not found")
			return nil, false
		}
		if !s.guardBootstrapWrite(w, r) {
			return nil, false
		}
		return r, true

	case accessPublic:
		// A public route does not REQUIRE a session, but it resolves one that
		// is offered. Two routes want that:
		//
		//   - /health reduces its body for an anonymous caller and returns the
		//     full report to an authenticated one, so it has to know which it
		//     is talking to.
		//   - /auth/login records an audit actor when somebody already signed
		//     in logs in again.
		//
		// Best effort by definition: an absent, expired, or invalid session is
		// simply no identity, never an error. A public route that failed on a
		// bad cookie would be a public route an attacker could take down by
		// setting one.
		return s.resolveOptionalIdentity(r), true
	}

	// Everything else needs a session, and an unclaimed installation has no
	// accounts to hold one. Saying "bootstrap required" is not a disclosure:
	// the bootstrap status endpoint is public precisely so the UI can ask.
	if !claimed {
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeBootstrapRequired,
			"this installation has no administrator yet")
		return nil, false
	}

	// ---- authentication ----------------------------------------------------

	token := readSessionToken(r)
	identity, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		if !errors.Is(err, service.ErrNoSession) {
			s.logger.ErrorContext(r.Context(), "could not evaluate a session",
				slog.String("error", err.Error()))
			writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
			return nil, false
		}
		// The cookie is cleared so a browser holding a dead token stops sending
		// it, which turns a permanent 401 loop into one redirect to the login
		// page.
		s.clearSessionCookie(w, r)
		writeError(w, r, s.logger, http.StatusUnauthorized, CodeUnauthenticated,
			"authentication is required")
		return nil, false
	}

	r = r.WithContext(withIdentity(r.Context(), identity))

	// ---- forced password change --------------------------------------------

	if identity.User.MustChangePassword && !access.allowPasswordChange {
		writeError(w, r, s.logger, http.StatusForbidden, CodePasswordChangeRequired,
			"this account must set a new password before doing anything else")
		return nil, false
	}

	// ---- authorization -----------------------------------------------------

	if access.kind == accessPermission && !identity.User.Can(access.permission) {
		s.recordDenied(r, identity, access.permission)
		// 403 rather than 404. The endpoint's existence is not a secret -- the
		// OpenAPI document is published -- and a 404 here would make an
		// operator debug a routing problem they do not have. Endpoints where
		// existence IS sensitive answer 404 from inside the handler instead.
		writeError(w, r, s.logger, http.StatusForbidden, CodeForbidden,
			"your role does not permit this")
		return nil, false
	}

	// ---- write protections -------------------------------------------------

	// A bare entry is on its way to a 405; see routeAccess.methodNotAllowed.
	//
	// This is the ONLY place the write protections run for an authenticated
	// route. A handler that repeated them would charge the rate limiter twice
	// for one request, which is how a limit of N becomes a limit of N/2.
	if stateChanging(r.Method) && !access.methodNotAllowed {
		guard := s.guardWrite
		if access.policyRate {
			guard = s.guardPolicyWrite
		}
		if err := guard(r); err != nil {
			s.writeGuardFailure(w, r, err)
			return nil, false
		}
		if !access.skipCSRF && !s.checkCSRF(w, r, identity) {
			return nil, false
		}
	}

	return r, true
}

// resolveOptionalIdentity attaches an identity when one is offered.
//
// Never fails and never writes a response. A public route with a broken cookie
// serves anonymously, which is the only behaviour that keeps a public route
// public.
func (s *Server) resolveOptionalIdentity(r *http.Request) *http.Request {
	token := readSessionToken(r)
	if token == "" {
		return r
	}
	identity, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		return r
	}
	return r.WithContext(withIdentity(r.Context(), identity))
}

// guardBootstrapWrite applies the write protections to the bootstrap routes.
//
// They have no session and therefore no CSRF token, so they lean on the other
// three controls: the Fetch Metadata and Origin checks, the JSON content type
// (which a cross-origin form cannot set without a preflight that fails), and
// the rate limiter. Claiming an installation additionally needs the one-time
// token, which no cross-site page can read.
func (s *Server) guardBootstrapWrite(w http.ResponseWriter, r *http.Request) bool {
	if !stateChanging(r.Method) {
		return true
	}
	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return false
	}
	return true
}

// stateChanging reports whether a method may change state.
func stateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// checkCSRF validates the per-session token on a state-changing request.
//
// # Constant-time, and derived rather than stored
//
// The expected token is HMAC(installation key, purpose, raw session token). The
// server has the raw token on every request -- it arrived in the cookie -- so
// the expected value is recomputed rather than looked up, and there is nothing
// at rest for a database thief to read. Comparison is constant-time so a
// partially correct token does not leak how far it got.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request, identity service.Authenticated) bool {
	presented := strings.TrimSpace(r.Header.Get(CSRFHeader))
	if presented == "" || !s.auth.ValidCSRF(identity.Token, presented) {
		s.recordCSRFRejection(r, identity)
		writeError(w, r, s.logger, http.StatusForbidden, CodeCSRF,
			"this request needs a valid CSRF token")
		return false
	}
	return true
}

// installationClaimed reports whether an administrator exists.
//
// Cached for a short interval rather than read per request: it changes exactly
// once in an installation's life, and a database round trip on every request to
// re-learn a value that will never change again is a poor trade. The cache is
// one-way -- it only ever flips false to true -- so a stale read can delay
// noticing the bootstrap by at most the interval and can never re-open it.
func (s *Server) installationClaimed(r *http.Request) bool {
	if s.claimed.Load() {
		return true
	}

	status, err := s.auth.BootstrapStatus(r.Context())
	if err != nil {
		// Fail CLOSED. If HarborMaster cannot tell whether it has been claimed,
		// treating it as claimed means requests need a session -- which is the
		// safe direction. Treating it as unclaimed would expose the bootstrap
		// flow on a live installation.
		s.logger.ErrorContext(r.Context(), "could not read the bootstrap state",
			slog.String("error", err.Error()))
		return true
	}
	if status.Completed {
		s.claimed.Store(true)
	}
	return status.Completed
}

// recordDenied audits an authorization failure.
//
// Every denial is recorded. A single 403 is a mistake; a hundred is somebody
// looking for a way in, and the pattern is only visible if all of them are
// written down.
func (s *Server) recordDenied(r *http.Request, identity service.Authenticated, permission domain.Permission) {
	if s.audit == nil {
		return
	}
	s.audit.Record(r.Context(), domain.AuditEvent{
		Action:  domain.AuditAuthorizationDenied,
		Outcome: domain.AuditDenied,

		ActorUserID:    identity.User.UserID,
		ActorUsername:  identity.User.Username,
		ActorRole:      identity.User.Role,
		ActorSessionID: identity.Session.SessionID,

		TargetType: domain.AuditTargetSystem,
		// The PERMISSION, not the path. A permission is a closed vocabulary; a
		// path is request-derived text, and this value reaches a page an
		// administrator reads.
		TargetID: string(permission),

		RequestID:  RequestIDFrom(r.Context()),
		ClientAddr: s.clientAddr(r),
		Reason:     "the role does not hold this permission",
	})
}

// recordCSRFRejection audits a rejected state-changing request.
func (s *Server) recordCSRFRejection(r *http.Request, identity service.Authenticated) {
	if s.audit == nil {
		return
	}
	s.audit.Record(r.Context(), domain.AuditEvent{
		Action:  domain.AuditCSRFRejected,
		Outcome: domain.AuditDenied,

		ActorUserID:    identity.User.UserID,
		ActorUsername:  identity.User.Username,
		ActorRole:      identity.User.Role,
		ActorSessionID: identity.Session.SessionID,

		TargetType: domain.AuditTargetSystem,
		RequestID:  RequestIDFrom(r.Context()),
		ClientAddr: s.clientAddr(r),
		Reason:     "the CSRF token was missing or did not match",
	})
}

// ----------------------------------------------------------------- cookies --

// setSessionCookie writes the session cookie.
//
// Every attribute here is load-bearing:
//
//   - HttpOnly stops JavaScript reading the token, so an XSS cannot exfiltrate
//     a session for later use off-box.
//   - SameSite=Strict stops the browser attaching it to any cross-site request,
//     which is the primary CSRF defence; the token header is the second.
//   - Secure marks it HTTPS-only. Set from the connection rather than
//     unconditionally, because a Secure cookie on a plain-HTTP deployment is
//     one the browser silently never sends.
//   - Path=/ is required by the __Host- prefix and is correct anyway: the SPA
//     and the API share an origin.
//   - MaxAge matches the session's absolute expiry, so a browser discards it at
//     the same moment the server would refuse it.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, issued service.IssuedSession) {
	secure := s.requestIsSecure(r)

	sameSite := http.SameSiteStrictMode
	if s.authCfg.CookieSameSiteLax {
		sameSite = http.SameSiteLaxMode
	}

	maxAge := int(issued.ExpiresAt.Sub(s.now()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}

	// `secure` is false in exactly one case: a request that arrived over
	// LOOPBACK on a deployment that has not configured TLS. Every other path --
	// TLS terminated here, a trusted proxy reporting HTTPS, the explicit
	// setting, or a peer that is not loopback -- yields true, and an
	// unidentifiable peer yields true as well.
	//
	// Plain HTTP on loopback is a supported mode: the traffic never leaves the
	// machine, and forcing Secure would make older browsers refuse to send the
	// cookie back over http://127.0.0.1, which breaks sign-in for the default
	// standalone deployment. The suppression is this one line and this one
	// case; the off-box case is what the audit closed.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(secure),
		Value:    issued.Token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})

	// Rotation on login can move a session between the two cookie names -- a
	// deployment that gained TLS, for instance. The other name is cleared so a
	// stale token is not left in the browser to be presented later.
	s.expireCookie(w, sessionCookieName(!secure), !secure)
}

// clearSessionCookie removes both cookie names.
//
// Both, unconditionally. A deployment that gained or lost TLS can have left a
// token under the other name, and a token left in a browser is one that can be
// presented later.
//
// # Why each deletion gets the Secure attribute it gets
//
// A cookie's identity is its NAME, DOMAIN, and PATH. The Secure attribute is a
// property of the cookie being written, not part of what it matches, so a
// Secure deletion still removes a non-Secure cookie of the same name. That is
// what lets both of these follow the safer rule rather than the one that
// happens to match what is already in the browser.
//
//   - `__Host-` REQUIRES Secure by definition, so its deletion is always
//     Secure. On a plain-HTTP origin the browser rejects it, which costs
//     nothing: a `__Host-` cookie could never have been set there either.
//   - The plain name's deletion follows the CONNECTION. Over HTTPS that makes
//     the deletion itself Secure, so it cannot be replayed over a downgraded
//     request. Over plain HTTP it must not be, because a browser rejects a
//     Secure cookie set from an insecure origin — the deletion would be
//     silently dropped and the dead token would stay in the browser, which is
//     the opposite of what signing out is for.
//
// The second case is the only one that ever writes a cookie without Secure, and
// it writes an EMPTY value with a negative Max-Age. There is nothing in it to
// protect in transit; its whole purpose is to delete the one that did carry a
// token.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	s.expireCookie(w, SecureSessionCookieName, true)
	s.expireCookie(w, SessionCookieName, s.requestIsSecure(r))
}

// expireCookie writes a cookie that a browser deletes immediately.
//
// The `secure` argument is the caller's decision; see clearSessionCookie for
// how each of the two names arrives at one.
func (s *Server) expireCookie(w http.ResponseWriter, name string, secure bool) {
	sameSite := http.SameSiteStrictMode
	if s.authCfg.CookieSameSiteLax {
		sameSite = http.SameSiteLaxMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
}
