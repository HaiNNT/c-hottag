package cli

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/notify"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// newDaemonNotifier is the daemon's notifier: osascript on darwin, a no-op
// elsewhere (M2 spec §4). It is a variable so a test can hand the daemon a
// recorder (stubDaemonNotifier). TestMain leaves it at this value and arms
// notify.SetRunForTest with a panicking runner instead, so a daemon test
// that posts a real notice without the stub fails loudly.
var newDaemonNotifier = func() notify.Notifier { return notify.New(runtime.GOOS) }

// daemonNotify is one daemon generation's notifications (D11): the state
// machine that decides when a notice fires, and the dispatcher that posts
// it off the request path. A nil *daemonNotify is valid and does nothing,
// so every daemonDeps literal that predates M2b keeps working.
//
// It also guards against a narrow race the selector's needs-login hook and
// the usage hook run into independently (T1 review #5, fixed in review
// round 1): a request that was already in flight, swapped onto an account,
// when that SAME account's needs-login notice fires for a different, later
// request can still finish with a 2xx afterwards. Naively treating that as
// "seen ok" would re-arm a notice that is still accurate and let the very
// next request post a spurious second one.
//
// The guard is a per-account blocked flag, not a count: markStale sets
// blocked[account] on a needs-login notice, and beginChoose — called from
// chooser.Choose on a successful choice — clears it. The usage hook's 2xx
// re-arms the notice only when the account isn't blocked. A successful
// Choose is the stand-in for "the login works again": while an account
// needs login, the selector routes every request for it through
// passthrough and never chooses it (see notifyEventHook and
// selector.Config's routing), so the only way beginChoose ever runs for a
// blocked account is a fresh choice made after it has recovered. The one
// inexact case is an old request's 2xx arriving after that fresh Choose:
// harmless, since the account is healthy again by then. Unlike an in-flight
// count, a boolean flag can't leak: a chosen request that never reaches the
// usage hook at all (a transport error, ErrorHandler, a client cancel
// before headers, a panic, or the safety net's own drift fallback) simply
// leaves nothing to clean up.
type daemonNotify struct {
	events *notify.Events
	disp   *notify.Dispatcher
	// state is how the notify switch and the label are read (post).
	state func() (store.State, error)

	// blockedCount lets beginChoose and isBlocked skip the mutex and the
	// map on the common path where nothing is blocked.
	blockedCount atomic.Int32
	mu           sync.Mutex
	blocked      map[string]bool // lowercased account: its needs-login notice is still accurate
}

// newDaemonNotify starts the dispatcher. state is how the switch is read,
// once per notice that would fire.
func newDaemonNotify(state func() (store.State, error), n notify.Notifier) *daemonNotify {
	disp := notify.NewDispatcher(n, notify.SendTimeout)
	return &daemonNotify{
		disp:  disp,
		state: state,
		events: notify.NewEvents(notify.Config{
			Emit: func(title, body string) {
				label := ""
				if st, err := state(); err == nil {
					label = st.Label
				}
				disp.Enqueue(labelTitle(title, label), body)
			},
			Enabled: func() bool { return notifyEnabled(state) },
		}),
		blocked: map[string]bool{},
	}
}

// post sends one notice that is not part of the notify.Events state machine
// (the update loop keeps its own once-per-version record), honouring the
// notify switch like every other notice and labelling the title (R118). A
// nil dn does nothing.
func (dn *daemonNotify) post(title, body string) {
	if dn == nil || !notifyEnabled(dn.state) {
		return
	}
	label := ""
	if st, err := dn.state(); err == nil {
		label = st.Label
	}
	dn.disp.Enqueue(labelTitle(title, label), body)
}

// labelTitle puts the install's label into a notice title (R118): a title
// starting "chottag: " becomes "chottag · <label>: ". No label, or a title
// that does not start that way, comes back unchanged.
func labelTitle(title, label string) string {
	const prefix = "chottag: "
	if label == "" || !strings.HasPrefix(title, prefix) {
		return title
	}
	return "chottag · " + label + ": " + title[len(prefix):]
}

// notifyEnabled reads state.json's switch (D12). The daemon passes
// store.Cache.State, which re-reads the file only when it changed, so
// `chottag notify off` applies to the next notice without a restart. An
// unreadable state.json reads as on, the default: a notice about the very
// breakage shouldn't be silenced by it.
func notifyEnabled(state func() (store.State, error)) bool {
	st, err := state()
	if err != nil {
		return true
	}
	return st.NotifyOn()
}

// Close stops the dispatcher. runProxyWithSignal calls it once runDaemon
// has returned, when nothing feeds it any more.
func (dn *daemonNotify) Close() {
	if dn != nil {
		dn.disp.Close()
	}
}

// Errors is daemon.notifyErrors: notices not delivered this generation.
func (dn *daemonNotify) Errors() uint64 {
	if dn == nil {
		return 0
	}
	return dn.disp.Errors()
}

