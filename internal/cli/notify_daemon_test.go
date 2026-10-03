package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/notify"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// notice is one delivered notification.
type notice struct{ title, body string }

// recordingNotifier records every notice, in delivery order.
type recordingNotifier struct{ got chan notice }

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{got: make(chan notice, 64)}
}

func (n *recordingNotifier) Send(_ context.Context, title, body string) error {
	n.got <- notice{title, body}
	return nil
}

// hangingNotifier blocks every send until its context ends, and says when
// the first one started.
type hangingNotifier struct{ entered chan struct{} }

func (h hangingNotifier) Send(ctx context.Context, _, _ string) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

const drainSentinel = "chottag-test-sentinel"

// drainNotices returns every notice delivered before this call. It puts a
// sentinel straight on dn's dispatcher, which has one sender and is FIFO,
// and reads up to it: anything Events queued earlier has been delivered by
// then, so "exactly one" is a real count, not a sampling window.
func drainNotices(t *testing.T, dn *daemonNotify, n *recordingNotifier) []notice {
	t.Helper()
	dn.disp.Enqueue(drainSentinel, "")
	var out []notice
	for {
		select {
		case m := <-n.got:
			if m.title == drainSentinel {
				return out
			}
			out = append(out, m)
		case <-time.After(5 * time.Second):
			t.Fatal("the sentinel notice never arrived")
			return nil
		}
	}
}

// stubDaemonNotifier makes runProxyWithSignal's daemon post to a recorder
// instead of osascript, for one test. TestMain's backstop crashes any
// daemon test that posts a notice without it.
func stubDaemonNotifier(t *testing.T) *recordingNotifier {
	t.Helper()
	n := newRecordingNotifier()
	orig := newDaemonNotifier
	newDaemonNotifier = func() notify.Notifier { return n }
	t.Cleanup(func() { newDaemonNotifier = orig })
	return n
}

// notifyTestState is a state func holding the named accounts, with emails,
// an org and slot paths that no notice may ever carry.
func notifyTestState(names ...string) func() (store.State, error) {
	return func() (store.State, error) {
		st := store.Default()
		for _, n := range names {
			st.Accounts = append(st.Accounts, store.Account{
				Name:  n,
				Email: strings.ToLower(n) + "@example.com",
				Org:   "Acme Org",
				Dir:   "/slots/" + n,
			})
		}
		return st, nil
	}
}

func needsLoginEvent(account string) selector.Event {
	return selector.Event{Kind: "passthrough", Account: account, Status: creds.Status{State: creds.StateNeedsLogin}}
}

// writeNotifyStatus writes cache/status.json under home with these rows.
func writeNotifyStatus(t *testing.T, home string, accts ...status.Account) {
	t.Helper()
	b, err := status.Marshal(status.File{Version: status.Version, Accounts: accts})
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

// TestNotifyEventHookFiresNeedsLoginOnceForABurst is Review Focus 4 through
// the hook the selector calls.
func TestNotifyEventHookFiresNeedsLoginOnceForABurst(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	var printed atomic.Int32
	hook := notifyEventHook(dn, func(selector.Event) { printed.Add(1) })
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook(needsLoginEvent("B"))
		}()
	}
	wg.Wait()
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want exactly one needs-login for B", got)
	}
	if printed.Load() != 32 {
		t.Fatalf("the printer saw %d events, want all 32: the hook forwards every event", printed.Load())
	}
}

func TestNotifyEventHookIgnoresOtherEvents(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	hook := notifyEventHook(dn, func(selector.Event) {})
	hook(selector.Event{Kind: "passthrough", Account: "B", Status: creds.Status{State: creds.StateStale}})
	hook(selector.Event{Kind: "passthrough", Detail: "no serving account set"})
	hook(selector.Event{Kind: "passthrough", Account: "B", Detail: "no such account"})
	hook(selector.Event{Kind: "owner-unregistered", Account: "B", Status: creds.Status{State: creds.StateNeedsLogin}})
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("notices = %+v, want none: only a passthrough whose token state is needs-login fires", got)
	}
}

