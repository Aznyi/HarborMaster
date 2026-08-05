package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Aznyi/HarborMaster/internal/logging"
)

// Every hazardous character in this file is written as a Go ESCAPE SEQUENCE
// rather than as a literal. A test about invisible characters that contains
// invisible characters cannot be reviewed, and a raw ESC or bidi override in a
// source file is the same defect the code under test exists to stop.

// assertInert is the property every case shares: a sanitised value carries
// nothing that can end a log record, start a new one, or drive a terminal.
func assertInert(t *testing.T, got string) {
	t.Helper()

	if !utf8.ValidString(got) {
		t.Errorf("output is not valid UTF-8: %q", got)
	}
	for i, r := range got {
		switch {
		case r < 0x20 || r == 0x7f:
			t.Errorf("output retains control byte %#x at %d: %q", r, i, got)
		case r >= 0x80 && r <= 0x9f:
			t.Errorf("output retains C1 control %#x at %d: %q", r, i, got)
		case r == '\u2028' || r == '\u2029':
			t.Errorf("output retains a Unicode line separator %#x at %d: %q", r, i, got)
		}
	}
}

func TestSafeStringNeutralisesInjection(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		// The attack this exists for: a forged second record. The inner double
		// quotes come back escaped too, which is %q doing its job.
		"newline injection": {
			in:   "/x\n{\"level\":\"INFO\",\"msg\":\"auth ok\"}",
			want: `/x\n{\"level\":\"INFO\",\"msg\":\"auth ok\"}`,
		},
		"carriage return": {in: "/a\rb", want: `/a\rb`},
		"crlf":            {in: "/a\r\nb", want: `/a\r\nb`},
		"tab":             {in: "/a\tb", want: `/a\tb`},
		"nul byte":        {in: "/a\x00b", want: `/a\x00b`},
		"bell":            {in: "/a\ab", want: `/a\ab`},
		"vertical tab":    {in: "/a\vb", want: `/a\vb`},
		"form feed":       {in: "/a\fb", want: `/a\fb`},
		"delete":          {in: "/a\x7fb", want: `/a\x7fb`},

		// Repositions and recolours a terminal, and some sequences change its
		// state outright.
		"ansi escape sequence": {
			in:   "/a\x1b[31mRED\x1b[0m",
			want: `/a\x1b[31mRED\x1b[0m`,
		},

		// C1 controls. Some terminals still act on these.
		"c1 next line": {in: "/a\u0085b", want: `/a\u0085b`},
		"c1 csi":       {in: "/a\u009bb", want: `/a\u009bb`},

		// Trojan Source: reverses how everything after it renders, so a path
		// can be made to display as something it is not.
		"bidi right-to-left override": {in: "/a\u202eb", want: `/a\u202eb`},
		"bidi left-to-right override": {in: "/a\u202db", want: `/a\u202db`},

		// Invisible, so it can hide a difference between two records.
		"zero width space":     {in: "/a\u200bb", want: `/a\u200bb`},
		"zero width joiner":    {in: "/a\u200db", want: `/a\u200db`},
		"word joiner":          {in: "/a\u2060b", want: `/a\u2060b`},
		"non-breaking space":   {in: "/a\u00a0b", want: `/a\u00a0b`},
		"ideographic space":    {in: "/a\u3000b", want: `/a\u3000b`},
		"soft hyphen":          {in: "/a\u00adb", want: `/a\u00adb`},
		"interlinear annotate": {in: "/a\ufff9b", want: `/a\ufff9b`},

		// Line terminators to a JavaScript parser, which is what many log
		// viewers are built on.
		"line separator":      {in: "/a\u2028b", want: `/a\u2028b`},
		"paragraph separator": {in: "/a\u2029b", want: `/a\u2029b`},

		// Above the BMP, so the wide escape form is exercised.
		"tag character": {in: "/a\U000e0041b", want: `/a\U000e0041b`},

		// Escaped so the encoding stays unambiguous: a path that literally
		// contained backslash-n must not read back as an escaped newline.
		"literal backslash": {in: `/a\nb`, want: `/a\\nb`},

		"empty": {in: "", want: ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := logging.SafeString(tc.in)
			if got != tc.want {
				t.Errorf("SafeString(%q)\n got  %q\n want %q", tc.in, got, tc.want)
			}
			assertInert(t, got)
		})
	}
}

// A path that is already safe comes back untouched. A sanitiser that mangled
// ordinary input would make the log worse for the people who read it, and they
// are the reason the log exists.
func TestSafeStringLeavesOrdinaryPathsAlone(t *testing.T) {
	for _, path := range []string{
		"/",
		"/api/v1/health",
		"/api/v1/containers/9f2c1a4b5e6d7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a",
		"/assets/index-C_XTPtdD.js",
		"/api/v1/snapshots/42/diff",
		"/a-b_c.d~e",
		"/x/y/z",
	} {
		if got := logging.SafeString(path); got != path {
			t.Errorf("SafeString(%q) = %q, want it unchanged", path, got)
		}
	}
}

