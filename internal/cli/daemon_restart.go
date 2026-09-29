package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/shim"
)

// restartPortWait bounds how long restart waits, after the old daemon has
// released its lock, for its listener to stop answering. It is a var only
// so tests can shorten it.
var restartPortWait = 2 * time.Second

// restartSupervisorWait bounds how long a forced restart of a supervised
// daemon waits for the supervisor itself to relaunch it, past launchd's own
// ~10s KeepAlive throttle (F140). A var only so tests can shorten it; any
// test's own timing bound is derived from whatever value it sets here.
var restartSupervisorWait = 15 * time.Second

// upstreamChange is restart's redacted upstream note (F139): from is the
// running daemon's upstream, to is what the restarted one gets from this
// shell. Both are rendered exactly as the text note renders them.
type upstreamChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// note is the text line, unchanged from M1d-c.
func (c *upstreamChange) note() string {
	return fmt.Sprintf("chottag: note: the restarted daemon's upstream proxy changes from %s to %s (restart takes it from this shell's HTTPS_PROXY)", c.From, c.To)
}

// restartResult is `daemon restart --json`'s fields (spec §5.3). oldPid is
// absent when no daemon was running; pid is absent when the new daemon's
// record is unreadable. relaunchedBy is "chottag", or the supervisor's name
// when --force left the relaunch to it (F140).
type restartResult struct {
	OldPID          int             `json:"oldPid,omitempty"`
	PID             int             `json:"pid,omitempty"`
	Port            int             `json:"port"`
	RelaunchedBy    string          `json:"relaunchedBy"`
	UpstreamChanged *upstreamChange `json:"upstreamChanged,omitempty"`
	LiveSessions    int             `json:"liveSessions"`
}

// runDaemonRestart is `chottag daemon restart [--force]` (spec §5): stop,
// then start. Unlike stop, live sessions do not refuse it; they only see a
// brief interruption.
func runDaemonRestart(args []string, r *reporter) int {
	flags := flag.NewFlagSet("daemon restart", flag.ContinueOnError)
	flags.SetOutput(r.Stderr())
	force := flags.Bool("force", false, "restart even under a supervisor")
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		fmt.Fprintf(r.Stderr(), "chottag: unexpected argument %q\nusage: chottag daemon restart [--force]\n", positional[0])
		return r.FailNoText(exit.Usage, codeUsage, fmt.Sprintf("unexpected argument %q", positional[0]), nil)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	port, err := daemonPort(h)
	if err != nil {
		return r.FailErr(err)
	}
	st, err := daemonlock.Inspect(h)
	switch {
	case errors.Is(err, daemonlock.ErrUnreadableRecord):
		return unreadableRecordFailure(h).report(r)
	case err != nil:
		return r.FailErr(err)
	}
	res := restartResult{Port: port, RelaunchedBy: "chottag"}
	if st.Running {
		rec := st.Record
		res.OldPID = rec.PID
		if rec.Supervisor != "" && !*force {
			cmd := supervisorCommand(rec, "restart", os.Getuid())
			fmt.Fprintf(r.Stderr(), "chottag: the daemon (pid %d) runs under %s; restart it through %s instead:\n  %s\nor pass --force to restart it here anyway.\n",
				rec.PID, rec.Supervisor, rec.Supervisor, cmd)
			return r.FailNoText(exit.UserAction, codeSupervised,
				fmt.Sprintf("the daemon (pid %d) runs under %s; restart it through %s instead, or pass --force", rec.PID, rec.Supervisor, rec.Supervisor),
				map[string]any{"command": cmd, "supervisor": rec.Supervisor, "pid": rec.PID})
		}
		// A registry chottag cannot read is informational here, not fatal:
		// restart's whole point is to replace the daemon regardless, and it
		// has nothing safer to fall back to than proceeding without the
		// session count. Unlike `stop`, nothing here asks the operator for
		// permission on the strength of it (--force isn't even in scope),
		// so a warning and continuing is the honest answer, not a refusal.
		// The live-sessions line is on stdout in text mode (TextWarn).
		if live, err := liveSessions(h); err != nil {
			r.Warn(warnSessionRegistryUnreadable, fmt.Sprintf("chottag: could not read the session registry: %v", err))
		} else if len(live) > 0 {
			res.LiveSessions = len(live)
			r.TextWarn(warnLiveSessions, fmt.Sprintf("chottag: %d live claude session(s) will see a brief interruption", len(live)))
		}
		// Probed before the stop, while the old daemon can still answer, but
		// only ever reported once terminate below actually succeeds: a
		// failed stop must not claim an upstream change that never happens.
		// Never computed at all for a supervised daemon: the supervisor
		// relaunches it with the unit's own environment, not this shell's,
		// so this shell's HTTPS_PROXY has nothing to do with what the
		// relaunched daemon will actually get, and the note would be false.
		var change *upstreamChange
		if rec.Supervisor == "" {
			change = upstreamChangeFor(port)
		}
		if _, fail := terminate(h, rec, r); fail != nil {
			return fail.asStopFailed().report(r)
		}
		if change != nil {
			res.UpstreamChanged = change
			r.Warn(warnUpstreamChanged, change.note())
		}
		if rec.Supervisor != "" {
			// Reaching here at all means --force overrode the supervisor
			// check above. Spawning now, the way the unsupervised path
			// below does, would itself take the lock: the supervisor's own
			// relaunch would then find "already running" and give up,
			// leaving the daemon this command started running unsupervised
			// (F140). So this waits for the supervisor's own relaunch
			// instead of starting one.
			r.Warn(warnSupervisorRelaunch, strings.TrimSuffix(supervisorRelaunchNote(rec, "restart", os.Getuid()), "\n"))
			pid, fail := waitForSupervisorRelaunch(h, rec, port, r)
			if fail != nil {
				return fail.report(r)
			}
			res.PID, res.RelaunchedBy = pid, rec.Supervisor
			return r.OK(res)
		}
		waitForPortToClose(h, port, restartPortWait)
	}
	started, fail := ensureStarted(h, port, r)
	if fail != nil {
		return fail.report(r)
	}
	res.PID = started.PID
	return r.OK(res)
}

