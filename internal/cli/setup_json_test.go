package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

// setupJSONEnv points CHOTTAG_HOME, HOME and SHELL at throwaway values so
// setup writes a temp rc, never the real one.
func setupJSONEnv(t *testing.T, shell string) (home, userHome string) {
	t.Helper()
	home, userHome = t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", shell)
	return home, userHome
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "setup installs the shim", command: "setup",
			setup: func(t *testing.T) []string { setupJSONEnv(t, "/bin/zsh"); return []string{"setup"} },
			check: func(t *testing.T, doc map[string]any) {
				if !strings.HasSuffix(doc["installed"].(string), "bin") || doc["rcUpdated"] != true || !strings.HasSuffix(doc["rcPath"].(string), ".zshrc") {
					t.Errorf("doc = %v, want installed <home>/bin, rcUpdated true, rcPath ~/.zshrc", doc)
				}
			},
		},
		jsonCase{
			name: "setup with a bad adopt flag", command: "setup",
			setup:    func(t *testing.T) []string { setupJSONEnv(t, "/bin/zsh"); return []string{"setup", "--nope"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}

// setup runs adopt inside itself. Both used to write their own output;
// under --json only setup's one document may reach stdout, whatever adopt
// found.
func TestSetupJSONWritesOneDocumentAlthoughItRunsAdopt(t *testing.T) {
	home, _ := setupJSONEnv(t, "/bin/zsh")
	if err := os.MkdirAll(filepath.Join(home, "accounts", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com"}`)
	code, out, errb := runChottag(t, "setup", "--claude", fake, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	if _, ok := doc["adopted"]; ok {
		t.Errorf("doc = %v: adopt's own fields leaked into setup's document", doc)
	}
	if doc["rcUpdated"] != true {
		t.Errorf("doc = %v", doc)
	}
}

// A usage error in adopt's flags is setup's usage error, reported once:
// one document, and the flag package's text once on stderr.
func TestSetupRelaysAdoptsUsageErrorOnce(t *testing.T) {
	setupJSONEnv(t, "/bin/zsh")
	code, out, errb := runChottag(t, "--json", "setup", "--nope")
	if code != exit.Usage {
		t.Fatalf("exit = %d, want %d", code, exit.Usage)
	}
	if e := docError(t, decodeOneDocument(t, out)); e["code"] != "usage" {
		t.Errorf("error = %v", e)
	}
	if n := strings.Count(errb, "flag provided but not defined: -nope"); n != 1 {
		t.Errorf("stderr = %q: the flag error must appear exactly once", errb)
	}
}

// An unrecognised $SHELL gets the PATH block printed instead of written:
// on stdout in text mode, exactly as before; as an rc_not_written warning
// carrying the block under --json.
func TestSetupUnrecognisedShellPrintsTheBlock(t *testing.T) {
	home, _ := setupJSONEnv(t, "/usr/bin/fish")
	code, out, errb := runChottag(t, "setup")
	want := "unrecognised $SHELL; add this to your shell's rc yourself:\n" + rcBlock(filepath.Join(home, "bin"), home)
	if code != 0 || !strings.Contains(out, want) {
		t.Fatalf("exit=%d stdout=%q stderr=%q; want the block on stdout", code, out, errb)
	}

	setupJSONEnv(t, "/usr/bin/fish")
	code, out, _ = runChottag(t, "setup", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	doc := decodeOneDocument(t, out)
	ws, _ := doc["warnings"].([]any)
	// A fresh home has no slot to adopt: that is not a warning (fix round
	// 1) — a script would otherwise see every first install as broken.
	// The empty adopt object is what shows nothing was there.
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != "rc_not_written" || !strings.Contains(ws[0].(map[string]any)["message"].(string), "export PATH=") {
		t.Errorf("warnings = %v, want only rc_not_written carrying the export line", ws)
	}
	if doc["rcUpdated"] != false {
		t.Errorf("rcUpdated = %v, want false", doc["rcUpdated"])
	}
	adopt, _ := doc["adopt"].(map[string]any)
	adopted, _ := adopt["adopted"].([]any)
	updated, _ := adopt["updated"].([]any)
	skipped, _ := adopt["skipped"].([]any)
	if adopt == nil || len(adopted) != 0 || len(updated) != 0 || len(skipped) != 0 {
		t.Errorf("adopt = %v, want an empty adopt object (arrays present, never null)", doc["adopt"])
	}
}

// When the accounts dir exists (setup's own MkdirAll already ran) but can't
// be read — a real failure, EACCES for one, not "nothing to adopt yet" —
// setup must surface it as adopt_failed, not silence it the way it silences
// a fresh install's no_accounts (fix round 2).
func TestSetupWarnsWhenAccountsDirCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod does not block root from reading the dir")
	}
	home, _ := setupJSONEnv(t, "/bin/zsh")
	accountsDir := filepath.Join(home, "accounts")
	// Pre-create the dir so setup's MkdirAll (setup.go) finds it already
	// there and leaves its mode alone (MkdirAll no-ops on an existing dir),
	// then strip read/search permission so adopt's ReadDir (accounts.go)
	// hits a real error, not an empty listing.
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(accountsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(accountsDir, 0o700) })

	code, out, errb := runChottag(t, "setup", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	ws, _ := doc["warnings"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != "adopt_failed" {
		t.Fatalf("warnings = %v, want exactly one adopt_failed warning", ws)
	}
	if _, ok := doc["adopt"]; ok {
		t.Errorf("doc = %v: no_slots must not get the silent empty adopt object", doc)
	}
}

// --json anywhere, and adopt's flags after it, parse the same.
func TestSetupJSONFlagOrder(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "setup", "--claude", "/nonexistent/claude"},
		{"setup", "--claude", "/nonexistent/claude", "--json"},
		{"setup", "--json", "--claude", "/nonexistent/claude"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setupJSONEnv(t, "/bin/zsh")
			code, out, errb := runChottag(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr=%q", code, errb)
			}
			decodeOneDocument(t, out)
		})
	}
}
