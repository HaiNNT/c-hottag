package cli

// Shared helpers for the daemon verb tests (M1d-c). Created by the scaffold
// task and read-only for the tasks that fill in the verbs: those run in
// parallel worktrees, and one package's test files compile together.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/shim"
)

// closedPort returns a loopback port where no daemon ever answers. Every
// daemon verb test writes state.json with a test port first. With no
// state.json the port is 47821, and the verbs' health probe would dial the
// developer's real daemon.
//
// The port stays reserved for the whole test: a listener holds it and drops
// every connection unanswered, so a probe fails as it would on a closed port.
// Releasing it at once (as this helper used to) let another package's test,
// running in parallel, bind the same port and answer as a chottag daemon
// (CI, linux/arm64, v0.8.2).
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// fakeRecord is a lock record for a daemon process that does not exist.
// Every test that uses one stubs signalFn completely, so its pid is never
// signalled.
func fakeRecord(pid int) daemonlock.Record {
	return daemonlock.Record{PID: pid, Started: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
}

// holdLock takes home's daemon lock in THIS process, with rec as its
// record: a stand-in for a running daemon with no process behind it. flock
// belongs to the open file description, so Inspect (a second descriptor,
// even in this process) sees it held. release is idempotent and also runs
// at cleanup.
func holdLock(t *testing.T, home string, rec daemonlock.Record) (release func()) {
	t.Helper()
	unlock, err := daemonlock.Acquire(home, rec)
	if err != nil {
		t.Fatalf("holdLock: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { unlock() }) }
	t.Cleanup(release)
	return release
}

type sentSignal struct {
	pid int
	sig syscall.Signal
}

type signalLog struct {
	mu   sync.Mutex
	sent []sentSignal
}

func (l *signalLog) list() []sentSignal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sentSignal(nil), l.sent...)
}

// stubSignals replaces signalFn for this test, so nothing is ever
// delivered. Each call is recorded, then handed to onSignal, which is where
// a test plays the daemon's part (releasing the lock on SIGTERM, say). A
// nil onSignal means "succeed and do nothing". Restores whatever was
// installed before, which is TestMain's panicking default.
func stubSignals(t *testing.T, onSignal func(pid int, sig syscall.Signal) error) *signalLog {
	t.Helper()
	l := &signalLog{}
	restore := SetSignalForTest(func(pid int, sig syscall.Signal) error {
		l.mu.Lock()
		l.sent = append(l.sent, sentSignal{pid, sig})
		l.mu.Unlock()
		if onSignal == nil {
			return nil
		}
		return onSignal(pid, sig)
	})
	t.Cleanup(restore)
	return l
}

// stubSpawn installs fn as shim's spawnFn for this test. The exec seam stays
// a panicking tripwire. Restores whatever was installed before, which is
// TestMain's panicking defaults.
func stubSpawn(t *testing.T, fn func(exe, home, upstream string) error) {
	t.Helper()
	restore := shim.SetSeamsForTest(
		func(string, []string, []string) error { panic("shim.execFn reached from a daemon verb test") },
		fn,
	)
	t.Cleanup(restore)
}

// healthDaemon stands in for a daemon's health endpoint on an ephemeral
// loopback port. It answers as chottag only while up is true, and serves no
// pid (spec §4.8: nothing reads it).
type healthDaemon struct {
	up   atomic.Bool
	port int
}

func newHealthDaemon(t *testing.T, up bool, upstream string) *healthDaemon {
	t.Helper()
	d := &healthDaemon{}
	d.up.Store(up)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxy.HealthPath || !d.up.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(proxy.Health{Chottag: true, Version: "test", Upstream: upstream})
	}))
	t.Cleanup(srv.Close)
	d.port = srv.Listener.Addr().(*net.TCPAddr).Port
	return d
}

// --- a real process the test starts itself ---------------------------------

const (
	helperModeEnv = "CHOTTAG_TEST_HELPER_MODE"
	// Not CHOTTAG_HOME: TestMain unsets that before any test runs,
	// including in a re-exec'd helper.
	helperHomeEnv = "CHOTTAG_TEST_HELPER_HOME"
	helperReady   = "chottag-test-helper-ready"
)

// TestDaemonVerbHelperProcess is not a test. It skips unless
// startHelperProcess re-execs this test binary with helperModeEnv set.
//   - "hold": take the daemon lock, like a running daemon, and exit cleanly
//     on SIGTERM, like the daemon's graceful shutdown.
//   - "hold-ignore-term": take the lock and ignore SIGTERM, so only SIGKILL
//     ends it.
//   - "idle": hold nothing, and exit on SIGTERM. It is a live pid for the
//     session registry.
//
// Every mode prints helperReady once set up, and gives up after 60s so a
// parent that lost track of it never leaves it running.
func TestDaemonVerbHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process for the daemon verb tests: runs only when startHelperProcess re-execs this binary")
	}
	switch mode {
	case "hold", "hold-ignore-term":
		if _, err := daemonlock.Acquire(os.Getenv(helperHomeEnv), daemonlock.Current()); err != nil {
			fmt.Println("helper:", err)
			os.Exit(1)
		}
	case "idle":
	default:
		fmt.Println("helper: unknown mode", mode)
		os.Exit(1)
	}
	term := make(chan os.Signal, 1)
	if mode == "hold-ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(term, syscall.SIGTERM)
	}
	fmt.Println(helperReady)
	select {
	case <-term:
	case <-time.After(60 * time.Second):
	}
}

// readyWriter is a helper's stdout and stderr. It closes ready once the
// helper prints helperReady.
type readyWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	once  sync.Once
	ready chan struct{}
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), helperReady+"\n") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func (w *readyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type helperProcess struct {
	cmd  *exec.Cmd
	out  *readyWriter
	done chan struct{}
	err  error // cmd.Wait's result; read only after done is closed
}

func (h *helperProcess) pid() int { return h.cmd.Process.Pid }

// wait returns the helper's exit error once it has exited.
func (h *helperProcess) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-h.done:
		return h.err
	case <-time.After(30 * time.Second):
		t.Fatalf("helper pid %d did not exit within 30s:\n%s", h.pid(), h.out.String())
		return nil
	}
}

// startHelperProcess re-execs this test binary as TestDaemonVerbHelperProcess
// and returns once the helper is ready. The helper is this test's own
// child: it is the only process any test here may really signal, and
// cleanup kills it if it is still running. Supervisor markers are stripped
// from its environment, so the record it writes names no supervisor.
func startHelperProcess(t *testing.T, home, mode string) *helperProcess {
	t.Helper()
	out := &readyWriter{ready: make(chan struct{})}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonVerbHelperProcess$", "-test.count=1")
	cmd.Env = append(withoutEnv(os.Environ(), "INVOCATION_ID", "SYSTEMD_EXEC_PID", "XPC_SERVICE_NAME", helperModeEnv, helperHomeEnv),
		helperModeEnv+"="+mode, helperHomeEnv+"="+home)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helperProcess{cmd: cmd, out: out, done: make(chan struct{})}
	go func() { h.err = cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			cmd.Process.Kill() // our own child
			<-h.done
		}
	})
	select {
	case <-out.ready:
	case <-h.done:
		t.Fatalf("helper (%s) exited before it was ready: %v\n%s", mode, h.err, out.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("helper (%s) not ready within 30s:\n%s", mode, out.String())
	}
	return h
}

// withoutEnv returns a copy of env without any entry for keys.
func withoutEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}
