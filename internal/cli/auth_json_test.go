package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

// authJSONLoginReady stubs `claude auth login` (it writes a credential
// file) and returns a fake claude whose `auth status --json` reports a@.
func authJSONLoginReady(t *testing.T) string {
	t.Helper()
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	stubAuthExec(t, func(_, slotDir, _ string) error {
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	})
	return writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com","orgName":"Org"}`)
}

// authJSONLogoutReady seeds A (serving, remote) and B, and stubs the revoke
// and the credential delete.
func authJSONLogoutReady(t *testing.T) (home string, calls, deleted *[]string) {
	t.Helper()
	home, _ = seedLoggedInAccounts(t)
	calls = stubAuthExec(t, func(string, string, string) error { return nil })
	deleted = stubCredsDelete(t, nil)
	return home, calls, deleted
}

// authJSONShell points HOME and SHELL at throwaway values so setup and
// uninstall edit a temp rc, never the real one.
func authJSONShell(t *testing.T) (home, userHome string) {
	t.Helper()
	home, userHome = t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	return home, userHome
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "login registers a new account", command: "login",
			setup: func(t *testing.T) []string { return []string{"login", "--claude", authJSONLoginReady(t), "A"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["account"] != "A" || doc["email"] != "a@example.com" || doc["org"] != "Org" {
					t.Errorf("doc = %v, want account A, a@example.com, Org", doc)
				}
			},
		},
		jsonCase{
			name: "login whose browser flow fails", command: "login",
			setup: func(t *testing.T) []string {
				t.Setenv("CHOTTAG_HOME", t.TempDir())
				stubAuthExec(t, func(string, string, string) error { return errors.New("exit status 1") })
				return []string{"login", "--claude", writeFakeClaude(t, `{"loggedIn":false}`), "A"}
			},
			wantExit: exit.Error, wantCode: codeLoginFailed,
		},
		jsonCase{
			name: "logout --yes removes the account", command: "logout",
			setup: func(t *testing.T) []string { authJSONLogoutReady(t); return []string{"logout", "--yes", "B"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["account"] != "B" || doc["removed"] != true || !strings.HasSuffix(doc["dir"].(string), filepath.Join("accounts", "B")) {
					t.Errorf("doc = %v, want account B removed from accounts/B", doc)
				}
			},
		},
		jsonCase{
			name: "logout of the serving account without --force", command: "logout",
			setup:    func(t *testing.T) []string { authJSONLogoutReady(t); return []string{"logout", "--yes", "A"} },
			wantExit: exit.UserAction, wantCode: codeRoleHeld,
		},
		jsonCase{
			name: "logout without --yes and without a terminal", command: "logout",
			setup:    func(t *testing.T) []string { authJSONLogoutReady(t); return []string{"logout", "B"} },
			wantExit: exit.UserAction, wantCode: codeConfirmationRequired,
			check: func(t *testing.T, doc map[string]any) {
				if docError(t, doc)["confirmWith"] != "--yes" {
					t.Errorf("error = %v, want confirmWith --yes", docError(t, doc))
				}
			},
		},
		jsonCase{
			name: "uninstall on a fresh home", command: "uninstall",
			setup: func(t *testing.T) []string { authJSONShell(t); return []string{"uninstall"} },
			check: func(t *testing.T, doc map[string]any) {
				if rm, ok := doc["removed"].([]any); !ok || len(rm) != 0 || doc["purged"] != false {
					t.Errorf("doc = %v, want removed [] and purged false", doc)
				}
			},
		},
		jsonCase{
			name: "uninstall --purge without a terminal", command: "uninstall",
			setup:    func(t *testing.T) []string { authJSONShell(t); return []string{"uninstall", "--purge"} },
			wantExit: exit.UserAction, wantCode: codeConfirmationRequired,
		},
	)
}

// Review Focus 1: login hands `claude auth login` chottag's stdout in text
// mode (login.go's runClaudeAuth). Under --json that output must go to
// stderr, or it lands in front of the document.
func TestLoginJSONGivesTheChildStderrAsItsStdout(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com"}`)
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "Opening your browser to sign in…")
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	}))
	code, out, errb := runChottag(t, "login", "--claude", fake, "A", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	decodeOneDocument(t, out)
	if !strings.Contains(errb, "Opening your browser") {
		t.Errorf("stderr = %q, want the child's output there", errb)
	}
}

// In text mode the child keeps chottag's own stdout, as before M1d-d.
func TestLoginTextGivesTheChildStdout(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com"}`)
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "Opening your browser to sign in…")
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	}))
	code, out, errb := runChottag(t, "login", "--claude", fake, "A")
	if code != 0 || out != "Opening your browser to sign in…\nlogged in: A (a@example.com)\n" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, errb)
	}
}

// Spec delta 6: logout's `claude auth logout` child gets the same
// treatment as login's.
func TestLogoutJSONGivesTheRevokeChildStderrAsItsStdout(t *testing.T) {
	seedLoggedInAccounts(t)
	stubCredsDelete(t, nil)
	t.Cleanup(SetAuthExecForTest(func(_, _, _ string, _ io.Reader, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "Successfully logged out")
		return nil
	}))
	code, out, errb := runChottag(t, "--json", "logout", "--yes", "B")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	decodeOneDocument(t, out)
	if !strings.Contains(errb, "Successfully logged out") {
		t.Errorf("stderr = %q, want the child's output there", errb)
	}
}

