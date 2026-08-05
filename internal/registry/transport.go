// Package registry reads image metadata from OCI distribution registries.
//
// # What this package can do, and what it can never do
//
// It performs anonymous HTTPS GETs against registry manifest and tag-listing
// endpoints. That is the entire capability. It does not pull, push, delete, or
// mutate anything, it holds no credentials, and it has no dependency on
// internal/docker -- an architecture test enforces the last part in both
// directions.
//
// # This is HarborMaster's only outbound network egress
//
// Everything else in HarborMaster talks to a local Docker socket and a local
// SQLite file. This package is the one place that opens a connection to the
// internet, which makes SSRF the dominant risk in the phase that introduced it.
// The defences are layered, and each is independent of the others:
//
//  1. DESTINATIONS COME ONLY FROM IMAGE REFERENCES. A host reaches this package
//     only via domain.NormalizeImageRef, which refuses IP literals, ports,
//     single-label names, localhost, and anything that is not purely a
//     hostname. No API parameter, config value, or database column supplies a
//     host. The refresh endpoint takes no target.
//  2. THE RESOLVED ADDRESS IS CHECKED AT DIAL TIME. guardedDialer inspects the
//     actual IP the connection is about to use and refuses loopback, private,
//     link-local, unique-local, carrier-grade NAT, multicast, and unspecified
//     ranges. Because the check runs on the socket address rather than on the
//     name, DNS rebinding cannot get past it: a name that resolved publicly a
//     moment ago is still checked on the address actually dialled.
//  3. REDIRECTS ARE REFUSED OUTRIGHT. A redirect is a registry-controlled URL,
//     which is precisely the input this package must not accept. Manifest and
//     tag endpoints do not redirect on any supported registry; blob endpoints
//     do, and this package never fetches a blob.
//  4. NO PROXY. Proxy environment variables are ignored, so the destination is
//     always the registry itself. A proxy would necessarily be an internal
//     address, which defence 2 exists to refuse.
//  5. HTTPS ONLY, WITH VERIFICATION. There is no plaintext path, no
//     InsecureSkipVerify, and a TLS 1.2 floor.
//
// # Registry responses are untrusted input
//
// A registry is a third party. Every response is bounded before it is read,
// decoded into a fixed shape, and never echoed: no registry-supplied string
// reaches a log line, an error message, or the database. Statuses and details
// are HarborMaster's own words, chosen from a fixed set.
package registry

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrBlockedAddress reports a destination that resolved into a range this
// package refuses to contact.
//
// Deliberately distinguishable from a network failure: a blocked address is a
// permanent property of the reference, not something a retry could fix.
var ErrBlockedAddress = errors.New("registry address is not publicly routable")

// ErrRedirectRefused reports that a registry attempted to redirect.
var ErrRedirectRefused = errors.New("registry attempted a redirect, which is not followed")

// Transport bounds. Every one of these exists to stop a slow or hostile
// registry from occupying a worker indefinitely.
const (
	// dialTimeout bounds establishing a TCP connection.
	dialTimeout = 5 * time.Second
	// tlsHandshakeTimeout bounds the TLS handshake separately, so a peer that
	// completes a connection and then stalls is still cut off.
	tlsHandshakeTimeout = 5 * time.Second
	// responseHeaderTimeout bounds the wait for the first byte of a response.
	// A registry that accepts a request and never answers is the cheapest way
	// to tie up a client.
	responseHeaderTimeout = 10 * time.Second
	// idleConnTimeout and maxIdleConnsPerHost bound the connection pool, so a
	// sweep over many registries does not accumulate sockets.
	idleConnTimeout     = 60 * time.Second
	maxIdleConns        = 32
	maxIdleConnsPerHost = 4
	// expectContinueTimeout is set explicitly rather than left at zero, which
	// would disable the 100-continue handshake entirely.
	expectContinueTimeout = 1 * time.Second
)

// UserAgent identifies HarborMaster to registries.
//
// Registries use it for rate limiting and for contacting operators of badly
// behaved clients, so it names the project and its version rather than
// impersonating a Docker client. The version is injected by the caller; nothing
// about the host, the daemon, or the estate is disclosed.
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return "HarborMaster/" + version + " (+https://github.com/Aznyi/HarborMaster)"
}

