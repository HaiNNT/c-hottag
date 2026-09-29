package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/doctor"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// writeVersionScript writes a stand-in `claude` into a temp dir: a shell
// script, never the real claude.
func writeVersionScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// versionTestHangGuard is what the success-path tests below pass as
// runClaudeVersion's timeout: a script that is supposed to finish
// instantly needs no wall-clock bound at all, only a guard against
// actually hanging forever if the code is broken. The production 5s
// (claudeVersionTimeout) is not that: under full-suite `-race` load it has
// already been observed too tight for an otherwise-instant script (the
// F136/F155 flake class — a correctness assertion must never share a
// bound with a real-world timeout it isn't testing).
const versionTestHangGuard = 60 * time.Second

func TestRunClaudeVersionReadsStdoutOnly(t *testing.T) {
	bin := writeVersionScript(t, `echo "2.1.282 (Claude Code)"; echo noise >&2`)
	out, err := runClaudeVersion(bin, versionTestHangGuard)
	if err != nil || out != "2.1.282 (Claude Code)\n" {
		t.Fatalf("runClaudeVersion = %q, %v", out, err)
	}
}

func TestRunClaudeVersionPassesOnlyTheVersionFlag(t *testing.T) {
	bin := writeVersionScript(t, `printf '%s|' "$@"`)
	if out, err := runClaudeVersion(bin, versionTestHangGuard); err != nil || out != "--version|" {
		t.Fatalf("argv = %q, %v, want only --version", out, err)
	}
}

// TestRunClaudeVersionTimesOut is Review Focus 5. The script sleeps 10s
// against a 200ms timeout. The kill plus the 1s WaitDelay bound the call
// well under 9s (F73: 200ms + the kill + 1s WaitDelay is nowhere near 9s),
// while 9s stays below the script's own 10s sleep — so anything under 9s
// still proves runClaudeVersion did not wait for the script, without
// sharing the tight 5s bound the F136/F155 flake class warns against.
func TestRunClaudeVersionTimesOut(t *testing.T) {
	bin := writeVersionScript(t, "exec sleep 10")
	start := time.Now()
	_, err := runClaudeVersion(bin, 200*time.Millisecond)
	if el := time.Since(start); el > 9*time.Second {
		t.Fatalf("runClaudeVersion took %v with a 200ms timeout", el)
	}
	if err == nil || !strings.Contains(err.Error(), "did not finish within 200ms") {
		t.Fatalf("err = %v, want the timeout named", err)
	}
}

// TestRunClaudeVersionBoundsItsOutput: 100,000 bytes on stdout against the
// 4096-byte cap; the script must still finish (the rest is drained).
func TestRunClaudeVersionBoundsItsOutput(t *testing.T) {
	bin := writeVersionScript(t, `head -c 100000 /dev/zero | tr '\0' 'x'`)
	out, err := runClaudeVersion(bin, versionTestHangGuard)
	if err != nil || len(out) != claudeVersionMaxBytes {
		t.Fatalf("len(out) = %d, err %v; want exactly %d", len(out), err, claudeVersionMaxBytes)
	}
}

func TestRunClaudeVersionReportsAFailingBinary(t *testing.T) {
	if _, err := runClaudeVersion(writeVersionScript(t, "exit 3"), versionTestHangGuard); err == nil {
		t.Fatal("a failing claude --version returned no error")
	}
}

func TestClaudeVersionBoundsAreTheSpecs(t *testing.T) {
	if claudeVersionTimeout != 5*time.Second || claudeVersionMaxBytes != 4096 {
		t.Fatalf("timeout %v, cap %d; want 5s and 4096 (M2c spec §5)", claudeVersionTimeout, claudeVersionMaxBytes)
	}
}

// TestDoctorClaudeVersionSeamPanicsUnlessStubbed is PF8: the ClaudeVersion
// seam's default (before any test stubs it) panics and names the seam, the
// same guarantee TestMain arms for every doctor test in this package.
func TestDoctorClaudeVersionSeamPanicsUnlessStubbed(t *testing.T) {
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("doctorClaudeVersion did not panic")
		}
		if msg, ok := p.(string); !ok || !strings.Contains(msg, "SetDoctorClaudeVersionForTest") {
			t.Fatalf("panic = %v, want it to name SetDoctorClaudeVersionForTest", p)
		}
	}()
	doctorClaudeVersion("x")
}

// TestDoctorClaudeVersionDefaultIsPinnedAndWired is fix round 1 item 5,
// tightened in fix round 2 item 1: a missing binary (round 1's version)
// fails the same way regardless of the timeout passed to runClaudeVersion,
// so it could not tell defaultDoctorClaudeVersion apart from a mutant that
// ignores doctorClaudeVersionTimeout entirely. Pinning the var to 200ms and
// making the script actually run past it (sleep 10) proves
// defaultDoctorClaudeVersion reads doctorClaudeVersionTimeout, and that
// newDoctorEnv's ClaudeVersion field reaches it — without ever running the
// real claude.
func TestDoctorClaudeVersionDefaultIsPinnedAndWired(t *testing.T) {
	if doctorClaudeVersionTimeout != claudeVersionTimeout {
		t.Fatalf("doctorClaudeVersionTimeout = %v, want claudeVersionTimeout %v", doctorClaudeVersionTimeout, claudeVersionTimeout)
	}
	orig := doctorClaudeVersionTimeout
	doctorClaudeVersionTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doctorClaudeVersionTimeout = orig })
	t.Cleanup(SetDoctorClaudeVersionForTest(defaultDoctorClaudeVersion))
	env, err := newDoctorEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := writeVersionScript(t, "exec sleep 10")
	if _, err := env.ClaudeVersion(bin); err == nil || !strings.Contains(err.Error(), "did not finish within 200ms") {
		t.Fatalf("err = %v, want the 200ms timeout named", err)
	}
}

func TestDoctorRunsVersionOnTheResolvedRealClaude(t *testing.T) {
	chottagHome, _, _ := doctorInstall(t)
	var got []string
	t.Cleanup(SetDoctorClaudeVersionForTest(func(bin string) (string, error) {
		got = append(got, bin)
		return "2.1.282 (Claude Code)\n", nil
	}))
	st, err := store.Store{Dir: chottagHome}.Load()
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs := runChottag(t, "doctor")
	if code != exit.OK {
		t.Fatalf("doctor = %d; %s %s", code, out, errs)
	}
	if len(got) != 1 || got[0] != st.RealClaude || got[0] == filepath.Join(chottagHome, "bin", "claude") {
		t.Fatalf("claude --version ran on %q, want only the real claude %q, never the shim", got, st.RealClaude)
	}
	if doctorRows(t, out)["version-drift"] != doctor.StatusInfo {
		t.Fatalf("version-drift not info with no trace:\n%s", out)
	}
}

func TestDoctorJSONVersionDriftRowCarriesItsFields(t *testing.T) {
	chottagHome, _, _ := doctorInstall(t)
	var f status.File
	f.SetLastTraced("2.1.282", time.Date(2026, 9, 25, 10, 40, 0, 0, time.UTC))
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(chottagHome), b); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runChottag(t, "--json", "doctor")
	if code != exit.OK {
		t.Fatalf("doctor = %d", code)
	}
	checks, _ := decodeOneDocument(t, out)["checks"].([]any)
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if m["id"] != "version-drift" {
			continue
		}
		lt, _ := m["lastTraced"].(map[string]any)
		if m["status"] != "ok" || m["installed"] != "2.1.282" || lt["claudeVersion"] != "2.1.282" {
			t.Fatalf("version-drift = %v", m)
		}
		return
	}
	t.Fatal("no version-drift row")
}
