package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/diagnostics"
)

// `serve` is deliberately not exercised here: it binds a port and blocks. The
// routing decisions around it are what this test covers.
func TestDispatchRoutesNonServingCommands(t *testing.T) {
	tests := map[string]struct {
		args []string
		want int
	}{
		"help":       {[]string{"help"}, exitOK},
		"-h":         {[]string{"-h"}, exitOK},
		"--help":     {[]string{"--help"}, exitOK},
		"version":    {[]string{"version"}, exitOK},
		"unknown":    {[]string{"inventory"}, exitUsage},
		"typo":       {[]string{"healthchek"}, exitUsage},
		"empty flag": {[]string{"--pull"}, exitUsage},
		// A near-miss on a new command must still be a usage error rather than
		// starting a server.
		"diagnose typo": {[]string{"diagnos"}, exitUsage},
		"backup typo":   {[]string{"backups", "x.db"}, exitUsage},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := dispatch(tc.args); got != tc.want {
				t.Errorf("dispatch(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// An unrecognised argument must not fall through to serving. Starting a server
// when the operator asked for something else is the wrong kind of surprise for
// a tool that fronts the Docker socket.
func TestDispatchDoesNotServeOnUnknownCommand(t *testing.T) {
	if got := dispatch([]string{"definitely-not-a-command"}); got == exitOK {
		t.Error("an unknown command must not report success")
	}
}

// The help text must document every command that exists.
//
// A command that works but is not listed might as well not exist, and the
// reliability commands are exactly the ones an operator needs to find at the
// moment they have no other information.
func TestUsageDocumentsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	usage(&out)

	for _, command := range []string{"serve", "healthcheck", "diagnose", "backup", "version", "help"} {
		if !strings.Contains(out.String(), command) {
			t.Errorf("the usage text does not mention %q", command)
		}
	}
	// The reason diagnose is a command rather than an endpoint should be
	// visible where an operator will read it, not only in a package comment.
	if !strings.Contains(out.String(), "read-only") {
		t.Error("the usage text must say that diagnose opens the database read-only")
	}
}

// `backup` with the wrong number of arguments is a usage error, not a failed
// backup: nothing was attempted.
func TestBackupCommandRequiresExactlyOneDestination(t *testing.T) {
	for name, args := range map[string][]string{
		"none":  {},
		"blank": {"   "},
		"two":   {"a.db", "b.db"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if got := backupCommand(&out, args); got != exitUsage {
				t.Errorf("backupCommand(%v) = %d, want %d", args, got, exitUsage)
			}
		})
	}
}

// The two new commands must be reachable from dispatch, and must not be
// mistaken for the serve default.
func TestDispatchRoutesTheReliabilityCommands(t *testing.T) {
	// A per-test database path, so dispatching `diagnose` inspects a temporary
	// location rather than the developer's real data directory.
	t.Setenv("HARBORMASTER_DB_PATH", filepath.Join(t.TempDir(), "harbormaster.db"))

	// No database exists, which is a valid state and must exit cleanly.
	if got := dispatch([]string{"diagnose"}); got != diagnostics.ExitOK {
		t.Errorf("dispatch(diagnose) = %d, want %d for a first-start database", got, diagnostics.ExitOK)
	}

	// `backup` with no destination is a usage error, which proves it routed
	// rather than falling through to serve.
	if got := dispatch([]string{"backup"}); got != exitUsage {
		t.Errorf("dispatch(backup) = %d, want %d", got, exitUsage)
	}
}

// The whole diagnosis must be reachable end to end, writing a report and a
// verified backup, without a Docker daemon anywhere in sight.
func TestDiagnoseAndBackupWorkWithoutDocker(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "harbormaster.db")
	t.Setenv("HARBORMASTER_DB_PATH", dbPath)
	// A Docker host that cannot exist, proving nothing here contacts a daemon.
	t.Setenv("HARBORMASTER_DOCKER_HOST", "unix:///nonexistent/harbormaster-test.sock")

	dest := filepath.Join(dir, "backup", "copy.db")
	var backupOut bytes.Buffer
	if got := backupCommand(&backupOut, []string{dest}); got != diagnostics.ExitOK {
		t.Fatalf("backup = %d, want %d; output: %s", got, diagnostics.ExitOK, backupOut.String())
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the backup was not written: %v", err)
	}

	var diagnoseOut bytes.Buffer
	code := diagnoseCommand(&diagnoseOut)
	if code == diagnostics.ExitFailed {
		t.Fatalf("diagnose could not run: %s", diagnoseOut.String())
	}
	if !strings.Contains(diagnoseOut.String(), "HarborMaster diagnosis") {
		t.Errorf("diagnose printed no report: %s", diagnoseOut.String())
	}
}
