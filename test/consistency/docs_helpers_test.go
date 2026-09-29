package consistency

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Shared helpers for the docs tests (part 3). Every other new test file in
// this package uses these and declares only names prefixed by its page.

// docPages is every public Markdown page a user reads, as slash paths from
// the repo root: README.md, docs/*.md and docs/migrating/*.md, in that order.
// docs/internal/ and docs/release-notes/ are not pages (part 3).
func docPages(t *testing.T) []string {
	t.Helper()
	pages := []string{"README.md"}
	for _, pat := range []string{"docs/*.md", "docs/migrating/*.md"} {
		m, err := filepath.Glob(filepath.Join(root(t), filepath.FromSlash(pat)))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(m)
		for _, p := range m {
			rel, _ := filepath.Rel(root(t), p)
			pages = append(pages, filepath.ToSlash(rel))
		}
	}
	return pages
}

// flat collapses every run of white space, newlines included, to one space,
// so a phrase a page wraps across lines still matches.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// requirePhrases fails for each phrase the page rel does not contain
// (compared flat).
func requirePhrases(t *testing.T, rel string, phrases ...string) {
	t.Helper()
	text := flat(read(t, rel))
	for _, p := range phrases {
		if !strings.Contains(text, flat(p)) {
			t.Errorf("%s lacks %q", rel, p)
		}
	}
}

// firstCells returns the first cell of each table row in body whose first
// cell is one code span: "| `--force` | …" gives "--force".
var firstCellCode = regexp.MustCompile("^\\| `([^`]+)`")

func firstCells(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if m := firstCellCode.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// tableRows returns each data row of the Markdown tables in body as its
// trimmed cells: header rows (the line before a |---| separator) and
// separators are skipped.
func tableRows(body string) [][]string {
	lines := strings.Split(body, "\n")
	sep := regexp.MustCompile(`^\|[\s:|-]+\|$`)
	var out [][]string
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "|") || sep.MatchString(l) {
			continue
		}
		if i+1 < len(lines) && sep.MatchString(strings.TrimSpace(lines[i+1])) {
			continue // a header row
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(l, "|"), "|") {
			cells = append(cells, strings.TrimSpace(c))
		}
		out = append(out, cells)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intConsts returns the integer constants declared in dir's non-test files.
func intConsts(t *testing.T, dir string) map[string]int {
	t.Helper()
	out := map[string]int{}
	eachFile(t, dir, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
						v, _ := strconv.Atoi(lit.Value)
						out[name.Name] = v
					}
				}
			}
			return true
		})
	})
	return out
}

// eachFile parses every non-test .go file in the repo-relative dir.
func eachFile(t *testing.T, dir string, fn func(*ast.File)) {
	t.Helper()
	abs := filepath.Join(root(t), filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(abs, e.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		fn(f)
	}
}

// stringConsts returns the values of dir's string constants of type typ.
func stringConsts(t *testing.T, dir, typ string) []string {
	t.Helper()
	var out []string
	eachFile(t, dir, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != typ {
				return true
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, _ := strconv.Unquote(lit.Value)
					out = append(out, s)
				}
			}
			return true
		})
	})
	return out
}
