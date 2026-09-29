package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestStateChecksAreOKOnAHealthyInstall(t *testing.T) {
	ti := newTestInstall(t)
	for _, r := range mustRun(t, ti.env, StateChecks(), false) {
		if r.Status != StatusOK {
			t.Errorf("%+v, want ok", r)
		}
	}
}

func TestRolesBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error {
		return st.Add(store.Account{Name: "B", Dir: filepath.Join(ti.home, "accounts", "B")})
	})
	breakThenRepair(t, ti, StateChecks(), "roles", func() {
		ti.update(func(st *store.State) error { st.Serving, st.Remote = "Gone", "B"; return nil })
	}, "home:state.json")
	st, err := ti.env.State()
	must(t, err)
	if st.Serving != "A" || st.Remote != "B" {
		t.Fatalf("roles = %s/%s, want the dangling serving on A and remote left on B", st.Serving, st.Remote)
	}
}

func TestRolesNamesBothDanglingRolesInOneRow(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error { st.Serving, st.Remote = "", "Gone"; return nil })
	r := rowByID(t, mustRun(t, ti.env, StateChecks(), false), "roles")
	if r.Status != StatusProblem || !strings.Contains(r.Detail, `serving ""`) || !strings.Contains(r.Detail, `remote "Gone"`) {
		t.Fatalf("roles = %+v", r)
	}
}

func TestRolesWithNoAccountIsReportOnly(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error { st.Accounts, st.Serving, st.Remote = nil, "", ""; return nil })
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, StateChecks(), true), "roles")
	if r.Status != StatusProblem || r.Hint != "chottag login <name>" {
		t.Fatalf("roles = %+v, want a report-only problem with the login hint", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestRealClaudeBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	breakThenRepair(t, ti, StateChecks(), "real-claude", func() {
		ti.update(func(st *store.State) error { st.RealClaude = filepath.Join(ti.home, "gone", "claude"); return nil })
	}, "home:state.json")
	st, err := ti.env.State()
	must(t, err)
	if st.RealClaude != ti.claude {
		t.Fatalf("realClaude = %q, want %q", st.RealClaude, ti.claude)
	}
}

func TestRealClaudeUnresolvableIsReportOnly(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error { st.RealClaude = ""; return nil })
	ti.env.PATH = ti.env.BinDir()
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, StateChecks(), true), "real-claude")
	if r.Status != StatusProblem || !strings.Contains(r.Detail, "no real `claude` found") || !strings.Contains(r.Hint, "install Claude Code") {
		t.Fatalf("real-claude = %+v", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestRealClaudeEmptyCacheIsInfo is controller ruling D13: a fresh install,
// which has never cached a real claude, is healthy rather than a problem.
func TestRealClaudeEmptyCacheIsInfo(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error { st.RealClaude = ""; return nil })
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, []Check{realClaudeCheck()}, true), "real-claude")
	if r.Status != StatusInfo || !strings.Contains(r.Detail, "not cached yet") {
		t.Fatalf("real-claude = %+v, want info", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// breakPort leaves the port held by something that is not a chottag
// daemon, with no daemon running.
func breakPort(ti *testInstall) {
	ti.healthy, ti.running = false, false
	ti.held[portAddr(testPort)] = true
}

func TestPortBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	ti.held[portAddr(PortRangeFirst)] = true // the first candidate is taken too
	breakThenRepair(t, ti, []Check{portCheck()}, "port", func() { breakPort(ti) }, "home:state.json")
	st, err := ti.env.State()
	must(t, err)
	if st.Port != PortRangeFirst+1 {
		t.Fatalf("port = %d, want %d", st.Port, PortRangeFirst+1)
	}
}

func TestPortNeverMovesUnderLiveSessions(t *testing.T) {
	ti := newTestInstall(t)
	breakPort(ti)
	ti.live = 2
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, []Check{portCheck()}, true), "port")
	if r.Status != StatusProblem || !strings.Contains(r.Detail, fmt.Sprintf("2 live claude session(s) were started with port %d", testPort)) {
		t.Fatalf("port = %+v, want the live-session refusal (D7)", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
	for _, a := range ti.listens {
		if a != portAddr(testPort) {
			t.Errorf("probed %s, although the fix must refuse before it scans", a)
		}
	}
}

func TestPortNeverMovesUnderARunningDaemon(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inspectErr error
	}{{"held lock", nil}, {"unreadable record", daemonlock.ErrUnreadableRecord}} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			breakPort(ti)
			ti.running, ti.inspectErr = true, tc.inspectErr
			before := ti.snapshot()
			r := rowByID(t, mustRun(t, ti.env, []Check{portCheck()}, true), "port")
			if r.Status != StatusProblem || !strings.Contains(r.Detail, "never moves the port under a running daemon") {
				t.Fatalf("port = %+v", r)
			}
			assertChangedOnly(t, before, ti.snapshot())
		})
	}
}

