package api

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Aznyi/HarborMaster/internal/logging"
)

// End-to-end log-injection tests.
//
// The unit tests in internal/logging prove the sanitiser is correct. These
// prove it is actually REACHED: they drive a real request through the real
// middleware with a real JSON handler, and assert on the bytes that come out.
// A correct sanitiser nobody calls is the failure mode worth testing for.
//
// Every hazardous character is written as a Go escape sequence, never as a
// literal, so this file stays reviewable.

// injectionProbes are request paths carrying each class of hazard.
//
// The path is built by hand rather than through url.Parse: net/url would
// percent-decode a request line, and what matters here is what r.URL.Path
// holds by the time a handler sees it.
var injectionProbes = map[string]string{
	"newline":       "/x\n{\"level\":\"INFO\",\"msg\":\"forged\"}",
	"carriage":      "/x\rrewritten",
	"crlf":          "/x\r\nforged",
	"tab":           "/x\tcolumn",
	"nul":           "/x\x00truncated",
	"ansi escape":   "/x\x1b[31mred\x1b[0m",
	"delete":        "/x\x7f",
	"c1 control":    "/x\u009b0m",
	"bidi override": "/x\u202egnol",
	"zero width":    "/x\u200bhidden",
	"line sep":      "/x\u2028forged",
	"invalid utf8":  "/x\xffbad",
	"long path":     "/" + strings.Repeat("a", 4096),
}

// captureRequest sends one request through the middleware stack under test and
// returns every log record it produced.
func captureRequest(t *testing.T, path string, handler http.Handler) (string, []map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	logger := logging.New(&buf, "debug", "json")

	stack := chain(handler,
		withRequestID,
		withRecovery(logger),
		withAccessLog(logger),
	)

	// The URL is assigned directly rather than parsed, so the raw bytes reach
	// the handler exactly as an attacker would deliver them.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = path

	stack.ServeHTTP(httptest.NewRecorder(), req)

	raw := buf.String()
	records := make([]map[string]any, 0, 2)
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not valid JSON: %v\nline: %q", err, line)
		}
		records = append(records, record)
	}
	return raw, records
}

// assertRecordsAreSafe is the shared assertion: one record per event, valid
// JSON throughout, no raw hazard in any string value, and nothing unbounded.
func assertRecordsAreSafe(t *testing.T, raw string, records []map[string]any, wantRecords int) {
	t.Helper()

	if len(records) != wantRecords {
		t.Errorf("produced %d log records, want %d; a forged record would show up as an extra one\n%s",
			len(records), wantRecords, raw)
	}
	if !utf8.ValidString(raw) {
		t.Error("log output is not valid UTF-8; a consumer would reject or drop the record")
	}

	for _, record := range records {
		for key, value := range record {
			text, ok := value.(string)
			if !ok {
				continue
			}
			// The stack trace is exempt and deliberately so: it is not
			// request-derived, and its newlines are what make it readable.
			if key == "stack" {
				continue
			}
			for _, r := range text {
				if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
					t.Errorf("field %q carries hazardous rune %#x: %q", key, r, text)
				}
			}
			if len(text) > logging.MaxLogFieldBytes+len(logging.TruncationMarker) {
				t.Errorf("field %q is %d bytes, past the bound", key, len(text))
			}
		}
	}
}

// The access log is the highest-volume record and the one an attacker can
// drive on demand, so it is the primary target.
func TestAccessLogNeutralisesInjectedPaths(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for name, path := range injectionProbes {
		t.Run(name, func(t *testing.T) {
			raw, records := captureRequest(t, path, ok)
			assertRecordsAreSafe(t, raw, records, 1)

			logged, _ := records[0]["path"].(string)
			if logged == "" {
				t.Fatal("the path field is missing; sanitising must not mean dropping it")
			}
			// Requirement: keep logging a sanitised path rather than removing
			// path logging. The prefix survives, so the record still says what
			// was requested.
			if !strings.HasPrefix(logged, "/x") && !strings.HasPrefix(logged, "/a") {
				t.Errorf("path = %q; the readable prefix should survive sanitisation", logged)
			}
		})
	}
}

