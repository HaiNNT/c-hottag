package consistency

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// configSection is the level-2 section of docs/configuration.md titled title.
func configSection(t *testing.T, title string) doccheck.Section {
	t.Helper()
	s, ok := doccheck.Find(read(t, "docs/configuration.md"), 2, title)
	if !ok {
		t.Fatalf("docs/configuration.md has no ## %s", title)
	}
	return s
}

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// productEnvVars is every environment variable chottag's product code reads
// or sets by name: the first argument of os.Getenv, os.LookupEnv, the second
// of the shim's envGet(env, …), and any "CHOTTAG_…" string literal. Test
// files and the chottag_fakeusage build (a test-only fake) are skipped.
func productEnvVars(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var dirs []string
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root(t), top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				rel, _ := filepath.Rel(root(t), p)
				dirs = append(dirs, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range dirs {
		eachFile(t, dir, func(f *ast.File) {
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if strings.HasPrefix(c.Text, "//go:build") && strings.Contains(c.Text, "chottag_fakeusage") && !strings.Contains(c.Text, "!chottag_fakeusage") {
						return // the test-only fake-usage build
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					name := ""
					switch fn := x.Fun.(type) {
					case *ast.SelectorExpr:
						name = fn.Sel.Name
					case *ast.Ident:
						name = fn.Name
					}
					argi := map[string]int{"Getenv": 0, "LookupEnv": 0, "envGet": 1}
					if i, ok := argi[name]; ok && len(x.Args) > i {
						if lit, ok := x.Args[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, _ := strconv.Unquote(lit.Value); envName.MatchString(s) {
								out[s] = true
							}
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						s, _ := strconv.Unquote(x.Value)
						s = strings.TrimSuffix(s, "=")
						if strings.HasPrefix(s, "CHOTTAG_") && envName.MatchString(s) {
							out[s] = true
						}
					}
				}
				return true
			})
		})
	}
	return out
}

// TestConfigurationDocListsEveryEnvVar (P3-R1): the environment table names
// every variable the product reads.
func TestConfigurationDocListsEveryEnvVar(t *testing.T) {
	vars := productEnvVars(t)
	if !vars["CHOTTAG_HOME"] || !vars["HTTPS_PROXY"] {
		t.Fatalf("the env scan found %v; it lost CHOTTAG_HOME or HTTPS_PROXY", sortedKeys(vars))
	}
	rows := map[string]bool{}
	for _, c := range firstCells(configSection(t, "Environment variables").Lead) {
		rows[c] = true
	}
	for _, v := range sortedKeys(vars) {
		if !rows[v] {
			t.Errorf("## Environment variables has no row for %s", v)
		}
	}
}

// configDocsHidden is a field store.State can decode but never writes, so
// the state.json example must not show it (final review item 10:
// Auto.Threshold is the pre-M4 field, never written — store.go's Auto).
var configDocsHidden = map[string]bool{"Auto.threshold": true}

// TestConfigurationDocStateJSONMatchesStoreState (P3-R1): the state.json
// example has exactly store.State's fields, at every depth.
func TestConfigurationDocStateJSONMatchesStoreState(t *testing.T) {
	blocks := doccheck.Fenced(configSection(t, "state.json").Lead, "json")
	if len(blocks) == 0 {
		t.Fatal("## state.json has no ```json example")
	}
	var doc any
	if err := json.Unmarshal([]byte(blocks[0]), &doc); err != nil {
		t.Fatalf("the state.json example does not parse: %v", err)
	}
	for _, d := range doccheck.Shape(doc, reflect.TypeFor[store.State](), configDocsHidden) {
		t.Errorf("state.json example: %s", d)
	}
}

// setupDirNames is internal/cli's setupDirs: the directories setup creates.
func setupDirNames(t *testing.T) []string {
	t.Helper()
	var out []string
	eachFile(t, "internal/cli", func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "setupDirs" {
				return true
			}
			for _, el := range vs.Values[0].(*ast.CompositeLit).Elts {
				s, _ := strconv.Unquote(el.(*ast.BasicLit).Value)
				out = append(out, s)
			}
			return false
		})
	})
	if len(out) == 0 {
		t.Fatal("internal/cli has no setupDirs")
	}
	return out
}

// TestConfigurationDocMapsTheHomeTree: the files table names every
// directory setup creates and every file chottag keeps in its home, and
// the page gives the default port.
func TestConfigurationDocMapsTheHomeTree(t *testing.T) {
	s := configSection(t, "Files")
	rows := map[string]bool{}
	for _, c := range firstCells(s.Lead) {
		rows[c] = true
	}
	want := []string{
		"state.json", "cache/status.json", "owners.json", "install.json",
		"daemon.log", "proxy.jsonl", "trace.jsonl", "versions/",
		"ca/ca.pem", "ca/ca.key", "ca/proxy.secret",
	}
	for _, d := range setupDirNames(t) {
		want = append(want, d+"/")
	}
	for _, w := range want {
		if !rows[w] {
			t.Errorf("## Files has no row for `%s`", w)
		}
	}
	if !strings.Contains(read(t, "docs/configuration.md"), strconv.Itoa(store.DefaultPort)) {
		t.Errorf("docs/configuration.md does not give the default port %d", store.DefaultPort)
	}
}
