package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/logging"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The console recovery commands.
//
// # Why these exist
//
// Every authentication system needs an answer to "the only administrator left".
// The answers people reach for otherwise are worse: a default password, a
// permanent recovery account, or a support tool that talks to the API. All
// three are backdoors that are always present. A CLI is a backdoor that is only
// present to somebody who already has the database file -- which is to say, not
// a backdoor at all, because that person could already edit the table by hand.
//
// # Why they are never HTTP
//
// `admin bootstrap` claims an installation without the one-time token, and
// `admin reset-password` sets a password without knowing the old one. Both take
// the filesystem as their proof of authority. Exposing either over a network
// would move that proof to "can reach the port", which is precisely the
// property Phase 9.5 exists to remove.
//
// The structural guarantee is in internal/service/auth_local.go: these commands
// use service.LocalAdmin, which the api package does not depend on and an
// architecture test forbids it from naming.
//
// # Why they refuse to run against a world-readable installation
//
// The database now holds password verifiers and session digests. Recovering an
// account into a directory anybody on the host can read is recovering it for
// them too, so the command says so and stops. --force exists because an
// operator locked out of a mis-permissioned installation still needs a way in;
// making them type it is the point.

// adminCommand dispatches `harbormaster admin ...`.
func adminCommand(out io.Writer, args []string) int {
	if len(args) == 0 {
		adminUsage(os.Stderr)
		return exitUsage
	}

	switch args[0] {
	case "bootstrap":
		return adminBootstrap(out, args[1:])
	case "reset-password":
		return adminResetPassword(out, args[1:])
	case "help", "-h", "--help":
		adminUsage(out)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "admin: unknown subcommand %q\n\n", args[0])
		adminUsage(os.Stderr)
		return exitUsage
	}
}

func adminUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `HarborMaster console administration.

Usage:
  harbormaster admin bootstrap [--username NAME] [--force]
      Create the first administrator on an unclaimed installation. Refused once
      an administrator exists.

  harbormaster admin reset-password --username NAME [--generate] [--keep-password] [--force]
      Replace an account's password, reactivate it if it was disabled or locked,
      and revoke every one of its live sessions.

Options:
  --username NAME   The account. Prompted for when omitted.
  --generate        Generate a strong password and PRINT IT ONCE, instead of
                    prompting. Without this flag no password is ever printed.
  --keep-password   Do not require a password change at next login. Only valid
                    with an interactively chosen password.
  --force           Proceed even when the database or key file is readable by
                    users other than its owner.

These commands read and write the database directly and are never reachable
over HTTP. They require filesystem access to the installation, which is the
same authority needed to edit the tables by hand.

Passwords are read from the terminal without echo. They are never accepted as a
command-line argument or an environment variable: both are visible in the
process list and in shell history.
`)
}

// adminFlags is the parsed console command line.
type adminFlags struct {
	username     string
	generate     bool
	keepPassword bool
	force        bool
}

// parseAdminFlags reads the console flags.
//
// Hand-parsed rather than via the flag package because the whole point is a
// tiny, explicit allowlist: an unrecognised flag is an error, and there is no
// syntax by which a password can arrive as an argument.
func parseAdminFlags(args []string) (adminFlags, error) {
	var parsed adminFlags

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 >= len(args) {
				return parsed, errors.New("--username needs a value")
			}
			i++
			parsed.username = args[i]
		case "--generate":
			parsed.generate = true
		case "--keep-password":
			parsed.keepPassword = true
		case "--force":
			parsed.force = true
		default:
			return parsed, fmt.Errorf("unknown option %q", args[i])
		}
	}

	if parsed.generate && parsed.keepPassword {
		return parsed, errors.New(
			"--keep-password cannot be used with --generate: a generated password " +
				"the operator has seen must be changed at next login")
	}
	return parsed, nil
}

// adminBootstrap creates the first administrator.
func adminBootstrap(out io.Writer, args []string) int {
	flags, err := parseAdminFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin bootstrap: %v\n", err)
		return exitUsage
	}

	session, code := openAdminSession(flags.force)
	if session == nil {
		return code
	}
	defer session.close()

	claimed, err := session.admin.Claimed(session.ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin bootstrap: %v\n", err)
		return exitFailure
	}
	if claimed {
		fmt.Fprintln(os.Stderr,
			"admin bootstrap: this installation already has an administrator\n"+
				"\tuse `harbormaster admin reset-password` to recover an account")
		return exitFailure
	}

	username, err := resolveUsername(out, flags.username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin bootstrap: %v\n", err)
		return exitUsage
	}

	password, generated, err := resolvePassword(out, username, flags.generate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin bootstrap: %v\n", err)
		return exitFailure
	}

	// A generated password has been printed to a terminal, so it is temporary
	// by the same rule the account-creation endpoint follows: a credential its
	// holder did not choose is a shared secret until they do.
	user, err := session.admin.Claim(session.ctx, username, password, generated)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin bootstrap: %v\n", err)
		return exitFailure
	}

	_, _ = fmt.Fprintf(out, "\nAdministrator %q created. This installation is now claimed.\n", user.Username)
	if generated {
		printGeneratedPassword(out, password)
	}
	return exitOK
}

