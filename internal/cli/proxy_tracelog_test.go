package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// TestOpenTraceLogRotatesRatherThanGrowingForever pins F68: proxy.jsonl gets
// one line per request and per tunnel, and since M1c3 the proxy runs as a
// login service, so an unbounded file grows for the life of the machine.
func TestOpenTraceLogRotatesRatherThanGrowingForever(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.jsonl")
	w, err := openTraceLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil {
		t.Fatal("openTraceLog returned no writer for a non-empty path")
	}
	defer w.Close()

	// Each record here marshals to 127 bytes (measured), so traceLogMaxBytes
	// (8 MiB) needs a little over 66,000 of them to cross once; 400,000
	// crosses it roughly six times over, well past a single rotation.
	for i := 0; i < 400_000; i++ {
		if err := w.Write(tracelog.Record{
			Kind: "req", Host: "api.anthropic.com",
			Path: "/v1/messages", Method: "POST", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > traceLogMaxBytes {
		t.Fatalf("live trace log is %d bytes, want <= %d: it is not being rotated (F68)", fi.Size(), traceLogMaxBytes)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated generation at %s.1: %v", path, err)
	}
	// Pins the retention decision (traceLogKeep = 5), not merely that
	// rotation happens: a %.1-only check still passes with Keep as low as
	// 2. 400,000 records cross the 8 MiB cap roughly six times, so all five
	// kept generations must exist and the sixth must have been dropped.
	if _, err := os.Stat(path + ".5"); err != nil {
		t.Fatalf("no rotated generation at %s.5: %v — traceLogKeep should keep 5 generations", path, err)
	}
	if _, err := os.Stat(path + ".6"); !os.IsNotExist(err) {
		t.Fatalf("%s.6 exists (err=%v), want it dropped — traceLogKeep should keep only 5 generations", path, err)
	}
	// Every generation must carry the same mode as the live file: the trace
	// log holds request metadata.
	for _, p := range []string{path, path + ".1", path + ".5"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
}

// TestOpenTraceLogWithAnEmptyPathDisablesLogging pins the off-switch. A
// nil proxy.Config.Log is already a supported state — proxy.Server.log
// returns early on it — so disabling needs no new branch in the proxy.
func TestOpenTraceLogWithAnEmptyPathDisablesLogging(t *testing.T) {
	w, err := openTraceLog("")
	if err != nil {
		t.Fatalf("openTraceLog(\"\") = %v, want no error: an empty path means no trace log", err)
	}
	if w != nil {
		t.Fatal("openTraceLog(\"\") returned a writer; an empty path must disable logging entirely")
	}
}
