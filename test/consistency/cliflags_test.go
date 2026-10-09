package consistency

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// usageText is internal/cli/cli.go's usage const, read as text.
func usageText(t *testing.T) string {
	t.Helper()
	src := read(t, "internal/cli/cli.go")
	const open = "const usage = `"
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatal("cli.go has no usage const")
	}
	body := src[i+len(open):]
	return body[:strings.Index(body, "`")]
}

var cmdWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// usagePaths maps each command path the usage text lists ("tag",
// "daemon logs", "proxy run") to the flags its line names, without dashes.
// A line's synopsis is the text before its first run of two spaces; its
// path is the synopsis's leading lower-case words. The "--json (global)"
// line names no command.
func usagePaths(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	// A flag starts a word: the "-session" inside the command name
	// name-session is not one.
	flagTok := regexp.MustCompile(`(?:^|[\s\[|(])(--?[a-z][a-z-]*)`)
	for _, l := range strings.Split(usageText(t), "\n") {
		rest, ok := strings.CutPrefix(l, "  ")
		if !ok {
			continue
		}
		syn, _, _ := strings.Cut(rest, "  ")
		var words []string
		for _, f := range strings.Fields(syn) {
			if !cmdWord.MatchString(f) {
				break
			}
			words = append(words, f)
		}
		if len(words) == 0 {
			continue
		}
		path := strings.Join(words, " ")
		if out[path] == nil {
			out[path] = map[string]bool{}
		}
		for _, m := range flagTok.FindAllStringSubmatch(syn, -1) {
			out[path][strings.TrimLeft(m[1], "-")] = true
		}
	}
	return out
}

// flagArg is where each flag.FlagSet method takes the flag's name.
var flagArg = map[string]int{
	"Bool": 0, "String": 0, "Int": 0, "Int64": 0, "Uint": 0, "Uint64": 0,
	"Float64": 0, "Duration": 0, "Func": 0, "BoolFunc": 0,
	"Var": 1, "BoolVar": 1, "StringVar": 1, "IntVar": 1, "Int64Var": 1,
	"UintVar": 1, "Uint64Var": 1, "Float64Var": 1, "DurationVar": 1, "TextVar": 1,
}

// cliFlagSets reads internal/cli's non-test sources and returns, for every
// flag.NewFlagSet("NAME", …) a function creates, the flags defined on that
// set in the same function, without dashes: {"tag": {"force"}, "daemon
// logs": {"n", "f"}, …}. A set created in two functions ("daemon run")
// gets the union. A flag whose name is not a string literal fails the test.
func cliFlagSets(t *testing.T) map[string]map[string]bool {
	t.Helper()
	dir := filepath.Join(root(t), "internal", "cli")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			sets := map[string]string{} // variable -> set name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
						return true
					}
					call, ok := x.Rhs[0].(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "NewFlagSet" || len(call.Args) == 0 {
						return true
					}
					lit, ok := call.Args[0].(*ast.BasicLit)
					id, idok := x.Lhs[0].(*ast.Ident)
					if !ok || lit.Kind != token.STRING || !idok {
						t.Errorf("%s: a NewFlagSet whose name is not a string literal", fset.Position(call.Pos()))
						return true
					}
					setName, _ := strconv.Unquote(lit.Value)
					sets[id.Name] = setName
					if out[setName] == nil {
						out[setName] = map[string]bool{}
					}
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					recv, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					setName, isSet := sets[recv.Name]
					argi, isFlag := flagArg[sel.Sel.Name]
					if !isSet || !isFlag || len(x.Args) <= argi {
						return true
					}
					lit, ok := x.Args[argi].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("%s: flag on %q whose name is not a string literal", fset.Position(x.Pos()), setName)
						return true
					}
					flagName, _ := strconv.Unquote(lit.Value)
					out[setName][flagName] = true
				}
				return true
			})
		}
	}
	return out
}

// cliAliases is internal/cli's helpAliases map: {"ls": "status", "rc":
// "remote", "remote-control": "remote"}.
func cliAliases(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root(t), "internal", "cli", "cli.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "helpAliases" || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("helpAliases is not a map literal")
		}
		for _, el := range lit.Elts {
			kv := el.(*ast.KeyValueExpr)
			k, _ := strconv.Unquote(kv.Key.(*ast.BasicLit).Value)
			v, _ := strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
			out[k] = v
		}
		return false
	})
	if len(out) == 0 {
		t.Fatal("cli.go has no helpAliases entries")
	}
	return out
}

