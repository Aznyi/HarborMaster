package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// -------------------------------------------------------------- permissions --

// TestRolePermissionMatrix pins exactly what each role holds.
//
// The whole authorization model reduces to this table. Everything else --
// middleware, route policies, the UI's role-aware controls -- reads from it, so
// a quiet edit here would change who can stop a container without touching
// anything that looks like a security file.
func TestRolePermissionMatrix(t *testing.T) {
	viewerReads := []domain.Permission{
		domain.PermInventoryRead, domain.PermEventRead, domain.PermSnapshotRead,
		domain.PermDriftRead, domain.PermPolicyRead, domain.PermPlanRead,
		domain.PermAcquisitionRead, domain.PermExecutionRead,
	}

	// A viewer reads and does nothing else.
	for _, permission := range viewerReads {
		if !domain.RoleViewer.Can(permission) {
			t.Errorf("viewer cannot %q, but every read is a viewer permission", permission)
		}
	}
	for _, permission := range []domain.Permission{
		domain.PermInventoryRefresh, domain.PermSnapshotCreate, domain.PermDriftAnnotate,
		domain.PermAcquisitionCreate, domain.PermExecutionCreate,
		domain.PermPolicyManage, domain.PermUserManage, domain.PermAuditRead,
	} {
		if domain.RoleViewer.Can(permission) {
			t.Errorf("viewer holds %q; a viewer must not be able to change anything", permission)
		}
	}

	// An operator acts, but cannot change the rules or the accounts.
	for _, permission := range append(viewerReads,
		domain.PermInventoryRefresh, domain.PermSnapshotCreate, domain.PermDriftAnnotate,
		domain.PermPolicyEvaluate, domain.PermPolicyAnnotate, domain.PermImageRefresh,
		domain.PermPlanGenerate, domain.PermAcquisitionCreate, domain.PermAcquisitionCancel,
		domain.PermExecutionCreate, domain.PermExecutionCancel) {
		if !domain.RoleOperator.Can(permission) {
			t.Errorf("operator cannot %q, but it is an operational action", permission)
		}
	}
	for _, permission := range []domain.Permission{
		domain.PermPolicyManage, domain.PermUserManage,
		domain.PermAuditRead,
	} {
		if domain.RoleOperator.Can(permission) {
			t.Errorf("operator holds %q; a policy is what BLOCKS an operator's action, "+
				"so editing one must be an administrator's job", permission)
		}
	}

	// An administrator holds everything.
	for _, permission := range domain.RoleOperator.Permissions() {
		if !domain.RoleAdministrator.Can(permission) {
			t.Errorf("administrator lacks the operator permission %q", permission)
		}
	}
	for _, permission := range []domain.Permission{
		domain.PermPolicyManage, domain.PermUserManage,
		domain.PermAuditRead,
	} {
		if !domain.RoleAdministrator.Can(permission) {
			t.Errorf("administrator lacks %q", permission)
		}
	}
}

// TestAnUnknownRoleHoldsNothing is the default-deny property at the model
// level.
//
// A role read back from a database row, a request body, or a future migration
// that this build does not know must grant nothing at all. The alternative --
// treating an unrecognised role as a viewer, say -- turns a corrupt row into a
// silent grant.
func TestAnUnknownRoleHoldsNothing(t *testing.T) {
	for _, role := range []domain.Role{"", "root", "superuser", "Administrator", "admin"} {
		for _, permission := range domain.RoleAdministrator.Permissions() {
			if role.Can(permission) {
				t.Errorf("unknown role %q holds %q; an unrecognised role must hold nothing",
					role, permission)
			}
		}
		if len(role.Permissions()) != 0 {
			t.Errorf("unknown role %q reports %d permissions", role, len(role.Permissions()))
		}
	}
}

// TestOnlyTheSocketTouchingPermissionsArePrivileged pins which actions get the
// loud log line.
func TestOnlyTheSocketTouchingPermissionsArePrivileged(t *testing.T) {
	privileged := map[domain.Permission]bool{
		domain.PermAcquisitionCreate: true,
		domain.PermExecutionCreate:   true,
	}

	for _, permission := range domain.RoleAdministrator.Permissions() {
		if got := permission.Privileged(); got != privileged[permission] {
			t.Errorf("permission %q privileged=%v, want %v", permission, got, privileged[permission])
		}
	}
}

