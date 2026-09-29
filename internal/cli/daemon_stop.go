package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/shim"
)

// Timing for `daemon stop` (spec §5). These are vars only so tests can
// shorten the grace periods; production never changes them. stopGrace is
// longer than the daemon's own shutdownGrace (proxy.go), so a graceful
// drain finishes before SIGKILL is considered.
var (
	stopPoll  = 50 * time.Millisecond
	stopGrace = 10 * time.Second
	killGrace = 2 * time.Second
)

// stopResult is `daemon stop --json`'s fields (spec §5.3). Not running is
// ok with stopped false and no pid. stopped is true only when a signal was
// sent; alreadyExited means the holder was gone before one was. temporary
// is true whenever a daemon was stopped: the next `claude` launch, or its
// supervisor, starts it again.
type stopResult struct {
	PID           int  `json:"pid,omitempty"`
	Stopped       bool `json:"stopped"`
	AlreadyExited bool `json:"alreadyExited"`
	Temporary     bool `json:"temporary"`
}

// runDaemonStop is `chottag daemon stop [--force]` (spec §5). It finds the
// daemon through its held lock (spec §4.6), never through the health
// document's pid (spec §4.8).
func runDaemonStop(args []string, r *reporter) int {
	flags := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	flags.SetOutput(r.Stderr())
	force := flags.Bool("force", false, "stop even under a supervisor or with live claude sessions")
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		fmt.Fprintf(r.Stderr(), "chottag: unexpected argument %q\nusage: chottag daemon stop [--force]\n", positional[0])
		return r.FailNoText(exit.Usage, codeUsage, fmt.Sprintf("unexpected argument %q", positional[0]), nil)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	st, err := daemonlock.Inspect(h)
	switch {
	case errors.Is(err, daemonlock.ErrUnreadableRecord):
		return unreadableRecordFailure(h).report(r)
	case err != nil:
		return r.FailErr(err)
	case !st.Running:
		return reportNotRunning(h, r)
	}
	rec := st.Record
	if rec.Supervisor != "" && !*force {
		cmd := supervisorCommand(rec, "stop", os.Getuid())
		fmt.Fprintf(r.Stderr(), "chottag: the daemon (pid %d) runs under %s, which restarts whatever `daemon stop` kills. Stop it through %s instead:\n  %s\nor pass --force to kill it anyway.\n",
			rec.PID, rec.Supervisor, rec.Supervisor, cmd)
		return r.FailNoText(exit.UserAction, codeSupervised,
			fmt.Sprintf("the daemon (pid %d) runs under %s, which restarts whatever `daemon stop` kills; stop it through %s instead, or pass --force", rec.PID, rec.Supervisor, rec.Supervisor),
			map[string]any{"command": cmd, "supervisor": rec.Supervisor, "pid": rec.PID})
	}
	if !*force {
		live, err := liveSessions(h)
		if err != nil {
			return r.Fail(exit.Error, codeSessionRegistryUnreadable, fmt.Sprintf("could not read the session registry: %v", err), nil)
		}
		if len(live) > 0 {
			fmt.Fprintf(r.Stderr(), "chottag: %d live claude session(s) use this daemon (%s); they lose the proxy until the next `claude` launch. Pass --force to stop it anyway.\n",
				len(live), session.Describe(live))
			return r.FailNoText(exit.UserAction, codeLiveSessions,
				fmt.Sprintf("%d live claude session(s) use this daemon; pass --force to stop it anyway", len(live)),
				map[string]any{"pids": sessionPIDs(live)})
		}
	}
	signalled, fail := terminate(h, rec, r)
	if fail != nil {
		return fail.report(r)
	}
	if rec.Supervisor != "" {
		// --force killed it past the supervisor check above, but the
		// supervisor itself (KeepAlive / Restart=always) restarts whatever
		// just died, immediately, not "on the next `claude` launch". Saying
		// otherwise here would be false, so this names the supervisor's own
		// stop command instead (spec §5).
		r.Warn(warnSupervisorRelaunch, strings.TrimSuffix(supervisorRelaunchNote(rec, "stop", os.Getuid()), "\n"))
	} else {
		r.Text("chottag: this is temporary: the next `claude` launch starts the daemon again.\n")
	}
	return r.OK(stopResult{PID: rec.PID, Stopped: signalled, AlreadyExited: !signalled, Temporary: true})
}

// reportNotRunning answers "not running" only when that is true. A chottag
// daemon that still answers the health probe on the port while nothing
// holds the lock was started by an older chottag or under another
// CHOTTAG_HOME (F138). Saying "not running" would be false, and stop has
// no safe way to identify it.
func reportNotRunning(h string, r *reporter) int {
	if port, err := daemonPort(h); err == nil {
		if ok, _ := shim.ProbeHealth(port); ok {
			return foreignDaemonFailure(h, port).report(r)
		}
	}
	r.Text("chottag: daemon not running\n")
	return r.OK(stopResult{})
}

