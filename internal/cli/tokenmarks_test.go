package cli

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// F273: a fresh login, a renewal or a poll success clears the passthrough
// text that only restates a token state, and leaves every other reason.

var markT0 = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

func sinkWith(t *testing.T, accts ...status.Account) *statusSink {
	t.Helper()
	home := t.TempDir()
	writeNotifyStatus(t, home, accts...)
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	return sink
}

func rowOf(sink *statusSink, name string) status.Account {
	for _, a := range sink.fileCopy().Accounts {
		if a.Name == name {
			return a
		}
	}
	return status.Account{}
}

func TestSetTokenClearedClearsATokenPassthroughButKeepsOtherReasons(t *testing.T) {
	sink := sinkWith(t,
		status.Account{Name: "A", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "token needs-login"},
		status.Account{Name: "B", Token: creds.StateStale, TokenAt: markT0, Passthrough: "token stale: expired"},
		status.Account{Name: "C", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "no serving account"},
	)
	for _, n := range []string{"A", "B", "C"} {
		sink.setTokenCleared(n, markT0.Add(time.Minute))
	}
	a, b, c := rowOf(sink, "A"), rowOf(sink, "B"), rowOf(sink, "C")
	if a.Token != creds.StateOK || a.Passthrough != "" || b.Token != creds.StateOK || b.Passthrough != "" {
		t.Fatalf("A=%+v B=%+v, want ok and no passthrough", a, b)
	}
	if c.Token != creds.StateOK || c.Passthrough != "no serving account" {
		t.Fatalf("C=%+v, want ok and its other reason kept", c)
	}
}

// A rotation-off account marked passthrough needs-login, whose login then
// advances, reads ok with no remote traffic at all: the hook is the only
// thing that ran.
func TestReloginHookClearsTheMarksWithoutAnyTraffic(t *testing.T) {
	sink := sinkWith(t, status.Account{Name: "A", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "token needs-login"})
	st := store.State{Accounts: []store.Account{{Name: "A", Dir: "/slots/A", LoggedInAt: markT0.Add(time.Hour)}}}
	var invalidated []string
	hook := newReloginHook(func(dir string) { invalidated = append(invalidated, dir) },
		func() (store.State, error) { return st, nil }, sink, nil, func() time.Time { return markT0.Add(time.Hour) })
	hook("/slots/A")
	if len(invalidated) != 1 || invalidated[0] != "/slots/A" {
		t.Fatalf("invalidated %v", invalidated)
	}
	if a := rowOf(sink, "A"); a.Token != creds.StateOK || a.Passthrough != "" {
		t.Fatalf("A = %+v, want ok and no passthrough", a)
	}
}

// A poll that parses clears it too (the poller's OnOK is recordRecovered).
func TestAPollSuccessClearsATokenPassthrough(t *testing.T) {
	sink := sinkWith(t, status.Account{Name: "A", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "token needs-login"})
	recordRecovered(sink, nil, "A", markT0.Add(time.Minute))
	if a := rowOf(sink, "A"); a.Token != creds.StateOK || a.Passthrough != "" {
		t.Fatalf("A = %+v", a)
	}
}

func TestDaemonStartDropsTheOldDaemonsTokenPassthroughs(t *testing.T) {
	sink := sinkWith(t,
		status.Account{Name: "A", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "token needs-login"},
		status.Account{Name: "B", Passthrough: "no serving account"},
	)
	sink.dropTokenPassthroughs()
	if a := rowOf(sink, "A"); a.Passthrough != "" {
		t.Fatalf("A = %+v, want no passthrough", a)
	}
	if b := rowOf(sink, "B"); b.Passthrough != "no serving account" {
		t.Fatalf("B = %+v, want its other reason kept", b)
	}
}

func TestStatusShowsNoNeedsLoginPassthroughOnceALaterLoginExists(t *testing.T) {
	mk := func(loggedIn time.Time) string {
		f := status.File{Version: status.Version, Accounts: []status.Account{{Name: "A", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "token needs-login"}}}
		st := store.State{Accounts: []store.Account{{Name: "A", LoggedInAt: loggedIn}}}
		dropSupersededNeedsLogin(&f, st)
		var buf bytes.Buffer
		renderStatus(&buf, f, markT0.Add(2*time.Hour))
		return buf.String()
	}
	if out := mk(markT0.Add(time.Hour)); bytes.Contains([]byte(out), []byte("passthrough")) {
		t.Fatalf("a later login still shows the mark:\n%s", out)
	}
	if out := mk(markT0.Add(-time.Hour)); !bytes.Contains([]byte(out), []byte("passthrough: token needs-login")) {
		t.Fatalf("an older login hid the mark:\n%s", out)
	}
}

// The wiring: a daemon that starts over a status.json holding the old
// daemon's token passthrough drops it.
func TestRunProxyDropsAnOldTokenPassthroughAtStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	writeNotifyStatus(t, home, status.Account{Name: "A", Token: creds.StateNeedsLogin, TokenAt: markT0, Passthrough: "token needs-login"})
	credsReadForTest = func(string) (creds.Token, error) {
		return creds.Token{AccessToken: "tok-A", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
	}
	t.Cleanup(func() { credsReadForTest = nil })
	stubDaemonNotifier(t)

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	<-errb.done
	// Shut the daemon down and join it: serve drains the sink last, so the
	// file is final once runProxyWithSignal has returned.
	sig <- os.Interrupt
	if code := <-codeCh; code != 0 {
		t.Fatalf("daemon exited %d; stderr:\n%s", code, errb.String())
	}
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Accounts {
		if a.Name == "A" && a.Passthrough != "" {
			t.Fatalf("passthrough = %q after the daemon started, want none", a.Passthrough)
		}
	}
}
