package cli

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/session"
)

// daemonJSONStopHome is stopHome plus a signal stub that records and
// delivers nothing: every daemon verb test writes a test port first.
func daemonJSONStopHome(t *testing.T) string {
	t.Helper()
	home := stopHome(t)
	stubSignals(t, nil)
	return home
}

// daemonJSONSupervised holds the lock as a launchd-supervised daemon.
func daemonJSONSupervised(t *testing.T, home string) {
	t.Helper()
	rec := fakeRecord(4242)
	rec.Supervisor, rec.Label = "launchd", "com.chottag.daemon"
	holdLock(t, home, rec)
}

// daemonJSONLiveSession registers a live session: a helper process this
// test started itself (the registry keeps only live pids).
func daemonJSONLiveSession(t *testing.T, home string, port int) int {
	t.Helper()
	idle := startHelperProcess(t, home, "idle")
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(idle.pid(), port); err != nil {
		t.Fatal(err)
	}
	return idle.pid()
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "daemon stop when not running", command: "daemon stop",
			setup: func(t *testing.T) []string { daemonJSONStopHome(t); return []string{"daemon", "stop"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["stopped"] != false || doc["temporary"] != false {
					t.Errorf("doc = %v, want stopped false", doc)
				}
				if _, ok := doc["pid"]; ok {
					t.Errorf("doc = %v, want no pid when nothing ran", doc)
				}
			},
		},
		jsonCase{
			name: "daemon stop of a supervised daemon", command: "daemon stop",
			setup: func(t *testing.T) []string {
				home := daemonJSONStopHome(t)
				daemonJSONSupervised(t, home)
				return []string{"daemon", "stop"}
			},
			wantExit: exit.UserAction, wantCode: codeSupervised,
			check: func(t *testing.T, doc map[string]any) {
				if c, _ := docError(t, doc)["command"].(string); !strings.HasPrefix(c, "launchctl bootout gui/") {
					t.Errorf("error.command = %q, want the launchctl bootout line", c)
				}
			},
		},
		jsonCase{
			name: "daemon start of a running healthy daemon", command: "daemon start",
			setup: func(t *testing.T) []string {
				home := startHome(t)
				d := newHealthDaemon(t, true, "")
				writeStateWithPort(t, home, d.port)
				holdLock(t, home, fakeRecord(4242))
				startNoSpawn(t)
				return []string{"daemon", "start"}
			},
			check: func(t *testing.T, doc map[string]any) {
				if doc["pid"] != float64(4242) || doc["started"] != false {
					t.Errorf("doc = %v, want pid 4242, started false", doc)
				}
			},
		},
		jsonCase{
			name: "daemon start of a wedged daemon", command: "daemon start",
			setup: func(t *testing.T) []string {
				home := startHome(t)
				writeStateWithPort(t, home, closedPort(t))
				holdLock(t, home, fakeRecord(4242))
				startNoSpawn(t)
				return []string{"daemon", "start"}
			},
			wantExit: exit.Error, wantCode: codeUnhealthy,
		},
		jsonCase{
			name: "daemon restart replaces the daemon", command: "daemon restart",
			setup: func(t *testing.T) []string {
				d := newHealthDaemon(t, true, "")
				home := restartHome(t, d.port)
				restartReplaceable(t, home, d)
				return []string{"daemon", "restart"}
			},
			check: func(t *testing.T, doc map[string]any) {
				if doc["oldPid"] != float64(4242) || doc["pid"] != float64(5151) || doc["relaunchedBy"] != "chottag" {
					t.Errorf("doc = %v, want oldPid 4242, pid 5151, relaunchedBy chottag", doc)
				}
			},
		},
		jsonCase{
			name: "daemon restart of a supervised daemon", command: "daemon restart",
			setup: func(t *testing.T) []string {
				home := restartHome(t, closedPort(t))
				daemonJSONSupervised(t, home)
				stubSignals(t, nil)
				stubSpawn(t, func(string, string, string) error { t.Error("spawned under a supervisor"); return nil })
				return []string{"daemon", "restart"}
			},
			wantExit: exit.UserAction, wantCode: codeSupervised,
		},
	)
}

