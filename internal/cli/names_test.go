package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

func TestNamesShowDefaultWritesNothing(t *testing.T) {
	home := notifyHome(t)
	code, out, errs := runChottag(t, "names")
	if code != exit.OK || out != "names: on (branch, then Claude's title)\n" {
		t.Fatalf("names = %d %q; stderr %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("showing wrote state.json (stat: %v)", err)
	}
}

func TestNamesSetPersistsAndIsShown(t *testing.T) {
	notifyHome(t)
	want := map[string]string{
		"model": "names: model (also a Haiku topic for a first prompt with no branch)\n",
		"off":   "names: off\n",
		"on":    "names: on (branch, then Claude's title)\n",
	}
	for _, mode := range []string{"model", "off", "on"} {
		if code, out, errs := runChottag(t, "names", mode); code != exit.OK || out != want[mode] {
			t.Fatalf("names %s = %d %q; stderr %q", mode, code, out, errs)
		}
		if code, out, _ := runChottag(t, "names"); code != exit.OK || out != want[mode] {
			t.Fatalf("names after %s = %d %q", mode, code, out)
		}
	}
}

func TestNamesBadValueAndExtraArgs(t *testing.T) {
	notifyHome(t)
	if code, _, errs := runChottag(t, "names", "maybe"); code != exit.Usage || !strings.Contains(errs, "on, model or off") {
		t.Fatalf("bad value = %d %q", code, errs)
	}
	if code, _, _ := runChottag(t, "names", "on", "off"); code != exit.Usage {
		t.Fatalf("two args = %d, want usage", code)
	}
}

func TestNamesJSON(t *testing.T) {
	notifyHome(t)
	code, out, errs := runChottag(t, "names", "model", "--json")
	if code != exit.OK {
		t.Fatalf("code %d; stderr %q", code, errs)
	}
	if doc := decodeOneDocument(t, out); doc["names"] != "model" {
		t.Fatalf("doc %v, want names model", doc)
	}
}

func TestStatusNamesLineAndJSON(t *testing.T) {
	notifyHome(t)
	_, out, _ := runChottag(t, "status")
	if strings.Contains(out, "names:") {
		t.Errorf("status shows names while on:\n%s", out)
	}
	runChottag(t, "names", "model")
	_, out, _ = runChottag(t, "status")
	if !strings.Contains(out, "names: model\n") {
		t.Errorf("status lacks names line:\n%s", out)
	}
	for _, mode := range []string{"model", "on"} {
		runChottag(t, "names", mode)
		_, out, _ = runChottag(t, "status", "--json")
		if doc := decodeOneDocument(t, out); doc["names"] != mode {
			t.Errorf("status --json names %v, want %s", doc["names"], mode)
		}
	}
}

// TestNameSessionModePrecedence: the env overrides the stored mode (0 off,
// model model); any other env value is ignored.
func TestNameSessionModePrecedence(t *testing.T) {
	cases := []struct {
		state, env string
		wantModel  bool
		wantSilent bool
	}{
		{"on", "", false, false}, {"on", "0", false, true}, {"on", "model", true, false}, {"on", "junk", false, false},
		{"model", "", true, false}, {"model", "0", false, true}, {"model", "model", true, false}, {"model", "junk", true, false},
		{"off", "", false, true}, {"off", "0", false, true}, {"off", "model", true, false}, {"off", "junk", false, true},
	}
	for _, c := range cases {
		t.Run(c.state+"/"+c.env, func(t *testing.T) {
			e := newNSEnv(t, "ref: refs/heads/main\n")
			if code, _, errs := runChottag(t, "names", c.state); code != exit.OK {
				t.Fatalf("names: %d %q", code, errs)
			}
			t.Setenv("CHOTTAG_NAME_SESSIONS", c.env)
			calls := stubModel(t, `{"subtype":"success","is_error":false,"result":"Fix date test"}`, nil)
			out := e.hookPrompt(t, "Add a retry to the upload client")
			if c.wantSilent != (out == "") {
				t.Fatalf("out %q, silent want %v", out, c.wantSilent)
			}
			if got := len(*calls) == 1; got != c.wantModel {
				t.Fatalf("model calls %d, want model %v", len(*calls), c.wantModel)
			}
		})
	}
}

func TestNameSessionUnreadableStateMeansOn(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/feat/x\n")
	if err := os.WriteFile(filepath.Join(e.home, "state.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := e.hookPrompt(t, "Add a retry"); !strings.Contains(out, `"sessionTitle":"x"`) {
		t.Fatalf("out %q, want a branch name", out)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "names model sets the mode", command: "names",
			setup: func(t *testing.T) []string { notifyHome(t); return []string{"names", "model"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["names"] != "model" {
					t.Errorf("doc = %v, want names model", doc)
				}
			},
		},
		jsonCase{
			name: "names rejects a bad value", command: "names",
			setup:    func(t *testing.T) []string { notifyHome(t); return []string{"names", "maybe"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}
