package cli

// The daemon leaves a dead login session (R164, F272). A daemon started with
// Setsid outlives the macOS GUI login session it was started in (a logout or
// a WindowServer crash) and keeps that session's bootstrap namespace, so
// every `security` child it runs fails and every slot reads as unreachable.
// creds reports that as ErrSessionGone (stale, never needs-login); this
// watcher ends the daemon once the condition lasts, so the next claude, which
// can only run in the live session, starts a fresh one.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/daemonlock"
)

const (
	// sessionLook is how often the watcher looks.
	sessionLook = 30 * time.Second
	// sessionGoneFor is how long a dead-session signal must have stood
	// before the daemon acts on it.
	sessionGoneFor = time.Minute
)

// newSessionTicker starts the watcher's ticker. A var so a test can inject
// its own; TestMain installs one that never fires.
var newSessionTicker = func(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// sessionProbe asks, with a fresh exec, whether this process can still see the
// user's keychains. TestMain installs a panicking default.
var sessionProbe = func() bool { return creds.KeychainReachable(creds.ExecRunner) }

// sessionWatcher ends the daemon when its login session is gone. A nil
// *sessionWatcher does nothing.
type sessionWatcher struct {
	log io.Writer
	// since is creds.SessionGoneSince; now is the clock.
	since func() time.Time
	now   func() time.Time
	// probe is a fresh reachability check.
	probe func() bool
	// correlated is creds.SessionGoneCorrelated: slots that read OK earlier
	// are missing together, which counts without a failing probe. clear is
	// creds.ClearSessionGone.
	correlated func() bool
	clear      func()
	// supervisor reads the supervisor the daemon lock record names; an error
	// means "could not tell", and the look is skipped.
	supervisor func() (string, error)
	// stop begins the daemon's graceful shutdown.
	stop func()
	// ticks, if non-nil, replaces the ticker. Tests only.
	ticks <-chan time.Time
	// processed, if non-nil, receives once per look. Tests only.
	processed chan<- struct{}

	loggedSupervised bool
}

// newSessionWatcher wires the production watcher for the daemon at home. It
// is nil off darwin, where there is no per-session keychain to lose.
func newSessionWatcher(home string, log io.Writer) *sessionWatcher {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return &sessionWatcher{
		log:   log,
		since: creds.SessionGoneSince,
		now:   timeNow,
		probe: func() bool { return sessionProbe() },

		correlated: creds.SessionGoneCorrelated,
		clear:      creds.ClearSessionGone,
		supervisor: func() (string, error) {
			st, err := daemonlock.Inspect(home)
			if err != nil {
				return "", err
			}
			return st.Record.Supervisor, nil
		},
	}
}

// Run looks every sessionLook until ctx ends or the daemon has been told to
// stop.
func (w *sessionWatcher) Run(ctx context.Context) {
	if w == nil {
		return
	}
	ticks := w.ticks
	if ticks == nil {
		var stop func()
		ticks, stop = newSessionTicker(sessionLook)
		defer stop()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		fired := w.look()
		if w.processed != nil {
			select {
			case w.processed <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		if fired {
			return
		}
	}
}

// look reports whether it told the daemon to stop. It acts only when the
// signal has stood for sessionGoneFor and a fresh probe still fails, so a
// transient failure (a wake, a locked keychain) never ends the daemon.
func (w *sessionWatcher) look() bool {
	since := w.since()
	if since.IsZero() || w.now().Sub(since) < sessionGoneFor {
		return false
	}
	if !w.correlated() && w.probe() {
		// Reachable and nothing correlates: the failure was transient.
		w.clear()
		return false
	}
	sup, err := w.supervisor()
	if err != nil {
		return false
	}
	if sup != "" {
		if !w.loggedSupervised {
			w.loggedSupervised = true
			fmt.Fprintf(w.log, "chottag: the login session this daemon started in has ended (the keychain is unreachable); not exiting, because %s supervises it\n", sup)
		}
		return false
	}
	fmt.Fprintln(w.log, "chottag: the login session this daemon started in has ended (the keychain is unreachable); exiting so the next claude starts a fresh daemon")
	w.stop()
	return true
}

// evidenceRead wraps a slot read so that the first time a slot reads as no
// login or denied, the daemon log carries the underlying error once (R164):
// its "exit status N" tells a lost login (44) from a dead session. The error
// text is safe: Reader.Read never puts store contents or stderr in it.
func evidenceRead(read func(string) (creds.Token, error), log io.Writer) func(string) (creds.Token, error) {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(dir string) (creds.Token, error) {
		tok, err := read(dir)
		if err != nil && (errors.Is(err, creds.ErrNoLogin) || errors.Is(err, creds.ErrKeychain)) {
			mu.Lock()
			first := !seen[dir]
			seen[dir] = true
			mu.Unlock()
			if first {
				fmt.Fprintf(log, "chottag: slot %s reads as needing a login: %v\n", filepath.Base(dir), err)
			}
		}
		return tok, err
	}
}
