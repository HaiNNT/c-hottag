package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
	"github.com/HaiNNT/c-hottag/internal/exit"
)

// exitConstValues names every internal/exit constant this scan recognises
// by its identifier, e.g. "Error" -> exit.Error. It is a literal map, not
// an AST read of internal/exit, so a renamed or added constant fails this
// package to compile rather than silently going unrecognised.
var exitConstValues = map[string]int{
	"OK":         exit.OK,
	"Error":      exit.Error,
	"Usage":      exit.Usage,
	"UserAction": exit.UserAction,
}

// errCodeConstNames is report.go's own errCode constants, by Go identifier
// ("codeUsage") to wire value ("usage"): a small AST read of this package's
// own const block, so the scan below recognises exactly the same set
// mustFailure and errCodes already close over.
func errCodeConstNames(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	eachCLIFile(t, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "errCode" {
				return true
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, _ := strconv.Unquote(lit.Value)
				out[name.Name] = s
			}
			return true
		})
	})
	if len(out) == 0 {
		t.Fatal("report.go declares no errCode constants")
	}
	return out
}

// eachCLIFile parses every non-test .go file directly in internal/cli.
func eachCLIFile(t *testing.T, fn func(*ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		fn(f)
	}
}

// selectorExit reports the exit value e names when e is a bare "exit.X"
// selector on a recognised constant, and whether it matched.
func selectorExit(e ast.Expr) (int, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exit" {
		return 0, false
	}
	v, ok := exitConstValues[sel.Sel.Name]
	return v, ok
}

// identCode reports the errCode value e names when e is a bare identifier
// naming one of names's known code constants.
func identCode(e ast.Expr, names map[string]string) (string, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return "", false
	}
	v, ok := names[id.Name]
	return v, ok
}

// codeExitSite is one place the scan found a code paired with an exit, kept
// for a clear failure message when two sites disagree.
type codeExitSite struct {
	pos  string
	exit int
}

// scanCodeExitPairs walks internal/cli's own non-test sources for every
// call site that pairs an exit with an errCode, in the three shapes the
// package uses (T3 fix round 1, review finding: no prior test pinned this
// column, and two code paths had drifted from the docs undetected):
//   - reporter.Fail / reporter.FailNoText's first two arguments;
//   - newDaemonFailure's first two arguments (also covers its wrappers,
//     internalFailure/foreignDaemonFailure/unreadableRecordFailure, whose
//     own bodies are exactly such a call);
//   - a &daemonFailure{...} (or daemonFailure{...}) composite literal's
//     "exit" and "code" keyed fields (asStopFailed, waitForSupervisorRelaunch).
//
// A site whose exit is a literal exit.X and whose code is a literal errCode
// constant is recorded in pairs. Every other site of those shapes is
// returned in unscanned, keyed by its enclosing function
// ("file.go:Func" or "file.go:Type.Method"): the scan cannot see what it
// pairs, so TestExitCodeScanSeesEveryCallSite requires each such function
// to be a reviewed entry in forwardingSites (part 5: before this, a
// non-literal site was skipped silently).
func scanCodeExitPairs(t *testing.T) (pairs map[string][]codeExitSite, unscanned map[string][]string) {
	t.Helper()
	names := errCodeConstNames(t)
	pairs, unscanned = map[string][]codeExitSite{}, map[string][]string{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := name + ":" + funcKey(fd)
			record := func(pos token.Pos, codeArg, exitArg ast.Expr) {
				code, codeOK := identCode(codeArg, names)
				ev, exitOK := selectorExit(exitArg)
				if codeOK && exitOK {
					pairs[code] = append(pairs[code], codeExitSite{pos: fset.Position(pos).String(), exit: ev})
					return
				}
				unscanned[key] = append(unscanned[key], fset.Position(pos).String())
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					var fname string
					switch fn := x.Fun.(type) {
					case *ast.Ident:
						fname = fn.Name
					case *ast.SelectorExpr:
						fname = fn.Sel.Name
					}
					if (fname == "Fail" || fname == "FailNoText" || fname == "newDaemonFailure") && len(x.Args) >= 2 {
						record(x.Pos(), x.Args[1], x.Args[0])
					}
				case *ast.CompositeLit:
					id, ok := x.Type.(*ast.Ident)
					if !ok || id.Name != "daemonFailure" {
						return true
					}
					var codeArg, exitArg ast.Expr
					for _, elt := range x.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						switch key.Name {
						case "code":
							codeArg = kv.Value
						case "exit":
							exitArg = kv.Value
						}
					}
					record(x.Pos(), codeArg, exitArg)
				}
				return true
			})
		}
	}
	return pairs, unscanned
}

// funcKey names fd as "Func", or "Type.Method" for a method.
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	rt := fd.Recv.List[0].Type
	if s, ok := rt.(*ast.StarExpr); ok {
		rt = s.X
	}
	if id, ok := rt.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// forwardingSites are the functions whose exit/code site the scan cannot
