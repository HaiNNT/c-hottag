package doctor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestAllIsInSpecOrder(t *testing.T) {
	var ids []string
	gates := 0
	for _, c := range All([]string{"A", "B"}) {
		ids = append(ids, c.ID)
		if c.Gate {
			gates++
		}
	}
	want := "setup tree ca proxy-secret bin rc-block path roles identities real-claude port daemon daemon-version daemon-identity token:A token:B owners route-drift limits version-drift plan-unknown"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("order = %s\nwant    %s", got, want)
	}
	if gates != 1 {
		t.Fatalf("%d gating checks, want only setup", gates)
	}
}

func TestAHealthyInstallHasNoProblem(t *testing.T) {
	ti := newTestInstall(t)
	before := ti.snapshot()
	for _, r := range mustRun(t, ti.env, All([]string{"A"}), true) {
		if r.Status != StatusOK && r.Status != StatusInfo {
			t.Errorf("%+v, want ok or info", r)
		}
	}
	assertChangedOnly(t, before, ti.snapshot())
	if len(ti.listens) != 0 {
		t.Fatalf("listens = %v on a healthy install", ti.listens)
	}
}

func TestAFreshHomeGetsOnlyTheSetupHint(t *testing.T) {
	ti := newTestInstall(t)
	ti.env.Home = filepath.Join(t.TempDir(), "chottag")
	rows := mustRun(t, ti.env, All(nil), true)
	if rows[0].Status != StatusProblem || rows[0].Hint != "chottag setup" {
		t.Fatalf("setup = %+v", rows[0])
	}
	for _, r := range rows[1:] {
		if r.Status != StatusSkipped {
			t.Errorf("%+v, want skipped", r)
		}
	}
}

// TestDetectOnABrokenInstallWritesNothing breaks one thing in each fixable
// group and checks the exact set of ids that come back as a problem (D13:
// daemon not running is info, never a problem, so it is deliberately left
// out).
func TestDetectOnABrokenInstallWritesNothing(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "accounts")))
	must(t, os.Remove(filepath.Join(ti.home, "ca", "ca.key")))
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	must(t, os.WriteFile(ti.rc, []byte("# mine\n"), 0o644))
	ti.update(func(st *store.State) error { st.Serving, st.RealClaude = "Gone", "/nope/claude"; return nil })
	ti.running, ti.healthy = false, false
	ti.held[portAddr(testPort)] = true
	writeTestStatus(t, ti.home, status.File{Accounts: []status.Account{{Name: "A", Token: creds.StateNeedsLogin}}})
	writeTestOwners(t, ti.home, map[string]string{"artifact:x": "Gone"})
	before := ti.snapshot()
	rows := mustRun(t, ti.env, All([]string{"A"}), false)
	var problems []string
	for _, r := range rows {
		if r.Status == StatusProblem {
			problems = append(problems, r.ID)
		}
	}
	sort.Strings(problems)
	want := []string{"bin", "ca", "port", "rc-block", "real-claude", "roles", "token:A", "tree"}
	if strings.Join(problems, " ") != strings.Join(want, " ") {
		t.Fatalf("problem ids = %v\nwant           %v\nrows: %+v", problems, want, rows)
	}
	if r := rowByID(t, rows, "daemon"); r.Status != StatusInfo {
		t.Errorf("daemon = %+v, want info (D13: not running is never a problem)", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}
