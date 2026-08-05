package diagnostics

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Rendering the diagnosis.
//
// Plain text, aligned, no colour and no escape sequences. The output is read
// through `docker logs`, over ssh, and pasted into issues; anything that needs
// a terminal to be legible is a liability in all three. Nothing here formats a
// value that came from the database's CONTENTS -- only counts, states from
// closed vocabularies, timestamps, and the operator's own paths.

// Render writes the report.
func Render(out io.Writer, report Report) {
	writeLine(out, "HarborMaster diagnosis")
	writeLine(out, strings.Repeat("=", 60))
	writeLine(out, "")

	section(out, "Build")
	field(out, "version", report.Build.Version)
	field(out, "commit", report.Build.Commit)
	field(out, "go", report.Build.GoVersion)
	field(out, "platform", report.Build.Platform)
	writeLine(out, "")

	renderFile(out, report)

	if !report.Opened {
		if report.OpenError != "" {
			section(out, "Database")
			field(out, "opened", "no")
			// The driver's message is deliberately NOT printed. It can name the
			// path and the daemon's internals, and the Findings section below
			// already carries the classified verdict and its remedy, which is
			// what an operator can act on.
			writeLine(out, "")
		}
		renderFindings(out, report)
		return
	}

	renderStorage(out, report)
	renderSchema(out, report)
	renderEngine(out, report)
	renderInventory(out, report)
	renderCounts(out, report)
	renderFindings(out, report)
}

func renderFile(out io.Writer, report Report) {
	section(out, "Database file")
	field(out, "path", report.DatabaseAt)
	field(out, "exists", yesNo(report.File.Exists))
	if !report.File.Exists {
		field(out, "directory writable", yesNo(report.File.DirectoryWritable))
		writeLine(out, "")
		return
	}
	field(out, "size", humanBytes(report.File.SizeBytes))
	field(out, "mode", fmt.Sprintf("%#o", report.File.Mode))
	field(out, "modified", report.File.ModifiedAt.Format("2006-01-02T15:04:05Z"))
	field(out, "write-ahead log", walDescription(report.File))
	field(out, "shared-memory index", yesNo(report.File.SHMExists))
	field(out, "directory mode", fmt.Sprintf("%#o", report.File.DirectoryMode))
	field(out, "directory writable", yesNo(report.File.DirectoryWritable))
	writeLine(out, "")
}

func walDescription(file FileReport) string {
	if !file.WALExists {
		return "absent (checkpointed)"
	}
	return humanBytes(file.WALSizeBytes)
}

func renderStorage(out io.Writer, report Report) {
	section(out, "Storage")
	field(out, "journal mode", report.Stats.JournalMode)
	field(out, "encoding", report.Stats.Encoding)
	field(out, "page size", fmt.Sprintf("%d", report.Stats.PageSize))
	field(out, "pages", fmt.Sprintf("%d (%d free)", report.Stats.PageCount, report.Stats.FreelistCount))
	field(out, "logical size", humanBytes(report.Stats.SizeBytes))
	field(out, "foreign keys", onOff(report.Stats.ForeignKeysOn))
	field(out, "busy timeout", fmt.Sprintf("%dms", report.Stats.BusyTimeoutMS))
	field(out, "synchronous", fmt.Sprintf("%d", report.Stats.Synchronous))
	field(out, "integrity", report.Integrity.Summary())
	if report.Integrity.ForeignKeyViolations > 0 {
		field(out, "foreign key violations", fmt.Sprintf("%d", report.Integrity.ForeignKeyViolations))
	}
	for _, problem := range report.Integrity.Problems {
		field(out, "problem", problem)
	}
	writeLine(out, "")
}

func renderSchema(out io.Writer, report Report) {
	section(out, "Schema")
	field(out, "applied", fmt.Sprintf("%d of %d",
		len(report.Migrations.Applied), len(report.Migrations.Expected)))
	if len(report.Migrations.Missing) > 0 {
		field(out, "pending", strings.Join(report.Migrations.Missing, ", "))
	}
	if len(report.Migrations.Unknown) > 0 {
		field(out, "unknown to this build", strings.Join(report.Migrations.Unknown, ", "))
	}
	writeLine(out, "")
}

