package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Registry client protocol tests.
//
// # Why these use an in-package double
//
// The guarded transport refuses loopback, which is exactly what
// TestTheRealClientCannotReachLoopback asserts. Testing the PROTOCOL therefore
// needs the dialler out of the way, so these tests substitute the client's
// unexported http field -- something no caller outside this package can do.
//
// The two halves are complementary and neither substitutes for the other:
// transport_test.go proves the guards hold against a real connection, and this
// file proves the request construction, status mapping, parsing, and bounds are
// right.

// testRegistry is a stub OCI registry over TLS.
//
// TLS because the client only ever builds https URLs; the server's own client
// trusts its certificate, so this exercises the real URL construction rather
// than a plaintext shortcut.
type testRegistry struct {
	*httptest.Server

	// requests records every path and query the client asked for, which is how
	// the pagination and conditional-request tests assert what was sent.
	mu       sync.Mutex
	requests []string
	headers  []http.Header
	// tokenRequests counts token negotiations.
	tokenRequests atomic.Int32
}

func (r *testRegistry) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.URL.RequestURI())
	r.headers = append(r.headers, req.Header.Clone())
}

func (r *testRegistry) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func (r *testRegistry) lastHeader(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.headers) == 0 {
		return ""
	}
	return r.headers[len(r.headers)-1].Get(key)
}

// testRegistryHost is the name the stub answers to.
//
// A name rather than the stub's real 127.0.0.1 address, and that matters: the
// token realm is validated with domain.ContactableRegistryHost, which refuses
// address literals and ports. Using a plausible public hostname means the token
// tests exercise the REAL realm validation rather than working around it, while
// the transport below quietly sends the connection to the stub.
const testRegistryHost = "registry.example.test"

// testAuthHost is the name the stub's token endpoint answers to.
const testAuthHost = "auth.example.test"

// newTestRegistry starts a stub registry and returns a client wired to it.
//
// The client's unexported http field is replaced -- something no caller outside
// this package can do, which is why the protocol tests live here. The
// replacement transport resolves every host to the stub and skips certificate
// verification, because the stub's certificate names an address rather than
// testRegistryHost.
//
// Neither relaxation touches production code. The guarded transport's own
// behaviour is asserted separately in transport_test.go, including that it
// refuses this very server when reached through the real dialler.
func newTestRegistry(t *testing.T, handler http.HandlerFunc) (*testRegistry, *Client, domain.NormalizedRef) {
	t.Helper()

	stub := &testRegistry{}
	stub.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		if r.URL.Path == "/token" {
			stub.tokenRequests.Add(1)
		}
		handler(w, r)
	}))
	t.Cleanup(stub.Close)

	parsed, err := url.Parse(stub.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	stubAddress := parsed.Host

	client := New(Options{
		Version:        "test",
		RequestTimeout: 5 * time.Second,
		MaxAttempts:    2,
		RetryBackoff:   time.Millisecond,
		Now:            func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})

	client.http = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				// Every destination resolves to the stub, whatever host the
				// client built into the URL.
				return (&net.Dialer{}).DialContext(ctx, network, stubAddress)
			},
			// Test-only: the stub's certificate names 127.0.0.1, not the
			// hostname the client is asked to reach. Production has no setting
			// that can do this -- see TestTLSIsRequiredAndVerified.
			TLSClientConfig: &tls.Config{
				//nolint:gosec // a test double's self-signed certificate; production verifies.
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			},
		},
		CheckRedirect: refuseRedirect,
	}

	ref := domain.NormalizedRef{
		Raw:       "example/app:1.0.0",
		Canonical: testRegistryHost + "/example/app:1.0.0",
		Familiar:  testRegistryHost + "/example/app:1.0.0",
		Kind:      domain.RegistryOCI,
		Host:      testRegistryHost,
		APIHost:   testRegistryHost,
		Namespace: "example",
		Name:      "app",
		Path:      "example/app",
		Tag:       "1.0.0",
	}
	return stub, client, ref
}

