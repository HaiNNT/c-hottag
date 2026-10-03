package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// refreshLogEvery is the least time between two identical refresh lines for
// one account: a slot whose refresh keeps failing the same way is told once
// every ten minutes, not on every backoff. A different outcome always logs.
const refreshLogEvery = 10 * time.Minute

// refreshReporter is tokens.Config.OnRefresh for the daemon (R149): it logs
// every refresh outcome to daemon.log, clears a stale token state on a
// renewal, and takes an account that never renews to needs-login with a
// notice. Its lines carry the account, the outcome, the duration and the
// expiry only; a token never reaches it.
type refreshReporter struct {
	log  io.Writer
	now  func() time.Time
	sink *statusSink
	dn   *daemonNotify

	mu   sync.Mutex
	seen map[string]refreshSeen
}

type refreshSeen struct {
	line string
	at   time.Time
}

func (r *refreshReporter) onRefresh(ev tokens.RefreshEvent) {
	if r.sink != nil && ev.Outcome == tokens.OutcomeRenewed {
		r.sink.setTokenCleared(ev.Account, r.now())
	}
	if ev.NeedsLogin {
		if r.sink != nil {
			r.sink.setTokenState(ev.Account, creds.StateNeedsLogin)
		}
		if r.dn != nil {
			r.dn.markStale(ev.Account)
			r.dn.events.NeedsLogin(ev.Account)
		}
	}
	if r.log == nil {
		return
	}
	line, key := refreshLine(ev)
	now := r.now()
	acct := strings.ToLower(ev.Account)
	r.mu.Lock()
	prev, ok := r.seen[acct]
	due := !ok || prev.line != key || now.Sub(prev.at) >= refreshLogEvery || ev.NeedsLogin
	if due {
		if r.seen == nil {
			r.seen = map[string]refreshSeen{}
		}
		r.seen[acct] = refreshSeen{line: key, at: now}
	}
	r.mu.Unlock()
	if !due {
		return
	}
	fmt.Fprintln(r.log, line)
	if ev.NeedsLogin {
		fmt.Fprintf(r.log, "chottag: refresh %s: still expired after %d tries over %dm; it needs a login (run: chottag login %s)\n", ev.Account, ev.Tries, int(ev.Span.Round(time.Minute)/time.Minute), ev.Account)
	}
}

// refreshLine words one event. The second result is the line's identity for
// the repeat limit: the line without the numbers that change every time.
func refreshLine(ev tokens.RefreshEvent) (line, key string) {
	// tokens already keeps a secret out of Detail; a line is the last place
	// one could leak, so it is checked here as well.
	if strings.Contains(ev.Detail, "sk-ant-") || strings.Contains(ev.Detail, "Bearer") {
		ev.Detail = "redacted"
	}
	switch ev.Outcome {
	case tokens.OutcomeRenewed:
		return fmt.Sprintf("chottag: refresh %s: renewed, expires in %s (%s, %s)", ev.Account, short(ev.ExpiresIn), ev.Trigger, took(ev.Took)),
			"renewed"
	case tokens.OutcomeNotRenewed:
		l := fmt.Sprintf("chottag: refresh %s: %s", ev.Account, ev.Detail)
		if ev.RetryIn > 0 {
			l += "; retry in " + short(ev.RetryIn)
		} else {
			l += fmt.Sprintf(" (%s, %s)", ev.Trigger, took(ev.Took))
		}
		return l, "not-renewed: " + ev.Detail
	case tokens.OutcomePanicked:
		return fmt.Sprintf("chottag: refresh %s: panicked: %s", ev.Account, ev.Detail), "panicked"
	}
	return fmt.Sprintf("chottag: refresh %s: failed: %s", ev.Account, ev.Detail), "failed: " + ev.Detail
}

// short renders d without noise: "7h59m", "2m", "30s".
func short(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func took(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// accountNameByDir names the account whose slot is dir, from the roster;
// the directory's own name when none matches.
func accountNameByDir(state func() (store.State, error)) func(dir string) string {
	return func(dir string) string {
		if st, err := state(); err == nil {
			for _, a := range st.Accounts {
				if a.Dir == dir {
					return a.Name
				}
			}
		}
		return baseName(dir)
	}
}

func baseName(dir string) string {
	if i := strings.LastIndexAny(dir, `/\`); i >= 0 {
		return dir[i+1:]
	}
	return dir
}
