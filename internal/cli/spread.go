package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/notify"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// The spread policy's placement engine (M7 spec §3 to §6). Under
// `policy spread` each identified session is placed on an account at its
// first serving-class request and stays there; it moves only when its
// account reaches a switch point or is limited, on a limit hit, or when its
// prompt cache is already cold and a move clearly helps.
const (
	// spreadMoveGap is the least time between two moves of one session
	// (a limit-hit replace excepted), and how long "the account it just
	// left" stays off the list for a forced move.
	spreadMoveGap = 10 * time.Minute
	// spreadColdMargin is how much better the best account's score must be
	// than the current one's for a cache-cold move.
	spreadColdMargin = 1.5
	// spreadSaveEvery debounces writes of placements.json.
	spreadSaveEvery = time.Second
	// spreadNoticeTargets caps the accounts a notice names.
	spreadNoticeTargets = 3
)

// spreadWrite writes placements.json. A variable so a test can observe or
// refuse the write (see TestMain); production is an atomic 0600 write.
var spreadWrite = fsutil.WriteFileAtomic

// spreadPlacer is the part of *spreadEngine the chooser calls: an interface
// so a test can prove serial never reaches it.
type spreadPlacer interface {
	// accountFor is the account sid's request goes out as, native being the
	// request's own session id header ("" if absent). pool is the session's
	// pool: the account is always one of its members.
	accountFor(sid, native, pool string, now time.Time) (string, bool)
	// fallback is the account for a session of pool accountFor had none for
	// (R90).
	// fallbackExcluding is fallback when exclude was tried and cannot serve.
	fallbackExcluding(pool string, exclude map[string]bool, now time.Time) (string, bool)
}

// placement is one session's place, as placements.json stores it.
type placement struct {
	Account string `json:"account"`
	// Dir is the account's slot dir, which survives `chottag rename`; a file
	// written before it existed has none and resolves by name.
	Dir     string    `json:"dir,omitempty"`
	At      time.Time `json:"at"`
	MovedAt time.Time `json:"movedAt"`
	From    string    `json:"from,omitempty"`
	// Conversations is the tracker's count when the session was placed or
	// last checked: a higher count now means its prompt cache is cold.
	Conversations int `json:"conversations"`
}

// markOutcome describes what mark did, for the notice.
type markOutcome struct {
	Newly    bool     // the account was not marked before
	Sessions int      // sessions placed on it now
	To       []string // up to spreadNoticeTargets accounts, in Place order
}

// spreadEngine is safe for concurrent use: one mutex guards everything.
type spreadEngine struct {
	state func() (store.State, error)
	file  func() status.File
	// conv is the tracker's Conversations count for sid as it would be after
	// a request carrying native.
	conv  func(sid, native string) int
	log   io.Writer
	path  string
	write func(path string, data []byte, perm os.FileMode) error

	mu         sync.Mutex
	placements map[string]*placement
	seen       map[string]time.Time  // last request per sid
	loadedAt   time.Time             // when the file was loaded: the prune grace of an unseen placement runs from here
	live       func(sid string) bool // the registry snapshot of the last prune; nil until the first
	// lastAccount, if set, is the account the tracker last saw sid on, so
	// turning spread on does not scatter running sessions (see accountFor).
	lastAccount func(sid string) string
	marked      map[string]string // lowercased account name -> why
	dirty       bool
	lastSave    time.Time
	saveSeq     uint64

	writeMu      sync.Mutex // orders writes; mu is never held across one
	inflight     sync.WaitGroup
	writtenSeq   uint64
	lastWriteErr string
}

// newSpreadEngine loads placements from path (missing is empty; a corrupt
// file is empty and logged once). now is when the daemon started: a loaded
// placement's prune grace runs from it, and it counts toward load only once
// its session is seen or listed live.
func newSpreadEngine(path string, state func() (store.State, error), file func() status.File, conv func(sid, native string) int, log io.Writer, now time.Time) *spreadEngine {
	e := &spreadEngine{
		state: state, file: file, conv: conv, log: log, path: path, write: spreadWrite,
		loadedAt:   now,
		placements: map[string]*placement{}, seen: map[string]time.Time{}, marked: map[string]string{},
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		fmt.Fprintf(log, "chottag: spread: cannot read placements.json, starting empty: %v\n", err)
	default:
		var loaded map[string]*placement
		if err := json.Unmarshal(raw, &loaded); err != nil {
			fmt.Fprintf(log, "chottag: spread: placements.json is not valid, starting empty: %v\n", err)
			break
		}
		for sid, p := range loaded {
			if p != nil && sid != "" && p.Account != "" {
				e.placements[sid] = p
			}
		}
	}
	return e
}

