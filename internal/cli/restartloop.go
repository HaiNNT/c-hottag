package cli

// The daemon's restart-when-idle loop (R126, spec §5b). A Mac that only
// sleeps never restarts its daemon, and `update` defers its restart while
// sessions are live, so a newer chottag can sit installed but not running
// for weeks. Every minute, and on every wake from sleep, this looks at the
// installed version; when it is newer than the running one, and the proxy is
// idle, it starts a detached `chottag daemon restart --force --json` of the
// installed binary: the same restart a person runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

const (
	// restartLook is how often the loop looks, besides a wake.
	restartLook = time.Minute
	// restartQuiet is how long no request may have started before the
	// proxy counts as idle.
	restartQuiet = 5 * time.Minute
	// restartRetry separates two restart attempts for one installed version.
	restartRetry = time.Hour
	// restartReportWait is how long after an attempt its child is given to
	// finish before its result is read.
	restartReportWait = 2 * time.Minute
	// restartLogLimit caps how much of the child's output is read.
	restartLogLimit = 64 << 10
)

// restartSpawn starts bin with args as a detached child (its own session,
// stdin on /dev/null, stdout and stderr in the file out, truncated, 0600) and
// does not wait for it: the restart it performs ends this very daemon.
// TestMain installs a panicking default.
var restartSpawn = spawnDetached

func spawnDetached(out, bin string, args ...string) error {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// bin is <home>/bin/chottag, built by the loop from the daemon's own
	// home, and args are constants: nothing here comes from the network.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, f, f // nil stdin: os/exec opens /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap it if the daemon outlives it (a restart that failed).
	go func() { _ = cmd.Wait() }()
	return nil
}

// installedViaLink reports whether <home>/bin/chottag links into
// <home>/versions/<version>: the binary a restart would run is that version.
// A version known only from install.json is not enough to act on. TestMain
// installs a panicking default.
var installedViaLink = func(h, version string) bool {
	v, ok := linkedVersionOf(h)
	return ok && v == version
}

// newRestartTicker starts the minute ticker the loop falls back to when
// restartLoop.ticks is nil. A var so a test can inject its own; TestMain
// installs a panicking default.
var newRestartTicker = func(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// restartLoop is one daemon generation's restart watcher. A nil
// *restartLoop is valid and does nothing.
type restartLoop struct {
	home    string
	version string // the running version
	state   func() (store.State, error)
	sink    *statusSink
	// idle is the proxy's own idle signal (proxy.Server.Idle).
	idle   func(now time.Time, quiet time.Duration) bool
	notify func(title, body string) // nil posts nothing
	log    io.Writer
	now    func() time.Time
	// ticks, if non-nil, replaces the minute ticker. Tests only.
	ticks <-chan time.Time
	// wake carries a wake-from-sleep event (capacity 1, never blocks the
	// sender).
	wake chan struct{}
	// processed, if non-nil, receives once per look. Tests only.
	processed chan<- struct{}

	// mu guards what follows: the Run goroutine's look and the update loop's
	// NoteNotified both change rs.
	mu         sync.Mutex
	rs         restartState // persisted in <home>/run/restart.json
	loaded     bool
	skipLogged string // installed version already logged as not actionable
}

// restartState is what outlives a daemon generation: without it a restart
// that does not end the mismatch would repeat at every start, and tell the
// user each time.
type restartState struct {
	// Notified is the installed version told "is installed".
	Notified string `json:"notified,omitempty"`
	// Failed is the installed version told "could not restart".
	Failed string `json:"failed,omitempty"`
	// Mismatch is the installed version whose restart ended with a daemon
	// still older than it (the binary is not that version): never retried.
	Mismatch string `json:"mismatch,omitempty"`
	// Attempt is the last restart started.
	Attempt *restartAttempt `json:"attempt,omitempty"`
}

type restartAttempt struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
	// Reported is set once the child's result has been read.
	Reported bool `json:"reported,omitempty"`
}

