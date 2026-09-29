package cli

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// TestUsageHookSeedsTheConfiguredRosterBeforeObserving pins contract 2 at
// the level a reviewer found unpinned: not statusSink.ensureAccounts in
// isolation (already covered), but the actual OnUsage hook runProxy wires
// up. Deleting the ensureAccounts call inside newUsageHook left the whole
// suite green before this test existed.
func TestUsageHookSeedsTheConfiguredRosterBeforeObserving(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}}}, nil
	}
	hook := newUsageHook(state, sink, nil)

	until := time.Now().Add(time.Hour)
	hdr := http.Header{}
	hdr.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	hdr.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	hdr.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	hdr.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(until.Unix(), 10))
	hook("D", 429, hdr)
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 4 {
		t.Fatalf("accounts = %+v, want the usage hook to have seeded all 4 configured accounts before observing", f.Accounts)
	}
	if f.Limits.AllLimited {
		t.Fatal("AllLimited = true with only 1 of 4 configured accounts ever observed (the other 3 have full quota); the usage hook must seed the roster before Observe")
	}
}

// TestPassthroughHookRoutesEventsToTheStatusCache pins newPassthroughHook's
// own behaviour: deleting the setPassthrough routing inside it left the
// whole suite green before this test existed. It also pins that a
// non-passthrough event (e.g. owner-unregistered) must not touch the
// cache, and that every event — passthrough or not — still reaches the
// printer.
//
// This does NOT pin that runProxy actually installs newPassthroughHook as
// selector.Config.OnEvent rather than a bare printer — see
// TestNewSelectorConfigRoutesPassthroughEventsToTheStatusCache below for
// that; a round of review found exactly this gap (the helper pinned, the
// wiring not) after this test was already in place.
func TestPassthroughHookRoutesEventsToTheStatusCache(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	printed := 0
	hook := newPassthroughHook(sink, func(selector.Event) { printed++ })

	hook(selector.Event{Kind: "passthrough", Account: "D", Status: creds.Status{State: creds.StateStale}})
	// Checked in-memory, not via disk: the write is now queued
	// asynchronously (F34), so a round trip through disk immediately after
	// would race the writer goroutine. sink.mu guards c.file the same way
	// it guards every other read of it.
	sink.mu.Lock()
	got := sink.passthroughLocked("D")
	sink.mu.Unlock()
	if got == "" {
		t.Fatalf("passthrough = %q, want D marked passthrough", got)
	}
	if printed != 1 {
		t.Fatalf("printed = %d, want 1: the hook must still forward every event to the throttled printer", printed)
	}

	hook(selector.Event{Kind: "owner-unregistered", Account: "D"})
	sink.Close()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Accounts[0].Passthrough, "stale") {
		t.Fatalf("a non-passthrough event must not touch the passthrough mark: %+v", f.Accounts[0])
	}
	if printed != 2 {
		t.Fatalf("printed = %d, want 2 after a second event", printed)
	}
}

// TestNewSelectorConfigRoutesPassthroughEventsToTheStatusCache pins the
// piece contract 3 was actually missing: not newPassthroughHook's own
// behaviour (already covered above), but that runProxy's call site
// installs it as OnEvent at all. `OnEvent: newPassthroughHook(sink,
// printEvent)` silently shrinking to `OnEvent: printEvent` in runProxy
// still compiles and left the whole suite green — TestPassthroughHook...
// calls the hook directly, and the proxy-run-level test forces an early
// return before any selector event can fire, so neither could see it.
// newSelectorConfig is the exact call site runProxy uses; this test builds
// the same Config it would and asserts on the OnEvent it contains.
//
// It also pins the M2b wiring (PF2): newSelectorConfig's dn parameter is
// what makes a needs-login passthrough event reach notifyEventHook at all
// — `OnEvent: newPassthroughHook(sink, printEvent)`, dropping dn, still
// compiles and leaves the rest of the suite green.
func TestNewSelectorConfigRoutesPassthroughEventsToTheStatusCache(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	printed := 0
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("D"), n)
	defer dn.Close()
	cfg := newSelectorConfig(
		func() (store.State, error) { return store.State{}, nil },
		fakeTokens{},
		fakeOwners{},
		sink,
		func(selector.Event) { printed++ },
		dn,
	)
	if cfg.OnEvent == nil {
		t.Fatal("OnEvent is nil; runProxy would install no passthrough routing at all")
	}

	cfg.OnEvent(selector.Event{Kind: "passthrough", Account: "D", Status: creds.Status{State: creds.StateStale}})
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("notices = %+v, want none for a merely stale token", got)
	}
	cfg.OnEvent(needsLoginEvent("D"))
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: D needs login" {
		t.Fatalf("notices = %+v, want one needs-login for D through the exact Config runProxy builds", got)
	}
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough == "" {
		t.Fatalf("accounts = %+v; the Config runProxy actually builds must route a passthrough event to the status cache (F20) — a bare printer would leave this empty", f.Accounts)
	}
	if printed != 2 {
		t.Fatalf("printed = %d, want 2: the Config must still forward every event to the printer", printed)
	}
}

// fixedServingChooser swaps every Serving-class request onto a fixed
// account, mirroring proxy_test's servingSwap (unexported to that
// package's tests, so this end-to-end test — which lives in package cli to
// reach newUsageHook — defines its own).
type fixedServingChooser struct{ account, token string }

func (f fixedServingChooser) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	if d.Class == router.Serving {
		return f.account, f.token, false, true
	}
	return "", "", false, false
}

func (fixedServingChooser) Record(router.Kind, []string, string) {}

func (fixedServingChooser) Refresh(context.Context, string) (string, bool) { return "", false }

// TestUsageHookThroughARealSwappedResponseReachesDisk is the integration
// test a reviewer asked for: it closes the gap that unit tests on
// newUsageHook/statusSink cannot see — that proxy.Server, given this hook,
// actually calls it for a real swapped HTTP response, with the real
// captured unified rate-limit header names, and the result lands on disk.
//
// It also pins the M2b wiring (PF2): newUsageHook's dn parameter is what
// makes a real swapped 2xx re-arm D's needs-login notice —
// `notifyUsageHook(dn, newUsageHook(...))` shrinking to bare `newUsageHook(...)`
// at the call site still compiles and leaves the rest of the suite green.
func TestUsageHookThroughARealSwappedResponseReachesDisk(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "D"}}}, nil
	}
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("D"), n)
	defer dn.Close()
	dn.events.NeedsLogin("D")
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: D needs login" {
		t.Fatalf("notices = %+v, want the seeded needs-login for D", got)
	}
	onUsage := newUsageHook(state, sink, dn)

	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.WriteHeader(200)
	}), proxytest.Options{
		Choose:  fixedServingChooser{account: "D", token: "tok-D"},
		OnUsage: onUsage,
	})

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-HOME-SECRET")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// fixedServingChooser is a test double, not proxy.go's own *chooser, so
	// it never calls dn.beginChoose: the response above is trusted at once,
	// and re-arms D straight away.
	dn.events.NeedsLogin("D")
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: D needs login" {
		t.Fatalf("notices = %+v, want a real swapped 2xx to have re-armed D's needs-login notice", got)
	}

	sink.Close()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Name != "D" || f.Accounts[0].Usage == nil ||
		f.Accounts[0].Usage.FiveHourPct == nil || *f.Accounts[0].Usage.FiveHourPct != 42 {
		t.Fatalf("accounts = %+v, want D observed on disk with a 5h pct of 42 (0-100 wire units)", f.Accounts)
	}
}
