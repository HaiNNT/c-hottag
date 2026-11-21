package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

var lrNow = time.Date(2026, 11, 21, 15, 4, 5, 0, time.UTC)

type lrRig struct {
	w    *loginRefusalWatch
	sink *statusSink
	log  *bytes.Buffer
	st   store.State
}

func newLRRig(t *testing.T) *lrRig {
	t.Helper()
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	r := &lrRig{sink: sink, log: &bytes.Buffer{}}
	r.st = store.Default()
	r.st.Accounts = []store.Account{{Name: "C", Dir: "/slots/C"}, {Name: "B", Dir: "/slots/B"}}
	r.w = newLoginRefusalWatch(r.log, sink, func() (store.State, error) { return r.st, nil }, func() time.Time { return lrNow })
	return r
}

func (r *lrRig) row(name string) status.Account {
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	for _, a := range r.sink.file.Accounts {
		if a.Name == name {
			return a
		}
	}
	return status.Account{}
}

func TestLoginRefusalMarksAndLogsOncePerEpisode(t *testing.T) {
	r := newLRRig(t)
	r.w.refused("C", 403)
	r.w.refused("C", 403)
	r.w.refused("C", 401)
	row := r.row("C")
	if !row.LoginRefused.Equal(lrNow) || row.LoginRefusedStatus != 403 {
		t.Fatalf("row = %+v, want the first refusal (403 at %s)", row, lrNow)
	}
	want := "chottag: C's login is refused on serving requests (403); out of rotation until it answers again or you run: chottag login C\n"
	if r.log.String() != want {
		t.Fatalf("log = %q, want one line %q", r.log.String(), want)
	}
	if !r.w.isRefused("C") || r.w.isRefused("B") {
		t.Fatal("isRefused: C must be refused and B not")
	}
}

func TestLoginRefusalAcceptedClearsAndLogs(t *testing.T) {
	r := newLRRig(t)
	r.w.accepted("C") // nothing to clear: no line, no row
	if r.log.Len() != 0 {
		t.Fatalf("log = %q, want none", r.log.String())
	}
	r.w.refused("C", 403)
	r.log.Reset()
	r.w.accepted("C")
	if r.w.isRefused("C") || !r.row("C").LoginRefused.IsZero() {
		t.Fatal("the mark survived accepted")
	}
	if want := "chottag: C's login works again (refused since 15:04:05)\n"; r.log.String() != want {
		t.Fatalf("log = %q, want %q", r.log.String(), want)
	}
}

func TestLoginRefusalSupersededByALaterLogin(t *testing.T) {
	r := newLRRig(t)
	r.w.refused("C", 403)
	r.st.Accounts[0].LoggedInAt = lrNow.Add(time.Minute)
	if r.w.isRefused("C") {
		t.Fatal("a login after the mark must supersede it")
	}
	// A refusal after that login starts a fresh episode.
	r.w.now = func() time.Time { return lrNow.Add(2 * time.Minute) }
	r.log.Reset()
	r.w.refused("C", 401)
	row := r.row("C")
	if !row.LoginRefused.Equal(lrNow.Add(2*time.Minute)) || row.LoginRefusedStatus != 401 || !r.w.isRefused("C") {
		t.Fatalf("row = %+v, want a new episode at +2m with 401", row)
	}
	if !strings.Contains(r.log.String(), "(401)") {
		t.Fatalf("log = %q, want the new episode's line", r.log.String())
	}
}

func TestLoginRefusalTokenRenewalDoesNotClear(t *testing.T) {
	r := newLRRig(t)
	r.w.refused("C", 403)
	r.sink.setTokenCleared("C", lrNow.Add(time.Minute)) // what a renewal records
	if !r.w.isRefused("C") {
		t.Fatal("a token renewal must not clear the mark")
	}
}

func TestLoginRefusalUsageHookClearsOnly2xx(t *testing.T) {
	r := newLRRig(t)
	var seen []int
	hook := loginRefusedUsageHook(r.w, func(_ string, code int, _ http.Header, _ proxy.UsageRequest) { seen = append(seen, code) })
	r.w.refused("C", 403)
	hook("C", 403, nil, proxy.UsageRequest{})
	hook("C", 429, nil, proxy.UsageRequest{})
	if !r.w.isRefused("C") {
		t.Fatal("a 403/429 cleared the mark")
	}
	hook("B", 200, nil, proxy.UsageRequest{})
	if !r.w.isRefused("C") {
		t.Fatal("another account's 2xx cleared C")
	}
	hook("C", 200, nil, proxy.UsageRequest{})
	if r.w.isRefused("C") {
		t.Fatal("a 2xx on C did not clear the mark")
	}
	if len(seen) != 4 {
		t.Fatalf("next hook saw %v, want all four calls", seen)
	}
}

func TestLoginRefusalNilSafe(t *testing.T) {
	var w *loginRefusalWatch
	w.refused("C", 403)
	w.accepted("C")
	if w.isRefused("C") {
		t.Fatal("nil watch reports a refusal")
	}
	called := false
	loginRefusedUsageHook(nil, func(string, int, http.Header, proxy.UsageRequest) { called = true })("C", 200, nil, proxy.UsageRequest{})
	if !called {
		t.Fatal("a nil watch must pass through")
	}
}

func TestServingRefusalHookMarksBeforeTheNotice(t *testing.T) {
	var log bytes.Buffer
	var order []string
	hook := servingRefusalHook(&log, nil, func(account string, st int) { order = append(order, account) })
	hook("C", 403, "", true, "POST", "/v1/messages")
	if len(order) != 1 || order[0] != "C" {
		t.Fatalf("onRefused calls = %v, want [C]", order)
	}
	if !strings.Contains(log.String(), "C's login was refused (403)") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestStatusRowAndSupersededMark(t *testing.T) {
	since := lrNow
	f := status.File{Accounts: []status.Account{
		{Name: "C", LoginRefused: since, LoginRefusedStatus: 403},
		{Name: "B", LoginRefused: since, LoginRefusedStatus: 401},
	}}
	st := store.Default()
	st.Accounts = []store.Account{{Name: "C"}, {Name: "B", LoggedInAt: since.Add(time.Second)}}
	dropSupersededLoginRefused(&f, st)
	if f.Accounts[0].LoginRefused.IsZero() {
		t.Fatal("C's standing mark was dropped")
	}
	if !f.Accounts[1].LoginRefused.IsZero() || f.Accounts[1].LoginRefusedStatus != 0 {
		t.Fatalf("B's superseded mark stayed: %+v", f.Accounts[1])
	}
	b, err := json.Marshal(f.Accounts[1])
	if err != nil || strings.Contains(string(b), "loginRefused") {
		t.Fatalf("json = %s (%v), want no loginRefused fields", b, err)
	}

	var out bytes.Buffer
	f.Accounts[0].Usage = &status.Usage{UpdatedAt: since}
	renderStatusSpread(&out, f, since, "", nil)
	want := "login refused (403) since " + since.Local().Format("15:04")
	if !strings.Contains(out.String(), want) {
		t.Fatalf("status output lacks %q:\n%s", want, out.String())
	}
}
