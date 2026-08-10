package arch_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/service"
)

// Architecture tests for identity and credentials.
//
// The route-level rules -- default deny, the public surface, every write
// audited, no role check in a handler -- are in internal/api, because they are
// about unexported structure a test outside the package could only grep for.
//
// These are the rules that are genuinely about which package may name what.

// credentialPackages are the packages permitted to handle password material.
//
// internal/store persists a verifier. internal/service produces and checks one.
// Nothing else needs to, and every other package that COULD would be a new
// place for a password to be logged, serialised, or compared with ==.
var credentialPackages = map[string]bool{
	"internal/store":   true,
	"internal/service": true,
}

// credentialSymbols are the identifiers that carry password material.
var credentialSymbols = []string{
	"store.Credential",
	"store.PreparedCredential",
	"golang.org/x/crypto/argon2",
}

// TestCredentialMaterialIsConfinedToStoreAndService fails if a password
// verifier reaches a package that should never see one.
//
// The API is the package that matters here. A handler holding a
// store.Credential is one struct-literal away from a JSON response containing a
// salt and a hash, and one log line away from the same. It cannot happen if the
// type never arrives.
func TestCredentialMaterialIsConfinedToStoreAndService(t *testing.T) {
	for _, file := range goFiles(t) {
		dir := filepath.ToSlash(filepath.Dir(file.rel))
		if credentialPackages[dir] || strings.HasSuffix(file.rel, "_test.go") {
			continue
		}
		// The console commands construct a hasher, which is the supported way
		// to set a password from the host. They never hold a Credential.
		if dir == "cmd/harbormaster" {
			continue
		}

		for _, symbol := range credentialSymbols {
			if !strings.Contains(file.text, symbol) {
				continue
			}
			t.Errorf("%s names %s\n"+
				"\tpassword material lives in internal/store and internal/service; a package "+
				"that can hold a verifier is a package that can serialise or log one",
				file.rel, symbol)
		}
	}
}

// TestLocalAdminIsNotReachableFromTheAPI fails if the HTTP layer can see the
// console recovery service.
//
// service.LocalAdmin claims an installation without the one-time token and
// replaces a password without knowing the old one. Both are correct for
// somebody holding the database file and catastrophic over a network. The api
// package depends on the AuthService and UserService interfaces, neither of
// which declares those methods -- and this test is what stops a future change
// from reaching past the interface to the concrete type.
func TestLocalAdminIsNotReachableFromTheAPI(t *testing.T) {
	for _, file := range goFiles(t) {
		dir := filepath.ToSlash(filepath.Dir(file.rel))
		if dir != "internal/api" {
			continue
		}
		for _, symbol := range []string{"LocalAdmin", "NewLocalAdmin"} {
			if !strings.Contains(file.text, symbol) {
				continue
			}
			t.Errorf("%s names service.%s\n"+
				"\tthe console recovery path takes filesystem access as its authority and "+
				"must never be reachable over HTTP",
				file.rel, symbol)
		}
	}
}

// TestAccountListingIsNotReachableOverHTTP fails if the account listing added
// for console recovery grows an HTTP caller.
//
// `harbormaster admin list-users` exists because recovering an account needs a
// username and there was no way to discover one -- release validation had to
// copy the database off the host to read a string. The listing is deliberately
// unauthenticated-by-filesystem-access rather than by a session, which is
// correct for somebody holding the database file and exactly wrong over a
// network: an installation's account list is the first thing a scrape wants,
// and the authenticated case is already served by the user-administration
// endpoints under their own authorization.
//
// Checked separately from TestLocalAdminIsNotReachableFromTheAPI because a
// future refactor could move the summary type somewhere the api package may
// legitimately import while leaving LocalAdmin behind.
func TestAccountListingIsNotReachableOverHTTP(t *testing.T) {
	for _, file := range goFiles(t) {
		dir := filepath.ToSlash(filepath.Dir(file.rel))
		if dir != "internal/api" {
			continue
		}
		for _, symbol := range []string{"ListAccounts", "AccountSummary", "list-users"} {
			if !strings.Contains(file.text, symbol) {
				continue
			}
			t.Errorf("%s names %q\n"+
				"\tthe console account listing takes filesystem access as its authority; "+
				"an HTTP route to it would be a second, weaker way to ask who exists",
				file.rel, symbol)
		}
	}
}

