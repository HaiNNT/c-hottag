package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

// TestServeStopsOnContextCancelAndDrains proves the shutdown path both
// stops serving and reaches the sink, so the last observation is not lost
// to the coalescing window (contract 4).
func TestServeStopsOnContextCancelAndDrains(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.ensureAccounts([]string{"A"})
	sink.setPassthrough("A", "token stale")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {}, sink)
	}()

	// Give the server a moment to be listening, then stop it.
	waitForListening(t, ln.Addr().String())
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range f.Accounts {
		if a.Name == "A" && a.Passthrough == "token stale" {
			found = true
		}
	}
	if !found {
		t.Error("shutdown did not drain the status sink")
	}
}

// TestServeStopsAcceptingNewConnectionsAfterContextCancel closes a gap
// TestServeStopsOnContextCancelAndDrains cannot see: that test only checks
// that serve returns and drains the sink, so a serve whose shutdown path
// skips srv.Shutdown entirely (leaving the listener open and every future
// connection accepted by the still-running srv.Serve goroutine) passes it
// unchanged — confirmed by mutation. This test dials the address again
// after serve has returned and requires the dial to fail, proving the
// listener itself was actually closed.
func TestServeStopsAcceptingNewConnectionsAfterContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {}, sink)
	}()

	waitForListening(t, addr)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Fatal("serve is still accepting connections on its listener after it returned")
	}
}

// TestServeDrainsTheSinkAfterClosingTunnelsNotBefore pins M24: serve's own
// doc comment says the sink drains "last — after which nothing can queue
// another write", but nothing enforced closeConns() running before
// sink.Close() — swapping their order survived a fully green suite.
// closeConns here queues a write of its own, standing in for anything
// that might still need to reach the sink as the tunnels close; per F4's
// fix-round-1 finding, a write queued after sink.Close() has already run
// is silently dropped (statusSink.queueLocked returns early once closed),
// so that write reaching disk is only possible if closeConns ran BEFORE
// sink.Close() — proving the order, not just that both eventually happen.
func TestServeDrainsTheSinkAfterClosingTunnelsNotBefore(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.ensureAccounts([]string{"A"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int {
			sink.setPassthrough("A", "closing")
			return 0
		}, func() int { return 0 }, func() {}, sink)
	}()

	waitForListening(t, ln.Addr().String())
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough != "closing" {
		t.Fatalf("accounts = %+v, want closeConns' write to have reached disk — sink.Close() must run strictly after closeConns(), not before", f.Accounts)
	}
}

// TestShutdownClosesTunnels: a hijacked CONNECT is invisible to
// http.Server.Shutdown, so without an explicit close it would keep the
// process alive past the grace period.
func TestShutdownClosesTunnels(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}

	held := make(chan net.Conn, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		held <- c
		select {} // never returns, exactly like a live tunnel
	})

	var closed atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, h, func() int {
			closed.Store(true)
			if c := <-held; c != nil {
				c.Close()
			}
			return 1
		}, func() int { return 0 }, func() {}, sink)
	}()

	waitForListening(t, ln.Addr().String())
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	buf := make([]byte, 64)
	if _, err := c.Read(buf); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("shutdown hung on a hijacked tunnel")
	}
	if !closed.Load() {
		t.Error("shutdown did not close the tunnels")
	}
}

// TestServeClosesClientTunnelsOnShutdown pins F54's wiring: Task 1 gave the
// proxy the ability to end live MITM tunnels; serve must use it.
func TestServeClosesClientTunnelsOnShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	var closedTunnels, closedUpstreams int
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NotFoundHandler(),
			func() int { closedUpstreams++; return 0 },
			func() int { closedTunnels++; return 0 },
			func() {},
			sink)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
	if closedTunnels != 1 {
		t.Fatalf("closeTunnels called %d times, want 1: a live MITM tunnel would keep serving after shutdown (F54)", closedTunnels)
	}
	if closedUpstreams != 1 {
		t.Fatalf("closeConns called %d times, want 1", closedUpstreams)
	}
}

// TestServeClosesTheOwnerMapOnShutdown pins that the owner map's writer
// goroutine is drained at shutdown. Without it the newest owner document
// can be lost at exit — the durability property owners' write ordering
// exists to protect.
func TestServeClosesTheOwnerMapOnShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NotFoundHandler(),
			func() int { note("conns"); return 0 },
			func() int { note("tunnels"); return 0 },
			func() { note("owners") },
			sink)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"conns", "tunnels", "owners"}
	if len(order) != len(want) {
		t.Fatalf("shutdown steps = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("shutdown order = %v, want %v: owners must drain after tunnels stop producing writes", order, want)
		}
	}
}

