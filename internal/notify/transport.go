// Package notify delivers notifications to operator-configured destinations.
//
// # This package is the second, and larger, outbound egress in HarborMaster
//
// internal/registry sends anonymous GETs to hosts DERIVED FROM IMAGE
// REFERENCES. No API parameter, config value, or database column supplies a
// host there; the refresh endpoint takes no target at all.
//
// This package sends data to a URL SOMEBODY TYPED. That is a server-side
// request forgery primitive by construction, and the defences have to assume it:
//
//  1. HTTPS ONLY, WITH VERIFICATION. No plaintext path, no InsecureSkipVerify,
//     a TLS 1.2 floor. A webhook URL is a bearer credential and a notification
//     names containers; neither travels in the clear.
//  2. THE URL IS VALIDATED BEFORE IT IS STORED. domain.ParseDestinationURL
//     refuses a non-HTTPS scheme, an address literal, embedded credentials,
//     `localhost`, and single-label names, so an operator is told at the moment
//     they type it.
//  3. THE RESOLVED ADDRESS IS CHECKED AT DIAL TIME. This is the defence that
//     actually holds. The guard inspects the IP the socket is about to use, so
//     a name that resolved publicly a moment ago is still checked against the
//     address actually dialled -- DNS rebinding cannot get past it.
//  4. REDIRECTS ARE REFUSED OUTRIGHT. A redirect is a destination-controlled
//     URL, which is precisely the input a guarded client must not accept.
//     Following one would let a public host redirect to 169.254.169.254.
//  5. NO PROXY. Proxy environment variables are ignored, so the destination is
//     always the destination. A proxy is an internal address, which defence 3
//     exists to refuse.
//  6. THE RESPONSE IS BOUNDED AND DISCARDED. HarborMaster reads a status code.
//     The body is read to a small bound so the connection can be reused, and is
//     then thrown away -- it is third-party text, and it reaches no log, no
//     error, and no column.
//
// # The private-address opt-in, and why it exists
//
// HarborMaster's audience runs self-hosted software. A meaningful number of
// them notify a Gotify, an ntfy, or a Home Assistant on their own LAN, and
// refusing every private address outright would make the feature useless to
// them -- so they would not use it, and would learn nothing when an update
// failed at 02:00.
//
// So there is one setting, off by default, that relaxes defence 3 to allow
// private and loopback addresses. It is documented in terms of what it costs:
// with it on, an administrator who can create a destination can make
// HarborMaster issue an HTTPS POST to anything the container can route to.
// Everything else -- HTTPS, no redirects, no proxy, bounded response -- still
// applies, and link-local, multicast, and cloud metadata addresses are refused
// whatever the setting says.
package notify

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Errors this package's transport can produce.
//
// Sentinels rather than formatted strings, so a caller classifies rather than
// matching text, and so no message can accidentally carry the destination.
var (
	// ErrBlockedAddress reports a destination that resolved into a range this
	// package refuses to contact.
	ErrBlockedAddress = errors.New("the notification destination is not publicly routable")
	// ErrRedirectRefused reports that a destination attempted a redirect.
	ErrRedirectRefused = errors.New("the notification destination attempted a redirect, which is not followed")
)

// Transport bounds. Every one exists to stop a slow or hostile destination from
// occupying a delivery worker.
const (
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 10 * time.Second
	expectContinueTimeout = 1 * time.Second

	idleConnTimeout     = 60 * time.Second
	maxIdleConns        = 16
	maxIdleConnsPerHost = 2

	// maxResponseHeaderBytes bounds a hostile peer's header block.
	maxResponseHeaderBytes = 32 << 10
)

// MaxResponseBodyBytes bounds how much of a response is read.
//
// HarborMaster needs the status code. The body is read only so the connection
// can be reused and then discarded, and reading it unbounded would let a
// destination make the process allocate.
const MaxResponseBodyBytes = 8 << 10

// MaxRequestBodyBytes bounds what HarborMaster will send.
//
// A notification is small by construction -- a title, a short body, and at most
// twelve bounded fields. This is a backstop against a channel encoder that
// somehow produced more, checked before the request is made so an oversized
// payload fails locally rather than at somebody else's server.
const MaxRequestBodyBytes = 64 << 10