// TestNotifyUsageHookReArmsNeedsLoginOnlyOnASuccess also isolates the
// status-code check from the blocked-flag check (round 2): dn.beginChoose
// runs right after the first needs-login notice, before the 429/401 calls,
// so those run unblocked — what excludes them from re-arming is purely
// `code >= 200 && code < 300`, not an incidental overlap with `isBlocked`.
// Mutating that condition to `if !dn.isBlocked(account)` (dropping the
// status-code check) makes 429 re-arm early: the second needs-login below
// then fires immediately instead of staying deduped, and the "want 1"
// assertion catches it.
func TestNotifyUsageHookReArmsNeedsLoginOnlyOnASuccess(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	var forwarded atomic.Int32
	usage := notifyUsageHook(dn, func(string, int, http.Header) { forwarded.Add(1) })
	hook := notifyEventHook(dn, func(selector.Event) {})

	hook(needsLoginEvent("B"))
	dn.beginChoose("B") // a fresh Choose right after the notice: unblocks B for the refused calls below
	usage("B", 429, http.Header{})
	usage("B", 401, http.Header{})
	hook(needsLoginEvent("B")) // re-blocks B: it still needs login as of this rediscovery
	if got := drainNotices(t, dn, n); len(got) != 1 {
		t.Fatalf("notices = %+v, want 1: a refused response is not a sign the login works", got)
	}
	// The request that actually succeeds is chosen fresh, after the
	// rediscovery above re-blocked B: its own Choose is what clears the
	// flag this time, exactly as production requires.
	dn.beginChoose("B")
	usage("B", 200, http.Header{})
	hook(needsLoginEvent("B"))
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want a second needs-login after a 2xx re-armed B", got)
	}
	if forwarded.Load() != 3 {
		t.Fatalf("the usage hook forwarded %d calls, want 3", forwarded.Load())
	}
}

// TestNotifyUsageHookIgnoresA2xxFromARequestInFlightBeforeNeedsLogin is T1
// review #5: a request that was already in flight, swapped onto an
// account, before that account's needs-login notice fires must not re-arm
// it just because it finishes with a 2xx afterwards — the account still
// needs login as of the notice, and re-arming on the stale response would
// let the very next request post a spurious second notice. dn.beginChoose
// stands in for what proxy.go's chooser.Choose calls in production.
func TestNotifyUsageHookIgnoresA2xxFromARequestInFlightBeforeNeedsLogin(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	usage := notifyUsageHook(dn, func(string, int, http.Header) {})
	hook := notifyEventHook(dn, func(selector.Event) {})

	hook(needsLoginEvent("B"))
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want the first needs-login for B", got)
	}

	// The request already in flight when the notice above fired (chosen
	// before it, so blocked[B] was still false at Choose time) finally
	// completes with a 2xx. B stays blocked, so it must not re-arm.
	usage("B", 200, http.Header{}) // the stale request's late success
	hook(needsLoginEvent("B"))
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("notices = %+v, want none: the 2xx belonged to a request chosen before the needs-login notice, so it must not re-arm B", got)
	}

	// A fresh request, chosen after B recovers, clears blocked[B] and
	// legitimately re-arms on its own 2xx.
	dn.beginChoose("B")
	usage("B", 200, http.Header{})
	hook(needsLoginEvent("B"))
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want B re-armed by the fresh success", got)
	}
}

// TestNotifyUsageHookBlockedFlagDoesNotLeakWhenAChosenRequestNeverCompletes
// is the leak case the old in-flight counter had: it only retired an
// in-flight slot when the usage hook fired, which a transport error,
// ErrorHandler, a client cancel before headers, a panic, or the drift
// safety net's own fallback all skip. A per-account boolean has nothing to
// leak: a Choose that clears blocked[B] and is then followed by no OnUsage
// call at all still leaves B free to re-arm normally on the next real
// success.
func TestNotifyUsageHookBlockedFlagDoesNotLeakWhenAChosenRequestNeverCompletes(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	usage := notifyUsageHook(dn, func(string, int, http.Header) {})
	hook := notifyEventHook(dn, func(selector.Event) {})

	hook(needsLoginEvent("B"))
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want the first needs-login for B", got)
	}

	// Chosen once, then the request errors before any response: no OnUsage
	// call ever follows for it.
	dn.beginChoose("B")

	// A later request is chosen again and succeeds.
	dn.beginChoose("B")
	usage("B", 200, http.Header{})
	hook(needsLoginEvent("B"))
	if got := drainNotices(t, dn, n); len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want a second needs-login: the completed request's 2xx re-armed B, and nothing leaked", got)
	}
}

func TestNotifyTickAllLimitedThenAvailable(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeNotifyStatus(t, dir,
		status.Account{Name: "A", Limited: true, LimitedUntil: now.Add(2 * time.Hour)},
		status.Account{Name: "B", Limited: true, LimitedUntil: now.Add(time.Hour)},
	)
	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := notifyTestState("A", "B")
	n := newRecordingNotifier()
	dn := newDaemonNotify(state, n)
	defer dn.Close()

	dn.tick(state, sink, 0, now)
	dn.tick(state, sink, 0, now.Add(time.Minute))
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: all accounts limited" || !strings.HasPrefix(got[0].body, "Next reset: B at ") {
		t.Fatalf("notices = %+v, want one all-limited naming B's earlier reset", got)
	}

	dn.tick(state, sink, 0, now.Add(90*time.Minute)) // B's reset has passed, with no traffic
	got = drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: an account is available again" || !strings.HasPrefix(got[0].body, "B is no longer limited") {
		t.Fatalf("notices = %+v, want B available again", got)
	}
}

