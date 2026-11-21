package cli

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// loginRefusalWatch is the daemon's per-account "login refused" mark (R201):
// an account whose own login was refused 401/403 on serving requests, after
// the refresh-and-retry, is out of rotation until it answers again or a later
// login replaces it. The mark lives in the status sink (accounts[].loginRefused)
// where the planner and `status` read it; this type only decides when it is
// set and cleared and writes the one daemon.log line per episode. It carries
// no token and no body. Every method is safe on a nil watch.
type loginRefusalWatch struct {
	log   io.Writer
	sink  *statusSink
	state func() (store.State, error)
	now   func() time.Time
}

func newLoginRefusalWatch(log io.Writer, sink *statusSink, state func() (store.State, error), now func() time.Time) *loginRefusalWatch {
	return &loginRefusalWatch{log: log, sink: sink, state: state, now: now}
}

// loggedInAt is the account's latest login time from state.json; zero when
// unreadable or unknown.
func (w *loginRefusalWatch) loggedInAt(account string) time.Time {
	if w.state == nil {
		return time.Time{}
	}
	st, err := w.state()
	if err != nil {
		return time.Time{}
	}
	for _, a := range st.Accounts {
		if strings.EqualFold(a.Name, account) {
			return a.LoggedInAt
		}
	}
	return time.Time{}
}

// refused records a login refusal (status 401 or 403): the mark is set and one
// log line written, once per episode.
func (w *loginRefusalWatch) refused(account string, code int) {
	if w == nil || w.sink == nil {
		return
	}
	if !w.sink.setLoginRefused(account, code, w.now(), w.loggedInAt(account)) {
		return
	}
	if w.log != nil {
		fmt.Fprintf(w.log, "chottag: %s's login is refused on serving requests (%d); out of rotation until it answers again or you run: chottag login %s\n", account, code, account)
	}
}

// accepted clears the account's mark when it has one, and logs that the login
// works again.
func (w *loginRefusalWatch) accepted(account string) {
	if w == nil || w.sink == nil {
		return
	}
	since, changed := w.sink.clearLoginRefused(account)
	if !changed || w.log == nil {
		return
	}
	fmt.Fprintf(w.log, "chottag: %s's login works again (refused since %s)\n", account, since.Format("15:04:05"))
}

// isRefused is proxy.Config.LoginRefused: the account has a mark that no later
// login has superseded. It does not block beyond the sink's mutex.
func (w *loginRefusalWatch) isRefused(account string) bool {
	if w == nil || w.sink == nil {
		return false
	}
	return w.sink.loginRefusedLocked(account, w.loggedInAt(account))
}

// loginRefusedUsageHook wraps proxy.Config.OnUsage, which fires only for a
// swapped response answered on the account's own credential: a 2xx clears the
// account's mark. It is the one place a response clears it (R201 ruling 6);
// a 401/403 never does.
func loginRefusedUsageHook(w *loginRefusalWatch, next func(string, int, http.Header, proxy.UsageRequest)) func(string, int, http.Header, proxy.UsageRequest) {
	if w == nil {
		return next
	}
	return func(account string, code int, h http.Header, req proxy.UsageRequest) {
		next(account, code, h, req)
		if code >= 200 && code < 300 {
			w.accepted(account)
		}
	}
}

// dropSupersededLoginRefused clears a login-refused mark that a later login
// has outlived (R201), as dropSupersededNeedsLogin does for needs-login: the
// row reads as it would without the mark. Both `status` surfaces use it.
func dropSupersededLoginRefused(f *status.File, st store.State) {
	for i := range f.Accounts {
		row := &f.Accounts[i]
		if row.LoginRefused.IsZero() {
			continue
		}
		for _, a := range st.Accounts {
			if strings.EqualFold(a.Name, row.Name) && !row.LoginRefusedNow(a.LoggedInAt) {
				row.LoginRefused, row.LoginRefusedStatus = time.Time{}, 0
				break
			}
		}
	}
}
