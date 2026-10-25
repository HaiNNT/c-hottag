package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

// homeLoginNoticeEvery is the least time between two notices that Home's login
// was refused (R183).
const homeLoginNoticeEvery = time.Hour

// homeLoginWatch follows Claude Code's own (Home) login as the proxy sees it
// (proxy.Config.OnHomeLogin, R183): Claude Code gates every request on that
// login, so when it is refused 401 every session says "Login expired" while
// chottag's accounts are fine. A refusal opens an episode (its time is the
// first refusal's); a 2xx on the same login closes it. The status document
// carries the open episode's start, `chottag doctor` reads it, and the
// desktop notice comes once per episode, only after the episode has lasted
// status.HomeLoginSettle, and at most once an hour. It carries no token and no
// body: the proxy passes only a bool.
type homeLoginWatch struct {
	log io.Writer
	dn  *daemonNotify // nil: no notice
	now func() time.Time

	mu         sync.Mutex
	since      time.Time // the open episode's start; zero when none
	noticed    bool      // this episode already sent its notice
	lastNotice time.Time
}

// newHomeLoginWatch builds the watch; now is the clock. dn may be nil.
func newHomeLoginWatch(log io.Writer, dn *daemonNotify, now func() time.Time) *homeLoginWatch {
	return &homeLoginWatch{log: log, dn: dn, now: now}
}

// report is proxy.Config.OnHomeLogin: ok is a 2xx on the client's own login,
// !ok a 401. It runs on request goroutines and does not block.
func (w *homeLoginWatch) report(ok bool) {
	t := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if ok {
		if w.since.IsZero() {
			return
		}
		fmt.Fprintf(w.log, "chottag: Claude Code's own login (Home) works again (refused since %s)\n", w.since.Format("15:04:05"))
		w.since, w.noticed = time.Time{}, false
		return
	}
	if w.since.IsZero() {
		w.since = t
		fmt.Fprintf(w.log, "chottag: Claude Code's own login (Home) was refused (401); sessions will say \"Login expired\"; run /login in a Claude Code session\n")
		return
	}
	if w.noticed || t.Sub(w.since) < status.HomeLoginSettle {
		return
	}
	if !w.lastNotice.IsZero() && t.Sub(w.lastNotice) < homeLoginNoticeEvery {
		return
	}
	w.noticed, w.lastNotice = true, t
	if w.dn != nil {
		w.dn.events.HomeLoginRefused()
	}
}

// refusedSince is the open episode's start, or the zero time when none. It is
// safe on a nil watch.
func (w *homeLoginWatch) refusedSince() time.Time {
	if w == nil {
		return time.Time{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.since
}
