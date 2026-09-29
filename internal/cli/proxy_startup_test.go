package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/owners"
)

// TestEveryPreServeFailureReachesTheRealStderr is F109's table: one row per
// failure path before the daemon starts serving (spec §4.6). For `daemon
// run`, runProxyWithSignal's stderr is daemon.log and startupErr is the
// process's real stderr, which is the only stream launchd's
// StandardErrorPath and the systemd journal see. Every row must reach both.
// Each row's mutation sends that one path back to stderr alone, and only
// that row fails.
//
// listenTCP is stubbed for the whole table. The last row uses it as its
// failure, and for every other row it is a tripwire: a row whose trigger
// stopped working would fall through to the bind, fail there with
// listenRefused, and miss its own want.
//
// Every path is triggerable. The only branch that is not is filepath.Abs
// failing inside home(), which needs os.Getwd to fail. It shares the
// "home" row's write site, so that row covers it.
func TestEveryPreServeFailureReachesTheRealStderr(t *testing.T) {
	const listenRefused = "test: listen refused"
	origListen := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) { return nil, errors.New(listenRefused) }
	t.Cleanup(func() { listenTCP = origListen })

	// blocker is a regular file: any path that needs it to be a directory
	// fails with ENOTDIR, naming it.
	blocker := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	listen := []string{"--listen", "127.0.0.1:0"}

	cases := []struct {
		name  string
		setup func(t *testing.T, home string) (args, want []string)
	}{
		{"usage", func(t *testing.T, home string) ([]string, []string) {
			return []string{"nope"}, []string{"usage: chottag"}
		}},
		{"home", func(t *testing.T, home string) ([]string, []string) {
			t.Setenv("CHOTTAG_HOME", "")
			t.Setenv("HOME", "")
			return []string{"run"}, []string{"$HOME is not defined"}
		}},
		{"flag_parse", func(t *testing.T, home string) ([]string, []string) {
			return []string{"run", "--no-such-flag"}, []string{"flag provided but not defined: -no-such-flag"}
		}},
		{"listen_not_loopback", func(t *testing.T, home string) ([]string, []string) {
			return []string{"run", "--listen", "0.0.0.0:0"}, []string{"must be loopback"}
		}},
		{"upstream_proxy", func(t *testing.T, home string) ([]string, []string) {
			return append([]string{"run", "--upstream-proxy", "ftp://corp:21"}, listen...), []string{"only http:// is supported"}
		}},
		{"home_dir", func(t *testing.T, home string) ([]string, []string) {
			b := blocker(t)
			t.Setenv("CHOTTAG_HOME", filepath.Join(b, "home"))
			return append([]string{"run"}, listen...), []string{b, "not a directory"}
		}},
		{"ca", func(t *testing.T, home string) ([]string, []string) {
			if err := os.WriteFile(filepath.Join(home, "ca"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return append([]string{"run"}, listen...), []string{filepath.Join(home, "ca"), "not a directory"}
		}},
		{"secret", func(t *testing.T, home string) ([]string, []string) {
			// The CA loads (and creates ca/) first, so a malformed
			// proxy.secret pre-planted there is only reached once
			// proxyauth.LoadOrCreate runs right after it (F109).
			if err := os.MkdirAll(filepath.Join(home, "ca"), 0o700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(home, "ca", "proxy.secret")
			if err := os.WriteFile(p, []byte("not hex"), 0o600); err != nil {
				t.Fatal(err)
			}
			return append([]string{"run"}, listen...), []string{"proxyauth", "malformed"}
		}},
		{"trace_log", func(t *testing.T, home string) ([]string, []string) {
			b := blocker(t)
			return append([]string{"run", "--log", filepath.Join(b, "proxy.jsonl")}, listen...), []string{"rotate: create directory for"}
		}},
		{"owners", func(t *testing.T, home string) ([]string, []string) {
			orig := ownersOpenFn
			ownersOpenFn = func(string) (*owners.Map, error) { return nil, errors.New("test: owners.Open failed") }
			t.Cleanup(func() { ownersOpenFn = orig })
			return append([]string{"run", "--log", ""}, listen...), []string{"test: owners.Open failed"}
		}},
		{"status_sink", func(t *testing.T, home string) ([]string, []string) {
			orig := newStatusSinkFn
			newStatusSinkFn = func(string, func(error)) (*statusSink, error) { return nil, errors.New("test: newStatusSink failed") }
			t.Cleanup(func() { newStatusSinkFn = orig })
			return append([]string{"run", "--log", ""}, listen...), []string{"test: newStatusSink failed"}
		}},
		{"listen", func(t *testing.T, home string) ([]string, []string) {
			return append([]string{"run", "--log", ""}, listen...), []string{listenRefused}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CHOTTAG_HOME", home)
			args, want := c.setup(t, home)

			logw, realErr := newSyncBuf(), newSyncBuf()
			if code := runProxyWithSignal(args, io.Discard, logw, realErr, nil); code == 0 {
				t.Fatalf("runProxyWithSignal(%q) = 0, want a failure", args)
			}
			for _, w := range []struct {
				name string
				buf  *syncBuf
			}{{"stderr (daemon.log)", logw}, {"startupErr (the real stderr)", realErr}} {
				for _, s := range want {
					if !strings.Contains(w.buf.String(), s) {
						t.Errorf("%s = %q, want it to contain %q", w.name, w.buf.String(), s)
					}
				}
			}
		})
	}
}

// Outside the daemon (`proxy run` in the foreground) there is only one
// stderr, so startupErr is nil and a failure must appear on it once, not
// twice (spec §4.6: "nothing changes").
func TestForegroundProxyRunWritesAPreServeFailureOnce(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	var stderr bytes.Buffer
	if code := runProxyWithSignal([]string{"run", "--listen", "0.0.0.0:0"}, io.Discard, &stderr, nil, nil); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if n := strings.Count(stderr.String(), "must be loopback"); n != 1 {
		t.Fatalf("stderr = %q: the message appears %d times, want once", stderr.String(), n)
	}
}

// alwaysFailWriter's Write always returns err and writes nothing, standing
// in for a daemon.log whose own write fails (a rotation failure, or a full
// disk) — fix round 1, I1.
type alwaysFailWriter struct{ err error }

func (w alwaysFailWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestStartupWriterReachesEveryDestinationEvenWhenOneFails pins fix round
// 1, I1: io.MultiWriter stops at the first writer that returns an error,
// and stderr (daemon.log, for `daemon run`) is that first writer here. A
// daemon.log write CAN fail on its own, exactly when a start is already
// failing, and the old io.MultiWriter-based startupWriter would then
// silently drop the message from startupErr too — the one stream
// launchd's StandardErrorPath and the systemd journal actually read.
func TestStartupWriterReachesEveryDestinationEvenWhenOneFails(t *testing.T) {
	failing := alwaysFailWriter{err: errors.New("test: daemon.log write failed")}
	var real bytes.Buffer
	w := startupWriter(failing, &real)

	const msg = "chottag: boom\n"
	n, err := w.Write([]byte(msg))
	if err == nil {
		t.Fatal("Write did not surface the failing destination's error")
	}
	if n != len(msg) {
		t.Fatalf("n = %d, want %d (the full length written) even though one destination failed", n, len(msg))
	}
	if real.String() != msg {
		t.Fatalf("real stderr = %q, want %q: startupWriter must reach every destination even when an earlier one fails", real.String(), msg)
	}
}

// TestStartupWriterReturnsStderrAloneWhenStartupErrIsNil pins the other
// half of startupWriter's contract, unchanged by fix round 1: the
// foreground `proxy run` passes startupErr = nil, and startupWriter must
// return stderr itself — not a wrapper that would write to it twice were
// the caller to also pass it as its own startupErr by mistake.
func TestStartupWriterReturnsStderrAloneWhenStartupErrIsNil(t *testing.T) {
	var buf bytes.Buffer
	if got := startupWriter(&buf, nil); got != io.Writer(&buf) {
		t.Fatalf("startupWriter(stderr, nil) = %v, want stderr itself", got)
	}
}

// TestRunProxyWithSignalPreServeFailureReachesStartupErrEvenWhenStderrFails
// drives the same fix (I1) through runProxyWithSignal itself, standing in
// for `daemon run`'s daemon.log with a writer whose Write always fails:
// startupErr (the real stderr) must still carry the pre-serve failure.
func TestRunProxyWithSignalPreServeFailureReachesStartupErrEvenWhenStderrFails(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	failing := alwaysFailWriter{err: errors.New("test: daemon.log write failed")}
	var startupErr bytes.Buffer
	code := runProxyWithSignal([]string{"run", "--listen", "0.0.0.0:0"}, io.Discard, failing, &startupErr, nil)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(startupErr.String(), "must be loopback") {
		t.Fatalf("startupErr = %q, want the pre-serve failure despite stderr's own write failing", startupErr.String())
	}
}
