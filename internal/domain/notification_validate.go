package domain

import (
	"errors"
	"net/mail"
	"strconv"
	"strings"
)

// Notification configuration validation.
//
// A destination and a rule arrive from an authenticated administrator, and are
// still validated as hostile input. An administrator account is the one an
// attacker most wants, and "an administrator typed it" is not a reason to skip
// a bound.
//
// Every message names the FIELD and the CONSTRAINT and never the value. For
// this subsystem that matters more than anywhere else in the project: the value
// being rejected is frequently a webhook URL, and a message that echoed it
// would put the credential in the response body, the browser console, and the
// operator's clipboard.

// NotificationLimits bounds a destination and a rule.
//
// Passed in rather than read from a package variable, matching PolicyLimits and
// UpdatePolicyLimits, so the API layer's configuration is what is enforced.
type NotificationLimits struct {
	MaxDestinations int `json:"maxDestinations"`
	MaxRules        int `json:"maxRules"`
	MaxRecipients   int `json:"maxRecipients"`
}

// Default notification bounds.
const (
	DefaultMaxNotificationDestinations = 25
	DefaultMaxNotificationRules        = 50
	DefaultMaxNotificationRecipients   = MaxEmailRecipients
)

// DefaultNotificationLimits returns the bounds used when none are configured.
func DefaultNotificationLimits() NotificationLimits {
	return NotificationLimits{
		MaxDestinations: DefaultMaxNotificationDestinations,
		MaxRules:        DefaultMaxNotificationRules,
		MaxRecipients:   DefaultMaxNotificationRecipients,
	}
}

// Normalised fills any unset bound with its default.
//
// Exported so a service can hold the resolved bounds rather than re-resolving
// them on every call, and so a zero-value NotificationLimits is safe rather
// than a set of bounds that are all zero and therefore refuse everything.
func (l NotificationLimits) Normalised() NotificationLimits { return l.normalise() }

// normalise fills any unset bound with its default.
func (l NotificationLimits) normalise() NotificationLimits {
	defaults := DefaultNotificationLimits()
	if l.MaxDestinations < 1 {
		l.MaxDestinations = defaults.MaxDestinations
	}
	if l.MaxRules < 1 {
		l.MaxRules = defaults.MaxRules
	}
	if l.MaxRecipients < 1 {
		l.MaxRecipients = defaults.MaxRecipients
	}
	return l
}

// SMTPSettings is where an email destination relays through.
//
// Separate from the destination so the same relay can serve several
// destinations without the password being stored several times, and so a
// deployment can supply it from the environment instead — which is how a
// password stays out of the database entirely.
type SMTPSettings struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// StartTLS upgrades a plaintext connection. The alternative is implicit
	// TLS on 465. There is no third option and no "no TLS": credentials and
	// container names do not travel in the clear.
	StartTLS bool `json:"startTls"`
}

// Default SMTP settings.
const (
	DefaultSMTPPort = 587
	MaxSMTPHostByte = 255
)

// Normalise trims and defaults a destination in place.
func (d *NotificationDestination) Normalise() {
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	d.TitlePrefix = strings.TrimSpace(d.TitlePrefix)
	d.EmailFrom = strings.TrimSpace(d.EmailFrom)
	d.Channel = NotificationChannel(strings.TrimSpace(string(d.Channel)))

	recipients := make([]string, 0, len(d.EmailTo))
	seen := make(map[string]struct{}, len(d.EmailTo))
	for _, address := range d.EmailTo {
		trimmed := strings.TrimSpace(address)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		recipients = append(recipients, trimmed)
	}
	d.EmailTo = recipients
}

// Validate checks a destination against the limits.
//
// The secret is validated alongside it rather than separately, because the
// rules are joint: a webhook channel needs a URL and no recipients, and an
// email channel needs recipients and no URL. Splitting them would mean each
// half could pass on its own and the pair still be nonsense.
func (d NotificationDestination) Validate(
	secret NotificationSecret,
	smtp SMTPSettings,
	limits NotificationLimits,
) error {
	limits = limits.normalise()

	if err := validatePolicyText("name", d.Name, MaxDestinationNameBytes, true); err != nil {
		return err
	}
	if err := validatePolicyText("description", d.Description,
		MaxDestinationDescriptionBytes, false); err != nil {
		return err
	}
	if err := validatePolicyText("titlePrefix", d.TitlePrefix,
		MaxTitlePrefixBytes, false); err != nil {
		return err
	}
	if !ValidNotificationChannel(string(d.Channel)) {
		return PolicyValidationError{
			Field:   "channel",
			Message: "must be one of webhook, discord, slack, teams, email",
		}
	}

	if d.Channel.WebhookBased() {
		return d.validateWebhook(secret)
	}
	return d.validateEmail(secret, smtp, limits)
}

