package tracelog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func TestWriterAppendsAndReadAll(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "trace.jsonl")
	w, err := tracelog.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Write(tracelog.Record{Kind: "req", Method: "GET", Host: "api.anthropic.com", Path: "/v1/models", Status: 200})
	_ = w.Write(tracelog.Record{Kind: "mark", Mark: "usage"})
	_ = w.Close()
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", st.Mode().Perm())
	}
	recs, err := tracelog.ReadAll(p)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs = %v err = %v", recs, err)
	}
	if recs[0].T.IsZero() || recs[1].Mark != "usage" {
		t.Fatalf("bad records: %+v", recs)
	}
}

// Run with -race: Close must not race with a concurrent Write.
func TestWriterCloseDuringWrites(t *testing.T) {
	w, err := tracelog.Open(filepath.Join(t.TempDir(), "t.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Write(tracelog.Record{Kind: "mark", Mark: "x"})
			}
		}()
	}
	w.Close()
	wg.Wait()
}

// fakeWC records what tracelog handed it and whether it was closed.
type fakeWC struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
	err    error // returned from Write when non-nil
	short  bool  // when true, Write reports n < len(p) with a nil error
}

func (f *fakeWC) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.short {
		// A writer that returns n < len(p) with a nil error violates the
		// io.Writer contract; fakeWC can still produce it so Write's guard
		// against that violation is exercised.
		n, _ := f.buf.Write(p[:len(p)-1])
		return n, nil
	}
	return f.buf.Write(p)
}

func (f *fakeWC) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeWC) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// TestNewWriterAppendsOneJSONLinePerRecordToTheInjectedWriter pins the seam:
// tracelog must hand the underlying writer exactly one newline-terminated
// JSON document per record, because that is the contract rotation counts
// bytes against.
func TestNewWriterAppendsOneJSONLinePerRecordToTheInjectedWriter(t *testing.T) {
	f := &fakeWC{}
	w := tracelog.NewWriter(f)

	if err := w.Write(tracelog.Record{Kind: "req", Host: "api.anthropic.com"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(tracelog.Record{Kind: "tunnel", Host: "example.test"}); err != nil {
		t.Fatal(err)
	}

	got := f.String()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("underlying writer got %d lines, want 2:\n%s", len(lines), got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("last record was not newline-terminated; rotation counts bytes on whole lines:\n%q", got)
	}
	for i, ln := range lines {
		var r tracelog.Record
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%q", i, err, ln)
		}
		if r.T.IsZero() {
			t.Fatalf("line %d has a zero timestamp; Write must stamp T", i)
		}
	}
}

// TestNewWriterCloseClosesTheInjectedWriter pins that ownership passes to
// tracelog: Task 3 hands it a *rotate.Writer and closes only the tracelog
// Writer, so a Close that stopped here would leak the rotating file.
func TestNewWriterCloseClosesTheInjectedWriter(t *testing.T) {
	f := &fakeWC{}
	w := tracelog.NewWriter(f)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		t.Fatal("tracelog.Writer.Close did not close the injected writer; the rotating file would leak")
	}
}

// TestNewWriterPropagatesTheUnderlyingWriteError pins that a failing sink is
// reported rather than swallowed: proxy.Server.log routes this to
// OnLogError, which is how an operator learns the trace log stopped working.
func TestNewWriterPropagatesTheUnderlyingWriteError(t *testing.T) {
	f := &fakeWC{err: errors.New("disk on fire")}
	w := tracelog.NewWriter(f)
	err := w.Write(tracelog.Record{Kind: "req"})
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("Write error = %v, want it to carry the underlying failure", err)
	}
}

// TestNewWriterReportsShortWriteFromTheInjectedWriter pins the guard against
// an io.Writer contract violation: io.Writer requires a non-nil error
// whenever n < len(p), but tracelog can no longer assume that holds now that
// any io.WriteCloser can be injected (it used to be only *os.File, which
// conforms). Without the guard, a writer that violates the contract would
// truncate a log line with no error reaching anyone.
func TestNewWriterReportsShortWriteFromTheInjectedWriter(t *testing.T) {
	f := &fakeWC{short: true}
	w := tracelog.NewWriter(f)
	err := w.Write(tracelog.Record{Kind: "req"})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write error = %v, want io.ErrShortWrite", err)
	}
}

// TestOpenStillWritesToTheNamedFile pins that the existing constructor is
// unchanged by the seam: trace run, the proxytest harness and ReadAll all
// still depend on Open(path) creating and appending to that exact file.
func TestOpenStillWritesToTheNamedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "proxy.jsonl")
	w, err := tracelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(tracelog.Record{Kind: "req", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	recs, err := tracelog.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Host != "h" {
		t.Fatalf("ReadAll = %+v, want one record for host h", recs)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600: the trace log carries request metadata", fi.Mode().Perm())
	}
}
