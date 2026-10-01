package cli

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
	"github.com/HaiNNT/c-hottag/internal/doctor"
	"github.com/HaiNNT/c-hottag/internal/exit"
)

// docsCommandsPath is the command reference, from this package's directory.
const docsCommandsPath = "../../docs/commands.md"

// docsResultTypes is the Go type each `chottag PATH` section of
// docs/commands.md documents with its first ```json block (P3-R1).
var docsResultTypes = map[string]any{
	"version":        versionResult{},
	"setup":          setupResult{},
	"uninstall":      uninstallResult{},
	"adopt":          adoptResult{},
	"login":          loginResult{},
	"logout":         logoutResult{},
	"tag":            tagResult{},
	"next":           nextResult{},
	"remote":         remoteResult{},
	"own":            ownResult{},
	"rotate":         rotateResult{},
	"rename":         renameResult{},
	"notify":         notifyResult{},
	"policy":         policyResult{},
	"pool":           poolListResult{},
	"pool add":       poolNameResult{},
	"pool join":      poolMemberResult{},
	"pool leave":     poolMemberResult{},
	"pool rm":        poolNameResult{},
	"auto":           autoResult{},
	"plan":           planResult{},
	"status":         statusDocument{},
	"statusline":     statuslineResult{},
	"doctor":         doctorResult{},
	"daemon start":   startResult{},
	"daemon stop":    stopResult{},
	"daemon restart": restartResult{},
	"update":         updateResult{},
	"trace":          traceResult{},
	"trace on":       traceResult{},
}

// docsNoJSON are the sections whose command refuses --json (refuseJSON):
// each says "does not support `--json`" and shows no JSON block.
var docsNoJSON = map[string]bool{
	"proxy run": true, "daemon run": true, "daemon logs": true,
	"trace run": true, "trace env": true, "trace mark": true, "trace summarize": true,
}

// docsHidden are fields a command never outputs, so its example must not
// show them: status clears each account's slot dir before it reports
// (status.go, F171).
var docsHidden = map[string]bool{"Account.dir": true}

var docsHeading = regexp.MustCompile("^`chottag ([a-z][a-z -]*[a-z])`$")

func docsRead(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(docsCommandsPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func docsSections(t *testing.T) map[string]doccheck.Section {
	t.Helper()
	out := map[string]doccheck.Section{}
	for _, s := range doccheck.Sections(docsRead(t)) {
		if m := docsHeading.FindStringSubmatch(s.Title); s.Level == 3 && m != nil {
			out[m[1]] = s
		}
	}
	return out
}

func docsDecode(t *testing.T, where, block string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(block), &v); err != nil {
		t.Fatalf("%s: the JSON example does not parse: %v", where, err)
	}
	return v
}

func docsKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// docsHasField reports whether t (a struct) has a JSON field named name.
func docsHasField(t reflect.Type, name string) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == name {
			return true
		}
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct && docsHasField(f.Type, name) {
			return true
		}
	}
	return false
}

// TestDocsCommandSectionsDocumentTheirJSON (P3-R1): each section's first
// JSON example is a success document whose fields, below the header, are
// exactly its command's result type's, at every depth; a command that
// refuses --json says so instead.
func TestDocsCommandSectionsDocumentTheirJSON(t *testing.T) {
	for path, s := range docsSections(t) {
		blocks := doccheck.Fenced(s.Body, "json")
		if docsNoJSON[path] {
			if !strings.Contains(s.Body, "does not support `--json`") || len(blocks) != 0 {
				t.Errorf("`chottag %s` refuses --json: its section must say \"does not support `--json`\" and show no JSON", path)
			}
			continue
		}
		v, ok := docsResultTypes[path]
		if !ok {
			t.Errorf("`chottag %s`: no result type in docsResultTypes; add it (or to docsNoJSON)", path)
			continue
		}
		if len(blocks) == 0 {
			t.Errorf("`chottag %s` has no ```json example", path)
			continue
		}
		doc := docsDecode(t, "`chottag "+path+"`", blocks[0])
		if doc["ok"] != true {
			t.Errorf("`chottag %s`: the first JSON example is not a success document (ok: true)", path)
		}
		typ := reflect.TypeOf(v)
		delete(doc, "ok")
		delete(doc, "warnings")
		if !docsHasField(typ, "version") {
			delete(doc, "version")
		}
		for _, d := range doccheck.Shape(doc, typ, docsHidden) {
			t.Errorf("`chottag %s` JSON example: %s", path, d)
		}
	}
}

// TestDocsEveryResultTypeIsMapped: a new *Result type (a new command's JSON)
// fails until docsResultTypes, and so docs/commands.md, covers it.
func TestDocsEveryResultTypeIsMapped(t *testing.T) {
	mapped := map[string]bool{}
	for _, v := range docsResultTypes {
		mapped[reflect.TypeOf(v).Name()] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, isStruct := ts.Type.(*ast.StructType); isStruct && strings.HasSuffix(ts.Name.Name, "Result") && !mapped[ts.Name.Name] {
				t.Errorf("%s: %s is not in docsResultTypes, so docs/commands.md never documents it", e.Name(), ts.Name.Name)
			}
			return true
		})
	}
}

