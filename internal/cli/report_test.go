package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

type reportSample struct {
	Serving  string `json:"serving"`
	Previous string `json:"previous"`
}

func TestReporterTextModeWritesTextAndNoDocument(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(false, &out, &errb)
	r.Text("serving: %s\n", "B")
	if code := r.OK(reportSample{"B", "A"}); code != exit.OK {
		t.Fatalf("OK = %d, want %d", code, exit.OK)
	}
	if out.String() != "serving: B\n" || errb.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want only the text line", out.String(), errb.String())
	}
}

func TestReporterJSONModeSuppressesTextAndWritesOneDocument(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(true, &out, &errb)
	r.Text("serving: %s\n", "B")
	fmt.Fprint(r.Stdout(), "also suppressed\n")
	if code := r.OK(reportSample{"B", "A"}); code != exit.OK {
		t.Fatalf("OK = %d", code)
	}
	want := "{\n  \"version\": 1,\n  \"ok\": true,\n  \"warnings\": [],\n  \"serving\": \"B\",\n  \"previous\": \"A\"\n}\n"
	if out.String() != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
}

// The splice must leave everything after the header exactly as
// json.MarshalIndent renders it: status --json's golden depends on it.
func TestReporterKeepsMarshalIndentBytesAfterTheHeader(t *testing.T) {
	type inner struct {
		A []int             `json:"a"`
		M map[string]string `json:"m"`
		E []string          `json:"e"`
		Z *int              `json:"z,omitempty"`
	}
	type payload struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
		In      inner  `json:"in"`
		HTML    string `json:"html"`
	}
	p := payload{Version: 1, Name: "x", In: inner{A: []int{1, 2}, M: map[string]string{"k": "v", "a": "b"}, E: []string{}}, HTML: "<a&b>"}
	indented, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(indented), "\"version\": 1,\n", "\"version\": 1,\n  \"ok\": true,\n  \"warnings\": [],\n", 1) + "\n"

	var out bytes.Buffer
	newReporter(true, &out, io.Discard).OK(p)
	if out.String() != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
}

// A payload's own version moves into the header, once.
func TestReporterTakesThePayloadsVersionIntoTheHeader(t *testing.T) {
	type payload struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
	}
	var out bytes.Buffer
	newReporter(true, &out, io.Discard).OK(payload{Version: 7, Name: "x"})
	if !strings.HasPrefix(out.String(), "{\n  \"version\": 7,\n  \"ok\": true,\n") {
		t.Fatalf("stdout = %q, want the payload's version 7 first", out.String())
	}
	if n := strings.Count(out.String(), "\"version\""); n != 1 {
		t.Fatalf("\"version\" appears %d times, want once:\n%s", n, out.String())
	}
}

func TestReporterRefusesAPayloadThatCollidesWithTheHeader(t *testing.T) {
	type payload struct {
		OK bool `json:"ok"`
	}
	var out, errb bytes.Buffer
	if code := newReporter(true, &out, &errb).OK(payload{true}); code != exit.Error {
		t.Fatalf("OK = %d, want %d for a colliding field", code, exit.Error)
	}
	doc := decodeReportDoc(t, out.String())
	if e, _ := doc["error"].(map[string]any); e["code"] != "internal" {
		t.Fatalf("doc = %v, want an internal error document", doc)
	}
}

