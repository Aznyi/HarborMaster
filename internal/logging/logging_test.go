package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/logging"
)

func TestNewEmitsJSONByDefault(t *testing.T) {
	var buf bytes.Buffer

	logging.New(&buf, "info", "json").Info("hello", slog.String("component", "test"))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", buf.String(), err)
	}
	if record["msg"] != "hello" {
		t.Errorf("msg = %v", record["msg"])
	}
	if record["component"] != "test" {
		t.Errorf("component = %v", record["component"])
	}
}

func TestNewHonoursTextFormat(t *testing.T) {
	var buf bytes.Buffer

	logging.New(&buf, "info", "text").Info("hello")

	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("expected text output, got %q", buf.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, "warn", "json")

	logger.Debug("debug message")
	logger.Info("info message")
	if buf.Len() != 0 {
		t.Errorf("records below the configured level should be dropped, got %q", buf.String())
	}

	logger.Warn("warn message")
	if !strings.Contains(buf.String(), "warn message") {
		t.Errorf("warn should pass, got %q", buf.String())
	}
}

// An unparseable level must still yield a usable logger: it is needed to report
// the configuration error itself.
func TestUnknownLevelFallsBackToInfo(t *testing.T) {
	var buf bytes.Buffer

	logger := logging.New(&buf, "not-a-level", "not-a-format")
	logger.Info("still works")

	if !strings.Contains(buf.String(), "still works") {
		t.Errorf("expected a usable logger, got %q", buf.String())
	}
}

func TestIsSensitiveHeader(t *testing.T) {
	sensitive := []string{"Authorization", "authorization", "Cookie", "Set-Cookie",
		"Proxy-Authorization", "X-Registry-Auth", "X-Api-Key"}
	for _, header := range sensitive {
		if !logging.IsSensitiveHeader(header) {
			t.Errorf("IsSensitiveHeader(%q) = false, want true", header)
		}
	}

	for _, header := range []string{"Content-Type", "Accept", "X-Request-ID"} {
		if logging.IsSensitiveHeader(header) {
			t.Errorf("IsSensitiveHeader(%q) = true, want false", header)
		}
	}
}
