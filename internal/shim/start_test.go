package shim

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
)

// closedPort returns a loopback port with nothing listening on it. The
// kernel picks it, and it is released at once, so nothing fixed is ever
// bound.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// flippingHealthServer answers the health probe as chottag only once up is
// set. It stands in for a daemon that comes up some time after the spawn.
func flippingHealthServer(t *testing.T) (port int, up *atomic.Bool) {
	t.Helper()
	up = new(atomic.Bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxy.HealthPath || !up.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(proxy.Health{Chottag: true, Version: "test"})
	}))
	t.Cleanup(srv.Close)
	return mustPort(t, srv.URL), up
}

func TestSpawnUpstream(t *testing.T) {
	const port = 51234
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"corporate_proxy", []string{"HTTPS_PROXY=http://corp:8080"}, "http://corp:8080"},
		{"our_own_address", []string{"HTTPS_PROXY=http://127.0.0.1:" + strconv.Itoa(port)}, ""},
		{"none", nil, ""},
		{"another_loopback_port", []string{"HTTPS_PROXY=http://127.0.0.1:3128"}, "http://127.0.0.1:3128"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SpawnUpstream(c.env, port); got != c.want {
				t.Errorf("SpawnUpstream = %q, want %q", got, c.want)
			}
		})
	}
}

// The daemon comes up 150ms AFTER the spawn returns, so a StartDaemon that
// probed once instead of polling would report "not confirmed".
func TestStartDaemonSpawnsForHomeAndWaitsForHealth(t *testing.T) {
	port, up := flippingHealthServer(t)
	var calls int
	var gotHome, gotUpstream string
	restore := swapSpawn(func(exe, home, upstream string) error {
		calls++
		gotHome, gotUpstream = home, upstream
		time.AfterFunc(150*time.Millisecond, func() { up.Store(true) })
		return nil
	})
	defer restore()

	confirmed, h, err := StartDaemon("/tmp/some-chottag-home", port, []string{"HTTPS_PROXY=http://corp:8080"})
	if !confirmed || !h.Chottag || err != nil {
		t.Fatalf("StartDaemon = %v, %+v, %v; want confirmed with no spawn error", confirmed, h, err)
	}
	if calls != 1 || gotHome != "/tmp/some-chottag-home" || gotUpstream != "http://corp:8080" {
		t.Errorf("spawnFn calls=%d home=%q upstream=%q; want 1, the home, and the caller's HTTPS_PROXY", calls, gotHome, gotUpstream)
	}
}

func TestStartDaemonReportsNotConfirmedWhenTheDaemonNeverAnswers(t *testing.T) {
	port := closedPort(t)
	restore := swapSpawn(func(string, string, string) error { return nil })
	defer restore()

	start := time.Now()
	confirmed, _, err := StartDaemon(t.TempDir(), port, nil)
	if confirmed || err != nil {
		t.Fatalf("StartDaemon = %v, %v; want not confirmed and no spawn error", confirmed, err)
	}
	if d := time.Since(start); d < pollBudget/2 {
		t.Errorf("StartDaemon gave up after %s, want it to poll for about pollBudget (%s)", d, pollBudget)
	}
}

// Another `claude` launch may have started the daemon while our own spawn
// failed. The poll still runs, and the failure is reported alongside.
func TestStartDaemonStillPollsWhenTheSpawnFails(t *testing.T) {
	port, up := flippingHealthServer(t)
	up.Store(true)
	restore := swapSpawn(func(string, string, string) error { return errors.New("test: spawn refused") })
	defer restore()

	confirmed, _, err := StartDaemon(t.TempDir(), port, nil)
	if !confirmed || err == nil {
		t.Fatalf("StartDaemon = %v, %v; want confirmed AND the spawn error reported", confirmed, err)
	}
}

// Run never reaches this branch: ResolveClaude fails first when the
// executable path is unknown. StartDaemon is now also called directly by
// the daemon verbs, so the branch is pinned here.
func TestStartDaemonReportsAnExecutablePathFailureWithoutSpawning(t *testing.T) {
	orig := executablePath
	executablePath = func() (string, error) { return "", errors.New("test: no executable") }
	defer func() { executablePath = orig }()
	port, up := flippingHealthServer(t)
	up.Store(true)
	called := false
	restore := swapSpawn(func(string, string, string) error { called = true; return nil })
	defer restore()

	confirmed, _, err := StartDaemon(t.TempDir(), port, nil)
	if called {
		t.Error("spawnFn was called without an executable path")
	}
	if !confirmed || err == nil || err.Error() != "test: no executable" {
		t.Fatalf("StartDaemon = %v, %v; want confirmed (a daemon is already up) and the executablePath error", confirmed, err)
	}
}

// F130's third rule: TestMain's panicking spawnFn default must survive a
// stub-and-restore cycle through SetSeamsForTest, the setter internal/cli's
// tests use. (This package's own tests use swapSpawn, so nothing else
// exercises SetSeamsForTest's restore.) executablePath points at a missing
// file, so if the mutation this test catches is live, the real spawnDaemon
// fails with ENOENT instead of forking this test binary.
func TestSetSeamsForTestRestoresWhatWasInstalledBefore(t *testing.T) {
	orig := executablePath
	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "no-such-chottag"), nil }
	defer func() { executablePath = orig }()
	port := closedPort(t)

	t.Run("stubbed", func(t *testing.T) {
		restore := SetSeamsForTest(
			func(string, []string, []string) error { return nil },
			func(string, string, string) error { return nil },
		)
		t.Cleanup(restore)
	})

	defer func() {
		if recover() == nil {
			t.Error("spawnFn did not panic after a SetSeamsForTest stub was restored: the restore installed production's spawnDaemon instead of TestMain's panicking default")
		}
	}()
	StartDaemon(t.TempDir(), port, nil)
}

// F239: on macOS os.Executable returns the path the process was started
// by, so a shim started as bin/claude (a symlink to chottag) sees
// .../bin/claude. Spawning that as `daemon run` starts another shim, which
// spawns another: a fork chain. StartDaemon spawns the resolved binary.
func TestStartDaemonSpawnsTheResolvedBinaryNotTheClaudeLink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "versions", "chottag")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	orig := executablePath
	executablePath = func() (string, error) { return link, nil }
	defer func() { executablePath = orig }()
	port, up := flippingHealthServer(t)
	up.Store(true)
	var spawned string
	restore := swapSpawn(func(exe, _, _ string) error { spawned = exe; return nil })
	defer restore()

	if _, _, err := StartDaemon(t.TempDir(), port, nil); err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if spawned != want {
		t.Errorf("spawned %q, want the resolved binary %q", spawned, want)
	}
}

// F239's backstop: a binary that is itself named claude (a copy, or a hard
// link) would run as the shim, so StartDaemon refuses to spawn it.
func TestStartDaemonRefusesToSpawnABinaryNamedClaude(t *testing.T) {
	self := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(self, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := executablePath
	executablePath = func() (string, error) { return self, nil }
	defer func() { executablePath = orig }()
	port, up := flippingHealthServer(t)
	up.Store(true)
	called := false
	restore := swapSpawn(func(string, string, string) error { called = true; return nil })
	defer restore()

	_, _, err := StartDaemon(t.TempDir(), port, nil)
	if called {
		t.Error("spawnFn was called with a binary named claude")
	}
	if err == nil || !strings.Contains(err.Error(), "named claude") {
		t.Errorf("err = %v, want a refusal naming the claude binary", err)
	}
}