// Printable Unicode survives. Escaping every non-ASCII byte would be the lazy
// fix and would render a legitimate path unreadable.
func TestSafeStringPreservesPrintableUnicode(t *testing.T) {
	for _, path := range []string{
		"/café",             // café
		"/日本語",              // 日本語
		"/конте",            // конте
		"/emoji/\U0001f6a2", // ship
		"/naïve-résumé",     // naïve-résumé
		"/مرحبا",            // Arabic, right-to-left by script
		"/€£¥",              // currency symbols
		"/✓-done",           // check mark
	} {
		got := logging.SafeString(path)
		if got != path {
			t.Errorf("SafeString(%q) = %q, want it unchanged; printable Unicode must survive", path, got)
		}
		assertInert(t, got)
	}
}

func TestSafeStringRepairsInvalidUTF8(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"lone continuation byte": {in: "/a\xffb", want: `/a\xffb`},
		"truncated sequence":     {in: "/a\xe6\x97", want: `/a\xe6\x97`},
		"bare 0x80":              {in: "/\x80", want: `/\x80`},
		"overlong encoding":      {in: "/\xc0\xaf", want: `/\xc0\xaf`},
		"surrogate half":         {in: "/\xed\xa0\x80", want: `/\xed\xa0\x80`},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if utf8.ValidString(tc.in) {
				t.Fatalf("input %q is already valid UTF-8; this case proves nothing", tc.in)
			}
			got := logging.SafeString(tc.in)
			if got != tc.want {
				t.Errorf("SafeString(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertInert(t, got)
		})
	}
}

func TestSafeStringBoundsLength(t *testing.T) {
	long := "/" + strings.Repeat("a", 4096)

	got := logging.SafeString(long)

	if !strings.HasSuffix(got, logging.TruncationMarker) {
		t.Errorf("a truncated value must say so; got %q...", got[:40])
	}
	if len(got) > logging.MaxLogFieldBytes+len(logging.TruncationMarker) {
		t.Errorf("len = %d, want at most %d",
			len(got), logging.MaxLogFieldBytes+len(logging.TruncationMarker))
	}
	// The bound applies to the content, not merely to where the marker lands.
	content := strings.TrimSuffix(got, logging.TruncationMarker)
	if len(content) > logging.MaxLogFieldBytes {
		t.Errorf("content len = %d, want at most %d", len(content), logging.MaxLogFieldBytes)
	}
}

// A value at exactly the bound must NOT be marked truncated. Without this the
// comparison could be off by one and every full-length record would carry a
// false marker.
func TestSafeStringDoesNotMarkUntruncatedValues(t *testing.T) {
	exact := strings.Repeat("a", logging.MaxLogFieldBytes)

	got := logging.SafeString(exact)
	if got != exact {
		t.Errorf("a value of exactly the maximum length was altered: len %d", len(got))
	}
	if strings.Contains(got, logging.TruncationMarker) {
		t.Error("a value that fits must not be marked truncated")
	}
}

// Truncation must never split a multi-byte rune, or the sanitiser would emit
// the invalid UTF-8 it exists to prevent.
func TestTruncationNeverSplitsARune(t *testing.T) {
	for name, unit := range map[string]string{
		"three byte": "日",
		"two byte":   "é",
		"four byte":  "\U0001f6a2",
		"escaped":    "\u202e",
	} {
		t.Run(name, func(t *testing.T) {
			got := logging.SafeString(strings.Repeat(unit, 4096))
			if !utf8.ValidString(got) {
				t.Errorf("truncation produced invalid UTF-8: %q", got)
			}
			if len(got) > logging.MaxLogFieldBytes+len(logging.TruncationMarker) {
				t.Errorf("len = %d exceeds the bound", len(got))
			}
			assertInert(t, got)
		})
	}
}

// Escaping expands the output, so a long value made entirely of characters
// that expand must still respect the bound.
func TestBoundHoldsWhenEveryRuneExpands(t *testing.T) {
	for name, unit := range map[string]string{
		"newlines":       "\n",
		"nul bytes":      "\x00",
		"invalid utf8":   "\xff",
		"bidi overrides": "\u202e",
	} {
		t.Run(name, func(t *testing.T) {
			got := logging.SafeString(strings.Repeat(unit, 4096))
			if len(got) > logging.MaxLogFieldBytes+len(logging.TruncationMarker) {
				t.Errorf("len = %d, want at most %d",
					len(got), logging.MaxLogFieldBytes+len(logging.TruncationMarker))
			}
			assertInert(t, got)
		})
	}
}

