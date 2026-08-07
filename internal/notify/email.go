package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The email channel.
//
// # Why SMTP is written out rather than pulled in
//
// A mail library would bring a dependency tree, and the subset HarborMaster
// needs is small: connect, STARTTLS or implicit TLS, optionally authenticate,
// one message, quit. net/smtp covers all of it, and writing the envelope by
// hand is what lets every header be one HarborMaster controls.
//
// # Header injection is the risk, and it is closed structurally
//
// A newline in an address or a subject is how an attacker adds `Bcc:` to
// somebody else's mail. Two things stop it here:
//
//  1. Addresses are validated at CONFIGURATION time -- net/mail parsed, control
//     characters refused, line breaks refused -- so a stored address cannot
//     carry one.
//  2. The subject is RFC 2047 encoded, which turns every byte outside a small
//     safe set into an encoded word. A newline in a title cannot survive it.
//
// And the body is plain text with no HTML part at all, so there is no markup
// for a container name to be interpreted as.
//
// # Credentials never reach a log
//
// The password is read from the secret at the moment of sending and is passed
// straight to smtp.Auth. It appears in no log line, no error, and no delivery
// record -- classifyTransport returns HarborMaster's own sentences, and the
// SMTP paths below do the same.

// emailChannel sends over SMTP.
type emailChannel struct {
	policy  AddressPolicy
	version string
}

func (c emailChannel) Name() domain.NotificationChannel { return domain.ChannelEmail }

// SMTP bounds.
const (
	// smtpDialTimeout bounds establishing the connection.
	smtpDialTimeout = 10 * time.Second
	// smtpTotalTimeout bounds the whole conversation, so a relay that accepts a
	// connection and then stalls cannot occupy a worker.
	smtpTotalTimeout = 30 * time.Second
	// maxSubjectRunes bounds the subject before encoding.
	maxSubjectRunes = 150
)

func (c emailChannel) Send(ctx context.Context, request SendRequest) Result {
	host := strings.TrimSpace(request.SMTP.Host)
	if host == "" || len(request.Destination.EmailTo) == 0 {
		return failed(FailureConfiguration, 0, false)
	}
	port := request.SMTP.Port
	if port <= 0 {
		port = domain.DefaultSMTPPort
	}

	ctx, cancel := context.WithTimeout(ctx, smtpTotalTimeout)
	defer cancel()

	message, err := c.buildMessage(request)
	if err != nil {
		return failed(FailureInternal, 0, false)
	}

	if err := c.deliver(ctx, host, port, request, message); err != nil {
		return classifySMTP(err)
	}
	return succeeded(0)
}