// read, each reviewed by hand (part 5). The value says why that site is
// safe to leave out of the scan. Add a function here only with a reason
// of the same kind; otherwise pass literal exit.X and errCode constants.
var forwardingSites = map[string]string{
	"daemon_common.go:newDaemonFailure":     "builds a daemonFailure from its own parameters; each caller's literal pair is recorded at its call",
	"daemon_common.go:daemonFailure.report": "relays f.exit and f.code, recorded where f was built",
	"report.go:reporter.Fail":               "forwards its own parameters to FailNoText; each caller's literal pair is recorded at its call",
	"report.go:reporter.FailErr":            "reports codeFor's result with exit.Error; codeExitException pins those codes",
	"report.go:reporter.Relay":              "relays a nested reporter's recorded failure; its fallback reports usage for exit 2 and internal otherwise, for a nested command that returned without reporting",
}

// TestExitCodeScanSeesEveryCallSite: every exit/code site the scan cannot
// read sits in a function forwardingSites reviewed, and every reviewed
// function still has such a site, so the list can't go stale.
func TestExitCodeScanSeesEveryCallSite(t *testing.T) {
	_, unscanned := scanCodeExitPairs(t)
	for key, pos := range unscanned {
		if _, ok := forwardingSites[key]; !ok {
			t.Errorf("%s pairs an exit and an errCode the doc check cannot read (%s): use literal exit.X and errCode constants, or add %q to forwardingSites with why it is safe", key, strings.Join(pos, ", "), key)
		}
	}
	for key := range forwardingSites {
		if len(unscanned[key]) == 0 {
			t.Errorf("forwardingSites lists %q, which no longer has an unreadable exit/code site: remove it", key)
		}
	}
}

// codeExitException is codeFor's own mapping (report.go): unknown_account
// and ambiguous_account never appear as a literal exit.X/codeY pair at a
// call site (codeFor returns a variable, always reported through
// reporter.FailErr, which always uses exit.Error) — a small, stable,
// hand-verified exception to the general scan above, not a gap in it.
var codeExitException = map[string]int{
	"unknown_account":   exit.Error,
	"ambiguous_account": exit.Error,
}

// TestDocsErrorCodesMatchTheirRealExit (Review Focus, fix round 1): every
// errCode's exit in docs/commands.md's "## Error codes" table is the exit
// every real call site in internal/cli actually uses for it — not merely a
// plausible number nobody checked. A code whose call sites disagree with
// each other, or with codeExitException, fails loudly rather than picking
// one silently.
func TestDocsErrorCodesMatchTheirRealExit(t *testing.T) {
	names := errCodeConstNames(t)
	sites, _ := scanCodeExitPairs(t)

	resolved := map[string]int{}
	for goName, code := range names {
		sitesForCode := sites[code]
		if len(sitesForCode) == 0 {
			if ev, ok := codeExitException[code]; ok {
				resolved[code] = ev
				continue
			}
			t.Errorf("%s (%q): the scan found no exit.X/%s pairing at any call site; add one, or add it to codeExitException with why", goName, code, goName)
			continue
		}
		want := sitesForCode[0].exit
		for _, s := range sitesForCode[1:] {
			if s.exit != want {
				t.Errorf("%q is reported with conflicting exit codes: %d at %s, %d at %s", code, want, sitesForCode[0].pos, s.exit, s.pos)
			}
		}
		resolved[code] = want
	}

	got := docsErrorTableExits(t)
	var codes []string
	for c := range resolved {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		ev, ok := got[c]
		if !ok {
			t.Errorf("## Error codes has no row for %q", c)
			continue
		}
		if ev != resolved[c] {
			t.Errorf("## Error codes lists %q as exit %d; the code actually exits %d", c, ev, resolved[c])
		}
	}
	for c := range got {
		if _, ok := resolved[c]; !ok {
			t.Errorf("## Error codes lists %q, which is not a real errCode constant", c)
		}
	}
}

var errTableRow = regexp.MustCompile("^\\|\\s*`([a-z_]+)`\\s*\\|\\s*([0-9]+)\\s*\\|")

// docsErrorTableExits reads docs/commands.md's "## Error codes" table as
// code -> the exit column's number.
func docsErrorTableExits(t *testing.T) map[string]int {
	t.Helper()
	s, ok := doccheck.Find(docsRead(t), 2, "Error codes")
	if !ok {
		t.Fatal("docs/commands.md has no ## Error codes")
	}
	out := map[string]int{}
	for _, l := range strings.Split(s.Lead, "\n") {
		m := errTableRow.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("## Error codes: exit column %q does not parse: %v", m[2], err)
		}
		out[m[1]] = n
	}
	if len(out) == 0 {
		t.Fatal("## Error codes has no rows this regexp recognises")
	}
	return out
}

// TestScanCodeExitPairsFindsKnownConflicts is a canary on the scanner
// itself: if a future report.go refactor changes the call shapes above
// enough that the scan silently stops matching anything, this fails
// loudly instead of TestDocsErrorCodesMatchTheirRealExit quietly checking
// nothing.
func TestScanCodeExitPairsFindsKnownConflicts(t *testing.T) {
	sites, _ := scanCodeExitPairs(t)
	for _, code := range []string{"usage", "internal", "role_held", "supervised", "doctor_problems"} {
		if len(sites[code]) == 0 {
			t.Errorf("the scan found no call site for %q; it should have found at least one", code)
		}
	}
	if got := fmt.Sprintf("%d", len(sites)); got == "0" {
		t.Fatal("the scan found no code/exit pairs at all")
	}
}
