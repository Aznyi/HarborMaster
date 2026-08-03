package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// renderDetail serialises a value for leak assertions. Used by tests that need
// to prove a secret appears nowhere in a structure.
func renderDetail(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

const rawInspectionFixture = `{
  "Id": "abc123",
  "Name": "/web",
  "Config": {
    "Image": "nginx:1.27",
    "Env": [
      "PATH=/usr/bin",
      "DB_PASSWORD=hunter2",
      "STRIPE_SECRET=sk_live_51H",
      "NORMAL=fine",
      "NOEQUALS"
    ],
    "Labels": {"app": "web", "auth-token": "should-be-masked"}
  },
  "HostConfig": {
    "LogConfig": {
      "Type": "splunk",
      "Config": {"splunk-token": "tok_secret", "splunk-url": "https://logs.example.com"}
    },
    "Sysctls": {"net.ipv4.ip_forward": "1"}
  },
  "NestedArbitrary": {
    "DeeperEnv": {"Env": ["NESTED_PASSWORD=deep-secret"]}
  }
}`

func TestRedactRawInspectionMasksEnvironmentValues(t *testing.T) {
	masker := domain.NewDefaultMasker()

	redacted := redactRawInspection([]byte(rawInspectionFixture), masker)
	rendered := string(redacted)

	for _, secret := range []string{"hunter2", "sk_live_51H", "tok_secret", "deep-secret", "should-be-masked"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("redacted payload still contains %q", secret)
		}
	}

	// Names survive: knowing a variable exists is the point of keeping the
	// payload at all.
	for _, name := range []string{"DB_PASSWORD", "STRIPE_SECRET", "NESTED_PASSWORD", "splunk-token"} {
		if !strings.Contains(rendered, name) {
			t.Errorf("redacted payload lost the variable name %q", name)
		}
	}

	// Non-sensitive values are untouched, or the payload would be useless.
	for _, keep := range []string{"/usr/bin", "fine", "nginx:1.27", "https://logs.example.com", "net.ipv4.ip_forward"} {
		if !strings.Contains(rendered, keep) {
			t.Errorf("redacted payload dropped the non-sensitive value %q", keep)
		}
	}
}

func TestRedactRawInspectionStaysValidJSON(t *testing.T) {
	redacted := redactRawInspection([]byte(rawInspectionFixture), domain.NewDefaultMasker())

	var decoded map[string]any
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redacted payload is not valid JSON: %v", err)
	}
	if decoded["Id"] != "abc123" {
		t.Errorf("structure not preserved: %+v", decoded)
	}
}

// An entry without "=" is not a name/value pair and must pass through rather
// than being mangled.
func TestRedactRawInspectionPreservesMalformedEnvEntries(t *testing.T) {
	redacted := string(redactRawInspection([]byte(rawInspectionFixture), domain.NewDefaultMasker()))

	if !strings.Contains(redacted, "NOEQUALS") {
		t.Error("an env entry without '=' should be preserved verbatim")
	}
}

// A payload that cannot be decoded cannot be checked for secrets, so it is
// dropped rather than stored unexamined.
func TestRedactRawInspectionDropsUndecodablePayloads(t *testing.T) {
	if got := redactRawInspection([]byte("{not json"), domain.NewDefaultMasker()); got != nil {
		t.Errorf("undecodable payload should be dropped, got %q", got)
	}
	if got := redactRawInspection(nil, domain.NewDefaultMasker()); got != nil {
		t.Errorf("empty payload should yield nil, got %q", got)
	}
}

func TestRedactRawInspectionHonoursCustomPatterns(t *testing.T) {
	// A deployment that names its secrets differently can say so.
	masker := domain.NewMasker([]string{"NORMAL"})

	redacted := string(redactRawInspection([]byte(rawInspectionFixture), masker))

	if strings.Contains(redacted, "=fine") {
		t.Error("custom pattern NORMAL was not applied")
	}
	// ...and the built-in patterns no longer apply, because the operator
	// replaced them rather than extending them.
	if !strings.Contains(redacted, "hunter2") {
		t.Error("expected default patterns to be replaced, not merged")
	}
}
