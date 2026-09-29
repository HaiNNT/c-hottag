package doctor

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestDoctorImportsNoCredentialNetworkOrCLIPackage is spec §2.4 as a
// structural rule. Doctor reaches the network, a process start and every
// cli-owned helper only through Env's funcs. It does not import any
// credential-reading package directly: internal/status (T5) is imported
// only for the recorded TokenState, and (M2c row 15) TracedVersion.
func TestDoctorImportsNoCredentialNetworkOrCLIPackage(t *testing.T) {
	forbidden := map[string]string{
		"github.com/HaiNNT/c-hottag/internal/cli":     "an import cycle: cli imports doctor; reach cli through Env",
		"github.com/HaiNNT/c-hottag/internal/creds":   "doctor never reads a credential (spec §2.4, D8)",
		"github.com/HaiNNT/c-hottag/internal/tokens":  "doctor never reads or refreshes a token (spec §2.4)",
		"github.com/HaiNNT/c-hottag/internal/refresh": "doctor never refreshes a token (spec §2.4)",
		"net/http": "doctor never contacts the network; the health probe is Env.ProbeHealth",
		"os/exec":  "doctor never runs a program",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if why, bad := forbidden[p]; bad {
				t.Errorf("%s imports %s: %s", name, p, why)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no source file: the walk is broken")
	}
}