// view is one decision's inputs, read fresh from state and the status cache.
type spreadView struct {
	st     store.State
	accts  []autoswitch.Account
	params autoswitch.Params
}

func (e *spreadEngine) view(now time.Time) (spreadView, bool) {
	st, err := e.state()
	if err != nil {
		return spreadView{}, false
	}
	f := e.file()
	return spreadView{st: st, accts: planAccounts(&st, &f, now), params: autoParams(st)}, true
}

// in is the view's accounts that are members of pool, in registration order,
// and the pool's pin. A pool that is not in state.json is default.
func (v spreadView) in(pool string) ([]autoswitch.Account, string) {
	if !v.st.HasPool(pool) {
		pool = store.DefaultPool
	}
	var out []autoswitch.Account
	for _, a := range v.accts {
		if m, ok := findExact(&v.st, a.Name); ok && m.InPool(pool) {
			out = append(out, a)
		}
	}
	return out, v.st.PoolOf(pool).Pin
}

// sharing is the view's accounts that are in at least one pool account is in.
func (v spreadView) sharing(account string) []autoswitch.Account {
	a, ok := findExact(&v.st, account)
	if !ok {
		return v.accts
	}
	var out []autoswitch.Account
	for _, pa := range v.accts {
		if m, ok := findExact(&v.st, pa.Name); ok && shareAPool(*a, *m) {
			out = append(out, pa)
		}
	}
	return out
}

func shareAPool(a, b store.Account) bool {
	for _, p := range a.InPools() {
		if b.InPool(p) {
			return true
		}
	}
	return false
}

// isMember reports whether name is a member of pool.
func (v spreadView) isMember(pool, name string) bool {
	if !v.st.HasPool(pool) {
		pool = store.DefaultPool
	}
	m, ok := findExact(&v.st, name)
	return ok && m.InPool(pool)
}

