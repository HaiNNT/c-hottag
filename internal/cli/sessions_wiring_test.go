package cli

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

const (
	sidOne = "11111111111111111111111111111111"
	sidTwo = "22222222222222222222222222222222"
)

func sessionChooser(t *testing.T) *chooser {
	t.Helper()
	state := func() (store.State, error) {
		return store.State{Serving: "A", Accounts: []store.Account{{Name: "A", Dir: "/slots/A"}}}, nil
	}
	sel := selector.New(selector.Config{
		State:  state,
		Tokens: fakeTokens{tok: "tok-secret", st: creds.Status{State: creds.StateOK}, ok: true},
		Owners: fakeOwners{},
	})
	return newDaemonChooser(sel, nil, nil, state, nil)
}

func TestChooserReportsTheIdentityFromContextToTheTracker(t *testing.T) {
	c := sessionChooser(t)
	id := proxy.Identity{Caller: proxyauth.Caller{Pool: "default", SID: sidOne}, NativeID: "native-1", Inference: true}
	ctx := proxy.ContextWithIdentityForTest(context.Background(), id)
	if _, _, _, ok := c.Choose(ctx, router.Decision{Class: router.Serving}, ""); !ok {
		t.Fatal("Choose declined")
	}
	snap := c.tracker.Snapshot()
	if len(snap) != 1 || snap[0].SID != sidOne || snap[0].Pool != "default" || snap[0].Account != "A" ||
		snap[0].Requests != 1 || snap[0].Conversations != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestChooserNeverTracksWithoutAnIdentity(t *testing.T) {
	c := sessionChooser(t)
	if _, _, _, ok := c.Choose(context.Background(), router.Decision{Class: router.Serving}, ""); !ok {
		t.Fatal("Choose declined")
	}
	if got := c.tracker.Snapshot(); len(got) != 0 {
		t.Fatalf("an unidentified request was tracked: %+v", got)
	}
}

func TestChooserNeverTracksADeclinedChoice(t *testing.T) {
	c := sessionChooser(t)
	id := proxy.Identity{Caller: proxyauth.Caller{Pool: "default", SID: sidOne}, Inference: true}
	ctx := proxy.ContextWithIdentityForTest(context.Background(), id)
	if _, _, _, ok := c.Choose(ctx, router.Decision{Class: router.Untouched}, ""); ok {
		t.Fatal("Choose accepted an untouched decision")
	}
	if got := c.tracker.Snapshot(); len(got) != 0 {
		t.Fatalf("a declined choice was tracked: %+v", got)
	}
}

func writeLiveEntry(t *testing.T, home, sid string) {
	t.Helper()
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	// Put keys by pid; the test pid is alive, so one entry is enough.
	if err := reg.Put(session.Session{PID: os.Getpid(), Port: 47850, SID: sid, Pool: "default", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func newTestSink(t *testing.T, home string) *statusSink {
	t.Helper()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	return sink
}

func TestStampSessionsKeepsLiveForgetsDeadPastGraceAndWritesThem(t *testing.T) {
	home := t.TempDir()
	writeLiveEntry(t, home, sidOne)
	tr := sessions.NewTracker()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-sessions.Grace - time.Minute)
	tr.Seen(sidOne, "default", "A", true, "", old)                             // live: kept despite its age
	tr.Seen(sidTwo, "default", "B", true, "", old)                             // dead and past grace: forgotten
	tr.Seen("33333333333333333333333333333333", "default", "A", true, "", now) // dead, inside grace: kept
	sink := newTestSink(t, home)

	got := stampSessions(sink, tr, home, now)
	if len(got) != 2 || got[0].SID != sidOne || got[1].SID != "33333333333333333333333333333333" {
		t.Fatalf("snapshot = %+v", got)
	}
	sink.Close()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sessions) != 2 || f.Sessions[0].SID != sidOne {
		t.Fatalf("status.json sessions = %+v", f.Sessions)
	}
}

func TestStampSessionsRegistryErrorKeepsEverything(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "run"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := sessions.NewTracker()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr.Seen(sidTwo, "default", "B", true, "", now.Add(-time.Hour))
	sink := newTestSink(t, home)
	got := stampSessions(sink, tr, home, now)
	if len(got) != 1 {
		t.Fatalf("a registry error dropped a session: %+v", got)
	}
}

func TestSetSessionsWritesOnlyOnChange(t *testing.T) {
	home := t.TempDir()
	sink := newTestSink(t, home)
	wrote := make(chan struct{}, 4)
	sink.write = func(string, []byte) error { wrote <- struct{}{}; return nil }
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := []sessions.Activity{{SID: sidOne, Pool: "default", Account: "A", LastSeen: now, Requests: 1}}
	sink.setSessions(a)
	<-wrote // the writer took the document: pending is clear again
	sink.setSessions(append([]sessions.Activity(nil), a...))
	sink.mu.Lock()
	queued := sink.pending != nil
	sink.mu.Unlock()
	if queued {
		t.Fatal("an unchanged snapshot queued a write")
	}
	sink.setSessions(nil)
	<-wrote // a changed one writes
}

// A clean stop must not leave the stopped daemon's per-session accounts in
// status.json for `status` to join with live claude processes.
func TestClearDaemonClearsSessions(t *testing.T) {
	home := t.TempDir()
	sink := newTestSink(t, home)
	sink.setDaemon(47850, 0, 0, 0, 0, time.Now())
	sink.setSessions([]sessions.Activity{{SID: sidOne, Pool: "default", Account: "A", LastSeen: time.Now(), Requests: 1}})
	sink.clearDaemon()
	sink.Close()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sessions) != 0 {
		t.Fatalf("status.json sessions after a clean stop = %+v", f.Sessions)
	}
}

func TestRosterTickWritesSessionsToStatusJSON(t *testing.T) {
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
	tr := sessions.NewTracker()
	tr.Seen(sidOne, "default", "A", true, "", time.Now())
	stderr := newSyncBuf()
	tick := make(chan time.Time)
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard, Stderr: stderr, Listen: "127.0.0.1:0", Home: dir,
			Cache: cache, Sink: sink, Tokens: tokens.New(tokens.Config{}), Owners: own,
			Chooser: &chooser{tracker: tr}, RosterTick: tick, RosterProcessed: processed,
		})
	}()
	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}
	select {
	case tick <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never consumed the tick")
	}
	deadline := time.After(5 * time.Second)
	for found := false; !found; {
		select {
		case f := <-snapshots:
			found = len(f.Sessions) == 1 && f.Sessions[0].SID == sidOne
		case <-deadline:
			t.Fatal("no status.json write carried the session")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// The real proxy's Identity, through the real chooser and tracker, to
// status.json: a CONNECT carrying a session credential, one inference
// request through the fake backend, a roster tick, then the session shows
// with its sid and the account that served it.
func TestSessionCredentialThroughTheDaemonReachesStatusJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wireFakeUpstream(t, up, upCA)
	ticks := injectRosterTicks(t)

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
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	addr := listenAddrFromStderr(t, errb.String())

	secret, err := proxyauth.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(secret.SessionProxyURL(addr, proxyauth.DefaultPool, sidOne))
	if err != nil || u.User == nil {
		t.Fatalf("session proxy URL: %v", err)
	}
	pw, _ := u.User.Password()
	hdr := "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))

	tlsConn, br := mitmConnAs(t, home, addr, hdr)
	if _, err := io.WriteString(tlsConn, "POST /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Length: 2\r\nAuthorization: Bearer sk-ant-oat01-client-owned\r\n\r\n{}"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the proxied response failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied response status = %d, want 200", resp.StatusCode)
	}

	ticks.tick("sessions")
	// The sink writes off the tick's path, so wait for the document itself.
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := status.Load(status.Path(home))
		if err == nil && len(f.Sessions) == 1 {
			if f.Sessions[0].SID != sidOne || f.Sessions[0].Account != "A" {
				t.Fatalf("status.json sessions = %+v, want sid %s served by A", f.Sessions, sidOne)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status.json never carried the session: %+v, %v", f.Sessions, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
