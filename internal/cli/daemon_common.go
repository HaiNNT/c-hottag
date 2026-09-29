package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// signalFn sends sig to pid. It is the one irreversible act of `daemon
// stop` and `daemon restart`, so it sits behind a seam, like shim's
// execFn/spawnFn and this package's claudeAuthExec/credsDelete. TestMain
// (invariants_test.go) installs a default that panics, so a test that
// forgets SetSignalForTest fails loudly instead of signalling a pid read
// from a lock record (F116).
var signalFn = syscall.Kill

// SetSignalForTest swaps signalFn for fn and returns a func that restores
// whatever was installed at the moment of the call: never production's
// syscall.Kill by name. That keeps TestMain's panicking default armed
// across every stub-and-restore cycle (F130). Same contract as
// shim.SetSeamsForTest and SetAuthExecForTest. It lives in a non-test file
// for the same reason they do (F103).
func SetSignalForTest(fn func(pid int, sig syscall.Signal) error) (restore func()) {
	orig := signalFn
	signalFn = fn
	return func() { signalFn = orig }
}

// systemdUnit is the unit packaging/systemd ships. stop and restart print
// it for a daemon running under systemd.
const systemdUnit = "chottag.service"

// supervisorCommand is the command that does verb ("stop" or "restart")
// through rec's supervisor, or "" when rec has none. A KeepAlive or
// Restart=always unit restarts whatever `daemon stop` kills, so for a
// supervised daemon stop and restart print this instead of signalling
// (spec §5). uid is the caller's (os.Getuid()); it is a parameter only so
// a test can pin the text.
func supervisorCommand(rec daemonlock.Record, verb string, uid int) string {
	switch rec.Supervisor {
	case "launchd":
		if verb == "restart" {
			return fmt.Sprintf("launchctl kickstart -k gui/%d/%s", uid, rec.Label)
		}
		return fmt.Sprintf("launchctl bootout gui/%d/%s", uid, rec.Label)
	case "systemd":
		return fmt.Sprintf("systemctl --user %s %s", verb, systemdUnit)
	}
	return ""
}

// supervisorRelaunchNote is the one place stop and restart both explain,
// on stderr, what a supervised daemon's own launchd KeepAlive or systemd
// Restart=always unit does once --force has signalled its process directly
// (a duplicated inline stdout text in stop vs a hedged supervisorForceNote
// in restart used to drift here — fix round 5). Result lines ("stopped…",
// "started…") stay on stdout regardless; this is always on stderr. Wording
// differs by verb because the two commands' relationship to the relaunch
// differs, not just its tense: stop's relaunch is a plain fact, already
// true the moment stop's signal lands, restarting the very process stop
// just killed, so it names it as a certainty ("will relaunch it") and
// points at the supervisor's own stop command. restart (F140) does not
// race the relaunch at all: it never spawns a competing daemon of its own
// once --force has bypassed a supervisor, and is itself the one waiting on
// that relaunch by the time this prints, so it says so, and points at the
// supervisor's own restart command as the alternative to waiting. uid is
// the caller's (os.Getuid()); a parameter only so a test can pin the text,
// same as supervisorCommand above.
func supervisorRelaunchNote(rec daemonlock.Record, verb string, uid int) string {
	cmd := supervisorCommand(rec, verb, uid)
	if verb == "stop" {
		return fmt.Sprintf("chottag: %s will relaunch it; stop it there instead:\n  %s\n", rec.Supervisor, cmd)
	}
	return fmt.Sprintf("chottag: waiting for %s to relaunch its own daemon; use %s directly instead if you would rather not wait:\n  %s\n",
		rec.Supervisor, rec.Supervisor, cmd)
}

// daemonPort is the port home's daemon binds: state.json's port, or the
// default (spec §4.8).
func daemonPort(h string) (int, error) {
	st, err := store.Store{Dir: h}.Load()
	if err != nil {
		return 0, err
	}
	return st.ResolvedPort(), nil
}

// liveSessions lists the managed claude sessions still alive for home
// (spec §4.3 step 6). This is session.Live()'s first production reader.
func liveSessions(h string) ([]session.Session, error) {
	reg, err := session.Open(filepath.Join(h, "run"))
	if err != nil {
		return nil, err
	}
	return reg.Live()
}

// foreignDaemonMessage explains a chottag daemon that answers the health
// probe on port while nothing holds home's lock (F138). It was started by
// a chottag that predates the lock, or under another CHOTTAG_HOME that
// shares the port. Nothing here can identify it safely: spec §4.8 rules
// out the health document's pid.
func foreignDaemonMessage(h string, port int) string {
	return fmt.Sprintf("chottag: a chottag daemon answers on port %d, but none holds %s: it was started by an older chottag, or under a different CHOTTAG_HOME. Stop that process, then retry. Find it with `lsof -nP -iTCP:%d -sTCP:LISTEN`.", port, daemonlock.Path(h), port)
}

// unreadableRecordMessage explains a lock that is held while its record is
// unreadable. The holder's pid is unknown, so nothing may be signalled.
func unreadableRecordMessage(h string) string {
	return fmt.Sprintf("chottag: a daemon holds %s but its record is unreadable, so its pid is unknown; nothing was signalled. Find the holder with `lsof %s` and stop it yourself.", daemonlock.Path(h), daemonlock.Path(h))
}

// daemonFailure is a refusal or failure found by a helper that more than one
// daemon verb shares (terminate, ensureStarted, waitForSupervisorRelaunch).
// The helper builds it without reporting it, because the verb decides how:
// `stop` reports terminate's failure as it is, while `restart` reports it as
// stop_failed with the original code as its cause (spec §5.3). text is the
// exact stderr text, "chottag: " prefix and trailing newline included, so
// text mode stays byte-identical to M1d-c.
type daemonFailure struct {
	exit    int
	code    errCode
	text    string
	message string
	details map[string]any
}

// newDaemonFailure is the common single-line shape: text is "chottag: "+msg.
func newDaemonFailure(exitCode int, code errCode, msg string, details map[string]any) *daemonFailure {
	return &daemonFailure{exit: exitCode, code: code, text: "chottag: " + msg + "\n", message: msg, details: details}
}

// report writes f's text and its document, and returns its exit code.
func (f *daemonFailure) report(r *reporter) int {
	fmt.Fprint(r.Stderr(), f.text)
	return r.FailNoText(f.exit, f.code, f.message, f.details)
}

// asStopFailed is f as `restart` reports a failure of its stop half.
func (f *daemonFailure) asStopFailed() *daemonFailure {
	details := map[string]any{"cause": string(f.code)}
	for k, v := range f.details {
		details[k] = v
	}
	return &daemonFailure{exit: exit.Error, code: codeStopFailed, text: f.text, message: f.message, details: details}
}

func internalFailure(err error) *daemonFailure {
	return newDaemonFailure(exit.Error, codeInternal, err.Error(), nil)
}

func foreignDaemonFailure(h string, port int) *daemonFailure {
	return newDaemonFailure(exit.Error, codeForeignDaemon, strings.TrimPrefix(foreignDaemonMessage(h, port), "chottag: "), map[string]any{"port": port})
}

func unreadableRecordFailure(h string) *daemonFailure {
	return newDaemonFailure(exit.Error, codeUnreadableRecord, strings.TrimPrefix(unreadableRecordMessage(h), "chottag: "), nil)
}

// sessionPIDs is live_sessions' details.pids.
func sessionPIDs(ss []session.Session) []int {
	pids := make([]int, 0, len(ss))
	for _, s := range ss {
		pids = append(pids, s.PID)
	}
	return pids
}
