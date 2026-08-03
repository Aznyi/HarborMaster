package docker

import (
	"encoding/json"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// redactRawInspection removes secret-bearing values from a raw inspection
// payload.
//
// The raw payload exists for troubleshooting and for future fidelity work, and
// it is genuinely useful: it carries fields HarborMaster has not normalized
// yet. But it is also the container's complete configuration, environment
// included, so it cannot be stored or served as-is.
//
// What this does NOT do is make the payload a faithful copy. Redacted values
// are gone, not encrypted or recoverable, so the result cannot be used to
// recreate a container exactly. Anything claiming otherwise would be wrong.
//
// Redaction walks the decoded JSON rather than the string, so a value that
// merely looks like a secret in some other field is untouched, and a secret in
// a nested structure is not missed.
func redactRawInspection(raw []byte, masker *domain.Masker) []byte {
	if len(raw) == 0 {
		return nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// Undecodable payload: drop it entirely. Storing bytes that could not
		// be inspected for secrets is the one outcome worse than storing
		// nothing.
		return nil
	}

	redacted := redactValue(decoded, masker, "")

	out, err := json.Marshal(redacted)
	if err != nil {
		return nil
	}
	return out
}

// envKeys are the JSON keys whose values are "NAME=value" environment lists.
var envKeys = map[string]struct{}{
	"Env": {},
}

// optionKeys are JSON keys holding maps whose own keys may name a credential,
// such as a log driver's configuration.
var optionKeys = map[string]struct{}{
	"Config":  {}, // LogConfig.Config
	"Options": {},
	"Sysctls": {},
	"Labels":  {},
}

// redactValue walks a decoded JSON value. parentKey is the object key the
// value was found under, which is what tells an environment list apart from
// any other array of strings.
func redactValue(value any, masker *domain.Masker, parentKey string) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			if _, isOptions := optionKeys[parentKey]; isOptions {
				// This map's own keys are option names; mask by key.
				if text, ok := nested.(string); ok && masker.IsSensitive(key) {
					out[key] = domain.MaskedValue
					_ = text
					continue
				}
			}
			out[key] = redactValue(nested, masker, key)
		}
		return out

	case []any:
		out := make([]any, 0, len(typed))
		for _, nested := range typed {
			if _, isEnv := envKeys[parentKey]; isEnv {
				if entry, ok := nested.(string); ok {
					out = append(out, redactEnvEntry(entry, masker))
					continue
				}
			}
			out = append(out, redactValue(nested, masker, parentKey))
		}
		return out

	default:
		return value
	}
}

// redactEnvEntry masks the value half of a "NAME=value" entry, preserving the
// name so the variable is still visible in the inventory.
func redactEnvEntry(entry string, masker *domain.Masker) string {
	name, _, found := strings.Cut(entry, "=")
	if !found {
		return entry
	}
	if masker.IsSensitive(name) {
		return name + "=" + domain.MaskedValue
	}
	return entry
}
