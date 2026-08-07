package domain

import (
	"errors"
	"net/url"
	"strings"
)

// Where a notification destination's credentials live, and why they live apart.
//
// # The URL is the credential
//
// For Slack, Discord, and Teams, the incoming-webhook URL IS the authentication
// token — anybody holding it can post to that channel forever. For a generic
// webhook it usually carries one too, in a path segment or a query parameter.
// An SMTP password is a password.
//
// So none of them is a field on NotificationDestination. They are here, on a
// type the API never returns, the repository stores in its own columns, and no
// read path loads unless it is about to send.
//
// # What an operator sees instead
//
// NotificationDestination.Endpoint: a scheme and a host, and nothing else.
// `https://hooks.slack.com` tells somebody which destination they are looking
// at without telling them how to post to it.
//
// # This is the same rule the rest of HarborMaster follows
//
// Invariant 3: no secret value is persisted, logged, or returned. A webhook URL
// cannot be a keyed digest — it has to be usable — so the rule it follows is the
// narrower one that applies to anything that must be stored in the clear: it
// lives in exactly one place, it is loaded only by the code that sends, and it
// appears in no response, no log line, and no error message.

// NotificationSecret is the part of a destination that must never be shown.
//
// Deliberately a separate type from NotificationDestination rather than fields
// with a `json:"-"` tag. A tag is a promise the marshaller keeps and a log line
// does not; a separate type is a promise the compiler keeps, because a handler
// that never loads one cannot leak one.
type NotificationSecret struct {
	// URL is the full webhook endpoint, including whatever token its path or
	// query carries. Empty for an email destination.
	URL string
	// SMTPPassword is the password for an email destination. Empty for a
	// webhook.
	SMTPPassword string
	// SMTPUsername is not a secret, but it lives here so the credential pair is
	// loaded and handled as one thing.
	SMTPUsername string
}

// HasURL reports whether a webhook endpoint was supplied.
func (s NotificationSecret) HasURL() bool { return strings.TrimSpace(s.URL) != "" }

// HasSMTPCredentials reports whether SMTP authentication was supplied.
//
// Unauthenticated SMTP is legitimate — a relay on the same host, or one that
// authenticates by address — so an empty pair is a valid configuration rather
// than an incomplete one.
func (s NotificationSecret) HasSMTPCredentials() bool {
	return strings.TrimSpace(s.SMTPUsername) != "" || s.SMTPPassword != ""
}

// URL validation errors. Each names the constraint and never the value: a
// message that echoed the URL would put the credential in the response that
// rejected it.
var (
	// ErrDestinationURLScheme reports a non-HTTPS destination.
	ErrDestinationURLScheme = errors.New(
		"a notification destination must be an https:// URL")
	// ErrDestinationURLShape reports a URL that could not be parsed, or that
	// carries something a webhook endpoint has no business carrying.
	ErrDestinationURLShape = errors.New(
		"the notification destination URL is not a well-formed absolute URL")
	// ErrDestinationURLHost reports a host that is not a plain hostname.
	ErrDestinationURLHost = errors.New(
		"the notification destination host must be a hostname, not an address literal")
	// ErrDestinationURLUserinfo reports credentials embedded in the URL.
	ErrDestinationURLUserinfo = errors.New(
		"the notification destination URL must not embed a username or password")
	// ErrDestinationURLLength reports a URL past the bound.
	ErrDestinationURLLength = errors.New(
		"the notification destination URL is too long")
)

// MaxDestinationURLBytes bounds a webhook URL.
//
// Generous for every real webhook — Discord's are around 120 characters — and
// small enough that a URL cannot become a way to store a payload in the
// destinations table.
const MaxDestinationURLBytes = 2048