func (v spreadView) find(name string) (autoswitch.Account, bool) {
	for _, a := range v.accts {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return autoswitch.Account{}, false
}

// resolve is the registered account p is on: by slot dir first, so a rename
// keeps the session where it is, else by name (a file from before dirs).
// It brings p's name and dir up to date. Caller holds e.mu.
func (e *spreadEngine) resolve(v spreadView, p *placement) (autoswitch.Account, bool) {
	if p.Dir != "" {
		for _, a := range v.st.Accounts {
			if a.Dir == p.Dir {
				if p.Account != a.Name {
					p.Account = a.Name
					e.dirty = true
				}
				return v.find(a.Name)
			}
		}
		return autoswitch.Account{}, false
	}
	a, ok := v.find(p.Account)
	if ok {
		if d := dirOf(v, a.Name); d != "" {
			p.Dir = d
			e.dirty = true
		}
	}
	return a, ok
}

func dirOf(v spreadView, name string) string {
	for _, a := range v.st.Accounts {
		if strings.EqualFold(a.Name, name) {
			return a.Dir
		}
	}
	return ""
}

// counts reports whether sid's placement is a live session's. Before the
// first registry snapshot, a session seen within the grace counts. After it,
// a session the registry lists counts, and so does one seen within two roster
// ticks (a brand-new session the snapshot does not list yet): one that has
// ended does not keep counting for the grace, which would steer new sessions
// away from accounts by ghost load (short `claude -p` jobs).
func (e *spreadEngine) counts(sid string, now time.Time) bool {
	t, seen := e.seen[sid]
	if e.live == nil {
		return seen && now.Sub(t) <= sessions.Grace
	}
	if e.live(sid) {
		return true
	}
	return seen && now.Sub(t) <= 2*rosterTickInterval
}

// load is the number of live placements per account, keyed by the account's
// registered name (spec §3: live identified sessions), whatever pool the
// session is in: an account's capacity is shared by every pool it is in. A
// placement on an
// account that is not registered counts nowhere, nor does one whose session
// has not been seen since a restart and is not in the registry.
func (e *spreadEngine) load(v spreadView, now time.Time) map[string]int {
	out := map[string]int{}
	for sid, p := range e.placements {
		if !e.counts(sid, now) {
			continue
		}
		if a, ok := e.resolve(v, p); ok {
			out[a.Name]++
		}
	}
	return out
}

func (e *spreadEngine) accountFor(sid, native, pool string, now time.Time) (string, bool) {
	if sid == "" {
		return "", false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.view(now)
	if !ok {
		return "", false
	}
	e.seen[sid] = now
	conv := e.conv(sid, native)
	cands, pin := v.in(pool)
	p := e.placements[sid]
	if p == nil {
		// A running session whose account is a candidate stays on it: turning
		// spread on must not scatter warm prompt caches. (A pin, which is for
		// new sessions, does not move one.) Later rebalancing is the
		// cold-move rule's.
		if e.lastAccount != nil {
			if cur, ok := v.find(e.lastAccount(sid)); ok && v.isMember(pool, cur.Name) && autoswitch.Candidate(cur, v.params, now) {
				e.placements[sid] = &placement{Account: cur.Name, Dir: dirOf(v, cur.Name), At: now, Conversations: conv}
				e.dirty = true
				return cur.Name, true
			}
		}
		name, ok := autoswitch.Place(cands, e.load(v, now), pin, nil, v.params, now)
		if !ok {
			return "", false // the caller serves the pool's fallback; the next request tries again
		}
		e.placements[sid] = &placement{Account: name, Dir: dirOf(v, name), At: now, Conversations: conv}
		e.dirty = true
		return name, true
	}
	return e.keepOrMove(sid, p, conv, v, pool, now)
}

// account is accountFor for a default-pool request without a native session
// id.
func (e *spreadEngine) account(sid string, now time.Time) (string, bool) {
	return e.accountFor(sid, "", store.DefaultPool, now)
}

// keepOrMove is accountFor for a session that already has a placement.
// Caller holds e.mu.
func (e *spreadEngine) keepOrMove(sid string, p *placement, conv int, v spreadView, pool string, now time.Time) (string, bool) {
	cur, found := e.resolve(v, p)
	// An account that is no longer in the session's pool (a `pool leave`)
	// is gone for this session: it re-places within the pool at once.
	if found && !v.isMember(pool, cur.Name) {
		found = false
	}
	cands, _ := v.in(pool)
	// hard: the account can no longer serve anyone (rotation off, needs a
	// login, limited, or gone). Such a session leaves at once, whatever
	// the move window says.
	hard := !found || !cur.Rotates || cur.NeedsLogin || cur.Limited
	candidate := !hard && autoswitch.Candidate(cur, v.params, now)
	_, isMarked := e.marked[strings.ToLower(p.Account)]
	if isMarked && candidate {
		delete(e.marked, strings.ToLower(p.Account)) // a candidate again: the mark is stale
		isMarked = false
	}
	recent := !p.MovedAt.IsZero() && now.Sub(p.MovedAt) < spreadMoveGap
	cold := conv > p.Conversations
	// moveTo picks another account: never the current one, and never the
	// one it just left within the move window. The pin is for new sessions.
	moveTo := func(allowBack bool) (string, bool) {
		exclude := map[string]bool{}
		if found {
			exclude[cur.Name] = true
		}
		if from, ok := v.find(p.From); ok && recent && !allowBack {
			exclude[from.Name] = true
		}
		return autoswitch.Place(cands, e.load(v, now), "", exclude, v.params, now)
	}
	switch {
	case hard:
		name, ok := moveTo(false)
		if !ok {
			// Nothing but the account it just left: going back beats
			// leaving a session on an account that cannot serve it.
			name, ok = moveTo(true)
		}
		if !ok {
			delete(e.placements, sid) // nowhere to go: the caller serves the serving account
			e.dirty = true
			return "", false
		}
		e.move(v, p, name, conv, now)
		return name, true
	case !candidate || isMarked:
		// At a switch point: move at the next request, unless it moved
		// within the window or nothing else can take it (then it keeps its
		// account rather than fail or loop: its cache is warm there).
		if !recent {
			if name, ok := moveTo(false); ok {
				e.move(v, p, name, conv, now)
				return name, true
			}
		}
	case cold:
		if !recent {
			if name, ok := moveTo(false); ok {
				// Like with like: the current account without this session
				// against the target with it, so a session never moves for
				// the sake of counting itself on the account it is leaving.
				best, _ := v.find(name)
				l := e.load(v, now)
				bs := autoswitch.Headroom(best, v.params, now) / float64(1+l[best.Name])
				cs := autoswitch.Headroom(cur, v.params, now) / float64(max(l[cur.Name], 1))
				if bs >= spreadColdMargin*cs {
					e.move(v, p, name, conv, now)
					return name, true
				}
			}
		}
	}
	if p.Conversations != conv {
		p.Conversations = conv
		e.dirty = true
	}
	return p.Account, true
}

// move records p moving to name. Caller holds e.mu.
func (e *spreadEngine) move(v spreadView, p *placement, name string, conv int, now time.Time) {
	p.From, p.Account, p.Dir, p.MovedAt, p.Conversations = p.Account, name, dirOf(v, name), now, conv
	e.dirty = true
}

// replace is replaceIn for the default pool.
func (e *spreadEngine) replace(sid string, now time.Time) (string, bool) {
	return e.replaceIn(sid, store.DefaultPool, now)
}

// replaceIn is the wall retry: sid's account hit a limit, so re-place it now
// within pool, excluding its current account. It ignores the move window and
// the no-move-back rule.
func (e *spreadEngine) replaceIn(sid, pool string, now time.Time) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.view(now)
	if !ok {
		return "", false
	}
	p := e.placements[sid]
	exclude := map[string]bool{}
	if p != nil {
		if a, ok := e.resolve(v, p); ok {
			exclude[a.Name] = true
		}
	}
	cands, _ := v.in(pool)
	name, ok := autoswitch.Place(cands, e.load(v, now), "", exclude, v.params, now)
	if !ok {
		return "", false
	}
	e.seen[sid] = now
	if p == nil {
		e.placements[sid] = &placement{Account: name, Dir: dirOf(v, name), At: now}
		e.dirty = true
		return name, true
	}
	e.move(v, p, name, p.Conversations, now)
	return name, true
}

// placed is the account sid is placed on, and whether it has a placement.
func (e *spreadEngine) placed(sid string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.placements[sid]; p != nil {
		return p.Account, true
	}
	return "", false
}

// mark records that account is at a switch point or limited: its sessions
// re-place at their next request. why is for the caller's notice. Only the
// first mark of an episode is Newly.
//
// To is an approximation of where the sessions will go: the accounts Place
// would pick one after another with today's loads, among the members of the
// pools account is in (a session never leaves its pool). Each session really
// decides at its own next request, with the loads of that moment.
func (e *spreadEngine) mark(account, why string, now time.Time) markOutcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := strings.ToLower(account)
	_, was := e.marked[key]
	res := markOutcome{Newly: !was}
	e.marked[key] = why
	v, ok := e.view(now)
	if !ok {
		return res
	}
	exclude := map[string]bool{}
	if a, ok := v.find(account); ok {
		exclude[a.Name] = true
		res.Sessions = e.load(v, now)[a.Name]
	}
	load := e.load(v, now)
	cands := v.sharing(account)
	for len(res.To) < spreadNoticeTargets {
		name, ok := autoswitch.Place(cands, load, "", exclude, v.params, now)
		if !ok {
			break
		}
		res.To = append(res.To, name)
		exclude[name] = true
	}
	return res
}

