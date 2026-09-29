package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

// logsPoll is how often `daemon logs -f` checks daemon.log for new bytes,
// a rotation or a truncation (spec §5: 250ms). It is a var only so tests
// can shorten it.
var logsPoll = 250 * time.Millisecond

// runDaemonLogs is `chottag daemon logs [-n N] [-f]` (spec §5). ctx is
// cancelled by Ctrl-C (Task 5's dispatcher), which ends -f with exit 0.
// Only daemon.log is read: daemon.stderr.log (the packaging units'
// StandardErrorPath) and the request log are out of scope.
func runDaemonLogs(ctx context.Context, args []string, r *reporter) int {
	stdout, stderr := r.Stdout(), r.Stderr()
	flags := flag.NewFlagSet("daemon logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	n := flags.Int("n", 50, "number of lines to print")
	follow := flags.Bool("f", false, "keep printing lines as they are written")
	// parseInterspersed (spec §5.3), rather than a bare flags.Parse: logs
	// takes no positional at all, and Go's flag package stops parsing
	// entirely at the first non-flag token, so a stray one ahead of a real
	// flag (`logs x -f`) must not be allowed to leave that flag unparsed
	// (fix round 4, item 3).
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		return exit.Usage
	}
	if len(positional) != 0 || *n < 0 {
		fmt.Fprintln(stderr, "usage: chottag daemon logs [-n N] [-f]")
		return exit.Usage
	}
	h, err := home()
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	path := filepath.Join(h, "daemon.log")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "chottag: no daemon log yet (%s)\n", path)
		return exit.OK
	}
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	b, err := io.ReadAll(f) // daemon.log is capped at 8 MiB (daemon.go)
	if err != nil {
		f.Close()
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	stdout.Write(lastLines(b, *n))
	if !*follow {
		f.Close()
		return exit.OK
	}
	followLog(ctx, path, f, stdout)
	return exit.OK
}

// lastLines returns the last n lines of b. A final line without a newline
// yet (the daemon may be mid-write) counts as a line.
func lastLines(b []byte, n int) []byte {
	if n == 0 || len(b) == 0 {
		return nil
	}
	end := len(b)
	if b[end-1] == '\n' {
		end--
	}
	i := end
	for count := 0; i > 0; i-- {
		if b[i-1] == '\n' {
			count++
			if count == n {
				break
			}
		}
	}
	return b[i:]
}

// followLog copies whatever is appended to path until ctx is done. f is
// open on path and positioned where printing stopped. followLog closes
// whichever file it holds last.
//
// Rotation: internal/rotate renames daemon.log to daemon.log.1 and creates
// a fresh daemon.log under its own lock, so once path names a different
// inode from f, the old file is complete. It is drained once more (anything
// written between this pass's copy and the rename), then path is reopened
// from its start. The drain covers a window between two syscalls, which no
// test can land in deterministically, so it is argued here rather than
// tested.
//
// Truncation: when path shrinks below what was already read, reading
// restarts from the top. A truncation followed, within one poll, by more
// bytes than had been read looks like an append. That cannot be told
// apart without content, and rotate never truncates, so an operator's
// hand-truncation is the only case.
func followLog(ctx context.Context, path string, f *os.File, stdout io.Writer) {
	defer func() { f.Close() }()
	tick := time.NewTicker(logsPoll)
	defer tick.Stop()
	for {
		io.Copy(stdout, f)
		if cur, err := os.Stat(path); err == nil {
			if held, err := f.Stat(); err == nil && !os.SameFile(cur, held) {
				io.Copy(stdout, f)
				if nf, err := os.Open(path); err == nil {
					f.Close()
					f = nf
					continue
				}
			} else if pos, err := f.Seek(0, io.SeekCurrent); err == nil && cur.Size() < pos {
				f.Seek(0, io.SeekStart)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
