// Package logging builds the application's structured logger.
//
// Security note: HarborMaster fronts a privileged Docker socket, so log records
// must never carry secrets. Two rules apply everywhere in the codebase:
//
//   - Never log environment-variable values, registry credentials, or the
//     Authorization/Cookie/Proxy-Authorization headers.
//   - Never log a raw error chain to the client. Errors go to the log; the API
//     returns a stable code and a generic message.
//
// Redact is provided so call sites that must mention a sensitive field can do
// so without disclosing it.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// Redacted is substituted for any value that must not appear in a log record.
const Redacted = "[REDACTED]"

// SensitiveHeaders are never copied into log records by the API middleware.
var SensitiveHeaders = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"x-registry-auth",
	"x-api-key",
}

// New returns a slog.Logger writing to w in the requested format.
//
// level is one of debug, info, warn, error; format is one of json, text.
// Unrecognised values fall back to info/json rather than failing, so a logger
// always exists for reporting the configuration error itself.
func New(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}

// IsSensitiveHeader reports whether a header must be omitted from logs.
func IsSensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, h := range SensitiveHeaders {
		if lower == h {
			return true
		}
	}
	return false
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