// TestTheAccountSummaryCannotCarryCredentialMaterial pins the shape of what the
// console may print about an account.
//
// The type exists to have nowhere to put a secret. A field added here would be
// printed by `admin list-users` on the next run without anybody deciding it
// should be, which is how a verifier, a session digest, or a password timestamp
// ends up on a terminal and in a scrollback.
//
// Reflection rather than source scanning: the question is what the type CAN
// hold, and that is a property of the type rather than of the file it is
// written in.
func TestTheAccountSummaryCannotCarryCredentialMaterial(t *testing.T) {
	permitted := map[string]string{
		"Username":           "string",
		"Role":               "domain.Role",
		"Status":             "domain.UserStatus",
		"MustChangePassword": "bool",
	}

	summary := reflect.TypeOf(service.AccountSummary{})
	if summary.NumField() != len(permitted) {
		t.Errorf("service.AccountSummary has %d fields, want %d\n"+
			"\tevery field here is printed to a console by `admin list-users`",
			summary.NumField(), len(permitted))
	}

	for i := range summary.NumField() {
		field := summary.Field(i)
		want, allowed := permitted[field.Name]
		if !allowed {
			t.Errorf("service.AccountSummary.%s is not in the permitted set\n"+
				"\taccount recovery needs a username, a role, a status, and whether a "+
				"password change is outstanding. Anything else is a fact about "+
				"credentials, behaviour, or structure that recovery does not need",
				field.Name)
			continue
		}
		if got := field.Type.String(); got != want {
			t.Errorf("service.AccountSummary.%s is %s, want %s", field.Name, got, want)
		}
	}

	// The names that must never appear, whatever the field set becomes.
	forbidden := []string{
		"Password", "Hash", "Verifier", "Credential", "Secret", "Token",
		"Digest", "Session", "Key", "Salt", "CSRF",
	}
	for i := range summary.NumField() {
		name := summary.Field(i).Name
		for _, banned := range forbidden {
			if strings.Contains(name, banned) && name != "MustChangePassword" {
				t.Errorf("service.AccountSummary.%s names %q; the console listing "+
					"must carry no credential or session material", name, banned)
			}
		}
	}
}

// TestSessionAndBootstrapTokensAreNeverPersistedRaw fails if a repository
// stores a token column that is not a digest.
//
// The property is that a stolen database yields no usable session. It rests on
// the schema: the sessions table has token_digest and no token, and auth_state
// has bootstrap_token_digest and no token. A column named otherwise would break
// it silently, because everything else would keep working.
func TestSessionAndBootstrapTokensAreNeverPersistedRaw(t *testing.T) {
	root := moduleRoot(t)
	migrations := filepath.Join(root, "internal", "store", "migrations")

	entries, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	// Column NAMES that would hold a raw secret.
	//
	// Matched as the first word of a column definition rather than as a
	// substring anywhere in the file, so "token_digest TEXT" is accepted and
	// "token TEXT" is not -- the whole point being the distinction between
	// storing a secret and storing a digest of one.
	forbidden := map[string]bool{
		"token": true, "session_token": true, "bootstrap_token": true,
		"csrf_token": true, "password": true, "secret": true,
		"password_hash": true, "raw_token": true,
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(migrations, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}

		for _, line := range strings.Split(string(source), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			name, rest, found := strings.Cut(trimmed, " ")
			if !found || rest == "" {
				continue
			}
			if !forbidden[strings.ToLower(name)] {
				continue
			}
			t.Errorf("migration %s declares a column %q\n"+
				"\tHarborMaster stores keyed digests, never the secret: a stolen database "+
				"must not yield a usable session, token, or password",
				entry.Name(), name)
		}
	}
}

// TestAuditColumnsCannotHoldARequestBody fails if the audit schema grows a
// column that would invite one.
//
// An audit row records who, what, to what, from where, and whether it worked.
// It must not record the REQUEST, because a request body can contain a
// password, an environment value, or a registry credential -- and a log that
// holds those is a worse liability than no log at all.
func TestAuditColumnsCannotHoldARequestBody(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "store", "migrations", "0011_auth.sql")

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read 0011_auth.sql: %v", err)
	}
	text := strings.ToLower(string(source))

	for _, column := range []string{"request_body", "payload", "raw_request", "headers", "cookies"} {
		if strings.Contains(text, column) {
			t.Errorf("the auth schema declares %q\n"+
				"\tthe audit log records the SHAPE of an action, never its content; a "+
				"request body can carry a password or a registry credential",
				column)
		}
	}
}

// ------------------------------------------------------------- plumbing --

// aFile is one Go source file and its text.
type aFile struct {
	rel  string
	text string
}

// goFiles reads every Go file in the module.
//
// Text rather than an AST, because these rules are about a symbol appearing at
// all -- in a call, a type, a comment that would mislead a reader into thinking
// the symbol is available here. A mention in a comment is a false positive
// worth having: it means the comment is describing something the package cannot
// actually do.
func goFiles(t *testing.T) []aFile {
	t.Helper()

	root := moduleRoot(t)
	var found []aFile

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist", "data":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		found = append(found, aFile{rel: filepath.ToSlash(rel), text: string(source)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no Go files found; the walk is not reaching the source tree")
	}
	return found
}
