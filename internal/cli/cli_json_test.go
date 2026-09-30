package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// cliJSONHome points CHOTTAG_HOME at a fresh directory.
func cliJSONHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	return home
}

// cliJSONCorruptState writes a state.json that store.Load rejects.
func cliJSONCorruptState(t *testing.T) string {
	t.Helper()
	home := cliJSONHome(t)
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func cliJSONRefusal(command string, args ...string) jsonCase {
	return jsonCase{
		name:     command + " refuses --json",
		command:  command,
		setup:    func(t *testing.T) []string { cliJSONHome(t); return slices.Clone(args) },
		wantExit: exit.Usage,
		wantCode: codeJSONUnsupported,
		jsonOnly: true,
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "version reports the build", command: "version",
			setup: func(t *testing.T) []string { return []string{"version"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["chottag"] != Version {
					t.Errorf("chottag = %v, want %q", doc["chottag"], Version)
				}
			},
		},
		jsonCase{
			name: "status on an empty home", command: "status",
			setup: func(t *testing.T) []string { cliJSONHome(t); return []string{"status"} },
			check: func(t *testing.T, doc map[string]any) {
				if a, ok := doc["accounts"].([]any); !ok || len(a) != 0 {
					t.Errorf("accounts = %v, want []", doc["accounts"])
				}
			},
		},
		jsonCase{
			name: "status with a corrupt state.json", command: "status",
			setup:    func(t *testing.T) []string { cliJSONCorruptState(t); return []string{"status"} },
			wantExit: exit.Error, wantCode: codeInternal,
		},
		jsonCase{
			name: "statusline on an empty home", command: "statusline",
			setup: func(t *testing.T) []string { cliJSONHome(t); return []string{"statusline"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["session"] != "home" || doc["daemon"] != "unknown" || doc["serving"] != "" {
					t.Errorf("doc = %v, want home / unknown / empty serving", doc)
				}
			},
		},
		jsonCase{
			name: "statusline with a stray argument", command: "statusline",
			setup:    func(t *testing.T) []string { cliJSONHome(t); return []string{"statusline", "extra"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			name: "ls on an empty home", command: "ls",
			setup: func(t *testing.T) []string { cliJSONHome(t); return []string{"ls"} },
		},
		cliJSONRefusal("help", "help"),
		cliJSONRefusal("-h", "-h"),
		cliJSONRefusal("--help", "--help"),
		cliJSONRefusal("proxy", "proxy", "run"),
		cliJSONRefusal("daemon run", "daemon", "run"),
		cliJSONRefusal("daemon logs", "daemon", "logs"),
		jsonCase{
			name: "no command is a usage error", command: "",
			setup:    func(t *testing.T) []string { return []string{} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			name: "an unknown command is a usage error", command: "",
			setup:    func(t *testing.T) []string { return []string{"nosuch"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			name: "daemon with no verb is a usage error", command: "",
			setup:    func(t *testing.T) []string { cliJSONHome(t); return []string{"daemon"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			name: "daemon with an unknown verb is a usage error", command: "",
			setup:    func(t *testing.T) []string { cliJSONHome(t); return []string{"daemon", "nosuch"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}

// Spec §5.3: `chottag --json X`, `chottag X --json` and `--json=true` are
// the same invocation; --json=false is text.
func TestJSONFlagIsAcceptedAnywhere(t *testing.T) {
	_, want, _ := runChottag(t, "--json", "version")
	for _, args := range [][]string{{"version", "--json"}, {"--json=true", "version"}, {"-json", "version"}} {
		if _, out, _ := runChottag(t, args...); out != want {
			t.Errorf("%q wrote %q, want the same document as --json version (%q)", args, out, want)
		}
	}
	if _, out, _ := runChottag(t, "--json=false", "version"); out != "chottag "+Version+"\n" {
		t.Errorf("--json=false version wrote %q, want the text line", out)
	}
}

// Review Focus 2: --json after -- is the command's text, not the global
// flag. trace refuses --json, so a pre-pass that read past -- would exit 2
// here instead of recording the marker.
func TestJSONAfterDoubleDashIsNotGlobal(t *testing.T) {
	home := cliJSONHome(t)
	code, out, errs := runChottag(t, "trace", "mark", "--", "--json")
	if code != 0 || out != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q; want 0 and nothing on stdout", code, out, errs)
	}
	recs, err := tracelog.ReadAll(filepath.Join(home, "trace.jsonl"))
	if err != nil || len(recs) != 1 || recs[0].Mark != "--json" {
		t.Fatalf("trace records = %+v, %v; want one marker \"--json\"", recs, err)
	}
}

// Review Focus 5: a status line that runs `status --json` must get a
// document on a failure too, not an empty stdout.
func TestStatusJSONFailureIsAnErrorDocumentOnStdout(t *testing.T) {
	cliJSONCorruptState(t)
	code, out, errs := runChottag(t, "status", "--json")
	if code != exit.Error {
		t.Fatalf("exit = %d, want %d", code, exit.Error)
	}
	doc := decodeOneDocument(t, out)
	assertDocumentHeader(t, doc, code)
	if e := docError(t, doc); e["code"] != "internal" || !strings.Contains(e["message"].(string), "corrupt") {
		t.Errorf("error = %v, want internal naming the corrupt state.json", e)
	}
	if !strings.HasPrefix(errs, "chottag: ") || !strings.Contains(errs, "corrupt") {
		t.Errorf("stderr = %q, want the same chottag: text as without --json", errs)
	}
}

// A command that returns without reporting under --json gets the internal
// fallback and exit 1 (spec §5.3); in text mode its own code passes through.
func TestRunFallsBackToAnInternalDocumentWhenACommandReportsNothing(t *testing.T) {
	silent := func([]string, *reporter) int { return 0 }
	var out, errb bytes.Buffer
	if code := runCommand(globalFlags{json: true}, []string{"x"}, &out, &errb, silent); code != exit.Error {
		t.Fatalf("exit = %d, want %d", code, exit.Error)
	}
	if e := docError(t, decodeOneDocument(t, out.String())); e["code"] != "internal" {
		t.Errorf("error = %v, want internal", e)
	}
	if code := runCommand(globalFlags{}, []string{"x"}, io.Discard, io.Discard, func([]string, *reporter) int { return 3 }); code != 3 {
		t.Errorf("text-mode exit = %d, want the command's own 3", code)
	}
}

func TestRunRejectsABadJSONValueInText(t *testing.T) {
	code, out, errs := runChottag(t, "--json=maybe", "version")
	if code != exit.Usage || out != "" || !strings.Contains(errs, "--json") {
		t.Fatalf("exit=%d stdout=%q stderr=%q; want 2, nothing on stdout, and a message naming --json", code, out, errs)
	}
}

// A refused command does no work: `trace mark` would create trace.jsonl.
func TestARefusedCommandDoesNoWork(t *testing.T) {
	home := cliJSONHome(t)
	if code, _, _ := runChottag(t, "--json", "trace", "mark", "hello"); code != exit.Usage {
		t.Fatalf("exit = %d, want %d", code, exit.Usage)
	}
	if _, err := os.Stat(filepath.Join(home, "trace.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("trace.jsonl exists (err=%v): the refusal must come before any work", err)
	}
}

// The pre-pass runs for chottag only. Invoked as `claude`, every argument,
// --json included, reaches Claude Code untouched. CHOTTAG_BYPASS is the
// shim's escape hatch: it execs the real claude before anything else, so
// the only seam this reaches is execFn, stubbed here.
func TestShimForwardsJSONUntouched(t *testing.T) {
	cliJSONHome(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("CHOTTAG_BYPASS", "1")
	var got []string
	t.Cleanup(shim.SetSeamsForTest(
		func(_ string, args, _ []string) error { got = slices.Clone(args); return nil },
		func(string, string, string) error { panic("spawn reached from TestShimForwardsJSONUntouched") },
	))
	var out, errb bytes.Buffer
	if code := Run(filepath.Join(bin, "..", "claude"), []string{"--json", "-p", "hi", "--json=false", "--json=maybe"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errb.String())
	}
	if want := []string{"--json", "-p", "hi", "--json=false", "--json=maybe"}; !slices.Equal(got, want) {
		t.Fatalf("claude got %q, want %q untouched", got, want)
	}
}