// manifestBody is a minimal OCI image index.
func manifestBody() string {
	return `{
		"schemaVersion": 2,
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"annotations": {
			"org.opencontainers.image.created": "2024-01-15T10:30:00Z",
			"org.opencontainers.image.vendor": "Example Inc",
			"org.opencontainers.image.source": "https://github.com/example/app",
			"org.opencontainers.image.ignored": "should not be kept"
		},
		"manifests": [
			{"mediaType": "application/vnd.oci.image.manifest.v1+json",
			 "digest": "sha256:` + strings.Repeat("a", 64) + `",
			 "platform": {"os": "linux", "architecture": "amd64"}},
			{"mediaType": "application/vnd.oci.image.manifest.v1+json",
			 "digest": "sha256:` + strings.Repeat("b", 64) + `",
			 "platform": {"os": "linux", "architecture": "arm64", "variant": "v8"}},
			{"mediaType": "application/vnd.oci.image.manifest.v1+json",
			 "digest": "sha256:` + strings.Repeat("c", 64) + `",
			 "platform": {"os": "unknown", "architecture": "unknown"}}
		]
	}`
}

// ------------------------------------------------------------- manifests --

func TestManifestResolvesADigest(t *testing.T) {
	body := manifestBody()
	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("ETag", `"abc123"`)
		// A deliberately WRONG content-digest header. The client computes the
		// digest from the bytes, so this must be ignored rather than believed.
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("9", 64))
		_, _ = w.Write([]byte(body))
	})

	result, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	// The digest is COMPUTED, which is both cheaper to trust and strictly more
	// correct than believing a header a registry controls.
	sum := sha256.Sum256([]byte(body))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if result.Digest != want {
		t.Errorf("digest = %q, want the computed %q", result.Digest, want)
	}

	if result.ETag != `"abc123"` {
		t.Errorf("etag = %q", result.ETag)
	}
	if result.MediaType != "application/vnd.oci.image.index.v1+json" {
		t.Errorf("media type = %q", result.MediaType)
	}

	// The attestation entry ("unknown/unknown") is not a platform a container
	// runs on and must be dropped.
	if len(result.Platforms) != 2 {
		t.Fatalf("platforms = %+v, want the two real ones", result.Platforms)
	}
	if result.Platforms[0].String() != "linux/amd64" ||
		result.Platforms[1].String() != "linux/arm64/v8" {
		t.Errorf("platforms = %+v", result.Platforms)
	}

	// Annotations are an ALLOWLIST. The unlisted key must not be stored.
	if result.Annotations["vendor"] != "Example Inc" {
		t.Errorf("vendor = %q", result.Annotations["vendor"])
	}
	if result.Annotations["source"] != "https://github.com/example/app" {
		t.Errorf("source = %q", result.Annotations["source"])
	}
	for key, value := range result.Annotations {
		if strings.Contains(value, "should not be kept") {
			t.Errorf("an unlisted annotation %q was kept", key)
		}
	}

	// The request itself: correct path, correct Accept, correct User-Agent.
	paths := stub.paths()
	if len(paths) != 1 || paths[0] != "/v2/example/app/manifests/1.0.0" {
		t.Errorf("requested %v", paths)
	}
	if accept := stub.lastHeader("Accept"); !strings.Contains(accept, "index.v1+json") {
		t.Errorf("Accept = %q", accept)
	}
	if agent := stub.lastHeader("User-Agent"); !strings.HasPrefix(agent, "HarborMaster/") {
		t.Errorf("User-Agent = %q", agent)
	}
}

// A digest-pinned reference asks for the digest, not the tag.
func TestManifestUsesTheDigestWhenPinned(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)

	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})
	ref.Digest = digest

	if _, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref}); err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	paths := stub.paths()
	if len(paths) != 1 || paths[0] != "/v2/example/app/manifests/"+digest {
		t.Errorf("requested %v, want the digest reference", paths)
	}
}

