package cli

import (
	"github.com/HaiNNT/c-hottag/internal/status"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

type refreshRig struct {
	rr   *refreshReporter
	buf  *syncBuf
	sink *statusSink
	dn   *daemonNotify
	rec  *recordingNotifier
	mu   sync.Mutex
	now  time.Time
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()
	g := &refreshRig{buf: newSyncBuf(), now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	g.sink = sink
	t.Cleanup(sink.Close) // drains the async status write before TempDir is removed
	g.rec = newRecordingNotifier()
	g.dn = newDaemonNotify(notifyTestState("C"), g.rec)
	t.Cleanup(g.dn.Close)
	g.rr = &refreshReporter{log: g.buf, sink: sink, dn: g.dn, now: func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.now
	}}
	return g
}

func (g *refreshRig) advance(d time.Duration) {
	g.mu.Lock()
	g.now = g.now.Add(d)
	g.mu.Unlock()
}

func (g *refreshRig) token(account string) creds.TokenState {
	for _, a := range g.sink.fileCopy().Accounts {
		if a.Name == account {
			return a.Token
		}
	}
	return ""
}

func TestRefreshReporterLogsTheThreeLines(t *testing.T) {
	g := newRefreshRig(t)
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Trigger: tokens.TriggerRequest, Outcome: tokens.OutcomeRenewed,
		ExpiresIn: 7*time.Hour + 59*time.Minute + 30*time.Second, Took: 5200 * time.Millisecond})
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeNotRenewed,
		Detail: "claude exited 0 but the token is still expired", RetryIn: 2 * time.Minute})
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"})
	got := g.buf.String()
	for _, want := range []string{
		"refresh C: renewed, expires in 7h59m (request, 5.2s)\n",
		"refresh C: claude exited 0 but the token is still expired; retry in 2m\n",
		"refresh C: failed: exit status 1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"sk-ant-", "Bearer"} {
		if strings.Contains(got, banned) {
			t.Errorf("log carries %q", banned)
		}
	}
}

func TestRefreshReporterNeverLogsASecretInADetail(t *testing.T) {
	g := newRefreshRig(t)
	for _, outcome := range []string{tokens.OutcomeFailed, tokens.OutcomeNotRenewed, tokens.OutcomePanicked} {
		g.advance(time.Hour)
		g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: outcome, Detail: "x Bearer abc sk-ant-oat01-zzz"})
	}
	got := g.buf.String()
	if strings.Count(got, "\n") != 3 {
		t.Fatalf("log = %q, want three lines", got)
	}
	for _, banned := range []string{"sk-ant-", "Bearer", "abc"} {
		if strings.Contains(got, banned) {
			t.Fatalf("log carries %q: %s", banned, got)
		}
	}
}

func TestRefreshReporterLimitsRepeatsToOneEveryTenMinutes(t *testing.T) {
	g := newRefreshRig(t)
	fail := tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"}
	g.rr.onRefresh(fail)
	g.advance(9 * time.Minute)
	g.rr.onRefresh(fail)
	if n := strings.Count(g.buf.String(), "failed: exit status 1"); n != 1 {
		t.Fatalf("logged %d times inside 10 minutes, want 1", n)
	}
	g.advance(time.Minute)
	g.rr.onRefresh(fail)
	if n := strings.Count(g.buf.String(), "failed: exit status 1"); n != 2 {
		t.Fatalf("logged %d times after 10 minutes, want 2", n)
	}
}

func TestRefreshReporterAChangedOutcomeAlwaysLogs(t *testing.T) {
	g := newRefreshRig(t)
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"})
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "keychain unavailable"})
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"})
	g.rr.onRefresh(tokens.RefreshEvent{Account: "D", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"})
	if n := strings.Count(g.buf.String(), "\n"); n != 4 {
		t.Fatalf("%d lines, want 4 (every change, and another account):\n%s", n, g.buf.String())
	}
}

func TestRefreshReporterRenewalClearsAStaleTokenState(t *testing.T) {
	g := newRefreshRig(t)
	g.sink.setTokenState("C", creds.StateStale)
	if g.token("C") != creds.StateStale {
		t.Fatal("setup: not stale")
	}
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"})
	if g.token("C") != creds.StateStale {
		t.Fatal("a failure cleared the stale state")
	}
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeRenewed, ExpiresIn: time.Hour})
	if g.token("C") != creds.StateOK {
		t.Fatalf("token = %q after a renewal, want ok", g.token("C"))
	}
}

func TestSetTokenClearedClearsStaleAfterASwap(t *testing.T) {
	g := newRefreshRig(t)
	g.sink.setTokenState("C", creds.StateStale)
	g.sink.setTokenCleared("C", g.now)
	if g.token("C") != creds.StateOK {
		t.Fatalf("token = %q, want ok", g.token("C"))
	}
}

func TestRefreshReporterNeedsLoginSetsStatusAndNotifies(t *testing.T) {
	g := newRefreshRig(t)
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeNotRenewed,
		Detail: "claude exited 0 but the token is still expired", NeedsLogin: true, Tries: 6, Span: 15*time.Minute + 30*time.Second})
	if g.token("C") != creds.StateNeedsLogin {
		t.Fatalf("token = %q, want needs-login", g.token("C"))
	}
	notices := drainNotices(t, g.dn, g.rec)
	if len(notices) != 1 || !strings.Contains(notices[0].body, "chottag login C") {
		t.Fatalf("notices = %+v, want one naming `chottag login C`", notices)
	}
	if !strings.Contains(g.buf.String(), "still expired after 6 tries over 16m; it needs a login (run: chottag login C)") {
		t.Fatalf("log lacks the fix:\n%s", g.buf.String())
	}
}

