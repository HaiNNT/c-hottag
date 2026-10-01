package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// policyHome seeds accounts A, B and work under a fresh CHOTTAG_HOME.
func policyHome(t *testing.T) (string, store.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B", "work"} {
			if err := st.Add(store.Account{Name: n, Dir: filepath.Join(home, "accounts", n)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return home, s
}

func TestPolicyShowsSerialByDefault(t *testing.T) {
	policyHome(t)
	code, out, errs := runChottag(t, "policy")
	if code != exit.OK || out != "policy: serial\n" {
		t.Fatalf("policy = %d %q %q, want 0 and %q", code, out, errs, "policy: serial\n")
	}
}

func TestPolicySetsAndShowsWithPin(t *testing.T) {
	_, s := policyHome(t)
	if code, out, errs := runChottag(t, "policy", "spread"); code != exit.OK || out != "policy: spread\n" {
		t.Fatalf("policy spread = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if !st.PolicySpread() {
		t.Fatalf("policy not stored: %+v", st)
	}
	if code, _, errs := runChottag(t, "tag", "B"); code != exit.OK {
		t.Fatalf("tag B = %d %q", code, errs)
	}
	if code, out, _ := runChottag(t, "policy"); code != exit.OK || out != "policy: spread\npin: B\n" {
		t.Fatalf("policy = %d %q, want the pin line", code, out)
	}
	// Back to serial: the pin stays stored but is not shown as in effect.
	if code, out, _ := runChottag(t, "policy", "serial"); code != exit.OK || out != "policy: serial\n" {
		t.Fatalf("policy serial = %d %q", code, out)
	}
	if st, _ := s.Load(); st.Pin != "B" {
		t.Fatalf("pin = %q after serial, want B kept", st.Pin)
	}
}

func TestPolicyRejectsABadValue(t *testing.T) {
	_, s := policyHome(t)
	code, _, errs := runChottag(t, "policy", "random")
	if code != exit.Usage || !strings.Contains(errs, "serial") || !strings.Contains(errs, "spread") {
		t.Fatalf("policy random = %d %q, want exit 2 naming serial and spread", code, errs)
	}
	if st, _ := s.Load(); st.Policy != "" {
		t.Fatalf("a bad value changed the policy to %q", st.Policy)
	}
	if code, _, _ := runChottag(t, "policy", "spread", "serial"); code != exit.Usage {
		t.Fatalf("two values = %d, want 2", code)
	}
}

func TestTagUnderSerialIsUnchangedAndLeavesPin(t *testing.T) {
	_, s := policyHome(t)
	code, out, _ := runChottag(t, "tag", "B")
	if code != exit.OK || out != "serving: B\n" {
		t.Fatalf("tag B = %d %q, want exactly the serving line", code, out)
	}
	if st, _ := s.Load(); st.Pin != "" {
		t.Fatalf("tag under serial set pin %q", st.Pin)
	}
}

func TestTagUnderSpreadPins(t *testing.T) {
	_, s := policyHome(t)
	runChottag(t, "policy", "spread")
	code, out, errs := runChottag(t, "tag", "work")
	if code != exit.OK {
		t.Fatalf("tag work = %d %q", code, errs)
	}
	want := "serving: work\nnew sessions are pinned to work\n"
	if out != want {
		t.Fatalf("tag out = %q, want %q", out, want)
	}
	st, _ := s.Load()
	if st.Pin != "work" || st.Serving != "work" {
		t.Fatalf("state = serving %q pin %q, want work, work", st.Serving, st.Pin)
	}
}

func TestTagUnderSpreadOnARotationOffAccountSaysItIsNotPinned(t *testing.T) {
	_, s := policyHome(t)
	runChottag(t, "policy", "spread")
	runChottag(t, "rotate", "B", "off")
	code, out, errs := runChottag(t, "tag", "B")
	if code != exit.OK {
		t.Fatalf("tag B = %d %q", code, errs)
	}
	if !strings.Contains(errs, "out of rotation") {
		t.Fatalf("the existing out-of-rotation warning is gone: %q", errs)
	}
	if !strings.Contains(out, "new sessions won't be pinned to B while its rotation is off") {
		t.Fatalf("tag out = %q, want the not-pinned line", out)
	}
	if st, _ := s.Load(); st.Pin != "B" {
		t.Fatalf("pin = %q, want B stored", st.Pin)
	}
}

func TestTagUnpin(t *testing.T) {
	_, s := policyHome(t)
	// Under serial it is a usage error that says why.
	code, _, errs := runChottag(t, "tag", "--unpin")
	if code != exit.Usage || !strings.Contains(errs, "chottag policy spread") {
		t.Fatalf("tag --unpin under serial = %d %q, want exit 2 pointing at policy spread", code, errs)
	}
	runChottag(t, "policy", "spread")
	runChottag(t, "tag", "B")
	code, out, errs := runChottag(t, "tag", "--unpin")
	if code != exit.OK || out != "unpinned\n" {
		t.Fatalf("tag --unpin = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if st.Pin != "" || st.Serving != "B" {
		t.Fatalf("state = serving %q pin %q, want B and no pin", st.Serving, st.Pin)
	}
	// --unpin takes no NAME.
	if code, _, _ := runChottag(t, "tag", "--unpin", "A"); code != exit.Usage {
		t.Fatalf("tag --unpin A = %d, want 2", code)
	}
}

func TestNextUnderSpreadIsRefused(t *testing.T) {
	_, s := policyHome(t)
	runChottag(t, "policy", "spread")
	before, _ := s.Load()
	code, _, errs := runChottag(t, "next")
	if code != exit.Usage || !strings.Contains(errs, "under spread, chottag places sessions itself: `chottag tag NAME` pins new sessions; `chottag policy serial` returns to one serving account.") {
		t.Fatalf("next = %d %q", code, errs)
	}
	if after, _ := s.Load(); after.Serving != before.Serving {
		t.Fatalf("next moved serving %q -> %q", before.Serving, after.Serving)
	}
	// A bare `tag` is the same move.
	if code, _, _ := runChottag(t, "tag"); code != exit.Usage {
		t.Fatalf("bare tag under spread = %d, want 2", code)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "policy shows serial", command: "policy",
			setup: func(t *testing.T) []string { policyHome(t); return []string{"policy"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["policy"] != "serial" || doc["pin"] != "" {
					t.Errorf("doc = %v, want policy serial and an empty pin", doc)
				}
			},
		},
		jsonCase{
			name: "policy spread with a pin", command: "policy",
			setup: func(t *testing.T) []string {
				policyHome(t)
				runChottag(t, "policy", "spread")
				runChottag(t, "tag", "A")
				return []string{"policy"}
			},
			check: func(t *testing.T, doc map[string]any) {
				if doc["policy"] != "spread" || doc["pin"] != "A" {
					t.Errorf("doc = %v, want spread and A", doc)
				}
			},
		},
		jsonCase{
			name: "policy with a bad value", command: "policy",
			setup:    func(t *testing.T) []string { policyHome(t); return []string{"policy", "random"} },
			wantExit: exit.Usage, wantCode: codeBadPolicy,
		},
		jsonCase{
			name: "next under spread", command: "next",
			setup: func(t *testing.T) []string {
				policyHome(t)
				runChottag(t, "policy", "spread")
				return []string{"next"}
			},
			wantExit: exit.Usage, wantCode: codeSpreadNext,
		},
		jsonCase{
			name: "tag --unpin under serial", command: "tag",
			setup:    func(t *testing.T) []string { policyHome(t); return []string{"tag", "--unpin"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}

// An older daemon ignores the policy and may reset it: the CLI warns.
func TestPolicySpreadWarnsWhenTheRunningDaemonPredatesSpread(t *testing.T) {
	_, s := policyHome(t)
	origVersion := Version
	Version = "0.7.0"
	t.Cleanup(func() { Version = origVersion })
	probe := func(v string, up bool) func() {
		return SetStatusProbeForTest(func(int) (bool, string) { return up, v })
	}
	const warn = "daemon_predates_spread"
	cases := []struct {
		name string
		up   bool
		ver  string
		want bool
	}{
		{"older daemon", true, "0.6.0", true},
		{"unparseable daemon version", true, "weird", true},
		{"same version", true, "0.7.0", false},
		{"newer daemon", true, "0.8.0", false},
		{"no daemon", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := probe(c.ver, c.up)
			defer restore()
			code, out, errs := runChottag(t, "--json", "policy", "spread")
			if code != exit.OK {
				t.Fatalf("exit %d %q %q", code, out, errs)
			}
			if got := strings.Contains(out, warn); got != c.want {
				t.Fatalf("warning present = %v, want %v: %s", got, c.want, out)
			}
			if c.want && !strings.Contains(errs, "chottag daemon restart") {
				t.Fatalf("stderr lacks the restart advice: %q", errs)
			}
			if st, _ := s.Load(); !st.PolicySpread() {
				t.Fatal("the policy was not stored: the warning must not refuse")
			}
			s.Update(func(st *store.State) error { st.SetPolicy("serial"); return nil })
		})
	}
	// serial never warns, and `tag NAME` under spread does.
	restore := probe("0.6.0", true)
	defer restore()
	if _, out, _ := runChottag(t, "--json", "policy", "serial"); strings.Contains(out, warn) {
		t.Fatalf("serial warned: %s", out)
	}
	s.Update(func(st *store.State) error { st.SetPolicy("spread"); return nil })
	if _, out, _ := runChottag(t, "--json", "tag", "B"); !strings.Contains(out, warn) {
		t.Fatalf("tag under spread did not warn: %s", out)
	}
}

// A daemon that already has spread (0.7.0 or later) gets no warning, even
// when this binary is newer than it.
func TestPolicySpreadDoesNotWarnForADaemonThatHasSpread(t *testing.T) {
	policyHome(t)
	origVersion := Version
	Version = "0.7.1"
	t.Cleanup(func() { Version = origVersion })
	restore := SetStatusProbeForTest(func(int) (bool, string) { return true, "0.7.0" })
	defer restore()
	code, out, errs := runChottag(t, "--json", "policy", "spread")
	if code != exit.OK || strings.Contains(out, "daemon_predates_spread") {
		t.Fatalf("exit %d, out %q, err %q: want no daemon_predates_spread", code, out, errs)
	}
}