// An account the cache has never observed is available: the tick seeds
// the roster from state.json before it reads the roll-up (ruling 7).
func TestNotifyTickCountsAnAccountTheCacheHasNotSeen(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeNotifyStatus(t, dir, status.Account{Name: "A", Limited: true, LimitedUntil: now.Add(time.Hour)})
	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := notifyTestState("A", "B")
	n := newRecordingNotifier()
	dn := newDaemonNotify(state, n)
	defer dn.Close()
	dn.tick(state, sink, 0, now)
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("notices = %+v, want none: B has no row yet, so not every account is limited", got)
	}
}

func TestNotifyTickRouteDriftOncePerGeneration(t *testing.T) {
	dir := t.TempDir()
	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := notifyTestState("A")
	n := newRecordingNotifier()
	dn := newDaemonNotify(state, n)
	defer dn.Close()
	now := time.Now()
	dn.tick(state, sink, 0, now)
	dn.tick(state, sink, 3, now)
	dn.tick(state, sink, 5, now)
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: route drift" || !strings.HasSuffix(got[0].body, "Run: chottag trace on") {
		t.Fatalf("notices = %+v, want one route-drift notice", got)
	}
	next := newDaemonNotify(state, n) // a restarted daemon
	defer next.Close()
	next.tick(state, sink, 5, now)
	if got := drainNotices(t, next, n); len(got) != 1 {
		t.Fatalf("a new generation sent %d route-drift notices, want 1 (D11)", len(got))
	}
}

// TestNotifyOffInStateJSONTakesEffectWithoutRestart is Review Focus 2,
// with the real command and the daemon's real store.Cache.
func TestNotifyOffInStateJSONTakesEffectWithoutRestart(t *testing.T) {
	_, s := seedTwoAccounts(t)
	cache := store.NewCache(s)
	n := newRecordingNotifier()
	dn := newDaemonNotify(cache.State, n)
	defer dn.Close()
	hook := notifyEventHook(dn, func(selector.Event) {})

	if code, _, errs := runChottag(t, "notify", "off"); code != 0 {
		t.Fatalf("notify off = %d; stderr %q", code, errs)
	}
	hook(needsLoginEvent("A"))
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("notices = %+v, want none with notify off", got)
	}
	if code, _, errs := runChottag(t, "notify", "on"); code != 0 {
		t.Fatalf("notify on = %d; stderr %q", code, errs)
	}
	hook(needsLoginEvent("A")) // consumed while off: not replayed
	hook(needsLoginEvent("B"))
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: B needs login" {
		t.Fatalf("notices = %+v, want only B's", got)
	}
}

func TestNotifyEnabledReadsAsOnWhenStateIsUnreadable(t *testing.T) {
	broken := func() (store.State, error) { return store.State{}, os.ErrPermission }
	if !notifyEnabled(broken) {
		t.Fatal("notifyEnabled = false on a state read error, want on (ruling 6)")
	}
}

// No notice ever carries an email, an org or a path, whatever state.json
// and status.json hold (spec §4 "Content").
func TestNotifyMessagesNeverCarryEmailOrgOrPath(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeNotifyStatus(t, dir,
		status.Account{Name: "A", Email: "a@example.com", Org: "Acme Org", Limited: true, LimitedUntil: now.Add(time.Hour)},
		status.Account{Name: "B", Email: "b@example.com", Org: "Acme Org", Limited: true, LimitedUntil: now.Add(2 * time.Hour)},
	)
	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := notifyTestState("A", "B")
	n := newRecordingNotifier()
	dn := newDaemonNotify(state, n)
	defer dn.Close()
	notifyEventHook(dn, func(selector.Event) {})(needsLoginEvent("B"))
	dn.tick(state, sink, 1, now)
	dn.tick(state, sink, 1, now.Add(90*time.Minute))
	got := drainNotices(t, dn, n)
	if len(got) != 4 {
		t.Fatalf("notices = %+v, want needs-login, route drift, all limited, available", got)
	}
	for _, m := range got {
		text := m.title + "\n" + m.body
		for _, bad := range []string{"@", "example.com", "Acme", "/slots/", dir, "sk-ant"} {
			if strings.Contains(text, bad) {
				t.Errorf("notice %q carries %q", text, bad)
			}
		}
	}
}