// delegatedFlags: a command that hands its arguments to another command's
// flag set. None now: setup declares its own flags (claude, name, label) and
// rebuilds adopt's arguments from them.
var delegatedFlags = map[string]string{}

// globalFlagNames are accepted by every command: --json (globalflags.go)
// and the help spellings (wantsHelp, cli.go).
var globalFlagNames = map[string]bool{"json": true, "h": true, "help": true}

// knownPaths is every command path: the usage text's plus every flag set's
// ("trace on" has a flag set but shares the "trace" usage line).
func knownPaths(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for p := range usagePaths(t) {
		out[p] = true
	}
	for p := range cliFlagSets(t) {
		out[p] = true
	}
	return out
}

// flagsFor is the set of flags path accepts besides the global ones.
func flagsFor(sets map[string]map[string]bool, path string) map[string]bool {
	out := map[string]bool{}
	for f := range sets[path] {
		out[f] = true
	}
	for f := range sets[delegatedFlags[path]] {
		out[f] = true
	}
	return out
}

// chottagInvocations returns, for each `chottag …` (or …/chottag …) on a
// code line, the tokens after it up to the end of that command: a pipe, a
// list operator, a comment, a closing paren or a literal "--".
func chottagInvocations(line string) [][]string {
	var out [][]string
	fields := strings.Fields(line)
	for i, f := range fields {
		g := strings.Trim(f, "\"'`($")
		if g != "chottag" && !strings.HasSuffix(g, "/chottag") {
			continue
		}
		var args []string
		for _, a := range fields[i+1:] {
			if a == "|" || a == "||" || a == "&&" || a == ";" || a == "--" || a == ")" || strings.HasPrefix(a, "#") {
				break
			}
			args = append(args, a)
			if strings.HasSuffix(a, ";") || strings.HasSuffix(a, ")") {
				break
			}
		}
		out = append(out, args)
	}
	return out
}

// resolvePath is the command path args start with ("" when none is known).
func resolvePath(args []string, known map[string]bool, aliases map[string]string) string {
	var words []string
	for _, a := range args {
		w := strings.Trim(a, "[](){}`'\",;")
		if !cmdWord.MatchString(w) {
			break
		}
		words = append(words, w)
	}
	if len(words) == 0 {
		return ""
	}
	if a, ok := aliases[words[0]]; ok {
		words[0] = a
	}
	if len(words) >= 2 && known[words[0]+" "+words[1]] {
		return words[0] + " " + words[1]
	}
	if known[words[0]] {
		return words[0]
	}
	return ""
}

// TestDocsUseOnlyRealFlags: every flag a doc page or the skill passes to a
// chottag command in a code block or span is one that command's flag set,
// or the global layer, accepts (part 3, Review Focus 1: a reader pastes
// it).
func TestDocsUseOnlyRealFlags(t *testing.T) {
	sets := cliFlagSets(t)
	known := knownPaths(t)
	aliases := cliAliases(t)
	for _, rel := range append(docPages(t), "plugin/skills/chottag/SKILL.md") {
		for _, line := range doccheck.Code(read(t, rel)) {
			for _, args := range chottagInvocations(line) {
				path := resolvePath(args, known, aliases)
				allowed := flagsFor(sets, path)
				for _, a := range args {
					tok := strings.Trim(a, "[](){}`'\",;.")
					if len(tok) < 2 || tok[0] != '-' || tok == "--" {
						continue
					}
					name, _, _ := strings.Cut(strings.TrimLeft(tok, "-"), "=")
					if name == "" || globalFlagNames[name] || allowed[name] {
						continue
					}
					t.Errorf("%s: `chottag %s` has no flag %s (in %q)", rel, path, tok, line)
				}
			}
		}
	}
}

// TestUsageFlagsExistInTheFlagSets: every flag a usage line names is one
// the command really defines (or --json, which is global), so the usage
// text the docs are checked against is itself true.
func TestUsageFlagsExistInTheFlagSets(t *testing.T) {
	sets := cliFlagSets(t)
	for path, flags := range usagePaths(t) {
		allowed := flagsFor(sets, path)
		for set := range sets {
			if strings.HasPrefix(set, path+" ") {
				for f := range sets[set] {
					allowed[f] = true // "trace [on [--for DUR]|off]" names trace on's flag
				}
			}
		}
		for f := range flags {
			if !globalFlagNames[f] && !allowed[f] {
				t.Errorf("usage names `chottag %s --%s`, which its flag set does not define", path, f)
			}
		}
	}
}
