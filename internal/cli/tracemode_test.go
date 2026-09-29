package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/rotate"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

const claudeUA = "claude-cli/2.1.282 (external, cli)"

func setTraceWindow(t *testing.T, home string, until time.Time) {
	t.Helper()
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error { st.SetTraceUntil(until); return nil }); err != nil {
		t.Fatal(err)
	}
}

// traceDaemon is the daemon's trace side over a fresh home: a real
// store.Cache over state.json and a real status sink. A non-zero until
// opens a trace window in state.json first.
func traceDaemon(t *testing.T, until time.Time) (string, *daemonTrace, *statusSink) {
	t.Helper()
	home := t.TempDir()
	if !until.IsZero() {
		setTraceWindow(t, home, until)
	}
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	dt := newDaemonTrace(home, store.NewCache(store.Store{Dir: home}).State, sink)
	t.Cleanup(func() { dt.Close() })
	return home, dt, sink
}

func loadLastTraced(t *testing.T, home string) (status.TracedVersion, bool) {
	t.Helper()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	return f.LastTraced()
}

func traceOKUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
}

// tracedGet sends one GET with ua and waits for the nth req record in
// proxy.jsonl. A traced record reaches trace.jsonl first (emit), so it is
// there too by then.
func tracedGet(t *testing.T, h *proxytest.Harness, ua string, n int) {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	req.Header.Set("User-Agent", ua)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	h.Records(t, "req", n)
}

func TestDaemonTraceLogIsOpenedLazily(t *testing.T) {
	home, dt, _ := traceDaemon(t, time.Time{})
	path := filepath.Join(home, "trace.jsonl")
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("trace.jsonl exists before any traced record (stat: %v)", err)
	}
	if err := dt.log.Write(tracelog.Record{Kind: "mark", Mark: "x"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("after the first record: %v, %v; want a 0600 file", fi, err)
	}
	if err := dt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dt.log.Write(tracelog.Record{Kind: "mark", Mark: "y"}); !errors.Is(err, rotate.ErrClosed) {
		t.Fatalf("write after Close = %v, want rotate.ErrClosed", err)
	}
	if err := dt.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestDaemonTraceCloseBeforeAnyRecordCreatesNothing(t *testing.T) {
	home, dt, _ := traceDaemon(t, time.Time{})
	if err := dt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "trace.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Close created trace.jsonl (stat: %v)", err)
	}
}