// ---------------------------------------------------------------- usernames --

func TestUsernameNormalisationIsASCIIOnly(t *testing.T) {
	cases := map[string]string{
		"  Admin  ":         "admin",
		"OPERATOR":          "operator",
		"mixed.Case-name_1": "mixed.case-name_1",
		// The Turkish dotless i and the Kelvin sign must NOT fold onto ASCII:
		// unicode.ToLower would map them, producing two distinct inputs that
		// normalise to the same name.
		"İstanbul": "İstanbul",
		"Åa":       "Åa",
	}

	for input, want := range cases {
		if got := domain.NormaliseUsername(input); got != want {
			t.Errorf("NormaliseUsername(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUsernameAllowlistRefusesHomographsAndOddShapes(t *testing.T) {
	valid := []string{"admin", "op1", "a.b-c_d", "user.name", "ab1"}
	for _, username := range valid {
		if !domain.ValidUsername(username) {
			t.Errorf("ValidUsername(%q) = false, want true", username)
		}
	}

	invalid := map[string]string{
		"":                       "empty",
		"a":                      "shorter than the minimum",
		"Admin":                  "not normalised",
		"аdmin":                  "Cyrillic а renders identically to ASCII a",
		".admin":                 "leading separator",
		"_admin":                 "leading separator",
		"1admin":                 "", // digits are allowed first; see below
		"admin.":                 "trailing separator",
		"admin-":                 "trailing separator",
		"ad min":                 "space",
		"ad\tmin":                "tab",
		"admin\n":                "newline",
		"admin@host":             "at sign",
		"admin/../..":            "path traversal shape",
		strings.Repeat("a", 200): "longer than the maximum",
	}
	for username, reason := range invalid {
		if username == "1admin" {
			// Documented as allowed: an alphanumeric first character includes
			// a digit. Asserted here so the exception is deliberate.
			if !domain.ValidUsername(username) {
				t.Errorf("ValidUsername(%q) = false; a digit is an allowed first character", username)
			}
			continue
		}
		if domain.ValidUsername(username) {
			t.Errorf("ValidUsername(%q) = true, want false (%s)", username, reason)
		}
	}
}

// ---------------------------------------------------------------- passwords --

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		username string
		want     domain.PasswordProblem
	}{
		{"a good passphrase", "correct horse battery staple", "admin", domain.PasswordOK},
		{"exactly the minimum", "zx8k3mq2wp7v", "admin", domain.PasswordOK},
		{"one short", "zx8k3mq2wp7", "admin", domain.PasswordTooShort},
		{"empty", "", "admin", domain.PasswordTooShort},
		{"beyond the maximum", strings.Repeat("a", 2000), "admin", domain.PasswordTooLong},
		{"a refused password", "password1234", "admin", domain.PasswordTooCommon},
		{"a refused password in another case", "PassWord1234", "admin", domain.PasswordTooCommon},
		{"an ascending run", "abcdefghijkl", "admin", domain.PasswordTooUniform},
		{"an embedded newline", "good passphrase\nhere", "admin", domain.PasswordNotPrintable},
		{"contains the username", "admin-is-the-name", "admin", domain.PasswordContainsUsername},
		{"contains the username in another case", "MyAdminPassword", "admin",
			domain.PasswordContainsUsername},
		{"one character repeated", strings.Repeat("a", 30), "admin", domain.PasswordTooUniform},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := domain.CheckPassword(testCase.password, testCase.username)
			if got != testCase.want {
				t.Errorf("CheckPassword(%q, %q) = %q, want %q\n\texplanation: %s",
					testCase.password, testCase.username, got, testCase.want, got.Explain())
			}
			if got != domain.PasswordOK && got.Explain() == "" {
				t.Error("a rejection with no explanation leaves the operator guessing")
			}
		})
	}
}