// unmark clears account's mark: it is a candidate again.
func (e *spreadEngine) unmark(account string) {
	e.mu.Lock()
	delete(e.marked, strings.ToLower(account))
	e.mu.Unlock()
}

// servingFor is the account serving should be: what a new session would get
// (the pin when it is a candidate, else the best score with load counted),
// except that the current serving account is kept while it is still a
// candidate and scores within spreadColdMargin of the best, so a small usage
// change never rewrites state.json.
func (e *spreadEngine) servingFor(pool, current string, now time.Time) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.view(now)
	if !ok {
		return "", false
	}
	load := e.load(v, now)
	cands, pin := v.in(pool)
	best, ok := autoswitch.Place(cands, load, pin, nil, v.params, now)
	if !ok {
		return "", false
	}
	if strings.EqualFold(best, current) || (pin != "" && strings.EqualFold(best, pin)) {
		return best, true
	}
	cur, curOK := v.find(current)
	curOK = curOK && v.isMember(pool, cur.Name)
	bestAcct, _ := v.find(best)
	if curOK && autoswitch.Candidate(cur, v.params, now) {
		score := func(a autoswitch.Account) float64 {
			return autoswitch.Headroom(a, v.params, now) / float64(1+load[a.Name])
		}
		if score(cur)*spreadColdMargin >= score(bestAcct) {
			return cur.Name, true
		}
	}
	return best, true
}

