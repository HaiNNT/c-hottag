// Package sessions tracks, in memory, what each identified Claude Code
// session has done through the proxy. A native session id is held only to
// count conversation changes; it is never exposed or persisted.
package sessions

import (
	"sort"
	"sync"
	"time"
)

// Grace is how long an unkept session stays after its last request.
const Grace = 10 * time.Minute

// Activity is the exported, id-free view of one session.
type Activity struct {
	SID           string    `json:"sid"`
	Pool          string    `json:"pool"`
	Account       string    `json:"account,omitempty"`
	LastSeen      time.Time `json:"lastSeen"`
	Requests      int       `json:"requests"`
	Conversations int       `json:"conversations"`
}

type entry struct {
	Activity
	native string
}

// Tracker is safe for concurrent use.
type Tracker struct {
	mu sync.Mutex
	m  map[string]*entry
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{m: map[string]*entry{}} }

// Seen records one request of session sid. An empty sid is ignored.
func (t *Tracker) Seen(sid, pool, account string, inference bool, nativeID string, at time.Time) {
	if sid == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[sid]
	if e == nil {
		e = &entry{Activity: Activity{SID: sid}}
		t.m[sid] = e
	}
	e.Pool = pool
	e.Requests++
	if inference && account != "" {
		e.Account = account
	}
	if at.After(e.LastSeen) {
		e.LastSeen = at
	}
	if nativeID != "" && nativeID != e.native {
		e.native = nativeID
		e.Conversations++
	}
}

// Peek is the Conversations count sid would have after a Seen carrying
// nativeID, without recording anything. It lets a caller ask, before it
// routes a request, whether that request starts a new conversation.
func (t *Tracker) Peek(sid, nativeID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[sid]
	if e == nil {
		if nativeID != "" {
			return 1
		}
		return 0
	}
	if nativeID != "" && nativeID != e.native {
		return e.Conversations + 1
	}
	return e.Conversations
}

// Account is the account sid's last inference request went out as, "" if
// unknown.
func (t *Tracker) Account(sid string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.m[sid]; e != nil {
		return e.Account
	}
	return ""
}

// Snapshot returns copies of every entry, sorted by SID.
func (t *Tracker) Snapshot() []Activity {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Activity, 0, len(t.m))
	for _, e := range t.m {
		out = append(out, e.Activity)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out
}

// Forget drops entries keep rejects whose last request is older than grace.
func (t *Tracker) Forget(keep func(sid string) bool, now time.Time, grace time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for sid, e := range t.m {
		if !keep(sid) && now.Sub(e.LastSeen) > grace {
			delete(t.m, sid)
		}
	}
}

// Natives returns each session id's latest native id, only for those that
// have one.
func (t *Tracker) Natives() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]string{}
	for sid, e := range t.m {
		if e.native != "" {
			out[sid] = e.native
		}
	}
	return out
}
