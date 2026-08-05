package registry

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Failure classification.
//
// # Why a registry's own words never appear here
//
// A registry is a third party, and its error bodies are attacker-influenced
// text on a private registry and simply unbounded text on a public one. Putting
// any of it into an error, a log line, or a database column would carry it into
// places that must stay trustworthy -- including a log a reviewer reads during
// an incident.
//
// So every failure is mapped to a HarborMaster CheckStatus and a detail chosen
// from the fixed set below. The registry's status code shapes the mapping; its
// response body is read only to be discarded.

// Failure sentinels.
var (
	// ErrNotFound reports that the repository or reference does not exist.
	ErrNotFound = errors.New("registry has no such repository or tag")
	// ErrUnauthorized reports that the repository requires credentials.
	// HarborMaster holds none by design, so this is an answer rather than a
	// fault.
	ErrUnauthorized = errors.New("registry requires credentials, which HarborMaster does not hold")
	// ErrRateLimited reports that the registry asked the client to slow down.
	ErrRateLimited = errors.New("registry rate limit reached")
	// ErrTransient reports a failure that another attempt could resolve.
	ErrTransient = errors.New("registry request failed transiently")
	// ErrPermanent reports a failure that retrying will not resolve.
	ErrPermanent = errors.New("registry request failed permanently")
	// ErrMalformedResponse reports a response that did not parse as the
	// distribution API.
	ErrMalformedResponse = errors.New("registry returned a response that could not be parsed")
	// ErrResponseTooLarge reports a response that exceeded its budget.
	ErrResponseTooLarge = errors.New("registry response exceeded its size budget")
	// ErrTagListingUnsupported reports that the registry does not enumerate
	// tags, so version comparison is unavailable.
	ErrTagListingUnsupported = errors.New("registry does not support tag listing")
)

// RetryAfter carries a registry's requested wait, when it sent one.
//
// Honoured rather than overridden: a registry that says "wait 60 seconds" is
// telling the client how to stay welcome, and a client that ignores it earns a
// longer ban.
type RetryAfter struct {
	Wait time.Duration
	// Set reports whether the registry actually supplied a value, as opposed to
	// the zero value meaning "no guidance".
	Set bool
}

// rateLimitError carries the registry's requested wait alongside the sentinel.
type rateLimitError struct {
	after RetryAfter
}

func (e rateLimitError) Error() string { return ErrRateLimited.Error() }

func (e rateLimitError) Unwrap() error { return ErrRateLimited }

// RetryAfterFor extracts a registry's requested wait from an error.
func RetryAfterFor(err error) RetryAfter {
	var limited rateLimitError
	if errors.As(err, &limited) {
		return limited.after
	}
	return RetryAfter{}
}

// maxRetryAfter caps how long a registry may ask HarborMaster to wait.
//
// A registry could otherwise ask for a year and remove an image from coverage
// indefinitely. The cap means a hostile or misconfigured Retry-After delays the
// next check rather than cancelling it.
const maxRetryAfter = 6 * time.Hour

// parseRetryAfter reads a Retry-After header value.
//
// Accepts the delta-seconds form and the HTTP-date form, and bounds both. A
// value that does not parse is treated as absent rather than as zero: "retry
// immediately" is not what an unparsable header means.
func parseRetryAfter(value string, now time.Time) RetryAfter {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 64 {
		return RetryAfter{}
	}

	if seconds, err := strconv.Atoi(trimmed); err == nil {
		if seconds < 0 {
			return RetryAfter{}
		}
		return RetryAfter{Wait: boundWait(time.Duration(seconds) * time.Second), Set: true}
	}

	if when, err := time.Parse(time.RFC1123, trimmed); err == nil {
		wait := when.Sub(now)
		if wait < 0 {
			wait = 0
		}
		return RetryAfter{Wait: boundWait(wait), Set: true}
	}
	return RetryAfter{}
}

func boundWait(wait time.Duration) time.Duration {
	if wait > maxRetryAfter {
		return maxRetryAfter
	}
	if wait < 0 {
		return 0
	}
	return wait
}

