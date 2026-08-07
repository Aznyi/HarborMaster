package notify

import (
	"context"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The sender: one guarded transport, five channels, and nothing else.
//
// # Why the registry is closed
//
// Channels are constructed here, from a fixed list, and there is no Register
// method. A subsystem that could accept a channel at runtime would be a plugin
// system, and a plugin system inside the one component that makes outbound
// requests to operator-supplied URLs is the last thing this project should
// grow.
//
// # The transport is built once and shared
//
// Every webhook channel holds the SAME guarded client, so the address policy,
// the redirect refusal, the timeouts, and the connection bounds are decided in
// one place. A channel cannot construct its own.

// Sender delivers a notification to whichever channel a destination names.
type Sender struct {
	channels map[domain.NotificationChannel]Channel
}

// SenderOptions configures a Sender.
type SenderOptions struct {
	// Policy decides which resolved addresses may be contacted.
	Policy AddressPolicy
	// Version is HarborMaster's build version, sent as the user agent and in
	// the generic webhook payload. Nothing else about the host is disclosed.
	Version string
}

// NewSender builds the sender and its channels.
func NewSender(opts SenderOptions) *Sender {
	transport := newTransport(opts.Policy, opts.Version)

	return &Sender{
		channels: map[domain.NotificationChannel]Channel{
			domain.ChannelWebhook: webhookChannel{sender: transport},
			domain.ChannelDiscord: discordChannel{sender: transport},
			domain.ChannelSlack:   slackChannel{sender: transport},
			domain.ChannelTeams:   teamsChannel{sender: transport},
			domain.ChannelEmail:   emailChannel{policy: opts.Policy, version: opts.Version},
		},
	}
}

// Send delivers one notification to one destination.
//
// # Sanitised again here
//
// The caller sanitised it before storing it. This does so again, immediately
// before it leaves the process, because this is the last point at which
// HarborMaster owns the bytes — and a bound applied twice costs a pass over a
// short string, while one applied never is discovered by the payload that
// reaches somebody's chat client.
func (s *Sender) Send(ctx context.Context, request SendRequest) Result {
	channel, known := s.channels[request.Destination.Channel]
	if !known {
		// A channel this build does not implement. Refused rather than
		// defaulted to a webhook: sending a document in the wrong shape to a
		// URL is worse than not sending it.
		return failed(FailureConfiguration, 0, false)
	}
	if !request.Destination.Enabled {
		// Checked here as well as by the caller. A disabled destination that
		// still received deliveries would be an off switch that does not.
		return failed(FailureConfiguration, 0, false)
	}

	request.Notification = request.Notification.Sanitise()
	return channel.Send(ctx, request)
}

// Channels reports which channels this build implements.
//
// Served to the UI so the destination editor's picker is built from what
// actually exists, rather than from a second list that can drift from it.
func (s *Sender) Channels() []domain.NotificationChannel {
	// Ordered from the fixed vocabulary rather than from the map, so the answer
	// is stable across calls.
	out := make([]domain.NotificationChannel, 0, len(domain.NotificationChannels))
	for _, channel := range domain.NotificationChannels {
		if _, ok := s.channels[channel]; ok {
			out = append(out, channel)
		}
	}
	return out
}