// Conditional requests: the cached validator is sent, and a 304 costs no body.
func TestManifestSendsAConditionalRequest(t *testing.T) {
	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"cached"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})

	result, err := client.Manifest(context.Background(),
		ManifestRequest{Ref: ref, ETag: `"cached"`})
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if !result.NotModified {
		t.Fatal("a 304 was not reported as unmodified")
	}
	if result.Digest != "" {
		t.Errorf("a 304 produced a digest %q; there was no body to hash", result.Digest)
	}
	if stub.lastHeader("If-None-Match") != `"cached"` {
		t.Error("the cached validator was not sent")
	}
}

// A stored ETag is validated on the way OUT. A header assembled from a stored
// string is a header-injection gap if the string was never constrained -- a
// restored backup or a hand-edited row would be enough.
func TestAMalformedCachedETagIsNotSent(t *testing.T) {
	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})

	if _, err := client.Manifest(context.Background(), ManifestRequest{
		Ref:  ref,
		ETag: "bad\r\nX-Injected: yes",
	}); err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got := stub.lastHeader("If-None-Match"); got != "" {
		t.Errorf("If-None-Match = %q, want it withheld", got)
	}
	if stub.lastHeader("X-Injected") != "" {
		t.Error("a header was injected through the cached validator")
	}
}

// ------------------------------------------------------------- responses --

// A registry is a third party. Every one of these must produce a HarborMaster
// status, never a crash and never the registry's own words.
func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantStatus domain.CheckStatus
	}{
		{"not found", http.StatusNotFound, `{"errors":[{"message":"MANIFEST_UNKNOWN"}]}`,
			ErrNotFound, domain.CheckNotFound},
		{"forbidden", http.StatusForbidden, `{"errors":[{"message":"DENIED"}]}`,
			ErrUnauthorized, domain.CheckUnauthorized},
		{"rate limited", http.StatusTooManyRequests, `too many requests`,
			ErrRateLimited, domain.CheckRateLimited},
		{"server error", http.StatusInternalServerError, `oops`,
			ErrTransient, domain.CheckFailed},
		{"bad gateway", http.StatusBadGateway, ``, ErrTransient, domain.CheckFailed},
		{"teapot", http.StatusTeapot, ``, ErrPermanent, domain.CheckFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			_, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
			if err == nil {
				t.Fatalf("status %d produced no error", tc.status)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}

			// THE PROPERTY THAT MATTERS: the registry's own text never travels.
			status, detail := Classify(err)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			for _, leaked := range []string{"MANIFEST_UNKNOWN", "DENIED", "oops", "too many requests"} {
				if strings.Contains(detail, leaked) || strings.Contains(err.Error(), leaked) {
					t.Errorf("the registry's text leaked into %q / %q", detail, err.Error())
				}
			}
		})
	}
}

// A registry's Retry-After is honoured, and bounded. A client that ignores it
// earns a longer ban; a client that believes an absurd one removes an image
// from coverage indefinitely.
func TestRateLimitRetryAfterIsHonouredAndBounded(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
		set    bool
	}{
		{"60", time.Minute, true},
		{"0", 0, true},
		{"999999999", maxRetryAfter, true},
		{"-5", 0, false},
		{"not-a-number", 0, false},
		{"", 0, false},
	}

	for _, tc := range cases {
		t.Run("retry-after "+tc.header, func(t *testing.T) {
			_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			})

			_, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("error = %v, want ErrRateLimited", err)
			}

			after := RetryAfterFor(err)
			if after.Set != tc.set {
				t.Errorf("set = %v, want %v", after.Set, tc.set)
			}
			if after.Wait != tc.want {
				t.Errorf("wait = %s, want %s", after.Wait, tc.want)
			}
		})
	}
}