// prune drops the placements of sessions the registry no longer lists, once
// they have gone sessions.Grace without a request.
func (e *spreadEngine) prune(live func(sid string) bool, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.live = live
	for sid := range e.placements {
		last := e.seen[sid]
		if last.Before(e.loadedAt) {
			last = e.loadedAt // never seen since the load: the grace runs from the start
		}
		if !live(sid) && now.Sub(last) > sessions.Grace {
			delete(e.placements, sid)
			delete(e.seen, sid)
			e.dirty = true
		}
	}
}

// snapshot is a copy of the placements.
func (e *spreadEngine) snapshot() map[string]placement {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]placement, len(e.placements))
	for sid, p := range e.placements {
		out[sid] = *p
	}
	return out
}

// tick saves a pending change once the debounce has passed.
func (e *spreadEngine) tick(now time.Time) { e.flush(now, false) }

// close saves any pending change.
func (e *spreadEngine) close() {
	e.flush(time.Time{}, true)
	e.inflight.Wait() // a write another goroutine has in flight finishes first
}

// flush writes placements.json when something changed and, unless force, at
// least spreadSaveEvery has passed since the last write. Only the roster tick
// and close call it: a request merely marks the placements dirty, so a slow
// disk never holds one up. It marshals under e.mu and writes outside it; a
// write older than one already done is dropped, and a failed one is retried
// at the next flush.
func (e *spreadEngine) flush(now time.Time, force bool) {

	e.mu.Lock()
	if !e.dirty || (!force && now.Sub(e.lastSave) < spreadSaveEvery) {
		e.mu.Unlock()
		return
	}
	data, err := json.Marshal(e.placements)
	if err != nil {
		e.mu.Unlock()
		return // placements hold only strings, ints and times
	}
	e.dirty, e.lastSave = false, now
	e.saveSeq++
	seq := e.saveSeq
	e.inflight.Add(1)
	e.mu.Unlock()
	ok := e.writeFile(seq, data)
	e.inflight.Done()
	if !ok {
		e.mu.Lock()
		e.dirty = true
		e.mu.Unlock()
	}
}

// writeFile reports whether the data is safely on disk (or superseded).
func (e *spreadEngine) writeFile(seq uint64, data []byte) bool {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if seq <= e.writtenSeq {
		return true
	}
	err := os.MkdirAll(filepath.Dir(e.path), 0o700)
	if err == nil {
		err = e.write(e.path, data, 0o600)
	}
	if err != nil {
		if msg := err.Error(); msg != e.lastWriteErr {
			fmt.Fprintf(e.log, "chottag: spread: cannot write placements.json: %v\n", err)
			e.lastWriteErr = msg
		}
		return false
	}
	e.writtenSeq, e.lastWriteErr = seq, ""
	return true
}

// evaluateSpread is a spread pool's decision (M7 spec §4 and §5; M8: one per
// pool): it never switches serving for a switch point. The marks (every
// account at a switch point or limited, a notice per account that just
// crossed, naming where its sessions will go, the marks of accounts that are
// candidates again cleared) are made once for all pools by spreadMarks, which
// passes limited here; this keeps the pool's serving at what a new session of
// the pool would get. The first evaluation after a start only seeds the
// marks, silently: a crossing that predates the daemon is not a new event.
//
// With deferNotice (a wall retry) it returns the serving move off a limited
// account as an AutoSwitch for the caller to announce once the resend's
// outcome is known, as serial does. v is the pool's view (poolView); tag is
// the pool name for notices. Caller holds a.mu.
func (a *autoSwitcher) evaluateSpread(now time.Time, deferNotice bool, pool string, v store.State, limited map[string]autoswitch.Account, params autoswitch.Params, tag string) (*status.AutoSwitch, status.Auto) {
	var sw *status.AutoSwitch
	decision := "spread: no account can take a new session"
	if target, ok := a.spread.servingFor(pool, v.Serving, now); ok {
		decision = "spread: new sessions go to " + target
		if !strings.EqualFold(target, v.Serving) {
			if a.spreadServe(now, pool, v.Serving, target) && deferNotice {
				if from, ok := limited[strings.ToLower(v.Serving)]; ok {
					sw = &status.AutoSwitch{From: v.Serving, To: target, Trigger: autoswitch.TriggerLimit, Window: string(from.LimitWindow), At: now}
				}
			}
		}
	}
	return sw, status.Auto{Decision: decision}
}