func TestDaemonStopJSONReportsTheStoppedPid(t *testing.T) {
	home := stopHome(t)
	release := holdLock(t, home, fakeRecord(4242))
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			time.AfterFunc(50*time.Millisecond, release)
		}
		return nil
	})
	code, out, errb := runChottag(t, "daemon", "stop", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	if doc["pid"] != float64(4242) || doc["stopped"] != true || doc["alreadyExited"] != false || doc["temporary"] != true {
		t.Errorf("doc = %v, want pid 4242, stopped, not alreadyExited, temporary", doc)
	}
}

func TestDaemonStopJSONCarriesLiveSessionPids(t *testing.T) {
	home := daemonJSONStopHome(t)
	holdLock(t, home, fakeRecord(4242))
	pid := daemonJSONLiveSession(t, home, 1)
	code, out, _ := runChottag(t, "--json", "daemon", "stop")
	if code != exit.UserAction {
		t.Fatalf("exit = %d, want %d", code, exit.UserAction)
	}
	e := docError(t, decodeOneDocument(t, out))
	pids, _ := e["pids"].([]any)
	if e["code"] != "live_sessions" || len(pids) != 1 || pids[0] != float64(pid) {
		t.Errorf("error = %v, want live_sessions with pids [%d]", e, pid)
	}
}