// waitForSupervisorRelaunch is what a forced restart of a supervised
// daemon does instead of spawning (F140): it polls until a holder whose
// record differs from stopped's — daemonlock.Record.Same, comparing pid
// AND Started, not pid alone: a bare pid compare would wrongly treat the
// OS reusing stopped's own pid number for the relaunch as "still the old
// daemon" and wait out the whole budget — takes home's lock, and its
// health probe on port answers, or until restartSupervisorWait passes.
// "Its health probe answers" deliberately does not mean "that new holder's
// own process answers": a chottag daemon that answers on port without
// holding home's lock at all — a foreign daemon, or a leftover listener
// (F138) — could equally be what health sees the instant after some other
// process takes the lock but before its own listener has bound. That is
// accepted here rather than guarded against further, because spec §4.8
// already rules out trusting the health document's own pid for anything,
// so there is no stronger identity check available to make instead.
func waitForSupervisorRelaunch(h string, stopped daemonlock.Record, port int, r *reporter) (int, *daemonFailure) {
	deadline := time.Now().Add(restartSupervisorWait)
	for {
		if st, err := daemonlock.Inspect(h); err == nil && st.Running && !st.Record.Same(stopped) {
			if ok, _ := shim.ProbeHealth(port); ok {
				r.Text("chottag: the supervisor relaunched the daemon (pid %d)\n", st.Record.PID)
				return st.Record.PID, nil
			}
		}
		if !time.Now().Before(deadline) {
			cmd := supervisorCommand(stopped, "restart", os.Getuid())
			return 0, &daemonFailure{
				exit:    exit.Error,
				code:    codeSupervisorNoRelaunch,
				text:    fmt.Sprintf("chottag: %s did not relaunch the daemon within %s; relaunch it yourself:\n  %s\n", stopped.Supervisor, restartSupervisorWait, cmd),
				message: fmt.Sprintf("%s did not relaunch the daemon within %s; relaunch it yourself", stopped.Supervisor, restartSupervisorWait),
				details: map[string]any{"command": cmd},
			}
		}
		time.Sleep(stopPoll)
	}
}

// upstreamChangeFor reports when the restarted daemon's upstream will
// differ from the running one's, or nil when it will not (or the probe
// itself failed). restart takes the upstream from this shell, as the shim
// does. Run inside a chottag session, HTTPS_PROXY is chottag's own address,
// which SpawnUpstream drops, so a corporate upstream would silently
// disappear.
//
// The old side is already userinfo-free (spec §4.8), so redact.UpstreamProxy
// is safe on it: there is no credential left for it to mask. The new side is
// rendered with redact.WithoutUserinfo instead, which drops any userinfo
// outright rather than masking it: UpstreamProxy masks whatever userinfo a
// URL has, username and password alike (F139), so it would be safe here
// too, but WithoutUserinfo's harder guarantee — nothing left at all, not
// even a masked placeholder — is what this note prefers when it can.
// UpstreamProxy on the raw value is used only as a fallback once
// WithoutUserinfo itself reduces to "" (next is empty, or unparsable in the
// shape redact's own doc comment describes), so the note still says
// "(none)" or "<redacted>" rather than nothing. The JSON document carries
// exactly these two rendered strings, never the raw value.
func upstreamChangeFor(port int) *upstreamChange {
	ok, health := shim.ProbeHealth(port)
	if !ok {
		return nil
	}
	next := shim.SpawnUpstream(os.Environ(), port)
	reduced := redact.WithoutUserinfo(next)
	if reduced == health.Upstream && !(next != "" && reduced == "") {
		return nil
	}
	newSide := reduced
	if newSide == "" {
		newSide = redact.UpstreamProxy(next)
	}
	return &upstreamChange{From: redact.UpstreamProxy(health.Upstream), To: newSide}
}

// waitForPortToClose returns once nothing answers the health probe on port,
// once a new daemon holds home's lock, or once budget has passed. The old
// daemon's listener dies with its process, but the kernel closes a dying
// process's descriptors in no promised order, so the lock can read as free
// a moment before the socket has gone. If the port still answers after
// budget, ensureStarted's F138 check refuses rather than spawning a daemon
// that cannot bind.
func waitForPortToClose(h string, port int, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ok, _ := shim.ProbeHealth(port); !ok {
			return
		}
		if st, err := daemonlock.Inspect(h); err == nil && st.Running {
			return // a `claude` launch already started the next daemon
		}
		time.Sleep(stopPoll)
	}
}
