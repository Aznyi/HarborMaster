package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
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

// ------------------------------------------------- the listener exposure --

// captureRecords runs fn against a JSON logger and returns one entry per line.
func captureRecords(t *testing.T, fn func(*slog.Logger)) []map[string]any {
	t.Helper()

	var buf bytes.Buffer
	fn(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// A loopback bind settles the question, so there is nothing to say about it in
// either environment.
func TestListenerExposureSaysNothingWhenBoundToLoopback(t *testing.T) {
	for _, containerised := range []bool{true, false} {
		cfg := config.Config{Server: config.Server{Addr: "127.0.0.1:8080"}}
		records := captureRecords(t, func(logger *slog.Logger) {
			announceListenerExposure(logger, cfg, containerised)
		})
		if len(records) != 0 {
			t.Errorf("containerised=%v: expected no output, got %v", containerised, records)
		}
	}
}

// The regression this exists for. The image must bind 0.0.0.0 for a published
// port to be reachable at all, so warning on that address reported a finding
// that had not been made -- on EVERY containerised deployment, including the
// supported Compose one. A warning that always fires is one nobody reads.
func TestListenerExposureDoesNotWarnInAContainer(t *testing.T) {
	cfg := config.Config{Server: config.Server{Addr: "0.0.0.0:8080"}}
	records := captureRecords(t, func(logger *slog.Logger) {
		announceListenerExposure(logger, cfg, true)
	})

	if len(records) != 1 {
		t.Fatalf("expected exactly one record, got %d: %v", len(records), records)
	}
	if level, _ := records[0]["level"].(string); level != "INFO" {
		t.Errorf("level = %q, want INFO: a check that could not be performed is not a finding", level)
	}

	msg, _ := records[0]["msg"].(string)
	if !strings.Contains(msg, "cannot be established from here") {
		t.Errorf("the message must say which fact is missing, got: %q", msg)
	}
	// It must not assert the thing it has not checked.
	if strings.Contains(msg, "ensure the port is not reachable") {
		t.Errorf("the containerised message must not repeat the unconditional warning: %q", msg)
	}
	if _, ok := records[0]["nextStep"]; !ok {
		t.Error("the message must name where the operator can read the missing fact")
	}
}

// Outside a container the bind address DOES settle the question, so the warning
// has to survive. Softening it everywhere would have traded one wrong answer
// for another.
func TestListenerExposureStillWarnsOutsideAContainer(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080", "[::]:8080"} {
		cfg := config.Config{Server: config.Server{Addr: addr}}
		records := captureRecords(t, func(logger *slog.Logger) {
			announceListenerExposure(logger, cfg, false)
		})

		if len(records) != 1 {
			t.Fatalf("addr=%s: expected exactly one record, got %d", addr, len(records))
		}
		if level, _ := records[0]["level"].(string); level != "WARN" {
			t.Errorf("addr=%s: level = %q, want WARN", addr, level)
		}
		msg, _ := records[0]["msg"].(string)
		if !strings.Contains(msg, "not bound to loopback") {
			t.Errorf("addr=%s: unexpected message %q", addr, msg)
		}
	}
}

// The probe reads fixed absolute paths and nothing else. A relative path, or
// one assembled from anything a caller controls, would turn a presence check
// into a way to influence what HarborMaster concludes about its own exposure.
func TestContainerMarkersAreFixedAbsolutePaths(t *testing.T) {
	if len(containerMarkers) == 0 {
		t.Fatal("no container markers are probed")
	}
	for _, marker := range containerMarkers {
		if !strings.HasPrefix(marker, "/") {
			t.Errorf("marker %q is not absolute", marker)
		}
		// `path`, not `filepath`: these are Linux paths read from /proc-adjacent
		// locations at runtime, never host paths on the machine running the test.
		if marker != path.Clean(marker) {
			t.Errorf("marker %q is not a cleaned path", marker)
		}
	}
}

// It must answer rather than panic wherever the suite runs, and it must not
// depend on anything being present.
func TestRunningInContainerAnswersOnAnyHost(t *testing.T) {
	_ = runningInContainer()
}
