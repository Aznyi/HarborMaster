package service

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/notify"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// maxSMTPPasswordFileBytes bounds the relay password file.
//
// A password is at most a few dozen bytes. The bound exists because the path
// comes from configuration and a misconfigured one could point at anything —
// reading a gigabyte into memory to discover it is not a password is a way to
// turn a typo into an outage.
const maxSMTPPasswordFileBytes = 4 << 10

// NotificationWiring is the assembled notification subsystem.
//
// Returned as one value rather than several so a caller cannot wire the engine
// without the sender, or the sender without the address policy.
type NotificationWiring struct {
	// Service is what the API reads and what a Notifier writes to. Never nil,
	// even when notifications are disabled: the history stays readable so an
	// operator can configure before switching sending on.
	Service *NotificationService
	// Notifier is what every other service holds. Nil when notifications are
	// disabled, which is what makes `raise` a no-op rather than a queue nobody
	// drains.
	Notifier Notifier
}

// WireNotifications assembles the notification subsystem.
//
// # Why the relay password is resolved here and not in config
//
// Because reading a file is not parsing an environment variable. The config
// package turns the environment into a struct and touches no disk; a file-backed
// secret is the same shape of thing as the installation key, and it is resolved
// where the other secrets are.
func WireNotifications(
	notifications *store.NotificationRepository,
	cfg config.Notifications,
	version string,
	logger *slog.Logger,
) (NotificationWiring, error) {
	secret, err := resolveSMTPSecret(cfg)
	if err != nil {
		return NotificationWiring{}, err
	}

	sender := notify.NewSender(notify.SenderOptions{
		Policy: notify.AddressPolicy{
			// The one relaxation of the SSRF guard, and it is off unless a
			// deployment asked. Even on, the guard still refuses link-local,
			// multicast, and the cloud metadata endpoint.
			AllowPrivate: cfg.AllowPrivateDestinations,
		},
		Version: version,
	})

	service := NewNotificationService(NotificationOptions{
		Store:  notifications,
		Sender: sender,
		SMTP: domain.SMTPSettings{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			StartTLS: cfg.SMTPStartTLS,
		},
		SMTPSecret: secret,
		Config:     cfg,
		Logger:     logger,
	})

	wiring := NotificationWiring{Service: service}
	if service.Enabled() {
		// Assigned to the interface ONLY when sending is on. A disabled
		// deployment leaves this nil, and every `raise` in the codebase becomes
		// a nil check rather than a queue that fills and drops.
		wiring.Notifier = service
	}
	return wiring, nil
}

// resolveSMTPSecret loads the relay credential.
func resolveSMTPSecret(cfg config.Notifications) (domain.NotificationSecret, error) {
	secret := domain.NotificationSecret{SMTPUsername: cfg.SMTPUsername}

	switch {
	case cfg.SMTPPasswordFile != "":
		password, err := readSecretFile(cfg.SMTPPasswordFile)
		if err != nil {
			// The PATH is named because an operator has to fix it and it is
			// their own path. The contents never appear, and neither does the
			// underlying error, whose text can include a partial read.
			return domain.NotificationSecret{}, fmt.Errorf(
				"read the SMTP password file at %q: %w", cfg.SMTPPasswordFile, err)
		}
		secret.SMTPPassword = password
	case cfg.SMTPPassword != "":
		secret.SMTPPassword = cfg.SMTPPassword
	}

	return secret, nil
}

// readSecretFile reads a bounded secret from disk.
//
// Trailing whitespace is stripped because a file written with `echo` ends in a
// newline, and a password with a newline on the end fails authentication in a
// way that looks exactly like a wrong password.
func readSecretFile(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // an operator's own configured path, bounded below
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	// One byte past the bound, so a file that is too long is DETECTED rather
	// than silently truncated into a wrong password.
	raw, err := io.ReadAll(io.LimitReader(file, maxSMTPPasswordFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxSMTPPasswordFileBytes {
		return "", errors.New("the file is larger than a credential should be")
	}

	password := strings.TrimRight(string(raw), "\r\n \t")
	if password == "" {
		return "", errors.New("the file is empty")
	}
	return password, nil
}
