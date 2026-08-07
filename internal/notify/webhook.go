package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The four URL-based channels.
//
// # One request builder, four payload shapes
//
// Discord, Slack, and Teams each want a different JSON document posted to the
// same kind of URL. The DOCUMENT differs; the request does not. So there is one
// post function that owns the guarded client, the bounds, the headers, and the
// classification, and four encoders that produce bytes.
//
// # There are no operator-supplied headers
//
// A destination cannot set one. Arbitrary headers are how a webhook becomes a
// way to reach an internal service that authenticates by header, and how a
// notification becomes a request-smuggling vector. The headers HarborMaster
// sends are fixed: a content type, an accept, and its own user agent.
//
// # Every payload is built by a marshaller from typed fields
//
// No string concatenation into JSON, no templating, no interpolation of
// operator text into a structure a receiver evaluates. encoding/json escapes,
// and the only operator text in the document is the title prefix, which arrives
// as a JSON string value.

// webhookChannel posts HarborMaster's own document.
//
// The document is a stable, versioned shape rather than whatever the internals
// happen to look like: somebody is going to parse it, and changing it silently
// would break them.
type webhookChannel struct{ sender *transport }

func (c webhookChannel) Name() domain.NotificationChannel { return domain.ChannelWebhook }

// webhookPayload is the generic document, and is a published contract.
type webhookPayload struct {
	// Version lets a receiver branch. Incremented only when a field's meaning
	// changes, never when one is added.
	Version int `json:"version"`

	Event    string `json:"event"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`

	ContainerName string               `json:"containerName,omitempty"`
	Fields        []webhookPayloadKV   `json:"fields,omitempty"`
	Source        webhookPayloadSource `json:"source"`

	OccurredAt string `json:"occurredAt"`
}

type webhookPayloadKV struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// webhookPayloadSource identifies the sender.
//
// Deliberately NOT the hostname or any host detail: a receiver needs to know
// which product sent this, not what the machine is called.
type webhookPayloadSource struct {
	Product string `json:"product"`
	Version string `json:"version"`
}

func (c webhookChannel) Send(ctx context.Context, request SendRequest) Result {
	payload := webhookPayload{
		Version:       1,
		Event:         string(request.Notification.Event),
		Severity:      string(request.Notification.Severity),
		Title:         titleFor(request),
		Body:          request.Notification.Body,
		ContainerName: request.Notification.ContainerName,
		Source: webhookPayloadSource{
			Product: "HarborMaster",
			Version: c.sender.version,
		},
		OccurredAt: request.Notification.OccurredAt.UTC().Format(time.RFC3339),
	}
	for _, field := range request.Notification.Fields {
		payload.Fields = append(payload.Fields,
			webhookPayloadKV{Label: field.Label, Value: field.Value})
	}
	return c.sender.postJSON(ctx, request.Secret.URL, payload)
}

// ---------------------------------------------------------------- Discord --

type discordChannel struct{ sender *transport }

func (c discordChannel) Name() domain.NotificationChannel { return domain.ChannelDiscord }