// A forged record is the actual attack. This asserts the shape of the failure
// rather than a character class: exactly one record, and no record claiming to
// be something the server did not log.
func TestInjectedPathCannotForgeASecondRecord(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	raw, records := captureRequest(t,
		"/x\n{\"time\":\"2026-01-01T00:00:00Z\",\"level\":\"INFO\",\"msg\":\"auth ok\"}", ok)

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1; a second record was forged\n%s", len(records), raw)
	}
	if strings.Contains(raw, `"msg":"auth ok"`) {
		t.Errorf("the forged message appears as a real field:\n%s", raw)
	}
	if msg, _ := records[0]["msg"].(string); msg != "http request" {
		t.Errorf("msg = %q, want the server's own message", msg)
	}
}

// Panic recovery logs method, path and the panic value. A handler that panics
// with a message built from its input is the ordinary way panics get written,
// so the panic VALUE is request-derived too.
func TestPanicRecoveryDoesNotLeakUnsanitisedRequestData(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exactly how a real handler would do it, and the reason the panic
		// value needs sanitising.
		panic("bad request for " + r.URL.Path)
	})

	raw, records := captureRequest(t, "/x\n{\"level\":\"ERROR\",\"msg\":\"forged\"}", panicky)

	// ONE record, not two. withAccessLog sits inside withRecovery in the
	// production chain (see NewServer), and it logs after next.ServeHTTP
	// returns -- so a panic unwinds straight past that statement and the
	// request produces no access-log line at all.
	//
	// That is pre-existing behaviour and this change does not alter it; the
	// count is asserted so a future reordering of the chain has to come past
	// this test rather than silently changing what a panicking request
	// records.
	assertRecordsAreSafe(t, raw, records, 1)

	var sawPanic bool
	for _, record := range records {
		if msg, _ := record["msg"].(string); msg != "panic recovered" {
			continue
		}
		sawPanic = true

		value, _ := record["panic"].(string)
		if value == "" {
			t.Error("the panic value must still be recorded")
		}
		if strings.Contains(value, "\n") {
			t.Errorf("the panic value carries a raw newline: %q", value)
		}
		if !strings.Contains(value, `\n`) {
			t.Errorf("panic = %q; the injected newline should appear escaped, not removed", value)
		}
		if _, ok := record["stack"].(string); !ok {
			t.Error("the stack trace must survive; it is the point of the record")
		}
		if _, ok := record["requestId"].(string); !ok {
			t.Error("the request ID must survive")
		}
	}
	if !sawPanic {
		t.Fatalf("no panic record was emitted\n%s", raw)
	}
}

// The static handler derives its `name` from r.URL.Path via path.Clean, which
// normalises traversal and does nothing about control characters. Its error
// carries the same string a second time, through fs.PathError.
func TestStaticHandlerSanitisesBothPathAndError(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, "debug", "json")

	// An FS whose Stat fails with something other than ErrNotExist, so the
	// default branch -- the one that logs -- is the branch taken.
	handler := &staticHandler{
		assets:    failingFS{},
		logger:    logger,
		available: true,
		fileSrv:   http.NotFoundHandler(),
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/asset\n{\"level\":\"INFO\",\"msg\":\"forged\"}\x1b[31m"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	raw := buf.String()
	if strings.Count(strings.TrimRight(raw, "\n"), "\n") != 0 {
		t.Fatalf("the record spans more than one line:\n%s", raw)
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(raw, "\n")), &record); err != nil {
		t.Fatalf("record is not valid JSON: %v\n%s", err, raw)
	}

	for _, field := range []string{"path", "error"} {
		text, ok := record[field].(string)
		if !ok {
			t.Errorf("field %q is missing", field)
			continue
		}
		for _, r := range text {
			if r < 0x20 || r == 0x7f {
				t.Errorf("field %q carries control rune %#x: %q", field, r, text)
			}
		}
	}
	// The error embeds the path, which is why sanitising only the path field
	// would have left the same input reaching the record.
	if errText, _ := record["error"].(string); !strings.Contains(errText, `\n`) {
		t.Errorf("error = %q; it should carry the escaped path, proving it was sanitised too", errText)
	}
}

