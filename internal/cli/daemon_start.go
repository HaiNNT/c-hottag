package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/shim"
)

// startResult is `daemon start --json`'s fields (spec §5.3). pid is absent
// when the lock record is unreadable (pid unknown); started is false when
// a daemon was already running.
type startResult struct {
	PID     int  `json:"pid,omitempty"`
	Port    int  `json:"port"`
	Started bool `json:"started"`
}

// runDaemonStart is `chottag daemon start` (spec §5). The shim starts the
// daemon on demand anyway (spec §4.3); this is the same start, on request.
func runDaemonStart(args []string, r *reporter) int {
	if len(args) != 0 {
		fmt.Fprintf(r.Stderr(), "chottag: unexpected argument %q\nusage: chottag daemon start\n", args[0])
		return r.FailNoText(exit.Usage, codeUsage, fmt.Sprintf("unexpected argument %q", args[0]), nil)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	port, err := daemonPort(h)
	if err != nil {
		return r.FailErr(err)
	}
	res, fail := ensureStarted(h, port, r)
	if fail != nil {
		return fail.report(r)
	}
	return r.OK(res)
}

// ensureStarted leaves home with a running, healthy daemon, or says why it
// cannot. The daemon is found through its held lock (spec §4.6), is judged
// up through the shim's own health probe, and is started through the shim's
// own spawn, with the upstream resolved from this process's environment
// exactly as the shim resolves it (F103). restart calls this after
// stopping. Its result line goes through r.Text; a failure is returned
// unreported, for the calling verb to report (M1d-d).
func ensureStarted(h string, port int, r *reporter) (startResult, *daemonFailure) {
	st, err := daemonlock.Inspect(h)
	if err != nil && !errors.Is(err, daemonlock.ErrUnreadableRecord) {
		return startResult{}, internalFailure(err)
	}
	if st.Running {
		pid := describePID(st.Record)
		if ok, _ := shim.ProbeHealth(port); ok {
			r.Text("chottag: daemon already running (%s)\n", pid)
			return startResult{PID: st.Record.PID, Port: port, Started: false}, nil
		}
		if errors.Is(err, daemonlock.ErrUnreadableRecord) {
			// `daemon restart` is not a real suggestion here: it refuses
			// this exact case too (its own ErrUnreadableRecord check), so
			// it would only be a dead end.
			return startResult{}, unreadableRecordFailure(h)
		}
		return startResult{}, newDaemonFailure(exit.Error, codeUnhealthy,
			fmt.Sprintf("the daemon (%s) holds %s but is not answering on port %d; run `chottag daemon restart`", pid, daemonlock.Path(h), port),
			map[string]any{"pid": st.Record.PID, "port": port})
	}
	if ok, _ := shim.ProbeHealth(port); ok {
		return startResult{}, foreignDaemonFailure(h, port)
	}

	confirmed, _, spawnErr := shim.StartDaemon(h, port, os.Environ())
	if !confirmed {
		// A spawned daemon has no real stderr (shim's spawnDaemon sets
		// none), so daemon.log is where its failure is, or `daemon run`
		// shows it directly.
		msg := fmt.Sprintf("the daemon did not answer on port %d in time; see %s, or run `chottag daemon run` to see the startup error directly", port, filepath.Join(h, "daemon.log"))
		if spawnErr != nil {
			msg += fmt.Sprintf(" (starting it failed: %v)", spawnErr)
		}
		return startResult{}, newDaemonFailure(exit.Error, codeStartFailed, msg, map[string]any{"port": port})
	}
	st, err = daemonlock.Inspect(h)
	if err != nil && !errors.Is(err, daemonlock.ErrUnreadableRecord) {
		return startResult{}, internalFailure(err)
	}
	if !st.Running {
		// Healthy, but not a daemon of this home: ours failed (a bind race,
		// say), and something else answers (F138).
		return startResult{}, foreignDaemonFailure(h, port)
	}
	r.Text("chottag: started the daemon (%s)\n", describePID(st.Record))
	return startResult{PID: st.Record.PID, Port: port, Started: true}, nil
}

// describePID names a lock holder for a message: "pid N", or "pid unknown"
// for the zero Record that Inspect returns with ErrUnreadableRecord.
func describePID(rec daemonlock.Record) string {
	if rec.PID <= 0 {
		return "pid unknown"
	}
	return fmt.Sprintf("pid %d", rec.PID)
}
