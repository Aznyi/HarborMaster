package domain

import "testing"

// TestDefaultMaskerClassifiesCredentialBearingNames covers the category the
// Phase 1 pattern list missed entirely: variables whose NAME describes a
// destination while their VALUE embeds a credential.
//
// DATABASE_URL=postgres://user:hunter2@db/app is one of the most common
// secret-bearing variables in existence and matched none of the original
// patterns.
func TestDefaultMaskerClassifiesCredentialBearingNames(t *testing.T) {
	m := NewDefaultMasker()
	for _, name := range []string{
		"DATABASE_URL", "DB_URL", "DSN", "CONNECTION_STRING", "SMTP_URL",
		"WEBHOOK_URL", "CREDENTIALS_JSON", "SERVICE_ACCOUNT",
		"SERVICE_ACCOUNT_JSON", "Database_Url", "dbUrl", "DB-URL", "db.url",
		"DB/URL", "MY_PASSPHRASE", "TLS_CERT", "SIGNING_KEY", "SESSION_KEY",
	} {
		if !m.IsSensitive(name) {
			t.Errorf("IsSensitive(%q) = false, want true", name)
		}
	}
}

// TestDefaultMaskerLeavesOrdinaryNamesAlone guards the other direction. The
// bias is towards over-masking, but a masker that classifies everything would
// make the inventory useless without anyone noticing.
func TestDefaultMaskerLeavesOrdinaryNamesAlone(t *testing.T) {
	m := NewDefaultMasker()
	for _, name := range []string{"PATH", "HOME", "LANG", "TZ", "NODE_ENV", "PORT"} {
		if m.IsSensitive(name) {
			t.Errorf("IsSensitive(%q) = true, want false", name)
		}
	}
}

// TestExistingDefaultPatternsStillClassify pins the Phase 1/2 behaviour. An
// upgrade that silently unmasked a value that used to be masked would be a
// security regression delivered as a feature.
func TestExistingDefaultPatternsStillClassify(t *testing.T) {
	m := NewDefaultMasker()
	for _, name := range []string{
		"PASSWORD", "PASSWD", "MY_SECRET", "API_TOKEN", "API_KEY", "APIKEY",
		"PRIVATE_KEY", "AWS_CREDENTIAL", "AUTHORIZATION",
	} {
		if !m.IsSensitive(name) {
			t.Errorf("IsSensitive(%q) = false, want true", name)
		}
	}
}

// TestMaskAllMakesEveryNameSensitive covers the mode for operators who cannot
// enumerate their variables and need the guarantee rather than the heuristic.
func TestMaskAllMakesEveryNameSensitive(t *testing.T) {
	m := NewMaskerWithMode(nil, MaskModeAll)
	for _, name := range []string{"PATH", "PORT", "ANYTHING", ""} {
		if !m.IsSensitive(name) {
			t.Errorf("all-sensitive: IsSensitive(%q) = false, want true", name)
		}
	}
}

// TestExtraPatternsAreAdditive: an operator adding a pattern must not silently
// lose the defaults. Configuration should not be able to reduce protection.
func TestExtraPatternsAreAdditive(t *testing.T) {
	m := NewMaskerWithExtra([]string{"TENANT"})
	if !m.IsSensitive("TENANT_ID") {
		t.Error("extra pattern TENANT did not classify TENANT_ID")
	}
	if !m.IsSensitive("PASSWORD") {
		t.Error("extra patterns must not displace the defaults")
	}
}

// TestOverrideReplacesDefaults is the one configuration that CAN reduce
// protection, which is why it is separate from the additive form and why the
// loader warns about it.
func TestOverrideReplacesDefaults(t *testing.T) {
	m := NewMaskerWithMode([]string{"TENANT"}, MaskModeDefault)
	if !m.IsSensitive("TENANT_ID") {
		t.Error("override pattern did not classify")
	}
	if m.IsSensitive("PASSWORD") {
		t.Error("override should have replaced the defaults")
	}
}

// TestNormalisationFoldsSeparators: the same variable written five ways is the
// same variable.
func TestNormalisationFoldsSeparators(t *testing.T) {
	m := NewMasker([]string{"ACCESS_KEY"})
	for _, name := range []string{
		"ACCESS_KEY", "access-key", "Access.Key", "ACCESS/KEY", "accesskey",
		"AWS_ACCESS_KEY_ID",
	} {
		if !m.IsSensitive(name) {
			t.Errorf("IsSensitive(%q) = false, want true", name)
		}
	}
}

// TestClassifyMasksSensitiveValues verifies the display value is replaced while
// RawValue keeps the real one for in-memory use.
func TestClassifyMasksSensitiveValues(t *testing.T) {
	m := NewDefaultMasker()

	sensitive := m.Classify("DB_PASSWORD", "hunter2")
	if sensitive.Value != MaskedValue {
		t.Errorf("Value = %q, want %q", sensitive.Value, MaskedValue)
	}
	if sensitive.RawValue != "hunter2" {
		t.Errorf("RawValue = %q, want the real value", sensitive.RawValue)
	}
	if !sensitive.Sensitive() {
		t.Error("Sensitive() = false for DB_PASSWORD")
	}

	normal := m.Classify("PORT", "8080")
	if normal.Value != "8080" {
		t.Errorf("Value = %q, want 8080", normal.Value)
	}
	if normal.Sensitive() {
		t.Error("Sensitive() = true for PORT")
	}
}

// TestNilMaskerClassifiesNothing: a nil Masker must not panic. It is reachable
// from a partially constructed service.
func TestNilMaskerClassifiesNothing(t *testing.T) {
	var m *Masker
	if m.IsSensitive("PASSWORD") {
		t.Error("nil Masker reported a name as sensitive")
	}
}

func TestValidMaskMode(t *testing.T) {
	for _, mode := range []string{"default", "all-sensitive"} {
		if !ValidMaskMode(mode) {
			t.Errorf("ValidMaskMode(%q) = false, want true", mode)
		}
	}
	for _, mode := range []string{"", "off", "none", "ALL-SENSITIVE"} {
		if ValidMaskMode(mode) {
			t.Errorf("ValidMaskMode(%q) = true, want false", mode)
		}
	}
}