// TestDaemonTraceLogRotatesAtItsCap: F68/F75's lesson, with F73's
// arithmetic. Each record below marshals to exactly 1053 bytes with its
// newline (checked first):
// {"t":"2026-09-25T10:40:00Z","kind":"mark","mark":"<1000 x>"}
// is 50 bytes of framing before the text, 1000 of text, then `"}` and a
// newline.
// 8 MiB = 8,388,608 bytes holds 7966 such lines (7966*1053 = 8,388,198),
// so rotation k happens at line 7966k+1: lines 7967, 15933, 23899 and
// 31865. 34,000 lines is past the 4th (31,865) and short of the 5th
// (39,831), so four rotations happen. With Keep 2, only .1 and .2 remain;
// .3 would exist with any Keep of 3 or more.
func TestDaemonTraceLogRotatesAtItsCap(t *testing.T) {
	home, dt, _ := traceDaemon(t, time.Time{})
	rec := tracelog.Record{T: time.Date(2026, 9, 25, 10, 40, 0, 0, time.UTC), Kind: "mark", Mark: strings.Repeat("x", 1000)}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(b)+1 != 1053 {
		t.Fatalf("a record is %d bytes, the arithmetic above assumes 1053", len(b)+1)
	}
	for i := 0; i < 34000; i++ {
		if err := dt.log.Write(rec); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	path := filepath.Join(home, "trace.jsonl")
	for _, p := range []string{path, path + ".1", path + ".2"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > daemonTraceMaxBytes {
			t.Fatalf("%s: %v, %v; want it present and at most %d bytes", p, fi, err, daemonTraceMaxBytes)
		}
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s.3 exists (stat: %v): Keep must be 2", path, err)
	}
	if daemonTraceMaxBytes != 8<<20 || daemonTraceKeep != 2 {
		t.Fatalf("bounds %d/%d, want 8 MiB and 2 (M2c spec §3)", daemonTraceMaxBytes, daemonTraceKeep)
	}
}

// TestDaemonTracingFollowsStateWithoutARestart is Review Focus 1: the
// switch is read per request, and a window ends by the clock alone.
func TestDaemonTracingFollowsStateWithoutARestart(t *testing.T) {
	home, dt, _ := traceDaemon(t, time.Time{})
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	dt.now = func() time.Time { return now }
	if dt.tracing() {
		t.Fatal("tracing with no window")
	}
	setTraceWindow(t, home, now.Add(time.Hour))
	if !dt.tracing() {
		t.Fatal("trace on in state.json not seen without a restart")
	}
	now = now.Add(59 * time.Minute)
	if !dt.tracing() {
		t.Fatal("off one minute before until")
	}
	now = now.Add(time.Minute) // exactly until: the window has ended
	if dt.tracing() {
		t.Fatal("still tracing at until: the window must end by itself")
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-30 * time.Minute)
	if dt.tracing() {
		t.Fatal("an unreadable state.json reads as tracing, want off (plan ruling 6)")
	}
}

func TestDaemonTraceStampsLastTracedOnceForAVersion(t *testing.T) {
	t1 := time.Date(2026, 9, 25, 10, 40, 0, 0, time.Local)
	home, dt, sink := traceDaemon(t, t1.Add(time.Hour))
	var clock atomic.Pointer[time.Time]
	clock.Store(&t1)
	dt.now = func() time.Time { return *clock.Load() }
	h := proxytest.Start(t, traceOKUpstream(), proxytest.Options{Tracing: dt.tracing, TraceLog: dt.log, OnClaudeVersion: dt.onClaudeVersion})
	tracedGet(t, h, claudeUA, 1)
	t2 := t1.Add(10 * time.Minute)
	clock.Store(&t2)
	tracedGet(t, h, claudeUA, 2)
	sink.Close()
	lt, ok := loadLastTraced(t, home)
	if !ok || lt.ClaudeVersion != "2.1.282" || !lt.At.Equal(t1) {
		t.Fatalf("lastTraced = %+v, %v; want 2.1.282 at the FIRST traced request (%v)", lt, ok, t1)
	}
	recs, err := tracelog.ReadAll(filepath.Join(home, "trace.jsonl"))
	if err != nil || len(recs) != 2 || recs[0].RespShape == nil {
		t.Fatalf("trace.jsonl = %+v, %v; want 2 shaped records", recs, err)
	}
}

func TestDaemonTraceALaterVersionReplacesThePair(t *testing.T) {
	t1 := time.Date(2026, 9, 25, 10, 40, 0, 0, time.Local)
	home, dt, sink := traceDaemon(t, t1.Add(time.Hour))
	var clock atomic.Pointer[time.Time]
	clock.Store(&t1)
	dt.now = func() time.Time { return *clock.Load() }
	h := proxytest.Start(t, traceOKUpstream(), proxytest.Options{Tracing: dt.tracing, TraceLog: dt.log, OnClaudeVersion: dt.onClaudeVersion})
	tracedGet(t, h, claudeUA, 1)
	t2 := t1.Add(5 * time.Minute)
	clock.Store(&t2)
	tracedGet(t, h, "claude-cli/2.1.283 (external, cli)", 2)
	sink.Close()
	if lt, ok := loadLastTraced(t, home); !ok || lt.ClaudeVersion != "2.1.283" || !lt.At.Equal(t2) {
		t.Fatalf("lastTraced = %+v, want 2.1.283 at %v", lt, t2)
	}
}

// TestDaemonTraceIgnoresForeignOrMalformedUserAgents is Review Focus 3.
func TestDaemonTraceIgnoresForeignOrMalformedUserAgents(t *testing.T) {
	home, dt, sink := traceDaemon(t, time.Now().Add(time.Hour))
	h := proxytest.Start(t, traceOKUpstream(), proxytest.Options{Tracing: dt.tracing, TraceLog: dt.log, OnClaudeVersion: dt.onClaudeVersion})
	uas := []string{
		"claude-cli/2.1",
		"evil/2.1.282 (external, cli)",
		"claude-cli/2.1.282 (" + strings.Repeat("x", 10<<10) + ")", // 10 KB
		"claude-cli/" + strings.Repeat("9", 5000) + ".1.1 (x)",
	}
	for i, ua := range uas {
		tracedGet(t, h, ua, i+1)
	}
	sink.Close()
	if lt, ok := loadLastTraced(t, home); ok {
		t.Fatalf("lastTraced = %+v from a foreign or malformed User-Agent", lt)
	}
}

func TestAnUntracedRequestNeverStampsOrOpensTheTraceLog(t *testing.T) {
	home, dt, sink := traceDaemon(t, time.Time{})
	h := proxytest.Start(t, traceOKUpstream(), proxytest.Options{Tracing: dt.tracing, TraceLog: dt.log, OnClaudeVersion: dt.onClaudeVersion})
	tracedGet(t, h, claudeUA, 1)
	sink.Close()
	if lt, ok := loadLastTraced(t, home); ok {
		t.Fatalf("an untraced request stamped %+v", lt)
	}
	if _, err := os.Stat(filepath.Join(home, "trace.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("trace.jsonl exists after an untraced request (stat: %v)", err)
	}
}

// TestAnUnwritableTraceLogNeverFailsTheRequest is Review Focus 4. It removes
// the directory occupying trace.jsonl's path after the first request, then
// asserts the second traced record lands — proving the failed open is
// retried, not latched (PF9).
func TestAnUnwritableTraceLogNeverFailsTheRequest(t *testing.T) {
	home, dt, _ := traceDaemon(t, time.Now().Add(time.Hour))
	path := filepath.Join(home, "trace.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil { // a directory where the log goes
		t.Fatal(err)
	}
	var logErrs atomic.Int32
	h := proxytest.Start(t, traceOKUpstream(), proxytest.Options{
		Tracing: dt.tracing, TraceLog: dt.log, OnClaudeVersion: dt.onClaudeVersion,
		OnLogError: func(error) { logErrs.Add(1) },
	})
	tracedGet(t, h, claudeUA, 1) // waits for proxy.jsonl's record: it was still written
	if got := logErrs.Load(); got != 1 {
		t.Fatalf("OnLogError called %d times after the first request, want 1", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tracedGet(t, h, claudeUA, 2) // the next traced record retries the open
	if got := logErrs.Load(); got != 1 {
		t.Fatalf("OnLogError called %d times, want still 1: the retried open must succeed", got)
	}
	recs, err := tracelog.ReadAll(path)
	if err != nil || len(recs) != 1 || recs[0].Kind != "req" {
		t.Fatalf("trace.jsonl = %+v, %v; want the one record that landed after the retry", recs, err)
	}
}

func TestDaemonTraceWireSetsTheThreeTraceFields(t *testing.T) {
	home, dt, sink := traceDaemon(t, time.Time{})
	cfg := dt.wire(proxy.Config{Version: "v-test"})
	if cfg.TraceLog != dt.log || cfg.Tracing == nil || cfg.OnClaudeVersion == nil || cfg.Version != "v-test" {
		t.Fatalf("wire = %+v", cfg)
	}
	if cfg.Tracing() {
		t.Fatal("wired Tracing true with no window")
	}
	setTraceWindow(t, home, time.Now().Add(time.Hour))
	if !cfg.Tracing() {
		t.Fatal("wired Tracing does not read state.json")
	}
	cfg.OnClaudeVersion("2.1.282")
	sink.Close()
	if lt, ok := loadLastTraced(t, home); !ok || lt.ClaudeVersion != "2.1.282" {
		t.Fatalf("wired OnClaudeVersion did not reach status.json: %+v", lt)
	}
}

// TestRunProxyWiresTheDaemonTrace pins `cfg = dt.wire(cfg)` in
// runProxyWithSignal (T5 M3): the *daemonTrace it builds must be the one
// whose TraceLog, Tracing and OnClaudeVersion actually reach the live
// daemon's cfg, not just a value constructed and dropped.
// TestDaemonTraceWireSetsTheThreeTraceFields already proves wire() itself
// in isolation; this drives the production path
// (TestRunProxyWiresThePoller's pattern: runProxyWithSignal on
// 127.0.0.1:0) and proves the same identity end to end.
//
// A trace window is armed in state.json before the daemon ever starts, so
// dt.tracing() already reads true. The one request sent is absolute-form
// (proxy.go's r.URL.IsAbs() case), which reaches forward() exactly like a
// MITM'd request does, without needing a CONNECT/TLS trust dance or any
// real network host — the upstream is a local httptest.Server. A traced
// request through the live daemon must produce both a trace.jsonl record
// (TraceLog + Tracing wired) and a status.json lastTraced stamp
// (OnClaudeVersion wired). Deleting the wire line zeroes all three cfg
// fields at once and leaves both files untouched, failing this test.
func TestRunProxyWiresTheDaemonTrace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	setTraceWindow(t, home, time.Now().Add(time.Hour))

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	got := make(chan *daemonTrace, 1)
	prev := newDaemonTrace
	t.Cleanup(func() { newDaemonTrace = prev })
	newDaemonTrace = func(home string, state func() (store.State, error), sink *statusSink) *daemonTrace {
		dt := prev(home, state, sink)
		got <- dt
		return dt
	}

	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(60 * time.Second): // hang guard only (F185)
			t.Error("daemon did not shut down during cleanup")
		}
	})

	var dt *daemonTrace
	select {
	case dt = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("runProxyWithSignal never built the daemon trace")
	}
	if !dt.tracing() {
		t.Fatal("the daemon trace's own state read does not see the window this test armed")
	}

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	const marker = "chottag proxy listening on "
	errs := errb.String()
	i := strings.Index(errs, marker)
	if i < 0 {
		t.Fatalf("stderr = %q, want it to name the listen address", errs)
	}
	rest := errs[i+len(marker):]
	addr := rest[:strings.IndexByte(rest, ',')]

	secret, err := proxyauth.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	upHost := strings.TrimPrefix(up.URL, "http://")
	fmt.Fprintf(c, "GET %s/v1/models HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nProxy-Authorization: %s\r\n\r\n", up.URL, upHost, claudeUA, proxyAuthHeaderForTest(t, secret))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	tracePath := filepath.Join(home, "trace.jsonl")
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, rerr := os.ReadFile(tracePath)
		if rerr == nil && strings.TrimSpace(string(b)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace.jsonl never got a record (err=%v): cfg.TraceLog/Tracing did not reach the live daemon", rerr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	deadline = time.Now().Add(10 * time.Second)
	for {
		if lt, ok := loadLastTraced(t, home); ok && lt.ClaudeVersion == "2.1.282" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status.json's lastTraced was never stamped: cfg.OnClaudeVersion did not reach the live daemon")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStatusJSONShowsTheLastTracedVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink.noteTracedVersion("2.1.282", time.Date(2026, 9, 25, 10, 40, 0, 0, time.UTC))
	sink.Close()
	code, out, _ := runChottag(t, "--json", "status")
	if code != exit.OK {
		t.Fatalf("status --json = %d", code)
	}
	tr, _ := decodeOneDocument(t, out)["trace"].(map[string]any)
	lt, _ := tr["lastTraced"].(map[string]any)
	if lt["claudeVersion"] != "2.1.282" || lt["at"] != "2026-09-25T10:40:00Z" {
		t.Fatalf("status --json trace = %v", tr)
	}
}

func TestLastTracedSurvivesARestartAndAStop(t *testing.T) {
	home := t.TempDir()
	s1, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	s1.noteTracedVersion("2.1.282", time.Date(2026, 9, 25, 10, 40, 0, 0, time.UTC))
	s1.setDaemon(47850, 0, 0, 0, 0, time.Now())
	s1.clearDaemon()
	s1.Close()
	s2, err := newStatusSink(home, nil) // the next daemon generation loads the file
	if err != nil {
		t.Fatal(err)
	}
	s2.setDaemon(47850, 0, 0, 0, 0, time.Now())
	s2.Close()
	if lt, ok := loadLastTraced(t, home); !ok || lt.ClaudeVersion != "2.1.282" {
		t.Fatalf("lastTraced after a restart = %+v, %v", lt, ok)
	}
}

func TestTraceMarkAppendsToTheDaemonsTraceLog(t *testing.T) {
	home, dt, _ := traceDaemon(t, time.Time{})
	t.Setenv("CHOTTAG_HOME", home)
	if err := dt.log.Write(tracelog.Record{Kind: "req", Path: "/a"}); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runChottag(t, "trace", "mark", "between"); code != exit.OK {
		t.Fatalf("trace mark = %d; %q", code, errs)
	}
	if err := dt.log.Write(tracelog.Record{Kind: "req", Path: "/b"}); err != nil {
		t.Fatal(err)
	}
	recs, err := tracelog.ReadAll(filepath.Join(home, "trace.jsonl"))
	if err != nil || len(recs) != 3 || recs[0].Path != "/a" || recs[1].Mark != "between" || recs[2].Path != "/b" {
		t.Fatalf("trace.jsonl = %+v, %v; want req, mark, req in order", recs, err)
	}
}

// TestNoteTracedVersionWritesOnceForAVersion pins PF14: the first traced
// request of a version costs one status.json write, and a second request
// of the SAME version costs none, because SetLastTraced refuses the
// unchanged pair and noteTracedVersion queues only on a reported change.
func TestNoteTracedVersionWritesOnceForAVersion(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	waitFor, assertNone := newWriteWatcher(t, sink)
	sink.noteTracedVersion("2.1.282", time.Date(2026, 9, 25, 10, 40, 0, 0, time.UTC))
	waitFor()
	sink.noteTracedVersion("2.1.282", time.Date(2026, 9, 25, 10, 41, 0, 0, time.UTC))
	assertNone()
}
