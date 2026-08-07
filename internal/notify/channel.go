package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The channel interface, and what every implementation of it may and may not
// do.
//
// # One interface, provider-specific code behind it
//
// A channel turns a domain.Notification into whatever one service expects and
// hands it to a transport this package owns. It does NOT own a transport of its
// own: the guarded client is built once, by the Sender, and passed in. A
// channel that could construct its own http.Client could construct one without
// the address guard.
//
// # What a channel is handed, and what it is not
//
// It gets the notification, the destination's public record, and the secret --
// and the secret only at the moment of sending. It never gets a database
// handle, a logger that writes the payload, or anything that could persist what
// it was given.
//
// # Failures are classified, not reported
//
// A channel returns a Result carrying a status code and one of this package's
// own failure reasons. It never returns the transport's error text: that
// carries hostnames, addresses, and sometimes the URL. Classification happens
// here, once, so no channel has to remember.

// Channel delivers a notification to one kind of destination.
type Channel interface {
	// Name is the channel this implements.
	Name() domain.NotificationChannel

	// Send delivers one notification. The context carries the delivery
	// deadline; a channel must not extend it.
	Send(ctx context.Context, request SendRequest) Result
}

// SendRequest is everything a channel needs for one delivery.
type SendRequest struct {
	// Notification is the sanitised payload. Already bounded and stripped of
	// control characters by domain.Notification.Sanitise.
	Notification domain.Notification
	// Destination is the public record: the name, the title prefix, the
	// recipients. Not the URL, and not the password.
	Destination domain.NotificationDestination
	// Secret is the credential, handed over only for the duration of this call.
	Secret domain.NotificationSecret
	// SMTP is the relay an email destination uses. Zero for a webhook.
	SMTP domain.SMTPSettings
}

// Result is how one delivery attempt ended.
//
// Carries a machine-readable reason and HarborMaster's own sentence, and
// deliberately carries no error value: an error would be a way for a
// destination's text to travel further than this function.
type Result struct {
	// OK reports that the destination accepted the notification.
	OK bool
	// StatusCode is the HTTP status, when there was one. Zero for email and for
	// a failure that never reached a response.
	StatusCode int
	// Reason classifies the failure. Empty when OK.
	Reason FailureReason
	// Detail is HarborMaster's own sentence about the outcome, from a fixed
	// vocabulary. Never the transport's error text.
	Detail string
	// Retryable reports whether another attempt could plausibly succeed. A 404
	// is not retryable; a 503 is.
	Retryable bool
}

// FailureReason is the closed vocabulary of delivery failures.
type FailureReason string

const (
	// FailureBlocked means the destination resolved somewhere this deployment
	// refuses to contact. Never retryable: the address will not change.
	FailureBlocked FailureReason = "blockedAddress"
	// FailureRedirect means the destination tried to redirect.
	FailureRedirect FailureReason = "redirectRefused"
	// FailureUnreachable means the connection could not be made.
	FailureUnreachable FailureReason = "unreachable"
	// FailureTimeout means the destination did not answer in time.
	FailureTimeout FailureReason = "timeout"
	// FailureTLS means the certificate could not be verified.
	FailureTLS FailureReason = "tls"
	// FailureRejected means the destination answered with a client error. Not
	// retryable: a 400 or a 404 means the request was wrong, and repeating it
	// will produce the same answer.
	FailureRejected FailureReason = "rejected"
	// FailureUnauthorised means the destination rejected the credential. The
	// most common real failure: a revoked or rotated webhook URL.
	FailureUnauthorised FailureReason = "unauthorised"
	// FailureRateLimited means the destination asked HarborMaster to slow down.
	FailureRateLimited FailureReason = "rateLimited"
	// FailureServer means the destination had a problem of its own. Retryable.
	FailureServer FailureReason = "serverError"
	// FailureConfiguration means the destination is not usable as configured --
	// a missing URL, an unparsable relay. Never retryable.
	FailureConfiguration FailureReason = "configuration"
	// FailureInternal means HarborMaster could not build the request.
	FailureInternal FailureReason = "internal"
)