// ParseDestinationURL validates an operator-supplied webhook URL.
//
// # This is the first of two SSRF defences, and the weaker one
//
// It refuses what can be refused by LOOKING: a non-HTTPS scheme, an address
// literal, embedded credentials, a host that is obviously local. It cannot
// refuse a hostname that resolves somewhere internal, because resolution has
// not happened yet and would be a different answer by the time it did.
//
// The defence that actually holds is the dial-time address guard in
// internal/notify, which inspects the IP the connection is about to use. This
// one exists so that an operator who typed something wrong is told at the
// moment they type it, rather than by a delivery that fails later.
//
// # Why address literals are refused outright
//
// `https://10.0.0.5/hook` is a destination whose only purpose is to reach
// something the dial guard would refuse anyway. Refusing the literal makes the
// error immediate and comprehensible instead of arriving as a failed delivery.
// An operator who genuinely wants an internal destination gives it a name and
// opts in to private addresses — see the notify package.
func ParseDestinationURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, ErrDestinationURLShape
	}
	if len(trimmed) > MaxDestinationURLBytes {
		return nil, ErrDestinationURLLength
	}
	// A control character in a URL is a request-splitting attempt or a paste
	// accident. Both are refused before parsing, because url.Parse accepts
	// some of them.
	for _, r := range trimmed {
		if r < 0x21 || r == 0x7f {
			return nil, ErrDestinationURLShape
		}
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, ErrDestinationURLShape
	}
	if !parsed.IsAbs() || parsed.Opaque != "" {
		return nil, ErrDestinationURLShape
	}
	// HTTPS only. There is no plaintext path and no setting that adds one: a
	// notification carries container names and failure reasons, and a webhook
	// URL carries a token, and neither belongs on the wire in the clear.
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, ErrDestinationURLScheme
	}
	if parsed.User != nil {
		return nil, ErrDestinationURLUserinfo
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, ErrDestinationURLHost
	}
	if !plainHostname(host) {
		return nil, ErrDestinationURLHost
	}
	// A fragment is meaningless to a server and is a sign of a pasted browser
	// URL rather than a webhook endpoint.
	if parsed.Fragment != "" {
		return nil, ErrDestinationURLShape
	}
	return parsed, nil
}

// plainHostname reports whether a host is a name rather than an address.
//
// Refuses IPv4 and IPv6 literals, `localhost`, and anything with no dot. The
// last of those also catches a container name or a Docker service alias, which
// is exactly the kind of internal destination this check exists to make
// deliberate rather than accidental.
func plainHostname(host string) bool {
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return false
	}
	// An IPv6 literal, which url.Hostname has already stripped the brackets
	// from.
	if strings.Contains(lower, ":") {
		return false
	}
	// An IPv4 literal: every label is numeric.
	labels := strings.Split(lower, ".")
	if len(labels) < 2 {
		// A single-label name is a container, a service alias, or a search-
		// domain guess. None is a public webhook host.
		return false
	}
	numeric := true
	for _, label := range labels {
		if label == "" {
			return false
		}
		for _, r := range label {
			if r < '0' || r > '9' {
				numeric = false
			}
			// The character set a hostname label may use.
			isAlphanumeric := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !isAlphanumeric && r != '-' && r != '_' {
				return false
			}
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		if len(label) > 63 {
			return false
		}
	}
	return !numeric
}

// SafeEndpoint renders a destination URL as the scheme and host alone.
//
// What an operator sees in every list, every delivery record, and every log
// line. The path is deliberately absent: for Slack, Discord, and Teams it is
// the token, and for a generic webhook it usually contains one.
func SafeEndpoint(raw string) string {
	parsed, err := ParseDestinationURL(raw)
	if err != nil {
		// A URL that does not validate is never rendered back. Returning the
		// input here would be the one place an unvalidated string reached a
		// page.
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// SafeSMTPEndpoint renders an SMTP server as host and port.
//
// No credentials, and no recipient list: the recipients are on the destination
// record, which is a different thing from where the mail is relayed.
func SafeSMTPEndpoint(host string, port int) string {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return ""
	}
	return SanitiseDisplayText(trimmed, MaxEndpointBytes) + ":" + itoa(port)
}

// itoa renders a small non-negative integer without pulling strconv into a file
// whose whole subject is not formatting numbers.
func itoa(value int) string {
	if value <= 0 {
		return "0"
	}
	var digits [8]byte
	index := len(digits)
	for value > 0 && index > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
