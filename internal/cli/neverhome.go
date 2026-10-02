package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// eventLogEvery is how often one account's stale-token line is logged.
const eventLogEvery = time.Minute

// remoteRefusal is the 503 message for a remote or owner request whose
// account has no usable login (R147): it names the account and the fix.
func remoteRefusal(account, role string) string {
	return fmt.Sprintf("chottag: account %s (%s) has no usable login right now; run: chottag login %s", account, role, account)
}

// tokenWords says what is wrong with an account's token, for a log line.
func tokenWords(account string, st creds.Status) string {
	if st.State == creds.StateNeedsLogin {
		return account + " needs login"
	}
	return account + "'s token is stale"
}

// newEventPrinter is the selector event printer for the daemon log. A
// passthrough on an unusable token has its own line (a serving request sent
// on Home's own login; a remote or owner request refused), logged at most
// once per account per minute per kind of line. Any other event keeps the
// plain line, throttled to one per 10 seconds as before.
func newEventPrinter(w io.Writer, now func() time.Time) func(selector.Event) {
	plain := throttle(10*time.Second, func(e selector.Event) {
		fmt.Fprintf(w, "chottag: %s %s %s %s\n", e.Kind, e.Account, e.Status.State, e.Detail)
	})
	var mu sync.Mutex
	last := map[string]time.Time{}
	return func(e selector.Event) {
		if e.Kind != "passthrough" || e.Role == "" {
			plain(e)
			return
		}
		key := strings.ToLower(e.Account) + "|" + fmt.Sprint(e.Refused)
		t := now()
		mu.Lock()
		prev, seen := last[key]
		due := !seen || t.Sub(prev) >= eventLogEvery
		if due {
			last[key] = t
		}
		mu.Unlock()
		if !due {
			return
		}
		if e.Refused && e.Role == selector.RoleServing {
			// Several pools: nothing went out on Home's login; another
			// member of the pool serves it, or it is refused.
			fmt.Fprintf(w, "chottag: %s (serving): not sent on Home's own login; another pool member serves it, or it is refused\n", tokenWords(e.Account, e.Status))
			return
		}
		if e.Refused {
			fmt.Fprintf(w, "chottag: refused a remote request: %s; run `chottag login %s` if it persists\n", tokenWords(e.Account, e.Status), e.Account)
			return
		}
		fmt.Fprintf(w, "chottag: sent on Home's own login: %s (%s)\n", tokenWords(e.Account, e.Status), e.Role)
	}
}

// warmWindow is how close to expiry a remote account's token is refreshed:
// Claude Code itself renews only inside creds.ExpiringWithin of expiry, so a
// minute more than that is as early as a refresh can do anything.
const warmWindow = creds.ExpiringWithin + time.Minute

// warmStopBound bounds the wait for a warm pass at shutdown.
const warmStopBound = 3 * time.Second

// warmEvery is the shortest wait between two passes of the warm loop.
const warmEvery = time.Minute

// remoteWarmer keeps every pool's remote account's token fresh (R147): a
// remote account is usually rotation-off and serves no prompts, so nothing
// else would. It records nothing in state; a needs-login account is skipped
// and logged once. A nil *remoteWarmer does nothing.
type remoteWarmer struct {
	state func() (store.State, error)
	// warm is tokens.Manager.Warm: one refresh at a time, backoff respected.
	warm func(ctx context.Context, dir string, within time.Duration) (creds.Status, bool)
	log  io.Writer
	now  func() time.Time

	mu      sync.Mutex
	last    time.Time
	running bool
	closed  bool            // stop was called: kick starts nothing more
	logged  map[string]bool // needs-login already logged for the account
	wg      sync.WaitGroup
}

// kick starts a pass in its own goroutine, unless one is running or the last
// one started under a minute ago. Called from the roster tick, so it never
// blocks.
func (w *remoteWarmer) kick(ctx context.Context) {
	if w == nil || ctx.Err() != nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	if w.closed || w.running || (!w.last.IsZero() && now.Sub(w.last) < warmEvery) {
		w.mu.Unlock()
		return
	}
	w.running, w.last = true, now
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
		}()
		w.pass(ctx)
	}()
}

// wait joins any pass in flight.
func (w *remoteWarmer) wait() {
	if w != nil {
		w.wg.Wait()
	}
}

// stop joins any pass in flight, for at most bound; it reports whether the
// pass had finished. The caller cancels the context kick was given first, so
// a refresh in flight is stopped rather than orphaned.
func (w *remoteWarmer) stop(bound time.Duration) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(bound):
		return false
	}
}

// pass warms each distinct remote account of every pool once.
func (w *remoteWarmer) pass(ctx context.Context) {
	st, err := w.state()
	if err != nil {
		return
	}
	done := map[string]bool{}
	for _, pool := range st.PoolNames() {
		name := st.PoolOf(pool).Remote
		key := strings.ToLower(name)
		if name == "" || done[key] {
			continue
		}
		done[key] = true
		var acct *store.Account
		for i := range st.Accounts {
			if strings.EqualFold(st.Accounts[i].Name, name) {
				acct = &st.Accounts[i]
				break
			}
		}
		if acct == nil || ctx.Err() != nil {
			continue
		}
		status, _ := w.warm(ctx, acct.Dir, warmWindow)
		w.note(acct.Name, status)
	}
}

// note logs a needs-login account once, until it recovers.
func (w *remoteWarmer) note(account string, st creds.Status) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st.State != creds.StateNeedsLogin {
		delete(w.logged, account)
		return
	}
	if w.logged[account] {
		return
	}
	if w.logged == nil {
		w.logged = map[string]bool{}
	}
	w.logged[account] = true
	if w.log != nil {
		fmt.Fprintf(w.log, "chottag: remote account %s needs login, so it is not kept warm (run: chottag login %s)\n", account, account)
	}
}