// spreadOnUsage is onUsage under spread: it only refreshes the marks (and
// their notices), which needs no disk. Serving is kept on the roster tick and
// the wall retry, never on the response path.
func (a *autoSwitcher) spreadOnUsage(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fake(now)
	st, err := a.state()
	if err != nil || !anySpread(st) {
		return
	}
	f := a.sink.fileCopy()
	params := autoParams(st)
	a.spreadMarks(now, st, planAccounts(&st, &f, now), params)
}

// spreadMarks marks every account at a switch point or limited, clears the
// marks of candidates, and posts one notice per account that just crossed. It
// returns the limited accounts, keyed by lowercased name. An account's mark
// is shared by every pool it is in: its limit is the same wherever it is seen
// from. Caller holds a.mu.
func (a *autoSwitcher) spreadMarks(now time.Time, st store.State, accts []autoswitch.Account, params autoswitch.Params) map[string]autoswitch.Account {
	quiet := !a.spreadSeeded
	a.spreadSeeded = true
	multi := len(st.PoolNames()) > 1
	var limited map[string]autoswitch.Account
	for _, pa := range accts {
		if autoswitch.Eligible(pa, params, now) == "" {
			a.spread.unmark(pa.Name)
			delete(a.spreadLimitNoticed, strings.ToLower(pa.Name))
			continue
		}
		if !pa.Rotates || pa.NeedsLogin {
			continue // never a placement target; its sessions leave at their next request
		}
		if pa.Limited {
			if limited == nil {
				limited = map[string]autoswitch.Account{}
			}
			limited[strings.ToLower(pa.Name)] = pa
		}
		window, isLimit := spreadWhy(pa, params, now)
		if res := a.spread.mark(pa.Name, window, now); res.Newly && res.Sessions > 0 && !quiet {
			tag := ""
			if m, ok := findExact(&st, pa.Name); ok && multi {
				// Only the pools that spread move sessions off the account.
				var spreading []string
				for _, p := range accountPools(st, *m) {
					if poolSpread(st, p) {
						spreading = append(spreading, p)
					}
				}
				tag = strings.Join(spreading, ", ")
			}
			a.notify.moved(notify.Moved{From: pa.Name, To: res.To, Sessions: res.Sessions, Window: window, Limited: isLimit, Pool: tag})
		}
	}
	return limited
}

// spreadServe keeps pool's serving at target, the account a new session of
// the pool would get, through the same compare-and-swap auto-switch uses. A
// lost swap (a concurrent `chottag tag`, a `pool leave`) is left for the next
// evaluation; any other failure backs off as trySwitch does. Caller holds
// a.mu.
func (a *autoSwitcher) spreadServe(now time.Time, pool, from, target string) bool {
	p := a.poolState(pool)
	key := from + "->" + target
	if key == p.swapBackoffKey && now.Before(p.swapBackoffUntil) {
		return false
	}
	swapped, err := a.store.SwapPoolServing(pool, from, target)
	if err != nil {
		a.invalidate()
		if swapFailedHarmlessly(err) {
			p.swapBackoffKey, p.swapBackoffUntil, p.swapLastErr = "", time.Time{}, ""
			return false
		}
		p.swapBackoffKey, p.swapBackoffUntil = key, now.Add(swapBackoff)
		if msg := err.Error(); msg != p.swapLastErr {
			fmt.Fprintf(a.log, "chottag: spread: serving %s -> %s failed: %v\n", from, target, err)
			p.swapLastErr = msg
		}
		return false
	}
	a.invalidate()
	p.swapBackoffKey, p.swapLastErr = "", ""
	p.known, p.userChosen, p.fallbackTarget, p.knownDir = target, false, false, ""
	if t, ok := findExact(&swapped, target); ok {
		p.knownDir = t.Dir
	}
	return true
}