// Malformed bodies must produce a clean error rather than a panic or a
// half-decoded result.
func TestMalformedResponses(t *testing.T) {
	for _, body := range []string{
		``, `not json`, `{`, `[]`, `null`, `{"manifests": "not-an-array"}`,
		"\x00\x01\x02",
	} {
		t.Run(fmt.Sprintf("%.12q", body), func(t *testing.T) {
			_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})

			result, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
			if err != nil {
				if !errors.Is(err, ErrMalformedResponse) {
					t.Errorf("error = %v, want ErrMalformedResponse", err)
				}
				return
			}
			// Some of these are valid JSON that simply carries nothing. A
			// digest is still computed, which is correct: the bytes are what
			// the registry served.
			if result.Digest == "" {
				t.Error("a successful parse produced no digest")
			}
		})
	}
}

// The response budget is enforced, and EXCEEDING it is detected rather than
// silently truncating a document into something that parses as different
// content.
func TestOversizedResponsesAreRefused(t *testing.T) {
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		// One byte over the manifest budget.
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 1<<20)
		for written := 0; written <= maxManifestBytes; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})

	_, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
	}

	status, detail := Classify(err)
	if status != domain.CheckFailed || detail == "" {
		t.Errorf("classified as %q / %q", status, detail)
	}
}

// A registry that accepts the request and never answers must not hold a worker.
func TestRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	defer close(release)

	client.requestTimeout = 50 * time.Millisecond
	client.maxAttempts = 1

	started := time.Now()
	_, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a hanging registry produced no error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the request took %s; the timeout did not apply", elapsed)
	}

	status, _ := Classify(err)
	if status != domain.CheckFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

// A cancelled context must abort promptly, so shutdown is not held by a
// registry.
func TestContextCancellationAborts(t *testing.T) {
	release := make(chan struct{})
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	if _, err := client.Manifest(ctx, ManifestRequest{Ref: ref}); err == nil {
		t.Fatal("a cancelled request produced no error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("cancellation took %s", elapsed)
	}
}

// ----------------------------------------------------------------- retry --

// Only TRANSIENT failures are retried. Retrying a 404 wastes a request;
// retrying a rate limit is precisely what the registry asked the client not to
// do.
func TestOnlyTransientFailuresAreRetried(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantAttempts int
	}{
		{"a server error is retried", http.StatusInternalServerError, 2},
		{"a not-found is not", http.StatusNotFound, 1},
		{"an unauthorized is not", http.StatusUnauthorized, 1},
		{"a rate limit is not", http.StatusTooManyRequests, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tc.status)
			})

			_, _ = client.Manifest(context.Background(), ManifestRequest{Ref: ref})

			if got := int(attempts.Load()); got != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tc.wantAttempts)
			}
		})
	}
}

// A transient failure that resolves must succeed on the retry.
func TestATransientFailureRecovers(t *testing.T) {
	var attempts atomic.Int32
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})

	result, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if result.Digest == "" {
		t.Error("the retry produced no digest")
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
}

// ---------------------------------------------------------------- tokens --