// TestServeDrainsOwnersOnTheListenerErrorPath pins the OTHER exit path
// serve has: the <-errc branch, reached when srv.Serve itself returns a
// non-ErrServerClosed error rather than a graceful ctx cancel. Per serve's
// own doc comment, that branch never calls closeConns/closeTunnels — but
// closeOwners must still run there, and before sink.Close(), or the newest
// owner document is lost precisely when the listener has already failed
// (the inverse of the durability property Close exists to protect).
// Closing the listener out from under srv.Serve is what forces that path,
// distinct from TestServeClosesTheOwnerMapOnShutdown above, which only
// exercises the graceful ctx-cancel path.
func TestServeDrainsOwnersOnTheListenerErrorPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NotFoundHandler(),
			func() int { note("conns"); return 0 },
			func() int { note("tunnels"); return 0 },
			func() { note("owners") },
			sink)
	}()

	waitForListening(t, ln.Addr().String())
	ln.Close() // srv.Serve now returns a non-ErrServerClosed error, taking the <-errc branch

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil on the listener-error path, want the Accept error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after its listener was closed out from under it")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "owners" {
		t.Fatalf("shutdown steps on the listener-error path = %v, want [owners]: closeConns/closeTunnels must not run there (serve's doc comment), but closeOwners must", order)
	}
}

// TestServeKeepsSinkLastAfterClosingOwners mirrors
// TestServeDrainsTheSinkAfterClosingTunnelsNotBefore, but for closeOwners
// specifically rather than closeConns: "sink stays last" is asserted at
// three places in this file's target, and swapping closeOwners() and
// sink.Close() survived all three until this review round (M1c4 Task 5,
// round 3) — this is the graceful-shutdown one.
func TestServeKeepsSinkLastAfterClosingOwners(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.ensureAccounts([]string{"A"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {
			sink.setPassthrough("A", "owners closed")
		}, sink)
	}()

	waitForListening(t, ln.Addr().String())
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough != "owners closed" {
		t.Fatalf("accounts = %+v, want closeOwners' write to have reached disk — sink.Close() must run strictly after closeOwners(), not before", f.Accounts)
	}
}

// TestServeKeepsSinkLastAfterClosingOwnersOnTheListenerErrorPath is
// TestServeKeepsSinkLastAfterClosingOwners' sibling for the <-errc branch
// (forced by closing the listener out from under srv.Serve), rather than
// the graceful ctx-cancel path.
func TestServeKeepsSinkLastAfterClosingOwnersOnTheListenerErrorPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.ensureAccounts([]string{"A"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {
			sink.setPassthrough("A", "owners closed")
		}, sink)
	}()

	waitForListening(t, ln.Addr().String())
	ln.Close() // srv.Serve now returns a non-ErrServerClosed error, taking the <-errc branch

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil on the listener-error path, want the Accept error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after its listener was closed out from under it")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough != "owners closed" {
		t.Fatalf("accounts = %+v, want closeOwners' write to have reached disk on the listener-error path too — sink.Close() must run strictly after closeOwners(), not before", f.Accounts)
	}
}

// TestServeClearsTheDaemonOnACleanShutdown pins fix round 2, item 1: with
// nothing to clear it, a clean shutdown left status.json carrying the last
// SetDaemon heartbeat, so `chottag own` and `chottag status --json` kept
// reporting a live daemon for up to status.DaemonStaleAfter (90s) after the
// process had already exited. Mutation: removing the sink.clearDaemon()
// call on this path must make this test fail.
func TestServeClearsTheDaemonOnACleanShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.setDaemon(47821, 0, 0, 0, 0, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {}, sink)
	}()

	waitForListening(t, ln.Addr().String())
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	f.DaemonRunningAt(time.Now())
	if f.Daemon != nil && f.Daemon.Running {
		t.Fatalf("Daemon = %+v after a clean shutdown, want it not running (no 90s wait)", f.Daemon)
	}
}

// TestServeClearsTheDaemonOnTheListenerErrorPath is
// TestServeClearsTheDaemonOnACleanShutdown's sibling for the <-errc branch:
// the fix's ruling is to clear on BOTH exit paths, since a listener failure
// exits the process too, and leaving a stale heartbeat there preserves the
// same bug for a user whose daemon just failed rather than stopped cleanly.
func TestServeClearsTheDaemonOnTheListenerErrorPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.setDaemon(47821, 0, 0, 0, 0, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {}, sink)
	}()

	waitForListening(t, ln.Addr().String())
	ln.Close() // srv.Serve now returns a non-ErrServerClosed error, taking the <-errc branch

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil on the listener-error path, want the Accept error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after its listener was closed out from under it")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	f.DaemonRunningAt(time.Now())
	if f.Daemon != nil && f.Daemon.Running {
		t.Fatalf("Daemon = %+v on the listener-error path, want it not running (no 90s wait)", f.Daemon)
	}
}
