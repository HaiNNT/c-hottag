// Package rotate provides a size-capped log writer that rotates in place.
//
// It exists for daemon.log (spec §4.2): the daemon starts at login and may
// run for weeks, so an unbounded append is not an option. Deliberately
// minimal — no compression, no time-based rotation, no external dependency.
package rotate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StampLayout is the local timestamp a stamped log line starts with.
const StampLayout = "2006-01-02 15:04:05"

// ErrClosed is returned by Write once Close has been called. Without this
// guard, a Write after Close that happens to cross MaxBytes would rotate:
// rotateLocked would open a fresh file and reassign w.f, resurrecting a
// writer its caller believed was shut down and leaking the new fd.
var ErrClosed = errors.New("rotate: write after Close")

type Config struct {
	Path     string // the live log
	MaxBytes int64  // rotate once the live log reaches this size
	Keep     int    // how many rotated files to keep (.1 .. .Keep)
	// Stamp, when set, is the clock for a local timestamp
	// ("2006-01-02 15:04:05" and a space) put at the start of every line
	// written. nil writes lines as given.
	Stamp func() time.Time
}

type Writer struct {
	mu     sync.Mutex
	cfg    Config
	f      *os.File
	size   int64
	closed bool
	// midLine is true when the last byte written was not a newline, so the
	// next write continues a line and gets no stamp.
	midLine bool
}

// Open appends to cfg.Path (0600), creating parent dirs (0700) as needed.
func Open(cfg Config) (*Writer, error) {
	if cfg.MaxBytes <= 0 {
		return nil, fmt.Errorf("rotate: MaxBytes must be positive, got %d", cfg.MaxBytes)
	}
	if cfg.Keep < 1 {
		return nil, fmt.Errorf("rotate: Keep must be at least 1, got %d", cfg.Keep)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("rotate: create directory for %s: %w", cfg.Path, err)
	}
	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("rotate: open %s: %w", cfg.Path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("rotate: stat %s: %w", cfg.Path, err)
	}
	return &Writer{cfg: cfg, f: f, size: fi.Size()}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	stamped := w.cfg.Stamp != nil && len(p) > 0
	out, starts, plen := p, []int(nil), 0
	if stamped {
		out, starts, plen = w.stamp(p, w.midLine)
	}
	if w.size+int64(len(out)) > w.cfg.MaxBytes {
		if err := w.rotateLocked(); err != nil {
			// Rotation didn't complete, but w.f is guaranteed to still be a
			// valid, open handle (rotateLocked never closes it before a
			// replacement is confirmed usable). Report the failure for this
			// write rather than silently dropping it; w.size is unchanged,
			// so the next Write retries rotation instead of wedging.
			return 0, err
		}
		// The new file starts a line, whatever the old one ended with.
		w.midLine = false
		if stamped {
			out, starts, plen = w.stamp(p, false)
		}
	}
	n, err := w.f.Write(out)
	w.size += int64(n)
	if n > 0 {
		w.midLine = out[n-1] != '\n'
	}
	if err != nil {
		// n counts stamped bytes: report only how much of p was written.
		wrote, prefixBytes := n, 0
		for _, st := range starts {
			if in := wrote - st; in > 0 {
				prefixBytes += min(in, plen)
			}
		}
		n = wrote - prefixBytes
		return n, err
	}
	return len(p), nil
}

// stamp returns p with the timestamp at the start of each line (the first
// one too unless midLine), where each prefix begins in the result, and the
// prefix's length. Caller holds mu.
func (w *Writer) stamp(p []byte, midLine bool) (out []byte, starts []int, plen int) {
	prefix := w.cfg.Stamp().Format(StampLayout) + " "
	out = make([]byte, 0, len(p)+len(prefix))
	atStart := !midLine
	for len(p) > 0 {
		if atStart {
			starts = append(starts, len(out))
			out = append(out, prefix...)
		}
		i := 0
		for i < len(p) && p[i] != '\n' {
			i++
		}
		if i < len(p) {
			i++
			atStart = true
		} else {
			atStart = false
		}
		out = append(out, p[:i]...)
		p = p[i:]
	}
	return out, starts, len(prefix)
}

func rotatedPath(path string, n int) string {
	return fmt.Sprintf("%s.%d", path, n)
}

// rotateLocked renames path -> path.1, path.1 -> path.2, dropping the
// oldest, then opens a fresh live file. Caller holds mu.
//
// It never closes w.f until a replacement file is open and confirmed
// usable: renaming an open file does not invalidate the handle, so any
// failure along the way (a blocked rename, a failed reopen) leaves w.f
// exactly as it was — still writable — instead of wedging the writer on a
// closed handle. If a previous attempt already renamed the live file away
// but failed to reopen it, cfg.Path won't exist; the rename step is skipped
// on retry rather than erroring on a missing source.
func (w *Writer) rotateLocked() error {
	// Drop the oldest, then shift the rest up, newest last. Best-effort:
	// these touch only already-rotated files, never the live handle.
	os.Remove(rotatedPath(w.cfg.Path, w.cfg.Keep))
	for i := w.cfg.Keep - 1; i >= 1; i-- {
		os.Rename(rotatedPath(w.cfg.Path, i), rotatedPath(w.cfg.Path, i+1))
	}

	rotated1 := rotatedPath(w.cfg.Path, 1)
	if _, err := os.Stat(w.cfg.Path); err == nil {
		if err := os.Rename(w.cfg.Path, rotated1); err != nil {
			return fmt.Errorf("rotate: rename %s: %w", w.cfg.Path, err)
		}
	}

	f, err := os.OpenFile(w.cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("rotate: open %s: %w", w.cfg.Path, err)
	}

	old := w.f
	w.f, w.size = f, 0
	// The replacement handle is installed above, so rotation has already
	// succeeded: Write will use the new file regardless of what happens to
	// the outgoing one. Deliberately discard old.Close's error rather than
	// reporting it — surfacing it here would make Write treat a healthy
	// rotation as a failure and drop the caller's payload, and POSIX still
	// releases the fd even when close reports an error, so nothing leaks.
	old.Close()
	return nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Idempotent: a second Close is a no-op, not an error.
	//
	// Not because anything currently double-closes one of these: every
	// production Close reaching a *Writer today — internal/cli/proxy.go's
	// trace log (`defer lw.Close()` on the tracelog.Writer wrapping it) and
	// daemon.go's daemon.log (`defer logw.Close()`) — is a single defer,
	// and proxy.Server never closes cfg.Log itself. The only genuine
	// double-Close anywhere in this tree today is a test harness
	// (proxytest.Harness.CloseLog, called explicitly by a test and again by
	// its own t.Cleanup) over a plain *os.File via tracelog.Open — it never
	// goes through a rotate.Writer at all.
	//
	// Required anyway: TestCloseIsIdempotent pins this as a done criterion,
	// and this is the only layer that can actually provide it.
	// tracelog.Writer.Close (the layer that sits on top of a rotate.Writer
	// in production) is a bare passthrough with no guard of its own — see
	// its own doc comment — so a caller that ever composed the two and
	// reached Close from two places would get idempotency only because
	// THIS Close guards it. Returning the os error on a second call here
	// would surface as a shutdown failure that did not happen.
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("rotate: close %s: %w", w.cfg.Path, err)
	}
	return nil
}
