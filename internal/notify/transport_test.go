package notify

import (
	"net"
	"net/http"
	"testing"
)

// Address-guard tests.
//
// This guard is the defence that actually holds against SSRF, because it runs
// on the IP the socket is about to use rather than on the name somebody typed.
// Everything else — the URL validation, the HTTPS requirement — is refusable by
// looking; this one is not.
//
// So it is tested exhaustively, in both directions: every range that must be
// refused, and every spelling of those ranges that an attacker would reach for.

func TestTheGuardRefusesEveryNonPublicRangeByDefault(t *testing.T) {
	policy := AddressPolicy{}

	cases := map[string]string{
		"loopback":                  "127.0.0.1",
		"loopback, another address": "127.9.9.9",
		"IPv6 loopback":             "::1",
		"private 10/8":              "10.0.0.5",
		"private 172.16/12":         "172.16.4.4",
		"private 192.168/16":        "192.168.1.1",
		"unique-local IPv6":         "fd00::1",
		"unique-local IPv6, fc":     "fc00::1",
		"link-local":                "169.254.10.10",
		"link-local IPv6":           "fe80::1",
		"cloud metadata":            "169.254.169.254",
		"unspecified":               "0.0.0.0",
		"unspecified IPv6":          "::",
		"multicast":                 "224.0.0.1",
		"multicast IPv6":            "ff02::1",
		"carrier-grade NAT":         "100.64.0.1",
		"benchmarking":              "198.18.0.1",

		// The mapped spellings. An attacker who knows the guard checks IPv4
		// reaches for these first.
		"IPv4-mapped loopback": "::ffff:127.0.0.1",
		"IPv4-mapped private":  "::ffff:10.0.0.5",
		"IPv4-mapped metadata": "::ffff:169.254.169.254",
	}

	for name, address := range cases {
		t.Run(name, func(t *testing.T) {
			ip := net.ParseIP(address)
			if ip == nil {
				t.Fatalf("fixture %q is not an address", address)
			}
			if policy.permits(ip) {
				t.Fatalf("%s must not be contactable by default", address)
			}
		})
	}
}

func TestTheGuardPermitsPublicAddresses(t *testing.T) {
	policy := AddressPolicy{}

	for _, address := range []string{
		"1.1.1.1",
		"8.8.8.8",
		// Slack, Discord, and Teams all live on public addresses; a guard that
		// refused these would refuse the whole feature.
		"151.101.1.140",
		"2606:4700:4700::1111",
	} {
		ip := net.ParseIP(address)
		if ip == nil {
			t.Fatalf("fixture %q is not an address", address)
		}
		if !policy.permits(ip) {
			t.Errorf("%s is a public address and must be contactable", address)
		}
	}
}

func TestTheOptInPermitsPrivateAddressesAndNothingElse(t *testing.T) {
	policy := AddressPolicy{AllowPrivate: true}

	// What the opt-in is FOR: a self-hosted receiver on a LAN.
	for _, address := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.10", "fd00::1"} {
		ip := net.ParseIP(address)
		if !policy.permits(ip) {
			t.Errorf("%s must be contactable once private destinations are allowed", address)
		}
	}

	// What it must NOT relax. These are not "internal services somebody might
	// notify" -- they are what an SSRF is aimed at, and the metadata endpoint
	// is the single most valuable target on any hosted machine.
	for _, address := range []string{
		"169.254.169.254",
		"169.254.1.1",
		"fe80::1",
		"224.0.0.1",
		"ff02::1",
		"0.0.0.0",
		"100.64.0.1",
		"198.18.0.1",
	} {
		ip := net.ParseIP(address)
		if policy.permits(ip) {
			t.Errorf("%s must be refused even when private destinations are allowed", address)
		}
	}
}

func TestTheGuardRefusesUnexpectedInput(t *testing.T) {
	policy := AddressPolicy{AllowPrivate: true}

	// A nil address is not a permissive default.
	if policy.permits(nil) {
		t.Fatal("a nil address must never be permitted")
	}

	// Control is only ever called for TCP. Anything else means something
	// unexpected is dialling, and the guard refuses rather than reasoning.
	if err := policy.guard("udp", "8.8.8.8:53", nil); err == nil {
		t.Fatal("a non-TCP network must be refused")
	}
	// An address Control could not parse.
	if err := policy.guard("tcp", "not-an-address", nil); err == nil {
		t.Fatal("an unparsable address must be refused")
	}
	// A NAME rather than a literal means resolution was bypassed.
	if err := policy.guard("tcp", "example.com:443", nil); err == nil {
		t.Fatal("an unresolved name must be refused")
	}
}

func TestTheGuardIsWiredIntoTheClient(t *testing.T) {
	// The guard is only a defence if the dialler actually calls it. This proves
	// the wiring rather than the logic: a refactor that built the client
	// without Control would leave every test above passing and the product
	// unprotected.
	client := newHTTPClient(AddressPolicy{})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the client's transport is %T, want the guarded one", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("the client has no guarded DialContext")
	}
	if transport.Proxy != nil {
		t.Fatal("proxy environment variables must be ignored: a proxy is an internal address")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("certificate verification must be on, with no way to turn it off")
	}
	if client.CheckRedirect == nil {
		t.Fatal("redirects must be refused: a redirect is a destination-controlled URL")
	}
	if err := client.CheckRedirect(nil, nil); err == nil {
		t.Fatal("CheckRedirect must refuse every redirect")
	}
}