// spreadWallRetry is wallRetry for an identified session under spread: the
// limit hit moves only this session, to another member of its pool, and the
// proxy resends on it. The limited account is marked by the evaluation, so
// its other sessions follow at their next request. The retry is reported as
// serial reports one: a "switched" notice once the resend's outcome is known,
// once per limited account until it is a candidate again.
func (a *autoSwitcher) spreadWallRetry(pool, tag, sid, account string, now time.Time, window string) (bool, func(string, int)) {
	chosen, moved := "", true
	if cur, ok := a.spread.placed(sid); !ok || strings.EqualFold(cur, account) {
		chosen, moved = a.spread.replaceIn(sid, pool, now)
	} else {
		chosen = cur // a concurrent request already moved it: resend on its new account
	}
	a.evaluate(now, false)
	if !moved {
		return false, nil
	}
	return true, func(to string, code int) {
		dest, retried := chosen, false
		if to != "" {
			fmt.Fprintln(a.log, wallRetryLine(account, to, code, window))
			dest, retried = to, code >= 200 && code < 300
		}
		// As serial's retried does: the notice names where the request went,
		// or where it was to go, even if the resend never happened. The
		// once-per-account flag is taken only when a notice is posted.
		key := strings.ToLower(account)
		a.mu.Lock()
		first := !a.spreadLimitNoticed[key]
		if first {
			if a.spreadLimitNoticed == nil {
				a.spreadLimitNoticed = map[string]bool{}
			}
			a.spreadLimitNoticed[key] = true
		}
		a.mu.Unlock()
		if first {
			a.notify.switched(notify.Switch{From: account, To: dest, Trigger: autoswitch.TriggerLimit, Window: window, Retried: retried, Pool: tag, Episode: pool})
		}
	}
}

// spreadWhy says why pa is marked: the window ("5h", "7d", "" if unknown)
// and whether it is a limit rather than a switch point.
func spreadWhy(pa autoswitch.Account, p autoswitch.Params, now time.Time) (window string, limit bool) {
	if pa.Limited {
		return string(pa.LimitWindow), true
	}
	for _, w := range []autoswitch.Window{autoswitch.Win5h, autoswitch.Win7d} {
		pct, reset, has := pa.Pct5h, pa.Reset5h, pa.Has5h
		if w == autoswitch.Win7d {
			pct, reset, has = pa.Pct7d, pa.Reset7d, pa.Has7d
		}
		if !has || (!reset.IsZero() && !reset.After(now)) {
			continue
		}
		if pt := p.SwitchPoint(w, pa.Tier); pct >= 100 || pct >= float64(pt) {
			return string(w), false
		}
	}
	return "", false
}

// fallback is the account to send a request of pool as when no candidate can
// take the session (R90): the pool's serving account when it rotates (ok is
// false, the caller uses serving as before); otherwise the least-bad
// rotating member of the pool, preferring one that is neither limited nor needing a
// login, then a limited one over one needing a login, then the lowest usage. ok is also false when no account rotates at
// all, where serving is all there is.
func (e *spreadEngine) fallback(pool string, now time.Time) (string, bool) {
	return e.fallbackExcluding(pool, nil, now)
}

// fallbackExcluding is fallback for an account that was tried and cannot
// serve (rotation off, no usable login) though it may rotate: with exclude
// set, the serving-rotates shortcut is skipped and no account in it (keyed by
// lowercased name) is a choice.
func (e *spreadEngine) fallbackExcluding(pool string, exclude map[string]bool, now time.Time) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.view(now)
	if !ok {
		return "", false
	}
	cands, _ := v.in(pool)
	serving := v.st.PoolOf(pool).Serving
	if !v.st.HasPool(pool) {
		serving = v.st.Serving
	}
	if sv, ok := v.find(serving); ok && sv.Rotates && len(exclude) == 0 {
		return "", false
	}
	best, bestKey, found := "", [2]float64{}, false
	for _, a := range cands {
		if !a.Rotates || exclude[strings.ToLower(a.Name)] {
			continue
		}
		// A needs-login account has no token: the request would go out on
		// Claude Code's own login. A limited one at least draws a clean 429.
		bad := 0.0
		switch {
		case a.NeedsLogin:
			bad = 2
		case a.Limited:
			bad = 1
		}
		key := [2]float64{bad, max(pctOr0(a.Has5h, a.Pct5h), pctOr0(a.Has7d, a.Pct7d))}
		if !found || key[0] < bestKey[0] || (key[0] == bestKey[0] && key[1] < bestKey[1]) {
			best, bestKey, found = a.Name, key, true
		}
	}
	return best, found
}

func pctOr0(has bool, pct float64) float64 {
	if has {
		return pct
	}
	return 0
}