// The bearer-token flow: a 401 with a challenge is negotiated once and retried.
func TestAnonymousTokenNegotiation(t *testing.T) {
	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"secret-token-value","expires_in":300}`))
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="https://`+testAuthHost+`/token",service="example.test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})

	if _, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref}); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if stub.tokenRequests.Load() != 1 {
		t.Errorf("token requests = %d, want 1", stub.tokenRequests.Load())
	}

	// The token is cached, so a second lookup negotiates nothing.
	if _, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref}); err != nil {
		t.Fatalf("second Manifest: %v", err)
	}
	if stub.tokenRequests.Load() != 1 {
		t.Errorf("token requests = %d after two lookups, want the cached 1",
			stub.tokenRequests.Load())
	}

	// The scope HarborMaster asks for is PULL ONLY. It must never hold a token
	// that could push or delete.
	for _, path := range stub.paths() {
		if !strings.HasPrefix(path, "/token") {
			continue
		}
		if !strings.Contains(path, "scope=repository") || !strings.Contains(path, "pull") {
			t.Errorf("token scope = %q, want a pull-only repository scope", path)
		}
		if strings.Contains(path, "push") || strings.Contains(path, "delete") {
			t.Errorf("token scope = %q, want no write rights", path)
		}
	}
}

// THE SECOND SSRF SURFACE. The token realm is the one URL in this package that
// comes from a registry response, so it clears exactly the same bar as an image
// reference.
func TestTokenRealmIsValidated(t *testing.T) {
	cases := []struct {
		name  string
		realm string
	}{
		{"plaintext", "http://auth.example.com/token"},
		{"loopback", "https://127.0.0.1/token"},
		{"the metadata endpoint", "https://169.254.169.254/token"},
		{"localhost", "https://localhost/token"},
		{"a single-label internal name", "https://auth/token"},
		{"a port", "https://auth.example.com:8443/token"},
		{"userinfo", "https://user:pass@auth.example.com/token"},
		{"a file scheme", "file:///etc/passwd"},
		{"not a url", "://"},
		{"empty", ""},
		{"oversized", "https://auth.example.com/" + strings.Repeat("a", 600)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached atomic.Bool
			_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/token") {
					reached.Store(true)
					_, _ = w.Write([]byte(`{"token":"t"}`))
					return
				}
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="%s",service="example.test"`, tc.realm))
				w.WriteHeader(http.StatusUnauthorized)
			})

			_, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref})
			if err == nil {
				t.Fatalf("realm %q was accepted", tc.realm)
			}
			if reached.Load() {
				t.Errorf("realm %q produced a token request", tc.realm)
			}
		})
	}
}

// A realm's own query and fragment are discarded and replaced with the
// parameters HarborMaster chose. A registry must not be able to decide what its
// client asks for.
func TestTokenRealmQueryIsReplaced(t *testing.T) {
	realm, err := url.Parse("https://auth.example.com/token?service=evil&scope=repository:x:push&extra=1#frag")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	built, err := tokenURL(challenge{realm: realm.String(), service: "good.example.com"},
		"repository:example/app:pull")
	if err != nil {
		t.Fatalf("tokenURL: %v", err)
	}

	query := built.Query()
	if query.Get("service") != "good.example.com" {
		t.Errorf("service = %q, want the challenge's own", query.Get("service"))
	}
	if query.Get("scope") != "repository:example/app:pull" {
		t.Errorf("scope = %q, want HarborMaster's pull-only scope", query.Get("scope"))
	}
	if query.Get("extra") != "" {
		t.Error("a realm-supplied query parameter survived")
	}
	if built.Fragment != "" {
		t.Error("a realm-supplied fragment survived")
	}
	if built.Scheme != "https" {
		t.Errorf("scheme = %q", built.Scheme)
	}
}

// A token that a registry claims lasts a year is not believed. A long-lived
// bearer credential in memory is not something to accept on a server's say-so.
func TestTokenLifetimeIsBounded(t *testing.T) {
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t","expires_in":31536000}`))
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="https://`+testAuthHost+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})

	if _, err := client.Manifest(context.Background(), ManifestRequest{Ref: ref}); err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	var found bool
	client.tokens.Range(func(_, value any) bool {
		token, ok := value.(cachedToken)
		if !ok {
			return true
		}
		found = true
		if lifetime := token.expiresAt.Sub(client.now()); lifetime > time.Hour {
			t.Errorf("cached lifetime = %s, want it clamped to an hour or less", lifetime)
		}
		return true
	})
	if !found {
		t.Error("no token was cached")
	}
}

// ------------------------------------------------------------------ tags --

