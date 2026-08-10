package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The console account listing.
//
// `harbormaster admin list-users` exists because recovering an account needs a
// username and there was no way to discover one: release validation had to copy
// the SQLite database off the host and read it — carrying every Argon2id
// verifier and every live session digest out of the installation to learn a
// string.
//
// These assert the two properties that make the listing a smaller thing to hand
// somebody than the database it replaces: it answers the question, and it
// answers only that question.

// TestListAccountsNamesEveryAccount is the reason the command exists.
func TestListAccountsNamesEveryAccount(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	harness.claim(t, "hm-admin", "a decent passphrase")
	if _, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "watcher", Role: domain.RoleViewer, Password: "another fine phrase",
	}); err != nil {
		t.Fatalf("create viewer: %v", err)
	}

	accounts, err := harness.local.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}

	found := map[string]bool{}
	for _, account := range accounts {
		found[account.Username] = true
	}
	for _, want := range []string{"hm-admin", "watcher"} {
		if !found[want] {
			t.Errorf("the listing omits %q; an operator cannot recover an account "+
				"whose name they cannot discover", want)
		}
	}
}

// TestListAccountsReportsTheStateRecoveryNeeds.
//
// The three facts beyond the name, and why each is there: a role says what the
// account can do, a status says whether it can authenticate at all, and an
// outstanding password change is the state a PREVIOUS recovery leaves behind —
// without it an operator cannot tell a working account from one nobody has
// finished setting up.
func TestListAccountsReportsTheStateRecoveryNeeds(t *testing.T) {
	harness := newAuthHarness(t)
	ctx := context.Background()

	harness.claim(t, "hm-admin", "a decent passphrase")

	// A generated password: temporary until its holder replaces it.
	if _, err := harness.local.ResetPassword(ctx, "hm-admin", "a replacement phrase", true); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// A disabled account, which must be visibly disabled.
	retired, err := harness.users.Create(ctx, service.Actor{}, service.CreateUserRequest{
		Username: "retired", Role: domain.RoleViewer, Password: "another fine phrase",
	})
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if _, err := harness.users.SetStatus(ctx, service.Actor{},
		retired.User.UserID, domain.UserDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}

	accounts, err := harness.local.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}

	seen := 0
	for _, account := range accounts {
		switch account.Username {
		case "hm-admin":
			seen++
			if account.Role != domain.RoleAdministrator {
				t.Errorf("hm-admin role = %q, want administrator", account.Role)
			}
			if !account.MustChangePassword {
				t.Error("hm-admin does not report an outstanding password change, " +
					"so a half-finished recovery is invisible to the operator")
			}
		case "retired":
			seen++
			if account.Status != domain.UserDisabled {
				t.Errorf("retired status = %q, want disabled", account.Status)
			}
		}
	}
	if seen != 2 {
		t.Errorf("matched %d of the 2 seeded accounts", seen)
	}
}

// TestTheListingCarriesNoCredentialMaterial serialises the whole answer and
// searches it for anything that should never have been in it.
//
// The type has nowhere to put a secret and an architecture test pins its field
// set; this is the belt to that braces, asserted against real rows rather than
// against the declaration. The password below is a canary: if it, its verifier,
// or anything shaped like a digest reaches the listing, this fails.
func TestTheListingCarriesNoCredentialMaterial(t *testing.T) {
	const canary = "canary passphrase 8f3k2m9v"

	harness := newAuthHarness(t)
	ctx := context.Background()

	harness.claim(t, "hm-admin", canary)

	// A live session, so its digest exists in the database while this runs.
	if _, _, err := harness.auth.Login(ctx, service.LoginRequest{
		Username: "hm-admin", Password: canary, ClientAddr: "192.0.2.10",
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	accounts, err := harness.local.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) == 0 {
		t.Fatal("no accounts listed; the sweep below would prove nothing")
	}

	encoded, err := json.Marshal(accounts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rendered := string(encoded)

	// The positive control: the listing really does contain the account, so a
	// clean sweep means "no secret" rather than "nothing was searched".
	if !strings.Contains(rendered, "hm-admin") {
		t.Fatal("the listing does not contain the account it was asked about")
	}

	if strings.Contains(rendered, canary) {
		t.Fatal("the listing carries the plaintext password")
	}
	// Argon2id verifiers are stored in PHC form; a session is identified by a
	// keyed digest. Neither belongs in an account listing.
	for _, marker := range []string{"$argon2", "$2a$", "pbkdf2", "sha256:", "ses_"} {
		if strings.Contains(rendered, marker) {
			t.Errorf("the listing carries %q, which is credential or session material", marker)
		}
	}

	// And nothing shaped like a digest, whatever it is called.
	for _, account := range accounts {
		for _, field := range []string{
			account.Username, string(account.Role), string(account.Status),
		} {
			if len(field) >= 40 && !strings.Contains(field, " ") {
				t.Errorf("field %q is %d characters with no spaces, which is the "+
					"shape of a digest rather than of a name, a role, or a status",
					field, len(field))
			}
		}
	}
}