// Explain renders a failure in operator-facing words.
func (r FailureReason) Explain() string {
	switch r {
	case FailureBlocked:
		return "the destination resolved to an address this deployment does not contact"
	case FailureRedirect:
		return "the destination tried to redirect, which is not followed"
	case FailureUnreachable:
		return "the destination could not be reached"
	case FailureTimeout:
		return "the destination did not answer in time"
	case FailureTLS:
		return "the destination's certificate could not be verified"
	case FailureRejected:
		return "the destination rejected the message"
	case FailureUnauthorised:
		return "the destination rejected the credential; the webhook URL may have been revoked"
	case FailureRateLimited:
		return "the destination asked HarborMaster to slow down"
	case FailureServer:
		return "the destination reported a problem of its own"
	case FailureConfiguration:
		return "this destination is not usable as configured"
	case FailureInternal:
		return "HarborMaster could not build the message"
	default:
		return string(r)
	}
}

// succeeded builds an OK result.
func succeeded(status int) Result {
	return Result{OK: true, StatusCode: status, Detail: "the destination accepted it"}
}

// failed builds a failure result from a reason.
func failed(reason FailureReason, status int, retryable bool) Result {
	return Result{
		StatusCode: status,
		Reason:     reason,
		Detail:     reason.Explain(),
		Retryable:  retryable,
	}
}

// classifyStatus turns an HTTP status into a result.
//
// The mapping decides what gets RETRIED, which matters more than the label: a
// destination answering 404 forever must not be retried forever, and one
// answering 503 during a deploy should be.
func classifyStatus(status int) Result {
	switch {
	case status >= 200 && status < 300:
		return succeeded(status)
	case status == 401 || status == 403:
		// A revoked webhook URL. Not retryable: it will keep being revoked.
		return failed(FailureUnauthorised, status, false)
	case status == 408:
		return failed(FailureTimeout, status, true)
	case status == 429:
		return failed(FailureRateLimited, status, true)
	case status >= 400 && status < 500:
		return failed(FailureRejected, status, false)
	case status >= 500:
		return failed(FailureServer, status, true)
	default:
		// 1xx and 3xx. A 3xx here means the redirect guard did not fire, which
		// should be impossible; treated as a rejection rather than reasoned
		// about.
		return failed(FailureRejected, status, false)
	}
}

// classifyTransport turns a transport error into a result WITHOUT carrying its
// text.
//
// This function is the reason no destination hostname, IP address, or URL ever
// reaches a delivery record or a log line. Every branch returns one of
// HarborMaster's own sentences.
func classifyTransport(err error) Result {
	switch {
	case err == nil:
		return succeeded(0)
	case errors.Is(err, ErrBlockedAddress):
		return failed(FailureBlocked, 0, false)
	case errors.Is(err, ErrRedirectRefused):
		return failed(FailureRedirect, 0, false)
	case errors.Is(err, context.DeadlineExceeded):
		return failed(FailureTimeout, 0, true)
	case errors.Is(err, context.Canceled):
		// Shutdown, or a cancelled delivery. Retryable: nothing about the
		// destination was established.
		return failed(FailureTimeout, 0, true)
	}

	// A certificate problem is worth distinguishing, because the remedy is
	// different from an unreachable host: an operator fixes a certificate, and
	// cannot fix a firewall from here.
	//
	// Matched on the error TYPE. errors.As unwraps the *url.Error the HTTP
	// client wraps everything in -- which matters beyond convenience, because
	// url.Error's own message embeds the URL, and the URL is the credential.
	// Nothing below ever renders err; every branch returns a fixed sentence.
	var certificateErr *tls.CertificateVerificationError
	if errors.As(err, &certificateErr) {
		return failed(FailureTLS, 0, false)
	}
	var recordErr *tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		// A TLS handshake against something that is not speaking TLS. Usually a
		// plaintext port typed into an https:// URL.
		return failed(FailureTLS, 0, false)
	}

	// A timeout that did not arrive as context.DeadlineExceeded -- a dial or a
	// response-header timeout from the transport's own budgets.
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return failed(FailureTimeout, 0, true)
	}
	return failed(FailureUnreachable, 0, true)
}

// titleFor renders the delivered title, with the destination's prefix.
//
// The prefix is the ONE piece of operator text that reaches a message. It is
// bounded and sanitised at validation, and it is concatenated here as plain
// text -- never interpolated into markup, and never into anything a receiver
// evaluates.
func titleFor(request SendRequest) string {
	title := request.Notification.Title
	prefix := strings.TrimSpace(request.Destination.TitlePrefix)
	if prefix == "" {
		return title
	}
	return prefix + " " + title
}