// Every value takes the SAME path, whatever its content.
//
// There is no shortcut that returns unescaped input, and there must not be:
// such a branch would return data that never passed the %q barrier, which both
// reintroduces the CodeQL finding and makes a field's safety depend on its
// content rather than on the code. This asserts the observable consequence --
// a short clean value and a long one are rendered by the same rules.
func TestEveryValueTakesTheSameEscapingPath(t *testing.T) {
	for _, base := range []string{"/api/v1/health", "/a-b_c.d~e", "/x/y/z"} {
		if got := logging.SafeString(base); got != base {
			t.Fatalf("a clean value was altered: %q -> %q", base, got)
		}
		// Past the bound, the same prefix must still render identically.
		long := logging.SafeString(base + strings.Repeat("/", logging.MaxLogFieldBytes))
		if !strings.HasPrefix(long, base) {
			t.Errorf("the same prefix rendered differently when long: %q", long)
		}
		// And a hazard in an otherwise clean value is escaped either way.
		if got := logging.SafeString(base + "\n"); !strings.HasSuffix(got, `\n`) {
			t.Errorf("SafeString(%q) = %q, want a trailing escaped newline", base+"\n", got)
		}
	}
}

func TestSafeAttrSanitisesItsValue(t *testing.T) {
	attr := logging.SafeAttr("path", "/x\nforged")

	if attr.Key != "path" {
		t.Errorf("Key = %q, want path", attr.Key)
	}
	if got := attr.Value.String(); got != `/x\nforged` {
		t.Errorf("Value = %q, want the sanitised form", got)
	}
}

// What the record actually looks like once a handler has written it: one line,
// one JSON object, no raw newline inside the field.
func TestSanitisedValueSurvivesTheJSONHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, "info", "json")

	logger.Info("http request",
		logging.SafeAttr("path", "/x\n{\"level\":\"INFO\",\"msg\":\"forged\"}"))

	out := buf.String()
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
		t.Errorf("the record spans more than one line:\n%s", out)
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(out), &record); err != nil {
		t.Fatalf("record is not a single JSON object: %v\n%s", err, out)
	}
	path, _ := record["path"].(string)
	if strings.Contains(path, "\n") {
		t.Errorf("decoded path still carries a real newline: %q", path)
	}
	if path != `/x\n{\"level\":\"INFO\",\"msg\":\"forged\"}` {
		t.Errorf("path = %q, want the escaped form", path)
	}
}

// The same, through the text handler, which escapes by a different mechanism.
func TestSanitisedValueSurvivesTheTextHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, "info", "text")

	logger.Info("http request", logging.SafeAttr("path", "/x\nforged"))

	if strings.Count(strings.TrimRight(buf.String(), "\n"), "\n") != 0 {
		t.Errorf("the record spans more than one line:\n%s", buf.String())
	}
}

// The honest control.
//
// slog's own handlers already escape, so the sanitiser is defence in depth for
// the escaping and the PRIMARY control only for the length bound. That claim
// appears in the package documentation, so it is pinned here: if a future Go
// release changed either behaviour, this test is what would say so rather than
// the documentation quietly becoming wrong.
func TestHandlerEscapingIsRealButUnbounded(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logger.Info("unsanitised", slog.String("path", "/x\nforged"))
	if strings.Contains(strings.TrimRight(buf.String(), "\n"), "\n") {
		t.Error("JSONHandler emitted a raw newline; the package documentation " +
			"claims it does not, and that claim would now be wrong")
	}

	// But it does NOT bound length, which is the gap the sanitiser closes.
	buf.Reset()
	logger.Info("unsanitised", slog.String("path", strings.Repeat("a", 8192)))
	if buf.Len() < 8192 {
		t.Error("the handler appears to truncate now; the length rationale in " +
			"the package documentation needs revisiting")
	}
}

func FuzzSafeString(f *testing.F) {
	for _, seed := range []string{
		"", "/", "/api/v1/health", "/x\nforged", "/a\r\nb", "/a\x00b",
		"/日本語", "/a\xffb", "\x1b[31m", "\u202e", `\n`,
		strings.Repeat("a", 1024), strings.Repeat("日", 512),
	} {
		f.Add(seed)
	}

	// The invariants, for ANY input.
	f.Fuzz(func(t *testing.T, in string) {
		got := logging.SafeString(in)

		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8 out of %q: %q", in, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
				t.Fatalf("hazardous rune %#x survived from %q: %q", r, in, got)
			}
		}
		if len(got) > logging.MaxLogFieldBytes+len(logging.TruncationMarker) {
			t.Fatalf("len %d exceeds the bound, from a %d-byte input", len(got), len(in))
		}
	})
}