// Detail phrases.
//
// A fixed set, because these strings reach the database and the UI. None of
// them is derived from a registry response.
const (
	detailNotFound      = "the registry has no such repository or tag"
	detailUnauthorized  = "the repository is private; HarborMaster holds no registry credentials"
	detailRateLimited   = "the registry is rate-limiting requests"
	detailTimeout       = "the registry did not respond in time"
	detailUnreachable   = "the registry could not be reached"
	detailBlocked       = "the registry resolved to an address that is not publicly routable"
	detailRedirect      = "the registry attempted a redirect, which HarborMaster does not follow"
	detailMalformed     = "the registry returned a response HarborMaster could not parse"
	detailTooLarge      = "the registry response exceeded its size budget"
	detailServerError   = "the registry reported a server error"
	detailUnsupportedTL = "the registry does not support tag listing, so only the digest was compared"
	detailUnsupported   = "the image reference cannot be looked up: see the supported-registry rules"
	detailUnexpected    = "the registry responded in a way HarborMaster does not handle"
)

// Classify maps an error to the status and detail HarborMaster records.
//
// The returned detail is always one of the constants above. Nothing derived
// from the error's text is included, which is what guarantees a registry cannot
// write into HarborMaster's own records.
func Classify(err error) (domain.CheckStatus, string) {
	switch {
	case err == nil:
		return domain.CheckOK, ""

	case errors.Is(err, ErrNotFound):
		return domain.CheckNotFound, detailNotFound
	case errors.Is(err, ErrUnauthorized):
		return domain.CheckUnauthorized, detailUnauthorized
	case errors.Is(err, ErrRateLimited):
		return domain.CheckRateLimited, detailRateLimited

	case errors.Is(err, domain.ErrUnsupportedReference):
		return domain.CheckUnsupported, detailUnsupported
	case errors.Is(err, ErrBlockedAddress):
		// A blocked address is permanent and is a property of the reference, so
		// it is recorded as unsupported rather than as a failure to retry.
		return domain.CheckUnsupported, detailBlocked
	case errors.Is(err, ErrRedirectRefused):
		return domain.CheckUnsupported, detailRedirect
	case errors.Is(err, ErrTagListingUnsupported):
		return domain.CheckOK, detailUnsupportedTL

	case errors.Is(err, ErrResponseTooLarge):
		return domain.CheckFailed, detailTooLarge
	case errors.Is(err, ErrMalformedResponse):
		return domain.CheckFailed, detailMalformed

	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return domain.CheckFailed, detailTimeout
	case errors.Is(err, ErrTransient):
		return domain.CheckFailed, detailServerError
	case errors.Is(err, ErrPermanent):
		return domain.CheckFailed, detailUnexpected
	}

	// A DNS failure, a refused connection, a reset. All transient in the sense
	// that matters: the reference is fine and the network is not.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return domain.CheckFailed, detailUnreachable
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return domain.CheckFailed, detailUnreachable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return domain.CheckFailed, detailUnreachable
	}
	return domain.CheckFailed, detailUnexpected
}

// isTimeout reports whether an error is a network timeout.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// statusError maps an HTTP status to a sentinel.
//
// The response body is NOT read into the error. Registries put arbitrary text
// there, and this is the boundary that stops it travelling.
func statusError(status int, header map[string][]string, now time.Time) error {
	switch {
	case status == 401, status == 403:
		return ErrUnauthorized
	case status == 404:
		return ErrNotFound
	case status == 429:
		wait := RetryAfter{}
		if values := header["Retry-After"]; len(values) > 0 {
			wait = parseRetryAfter(values[0], now)
		}
		return rateLimitError{after: wait}
	case status == 408, status == 425:
		return ErrTransient
	case status >= 500:
		// 501 is the distribution API's "not implemented", which some
		// registries return for tag listing. Permanent rather than transient.
		if status == 501 {
			return ErrTagListingUnsupported
		}
		return ErrTransient
	case status >= 400:
		return ErrPermanent
	}
	return nil
}

// Transient reports whether an error is worth retrying immediately, as opposed
// to on the slow schedule.
//
// Used by the client's own retry loop. A rate limit is deliberately NOT
// transient here: retrying it within one request would be exactly the behaviour
// the registry asked HarborMaster to stop.
func Transient(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrRateLimited),
		errors.Is(err, ErrNotFound),
		errors.Is(err, ErrUnauthorized),
		errors.Is(err, ErrPermanent),
		errors.Is(err, ErrBlockedAddress),
		errors.Is(err, ErrRedirectRefused),
		errors.Is(err, domain.ErrUnsupportedReference),
		errors.Is(err, context.Canceled),
		errors.Is(err, ErrTagListingUnsupported):
		return false
	case errors.Is(err, ErrTransient),
		errors.Is(err, ErrMalformedResponse),
		errors.Is(err, context.DeadlineExceeded):
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}
