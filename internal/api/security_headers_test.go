package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The security headers are pinned by test rather than merely set in code.
//
// A response header is the kind of thing that gets weakened by accident: one
// 'unsafe-inline' added to make a component work, one header dropped in a
// refactor, and nothing fails until someone runs a scanner months later. These
// tests make the policy a contract.

func TestSecurityHeadersArePresentOnEveryResponse(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	// A JSON route, an SPA route, and an error route: the middleware wraps the
	// whole mux, and all three must carry the headers.
	for _, target := range []string{"/api/v1/health", "/", "/api/v1/does-not-exist"} {
		t.Run(target, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, target, nil)

			for header, want := range map[string]string{
				"X-Content-Type-Options":       "nosniff",
				"X-Frame-Options":              "DENY",
				"Referrer-Policy":              "no-referrer",
				"Cross-Origin-Opener-Policy":   "same-origin",
				"Cross-Origin-Resource-Policy": "same-origin",
				"Content-Security-Policy":      contentSecurityPolicy,
			} {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}

			if rec.Header().Get("Permissions-Policy") == "" {
				t.Error("Permissions-Policy is missing")
			}
		})
	}
}

// The whole point of the tightened policy.
//
// If a future change adds an inline style or an inline script, the right fix is
// a nonce or a hash -- not reopening the policy. Failing here is the prompt to
// have that conversation.
func TestContentSecurityPolicyForbidsInlineAndRemoteContent(t *testing.T) {
	if strings.Contains(contentSecurityPolicy, "unsafe-inline") {
		t.Error("CSP allows 'unsafe-inline'; use a nonce or hash instead of reopening the policy")
	}
	if strings.Contains(contentSecurityPolicy, "unsafe-eval") {
		t.Error("CSP allows 'unsafe-eval'")
	}
	if strings.Contains(contentSecurityPolicy, "*") {
		t.Error("CSP contains a wildcard source")
	}

	// Every directive that must be present and locked to 'self' or 'none'.
	for _, directive := range []string{
		"default-src 'self'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"connect-src 'self'",
	} {
		if !strings.Contains(contentSecurityPolicy, directive) {
			t.Errorf("CSP is missing %q", directive)
		}
	}
}

// A client-supplied request ID must never be echoed: it is attacker controlled
// and would let a caller forge log correlation.
func TestRequestIDIsServerGeneratedNotEchoed(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	const forged = "forged-request-id-aaaaaaaa"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set(RequestIDHeader, forged)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	got := rec.Header().Get(RequestIDHeader)
	if got == forged {
		t.Error("the server echoed a client-supplied request ID")
	}
	if got == "" {
		t.Error("no request ID was assigned")
	}
	// 12 random bytes, hex encoded.
	if len(got) != 24 {
		t.Errorf("request ID = %q (%d chars), want 24 hex characters", got, len(got))
	}
}

// An error response must never carry internal detail, whatever the route.
func TestErrorResponsesCarryNoInternalDetail(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	rec := do(t, srv, http.MethodGet, "/api/v1/does-not-exist", nil)
	body := rec.Body.String()

	for _, leak := range []string{
		"goroutine", ".go:", "/src/", "harbormaster.db",
		"docker.sock", "internal/api", "sql:",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("error response leaked %q: %s", leak, body)
		}
	}
}

// nosniff is only meaningful if the type is actually correct.
func TestJSONResponsesDeclareTheirContentType(t *testing.T) {
	srv := newTestServer(t, &fakeHealth{})

	rec := do(t, srv, http.MethodGet, "/api/v1/health", nil)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