func TestReporterWarnWritesStderrAndCollectsAWarning(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(true, &out, &errb)
	r.Warn(warnLimited, "chottag: warning: B is limited")
	r.TextWarn(warnLiveSessions, "chottag: 1 live claude session(s) will see a brief interruption")
	r.OK(nil)
	if errb.String() != "chottag: warning: B is limited\n" {
		t.Errorf("stderr = %q", errb.String())
	}
	want := "{\n  \"version\": 1,\n  \"ok\": true,\n  \"warnings\": [\n" +
		"    {\n      \"code\": \"limited\",\n      \"message\": \"B is limited\"\n    },\n" +
		"    {\n      \"code\": \"live_sessions\",\n      \"message\": \"1 live claude session(s) will see a brief interruption\"\n    }\n" +
		"  ]\n}\n"
	if out.String() != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TextWarn keeps a line that is on stdout today on stdout in text mode.
func TestReporterTextWarnStaysOnStdoutInTextMode(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(false, &out, &errb)
	r.TextWarn(warnNewDaemon, "chottag: note: a new daemon (pid 5151) has already started")
	if out.String() != "chottag: note: a new daemon (pid 5151) has already started\n" || errb.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errb.String())
	}
}

func TestReporterFailWritesTheSameStderrTextInBothModes(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		var out, errb bytes.Buffer
		r := newReporter(jsonMode, &out, &errb)
		if code := r.Fail(exit.Error, codeUnknownAccount, `no such account: "nope"`, nil); code != exit.Error {
			t.Fatalf("json=%v: Fail = %d", jsonMode, code)
		}
		if errb.String() != "chottag: no such account: \"nope\"\n" {
			t.Errorf("json=%v: stderr = %q", jsonMode, errb.String())
		}
		if !jsonMode && out.Len() != 0 {
			t.Errorf("text mode wrote stdout %q", out.String())
		}
	}
}

func TestReporterErrorDocumentShape(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(true, &out, &errb)
	r.Warn(warnLimited, "chottag: warning: B is limited")
	code := r.Fail(exit.UserAction, codeNoCandidate, "no account to switch to",
		map[string]any{"skipped": []map[string]string{{"name": "B", "reason": "limited"}}})
	if code != exit.UserAction {
		t.Fatalf("Fail = %d", code)
	}
	want := "{\n  \"version\": 1,\n  \"ok\": false,\n  \"warnings\": [\n" +
		"    {\n      \"code\": \"limited\",\n      \"message\": \"B is limited\"\n    }\n  ],\n" +
		"  \"error\": {\n    \"code\": \"no_candidate\",\n    \"message\": \"no account to switch to\",\n    \"exit\": 3,\n" +
		"    \"skipped\": [\n      {\n        \"name\": \"B\",\n        \"reason\": \"limited\"\n      }\n    ]\n  }\n}\n"
	if out.String() != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
	if errb.String() != "chottag: warning: B is limited\nchottag: no account to switch to\n" {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestReporterFailNoTextWritesNoStderr(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(true, &out, &errb)
	r.FailNoText(exit.Usage, codeUsage, "flag provided but not defined: -x", nil)
	if errb.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: the caller already printed its text", errb.String())
	}
	if e, _ := decodeReportDoc(t, out.String())["error"].(map[string]any); e["code"] != "usage" || e["exit"] != float64(2) {
		t.Errorf("error = %v", e)
	}
}

func TestReporterUsageAndFlagError(t *testing.T) {
	var errb bytes.Buffer
	r := newReporter(false, io.Discard, &errb)
	if code := r.Usage("usage: chottag rotate <name> [on|off]"); code != exit.Usage {
		t.Fatalf("Usage = %d", code)
	}
	if errb.String() != "usage: chottag rotate <name> [on|off]\n" {
		t.Errorf("stderr = %q", errb.String())
	}
	var errb2 bytes.Buffer
	if code := newReporter(false, io.Discard, &errb2).FlagError(errors.New("flag provided but not defined: -x")); code != exit.Usage || errb2.Len() != 0 {
		t.Errorf("FlagError = %d, stderr %q; want 2 and nothing (flag already printed)", code, errb2.String())
	}
}

func TestReporterRefusesASecondDocument(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		r := newReporter(jsonMode, io.Discard, io.Discard)
		r.OK(nil)
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("json=%v: a second document did not panic", jsonMode)
				}
			}()
			r.Fail(exit.Error, codeInternal, "again", nil)
		}()
	}
}