// deliver runs the SMTP conversation.
//
// The address guard applies here too. A relay is operator-supplied, exactly
// like a webhook URL, and "it is only mail" is not a reason to let it be an
// internal address scanner.
func (c emailChannel) deliver(
	ctx context.Context,
	host string,
	port int,
	request SendRequest,
	message []byte,
) error {
	address := net.JoinHostPort(host, strconv.Itoa(port))

	dialer := &net.Dialer{Timeout: smtpDialTimeout, Control: c.policy.guard}

	var (
		conn net.Conn
		err  error
	)
	// Port 465 is implicit TLS: the connection is encrypted from the first
	// byte. Everything else starts in the clear and is upgraded by STARTTLS,
	// which is required rather than attempted -- a relay that will not upgrade
	// is a relay HarborMaster does not send credentials or container names to.
	if port == 465 {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: smtpTLSConfig(host)}
		conn, err = tlsDialer.DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// The whole conversation inherits the context's deadline, so a stalled
	// relay is cut off rather than waited on.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if port != 465 {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return errSTARTTLSUnsupported
		}
		if err := client.StartTLS(smtpTLSConfig(host)); err != nil {
			return err
		}
	}

	if request.Secret.HasSMTPCredentials() {
		// PlainAuth refuses to send credentials over an unencrypted connection
		// unless the host is localhost. Both paths above are encrypted by this
		// point, so the refusal never fires -- and it is a second guarantee
		// that a password cannot cross a plaintext link.
		auth := smtp.PlainAuth("", request.Secret.SMTPUsername,
			request.Secret.SMTPPassword, host)
		if err := client.Auth(auth); err != nil {
			return err
		}
	}

	if err := client.Mail(request.Destination.EmailFrom); err != nil {
		return err
	}
	for _, recipient := range request.Destination.EmailTo {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// errSTARTTLSUnsupported reports a relay that will not encrypt.
var errSTARTTLSUnsupported = errors.New("the mail relay does not support STARTTLS")

// smtpTLSConfig is the TLS settings for a mail relay.
//
// Verification is on and the server name is set, so a relay presenting somebody
// else's certificate is refused. Same floor as every other outbound connection.
func smtpTLSConfig(host string) *tls.Config {
	config := tlsConfig()
	config.ServerName = host
	return config
}

// buildMessage renders the RFC 5322 message.
//
// # Every header is one HarborMaster wrote
//
// There is no header a destination or a notification can add. The subject is
// encoded; the addresses were validated at configuration; the body is plain
// text. A container name containing a newline could not have been stored, and
// would be encoded here even if it had been.
func (c emailChannel) buildMessage(request SendRequest) ([]byte, error) {
	notification := request.Notification

	var builder strings.Builder

	// RFC 2047 encoding turns anything outside printable ASCII into an encoded
	// word, which is what makes a newline in a title structurally impossible to
	// smuggle into the header block.
	subject := mime.QEncoding.Encode("utf-8", truncateRunes(titleFor(request), maxSubjectRunes))

	fmt.Fprintf(&builder, "From: %s\r\n", request.Destination.EmailFrom)
	fmt.Fprintf(&builder, "To: %s\r\n", strings.Join(request.Destination.EmailTo, ", "))
	fmt.Fprintf(&builder, "Subject: %s\r\n", subject)
	fmt.Fprintf(&builder, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&builder, "MIME-Version: 1.0\r\n")
	// text/plain only. There is no HTML alternative, so nothing in the body is
	// ever interpreted as markup by a mail client.
	fmt.Fprintf(&builder, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&builder, "Content-Transfer-Encoding: 8bit\r\n")
	// Identifies the sender without disclosing the host.
	fmt.Fprintf(&builder, "X-Mailer: HarborMaster/%s\r\n", c.version)
	// Marks this as automated so a mail system does not auto-reply to it, and
	// so a vacation responder does not answer every failed update.
	fmt.Fprintf(&builder, "Auto-Submitted: auto-generated\r\n")
	builder.WriteString("\r\n")

	builder.WriteString(notification.Title)
	builder.WriteString("\r\n\r\n")
	if notification.Body != "" {
		builder.WriteString(notification.Body)
		builder.WriteString("\r\n\r\n")
	}
	fmt.Fprintf(&builder, "Severity: %s\r\n", notification.Severity)
	if notification.ContainerName != "" {
		fmt.Fprintf(&builder, "Container: %s\r\n", notification.ContainerName)
	}
	for _, field := range notification.Fields {
		fmt.Fprintf(&builder, "%s: %s\r\n", field.Label, field.Value)
	}
	fmt.Fprintf(&builder, "\r\nSent by HarborMaster at %s\r\n",
		notification.OccurredAt.UTC().Format(time.RFC3339))

	return []byte(builder.String()), nil
}

// classifySMTP turns an SMTP failure into a result without carrying its text.
//
// A relay's rejection text is third-party input and occasionally quotes the
// envelope back, so it reaches no delivery record. What survives is one of
// HarborMaster's own sentences and, where the relay gave one, the class of its
// reply code.
func classifySMTP(err error) Result {
	switch {
	case err == nil:
		return succeeded(0)
	case errors.Is(err, ErrBlockedAddress):
		return failed(FailureBlocked, 0, false)
	case errors.Is(err, errSTARTTLSUnsupported):
		return failed(FailureTLS, 0, false)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return failed(FailureTimeout, 0, true)
	}

	var certificateErr *tls.CertificateVerificationError
	if errors.As(err, &certificateErr) {
		return failed(FailureTLS, 0, false)
	}

	// net/smtp wraps *textproto.Error, which carries the relay's reply code.
	// The CODE is useful and safe to act on; the Msg field beside it is
	// relay-controlled text and is deliberately never read.
	var protocolErr *textproto.Error
	if errors.As(err, &protocolErr) {
		return classifySMTPCode(protocolErr.Code)
	}

	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return failed(FailureTimeout, 0, true)
	}
	return failed(FailureUnreachable, 0, true)
}

// classifySMTPCode maps an SMTP reply code to a result.
//
// 4xx is transient by definition and is retried; 5xx is permanent and is not.
// That is the whole of SMTP's own retry contract, and honouring it is what
// stops HarborMaster hammering a relay that has told it to stop.
func classifySMTPCode(code int) Result {
	switch {
	case code >= 200 && code < 400:
		return succeeded(0)
	case code == 421 || code == 450 || code == 451 || code == 452:
		return failed(FailureServer, 0, true)
	case code >= 400 && code < 500:
		return failed(FailureServer, 0, true)
	case code == 530 || code == 535:
		// Authentication required, or authentication failed.
		return failed(FailureUnauthorised, 0, false)
	case code >= 500:
		return failed(FailureRejected, 0, false)
	default:
		return failed(FailureUnreachable, 0, true)
	}
}
