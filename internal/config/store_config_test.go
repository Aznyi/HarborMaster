package config

import (
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// Storage reliability configuration.
//
// Every setting here changes how HarborMaster behaves when something is
// already wrong, so the negative cases matter more than the happy one: a
// misconfigured integrity check that silently becomes "off" is exactly the
// failure the setting exists to prevent.

func TestStoreDefaults(t *testing.T) {
	cfg, err := load(envMap(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Store.BusyTimeout != DefaultDBBusyTimeout {
		t.Errorf("BusyTimeout = %s, want %s", cfg.Store.BusyTimeout, DefaultDBBusyTimeout)
	}
	if cfg.Store.IntegrityCheck != DefaultDBIntegrityCheck {
		t.Errorf("IntegrityCheck = %q, want %q", cfg.Store.IntegrityCheck, DefaultDBIntegrityCheck)
	}
	if cfg.Store.IntegrityTimeout != DefaultDBIntegrityTimeout {
		t.Errorf("IntegrityTimeout = %s, want %s", cfg.Store.IntegrityTimeout, DefaultDBIntegrityTimeout)
	}
	// WAL is a warning by default, not a refusal: a rollback journal is slower
	// but correct, and refusing to start on a filesystem that cannot do WAL
	// would be a harsh default.
	if cfg.Store.RequireWAL {
		t.Error("RequireWAL must default to false")
	}
}

func TestStoreSettingsAreRead(t *testing.T) {
	cfg, err := load(envMap(map[string]string{
		"HARBORMASTER_DB_BUSY_TIMEOUT":      "12s",
		"HARBORMASTER_DB_INTEGRITY_CHECK":   "FULL",
		"HARBORMASTER_DB_INTEGRITY_TIMEOUT": "2m",
		"HARBORMASTER_DB_REQUIRE_WAL":       "true",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Store.BusyTimeout != 12*time.Second {
		t.Errorf("BusyTimeout = %s, want 12s", cfg.Store.BusyTimeout)
	}
	// Case is normalised, so FULL and full are the same setting.
	if cfg.Store.IntegrityCheck != IntegrityCheckFull {
		t.Errorf("IntegrityCheck = %q, want %q", cfg.Store.IntegrityCheck, IntegrityCheckFull)
	}
	if cfg.Store.IntegrityTimeout != 2*time.Minute {
		t.Errorf("IntegrityTimeout = %s, want 2m", cfg.Store.IntegrityTimeout)
	}
	if !cfg.Store.RequireWAL {
		t.Error("RequireWAL was not read")
	}
}

// An unrecognised mode must be rejected, never treated as "off". Silently
// skipping the integrity check because of a typo is the failure this prevents.
func TestUnknownIntegrityModeIsRejected(t *testing.T) {
	// Distinctive values only. A short one like "on" appears as a substring of
	// the phrase "must be one of", which would make the leak assertion below
	// fail on the message rather than on a leak.
	for _, value := range []string{"thorough", "quick_check", "enabled", "verify-everything"} {
		t.Run(value, func(t *testing.T) {
			_, err := load(envMap(map[string]string{
				"HARBORMASTER_DB_INTEGRITY_CHECK": value,
			}))
			if err == nil {
				t.Fatalf("%q was accepted; the vocabulary must be closed", value)
			}
			if !strings.Contains(err.Error(), "DB_INTEGRITY_CHECK") {
				t.Errorf("the error must name the offending variable: %v", err)
			}
			// And never its value, which is the rule for every config error:
			// an offending value can itself be sensitive.
			if strings.Contains(err.Error(), value) {
				t.Errorf("the error echoed the value %q; config errors name variables, not values", value)
			}
		})
	}
}

func TestOutOfRangeBusyTimeoutIsRejected(t *testing.T) {
	for name, value := range map[string]string{
		"too small": "1ms",
		"too large": "1h",
		"zero":      "0s",
		"negative":  "-5s",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(envMap(map[string]string{
				"HARBORMASTER_DB_BUSY_TIMEOUT": value,
			})); err == nil {
				t.Errorf("%s (%s) was accepted", name, value)
			}
		})
	}
}

// Zero would mean an unbounded check on the startup path, which is the hang
// this timeout exists to prevent.
func TestNonPositiveIntegrityTimeoutIsRejected(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		if _, err := load(envMap(map[string]string{
			"HARBORMASTER_DB_INTEGRITY_TIMEOUT": value,
		})); err == nil {
			t.Errorf("%q was accepted; an unbounded startup check is the hang this prevents", value)
		}
	}
}

func TestMalformedStoreDurationsAreRejected(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"busy timeout":      {"HARBORMASTER_DB_BUSY_TIMEOUT": "soon"},
		"integrity timeout": {"HARBORMASTER_DB_INTEGRITY_TIMEOUT": "a while"},
		"require wal":       {"HARBORMASTER_DB_REQUIRE_WAL": "maybe"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(envMap(values)); err == nil {
				t.Error("a malformed value was accepted")
			}
		})
	}
}

// The integrity vocabulary is written out in two packages -- here, so that
// config stays a leaf, and in internal/store, which does the work. They must
// agree, or a value the loader accepts would be rejected at open.
func TestIntegrityVocabularyMatchesTheStorePackage(t *testing.T) {
	for _, value := range validIntegrityChecks {
		if !store.ValidIntegrityMode(store.IntegrityMode(value)) {
			t.Errorf("config accepts %q but internal/store does not; the two vocabularies have drifted",
				value)
		}
	}

	// And in the other direction: a mode the store gained must be offered here
	// too, or it would be unreachable from configuration.
	for _, mode := range []store.IntegrityMode{
		store.IntegrityOff, store.IntegrityQuick, store.IntegrityFull,
	} {
		var found bool
		for _, value := range validIntegrityChecks {
			if value == string(mode) {
				found = true
			}
		}
		if !found {
			t.Errorf("internal/store offers %q but configuration does not expose it", mode)
		}
	}
}

// The redacted String must not start naming storage settings.
func TestConfigStringStillRedactsEverything(t *testing.T) {
	cfg, err := load(envMap(map[string]string{
		"HARBORMASTER_DB_PATH": "/very/specific/path/harbormaster.db",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Contains(cfg.String(), "/very/specific/path") {
		t.Error("Config.String leaked a configured value")
	}
}