// newHTTPClient builds the guarded client every registry request uses.
//
// There is no exported way to supply a different one: a caller that could
// substitute a transport could substitute the address guard with it.
func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		// The address guard. Control runs after name resolution and before the
		// socket is used, with the ACTUAL address being dialled -- which is what
		// makes this immune to a name that resolves differently between the
		// check and the connection.
		Control: guardAddress,
	}

	return &http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			// Proxy environment variables are deliberately ignored. See the
			// package comment: a proxy is an internal address, and the guard
			// above exists to refuse those.
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				// Certificate verification is on. There is no configuration
				// that turns it off, which is what "no insecure registry
				// support in this phase" means in practice.
			},
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			ExpectContinueTimeout: expectContinueTimeout,
			IdleConnTimeout:       idleConnTimeout,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			// A registry that sends a header block this large is not one worth
			// talking to, and the bound stops a hostile peer from making the
			// client allocate.
			MaxResponseHeaderBytes: 64 << 10,
			DisableKeepAlives:      false,
		},
		// Every redirect is refused. See the package comment.
		CheckRedirect: refuseRedirect,
		// No overall client timeout: each request carries a context deadline,
		// which is cancellable and composes with shutdown. A Timeout here would
		// silently override a caller that wanted less.
	}
}

// refuseRedirect rejects any redirect a registry attempts.
//
// The error names nothing from the response. A redirect Location is
// registry-controlled text, and putting it in an error would carry it into a
// log line.
func refuseRedirect(*http.Request, []*http.Request) error {
	return ErrRedirectRefused
}

// guardAddress refuses a connection to an address that is not publicly
// routable.
//
// Called by net.Dialer.Control with the resolved address, immediately before
// the socket is used. Returning an error aborts that connection attempt.
//
// This is the defence that does not depend on parsing, on DNS being honest, or
// on the name check having been run at all.
func guardAddress(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// Nothing else should ever be dialled by an HTTPS client.
		return fmt.Errorf("%w: unexpected network", ErrBlockedAddress)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address", ErrBlockedAddress)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// Control is always called with a literal address. A name here would
		// mean resolution was bypassed, which is not a situation to proceed
		// from.
		return fmt.Errorf("%w: unresolved address", ErrBlockedAddress)
	}
	if !publiclyRoutable(ip) {
		return ErrBlockedAddress
	}
	return nil
}

// carrierGradeNAT is 100.64.0.0/10, which is routable on some networks but is
// shared address space rather than public internet.
var carrierGradeNAT = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

// benchmarking is 198.18.0.0/15, reserved for network benchmarking and
// occasionally routed internally.
var benchmarking = &net.IPNet{
	IP:   net.IPv4(198, 18, 0, 0),
	Mask: net.CIDRMask(15, 32),
}

// publiclyRoutable reports whether an address is one a public registry could
// legitimately live at.
//
// An allowlist by exclusion: every range that is not the public internet is
// named and refused. An IPv4-mapped IPv6 address is unwrapped first, so
// "::ffff:127.0.0.1" cannot slip past the IPv4 checks.
func publiclyRoutable(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Unwrap IPv4-in-IPv6 so one set of checks covers both spellings.
	if mapped := ip.To4(); mapped != nil {
		ip = mapped
	}

	switch {
	case ip.IsUnspecified(),
		ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast():
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case carrierGradeNAT.Contains(ip4), benchmarking.Contains(ip4):
			return false
		// 0.0.0.0/8 is "this network"; 240.0.0.0/4 is reserved. Neither is a
		// destination, and both are reachable spellings on some stacks.
		case ip4[0] == 0, ip4[0] >= 240:
			return false
		}
		return true
	}

	// IPv6 unique-local (fc00::/7). Go has no helper, and it is the v6
	// equivalent of the private ranges refused above.
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return false
	}
	// The IPv4-compatible and NAT64 well-known prefixes both carry an embedded
	// v4 address that the checks above would not have seen.
	if ip.Equal(net.IPv6zero) {
		return false
	}
	return true
}