// failingFS fails with an error that is neither nil nor fs.ErrNotExist, which
// is the only route to staticHandler's logging branch.
//
// It returns an *fs.PathError carrying the requested name, because that is
// what embed.FS actually does and the whole point of the test is that the
// error text embeds the attacker's path. A fake returning a bare sentinel
// would pass while proving nothing.
type failingFS struct{}

func (failingFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

// The request ID is server-generated and hex-encoded, which is why the access
// log does not sanitise it. That is an assumption about withRequestID, so it is
// pinned rather than assumed.
func TestRequestIDIsAlwaysSafeToLogUnsanitised(t *testing.T) {
	seen := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	for i := 0; i < 64; i++ {
		_, records := captureRequest(t, "/api/v1/health", seen)
		id, _ := records[0]["requestId"].(string)
		if id == "" {
			t.Fatal("no request ID was recorded")
		}
		for _, r := range id {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("request ID %q contains %q, outside the hex alphabet; "+
					"if the generator changed, the access log must sanitise it", id, r)
			}
		}
	}
}

// A client-supplied X-Request-ID must never become the logged correlation ID.
// If it did, the field would be attacker-controlled and would need sanitising.
func TestClientSuppliedRequestIDIsIgnored(t *testing.T) {
	forged := "aaaa\n{\"level\":\"INFO\",\"msg\":\"forged\"}"

	var buf bytes.Buffer
	logger := logging.New(&buf, "debug", "json")
	stack := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		withRequestID, withAccessLog(logger))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set(RequestIDHeader, forged)
	stack.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(buf.String(), "forged") {
		t.Errorf("a client-supplied request ID reached the log:\n%s", buf.String())
	}
}

// ------------------------------------------------------- source-level guard --

// requestDerivedRoots are the expressions that carry attacker-controlled text
// off an *http.Request.
var requestDerivedRoots = []string{
	"r.URL.Path", "r.URL.RawQuery", "r.URL.RawPath", "r.URL.Fragment",
	"r.Method", "r.Host", "r.RemoteAddr", "r.RequestURI", "r.Referer",
	"r.UserAgent", "r.PathValue", "r.FormValue", "r.Header.Get",
}

// TestNoRequestDerivedValueReachesSlogUnsanitised parses this package and fails
// if a request-derived expression is passed straight to slog.String.
//
// This is the guard that keeps the fix from decaying. The four CodeQL findings
// were each one line, and each was individually easy to write; nothing stopped
// the fifth from being written tomorrow. A source-level assertion does, and it
// runs in `go test` rather than waiting for a scan.
//
// It parses with go/ast rather than grepping, so a match is a real call
// argument and not a mention in a comment or a string.
func TestNoRequestDerivedValueReachesSlogUnsanitised(t *testing.T) {
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var checked int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		parsed, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isSlogStringCall(call) {
				return true
			}
			for _, arg := range call.Args {
				if root := requestDerivedRoot(arg); root != "" {
					position := fileSet.Position(arg.Pos())
					t.Errorf("%s:%d: slog.String receives %s directly\n"+
						"\tuse logging.SafeAttr instead: an attacker-controlled value must not "+
						"reach a log record unsanitised",
						filepath.Base(name), position.Line, root)
				}
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("no source files were parsed; the guard is not looking at anything")
	}
}

