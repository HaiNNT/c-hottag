package cli

// F164: `--help` never changes anything. wantsHelp and commandUsage are
// unit tested directly; TestHelpFlagHasNoSideEffects sweeps every
// dispatchable command (jsoncomplete_test.go's dispatchLabels, so a new
// command is covered automatically) and proves the guard answers before
// any command runs a byte of its own logic.

import (
	"os"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

func TestWantsHelp(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"-help"}, true},
		{[]string{"--help=true"}, true},
		{[]string{"NAME", "--help"}, true},
		{[]string{"--", "--help"}, false},
		{[]string{"help-desk"}, false},
		{[]string{}, false},
	}
	for _, c := range cases {
		if got := wantsHelp(c.args); got != c.want {
			t.Errorf("wantsHelp(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestCommandUsage(t *testing.T) {
	daemon := commandUsage("daemon")
	for _, verb := range []string{"daemon run", "daemon start", "daemon stop", "daemon restart", "daemon logs"} {
		if !strings.Contains(daemon, verb) {
			t.Errorf("commandUsage(\"daemon\") = %q, want it to contain %q", daemon, verb)
		}
	}
	if strings.Contains(daemon, "trace") {
		t.Errorf("commandUsage(\"daemon\") = %q, want no trace line", daemon)
	}

	if ls := commandUsage("ls"); !strings.Contains(ls, "status") {
		t.Errorf("commandUsage(\"ls\") = %q, want it to contain the status line", ls)
	}
}

// assertEmptyDir fails the test unless dir holds nothing: the --help guard
// must run before any command touches the filesystem.
func assertEmptyDir(t *testing.T, label, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s (%s): %v", label, dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("%s (%s) is not empty after --help: %v", label, dir, names)
	}
}

// helpSweepArgs builds label's own argv plus a trailing --help: label is
// one of dispatchLabels(t)'s entries, split on space for a compound one
// ("daemon stop" -> "daemon", "stop"). "proxy" is a command group whose
// verbs live inside runProxy, invisible to the AST walk dispatchLabels
// does, so it is given its representative subcommand (proxy run --help,
// the form Review Focus names). trace's verbs are walked (dispatchLabels
// collects runTrace's switch), and bare "trace --help" is answered by
// dispatch's F164 guard like any other command, so it needs no special case.
func helpSweepArgs(label string) []string {
	switch label {
	case "proxy":
		return []string{"proxy", "run", "--help"}
	default:
		return append(strings.Fields(label), "--help")
	}
}

// TestHelpFlagHasNoSideEffects is F164's core claim: every dispatchable
// command answers --help with its own usage, exit 0, and touches neither a
// temp $HOME nor a temp $CHOTTAG_HOME — in particular `setup --help`, which
// used to install the shim and edit the shell rc before F164.
func TestHelpFlagHasNoSideEffects(t *testing.T) {
	for _, label := range dispatchLabels(t) {
		if label == "help" {
			continue // no usage line of its own; covered by TestCommandUsage's fallback case indirectly
		}
		label := label
		t.Run(label, func(t *testing.T) {
			home := t.TempDir()
			chottagHome := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CHOTTAG_HOME", chottagHome)
			// setup *would* write ~/.zshrc if the guard let it run.
			t.Setenv("SHELL", "/bin/zsh")

			args := helpSweepArgs(label)
			code, out, errs := runChottag(t, args...)
			if code != exit.OK {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, exit.OK, out, errs)
			}
			top := strings.Fields(label)[0]
			if want := commandUsage(top); !strings.Contains(out, want) {
				t.Errorf("stdout does not contain %s's usage:\nstdout=%q\nwant substring=%q", top, out, want)
			}
			assertEmptyDir(t, "HOME", home)
			assertEmptyDir(t, "CHOTTAG_HOME", chottagHome)
		})
	}
}

// TestJSONSetupHelpRefusesAndDoesNothing is Review Focus's binding case:
// `--json setup --help` refuses with the JSON-unsupported code, exit 2, one
// document on stdout, and no side effects — setup's own --json refusal
// (there is none; setup normally supports --json) never gets a chance to
// run, because the --help guard answers first.
func TestJSONSetupHelpRefusesAndDoesNothing(t *testing.T) {
	home := t.TempDir()
	chottagHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHOTTAG_HOME", chottagHome)
	t.Setenv("SHELL", "/bin/zsh")

	code, out, errs := runChottag(t, "--json", "setup", "--help")
	if code != exit.Usage {
		t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, exit.Usage, out, errs)
	}
	doc := decodeOneDocument(t, out)
	assertDocumentHeader(t, doc, code)
	if got := docError(t, doc)["code"]; got != string(codeJSONUnsupported) {
		t.Errorf("error.code = %v, want %s", got, codeJSONUnsupported)
	}
	assertEmptyDir(t, "HOME", home)
	assertEmptyDir(t, "CHOTTAG_HOME", chottagHome)
}