// TestThePasswordPolicyNeverEchoesThePassword is the leak check on the one
// message that has the password in scope.
func TestThePasswordPolicyNeverEchoesThePassword(t *testing.T) {
	const password = "hunter2-hunter2-hunter2"

	problem := domain.CheckPassword(password, "admin")
	if problem == domain.PasswordOK {
		// Force a rejection so there is a message to inspect.
		problem = domain.CheckPassword("short", "admin")
	}
	if strings.Contains(problem.Explain(), password) {
		t.Error("the password policy explanation contains the password")
	}
	for _, candidate := range []domain.PasswordProblem{
		domain.PasswordTooShort, domain.PasswordTooLong, domain.PasswordTooCommon,
		domain.PasswordContainsUsername, domain.PasswordTooUniform,
	} {
		if strings.Contains(candidate.Explain(), password) {
			t.Errorf("%q echoes the password", candidate)
		}
	}
}

// ----------------------------------------------------------------- sessions --

// TestASessionNeverSerialisesItsDigest is the leak check on the type an
// operator's session list is built from.
//
// The digest is not the token and cannot be replayed, but it is the exact value
// a lookup compares against -- so publishing it hands an attacker with database
// read access a way to confirm a guess offline.
func TestASessionNeverSerialisesItsDigest(t *testing.T) {
	session := domain.Session{
		SessionID:   "sess-1",
		UserID:      "user-1",
		TokenDigest: "THE-DIGEST-VALUE",
	}

	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if strings.Contains(string(encoded), "THE-DIGEST-VALUE") {
		t.Errorf("the session JSON contains its token digest: %s", encoded)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "digest") {
		t.Errorf("the session JSON names a digest field: %s", encoded)
	}
}

func TestSessionTokensAreLongAndDistinct(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 256; i++ {
		token, err := domain.NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken: %v", err)
		}
		if !domain.ValidSessionToken(token) {
			t.Fatalf("a freshly minted token %q fails its own shape check", token)
		}
		if seen[token] {
			t.Fatalf("token %q was issued twice in %d draws", token, i)
		}
		seen[token] = true
	}
}

func TestSessionTokenShapeIsRefusedForAnythingElse(t *testing.T) {
	for _, token := range []string{
		"", "short", strings.Repeat("a", 42), strings.Repeat("a", 44),
		strings.Repeat("a", 42) + "+", strings.Repeat("a", 42) + "/",
		strings.Repeat("a", 42) + "=",
	} {
		if domain.ValidSessionToken(token) {
			t.Errorf("ValidSessionToken(%q) = true; only a 43-character base64url "+
				"token has the right shape", token)
		}
	}
}

// -------------------------------------------------------------------- audit --

func TestClientAddressNormalisationDropsThePort(t *testing.T) {
	cases := map[string]string{
		"192.0.2.10:54321":  "192.0.2.10",
		"192.0.2.10":        "192.0.2.10",
		"[2001:db8::1]:443": "2001:db8::1",
		"2001:db8::1":       "2001:db8::1",
		// A canonical form, so the same client throttles as one address.
		"[2001:DB8:0:0:0:0:0:1]:443": "2001:db8::1",
		"":                           "unknown",
		"   ":                        "unknown",
		"not an address":             "unknown",
	}

	for input, want := range cases {
		if got := domain.NormaliseClientAddr(input); got != want {
			t.Errorf("NormaliseClientAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestEveryAuditActionIsRecognised guards against a constant that the
// validator does not know, which would make an event unwritable at runtime
// rather than at build time.
func TestEveryAuditActionIsRecognised(t *testing.T) {
	actions := domain.AuditActions
	if len(actions) == 0 {
		t.Fatal("no audit actions are declared")
	}

	seen := map[domain.AuditAction]bool{}
	for _, action := range actions {
		if seen[action] {
			t.Errorf("audit action %q is listed twice", action)
		}
		seen[action] = true

		if !domain.ValidAuditAction(string(action)) {
			t.Errorf("audit action %q is not recognised by its own validator", action)
		}
		if strings.TrimSpace(string(action)) == "" {
			t.Error("an audit action is blank")
		}
	}

	if domain.ValidAuditAction("something.invented") {
		t.Error("an unknown audit action was accepted; the set must be an allowlist")
	}
}