// F269: a refresh that finds no login at all records needs-login and posts
// one notice, not only a lockout does.
func TestRefreshReporterNoLoginSetsStatusAndNotifiesOnce(t *testing.T) {
	g := newRefreshRig(t)
	ev := tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "no usable login: no-login", NoLogin: true}
	g.rr.onRefresh(ev)
	g.rr.onRefresh(ev)
	if g.token("C") != creds.StateNeedsLogin {
		t.Fatalf("token = %q, want needs-login", g.token("C"))
	}
	notices := drainNotices(t, g.dn, g.rec)
	if len(notices) != 1 || !strings.Contains(notices[0].body, "chottag login C") {
		t.Fatalf("notices = %+v, want exactly one naming `chottag login C`", notices)
	}
	// An ordinary failure still records nothing.
	g2 := newRefreshRig(t)
	g2.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "exit status 1"})
	if g2.token("C") == creds.StateNeedsLogin {
		t.Fatal("a plain failure recorded needs-login")
	}
}

func TestShortDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{
		7*time.Hour + 59*time.Minute: "7h59m", 2 * time.Minute: "2m", 90 * time.Second: "1m30s", 30 * time.Second: "30s", -time.Minute: "0s",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%v) = %q, want %q", d, got, want)
		}
	}
}

// The daemon's own wiring (runProxy: refresher, OnRefresh, reporter, sink)
// clears `token: stale` when the slot's token renews.
func TestRunProxyClearsStaleOnARenewalThroughItsWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	slotDir := filepath.Join(home, "accounts", "A")
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	flags := t.TempDir()
	release, renewed := filepath.Join(flags, "release"), filepath.Join(flags, "renewed")
	// A stand-in for claude: it waits to be released, "renews" the token and
	// exits 0, as `claude mcp list` does.
	claude := filepath.Join(flags, "claude")
	script := "#!/bin/sh\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\n: > " + renewed + "\nexit 0\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNotifyStatus(t, home, status.Account{Name: "A", Token: creds.StateStale, TokenAt: time.Now()})

	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	wireFakeUpstream(t, up, upCA)
	credsReadForTest = func(string) (creds.Token, error) {
		if _, err := os.Stat(renewed); err == nil {
			return creds.Token{AccessToken: "tok-A", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		}
		return creds.Token{AccessToken: "tok-A", ExpiresAt: time.Now().Add(-time.Minute)}, nil
	}
	stubDaemonNotifier(t)

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", "", "--claude", claude}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		<-codeCh
	})
	<-errb.done
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The warm loop refreshes the expired serving slot; its log line comes
	// after the status was cleared.
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(errb.String(), "refresh A: renewed") {
		if time.Now().After(deadline) {
			t.Fatalf("no renewal logged; stderr:\n%s", errb.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The status file is written by the sink's own goroutine.
	for {
		f, err := status.Load(status.Path(home))
		if err != nil {
			t.Fatal(err)
		}
		var tok creds.TokenState
		for _, a := range f.Accounts {
			if a.Name == "A" {
				tok = a.Token
			}
		}
		if tok == creds.StateOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("token = %q after a renewal, want ok", tok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRefreshLineForAnUnchangedTokenNamesTheTriggerAndTime(t *testing.T) {
	line, _ := refreshLine(tokens.RefreshEvent{Account: "C", Trigger: tokens.TriggerWarm, Outcome: tokens.OutcomeNotRenewed,
		Detail: "token unchanged, not yet due", Took: 1200 * time.Millisecond})
	if want := "chottag: refresh C: token unchanged, not yet due (warm, 1.2s)"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
}

// Review 11: a needs-login notice fires again after the account recovers (a
// renewal) and then loses its login again.
func TestRefreshReporterRenewalRearmsTheNeedsLoginNotice(t *testing.T) {
	g := newRefreshRig(t)
	bad := tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeFailed, Detail: "no usable login: no-login", NoLogin: true}
	g.rr.onRefresh(bad)
	if n := len(drainNotices(t, g.dn, g.rec)); n != 1 {
		t.Fatalf("first: %d notices, want 1", n)
	}
	g.rr.onRefresh(tokens.RefreshEvent{Account: "C", Outcome: tokens.OutcomeRenewed, ExpiresIn: time.Hour})
	g.rr.onRefresh(bad)
	if n := len(drainNotices(t, g.dn, g.rec)); n != 1 {
		t.Fatalf("after a recovery: %d notices, want 1 more", n)
	}
}

func TestRecordRecoveredClearsTheMarkAndRearmsTheNotice(t *testing.T) {
	g := newRefreshRig(t)
	recordNeedsLogin(g.sink, g.dn, "C")
	drainNotices(t, g.dn, g.rec)
	recordRecovered(g.sink, g.dn, "C", g.now)
	if g.token("C") != creds.StateOK {
		t.Fatalf("token = %q, want ok after a poll that worked", g.token("C"))
	}
	recordNeedsLogin(g.sink, g.dn, "C")
	if n := len(drainNotices(t, g.dn, g.rec)); n != 1 {
		t.Fatalf("%d notices after recovery and a new loss, want 1", n)
	}
}
