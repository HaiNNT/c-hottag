package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// dispatchLabels reads the dispatch list from the code itself: every
// string case label of cli.go's dispatch, and of daemon.go's runDaemonCmd
// prefixed "daemon ". A command added to either switch is therefore in the
// list the moment it exists, with no second list to forget.
func dispatchLabels(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	var labels []string
	collect := func(file, fn, prefix string, skip map[string]bool) {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn {
				continue
			}
			found = true
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, e := range cc.List {
					lit, ok := e.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					if !skip[s] {
						labels = append(labels, prefix+s)
					}
				}
				return true
			})
		}
		if !found {
			t.Fatalf("%s has no func %s: the dispatch moved, update this test", file, fn)
		}
	}
	// "daemon" itself is covered verb by verb.
	collect("cli.go", "dispatch", "", map[string]bool{"daemon": true})
	collect("daemon.go", "runDaemonCmd", "daemon ", nil)
	// trace's verbs are dispatched inside runTrace (M2c): each is its own
	// label, and bare "trace" (dispatch's own case) stays one too.
	collect("trace.go", "runTrace", "trace ", nil)
	return labels
}

// TestEveryDispatchableCommandHasJSONCases is spec §5.3's completeness
// rule: the one-document harness's rows are compared with the dispatch
// list, so a new command without a row fails here.
//   - A command that refuses --json needs a json_unsupported row.
//   - version cannot fail, and an alias shares its command's code, so one
//     row each is enough.
//   - Every other command needs a success row and a failure row.
//   - A row that names a command must name a real dispatch label ("" is
//     for rows about no command at all).
func TestEveryDispatchableCommandHasJSONCases(t *testing.T) {
	labels := dispatchLabels(t)
	if len(labels) < 20 {
		t.Fatalf("found only %d dispatch labels (%v): the AST walk is broken", len(labels), labels)
	}
	refusing := map[string]bool{"proxy": true, "help": true, "-h": true, "--help": true, "daemon run": true, "daemon logs": true,
		"trace run": true, "trace env": true, "trace mark": true, "trace summarize": true}
	oneRow := map[string]bool{"version": true, "ls": true, "rc": true, "remote-control": true}

	byCommand := map[string][]jsonCase{}
	for _, c := range jsonCases {
		byCommand[c.command] = append(byCommand[c.command], c)
	}
	known := map[string]bool{"": true}
	for _, l := range labels {
		known[l] = true
		cs := byCommand[l]
		if len(cs) == 0 {
			t.Errorf("%q has no one-document case: register one (jsonharness_test.go)", l)
			continue
		}
		var success, failure, refused bool
		for _, c := range cs {
			success = success || c.wantExit == 0
			failure = failure || c.wantExit != 0
			refused = refused || c.wantCode == codeJSONUnsupported
		}
		switch {
		case refusing[l]:
			if !refused {
				t.Errorf("%q refuses --json but has no json_unsupported case", l)
			}
		case oneRow[l]:
		default:
			if !success || !failure {
				t.Errorf("%q needs a success case and a failure case (success=%v failure=%v)", l, success, failure)
			}
		}
	}
	for cmd := range byCommand {
		if !known[cmd] {
			t.Errorf("a one-document case names %q, which is not a dispatch label", cmd)
		}
	}
}