// Review Focus 4: `echo y | chottag logout B` used to confirm through the
// pipe. Without a terminal it now refuses with exit 3 naming --yes, writes
// no prompt anywhere, and touches nothing.
func TestLogoutFromAPipeRefusesWithConfirmationRequired(t *testing.T) {
	home, calls, deleted := authJSONLogoutReady(t)
	var out, errb bytes.Buffer
	if code := runLogout([]string{"B"}, strings.NewReader("y\n"), newReporter(false, &out, &errb)); code != 3 {
		t.Fatalf("exit = %d, want 3; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
	if !strings.Contains(errb.String(), "--yes") || strings.Contains(errb.String(), "[y/N]") {
		t.Errorf("stderr = %q, want a refusal naming --yes and no prompt", errb.String())
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("revoked %v, deleted %v; want nothing touched", *calls, *deleted)
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "B")); err != nil {
		t.Errorf("slot B is gone: %v", err)
	}
}

// On a terminal, the prompt now goes to stderr, and stdout carries only
// the result line.
func TestLogoutPromptGoesToStderr(t *testing.T) {
	authJSONLogoutReady(t)
	interactiveStdin(t)
	var out, errb bytes.Buffer
	if code := runLogout([]string{"B"}, strings.NewReader("y\n"), newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb.String())
	}
	if out.String() != "logged out: B\n" {
		t.Errorf("stdout = %q, want only the result line", out.String())
	}
	if !strings.Contains(errb.String(), "[y/N]") {
		t.Errorf("stderr = %q, want the prompt", errb.String())
	}
}

// --json never prompts, even on a terminal.
func TestLogoutJSONNeverPromptsEvenOnATerminal(t *testing.T) {
	_, calls, _ := authJSONLogoutReady(t)
	interactiveStdin(t)
	code, out, _ := runChottag(t, "logout", "B", "--json")
	if code != exit.UserAction {
		t.Fatalf("exit = %d, want %d", code, exit.UserAction)
	}
	if e := docError(t, decodeOneDocument(t, out)); e["code"] != "confirmation_required" {
		t.Errorf("error = %v", e)
	}
	if len(*calls) != 0 {
		t.Errorf("revoked %v, want nothing", *calls)
	}
}

// --json is global in any position, and logout's own flags stay order-free.
func TestLogoutJSONFlagOrder(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "logout", "--yes", "B"},
		{"logout", "--json", "--yes", "B"},
		{"logout", "B", "--yes", "--json"},
		{"logout", "--yes", "--json", "B"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			authJSONLogoutReady(t)
			code, out, errb := runChottag(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr=%q", code, errb)
			}
			if doc := decodeOneDocument(t, out); doc["account"] != "B" {
				t.Errorf("doc = %v", doc)
			}
		})
	}
}

// uninstall --purge always refuses without a terminal, and always under
// --json, before anything is removed; --yes does not cover it.
func TestPurgeRefusesWithoutATerminalOrUnderJSON(t *testing.T) {
	for _, c := range []struct {
		name        string
		interactive bool
		args        []string
	}{
		{"pipe", false, []string{"uninstall", "--purge"}},
		{"pipe_with_yes_flag_is_still_a_usage_error", false, []string{"uninstall", "--purge", "--yes"}},
		{"json_on_a_terminal", true, []string{"uninstall", "--purge", "--json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, _ := authJSONShell(t)
			if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
				t.Fatalf("setup = %d", code)
			}
			if c.interactive {
				interactiveStdin(t)
			}
			code, _, errb := runChottag(t, c.args...)
			want := exit.UserAction
			if c.name == "pipe_with_yes_flag_is_still_a_usage_error" {
				want = exit.Usage // uninstall has no --yes flag
			}
			if code != want {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, want, errb)
			}
			if _, err := os.Lstat(filepath.Join(home, "bin", "claude")); err != nil {
				t.Errorf("bin/claude is gone (%v): the refusal must come before any removal", err)
			}
		})
	}
}

// removed lists what uninstall actually removed: the rc file (its block
// was there) and both symlinks.
func TestUninstallJSONListsWhatItRemoved(t *testing.T) {
	home, userHome := authJSONShell(t)
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	code, out, errb := runChottag(t, "uninstall", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	rm, _ := decodeOneDocument(t, out)["removed"].([]any)
	got := map[string]bool{}
	for _, p := range rm {
		got[p.(string)] = true
	}
	for _, p := range []string{filepath.Join(userHome, ".zshrc"), filepath.Join(home, "bin", "chottag"), filepath.Join(home, "bin", "claude")} {
		if !got[p] {
			t.Errorf("removed = %v, want it to include %s", rm, p)
		}
	}
}

// A start fence with no matching end fence is exactly what stripRCBlock
// (and so removeRCBlock) leaves untouched (rcfile.go). removed must not
// claim that rc file was removed when it was not — spec §5.3 detail 9:
// "removed" lists paths ACTUALLY removed, not merely files that mention the
// start fence.
func TestUninstallJSONOmitsAnRCFileWithOnlyAStartFence(t *testing.T) {
	_, userHome := authJSONShell(t)
	rc := filepath.Join(userHome, ".zshrc")
	original := rcStart + "\nexport PATH=\"/somewhere:$PATH\"\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errb := runChottag(t, "uninstall", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	rm, _ := decodeOneDocument(t, out)["removed"].([]any)
	for _, p := range rm {
		if p == rc {
			t.Errorf("removed = %v, want %s excluded: no matching end fence, so nothing was actually removed", rm, rc)
		}
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("rc = %q, want it unchanged: %q", got, original)
	}
}