// discordPayload is Discord's incoming-webhook shape, reduced to what
// HarborMaster uses.
type discordPayload struct {
	// Username and Content are deliberately absent as operator-settable
	// fields. Content would render as Markdown, which is a formatting-injection
	// surface; the embed's fields do not.
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Footer      *discordFooter      `json:"footer,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordFooter struct {
	Text string `json:"text"`
}

func (c discordChannel) Send(ctx context.Context, request SendRequest) Result {
	embed := discordEmbed{
		Title:       titleFor(request),
		Description: request.Notification.Body,
		Color:       discordColour(request.Notification.Severity),
		Timestamp:   request.Notification.OccurredAt.UTC().Format(time.RFC3339),
		Footer:      &discordFooter{Text: "HarborMaster"},
	}
	if name := request.Notification.ContainerName; name != "" {
		embed.Fields = append(embed.Fields,
			discordEmbedField{Name: "Container", Value: name, Inline: true})
	}
	for _, field := range request.Notification.Fields {
		embed.Fields = append(embed.Fields,
			discordEmbedField{Name: field.Label, Value: field.Value, Inline: true})
	}
	return c.sender.postJSON(ctx, request.Secret.URL,
		discordPayload{Embeds: []discordEmbed{embed}})
}

// discordColour maps severity to the embed's left bar.
//
// Colour is never the only signal: the title carries the severity in words too,
// for the same reason every badge in the UI does.
func discordColour(severity domain.NotificationSeverity) int {
	switch severity {
	case domain.NotifyCritical:
		return 0xD9382B // red
	case domain.NotifyWarning:
		return 0xE0A030 // amber
	default:
		return 0x3A8FD9 // blue
	}
}

// ------------------------------------------------------------------ Slack --

type slackChannel struct{ sender *transport }

func (c slackChannel) Name() domain.NotificationChannel { return domain.ChannelSlack }

// slackPayload uses Block Kit, whose text is escaped by Slack rather than
// interpreted as Markdown when the type is plain_text.
//
// `plain_text` throughout, deliberately. `mrkdwn` would let a container name
// containing an underscore render as italics, and would make any future
// unsanitised field an injection into somebody's chat client.
type slackPayload struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks,omitempty"`
}