// validateWebhook checks the URL-based channels.
func (d NotificationDestination) validateWebhook(secret NotificationSecret) error {
	if !secret.HasURL() {
		return PolicyValidationError{
			Field:   "url",
			Message: "is required for a webhook, Discord, Slack, or Teams destination",
		}
	}
	// The message names the constraint the URL broke and never the URL: a
	// webhook URL is a credential, and an error that echoed it would put the
	// credential in the response.
	if _, err := ParseDestinationURL(secret.URL); err != nil {
		return PolicyValidationError{Field: "url", Message: urlConstraint(err)}
	}
	// A webhook destination has no recipients. Rejected rather than ignored:
	// a caller that supplied them believes they do something.
	if len(d.EmailTo) > 0 || d.EmailFrom != "" {
		return PolicyValidationError{
			Field:   "emailTo",
			Message: "must be empty for a webhook destination",
		}
	}
	if secret.HasSMTPCredentials() {
		return PolicyValidationError{
			Field:   "smtpUsername",
			Message: "must be empty for a webhook destination",
		}
	}
	return nil
}

// validateEmail checks the SMTP channel.
func (d NotificationDestination) validateEmail(
	secret NotificationSecret,
	smtp SMTPSettings,
	limits NotificationLimits,
) error {
	if secret.HasURL() {
		return PolicyValidationError{
			Field:   "url",
			Message: "must be empty for an email destination",
		}
	}
	if strings.TrimSpace(smtp.Host) == "" {
		return PolicyValidationError{
			Field:   "smtpHost",
			Message: "is required for an email destination; configure a relay first",
		}
	}
	if len(smtp.Host) > MaxSMTPHostByte {
		return PolicyValidationError{
			Field:   "smtpHost",
			Message: "must be at most " + strconv.Itoa(MaxSMTPHostByte) + " bytes",
		}
	}
	if smtp.Port < 1 || smtp.Port > 65535 {
		return PolicyValidationError{
			Field:   "smtpPort",
			Message: "must be between 1 and 65535",
		}
	}

	switch {
	case len(d.EmailTo) == 0:
		return PolicyValidationError{
			Field:   "emailTo",
			Message: "must name at least one recipient",
		}
	case len(d.EmailTo) > limits.MaxRecipients:
		return PolicyValidationError{
			Field: "emailTo",
			Message: "must contain at most " +
				strconv.Itoa(limits.MaxRecipients) + " recipients",
		}
	}
	for index, address := range d.EmailTo {
		if message := validateEmailAddress(address); message != "" {
			return PolicyValidationError{
				Field:   "emailTo[" + strconv.Itoa(index) + "]",
				Message: message,
			}
		}
	}
	if d.EmailFrom == "" {
		return PolicyValidationError{
			Field:   "emailFrom",
			Message: "must name the envelope sender",
		}
	}
	if message := validateEmailAddress(d.EmailFrom); message != "" {
		return PolicyValidationError{Field: "emailFrom", Message: message}
	}
	return nil
}

// validateEmailAddress checks one address, returning a message or the empty
// string.
//
// Parsed with net/mail rather than matched against a pattern, and then checked
// for the things a parser accepts that an SMTP envelope must not carry: a
// display name, a control character, or a newline. The last is the one that
// matters — a newline in an address is SMTP header injection.
func validateEmailAddress(address string) string {
	if address == "" {
		return "must not be empty"
	}
	if len(address) > MaxEmailAddressBytes {
		return "must be at most " + strconv.Itoa(MaxEmailAddressBytes) + " bytes"
	}
	// Refused before parsing: net/mail tolerates some of these inside quoted
	// strings, and an address that reaches the envelope must be plain.
	for _, r := range address {
		if r < 0x20 || r == 0x7f {
			return "must not contain control characters"
		}
	}
	if strings.ContainsAny(address, "\r\n") {
		return "must not contain a line break"
	}

	parsed, err := mail.ParseAddress(address)
	if err != nil {
		return "must be a valid email address"
	}
	// `Name <addr>` is a valid address and is not what belongs in an envelope
	// or in a configuration field. Refused so the stored value and the sent
	// value are the same string.
	if parsed.Name != "" || parsed.Address != address {
		return "must be a bare address, without a display name"
	}
	if !strings.Contains(parsed.Address, "@") {
		return "must contain a domain"
	}
	return ""
}

// urlConstraint renders a URL validation failure without the URL.
//
// Classified with errors.Is against this package's own sentinels rather than by
// inspecting message text, so the mapping cannot drift when a message is
// reworded. The default is deliberately vague: an unrecognised failure must
// still say nothing about the value.
func urlConstraint(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrDestinationURLScheme):
		return "must be an https:// URL"
	case errors.Is(err, ErrDestinationURLHost):
		return "must name a hostname rather than an IP address literal, localhost, or a single-label name"
	case errors.Is(err, ErrDestinationURLUserinfo):
		return "must not embed a username or password"
	case errors.Is(err, ErrDestinationURLLength):
		return "must be at most " + strconv.Itoa(MaxDestinationURLBytes) + " bytes"
	default:
		return "must be a well-formed absolute https:// URL"
	}
}

// ------------------------------------------------------------------ rules --