// The positive control. The guard above passes trivially if its matcher cannot
// recognise the pattern it hunts, so the matcher is tested directly against
// source it must reject and source it must accept.
func TestRequestDerivedMatcherActuallyMatches(t *testing.T) {
	mustFlag := map[string]string{
		"plain path":     `slog.String("path", r.URL.Path)`,
		"method":         `slog.String("method", r.Method)`,
		"path value":     `slog.String("id", r.PathValue("id"))`,
		"header":         `slog.String("h", r.Header.Get("X-Thing"))`,
		"query":          `slog.String("q", r.URL.RawQuery)`,
		"remote address": `slog.String("peer", r.RemoteAddr)`,
	}
	for name, source := range mustFlag {
		t.Run("flags "+name, func(t *testing.T) {
			if !exprIsFlagged(t, source) {
				t.Errorf("the guard failed to flag %s; it would miss a real regression", source)
			}
		})
	}

	mustPass := map[string]string{
		"sanitised":     `logging.SafeAttr("path", r.URL.Path)`,
		"literal":       `slog.String("msg", "static")`,
		"server value":  `slog.String("requestId", RequestIDFrom(r.Context()))`,
		"error":         `slog.String("error", err.Error())`,
		"wrapped":       `slog.String("path", logging.SafeString(r.URL.Path))`,
		"other request": `slog.Int("status", rec.status)`,
	}
	for name, source := range mustPass {
		t.Run("accepts "+name, func(t *testing.T) {
			if exprIsFlagged(t, source) {
				t.Errorf("the guard wrongly flagged %s; a false positive makes it get deleted", source)
			}
		})
	}
}

func exprIsFlagged(t *testing.T, source string) bool {
	t.Helper()

	expr, err := parser.ParseExpr(source)
	if err != nil {
		t.Fatalf("parse %q: %v", source, err)
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		t.Fatalf("%q is not a call", source)
	}
	if !isSlogStringCall(call) {
		return false
	}
	for _, arg := range call.Args {
		if requestDerivedRoot(arg) != "" {
			return true
		}
	}
	return false
}

// isSlogStringCall reports whether call is slog.String(...).
//
// slog.Any and slog.Group are not matched: neither is used with a
// request-derived value anywhere in this package, and slog.Any on an untrusted
// value has a separate problem (it invokes the value's own marshaller) that a
// string sanitiser would not solve.
//
// ERROR STRINGS ARE NOT COVERED BY THIS GUARD, and that is a decision rather
// than an omission. `slog.String("error", err.Error())` appears about thirty
// times in this package, and an error is only a log-injection carrier if it
// embeds request text. The question was settled by reading the code that
// produces them rather than by assuming:
//
//   - Every fmt.Errorf in internal/store interpolates internal identifiers
//     only -- migration names, table names, column names, an integrity mode.
//     ResolveID, the one repository call that takes a caller-supplied
//     reference, wraps the driver error and never the reference.
//   - The write errors in response.go and event_stream.go come from the
//     network stack and the ResponseController, not from request content.
//
// The one exception was found and fixed: fs.Stat in static.go fails with an
// *fs.PathError whose message embeds the requested name, so that call site
// sanitises its error as well as its path, pinned by a test using a fake FS
// that reproduces the real wrapping.
//
// If a future store error starts embedding caller input, this reasoning
// expires. That is why the audit is written down here, next to the guard, and
// not left in a pull-request comment.
func isSlogStringCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "String" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "slog"
}

// requestDerivedRoot renders expr as source and reports which request-derived
// expression it contains, or "" for none.
func requestDerivedRoot(expr ast.Expr) string {
	rendered := renderExpr(expr)
	// A value already routed through the sanitiser is fine however it was
	// built, which keeps the guard from punishing the correct spelling.
	if strings.Contains(rendered, "SafeString") || strings.Contains(rendered, "SafeAttr") {
		return ""
	}
	for _, root := range requestDerivedRoots {
		if strings.Contains(rendered, root) {
			return root
		}
	}
	return ""
}

// renderExpr flattens an expression to its source-like text.
//
// Only the node shapes that can carry a request value are handled -- selectors,
// calls, identifiers, and string concatenation. Anything else renders empty,
// which is safe: the guard is a backstop for the obvious spelling, and the
// tests above pin what it must catch.
func renderExpr(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return renderExpr(node.X) + "." + node.Sel.Name
	case *ast.CallExpr:
		parts := make([]string, 0, len(node.Args)+1)
		parts = append(parts, renderExpr(node.Fun))
		for _, arg := range node.Args {
			parts = append(parts, renderExpr(arg))
		}
		return strings.Join(parts, " ")
	case *ast.BinaryExpr:
		return renderExpr(node.X) + " " + renderExpr(node.Y)
	case *ast.ParenExpr:
		return renderExpr(node.X)
	case *ast.IndexExpr:
		return renderExpr(node.X)
	default:
		return ""
	}
}
