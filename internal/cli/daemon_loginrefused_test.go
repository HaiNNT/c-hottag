package cli

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

type lrDaemon struct {
	home  string
	addr  string
	errb  *syncBuf
	rec   *recordingNotifier
	ticks *rosterTicks
}

// startLoginRefusedDaemon runs the real daemon with accounts C (serving) and B
// against a fake upstream that answers cStatus for C's bearer (loaded on every
// request, so a test can change it), 200 for B's and for Home's own.
func startLoginRefusedDaemon(t *testing.T, cStatus *atomic.Int32) *lrDaemon {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "C")
	addSlotAccount(t, home, "B")
	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-C" {
			w.WriteHeader(int(cStatus.Load()))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	wireFakeUpstream(t, up, upCA)
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &lrDaemon{home: home, rec: stubDaemonNotifier(t), errb: newSyncBuf()}
	d.ticks = injectRosterTicks(t)
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", "", "--claude", fakeClaude}, io.Discard, d.errb, nil, sig)
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
	case <-d.errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	d.addr = listenAddrFromStderr(t, d.errb.String())
	// The watcher's startup stamp is not a tick; the first tick call below
	// waits for it to take the channel, so nothing needs draining here.
	return d
}

// get sends one GET through the daemon's MITM tunnel with Claude Code's own
// bearer and returns the status the client saw.
func (d *lrDaemon) get(t *testing.T, path string) int {
	t.Helper()
	tlsConn, br := mitmConn(t, d.home, d.addr)
	if _, err := io.WriteString(tlsConn, "GET "+path+" HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-oat01-client-owned\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the proxied response failed: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// R201: C's login is refused on a fallback route and then on /v1/messages. The
// refusal is one account-wide login refusal, not a route problem: no "new
// route" notice, one serving-refused notice, the mark is set, and a tick with
// auto on moves serving to B.
func TestDaemonLoginRefusedIsNotRouteDriftAndLeavesRotation(t *testing.T) {
	var cStatus atomic.Int32
	cStatus.Store(http.StatusForbidden)
	d := startLoginRefusedDaemon(t, &cStatus)

	for _, path := range []string{"/api/claude_cli/bootstrap", "/v1/messages"} {
		if got := d.get(t, path); got != http.StatusOK {
			t.Fatalf("GET %s: client saw %d, want 200 (resent on Home)", path, got)
		}
	}
	d.ticks.tick("settle")

	const marked = "chottag: C's login is refused on serving requests (403); out of rotation until it answers again or you run: chottag login C"
	if !strings.Contains(d.errb.String(), marked) {
		t.Fatalf("daemon.log lacks the mark line %q:\n%s", marked, d.errb.String())
	}
	if n := strings.Count(d.errb.String(), "C's login is refused on serving requests"); n != 1 {
		t.Fatalf("mark line count = %d, want 1 per episode", n)
	}
	// Notices are FIFO: a route notice from the bootstrap refusal would
	// arrive before the serving-refused one from /v1/messages.
	select {
	case m := <-d.rec.got:
		if m.title != "chottag: C's login was refused (403)" {
			t.Fatalf("first notice = %+v, want the serving-refused notice (no route notice)", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no serving-refused notice arrived; stderr %q", d.errb.String())
	}
	if strings.Contains(d.errb.String(), "may belong to the remote account") {
		t.Fatalf("a login refusal was reported as an unclassified route:\n%s", d.errb.String())
	}

	d.ticks.tick("auto-switch")
	st, err := (store.Store{Dir: d.home}).Load()
	if err != nil || st.Serving != "B" {
		t.Fatalf("serving = %q (%v), want B after a tick with auto on", st.Serving, err)
	}
}

// R201: a 2xx on C's own login ends the episode: one "works again" line.
func TestDaemonLoginRefusedClearsOnA2xx(t *testing.T) {
	var cStatus atomic.Int32
	cStatus.Store(http.StatusForbidden)
	d := startLoginRefusedDaemon(t, &cStatus)

	d.get(t, "/v1/messages")
	if !strings.Contains(d.errb.String(), "C's login is refused on serving requests (403)") {
		t.Fatalf("C was not marked:\n%s", d.errb.String())
	}
	cStatus.Store(http.StatusOK)
	if got := d.get(t, "/v1/messages"); got != http.StatusOK {
		t.Fatalf("client saw %d, want 200", got)
	}
	// OnUsage runs inside ModifyResponse, before the client sees the response.
	if !strings.Contains(d.errb.String(), "chottag: C's login works again (refused since ") {
		t.Fatalf("no recovery line after a 2xx as C:\n%s", d.errb.String())
	}
}