type slackBlock struct {
	Type   string      `json:"type"`
	Text   *slackText  `json:"text,omitempty"`
	Fields []slackText `json:"fields,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c slackChannel) Send(ctx context.Context, request SendRequest) Result {
	title := titleFor(request)
	payload := slackPayload{
		// The fallback text, used in notifications and by clients that do not
		// render blocks.
		Text: title,
		Blocks: []slackBlock{{
			Type: "header",
			Text: &slackText{Type: "plain_text", Text: truncateRunes(title, 150)},
		}},
	}
	if body := request.Notification.Body; body != "" {
		payload.Blocks = append(payload.Blocks, slackBlock{
			Type: "section",
			Text: &slackText{Type: "plain_text", Text: truncateRunes(body, 3000)},
		})
	}

	fields := make([]slackText, 0, len(request.Notification.Fields)+2)
	fields = append(fields, slackText{
		Type: "plain_text",
		Text: "Severity: " + string(request.Notification.Severity),
	})
	if name := request.Notification.ContainerName; name != "" {
		fields = append(fields, slackText{Type: "plain_text", Text: "Container: " + name})
	}
	for _, field := range request.Notification.Fields {
		fields = append(fields, slackText{
			Type: "plain_text",
			Text: truncateRunes(field.Label+": "+field.Value, 2000),
		})
	}
	// Slack rejects a section with more than ten fields, so the list is bounded
	// here as well as at the notification.
	if len(fields) > 10 {
		fields = fields[:10]
	}
	payload.Blocks = append(payload.Blocks, slackBlock{Type: "section", Fields: fields})

	return c.sender.postJSON(ctx, request.Secret.URL, payload)
}

// ------------------------------------------------------------------ Teams --

type teamsChannel struct{ sender *transport }

func (c teamsChannel) Name() domain.NotificationChannel { return domain.ChannelTeams }

// teamsPayload is the MessageCard shape Teams incoming webhooks accept.
//
// MessageCard rather than an Adaptive Card: it is what a plain incoming webhook
// accepts without a Power Automate flow, which is what most self-hosted
// operators have. `markdown: false` on every section, so a container name is
// rendered as text.
type teamsPayload struct {
	Type       string         `json:"@type"`
	Context    string         `json:"@context"`
	Summary    string         `json:"summary"`
	ThemeColor string         `json:"themeColor"`
	Title      string         `json:"title"`
	Text       string         `json:"text,omitempty"`
	Sections   []teamsSection `json:"sections,omitempty"`
}

type teamsSection struct {
	Facts    []teamsFact `json:"facts,omitempty"`
	Markdown bool        `json:"markdown"`
}

type teamsFact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (c teamsChannel) Send(ctx context.Context, request SendRequest) Result {
	title := titleFor(request)
	payload := teamsPayload{
		Type:       "MessageCard",
		Context:    "https://schema.org/extensions",
		Summary:    truncateRunes(title, 200),
		ThemeColor: teamsColour(request.Notification.Severity),
		Title:      title,
		Text:       request.Notification.Body,
	}

	facts := make([]teamsFact, 0, len(request.Notification.Fields)+2)
	facts = append(facts, teamsFact{
		Name:  "Severity",
		Value: string(request.Notification.Severity),
	})
	if name := request.Notification.ContainerName; name != "" {
		facts = append(facts, teamsFact{Name: "Container", Value: name})
	}
	for _, field := range request.Notification.Fields {
		facts = append(facts, teamsFact{Name: field.Label, Value: field.Value})
	}
	payload.Sections = []teamsSection{{Facts: facts, Markdown: false}}

	return c.sender.postJSON(ctx, request.Secret.URL, payload)
}

// teamsColour maps severity to the card's accent, as a hex string without the
// leading hash, which is what MessageCard expects.
func teamsColour(severity domain.NotificationSeverity) string {
	switch severity {
	case domain.NotifyCritical:
		return "D9382B"
	case domain.NotifyWarning:
		return "E0A030"
	default:
		return "3A8FD9"
	}
}

// ------------------------------------------------------------- the request --

// transport owns the guarded client and performs every webhook POST.
//
// One place, so the bounds, the headers, the redirect refusal, and the
// classification cannot be forgotten by a channel.
type transport struct {
	client  *http.Client
	version string
}

// newTransport builds the shared webhook transport.
func newTransport(policy AddressPolicy, version string) *transport {
	return &transport{client: newHTTPClient(policy), version: version}
}

// postJSON marshals a payload and posts it.
//
// # Everything a destination controls is bounded or discarded
//
// The URL was validated before storage and is re-validated here, so a row
// edited by hand cannot become a destination. The request body is bounded. The
// response body is read to a small bound and thrown away. The status code is
// the only thing that survives.
func (t *transport) postJSON(ctx context.Context, rawURL string, payload any) Result {
	// Re-validated at the point of use, not trusted from the database. A row
	// somebody edited with a sqlite3 shell is exactly the case this catches,
	// and it costs a parse.
	parsed, err := domain.ParseDestinationURL(rawURL)
	if err != nil {
		return failed(FailureConfiguration, 0, false)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return failed(FailureInternal, 0, false)
	}
	if len(body) > MaxRequestBodyBytes {
		// A payload this large means a channel encoder produced something the
		// notification bounds should have prevented. Refused locally rather
		// than sent.
		return failed(FailureInternal, 0, false)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		parsed.String(), bytes.NewReader(body))
	if err != nil {
		return failed(FailureInternal, 0, false)
	}
	// The complete header set. There is no way for a destination to add one.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent(t.version))

	response, err := t.client.Do(req)
	if err != nil {
		return classifyTransport(err)
	}
	defer func() { _ = response.Body.Close() }()

	// Read and discard, bounded. Draining lets the connection be reused; the
	// bound stops a hostile destination from making the process allocate; and
	// the value is discarded because it is third-party text that must reach no
	// log, no error, and no column.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxResponseBodyBytes))

	return classifyStatus(response.StatusCode)
}

// userAgent identifies HarborMaster to a destination.
//
// Names the project and its version and discloses nothing about the host, the
// daemon, or the estate.
func userAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return "HarborMaster/" + version + " (+https://github.com/Aznyi/HarborMaster)"
}

// truncateRunes bounds a string by RUNES rather than bytes.
//
// Byte truncation can split a multi-byte character and produce invalid UTF-8,
// which some receivers reject outright and others render as a replacement
// glyph. The limits here are the receiving services' own.
func truncateRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if count >= limit {
			break
		}
		builder.WriteRune(r)
		count++
	}
	return builder.String()
}