func renderEngine(out io.Writer, report Report) {
	section(out, "Event engine (persisted state)")
	if !report.Engine.Present {
		field(out, "state", "never connected")
		writeLine(out, "")
		return
	}
	field(out, "last connected", orNever(report.Engine.LastConnectedAt))
	field(out, "last disconnected", orNever(report.Engine.LastDisconnectedAt))
	field(out, "last event", orNever(report.Engine.LastEventAt))
	field(out, "last reconciled", orNever(report.Engine.LastReconciledAt))
	field(out, "reconnects", fmt.Sprintf("%d", report.Engine.ReconnectCount))
	writeLine(out, "")
}

func renderInventory(out io.Writer, report Report) {
	section(out, "Inventory")
	if !report.Inventory.Present {
		field(out, "state", "no refresh recorded")
		writeLine(out, "")
		return
	}
	field(out, "generation", fmt.Sprintf("%d", report.Inventory.Generation))
	field(out, "last refresh", fmt.Sprintf("%s (%s) at %s",
		report.Inventory.LastState, report.Inventory.LastTrigger, orNever(report.Inventory.LastStarted)))
	if report.Inventory.RunningRefreshes > 0 {
		field(out, "rows still running", fmt.Sprintf("%d", report.Inventory.RunningRefreshes))
	}
	writeLine(out, "")
}

func renderCounts(out io.Writer, report Report) {
	if len(report.Counts) == 0 {
		return
	}
	section(out, "Row counts")

	names := make([]string, 0, len(report.Counts))
	for name := range report.Counts {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		field(out, name, fmt.Sprintf("%d", report.Counts[name]))
	}
	writeLine(out, "")
}

func renderFindings(out io.Writer, report Report) {
	section(out, "Findings")

	actionable := 0
	for _, finding := range report.Findings {
		if finding.Severity != SeverityInfo {
			actionable++
		}
	}
	if len(report.Findings) == 0 {
		writeLine(out, "  none -- every check passed")
		writeLine(out, "")
		return
	}

	// Most severe first: the line an operator reads before they stop reading
	// should be the one that matters most.
	ordered := make([]Finding, len(report.Findings))
	copy(ordered, report.Findings)
	sort.SliceStable(ordered, func(i, j int) bool {
		return severityRank(ordered[i].Severity) > severityRank(ordered[j].Severity)
	})

	for _, finding := range ordered {
		writeLine(out, fmt.Sprintf("  [%s] %s", strings.ToUpper(string(finding.Severity)), finding.Summary))
		if finding.Remedy != "" {
			writeLine(out, "          "+finding.Remedy)
		}
	}
	writeLine(out, "")
	writeLine(out, fmt.Sprintf("%d finding(s), %d needing attention", len(report.Findings), actionable))
}

func severityRank(severity Severity) int {
	switch severity {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	default:
		return 0
	}
}

// fieldWidth aligns the labels. Wide enough for the longest label in the
// report; a label past it simply pushes its value right rather than being
// truncated, because a truncated diagnostic label is a riddle.
const fieldWidth = 22

func section(out io.Writer, title string) {
	writeLine(out, title)
	writeLine(out, strings.Repeat("-", len(title)))
}

func field(out io.Writer, label, value string) {
	writeLine(out, fmt.Sprintf("  %-*s %s", fieldWidth, label, value))
}

// writeLine writes one line, ignoring the error.
//
// The error is ignored deliberately: the destination is stdout, a failure to
// write means the stream is gone, and there is nowhere left to report it. The
// exit code carries the verdict regardless of whether the text arrived.
func writeLine(out io.Writer, line string) {
	_, _ = fmt.Fprintln(out, line)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func orNever(value string) string {
	if strings.TrimSpace(value) == "" {
		return "never"
	}
	return value
}
