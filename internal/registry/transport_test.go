package registry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Transport and SSRF tests.
//
// These are the most important tests in the phase. Phase 6 gave HarborMaster
// its first outbound egress, and everything that keeps that egress safe lives in
// transport.go. Each defence is asserted independently, because they are meant
// to be independent: any one of them failing must not be enough.

// The address guard, exhaustively. Every range here is one an SSRF payload
// would aim at, and each is named so a future reader knows why it is refused.
func TestPubliclyRoutableRefusesEveryNonPublicRange(t *testing.T) {
	blocked := []struct {
		name string
		ip   string
	}{
		{"the unspecified address", "0.0.0.0"},
		{"IPv4 loopback", "127.0.0.1"},
		{"another IPv4 loopback", "127.255.255.254"},
		{"RFC1918 ten", "10.0.0.1"},
		{"RFC1918 172.16", "172.16.0.1"},
		{"RFC1918 192.168", "192.168.1.1"},
		{"link-local, which is the cloud metadata endpoint", "169.254.169.254"},
		{"carrier-grade NAT", "100.64.0.1"},
		{"benchmarking", "198.18.0.1"},
		{"this-network", "0.1.2.3"},
		{"reserved class E", "240.0.0.1"},
		{"broadcast", "255.255.255.255"},
		{"IPv4 multicast", "224.0.0.1"},

		{"IPv6 unspecified", "::"},
		{"IPv6 loopback", "::1"},
		{"IPv6 unique-local", "fd00::1"},
		{"IPv6 unique-local lower bound", "fc00::1"},
		{"IPv6 link-local", "fe80::1"},
		{"IPv6 multicast", "ff02::1"},

		// The spelling that catches a naive IPv4-only check.
		{"IPv4-mapped loopback", "::ffff:127.0.0.1"},
		{"IPv4-mapped private", "::ffff:10.0.0.1"},
		{"IPv4-mapped metadata", "::ffff:169.254.169.254"},
	}

	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test fixture %q is not an address", tc.ip)
			}
			if publiclyRoutable(ip) {
				t.Errorf("%s (%s) was accepted as publicly routable", tc.name, tc.ip)
			}
		})
	}

	// The positive control. Without it, a function that returned false for
	// everything would pass every assertion above.
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700::1111"}
	for _, address := range allowed {
		ip := net.ParseIP(address)
		if ip == nil {
			t.Fatalf("test fixture %q is not an address", address)
		}
		if !publiclyRoutable(ip) {
			t.Errorf("%s was refused; the guard rejects everything", address)
		}
	}

	if publiclyRoutable(nil) {
		t.Error("a nil address was accepted")
	}
}

// guardAddress is what net.Dialer.Control calls. It must refuse before a socket
// is used, and must refuse anything it cannot positively vouch for.
func TestGuardAddressRefusals(t *testing.T) {
	cases := []struct {
		name    string
		network string
		address string
		wantErr bool
	}{
		{"a public address", "tcp", "93.184.216.34:443", false},
		{"loopback", "tcp", "127.0.0.1:443", true},
		{"the metadata endpoint", "tcp4", "169.254.169.254:80", true},
		{"IPv6 loopback", "tcp6", "[::1]:443", true},
		{"a unix socket", "unix", "/var/run/docker.sock:0", true},
		{"udp", "udp", "93.184.216.34:443", true},
		{"an unparsable address", "tcp", "not-an-address", true},
		{"an unresolved name", "tcp", "example.com:443", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardAddress(tc.network, tc.address, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("guardAddress(%q, %q) = %v, wantErr %v",
					tc.network, tc.address, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrBlockedAddress) {
				t.Errorf("error = %v, want ErrBlockedAddress", err)
			}
		})
	}
}

// THE END-TO-END SSRF ASSERTION.
//
// A real server is started on loopback and the REAL client -- the one the
// service uses, with no substitution -- is pointed at it. The connection must
// fail at the dialler, which is the defence that does not depend on the name
// check having run.
//
// This is also why the transport is not substitutable from outside the package:
// a caller that could supply its own would have skipped exactly this.
func TestTheRealClientCannotReachLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the guarded client reached a loopback server")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newHTTPClient()

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("the guarded client connected to loopback")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("error = %v, want ErrBlockedAddress", err)
	}
}

// Redirects are refused outright. A redirect is a registry-controlled URL,
// which is the one input this package must not accept.
func TestRedirectsAreRefused(t *testing.T) {
	// The redirect target is deliberately a plausible SSRF payload: a
	// followed redirect here is the whole attack.
	target := "http://169.254.169.254/latest/meta-data/"

	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// The guard would refuse the loopback dial before the redirect is even
	// reached, so this exercises the redirect POLICY specifically, with the
	// dialler out of the way. Both are asserted; neither substitutes for the
	// other.
	client := &http.Client{CheckRedirect: refuseRedirect}

	request, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, server.URL+"/redirect", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("a redirect was followed")
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("error = %v, want ErrRedirectRefused", err)
	}
	if reached {
		t.Error("the redirect target was contacted")
	}
}

// The transport must carry no proxy. A proxy is necessarily an internal
// address, which the guard exists to refuse -- and honouring HTTP_PROXY would
// hand an operator's environment a way to redirect every registry request.
func TestTheTransportIgnoresProxyConfiguration(t *testing.T) {
	// Set deliberately: the assertion is that the transport does not consult
	// the environment at all.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:9")

	client := newHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Error("the transport consults a proxy; registry requests must go direct")
	}
}

// TLS is required and verified. There is no configuration that turns either
// off, which is what "no insecure registry support in this phase" means.
func TestTLSIsRequiredAndVerified(t *testing.T) {
	client := newHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}

	if transport.TLSClientConfig == nil {
		t.Fatal("no TLS configuration")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("certificate verification is disabled")
	}
	// tls.VersionTLS12 is 0x0303.
	if transport.TLSClientConfig.MinVersion < 0x0303 {
		t.Errorf("MinVersion = %#x, want at least TLS 1.2", transport.TLSClientConfig.MinVersion)
	}

	// Every bound that stops a slow peer from occupying a worker.
	if transport.ResponseHeaderTimeout <= 0 {
		t.Error("no response header timeout")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Error("no TLS handshake timeout")
	}
	if transport.MaxResponseHeaderBytes <= 0 {
		t.Error("no response header size bound")
	}
}

// The User-Agent identifies HarborMaster rather than impersonating a Docker
// client, and discloses nothing about the host or the estate.
func TestUserAgentIdentifiesHarborMaster(t *testing.T) {
	agent := UserAgent("1.2.3")

	if !strings.HasPrefix(agent, "HarborMaster/1.2.3") {
		t.Errorf("user agent = %q, want it to name HarborMaster and its version", agent)
	}
	if strings.Contains(strings.ToLower(agent), "docker/") {
		t.Errorf("user agent = %q, want it not to impersonate a Docker client", agent)
	}

	// A missing version still produces a usable identifier rather than a
	// malformed header.
	if bare := UserAgent(""); !strings.HasPrefix(bare, "HarborMaster/") {
		t.Errorf("user agent with no version = %q", bare)
	}
	for _, agent := range []string{UserAgent("1.2.3"), UserAgent("")} {
		if strings.ContainsAny(agent, "\r\n") {
			t.Errorf("user agent %q carries a control character", agent)
		}
	}
}