// TestDocsJSONEnvelopeMatchesTheReporter: "## JSON output" shows a success
// and an error document with exactly the header the reporter writes.
func TestDocsJSONEnvelopeMatchesTheReporter(t *testing.T) {
	s, ok := doccheck.Find(docsRead(t), 2, "JSON output")
	if !ok {
		t.Fatal("docs/commands.md has no ## JSON output")
	}
	blocks := doccheck.Fenced(s.Lead, "json")
	if len(blocks) < 2 {
		t.Fatalf("## JSON output has %d JSON examples, want a success and an error document", len(blocks))
	}
	okDoc, err := successDocument([]warning{{Code: warnLimited, Message: "m"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errDoc, err := errorDocument(nil, &failure{exit: exit.Usage, code: codeUsage, message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for i, real := range [][]byte{okDoc, errDoc} {
		var want map[string]any
		if err := json.Unmarshal(real, &want); err != nil {
			t.Fatal(err)
		}
		got := docsDecode(t, "## JSON output", blocks[i])
		if g, w := strings.Join(docsKeys(got), ","), strings.Join(docsKeys(want), ","); g != w {
			t.Errorf("## JSON output example %d has keys %s; the reporter writes %s", i+1, g, w)
		}
		if i == 0 {
			ws, _ := got["warnings"].([]any)
			if len(ws) == 0 {
				t.Error("## JSON output: show one warning in the success example")
				continue
			}
			w0, _ := ws[0].(map[string]any)
			if strings.Join(docsKeys(w0), ",") != "code,message" {
				t.Errorf("## JSON output: a warning has keys %v, want code,message", docsKeys(w0))
			}
		} else {
			e, _ := got["error"].(map[string]any)
			if strings.Join(docsKeys(e), ",") != "code,exit,message" {
				t.Errorf("## JSON output: error has keys %v, want code, message and exit", docsKeys(e))
			}
		}
	}
}

// docsCodeRows is the set of first-cell code spans in the ## title table.
func docsCodeRows(t *testing.T, title string) map[string]bool {
	t.Helper()
	s, ok := doccheck.Find(docsRead(t), 2, title)
	if !ok {
		t.Fatalf("docs/commands.md has no ## %s", title)
	}
	cell := regexp.MustCompile("^\\| `([^`]+)`")
	out := map[string]bool{}
	for _, l := range strings.Split(s.Lead, "\n") {
		if m := cell.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// TestDocsListEveryErrorAndWarningCode: the two closed lists (report.go) are
// documented exactly.
func TestDocsListEveryErrorAndWarningCode(t *testing.T) {
	check := func(title string, want []string) {
		got := docsCodeRows(t, title)
		wantSet := map[string]bool{}
		for _, c := range want {
			wantSet[c] = true
			if !got[c] {
				t.Errorf("## %s lacks %q", title, c)
			}
		}
		for c := range got {
			if !wantSet[c] {
				t.Errorf("## %s lists %q, which is not a code chottag reports", title, c)
			}
		}
	}
	var errs, warns []string
	for _, c := range errCodes {
		errs = append(errs, string(c))
	}
	for _, c := range warnCodes {
		warns = append(warns, string(c))
	}
	check("Error codes", errs)
	check("Warning codes", warns)
}

// TestDocsDoctorProblemsExampleCarriesTheRows (Review Focus 5): an agent
// reading the doctor section learns that a run with problems is ok:false,
// exit 3, doctor_problems, with the rows under error.checks.
func TestDocsDoctorProblemsExampleCarriesTheRows(t *testing.T) {
	s, ok := docsSections(t)["doctor"]
	if !ok {
		t.Fatal("no `chottag doctor` section")
	}
	blocks := doccheck.Fenced(s.Body, "json")
	if len(blocks) < 2 {
		t.Fatal("the doctor section needs a second JSON example: the doctor_problems error document")
	}
	doc := docsDecode(t, "doctor problems", blocks[1])
	e, _ := doc["error"].(map[string]any)
	if doc["ok"] != false || e == nil || e["code"] != string(codeDoctorProblems) || e["exit"] != float64(exit.UserAction) {
		t.Fatalf("the second doctor example is not ok:false with error.code %q and exit %d", codeDoctorProblems, exit.UserAction)
	}
	rows, _ := e["checks"].([]any)
	if len(rows) == 0 {
		t.Fatal("the doctor_problems example shows no rows under error.checks")
	}
	if _, ok := e["problems"]; !ok {
		t.Error("the doctor_problems example lacks error.problems")
	}
	for i, r := range rows {
		for _, d := range doccheck.Shape(r, reflect.TypeFor[doctor.Row](), nil) {
			t.Errorf("error.checks[%d]: %s", i, d)
		}
	}
}

// TestDocsJSONExamplesUseFixtures (P3-R3): every email in a JSON example is
// an example.com address, and every org is Acme.
func TestDocsJSONExamplesUseFixtures(t *testing.T) {
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				s, isStr := vv.(string)
				switch {
				case isStr && strings.EqualFold(k, "email") && s != "" && !strings.HasSuffix(s, "@example.com"):
					t.Errorf("%s.%s = %q: use an @example.com fixture", path, k, s)
				case isStr && k == "org" && s != "" && s != "Acme":
					t.Errorf("%s.%s = %q: use the fixture org Acme", path, k, s)
				}
				walk(path+"."+k, vv)
			}
		case []any:
			for _, vv := range x {
				walk(path+"[]", vv)
			}
		}
	}
	for i, b := range doccheck.Fenced(docsRead(t), "json") {
		var v any
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			t.Errorf("JSON example %d does not parse: %v", i+1, err)
			continue
		}
		walk(fmt.Sprintf("example %d", i+1), v)
	}
}
