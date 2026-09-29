package tracelog

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Writer struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// NewWriter appends records to wc, taking ownership of it: Close closes wc.
//
// The indirection exists so the trace log can be bounded. proxy.jsonl gets
// one line per proxied request and per tunnel, and since M1c3 the proxy runs
// as a login service rather than a foreground command, so an unbounded
// append-only file grows for the life of the machine (F68). rotate.Writer
// satisfies io.WriteCloser and is what runProxy injects.
func NewWriter(wc io.WriteCloser) *Writer { return &Writer{w: wc} }

// Open appends to path (0600), creating parent dirs (0700) as needed.
func Open(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return NewWriter(f), nil
}

// Write appends r as one JSON line, stamping T if unset.
func (w *Writer) Write(r Record) error {
	if r.T.IsZero() {
		r.T = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.w.Write(b)
	if err != nil {
		return err
	}
	// The injected writer is outside this package's control (it used to be
	// only *os.File, which conforms). io.Writer requires a non-nil error
	// whenever n < len(p); a writer that violates that would otherwise
	// truncate a log line silently instead of raising an error anyone sees.
	if n < len(b) {
		return io.ErrShortWrite
	}
	return nil
}

// Close closes the underlying writer. NOT idempotent: this is a bare
// passthrough to wc.Close (whatever wc's own second-Close behaviour is), no
// guard of its own. rotate.Writer, the wc production always injects
// (internal/cli/proxy.go's openTraceLog), IS idempotent — see its own Close
// doc comment — so a caller stacking the two and reaching Close from two
// places gets idempotency entirely from that layer, not this one.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Close()
}

// ReadAll returns every well-formed record in path, skipping malformed lines.
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var out []Record
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
