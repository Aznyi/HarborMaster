package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Resolving a request's source address.
//
// # Forwarding headers are attacker-controlled text
//
// `X-Forwarded-For` and `Forwarded` are set by whatever sent the request. If
// HarborMaster believed them unconditionally, then:
//
//   - the source address in every audit record would be whatever the attacker
//     typed, which makes the audit log worse than useless -- it would be
//     confidently wrong;
//   - the per-address login throttle would be evaded by rotating the header,
//     which turns a brute-force control into decoration.
//
// So the default is to ignore them entirely and use the transport peer, which
// cannot be forged over TCP. An operator running behind a reverse proxy opts in
// by naming the proxy's address in TRUSTED_PROXIES.
//
// # Why the rightmost untrusted hop
//
// `X-Forwarded-For` is a list appended to by each hop: `client, proxy1,
// proxy2`. Only the entries added by hops HarborMaster trusts are trustworthy;
// everything to the left of the first trusted hop was supplied by whoever came
// before, including the client. So the walk goes RIGHT to LEFT, discarding
// trusted hops, and takes the first address that is not one -- that is the
// closest hop HarborMaster can attribute.
//
// Taking the LEFTMOST entry is the common mistake, and it is exactly the value
// an attacker controls.

// trustedProxies is a parsed TRUSTED_PROXIES allowlist.
//
// Parsed once at construction rather than per request: parsing a CIDR on every
// request would be a per-request allocation on the hot path, and a parse error
// discovered per request is a parse error nobody sees.
type trustedProxies struct {
	prefixes []netip.Prefix
}

// newTrustedProxies parses the configured allowlist.
//
// Unparseable entries are DROPPED here rather than refused, because
// config.Auth.validateTrustedProxies already refused startup for them. Reaching
// this with a bad entry means validation was bypassed, and silently trusting a
// value nobody could parse would be the wrong recovery.
func newTrustedProxies(entries []string) *trustedProxies {
	parsed := &trustedProxies{}

	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}

		// A bare address is read as a single-host range, because "127.0.0.1" is
		// what an operator writes.
		if addr, err := netip.ParseAddr(trimmed); err == nil {
			parsed.prefixes = append(parsed.prefixes,
				netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		if prefix, err := netip.ParsePrefix(trimmed); err == nil {
			parsed.prefixes = append(parsed.prefixes, prefix.Masked())
		}
	}
	return parsed
}

// enabled reports whether any proxy is trusted.
func (t *trustedProxies) enabled() bool { return t != nil && len(t.prefixes) > 0 }

// trusts reports whether an address is a configured proxy.
func (t *trustedProxies) trusts(addr netip.Addr) bool {
	if !t.enabled() {
		return false
	}
	unmapped := addr.Unmap()
	for _, prefix := range t.prefixes {
		if prefix.Contains(unmapped) {
			return true
		}
	}
	return false
}

// clientAddr resolves the address to attribute a request to.
//
// Returns a NORMALISED address: no port, canonical IPv6, and "unknown" for
// anything that will not parse. The value reaches an audit row, a session row,
// and a browser, so it must be one of a small set of well-formed shapes rather
// than whatever arrived.
func (s *Server) clientAddr(r *http.Request) string {
	peer := peerAddr(r)

	// With no trusted proxies, the transport peer is the only answer. This is
	// the default and the safe one.
	if !s.proxies.enabled() {
		return domain.NormaliseClientAddr(peer)
	}

	peerIP, err := netip.ParseAddr(peer)
	if err != nil {
		return domain.NormaliseClientAddr(peer)
	}
	// The immediate peer is not a proxy we trust, so it IS the client and its
	// headers are not to be believed.
	if !s.proxies.trusts(peerIP) {
		return domain.NormaliseClientAddr(peer)
	}

	forwarded := forwardedChain(r)
	// Walk right to left, discarding hops we trust. The first address that is
	// not a trusted proxy is the closest one we can attribute.
	for i := len(forwarded) - 1; i >= 0; i-- {
		addr, parseErr := netip.ParseAddr(forwarded[i])
		if parseErr != nil {
			// An unparseable entry means the chain is not what it claims to be.
			// Stop believing it from here leftwards and fall back to the last
			// address we could attribute, which is the trusted peer.
			break
		}
		if s.proxies.trusts(addr) {
			continue
		}
		return domain.NormaliseClientAddr(addr.String())
	}

	// Every hop in the chain is a trusted proxy, or the chain was empty. The
	// peer is the best answer available.
	return domain.NormaliseClientAddr(peer)
}

// maxForwardedHops bounds how much of a forwarding chain is parsed.
//
// A header is attacker-controlled and can be as long as the header limit
// allows. Without a bound, one request could make the server parse thousands of
// addresses.
const maxForwardedHops = 16

// forwardedChain extracts the address list from the forwarding headers.
//
// `X-Forwarded-For` only. RFC 7239's `Forwarded` is deliberately NOT parsed:
// its grammar has quoted strings, parameters, and obsolete line folding, and a
// hand-rolled parser for an attacker-controlled header is a liability out of
// all proportion to the header's rarity in practice. A deployment that only
// sets `Forwarded` gets the transport peer, which is safe and documented.
func forwardedChain(r *http.Request) []string {
	header := r.Header.Get("X-Forwarded-For")
	if header == "" {
		return nil
	}

	parts := strings.Split(header, ",")
	if len(parts) > maxForwardedHops {
		// Keep the RIGHTMOST hops: they are the ones closest to us and the ones
		// the walk above actually consults.
		parts = parts[len(parts)-maxForwardedHops:]
	}

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Some proxies append a port. Strip it; the port identifies nothing.
		if host, _, err := net.SplitHostPort(trimmed); err == nil {
			trimmed = host
		}
		// An IPv6 literal may arrive bracketed.
		trimmed = strings.Trim(trimmed, "[]")
		out = append(out, trimmed)
	}
	return out
}

// peerAddr returns the transport peer's address without its port.
func peerAddr(r *http.Request) string {
	if r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// requestIsSecure reports whether the browser's connection used HTTPS.
//
// Used to decide the Secure attribute on the session cookie. Three sources, in
// descending order of trustworthiness:
//
//  1. The configured override. An operator behind a TLS proxy that is not in
//     TRUSTED_PROXIES sets it because HarborMaster cannot know.
//  2. The transport. r.TLS is set when HarborMaster terminated TLS itself.
//  3. `X-Forwarded-Proto`, and ONLY from a trusted proxy. Believing it from an
//     arbitrary client would let anyone make the cookie Secure on a plain-HTTP
//     deployment, which stops the browser sending it -- a denial of service
//     delivered by a header.
func (s *Server) requestIsSecure(r *http.Request) bool {
	if s.authCfg.CookieSecure {
		return true
	}
	if r.TLS != nil {
		return true
	}
	if !s.proxies.enabled() {
		return false
	}

	peerIP, err := netip.ParseAddr(peerAddr(r))
	if err != nil || !s.proxies.trusts(peerIP) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