func TestPortWithNoFreePortInRange(t *testing.T) {
	ti := newTestInstall(t)
	breakPort(ti)
	for p := PortRangeFirst; p <= PortRangeLast; p++ {
		ti.held[portAddr(p)] = true
	}
	r := rowByID(t, mustRun(t, ti.env, []Check{portCheck()}, true), "port")
	if r.Status != StatusProblem || !strings.Contains(r.Detail, "no free port in 47822-47841") {
		t.Fatalf("port = %+v", r)
	}
}

func TestDaemonForeignOrUnhealthyIsReportOnly(t *testing.T) {
	for _, tc := range []struct {
		name             string
		running, healthy bool
		inspectErr       error
		hint             string
	}{
		{"unhealthy", true, false, nil, "chottag daemon restart"},
		{"foreign", false, true, nil, fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", testPort)},
		{"unreadable record", true, false, daemonlock.ErrUnreadableRecord, "lsof "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			ti.running, ti.healthy, ti.inspectErr = tc.running, tc.healthy, tc.inspectErr
			r := rowByID(t, mustRun(t, ti.env, []Check{daemonCheck()}, true), "daemon")
			if r.Status != StatusProblem || !strings.HasPrefix(r.Hint, tc.hint) {
				t.Fatalf("daemon = %+v, want a report-only problem with hint %q", r, tc.hint)
			}
		})
	}
}

// TestDaemonNotRunningIsInfo is controller ruling D13: a stopped daemon
// with no session open is the normal state (the shim starts one on demand,
// R38), so it is info, not a problem, and daemonCheck has no Fix at all.
func TestDaemonNotRunningIsInfo(t *testing.T) {
	if daemonCheck().Fix != nil {
		t.Fatal("daemonCheck has a Fix; a not-running daemon must never be repaired")
	}
	ti := newTestInstall(t)
	ti.running, ti.healthy = false, false
	r := rowByID(t, mustRun(t, ti.env, []Check{daemonCheck()}, false), "daemon")
	if r.Status != StatusInfo || r.Hint != "chottag daemon start" {
		t.Fatalf("daemon = %+v, want info with the daemon-start hint", r)
	}
}

// TestDaemonNotRunningFixLeavesItInfo checks that --fix against a stopped
// daemon starts nothing and writes nothing: with no Fix, Run never probes
// or touches state.json for this row.
func TestDaemonNotRunningFixLeavesItInfo(t *testing.T) {
	ti := newTestInstall(t)
	ti.running, ti.healthy = false, false
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, []Check{daemonCheck()}, true), "daemon")
	if r.Status != StatusInfo || r.Hint != "chottag daemon start" {
		t.Fatalf("daemon = %+v, want info with the daemon-start hint", r)
	}
	if len(ti.listens) != 0 {
		t.Fatalf("listens = %v, want none", ti.listens)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestStateChecksLeaveALiveDaemonAlone is Review Focus 1: doctor --fix
// against a live, serving daemon writes nothing, never probes the port
// with a bind, and never starts anything.
func TestStateChecksLeaveALiveDaemonAlone(t *testing.T) {
	ti := newTestInstall(t)
	ti.live = 3
	before := ti.snapshot()
	for _, r := range mustRun(t, ti.env, StateChecks(), true) {
		if r.Status != StatusOK {
			t.Errorf("%+v, want ok", r)
		}
	}
	if len(ti.listens) != 0 {
		t.Fatalf("listens = %v, want none", ti.listens)
	}
	assertChangedOnly(t, before, ti.snapshot())
}