// restart's live-sessions line is on stdout in text mode. Under --json it
// must be a warning, never text in front of the document.
func TestDaemonRestartJSONPutsTheLiveSessionLineInWarnings(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	restartReplaceable(t, home, d)
	daemonJSONLiveSession(t, home, d.port)
	code, out, errb := runChottag(t, "daemon", "restart", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	if doc["liveSessions"] != float64(1) {
		t.Errorf("liveSessions = %v, want 1", doc["liveSessions"])
	}
	ws, _ := doc["warnings"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != "live_sessions" {
		t.Errorf("warnings = %v, want one live_sessions", ws)
	}
}

// The upstream change is redacted in the document exactly as in the text
// (F139): the credential never appears on stdout or stderr.
func TestDaemonRestartJSONCarriesARedactedUpstreamChange(t *testing.T) {
	d := newHealthDaemon(t, true, "http://corp-a:8080")
	home := restartHome(t, d.port)
	restartReplaceable(t, home, d)
	t.Setenv("HTTPS_PROXY", "http://bob:hunter2@corp-b:8080")
	code, out, errb := runChottag(t, "--json", "daemon", "restart")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	uc, _ := doc["upstreamChanged"].(map[string]any)
	if uc["from"] != "http://corp-a:8080" || !strings.Contains(uc["to"].(string), "corp-b:8080") {
		t.Errorf("upstreamChanged = %v, want from corp-a to corp-b", uc)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(errb, "hunter2") {
		t.Errorf("the proxy credential leaked: stdout=%q stderr=%q", out, errb)
	}
	ws, _ := doc["warnings"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != "upstream_changed" {
		t.Errorf("warnings = %v, want one upstream_changed", ws)
	}
}

// A failure of restart's stop half is stop_failed, with the stop's own
// code as its cause; nothing is started.
func TestDaemonRestartJSONReportsAStopFailureAsStopFailed(t *testing.T) {
	shortenStopTimings(t, 50*time.Millisecond, 50*time.Millisecond)
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	holdLock(t, home, fakeRecord(4242)) // never released: survives SIGKILL
	stubSignals(t, nil)
	stubSpawn(t, func(string, string, string) error {
		t.Error("spawned although the old daemon never stopped")
		return nil
	})
	code, out, _ := runChottag(t, "daemon", "restart", "--json")
	if code != exit.Error {
		t.Fatalf("exit = %d, want %d", code, exit.Error)
	}
	e := docError(t, decodeOneDocument(t, out))
	if e["code"] != "stop_failed" || e["cause"] != "stop_timeout" {
		t.Errorf("error = %v, want stop_failed caused by stop_timeout", e)
	}
	// asStopFailed must carry the stop half's own details (here, pid)
	// forward, not just its cause: dropping that merge would still leave
	// cause correct, so this checks pid explicitly.
	if e["pid"] != float64(4242) {
		t.Errorf("error.pid = %v, want 4242 (the stop half's own detail, carried through asStopFailed)", e["pid"])
	}
}

// A plain `stop --force` that outlives SIGKILL reports stop_timeout
// directly (never wrapped as stop_failed: that wrapping is restart's own
// job), with the pid it could not confirm gone.
func TestDaemonStopJSONReportsStopTimeout(t *testing.T) {
	shortenStopTimings(t, 50*time.Millisecond, 50*time.Millisecond)
	home := stopHome(t)
	holdLock(t, home, fakeRecord(4242)) // never released: survives SIGKILL
	stubSignals(t, nil)
	code, out, errb := runChottag(t, "daemon", "stop", "--force", "--json")
	if code != exit.Error {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Error, errb)
	}
	e := docError(t, decodeOneDocument(t, out))
	if e["code"] != "stop_timeout" || e["pid"] != float64(4242) {
		t.Errorf("error = %v, want stop_timeout with pid 4242", e)
	}
}

// A forced restart of a systemd-supervised daemon that never relaunches
// reports supervisor_no_relaunch with the supervisor's own restart command,
// never falling back to the internal document.
func TestDaemonRestartJSONReportsSupervisorNoRelaunch(t *testing.T) {
	old := restartSupervisorWait
	restartSupervisorWait = 100 * time.Millisecond
	t.Cleanup(func() { restartSupervisorWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	rec := fakeRecord(4242)
	rec.Supervisor = "systemd"
	release := holdLock(t, home, rec)
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		d.up.Store(false)
		release()
		return nil
	})
	// No stubSpawn: a forced restart of a supervised daemon never spawns
	// (F140); TestMain's panicking default would fail this test if it did.
	code, out, errb := runChottag(t, "daemon", "restart", "--force", "--json")
	if code != exit.Error {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Error, errb)
	}
	e := docError(t, decodeOneDocument(t, out))
	cmd, _ := e["command"].(string)
	if e["code"] != "supervisor_no_relaunch" || !strings.Contains(cmd, "systemctl --user restart chottag.service") {
		t.Errorf("error = %v, want supervisor_no_relaunch naming the systemctl restart command", e)
	}
}

// restart with nothing running falls through to ensureStarted; a spawn that
// never answers health is start_failed, exactly as `daemon start` reports
// it, never the internal fallback.
func TestDaemonRestartJSONReportsStartFailedWhenNothingWasRunning(t *testing.T) {
	restartHome(t, closedPort(t))
	stubSpawn(t, func(string, string, string) error { return nil }) // never takes the lock
	code, out, errb := runChottag(t, "daemon", "restart", "--json")
	if code != exit.Error {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Error, errb)
	}
	e := docError(t, decodeOneDocument(t, out))
	if e["code"] != "start_failed" {
		t.Errorf("error = %v, want start_failed", e)
	}
}

// The daemon verbs' flags are order-free, with --json anywhere (including
// between `daemon` and its verb); a flag after -- is a positional, exit 2.
func TestDaemonVerbFlagOrder(t *testing.T) {
	for _, args := range [][]string{
		{"daemon", "stop", "--force", "--json"},
		{"--json", "daemon", "stop", "--force"},
		{"daemon", "--json", "stop", "--force"},
		{"daemon", "stop", "--json", "--force"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := stopHome(t)
			rec := fakeRecord(4242)
			rec.Supervisor, rec.Label = "launchd", "com.chottag.daemon"
			release := holdLock(t, home, rec)
			stubSignals(t, func(pid int, sig syscall.Signal) error {
				if sig == syscall.SIGTERM {
					release()
				}
				return nil
			})
			// --force must get past the supervisor refusal (exit 3) in every
			// order; the stub plays the daemon exiting on SIGTERM.
			code, out, errb := runChottag(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0 (--force parsed); stderr=%q", code, errb)
			}
			doc := decodeOneDocument(t, out)
			if doc["stopped"] != true {
				t.Errorf("doc = %v, want stopped", doc)
			}
			ws, _ := doc["warnings"].([]any)
			if len(ws) != 1 || ws[0].(map[string]any)["code"] != "supervisor_relaunch" {
				t.Errorf("warnings = %v, want one supervisor_relaunch", ws)
			}
		})
	}
	for _, args := range [][]string{{"daemon", "stop", "--", "--force"}, {"daemon", "restart", "--", "--force"}, {"daemon", "stop", "now", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			daemonJSONStopHome(t)
			if code, _, errb := runChottag(t, args...); code != exit.Usage || !strings.Contains(errb, "unexpected argument") {
				t.Fatalf("exit = %d stderr=%q, want 2 and \"unexpected argument\"", code, errb)
			}
		})
	}
}