// Pagination walks with the specification's own `last` cursor, built from the
// last tag HarborMaster itself received.
//
// A FULL page is what continues the walk, so the fixture returns exactly the
// provider's page size on each page but the last -- a short page is how the
// client knows the listing is exhausted.
func TestTagsPaginate(t *testing.T) {
	pageSize := ociProvider{}.TagPageSize()

	// Two full pages then a short one, so the walk takes three requests.
	page := func(start int, count int) string {
		tags := make([]string, 0, count)
		for index := 0; index < count; index++ {
			tags = append(tags, fmt.Sprintf("1.%d.0", start+index))
		}
		return `{"name":"example/app","tags":["` + strings.Join(tags, `","`) + `"]}`
	}

	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("last") {
		case "":
			_, _ = w.Write([]byte(page(0, pageSize)))
		case fmt.Sprintf("1.%d.0", pageSize-1):
			_, _ = w.Write([]byte(page(pageSize, pageSize)))
		default:
			_, _ = w.Write([]byte(page(2*pageSize, 3)))
		}
	})

	result, err := client.Tags(context.Background(), ref, 5)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(result.Tags) != 2*pageSize+3 {
		t.Errorf("tags = %d, want %d across three pages", len(result.Tags), 2*pageSize+3)
	}
	if result.Truncated {
		t.Error("a complete listing reported as truncated")
	}

	paths := stub.paths()
	if len(paths) != 3 {
		t.Fatalf("requested %d pages, want 3: %v", len(paths), paths)
	}
	// The first request carries no cursor; each subsequent one carries the last
	// tag the CLIENT received, never a URL the registry supplied.
	if strings.Contains(paths[0], "last=") {
		t.Errorf("the first page carried a cursor: %q", paths[0])
	}
	if !strings.Contains(paths[1], fmt.Sprintf("last=1.%d.0", pageSize-1)) {
		t.Errorf("the second page's cursor = %q", paths[1])
	}
	for _, path := range paths {
		if !strings.Contains(path, "/v2/example/app/tags/list") || !strings.Contains(path, "n=") {
			t.Errorf("malformed tag request: %q", path)
		}
	}
}

// A listing that hits its page budget must report TRUNCATED, so the caller
// answers "cannot determine" rather than "no update available".
func TestTagsReportTruncationAtThePageBudget(t *testing.T) {
	pageSize := ociProvider{}.TagPageSize()

	// Always a full page, so the walk never ends on its own.
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if last := r.URL.Query().Get("last"); last != "" {
			start = 1000
		}
		tags := make([]string, 0, pageSize)
		for index := 0; index < pageSize; index++ {
			tags = append(tags, fmt.Sprintf("1.%d.0", start+index))
		}
		_, _ = w.Write([]byte(`{"name":"example/app","tags":["` + strings.Join(tags, `","`) + `"]}`))
	})

	result, err := client.Tags(context.Background(), ref, 2)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if !result.Truncated {
		t.Error("a listing that hit its page budget did not report as truncated")
	}
}

// A Link header is a registry-supplied URL, which this package refuses to
// accept. Pagination uses the specification's own `last` cursor instead.
func TestTagsIgnoreTheLinkHeader(t *testing.T) {
	stub, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://169.254.169.254/v2/x/tags/list>; rel="next"`)
		_, _ = w.Write([]byte(`{"name":"example/app","tags":["1.0.0"]}`))
	})

	if _, err := client.Tags(context.Background(), ref, 3); err != nil {
		t.Fatalf("Tags: %v", err)
	}

	for _, path := range stub.paths() {
		if strings.Contains(path, "169.254") {
			t.Errorf("the Link header was followed: %q", path)
		}
	}
}

