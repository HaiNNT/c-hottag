package notify

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNotifyImportsOnlyTheStandardLibrary keeps an email, an org, a token or
// a path structurally out of every notice: this package can't import the
// store, creds or status packages that hold them, and can't reach the
// network (spec §4 "Content").
func TestNotifyImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(p, "github.com/HaiNNT/c-hottag/") || p == "net" || strings.HasPrefix(p, "net/") {
				t.Errorf("%s imports %q: internal/notify imports only the standard library, and no network package", f, p)
			}
		}
	}
	if checked < 5 {
		t.Fatalf("checked %d files, want the package's 5 non-test files", checked)
	}
}
