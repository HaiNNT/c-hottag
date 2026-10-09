package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/journal"
	"github.com/HaiNNT/c-hottag/internal/sessions"
)

// journalAlive reports whether pid is a live process. A package-level var so
// tests never probe real pids.
var journalAlive = func(pid int) bool { return syscall.Kill(pid, 0) == nil }

// journalPassInterval is the least time between two journal passes.
const journalPassInterval = 15 * time.Second

// journalPass is the daemon's roster-tick pass over the session journal: it
// records each session's native id, ends entries whose process died, and
// prunes. A nil *journalPass does nothing. It is called from the roster
// watcher's single goroutine only.
type journalPass struct {
	home    string
	log     io.Writer
	last    time.Time
	lastErr string
}

func (p *journalPass) tick(tr *sessions.Tracker, now time.Time) {
	if p == nil {
		return
	}
	if !p.last.IsZero() && now.Sub(p.last) < journalPassInterval {
		return
	}
	p.last = now
	err := p.run(tr, now)
	if err == nil {
		p.lastErr = ""
		return
	}
	if err.Error() != p.lastErr {
		p.lastErr = err.Error()
		if p.log != nil {
			fmt.Fprintf(p.log, "chottag: journal: %v\n", err)
		}
	}
}

func (p *journalPass) run(tr *sessions.Tracker, now time.Time) error {
	j, err := journal.Open(filepath.Join(p.home, "sessions"))
	if err != nil {
		return err
	}
	if tr != nil {
		if _, err := j.SetNatives(tr.Natives()); err != nil {
			return err
		}
	}
	if _, err := j.Sweep(now, journalBootTime(), journalAlive); err != nil {
		return err
	}
	return j.Prune(now)
}