// terminate stops the daemon rec names: SIGTERM (its graceful shutdown),
// then SIGKILL once stopGrace has passed. The lock is re-read before every
// signal, and once it no longer names rec (the daemon exited on its own,
// or a `claude` launch has already replaced it), nothing more is sent. A
// pid is never signalled after its holder has gone (spec §5). signalled is
// false when rec's holder was already gone before any signal. A failure is
// returned unreported: stop reports it as-is, restart as stop_failed.
func terminate(h string, rec daemonlock.Record, r *reporter) (signalled bool, fail *daemonFailure) {
	// Defense in depth: rec always comes from a just-Inspect'd record in
	// practice (runDaemonStop), never a caller-built pid, so this should be
	// unreachable — but a 0 or negative pid handed to kill(2) means "every
	// process in this pid's process group" or "every process this caller
	// may signal" (kill(0)/kill(-1)), never a single daemon. Checked once,
	// before any signal is even considered, rather than trusted implicitly.
	if rec.PID <= 0 {
		return false, newDaemonFailure(exit.Error, codeInternal, fmt.Sprintf("refusing to signal pid %d", rec.PID), nil)
	}
	steps := []struct {
		sig   syscall.Signal
		grace time.Duration
	}{{syscall.SIGTERM, stopGrace}, {syscall.SIGKILL, killGrace}}
	for _, step := range steps {
		gone, err := signalHolder(h, rec, step.sig)
		if err != nil {
			var sf *signalFailedError
			switch {
			case errors.Is(err, daemonlock.ErrUnreadableRecord):
				return signalled, unreadableRecordFailure(h)
			case errors.As(err, &sf):
				return signalled, newDaemonFailure(exit.Error, codeSignalFailed, err.Error(), map[string]any{"pid": rec.PID})
			default:
				return signalled, internalFailure(err)
			}
		}
		if !gone {
			signalled = true
		}
		if gone || waitForRelease(h, rec, step.grace) {
			reportStopped(h, rec, signalled, r)
			return signalled, nil
		}
	}
	return signalled, newDaemonFailure(exit.Error, codeStopTimeout,
		fmt.Sprintf("the daemon (pid %d) still holds %s after SIGKILL", rec.PID, daemonlock.Path(h)),
		map[string]any{"pid": rec.PID})
}

// signalFailedError marks a kill(2) that failed (anything but ESRCH), so
// terminate can tell it from a failure to read the lock.
type signalFailedError struct{ err error }

func (e *signalFailedError) Error() string { return e.err.Error() }
func (e *signalFailedError) Unwrap() error { return e.err }

// signalHolder re-reads the lock, and signals rec.PID only while rec's
// daemon still holds it. gone is true when it no longer does, in which case
// nothing is sent. A holder whose record cannot be read is not rec's to
// signal, so that is an error (terminate recognizes
// daemonlock.ErrUnreadableRecord specially and reports it, rather than the
// generic error text). ESRCH (the process vanished between the re-read and
// the kill) is not itself treated as a stop: signalFn simply reports no
// error for it, exactly as on a delivered signal, and it is waitForRelease,
// back in terminate, that confirms the daemon is actually gone by polling
// the lock. A residual pid-reuse window remains between the re-read above
// and the kill(2) call below: rec.PID would have to die AND the OS reuse
// that same number for an unrelated process, both within the microseconds
// this function takes to run, all while the on-disk record stays
// byte-identical to rec. That is accepted here, as it is by every
// kill(2)-based process manager: no OS-level lock can be held across a
// signal, and the window is negligible next to how long pid reuse actually
// takes to occur in practice.
func signalHolder(h string, rec daemonlock.Record, sig syscall.Signal) (gone bool, err error) {
	st, err := daemonlock.Inspect(h)
	if err != nil {
		return false, err
	}
	if !st.Running || !st.Record.Same(rec) {
		return true, nil
	}
	if err := signalFn(rec.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, &signalFailedError{fmt.Errorf("signal %v to the daemon (pid %d): %w", sig, rec.PID, err)}
	}
	return false, nil
}

// waitForRelease polls the lock every stopPoll until rec's daemon no longer
// holds it, or budget passes.
func waitForRelease(h string, rec daemonlock.Record, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if st, err := daemonlock.Inspect(h); err == nil && (!st.Running || !st.Record.Same(rec)) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(stopPoll)
	}
}

// reportStopped confirms the outcome, noting a new daemon that has already
// taken the lock (a `claude` launch starts one on demand). signalled is
// false only when rec's holder was already gone before terminate ever sent
// a signal — nothing was actually stopped here, so that is said plainly
// rather than claiming credit for it. The new-daemon note stays on stdout
// in text mode and is a warning under --json.
func reportStopped(h string, rec daemonlock.Record, signalled bool, r *reporter) {
	if signalled {
		r.Text("chottag: stopped the daemon (pid %d)\n", rec.PID)
	} else {
		r.Text("chottag: the daemon (pid %d) had already exited\n", rec.PID)
	}
	if st, err := daemonlock.Inspect(h); err == nil && st.Running && !st.Record.Same(rec) {
		r.TextWarn(warnNewDaemon, fmt.Sprintf("chottag: note: a new daemon (pid %d) has already started", st.Record.PID))
	}
}
