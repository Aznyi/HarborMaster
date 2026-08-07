package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Deployment coverage.
//
// # Why these tests exist
//
// A setting that exists in the code and in nothing else is a setting nobody
// will ever find. Worse, one that is DOCUMENTED but has no entry in the compose
// file cannot be set at all by an operator following the supported deployment:
// the variable is read from the process environment, and Compose passes only
// what the `environment:` block names.
//
// That exact failure has happened here before. The acquisition, execution, and
// rollback settings were documented in `.env.example` and absent from
// `compose.yaml`, so a deployment could not enable image acquisition even
// though every document said how to. Nothing failed; the variables simply never
// reached the process.
//
// These tests close the loop in both directions.

// envVarPattern finds every environment variable the loader reads.
//
// The loader reaches the environment through a small set of helpers, each of
// which takes the name WITHOUT the HARBORMASTER_ prefix as a string literal.
// Matching on the helpers rather than on the prefix is what makes this find the
// names rather than the prose in the comments around them.
var envVarPattern = regexp.MustCompile(
	`(?:stringVar|intVar|boolVar|durationVar|floatVar|listVar|secretVar|lookup)\(\s*lookup\s*,\s*"([A-Z0-9_]+)"`)

// tableEntryPattern finds the names in the loader's table-driven blocks, which
// list a name in a struct literal rather than in a call.
var tableEntryPattern = regexp.MustCompile(`\{"([A-Z0-9_]+)",\s`)

// deploymentOnly are read by Compose rather than by the process.
//
// They configure the DEPLOYMENT — which image to run, where to publish it — so
// they belong in `.env.example` and have no place in the container's own
// environment block.
var deploymentOnly = map[string]bool{
	"HARBORMASTER_IMAGE":      true,
	"HARBORMASTER_TAG":        true,
	"HARBORMASTER_BIND":       true,
	"HARBORMASTER_PORT":       true,
	"HARBORMASTER_DOCKER_GID": true,
}

// notInCompose are settings the compose file deliberately does not forward.
//
// Each needs a REASON, and the reason is checked: an empty one fails. A
// deliberate omission with no stated rationale is indistinguishable from an
// oversight, which is the failure this whole file exists to catch.
var notInCompose = map[string]string{
	"HARBORMASTER_SMTP_PASSWORD": "a password in an environment variable is readable by " +
		"anything that can run `docker inspect`; the compose file forwards the FILE form instead",
}

func TestEveryEnvironmentVariableIsDocumented(t *testing.T) {
	t.Parallel()

	names := loaderVariables(t)
	if len(names) < 50 {
		t.Fatalf("found only %d environment variables in config.go; the pattern no "+
			"longer matches the loader", len(names))
	}

	documented := readFile(t, filepath.Join("..", "..", ".env.example"))

	var missing []string
	for _, name := range names {
		if !strings.Contains(documented, name) {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Errorf(".env.example does not mention:\n\t%s\n\n"+
			"A setting that exists in the code and in nothing else is a setting "+
			"nobody will find. Document it, including what happens when it is "+
			"wrong.", strings.Join(missing, "\n\t"))
	}
}

// Every FEATURE TOGGLE, and every setting that changes what the process is
// allowed to do, must be settable from the supported deployment.
//
// # Why this is scoped rather than exhaustive
//
// The compose file deliberately forwards the settings a deployment realistically
// changes, not all two hundred; anything else can be added to the block, and its
// header says so. But a TOGGLE is different in kind. It is the difference
// between a capability being present and absent, and an operator who cannot
// reach one from the supported deployment cannot use the feature at all —
// which is exactly the failure that shipped once already.
func TestEveryFeatureToggleReachesTheContainer(t *testing.T) {
	t.Parallel()

	compose := readFile(t, filepath.Join("..", "..", "deployments", "compose.yaml"))

	// Every toggle, discovered rather than listed: a new `_ENABLED` setting is
	// covered the day it is added.
	var required []string
	for _, name := range loaderVariables(t) {
		if strings.HasSuffix(name, "_ENABLED") {
			required = append(required, name)
		}
	}
	if len(required) < 8 {
		t.Fatalf("found only %d feature toggles; the pattern no longer matches "+
			"the loader", len(required))
	}

	// Plus the settings that decide how HarborMaster is reached and trusted.
	// Not toggles, but each one is a security decision an operator has to be
	// able to make without editing a file they did not write.
	required = append(required,
		"HARBORMASTER_TRUSTED_PROXIES",
		"HARBORMASTER_COOKIE_SECURE",
		"HARBORMASTER_HTTP_ADDR",
		"HARBORMASTER_DOCKER_HOST",
		"HARBORMASTER_DB_PATH",
		"HARBORMASTER_SELF_CONTAINER_ID",
		"HARBORMASTER_NOTIFICATIONS_ALLOW_PRIVATE_DESTINATIONS",
		"HARBORMASTER_SMTP_HOST",
		"HARBORMASTER_SMTP_PASSWORD_FILE",
	)

	var missing []string
	for _, name := range required {
		if reason, exempt := notInCompose[name]; exempt {
			if reason == "" {
				t.Errorf("%s is exempted from the compose file without a reason", name)
			}
			continue
		}
		// The name must appear as a KEY in the environment block. A mention in
		// a comment is not a mention Compose acts on.
		if !strings.Contains(compose, name+":") {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("deployments/compose.yaml does not forward:\n\t%s\n\n"+
			"Compose passes only what the `environment:` block names, so an "+
			"operator following the supported deployment cannot set these at all "+
			"-- and nothing will tell them, because the variable simply never "+
			"reaches the process.", strings.Join(missing, "\n\t"))
	}
}

// The compose file must not forward a variable the loader does not read.
//
// The other direction, and the one that produces a setting an operator changes
// with no effect whatsoever.
func TestTheComposeFileForwardsNothingUnknown(t *testing.T) {
	t.Parallel()

	known := make(map[string]bool)
	for _, name := range loaderVariables(t) {
		known[name] = true
	}
	for name := range deploymentOnly {
		known[name] = true
	}

	compose := readFile(t, filepath.Join("..", "..", "deployments", "compose.yaml"))

	// Only the KEYS of the environment block: a `${VAR:-default}` on the right
	// of one is the deployment's own variable, not the container's.
	keyPattern := regexp.MustCompile(`(?m)^\s{6}(HARBORMASTER_[A-Z0-9_]+):`)

	var unknown []string
	for _, match := range keyPattern.FindAllStringSubmatch(compose, -1) {
		if !known[match[1]] {
			unknown = append(unknown, match[1])
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Errorf("deployments/compose.yaml forwards variables the loader does not read:\n\t%s\n\n"+
			"An operator who changes one of these sees no effect at all, which is "+
			"worse than a setting that does not exist.", strings.Join(unknown, "\n\t"))
	}
}

// loaderVariables lists every HARBORMASTER_-prefixed name config.go reads.
func loaderVariables(t *testing.T) []string {
	t.Helper()

	source := readFile(t, "config.go")

	seen := make(map[string]bool)
	for _, pattern := range []*regexp.Regexp{envVarPattern, tableEntryPattern} {
		for _, match := range pattern.FindAllStringSubmatch(source, -1) {
			seen["HARBORMASTER_"+match[1]] = true
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
