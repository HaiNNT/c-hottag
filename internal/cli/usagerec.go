package cli

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/msgusage"
	"github.com/HaiNNT/c-hottag/internal/store"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// usageQueueSize bounds the recorder's queue; a full queue drops (and counts)
// rather than block the proxy's response path (R203).
const usageQueueSize = 1024

// snapshotEvery is the most often one account gets a utilization snapshot,
// unless its 7d reset moved.
const snapshotEvery = 10 * time.Minute

type usageItem struct {
	ev   *usagelog.Event
	snap *usagelog.Snapshot
}

// usageRecorder writes the daemon's live usage events and utilization
// snapshots to the ledger from one goroutine behind a bounded queue.
type usageRecorder struct {
	appendEvents   func([]usagelog.Event) error
	appendSnapshot func(usagelog.Snapshot) error
	onErr          func(error)
	now            func() time.Time

	mu     sync.RWMutex // guards closed against sends on q
	closed bool
	q      chan usageItem
	done   chan struct{}

	dropped atomic.Int64

	smu  sync.Mutex
	last map[string]snapMark
}

type snapMark struct {
	at time.Time
	r7 int64
}

func newUsageRecorder(l usagelog.Ledger, now func() time.Time) *usageRecorder {
	return newUsageRecorderWith(l.AppendEvents, l.AppendSnapshot, now)
}

func newUsageRecorderWith(ae func([]usagelog.Event) error, as func(usagelog.Snapshot) error, now func() time.Time) *usageRecorder {
	r := &usageRecorder{
		now:            now,
		appendEvents:   ae,
		appendSnapshot: as,
		q:              make(chan usageItem, usageQueueSize),
		done:           make(chan struct{}),
		last:           map[string]snapMark{},
	}
	go r.run()
	return r
}

func (r *usageRecorder) run() {
	defer close(r.done)
	for it := range r.q {
		var err error
		if it.ev != nil {
			err = r.appendEvents([]usagelog.Event{*it.ev})
		} else if it.snap != nil {
			err = r.appendSnapshot(*it.snap)
		}
		if err != nil && r.onErr != nil {
			r.onErr(err)
		}
	}
}

func (r *usageRecorder) enqueue(it usageItem) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false
	}
	select {
	case r.q <- it:
		return true
	default:
		r.dropped.Add(1)
		return false
	}
}

// Message queues one live message's usage. It never blocks: a full queue
// drops the event and counts it.
func (r *usageRecorder) Message(account string, u msgusage.Usage, at time.Time) {
	if at.IsZero() {
		at = r.now()
	}
	r.enqueue(usageItem{ev: &usagelog.Event{
		T: at, MID: u.ID, Src: "live", Acct: account, Model: u.Model, Speed: u.Speed,
		In: u.In, Out: u.Out, CR: u.CR, CW5m: u.CW5m, CW1h: u.CW1h, Web: u.Web,
	}})
}

// Snapshot queues one utilization sample, at most one per account per
// snapshotEvery unless the 7d reset changed. p5 and p7 are 0-100.
func (r *usageRecorder) Snapshot(acct, tier string, units float64, p5, p7 *float64, r7 int64, at time.Time) {
	if p5 == nil && p7 == nil {
		return
	}
	// The headers and the poll name the same reset a second or minutes apart:
	// round it to the hour so they are one window and one throttle key.
	if r7 > 0 {
		r7 = (r7 + 1800) / 3600 * 3600
	}
	r.smu.Lock()
	m, seen := r.last[acct]
	if seen && m.r7 == r7 && at.Sub(m.at) < snapshotEvery {
		r.smu.Unlock()
		return
	}
	r.last[acct] = snapMark{at: at, r7: r7}
	r.smu.Unlock()
	r.enqueue(usageItem{snap: &usagelog.Snapshot{T: at, Acct: acct, Tier: tier, Units: units, P5: p5, P7: p7, R7: r7}})
}

// Dropped is how many events the full queue has refused.
func (r *usageRecorder) Dropped() int64 { return r.dropped.Load() }

// Close stops accepting, writes what is queued and returns. Safe twice.
func (r *usageRecorder) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.q)
	}
	r.mu.Unlock()
	<-r.done
}

// tierUnits is an account's plan tier and capacity units for a snapshot: the
// --units override when set, else the tier's. Unknown account or tier gives
// "" and 0.
func tierUnits(st store.State, account string) (string, float64) {
	for _, a := range st.Accounts {
		if !strings.EqualFold(a.Name, account) {
			continue
		}
		t, ok := autoswitch.ParseTier(a.Plan)
		if !ok {
			return "", 0
		}
		if a.Units > 0 {
			return string(t), float64(a.Units)
		}
		return string(t), t.Units()
	}
	return "", 0
}

func pct(w usagehdr.Window) *float64 {
	if !w.HasUtilization {
		return nil
	}
	v := w.Utilization * 100
	return &v
}

// snapshotFeed returns the sink's snapshot hook: it records one account's
// windows (usagehdr.Window is 0-1, the ledger 0-100) with its tier from state.
func snapshotFeed(rec *usageRecorder, state func() (store.State, error)) func(account string, five, seven usagehdr.Window, at time.Time) {
	return func(account string, five, seven usagehdr.Window, at time.Time) {
		p5, p7 := pct(five), pct(seven)
		if p5 == nil && p7 == nil {
			return
		}
		var r7 int64
		if !seven.ResetsAt.IsZero() {
			r7 = seven.ResetsAt.Unix()
		}
		var tier string
		var units float64
		if state != nil {
			if st, err := state(); err == nil {
				tier, units = tierUnits(st, account)
			}
		}
		rec.Snapshot(account, tier, units, p5, p7, r7, at)
	}
}