// TestRosterTickStampsNotifyErrorsInsideTheHeartbeatWindow is Review
// Focus 3's count reaching status.json: a changed notifyErrors makes the
// tick write even inside daemonHeartbeatInterval, like the other counters.
func TestRosterTickStampsNotifyErrorsInsideTheHeartbeatWindow(t *testing.T) {
	dir := t.TempDir()
	addSlotAccount(t, dir, "A")
	cache := store.NewCache(store.Store{Dir: dir})
	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, snapshots := daemonSnapshotWriter(t)
	sink.write = writer
	entered := make(chan struct{}, 1)
	dn := newDaemonNotify(cache.State, hangingNotifier{entered: entered})
	defer dn.Close()
	stderr := newSyncBuf()
	tick := make(chan time.Time)
	processed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Cache:           cache,
			Sink:            sink,
			Tokens:          tokens.New(tokens.Config{}),
			Owners:          own,
			Notify:          dn,
			RosterTick:      tick,
			RosterProcessed: processed,
		})
	}()
	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}
	// The F96 startup stamp: nothing has failed yet.
	awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool { return !d.Heartbeat.IsZero() && d.NotifyErrors == 0 })

	// One notice hangs in flight, QueueSize wait, and two overflow: the
	// overflow is counted synchronously, and the hung send holds the count
	// still until the tick below reads it.
	dn.disp.Enqueue("in flight", "")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatcher never started the first send")
	}
	for i := 0; i < notify.QueueSize+2; i++ {
		dn.disp.Enqueue("queued", "")
	}
	if got := dn.Errors(); got != 2 {
		t.Fatalf("Errors = %d after overfilling the queue, want 2", got)
	}

	// time.Now() is well inside daemonHeartbeatInterval of the startup
	// stamp, so only the changed counter can make this tick write.
	select {
	case tick <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never consumed the tick")
	}
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never finished processing the tick")
	}
	awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool { return d.NotifyErrors == 2 })

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// TestRunProxyWiresNotifications drives the production path: the daemon
// that runProxyWithSignal builds takes its notifier from newDaemonNotifier,
// hands it to runDaemon, and the startup tick posts the all-limited notice
// for a home whose every account is limited.
func TestRunProxyWiresNotifications(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	addSlotAccount(t, home, "B")
	until := time.Now().Add(time.Hour)
	writeNotifyStatus(t, home,
		status.Account{Name: "A", Limited: true, LimitedUntil: until},
		status.Account{Name: "B", Limited: true, LimitedUntil: until},
	)
	rec := stubDaemonNotifier(t) // registered first, so restored last

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})
	select {
	case m := <-rec.got:
		if m.title != "chottag: all accounts limited" {
			t.Fatalf("first notice = %+v, want the all-limited notice", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the daemon's startup tick never posted the all-limited notice; stderr %q", errb.String())
	}
}

// R158: a refused serving request writes one daemon.log line per refusal,
// with the account, status, method and templated path and nothing else, and
// posts one notice per account per hour.
func TestServingRefusalHookLogsEveryRefusalAndNotifiesOnce(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	var log bytes.Buffer
	hook := servingRefusalHook(&log, dn)
	hook("B", 401, true, "POST", "/v1/messages")
	hook("B", 403, true, "POST", "/v1/messages")
	wantLog := "chottag: B's login was refused (401) on POST /v1/messages; sent on Home's own login\n" +
		"chottag: B's login was refused (403) on POST /v1/messages; sent on Home's own login\n"
	if log.String() != wantLog {
		t.Fatalf("log = %q\nwant %q", log.String(), wantLog)
	}
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: B's login was refused (401)" {
		t.Fatalf("notices = %+v, want exactly one, for the first refusal", got)
	}
}

// The line names no credential, body or proxy setting, and a request-derived
// field cannot start a second log line.
func TestServingRefusalHookLineCarriesNoSecretsOrLineBreaks(t *testing.T) {
	// The hook reads no environment; this guards a future change that would
	// start echoing the proxy setting.
	t.Setenv("HTTPS_PROXY", "http://user:proxy-secret@127.0.0.1:1")
	var log bytes.Buffer
	servingRefusalHook(&log, nil)("B", 401, false, "POST", "/v1/messages\nchottag: forged")
	out := log.String()
	for _, secret := range []string{"proxy-secret", "HTTPS_PROXY", "Bearer", "sk-ant"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log line carries %q: %q", secret, out)
		}
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "; not resent\n") {
		t.Fatalf("log = %q, want one line ending \"; not resent\"", out)
	}
}