// Tags come FROM a registry, so they are untrusted input. Malformed ones are
// discarded rather than stored or displayed.
func TestMalformedTagsAreDiscarded(t *testing.T) {
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"example/app","tags":[
			"1.0.0", "", "has space", "has/slash", "bad\nnewline",
			".leading-dot", "` + strings.Repeat("x", 200) + `", "1.1.0"
		]}`))
	})

	result, err := client.Tags(context.Background(), ref, 1)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(result.Tags) != 2 {
		t.Fatalf("tags = %v, want only the two well-formed ones", result.Tags)
	}
	for _, tag := range result.Tags {
		if !domain.ValidImageTag(tag) {
			t.Errorf("a malformed tag %q survived", tag)
		}
	}
}

// A registry that does not implement tag listing must be reported as such, so
// the caller falls back to digest comparison rather than recording the image as
// missing.
func TestTagListingUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})

		_, err := client.Tags(context.Background(), ref, 1)
		if !errors.Is(err, ErrTagListingUnsupported) {
			t.Errorf("status %d produced %v, want ErrTagListingUnsupported", status, err)
		}

		// Reported as OK with an explanatory detail, not as a failure: the
		// digest comparison still worked.
		checkStatus, detail := Classify(err)
		if checkStatus != domain.CheckOK || detail == "" {
			t.Errorf("classified as %q / %q", checkStatus, detail)
		}
	}
}

// ------------------------------------------------------------ concurrency --

// The client is used from a bounded worker pool, so it must be safe for
// concurrent use and must not corrupt its token cache under contention.
func TestConcurrentLookupsAreSafe(t *testing.T) {
	_, client, ref := newTestRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"t","expires_in":300}`))
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="https://`+testAuthHost+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})

	var wait sync.WaitGroup
	errs := make([]error, 24)
	for index := range errs {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			local := ref
			local.Tag = fmt.Sprintf("1.0.%d", slot)
			_, errs[slot] = client.Manifest(context.Background(), ManifestRequest{Ref: local})
		}(index)
	}
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Errorf("lookup %d failed: %v", index, err)
		}
	}
}

// The token cache is bounded, so a large estate cannot grow it without limit.
func TestTokenCacheIsBounded(t *testing.T) {
	client := New(Options{Now: func() time.Time { return time.Unix(0, 0).UTC() }})

	for index := 0; index < maxCachedTokens+50; index++ {
		client.storeToken(fmt.Sprintf("key-%d", index),
			cachedToken{value: "t", expiresAt: time.Unix(1, 0)})
	}

	count := 0
	client.tokens.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count > maxCachedTokens {
		t.Errorf("cached tokens = %d, want at most %d", count, maxCachedTokens)
	}
}

// ------------------------------------------------------------- providers --

// Adding a registry is adding a Provider and registering it. The resolution
// order matters, because the generic provider matches everything.
func TestProviderResolution(t *testing.T) {
	cases := []struct {
		host string
		want domain.RegistryKind
	}{
		{domain.DockerHubAPIHost, domain.RegistryDockerHub},
		{domain.GHCRHost, domain.RegistryGHCR},
		{"quay.io", domain.RegistryOCI},
		{"registry.example.com", domain.RegistryOCI},
		{"", domain.RegistryOCI},
		// Case is normalised, so one registry is not two providers.
		{strings.ToUpper(domain.GHCRHost), domain.RegistryGHCR},
	}

	for _, tc := range cases {
		if got := ProviderKind(tc.host); got != tc.want {
			t.Errorf("ProviderKind(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}

	// Every provider must ask for a pull-only scope. A client that could push
	// is a client that could be made to.
	for _, provider := range providers {
		scope := provider.Scope("example/app")
		if !strings.HasSuffix(scope, ":pull") {
			t.Errorf("%s scope = %q, want pull-only", provider.Kind(), scope)
		}
		if provider.TagPageSize() < 1 {
			t.Errorf("%s has a non-positive tag page size", provider.Kind())
		}
	}
}

// A reference with no host or no path must never produce a URL.
func TestURLBuildingRefusesAnIncompleteReference(t *testing.T) {
	for _, ref := range []domain.NormalizedRef{
		{APIHost: "", Path: "example/app"},
		{APIHost: "registry.example.com", Path: ""},
		{},
	} {
		if _, err := manifestURL(ref, "1.0.0"); err == nil {
			t.Errorf("manifestURL built a URL from %+v", ref)
		}
		if _, err := tagsURL(ref, 50, ""); err == nil {
			t.Errorf("tagsURL built a URL from %+v", ref)
		}
	}
}
