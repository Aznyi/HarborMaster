package config

import (
	"strings"
	"testing"
	"time"
)

// snapshotEnv builds a lookupFunc over a fixed map.
func snapshotEnv(vars map[string]string) lookupFunc {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func emptyEnv() lookupFunc {
	return func(string) (string, bool) { return "", false }
}

func TestSnapshotDefaults(t *testing.T) {
	cfg, err := load(emptyEnv())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	s := cfg.Snapshots
	if !s.Enabled {
		t.Error("snapshots should default to enabled")
	}
	if s.MaskMode != string(defaultMaskMode) {
		t.Errorf("MaskMode = %q, want %q", s.MaskMode, defaultMaskMode)
	}
	if s.MaxInventoryAge != DefaultSnapshotMaxInventoryAge {
		t.Errorf("MaxInventoryAge = %v, want %v", s.MaxInventoryAge, DefaultSnapshotMaxInventoryAge)
	}
	if s.MaxConcurrentDiffs != DefaultSnapshotMaxConcurrentDiffs {
		t.Errorf("MaxConcurrentDiffs = %d, want %d", s.MaxConcurrentDiffs, DefaultSnapshotMaxConcurrentDiffs)
	}
	if s.DiffTimeout != DefaultSnapshotDiffTimeout {
		t.Errorf("DiffTimeout = %v, want %v", s.DiffTimeout, DefaultSnapshotDiffTimeout)
	}
	if s.MaxDiffEntries != DefaultSnapshotMaxDiffEntries {
		t.Errorf("MaxDiffEntries = %d, want %d", s.MaxDiffEntries, DefaultSnapshotMaxDiffEntries)
	}
	if s.RetentionCount != DefaultSnapshotRetentionCount {
		t.Errorf("RetentionCount = %d, want %d", s.RetentionCount, DefaultSnapshotRetentionCount)
	}
	if s.MaxReasonBytes != DefaultSnapshotMaxReasonBytes {
		t.Errorf("MaxReasonBytes = %d, want %d", s.MaxReasonBytes, DefaultSnapshotMaxReasonBytes)
	}
	if s.WriteRateBurst != DefaultWriteRateBurst {
		t.Errorf("WriteRateBurst = %d, want %d", s.WriteRateBurst, DefaultWriteRateBurst)
	}
}

func TestSnapshotRejectsUnknownMaskMode(t *testing.T) {
	_, err := load(snapshotEnv(map[string]string{
		envPrefix + "MASK_MODE": "off",
	}))
	if err == nil {
		t.Fatal("expected an unknown mask mode to be rejected")
	}
	if !strings.Contains(err.Error(), "MASK_MODE") {
		t.Errorf("error should name the setting: %v", err)
	}
}

func TestSnapshotAcceptsAllSensitiveMode(t *testing.T) {
	cfg, err := load(snapshotEnv(map[string]string{
		envPrefix + "MASK_MODE": "all-sensitive",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Masker().IsSensitive("PORT") {
		t.Error("all-sensitive mode did not reach the constructed Masker")
	}
}

func TestSnapshotRejectsNegativeRetention(t *testing.T) {
	for _, name := range []string{
		"SNAPSHOT_RETENTION_COUNT",
		"SNAPSHOT_PRUNE_BATCH",
		"SNAPSHOT_MAX_CONCURRENT_DIFFS",
	} {
		if _, err := load(snapshotEnv(map[string]string{envPrefix + name: "-1"})); err == nil {
			t.Errorf("%s: expected a negative value to be rejected", name)
		}
	}
}

func TestSnapshotRejectsNegativeAge(t *testing.T) {
	if _, err := load(snapshotEnv(map[string]string{
		envPrefix + "SNAPSHOT_RETENTION_AGE": "-1h",
	})); err == nil {
		t.Fatal("expected a negative retention age to be rejected")
	}
}

func TestSnapshotRejectsNonPositiveDiffTimeout(t *testing.T) {
	if _, err := load(snapshotEnv(map[string]string{
		envPrefix + "SNAPSHOT_DIFF_TIMEOUT": "0s",
	})); err == nil {
		t.Fatal("expected a zero diff timeout to be rejected; an unbounded diff is a DoS surface")
	}
}

// Zero on both retention dimensions is a valid, documented "keep everything".
func TestSnapshotZeroRetentionIsAccepted(t *testing.T) {
	cfg, err := load(snapshotEnv(map[string]string{
		envPrefix + "SNAPSHOT_RETENTION_COUNT": "0",
		envPrefix + "SNAPSHOT_RETENTION_AGE":   "0s",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Snapshots.RetentionCount != 0 || cfg.Snapshots.RetentionAge != 0 {
		t.Error("zero retention should be preserved, not replaced by defaults")
	}
}

func TestMaskPatternsExtraIsAdditive(t *testing.T) {
	cfg, err := load(snapshotEnv(map[string]string{
		envPrefix + "MASK_PATTERNS_EXTRA": "TENANT,REALM",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Masker()
	if !m.IsSensitive("TENANT_ID") {
		t.Error("extra pattern did not take effect")
	}
	if !m.IsSensitive("PASSWORD") {
		t.Error("extra patterns must not displace the defaults")
	}
}

func TestMaskPatternsOverrideReplacesDefaults(t *testing.T) {
	cfg, err := load(snapshotEnv(map[string]string{
		envPrefix + "MASK_PATTERNS_OVERRIDE": "TENANT",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Masker()
	if !m.IsSensitive("TENANT_ID") {
		t.Error("override pattern did not take effect")
	}
	if m.IsSensitive("PASSWORD") {
		t.Error("override should have replaced the defaults")
	}
}

// The HMAC key must never appear in the redacted config rendering, which is the
// one rendering allowed into a log record.
func TestConfigStringNeverContainsTheHMACKey(t *testing.T) {
	cfg, err := load(snapshotEnv(map[string]string{
		envPrefix + "SNAPSHOT_HMAC_KEY": strings.Repeat("ab", 32),
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Contains(cfg.String(), "ab") {
		t.Errorf("Config.String leaked key material: %s", cfg.String())
	}
}

func TestSnapshotValidationRunsEvenWhenDisabled(t *testing.T) {
	// A configuration error that only surfaces the day someone enables the
	// feature is a worse failure than one caught at startup.
	if _, err := load(snapshotEnv(map[string]string{
		envPrefix + "SNAPSHOTS_ENABLED":        "false",
		envPrefix + "SNAPSHOT_RETENTION_COUNT": "-5",
	})); err == nil {
		t.Fatal("expected validation to run with snapshots disabled")
	}
}

func TestSnapshotDurationsParse(t *testing.T) {
	cfg, err := load(snapshotEnv(map[string]string{
		envPrefix + "SNAPSHOT_READINESS_MAX_INVENTORY_AGE": "45m",
		envPrefix + "SNAPSHOT_PRUNE_INTERVAL":              "2h",
		envPrefix + "SNAPSHOT_DIFF_TIMEOUT":                "3s",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Snapshots.MaxInventoryAge != 45*time.Minute {
		t.Errorf("MaxInventoryAge = %v, want 45m", cfg.Snapshots.MaxInventoryAge)
	}
	if cfg.Snapshots.PruneInterval != 2*time.Hour {
		t.Errorf("PruneInterval = %v, want 2h", cfg.Snapshots.PruneInterval)
	}
	if cfg.Snapshots.DiffTimeout != 3*time.Second {
		t.Errorf("DiffTimeout = %v, want 3s", cfg.Snapshots.DiffTimeout)
	}
}