// AddressPolicy decides which resolved addresses may be contacted.
type AddressPolicy struct {
	// AllowPrivate permits loopback, private, and unique-local addresses.
	//
	// OFF by default. See the package comment: this is the one relaxation, it
	// exists for self-hosted destinations on a LAN, and it does not relax
	// anything else.
	AllowPrivate bool
}

// newHTTPClient builds the guarded client every webhook delivery uses.
//
// There is no exported way to supply a different one. A caller that could
// substitute a transport could substitute the address guard with it, which
// would make every defence above a suggestion.
func newHTTPClient(policy AddressPolicy) *http.Client {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		// The address guard. Runs after name resolution and before the socket
		// is used, with the ACTUAL address being dialled.
		Control: policy.guard,
	}

	return &http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			// Proxy environment variables are deliberately ignored.
			Proxy:                  nil,
			TLSClientConfig:        tlsConfig(),
			ForceAttemptHTTP2:      true,
			TLSHandshakeTimeout:    tlsHandshakeTimeout,
			ResponseHeaderTimeout:  responseHeaderTimeout,
			ExpectContinueTimeout:  expectContinueTimeout,
			IdleConnTimeout:        idleConnTimeout,
			MaxIdleConns:           maxIdleConns,
			MaxIdleConnsPerHost:    maxIdleConnsPerHost,
			MaxResponseHeaderBytes: maxResponseHeaderBytes,
		},
		CheckRedirect: refuseRedirect,
		// No overall client timeout: each delivery carries a context deadline,
		// which is cancellable and composes with shutdown.
	}
}

// refuseRedirect rejects any redirect a destination attempts.
//
// The error names nothing from the response. A redirect Location is
// destination-controlled text, and putting it in an error would carry it into a
// log line and, worse, into the delivery record an operator reads.
func refuseRedirect(*http.Request, []*http.Request) error {
	return ErrRedirectRefused
}

// guard refuses a connection to an address the policy does not permit.
//
// Called by net.Dialer.Control with the resolved address, immediately before
// the socket is used. Returning an error aborts that connection attempt.
//
// This is the defence that does not depend on parsing, on DNS being honest, or
// on the URL check having run at all.
func (p AddressPolicy) guard(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return fmt.Errorf("%w: unexpected network", ErrBlockedAddress)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address", ErrBlockedAddress)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control is always called with a literal. A name here would mean
		// resolution was bypassed, which is not a state to proceed from.
		return fmt.Errorf("%w: unresolved address", ErrBlockedAddress)
	}
	if !p.permits(ip) {
		return ErrBlockedAddress
	}
	return nil
}

// permits reports whether an address may be contacted under this policy.
func (p AddressPolicy) permits(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Unwrap IPv4-in-IPv6 so one set of checks covers both spellings and
	// "::ffff:127.0.0.1" cannot slip past the IPv4 rules.
	if mapped := ip.To4(); mapped != nil {
		ip = mapped
	}

	// ALWAYS refused, whatever the policy says. These are not "internal
	// services an operator might legitimately notify" -- they are the ranges an
	// SSRF is aimed at, and the cloud metadata endpoint is the single most
	// valuable target on any hosted machine.
	switch {
	case ip.IsUnspecified(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast():
		return false
	}
	if cloudMetadata.Contains(ip) {
		return false
	}
	// Shared address space and benchmarking ranges: routable on some networks,
	// never a place a notification destination legitimately lives.
	if carrierGradeNAT.Contains(ip) || benchmarking.Contains(ip) {
		return false
	}

	// The relaxation, and the only one.
	if p.AllowPrivate {
		return true
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !uniqueLocalIPv6(ip)
}

// cloudMetadata is 169.254.169.254/32, the link-local endpoint that serves
// instance credentials on every major cloud.
//
// Already covered by the link-local check above, and named separately anyway:
// this is the address the whole SSRF defence exists for, and an explicit refusal
// is what a reviewer looks for.
var cloudMetadata = &net.IPNet{
	IP:   net.IPv4(169, 254, 169, 254),
	Mask: net.CIDRMask(32, 32),
}

// carrierGradeNAT is 100.64.0.0/10: shared address space, not the internet.
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

// uniqueLocalIPv6 reports whether an address is in fc00::/7.
//
// net.IP.IsPrivate covers this, and it is spelled out because the IPv6 case is
// the one people forget: a destination on fd00::/8 is as internal as one on
// 10.0.0.0/8.
func uniqueLocalIPv6(ip net.IP) bool {
	if ip.To4() != nil {
		return false
	}
	return len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc
}
