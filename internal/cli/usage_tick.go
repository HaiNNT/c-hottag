package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// usageTickInterval is how often the daemon imports transcript usage into the
// ledger and compacts it (R203).
const usageTickInterval = 24 * time.Hour

// usageRetryAfter is how long the daemon waits to retry a pass that found the
// ledger lock busy, instead of waiting a whole usageTickInterval.
const usageRetryAfter = time.Minute

// usageTick runs the ledger's import and compaction once at start and then
// every usageTickInterval, so history is imported even when `chottag stats`
// is never run. Errors go to the daemon log once per distinct message. It
// never waits for the ledger lock: a busy lock skips the pass and retries
// after usageRetryAfter.
type usageTick struct {
	home string
	log  io.Writer
	now  func() time.Time
	// tick, when set, replaces the 24 h ticker (tests drive it).
	tick <-chan time.Time
	// retry, when set, replaces the one-minute timer after a skipped pass.
	retry func() <-chan time.Time
	// passed, when set, is sent to after every pass (tests wait on it).
	passed chan<- struct{}

	lastErr     string
	lastSkipped int
}

// pass imports and compacts once under the ledger lock and reports whether the
// lock was busy (the pass was skipped). A failure is logged by kind only
// (safeImportErr): a transcript path holds project folder names.
func (u *usageTick) pass(ctx context.Context) (busy bool) {
	skipped, busy, err := u.run(ctx)
	if err == nil || ctx.Err() != nil {
		u.lastErr = ""
	} else if msg := safeImportErr(err); msg != u.lastErr {
		u.lastErr = msg
		if u.log != nil {
			fmt.Fprintf(u.log, "chottag: usage import: %s\n", msg)
		}
	}
	if !busy {
		if skipped > 0 && skipped != u.lastSkipped && u.log != nil {
			fmt.Fprintf(u.log, "chottag: usage import: %d transcript files or folders could not be read\n", skipped)
		}
		u.lastSkipped = skipped
	}
	if u.passed != nil {
		u.passed <- struct{}{}
	}
	return busy
}

func (u *usageTick) run(ctx context.Context) (skipped int, busy bool, err error) {
	st, err := (store.Store{Dir: u.home}).Load()
	if err != nil {
		return 0, false, err
	}
	l := usagelog.Open(u.home)
	unlock, ok, err := l.TryLock()
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, true, nil
	}
	defer unlock()
	res, err := importLedger(ctx, l, st, u.now(), false)
	if err != nil {
		return 0, false, err
	}
	return res.Skipped, false, l.Compact(u.now())
}

// Run passes once, then on every tick, until ctx is cancelled. A pass that
// found the ledger busy is retried after usageRetryAfter.
func (u *usageTick) Run(ctx context.Context) {
	tick := u.tick
	if tick == nil {
		t := time.NewTicker(usageTickInterval)
		defer t.Stop()
		tick = t.C
	}
	busy := u.pass(ctx)
	for {
		var retry <-chan time.Time
		if busy {
			if u.retry != nil {
				retry = u.retry()
			} else {
				retry = time.After(usageRetryAfter)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-retry:
		}
		busy = u.pass(ctx)
	}
}