func (l *restartLoop) statePath() string { return filepath.Join(l.home, "run", "restart.json") }
func (l *restartLoop) logPath() string   { return filepath.Join(l.home, "run", "restart.log") }

// loadState reads restart.json once. A missing or unreadable file is an
// empty state: it is a limiter, never a reason to stop.
func (l *restartLoop) loadState() {
	if l.loaded {
		return
	}
	l.loaded = true
	b, err := os.ReadFile(l.statePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			l.logf("%s unreadable: %v", l.statePath(), err)
		}
		return
	}
	if err := json.Unmarshal(b, &l.rs); err != nil {
		l.logf("%s unreadable: %v", l.statePath(), err)
		l.rs = restartState{}
	}
}

func (l *restartLoop) saveState() {
	b, err := json.Marshal(l.rs)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(l.statePath()), 0o700)
	}
	if err == nil {
		err = fsutil.WriteFileAtomic(l.statePath(), b, 0o600)
	}
	if err != nil {
		l.logf("could not write %s: %v", l.statePath(), err)
	}
}

// restartResult reads the child's result from its log: the first JSON
// document that starts a line (text warnings may precede it). ok false with
// code "no_result" when there is none.
func readRestartResult(path string) (ok bool, code string) {
	f, err := os.Open(path)
	if err != nil {
		return false, "no_result"
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, restartLogLimit))
	if err != nil {
		return false, "no_result"
	}
	for i := 0; i < len(b); {
		if b[i] == '{' && (i == 0 || b[i-1] == '\n') {
			var doc struct {
				OK    bool `json:"ok"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(bytes.TrimSpace(b[i:]), &doc) == nil {
				if doc.OK {
					return true, ""
				}
				if doc.Error.Code == "" {
					return false, "no_result"
				}
				return false, doc.Error.Code
			}
		}
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return false, "no_result"
}

// reportLastAttempt reads the last attempt's result once it has had time to
// finish, and tells the user once per version when the restart did not
// happen. A restart that did happen left a daemon running the attempted
// version, so a running version that is not older than it needs no report.
func (l *restartLoop) reportLastAttempt(now time.Time) {
	a := l.rs.Attempt
	if a == nil || a.Reported || now.Sub(a.At) < restartReportWait {
		return
	}
	a.Reported = true
	if !updatecheck.Newer(a.Version, l.version) {
		l.saveState()
		return
	}
	if ok, code := readRestartResult(l.logPath()); !ok {
		l.logf("the restart onto %s failed: %s", a.Version, code)
		l.failed(a.Version, code, "chottag daemon restart")
	} else {
		// It says ok, yet the daemon that is running is still older: the
		// installed binary is not the version its directory says.
		why := "the installed binary is not " + a.Version
		l.logf("the restart onto %s reported ok but the daemon is still %s: %s", a.Version, l.version, why)
		l.rs.Mismatch = a.Version
		l.failed(a.Version, why, "chottag update --version v"+a.Version) // restarting again cannot help; reinstalling can
	}
	l.saveState()
}

// failed notifies once per installed version that its restart did not
// happen; remedy is the command the notice tells the user to run.
func (l *restartLoop) failed(version, why, remedy string) {
	if l.rs.Failed == version {
		return
	}
	l.rs.Failed = version
	l.post("chottag: restart failed", fmt.Sprintf("chottag could not restart onto %s: %s. Run: %s", version, why, remedy))
}

// newRestartLoop returns the loop, or nil when this daemon never restarts
// itself: a version that is not a release (a dev build). The caller binds
// idle before Run.
func newRestartLoop(h string, state func() (store.State, error), sink *statusSink, dn *daemonNotify, log io.Writer) *restartLoop {
	if !updatecheck.Parses(Version) {
		return nil
	}
	l := &restartLoop{home: h, version: Version, state: state, sink: sink, log: log, now: updateLoopClock, wake: make(chan struct{}, 1)}
	if dn != nil {
		l.notify = dn.post
	}
	return l
}

// NoteNotified records that the user was already told version is installed
// (the update loop's "updated" notice), so this loop does not repeat it.
func (l *restartLoop) NoteNotified(version string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadState()
	l.rs.Notified = version
	l.saveState()
}

// Wake asks the loop to look now, because the machine woke from sleep. It
// never blocks.
func (l *restartLoop) Wake() {
	if l == nil {
		return
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Run looks at start (which also clears a pending mark an earlier daemon
// generation left), then every minute and on every wake, until ctx ends.
func (l *restartLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	ticks := l.ticks
	if ticks == nil {
		var stop func()
		ticks, stop = newRestartTicker(restartLook)
		defer stop()
	}
	for {
		l.look()
		if l.processed != nil {
			select {
			case l.processed <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		case <-l.wake:
		}
	}
}

func (l *restartLoop) logf(format string, a ...any) {
	if l.log != nil {
		fmt.Fprintf(l.log, "chottag: restart: "+format+"\n", a...)
	}
}

func (l *restartLoop) post(title, body string) {
	if l.notify != nil {
		l.notify(title, body)
	}
}

// updateRunning is whether an update is installing: this daemon's own
// auto-install child, or any `chottag update` holding the update lock. The
// lock is only probed and released at once.
func (l *restartLoop) updateRunning() bool {
	if autoInstalling.Load() {
		return true
	}
	if err := os.MkdirAll(filepath.Join(l.home, "run"), 0o700); err != nil {
		l.logf("cannot check for a running update, not restarting: %v", err)
		return true
	}
	unlock, ok, err := fsutil.TryLock(filepath.Join(l.home, "run", updateLockName))
	if err != nil {
		l.logf("cannot check for a running update, not restarting: %v", err)
		return true
	}
	if !ok {
		return true
	}
	if err := unlock(); err != nil {
		l.logf("could not release the update lock probe: %v", err)
	}
	return false
}

// look is one pass: stamp what is pending, and restart when it is allowed.
func (l *restartLoop) look() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadState()
	now := l.now()
	l.reportLastAttempt(now)
	pending := ""
	if updatecheck.Parses(l.version) {
		if inst, err := installedVersion(l.home); err == nil && updatecheck.Newer(inst, l.version) {
			pending = inst
		}
	}
	l.sink.setRestartPending(pending)
	if pending == "" {
		return
	}
	st, err := l.state()
	if err != nil {
		l.logf("state.json unreadable, not restarting: %v", err)
		return
	}
	if !st.RestartOn() {
		return
	}
	if !installedViaLink(l.home, pending) {
		// The restart runs bin/chottag; if that is not the pending version
		// (the version came from install.json), restarting would not end
		// the mismatch.
		if l.skipLogged != pending {
			l.skipLogged = pending
			l.logf("%s is recorded as installed but bin/chottag does not link into versions/%s: not restarting", pending, pending)
		}
		return
	}
	if l.updateRunning() {
		// An install is relinking bin/chottag right now: restarting would
		// kill it half done, and the notice is the update's to give.
		return
	}
	if l.rs.Notified != pending {
		l.rs.Notified = pending
		l.saveState()
		l.post("chottag: update installed", fmt.Sprintf("chottag %s is installed. The daemon switches to it when Claude Code is idle.", pending))
	}
	if l.rs.Mismatch == pending {
		return
	}
	if !l.idle(now, restartQuiet) {
		return
	}
	if a := l.rs.Attempt; a != nil && a.Version == pending && now.Sub(a.At) < restartRetry {
		return
	}
	l.rs.Attempt = &restartAttempt{Version: pending, At: now}
	l.saveState()
	bin := filepath.Join(l.home, "bin", "chottag")
	if err := restartSpawn(l.logPath(), bin, "daemon", "restart", "--force", "--json"); err != nil {
		l.logf("could not start the restart onto %s: %v", pending, err)
		l.rs.Attempt.Reported = true
		l.failed(pending, err.Error(), "chottag daemon restart")
		l.saveState()
		return
	}
	l.logf("idle: restarting onto %s", pending)
}