// tick runs on the roster tick, inside runDaemon's stamp. It reports route
// drift, then re-seeds the sink's rows from state.json and reports the
// limit roll-up at now. The re-seed is the same one newUsageHook does per
// response: without it, an account added since the last response would
// be missing from "all limited".
//
// The flap guard inside notify.Events runs on its own clock (Config.Now,
// wall time by default), while the roll-up itself is evaluated at now, the
// tick's own clock: the two agree in production (both wall time), but a
// test driving now artificially can make them disagree (PF4).
func (dn *daemonNotify) tick(state func() (store.State, error), sink *statusSink, routeDrift uint64, now time.Time) {
	if dn == nil {
		return
	}
	dn.events.RouteDrift(routeDrift)
	seedRosterAtStartup(state, sink)
	dn.events.Limits(sink.limitsAt(now))
}

// beginChoose reports that account was just chosen for a new request:
// proxy.go's chooser.Choose calls this right after a successful Choose. It
// clears blocked[account] — the account was choosable, so it is trusted
// again. account is normalised on the slow path only: the fast path (no
// account is blocked) never allocates or takes the lock. A nil dn is a
// no-op.
func (dn *daemonNotify) beginChoose(account string) {
	if dn == nil || account == "" || dn.blockedCount.Load() == 0 {
		return
	}
	key := strings.ToLower(account)
	dn.mu.Lock()
	if dn.blocked[key] {
		delete(dn.blocked, key)
		dn.blockedCount.Add(-1)
	}
	dn.mu.Unlock()
}

// markStale sets blocked[account]: notifyEventHook calls this every time it
// forwards a needs-login passthrough for account, whether or not
// notify.Events actually emits — the account genuinely needs login as of
// right now either way, and the usage hook's next 2xx for it must not
// re-arm the notice until a fresh Choose clears the flag again.
func (dn *daemonNotify) markStale(account string) {
	if dn == nil || account == "" {
		return
	}
	key := strings.ToLower(account)
	dn.mu.Lock()
	if !dn.blocked[key] {
		dn.blocked[key] = true
		dn.blockedCount.Add(1)
	}
	dn.mu.Unlock()
}

// isBlocked reports whether account's needs-login notice is still presumed
// accurate: the usage hook must not treat a 2xx for it as "seen ok". The
// fast path (nothing blocked) never takes the lock.
func (dn *daemonNotify) isBlocked(account string) bool {
	if dn == nil || account == "" || dn.blockedCount.Load() == 0 {
		return false
	}
	key := strings.ToLower(account)
	dn.mu.Lock()
	blocked := dn.blocked[key]
	dn.mu.Unlock()
	return blocked
}

// notifyEventHook wraps the selector's event printer. newPassthroughHook
// hands every event to its printer, so this sees each one. A passthrough
// whose token state is needs-login fires that account's notice, and blocks
// the account's usage hook from re-arming it until a fresh Choose clears
// that (ruling from T1 review #5). It runs on a request goroutine, and
// Events plus Dispatcher.Enqueue never block.
func notifyEventHook(dn *daemonNotify, next func(selector.Event)) func(selector.Event) {
	if dn == nil {
		return next
	}
	return func(e selector.Event) {
		next(e)
		if e.Kind == "passthrough" && e.Status.State == creds.StateNeedsLogin {
			dn.markStale(e.Account)
			dn.events.NeedsLogin(e.Account)
		}
	}
}

// notifyUsageHook wraps proxy.Config.OnUsage, which fires only for a
// swapped response answered on the account's own credential
// (internal/proxy's OnUsage doc). A 2xx is that account's "seen ok"; it
// re-arms the needs-login notice only when the account isn't blocked —
// i.e. no needs-login notice has fired for it since the last successful
// Choose (ruling 4, and T1 review #5's blocked-flag fix).
func notifyUsageHook(dn *daemonNotify, next func(string, int, http.Header)) func(string, int, http.Header) {
	if dn == nil {
		return next
	}
	return func(account string, code int, h http.Header) {
		next(account, code, h)
		if code >= 200 && code < 300 && !dn.isBlocked(account) {
			dn.events.AccountOK(account)
		}
	}
}

// limitsAt is the roll-up at now, with RollUpAt's rule: a reset that has
// passed counts as available, with or without traffic. It is computed on
// copies (status.File.LimitsAt and AvailableAt take the File by value), so
// the durable Limits the write path records are untouched.
func (c *statusSink) limitsAt(now time.Time) notify.LimitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.file.LimitsAt(now)
	return notify.LimitState{
		AllLimited:       l.AllLimited,
		NextReset:        l.NextReset,
		NextResetAccount: l.NextResetAccount,
		Available:        c.file.AvailableAt(now),
	}
}