// Normalise trims and deduplicates a rule in place.
func (r *NotificationRule) Normalise() {
	r.Name = strings.TrimSpace(r.Name)
	r.MinimumSeverity = NotificationSeverity(strings.TrimSpace(string(r.MinimumSeverity)))
	if r.MinimumSeverity == "" {
		r.MinimumSeverity = NotifyInfo
	}

	events := make([]NotificationEvent, 0, len(r.Events))
	seenEvents := make(map[NotificationEvent]struct{}, len(r.Events))
	for _, event := range r.Events {
		trimmed := NotificationEvent(strings.TrimSpace(string(event)))
		if trimmed == "" {
			continue
		}
		if _, duplicate := seenEvents[trimmed]; duplicate {
			continue
		}
		seenEvents[trimmed] = struct{}{}
		events = append(events, trimmed)
	}
	SortNotificationEvents(events)
	r.Events = events

	destinations := make([]string, 0, len(r.Destinations))
	seenDestinations := make(map[string]struct{}, len(r.Destinations))
	for _, destination := range r.Destinations {
		trimmed := strings.TrimSpace(destination)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seenDestinations[trimmed]; duplicate {
			continue
		}
		seenDestinations[trimmed] = struct{}{}
		destinations = append(destinations, trimmed)
	}
	r.Destinations = destinations

	if r.CooldownSeconds < 0 {
		r.CooldownSeconds = 0
	}
}

// ruleDestinationCeiling is the most destinations one rule may name.
//
// The SMALLER of the type's own bound and the deployment's destination limit. A
// rule cannot route to more destinations than the deployment permits to exist,
// and a message quoting a ceiling above that would send an administrator
// looking for destinations they are not allowed to create.
func ruleDestinationCeiling(limits NotificationLimits) int {
	if limits.MaxDestinations > 0 && limits.MaxDestinations < MaxRuleDestinations {
		return limits.MaxDestinations
	}
	return MaxRuleDestinations
}

// Validate checks a rule against the limits.
func (r NotificationRule) Validate(limits NotificationLimits) error {
	limits = limits.normalise()

	if err := validatePolicyText("name", r.Name, MaxRuleNameBytes, true); err != nil {
		return err
	}
	if !ValidNotificationSeverity(string(r.MinimumSeverity)) {
		return PolicyValidationError{
			Field:   "minimumSeverity",
			Message: "must be one of info, warning, critical",
		}
	}

	if len(r.Events) > MaxRuleEvents {
		return PolicyValidationError{
			Field:   "events",
			Message: "must contain at most " + strconv.Itoa(MaxRuleEvents) + " events",
		}
	}
	for index, event := range r.Events {
		if !ValidNotificationEvent(string(event)) {
			return PolicyValidationError{
				Field:   "events[" + strconv.Itoa(index) + "]",
				Message: "is not a known event",
			}
		}
	}

	switch {
	case len(r.Destinations) == 0:
		// A rule with no destination delivers nothing. Refused rather than
		// stored, because it would sit in the list looking like it worked.
		return PolicyValidationError{
			Field:   "destinations",
			Message: "must name at least one destination",
		}
	case len(r.Destinations) > ruleDestinationCeiling(limits):
		return PolicyValidationError{
			Field: "destinations",
			Message: "must contain at most " +
				strconv.Itoa(ruleDestinationCeiling(limits)) + " destinations",
		}
	}
	for index, destination := range r.Destinations {
		if !ValidNotificationDestinationID(destination) {
			return PolicyValidationError{
				Field:   "destinations[" + strconv.Itoa(index) + "]",
				Message: "is not a well-formed destination identifier",
			}
		}
	}

	if r.CooldownSeconds < 0 || r.CooldownSeconds > MaxRuleCooldownSecond {
		return PolicyValidationError{
			Field: "cooldownSeconds",
			Message: "must be between 0 and " +
				strconv.Itoa(MaxRuleCooldownSecond),
		}
	}
	return nil
}

// Warnings reports rule settings that are legal but worth telling an operator
// about.
func (r NotificationRule) Warnings() []string {
	var warnings []string
	if len(r.Events) == 0 && r.MinimumSeverity == NotifyInfo {
		warnings = append(warnings,
			"this rule matches every event at every severity, which on a busy estate is a lot of messages")
	}
	if r.CooldownSeconds == 0 {
		warnings = append(warnings,
			"there is no cooldown, so a container failing repeatedly will send a message every time")
	}
	return warnings
}

// Warnings reports destination settings that are legal but worth telling an
// administrator about.
//
// Warnings are not refusals. Each of these is something somebody might mean;
// each is also something somebody frequently did not.
func (d NotificationDestination) Warnings() []string {
	var warnings []string

	if !d.Enabled {
		warnings = append(warnings,
			"this destination is disabled, so no rule that routes to it will deliver anything")
	}
	if d.Channel == ChannelEmail && len(d.EmailTo) > 1 {
		warnings = append(warnings,
			"every recipient receives every notification this destination is sent; "+
				"a distribution list is usually easier to manage than several addresses here")
	}
	if d.Channel != ChannelEmail && d.TitlePrefix == "" {
		warnings = append(warnings,
			"there is no title prefix, so messages from several HarborMaster "+
				"deployments in one channel will look alike")
	}
	return warnings
}