func TestReporterPanicsOnACodeOutsideTheClosedList(t *testing.T) {
	for name, f := range map[string]func(r *reporter){
		"fail": func(r *reporter) { r.Fail(exit.Error, errCode("made_up"), "x", nil) },
		"warn": func(r *reporter) { r.Warn(warnCode("made_up"), "x") },
		"ok_0": func(r *reporter) { r.FailNoText(exit.OK, codeInternal, "x", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("did not panic")
				}
			}()
			f(newReporter(true, io.Discard, io.Discard))
		})
	}
}

func TestReporterFinishFallsBackOnlyInJSONModeWithNothingWritten(t *testing.T) {
	var out, errb bytes.Buffer
	if code := newReporter(true, &out, &errb).finish(0); code != exit.Error {
		t.Fatalf("finish = %d, want %d", code, exit.Error)
	}
	e, _ := decodeReportDoc(t, out.String())["error"].(map[string]any)
	if e["code"] != "internal" || e["commandExit"] != float64(0) {
		t.Fatalf("error = %v, want internal with commandExit 0", e)
	}
	if !strings.Contains(errb.String(), "chottag: internal error") {
		t.Errorf("stderr = %q", errb.String())
	}
	if code := newReporter(false, io.Discard, io.Discard).finish(3); code != 3 {
		t.Errorf("text finish = %d, want the command's own 3", code)
	}
	r := newReporter(true, io.Discard, io.Discard)
	r.OK(nil)
	if code := r.finish(0); code != 0 {
		t.Errorf("finish after a document = %d, want 0", code)
	}
}

func TestReporterNestedRecordsAndRelayReports(t *testing.T) {
	var out, errb bytes.Buffer
	r := newReporter(true, &out, &errb)
	n := r.Nested()
	n.Warn(warnNameNoSlot, "chottag: --name X=Y: no such slot directory (found: )")
	if code := n.Fail(exit.Usage, codeUsage, "bad flag", map[string]any{"flag": "-x"}); code != exit.Usage {
		t.Fatalf("nested Fail = %d", code)
	}
	if out.Len() != 0 {
		t.Fatalf("a nested reporter wrote a document: %q", out.String())
	}
	if code := r.Relay(n, exit.Usage); code != exit.Usage {
		t.Fatalf("Relay = %d", code)
	}
	doc := decodeReportDoc(t, out.String())
	e, _ := doc["error"].(map[string]any)
	if e["code"] != "usage" || e["message"] != "bad flag" || e["flag"] != "-x" {
		t.Errorf("error = %v", e)
	}
	if ws, _ := doc["warnings"].([]any); len(ws) != 1 {
		t.Errorf("warnings = %v, want the nested warning", doc["warnings"])
	}
	if n := strings.Count(errb.String(), "chottag: bad flag"); n != 1 {
		t.Errorf("stderr = %q: the failure text must appear exactly once", errb.String())
	}

	// A nested command that returned a code without reporting (one not yet
	// converted to the reporter) is relayed by its code.
	for code, want := range map[int]string{exit.Usage: "usage", exit.Error: "internal"} {
		var o bytes.Buffer
		p := newReporter(true, &o, io.Discard)
		if got := p.Relay(p.Nested(), code); got != code {
			t.Errorf("Relay(unreported, %d) = %d", code, got)
		}
		if e, _ := decodeReportDoc(t, o.String())["error"].(map[string]any); e["code"] != want {
			t.Errorf("Relay(unreported, %d) code = %v, want %s", code, e["code"], want)
		}
	}
}

func TestReporterNestedOKWritesNothing(t *testing.T) {
	var out bytes.Buffer
	r := newReporter(true, &out, io.Discard)
	if code := r.Nested().OK(reportSample{"B", "A"}); code != exit.OK || out.Len() != 0 {
		t.Fatalf("nested OK = %d, stdout %q; want 0 and nothing", code, out.String())
	}
	r.OK(nil) // the parent still writes its own one document
	decodeReportDoc(t, out.String())
}