// adminResetPassword replaces an account's password.
func adminResetPassword(out io.Writer, args []string) int {
	flags, err := parseAdminFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin reset-password: %v\n", err)
		return exitUsage
	}

	session, code := openAdminSession(flags.force)
	if session == nil {
		return code
	}
	defer session.close()

	username, err := resolveUsername(out, flags.username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin reset-password: %v\n", err)
		return exitUsage
	}

	// The destructive part is the session revocation, and it is confirmed
	// BEFORE the password is collected: a confirmation offered after the
	// operator has already typed a new password twice is one they will accept
	// without reading.
	if !confirm(out, fmt.Sprintf(
		"Reset the password for %q? Every live session for that account will end.", username)) {
		_, _ = fmt.Fprintln(out, "Cancelled. Nothing was changed.")
		return exitOK
	}

	password, generated, err := resolvePassword(out, username, flags.generate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin reset-password: %v\n", err)
		return exitFailure
	}

	// A generated password is always temporary; an operator-chosen one is
	// temporary unless they said otherwise.
	mustChange := generated || !flags.keepPassword

	outcome, err := session.admin.ResetPassword(session.ctx, username, password, mustChange)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			// Enumeration is not a concern here -- the caller is reading the
			// database -- and a silent no-op would be far worse.
			fmt.Fprintf(os.Stderr, "admin reset-password: no account named %q\n", username)
			return exitFailure
		}
		fmt.Fprintf(os.Stderr, "admin reset-password: %v\n", err)
		return exitFailure
	}

	_, _ = fmt.Fprintf(out, "\nPassword replaced for %q (%s).\n",
		outcome.User.Username, outcome.User.Role)
	_, _ = fmt.Fprintf(out, "  sessions revoked:  %d\n", outcome.SessionsRevoked)
	if outcome.Reactivated {
		_, _ = fmt.Fprintln(out, "  account status:    reactivated (it was disabled or locked)")
	}
	if mustChange {
		_, _ = fmt.Fprintln(out, "  next login:        must choose a new password")
	}
	if generated {
		printGeneratedPassword(out, password)
	}
	return exitOK
}

// ------------------------------------------------------------- plumbing --

// adminSession is an opened installation for a console command.
type adminSession struct {
	ctx   context.Context
	db    *store.DB
	admin *service.LocalAdmin
}

func (s *adminSession) close() {
	if err := s.db.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "admin: close database: %v\n", err)
	}
}

// openAdminSession loads configuration, checks permissions, and opens the
// installation.
//
// Returns nil and an exit code on failure, so each command's error path is one
// line rather than five.
func openAdminSession(force bool) (*adminSession, int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin: invalid configuration: %v\n", err)
		return nil, exitUsage
	}

	if !checkInstallationPermissions(os.Stderr, cfg, force) {
		return nil, exitFailure
	}

	// Quiet by default. These commands print a deliberate, readable report, and
	// interleaving migration and integrity logging into it makes the one line
	// that matters -- the generated password -- easy to miss.
	logger := logging.New(io.Discard, cfg.Log.Level, cfg.Log.Format)

	ctx := context.Background()
	db, err := openStore(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin: %v\n", err)
		return nil, exitFailure
	}

	hasher, err := service.NewPasswordHasher(service.ArgonParamsFrom(cfg.Auth))
	if err != nil {
		_ = db.Close()
		fmt.Fprintf(os.Stderr, "admin: password hashing parameters: %v\n", err)
		return nil, exitUsage
	}

	return &adminSession{
		ctx:   ctx,
		db:    db,
		admin: service.NewLocalAdmin(db.Users, db.Sessions, db.Audit, hasher, nil),
	}, exitOK
}