func TestReporterChildStdout(t *testing.T) {
	var out, errb bytes.Buffer
	fmt.Fprint(newReporter(false, &out, &errb).ChildStdout(), "a")
	fmt.Fprint(newReporter(true, &out, &errb).ChildStdout(), "b")
	if out.String() != "a" || errb.String() != "b" {
		t.Fatalf("stdout=%q stderr=%q; want a child's stdout on stdout in text mode and on stderr in JSON mode", out.String(), errb.String())
	}
}

func TestCodeForClassifiesStoreErrors(t *testing.T) {
	for err, want := range map[error]errCode{
		fmt.Errorf("%w: %q", store.ErrNotFound, "x"):                  codeUnknownAccount,
		fmt.Errorf("%w: %q matches A and B", store.ErrAmbiguous, "a"): codeAmbiguousAccount,
		errors.New("state.json is corrupt"):                           codeInternal,
	} {
		if got := codeFor(err); got != want {
			t.Errorf("codeFor(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestHumanMessageStripsThePrefixes(t *testing.T) {
	for line, want := range map[string]string{
		"chottag: warning: B is limited":          "B is limited",
		"chottag: note: the upstream changes":     "the upstream changes",
		"chottag: owners.json was corrupt":        "owners.json was corrupt",
		"A's login is outside /h and was left in": "A's login is outside /h and was left in",
	} {
		if got := humanMessage(line); got != want {
			t.Errorf("humanMessage(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestReporterCallSitesNeverSpellACodeAsALiteral pins every Fail,
// FailNoText, Warn and TextWarn call in the package's production files to a
// named code (spec §5.3: "A test pins every Fail call site to the list").
// A named constant is checked against the list at compile time by its
// type; a literal would compile (untyped string constants convert) and
// bypass it. The reporter's own runtime check catches a code that reaches
// Fail through a variable.
func TestReporterCallSitesNeverSpellACodeAsALiteral(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx := -1
			switch sel.Sel.Name {
			case "Fail", "FailNoText":
				idx = 1
			case "Warn", "TextWarn", "RecordWarn":
				idx = 0
			}
			if idx < 0 || len(call.Args) <= idx {
				return true
			}
			calls++
			switch call.Args[idx].(type) {
			case *ast.Ident, *ast.SelectorExpr:
			default:
				t.Errorf("%s: %s's code is a %T; name a code constant from report.go's closed list", fset.Position(call.Pos()), sel.Sel.Name, call.Args[idx])
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatal("found no reporter call sites at all: the walk is broken")
	}
}

// The constants and the closed lists must name the same set, so a code
// cannot be declared without being listed (or listed without a constant).
func TestCodeConstantsMatchTheClosedLists(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "report.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string][]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			typ, ok := vs.Type.(*ast.Ident)
			if !ok || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			declared[typ.Name] = append(declared[typ.Name], v)
		}
	}
	snake := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	var errs, warns []string
	for _, c := range errCodes {
		errs = append(errs, string(c))
	}
	for _, c := range warnCodes {
		warns = append(warns, string(c))
	}
	for typ, listed := range map[string][]string{"errCode": errs, "warnCode": warns} {
		got := slices.Clone(declared[typ])
		slices.Sort(got)
		want := slices.Clone(listed)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s constants %v != closed list %v", typ, got, want)
		}
		if len(slices.Compact(slices.Clone(want))) != len(want) {
			t.Errorf("%s closed list has a duplicate: %v", typ, want)
		}
		for _, v := range want {
			if !snake.MatchString(v) {
				t.Errorf("%s %q is not snake_case", typ, v)
			}
		}
	}
}

// decodeReportDoc decodes one JSON object from s and fails the test if
// anything else is there.
func decodeReportDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, s)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("more than one JSON value:\n%s", s)
	}
	return doc
}