// checkInstallationPermissions refuses to operate on a world-readable
// installation.
//
// It checks the database and the HMAC key file, which between them hold the
// password verifiers and the material that validates session digests. A group-
// or world-readable copy of either turns an account recovery into an account
// handover.
//
// Windows reports POSIX-style bits that do not reflect its real ACLs, so the
// check is skipped there rather than producing a warning an operator cannot act
// on. Said out loud, because a security check that silently does nothing is
// worse than one that is absent.
func checkInstallationPermissions(w io.Writer, cfg config.Config, force bool) bool {
	if !posixPermissionsMeaningful() {
		_, _ = fmt.Fprintln(w,
			"admin: note: file permission checks are not meaningful on this platform; "+
				"confirm the data directory's access control yourself")
		return true
	}

	paths := []string{cfg.Store.Path}
	if cfg.Snapshots.HMACKeyFile != "" {
		paths = append(paths, cfg.Snapshots.HMACKeyFile)
	} else {
		paths = append(paths, filepath.Join(filepath.Dir(cfg.Store.Path), "snapshot-hmac.key"))
	}

	var wide []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			// A missing file is not a permission problem. A first bootstrap has
			// neither a database nor a key yet.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			_, _ = fmt.Fprintf(w, "admin: cannot inspect %s: %v\n", filepath.Base(path), err)
			return false
		}
		if info.Mode().Perm()&0o077 != 0 {
			wide = append(wide, fmt.Sprintf("%s (%#o)", filepath.Base(path), info.Mode().Perm()))
		}
	}

	if len(wide) == 0 {
		return true
	}

	_, _ = fmt.Fprintf(w,
		"admin: refusing to continue: %s readable beyond the owner\n"+
			"\tthis installation holds password verifiers and session key material\n"+
			"\tfix with: chmod 600 <file>, or pass --force to proceed anyway\n",
		strings.Join(wide, ", "))
	return force
}

// resolveUsername takes the flag value or prompts, and validates the shape
// before anything touches the database.
func resolveUsername(out io.Writer, provided string) (string, error) {
	raw := provided
	if strings.TrimSpace(raw) == "" {
		answer, err := prompt(out, "Username: ")
		if err != nil {
			return "", err
		}
		raw = answer
	}

	username := domain.NormaliseUsername(raw)
	if !domain.ValidUsername(username) {
		return "", errors.New(domain.UsernameRule())
	}
	return username, nil
}

// resolvePassword generates or prompts for a password, and reports which.
//
// Prompting is the default and generation is opt-in, because a generated
// password has to be PRINTED to be usable and the spec's rule is that a
// password is never printed unless the operator asked for one.
func resolvePassword(out io.Writer, username string, generate bool) (string, bool, error) {
	if generate {
		password, err := service.GeneratePassword()
		if err != nil {
			return "", false, err
		}
		return password, true, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", false, errors.New(
			"a password must be typed at a terminal; this is not one\n" +
				"\tuse --generate for an unattended run, which prints the password once")
	}

	for attempt := 0; attempt < maxPasswordAttempts; attempt++ {
		first, err := promptSecret(out, "New password: ")
		if err != nil {
			return "", false, err
		}
		if problem := domain.CheckPassword(first, username); problem != domain.PasswordOK {
			_, _ = fmt.Fprintf(out, "  %s\n", problem.Explain())
			continue
		}

		second, err := promptSecret(out, "Repeat password: ")
		if err != nil {
			return "", false, err
		}
		if first != second {
			_, _ = fmt.Fprintln(out, "  the two entries did not match")
			continue
		}
		return first, false, nil
	}

	return "", false, errors.New("too many attempts; nothing was changed")
}

// maxPasswordAttempts bounds the interactive retry loop, so a script piping
// junk at the prompt terminates.
const maxPasswordAttempts = 3

// printGeneratedPassword shows a generated password once, with its warning.
func printGeneratedPassword(out io.Writer, password string) {
	_, _ = fmt.Fprintf(out, "\n"+
		"  ==========================================================\n"+
		"   Generated password -- shown ONCE and stored nowhere:\n"+
		"\n"+
		"     %s\n"+
		"\n"+
		"   It must be changed at next login.\n"+
		"  ==========================================================\n\n",
		password)
}

// prompt reads one line of visible input.
func prompt(out io.Writer, question string) (string, error) {
	_, _ = fmt.Fprint(out, question)
	reader := bufio.NewReader(io.LimitReader(os.Stdin, maxPromptBytes))
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no input")
	}
	return strings.TrimSpace(line), nil
}

// maxPromptBytes bounds one answer, so a prompt cannot be used to make the
// process allocate without limit.
const maxPromptBytes = 4096

// promptSecret reads one line WITHOUT echoing it.
//
// term.ReadPassword turns off terminal echo for the duration. Without it the
// password is on the operator's screen, in their scrollback, and in any
// recording of the session.
func promptSecret(out io.Writer, question string) (string, error) {
	_, _ = fmt.Fprint(out, question)
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(secret) > domain.MaxPasswordBytes {
		return "", errors.New("that password is longer than the maximum")
	}
	return string(secret), nil
}

// confirm asks a yes/no question, defaulting to no.
//
// Defaulting to no matters: the question is asked before an operation that ends
// every session an account holds, and a bare Enter must not perform it.
func confirm(out io.Writer, question string) bool {
	answer, err := prompt(out, question+" [y/N]: ")
	if err != nil {
		return false
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
