package usagepoll

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Grace is how long after a limited window's reset its reset poll runs, so
// the server has rolled the window over by then (R47).
const Grace = 60 * time.Second

// Stagger spaces the polls one roster sync makes due at once, such as the
// daemon-start polls (spec §6.4: "staggered ~2s apart").
const Stagger = 2 * time.Second

// backoff is the reset-time-unknown schedule: 15, then 30, then every 60
// minutes (R47).
var backoff = [...]time.Duration{15 * time.Minute, 30 * time.Minute, 60 * time.Minute}

// TokenRetry is how long a start poll skipped for no-token waits before
// its one retry: long enough for the background refresh Token started
// (F159).
const TokenRetry = 30 * time.Second

// IdleEvery is how long after an account's last usage update its idle poll
// is due (F268), and OffEvery the same for a rotation-off account. Each is
// spread by +-IdleJitter, so accounts do not all come due together.
const (
	IdleEvery  = 30 * time.Minute
	OffEvery   = 2 * time.Hour
	IdleJitter = 0.1
)

// idleBackoff is an idle poll's schedule after a 429 that carried no usable
// Retry-After: 60 minutes, then 120 for every further one (F268).
var idleBackoff = [...]time.Duration{60 * time.Minute, 120 * time.Minute}

type kind int

const (
	kindStart kind = iota + 1 // "no data": missing or stale usage
	kindReset                 // re-check a limited account
	kindIdle                  // keep an unlimited account's usage from going stale
)

func (k kind) String() string {
	switch k {
	case kindStart:
		return "start"
	case kindIdle:
		return "idle"
	}
	return "reset"
}

// entry is one account's pending poll. due is a wall-clock time. step is
// an index into backoff for a reset entry's next failure; a start entry
// never uses it, since it never repeats beyond its one no-token retry.
// tokenRetried marks that this poll is that one no-token retry, on either
// kind (F159, final review): a second NoToken on it falls through to the ordinary
// path instead of retrying again.
type entry struct {
	due          time.Time
	kind         kind
	step         int
	tokenRetried bool
}

// acct is the poller's state for one registered slot account.
type acct struct {
	name, dir string
	pending   *entry // at most one pending poll per account
	inflight  bool
	gen       uint64 // bumped by Observed; a poll that lost a race to it reschedules nothing
	limited   bool
	until     time.Time
	rotates   bool
	loggedIn  time.Time
	// needsLogin: the account has no usable login, so nothing is polled
	// until the status row stops saying so, or the account logs in again.
	needsLogin bool
	// waiters are PollNow callers waiting for this account's poll to end.
	waiters []chan bool
}

// Config configures a Poller. Fetch, Cached and Apply are required.
type Config struct {
	Fetch Fetcher
	// Cached reads the status cache for an account joining the roster. It
	// is never called with the poller's lock held.
	Cached func(account string, now time.Time) CacheView
	// Apply writes one Result into the status cache. It is never called
	// with the poller's lock held.
	Apply func(account string, r Result, sent time.Time) Applied
	// Log gets one line per poll (daemon.log). nil discards.
	Log io.Writer
	// Now is the wall clock. Default: time.Now with the monotonic reading
	// stripped.
	Now func() time.Time
	// After is the timer. Default: time.After.
	After func(time.Duration) <-chan time.Time
	// Rand returns a number in [0,1) for the idle schedule's jitter.
	// Default: math/rand.
	Rand func() float64
	// OnNeedsLogin is told, outside the poller's lock, that a poll found
	// account without a usable login. The daemon records the token state
	// and posts the notice there.
	OnNeedsLogin func(account string)
	// OnOK is told, outside the poller's lock, that a poll of account wrote
	// usage: the account's login works.
	OnOK func(account string)
}

// Poller is the events-only usage poll scheduler (spec §6.4).
//
// Lock order: the status sink's lock may be held while calling Observed
// (it is, by design); the poller never holds its own lock while calling
// Fetch, Cached or Apply. So the two locks are only ever taken sink first.
type Poller struct {
	cfg   Config
	kick  chan struct{}
	mu    sync.Mutex
	accts map[string]*acct
}

// New builds a Poller. It panics on a missing required Config field.
func New(cfg Config) *Poller {
	if cfg.Fetch == nil || cfg.Cached == nil || cfg.Apply == nil {
		panic("usagepoll: Config needs Fetch, Cached and Apply")
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Now == nil {
		cfg.Now = wallNow
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	return &Poller{cfg: cfg, kick: make(chan struct{}, 1), accts: map[string]*acct{}}
}

func key(name string) string { return strings.ToLower(name) }

// nudge wakes Run to recompute its next deadline. Never blocks: one
// queued nudge already covers every change made before Run reads it.
func (p *Poller) nudge() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// SyncRoster makes the poller's roster match accounts (every registered
// slot account; one with no Dir is skipped, since only a slot can be
// polled). The daemon calls it once at start and on every roster tick.
//
// An account seen for the first time is seeded from the cache: a limited
// one gets a reset poll at its reset plus Grace (now, if that has passed
// or is unknown), and one with missing or stale usage gets a start poll.
// The polls due now are staggered Stagger apart. An account no longer
// listed is dropped with its pending poll.
func (p *Poller) SyncRoster(accounts []Account) {
	now := p.cfg.Now()
	p.mu.Lock()
	known := make(map[string]bool, len(p.accts))
	for k := range p.accts {
		known[k] = true
	}
	p.mu.Unlock()

	type seed struct {
		a      Account
		v      CacheView
		resume bool // a stopped needs-login account that may poll again
	}
	var seeds []seed
	cur := make(map[string]Account, len(accounts))
	stopped := map[string]bool{}
	p.mu.Lock()
	for k, st := range p.accts {
		stopped[k] = st.needsLogin
	}
	p.mu.Unlock()
	for _, a := range accounts {
		if a.Dir == "" {
			continue
		}
		cur[key(a.Name)] = a
		if !known[key(a.Name)] {
			// Outside p.mu: Cached takes the status sink's lock.
			seeds = append(seeds, seed{a: a, v: p.cfg.Cached(a.Name, now)})
		} else if stopped[key(a.Name)] {
			if v := p.cfg.Cached(a.Name, now); !needsLogin(v, a) {
				seeds = append(seeds, seed{a: a, v: v, resume: true})
			}
		}
	}

	p.mu.Lock()
	for k, st := range p.accts {
		if a, ok := cur[k]; ok {
			// A case-only rename keeps the key; take the configured
			// spelling for the writes and log lines (F175).
			st.dir, st.name = a.Dir, a.Name
			st.rotates, st.loggedIn = a.Rotates, a.LoggedInAt
		} else {
			delete(p.accts, k)
		}
	}
	slot := 0
	for _, s := range seeds {
		k := key(s.a.Name)
		if st, ok := p.accts[k]; ok {
			if s.resume && st.needsLogin {
				st.needsLogin = false
				st.limited, st.until = s.v.Limited, s.v.Until
				st.pending = p.seedEntry(s.a, s.v, now, &slot)
			}
			continue
		}
		p.accts[k] = &acct{
			name: s.a.Name, dir: s.a.Dir, rotates: s.a.Rotates, loggedIn: s.a.LoggedInAt,
			limited: s.v.Limited, until: s.v.Until,
			needsLogin: needsLogin(s.v, s.a),
		}
		p.accts[k].pending = p.seedEntry(s.a, s.v, now, &slot)
	}
	p.mu.Unlock()
	p.nudge()
}

// needsLogin reports whether the status row says a has no usable login: the
// row's mark counts until the account logs in again.
func needsLogin(v CacheView, a Account) bool {
	return v.NeedsLogin && !a.LoggedInAt.After(v.TokenAt)
}

// every is how long after an update an account's idle poll is due, before
// jitter.
func (st *acct) every() time.Duration {
	if st.rotates {
		return IdleEvery
	}
	return OffEvery
}

// jittered spreads d by +-IdleJitter.
func (p *Poller) jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 + IdleJitter*(2*p.cfg.Rand()-1)))
}

// idleEntry is st's next idle poll, due after from + its interval and no
// closer than Stagger to another account's pending idle poll. Caller holds
// p.mu.
func (p *Poller) idleEntry(st *acct, from time.Time, step int) *entry {
	return p.idleAt(st, from.Add(p.jittered(st.every())), step)
}

// idleAt is an idle entry due at due, moved later while another account's
// pending idle poll sits within Stagger of it. Caller holds p.mu.
func (p *Poller) idleAt(st *acct, due time.Time, step int) *entry {
	for moved := true; moved; {
		moved = false
		for _, o := range p.accts {
			if o == st || o.pending == nil || o.pending.kind != kindIdle {
				continue
			}
			if d := o.pending.due.Sub(due); d > -Stagger && d < Stagger {
				due, moved = o.pending.due.Add(Stagger), true
			}
		}
	}
	return &entry{due: due, kind: kindIdle, step: step}
}

// seedEntry is the first pending poll for an account that just joined or
// resumed. Caller holds p.mu.
func (p *Poller) seedEntry(a Account, v CacheView, now time.Time, slot *int) *entry {
	staggered := func(k kind) *entry {
		e := &entry{due: now.Add(time.Duration(*slot) * Stagger), kind: k}
		*slot++
		return e
	}
	st := p.accts[key(a.Name)]
	switch {
	case st.needsLogin:
		return nil
	case v.Limited && !v.Until.IsZero() && v.Until.Add(Grace).After(now):
		return &entry{due: v.Until.Add(Grace), kind: kindReset}
	case v.Limited:
		return staggered(kindReset)
	case !v.Fresh:
		return staggered(kindStart)
	}
	// Fresh usage: its idle poll is due an interval after its last update.
	e := p.idleEntry(st, v.UpdatedAt, 0)
	if e.due.Before(now) {
		e = staggered(kindIdle)
	}
	return e
}

// SetOnNeedsLogin installs Config.OnNeedsLogin after construction, for a
// daemon that builds the poller before the notifier it reports to.
func (p *Poller) SetOnNeedsLogin(fn func(account string)) {
	p.mu.Lock()
	p.cfg.OnNeedsLogin = fn
	p.mu.Unlock()
}

// SetOnOK installs Config.OnOK after construction.
func (p *Poller) SetOnOK(fn func(account string)) {
	p.mu.Lock()
	p.cfg.OnOK = fn
	p.mu.Unlock()
}

// Observed is the status sink's hook after an observation that carried
// usage headers: traffic wins over polling (spec §6.4). An unlimited
// account loses any pending poll; a limited one gets a reset poll at the
// observed reset plus Grace, or, with the reset unknown, keeps its pending
// reset poll or starts the backoff. An account not on the roster (never
// Home, which has no slot) is ignored.
//
// Called with the sink's lock held: it takes only the poller's lock, does
// no I/O and never blocks.
func (p *Poller) Observed(account string, limited bool, until time.Time) {
	now := p.cfg.Now()
	p.mu.Lock()
	st := p.accts[key(account)]
	if st == nil {
		p.mu.Unlock()
		return
	}
	st.gen++
	st.limited, st.until = limited, until
	// Traffic answered on the account's own login: it has one.
	st.needsLogin = false
	switch {
	case !limited:
		st.pending = p.idleEntry(st, now, 0)
	case !until.IsZero() && until.Add(Grace).After(now):
		st.pending = &entry{due: until.Add(Grace), kind: kindReset}
	case st.pending == nil || st.pending.kind != kindReset:
		st.pending = &entry{due: now.Add(backoff[0]), kind: kindReset, step: 1}
	}
	p.mu.Unlock()
	p.nudge()
}

// Wake is the wake detector's hook. Deadlines are wall-clock times, so a
// pending poll whose time passed during sleep is simply overdue once Run
// recomputes; Wake also re-queues a limited account that has no pending
// poll (a 401 gave up on it) when its reset has passed or is unknown.
// Never blocks.
func (p *Poller) Wake() {
	now := p.cfg.Now()
	p.mu.Lock()
	for _, st := range p.accts {
		if st.limited && st.pending == nil && !st.inflight && !st.until.After(now) {
			st.pending = &entry{due: now, kind: kindReset}
		}
	}
	p.mu.Unlock()
	p.nudge()
}

// Run is the one worker: it performs due polls one at a time until ctx is
// cancelled. Cancelling ctx drops an in-flight request; its result is
// neither written nor logged.
func (p *Poller) Run(ctx context.Context) {
	for {
		wait, ok := p.runDue(ctx)
		var fire <-chan time.Time
		if ok {
			fire = p.cfg.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		case <-fire:
		}
	}
}

// job is one popped poll.
type job struct {
	st           *acct
	name, dir    string
	kind         kind
	step         int
	tokenRetried bool
	gen          uint64
}

// runDue performs every poll due at the current time, one at a time, and
// reports how long until the next pending one (ok false: none pending).
func (p *Poller) runDue(ctx context.Context) (time.Duration, bool) {
	for ctx.Err() == nil {
		now := p.cfg.Now()
		j, next, found := p.pop(now)
		if !found {
			if next.IsZero() {
				return 0, false
			}
			return next.Sub(now), true
		}
		p.poll(ctx, j)
	}
	return 0, false
}

// pop takes the earliest due poll (ties by name), or reports the earliest
// future due time (zero: nothing pending).
func (p *Poller) pop(now time.Time) (job, time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *acct
	for _, st := range p.accts {
		if st.pending == nil {
			continue
		}
		if best == nil || st.pending.due.Before(best.pending.due) ||
			(st.pending.due.Equal(best.pending.due) && st.name < best.name) {
			best = st
		}
	}
	if best == nil {
		return job{}, time.Time{}, false
	}
	if best.pending.due.After(now) {
		return job{}, best.pending.due, false
	}
	e := best.pending
	best.pending, best.inflight = nil, true
	return job{st: best, name: best.name, dir: best.dir, kind: e.kind, step: e.step, tokenRetried: e.tokenRetried, gen: best.gen}, time.Time{}, true
}

// poll performs one job: fetch, write, log, reschedule.
//
// Fetch runs in its own goroutine so a cancelled ctx (daemon stop) can drop
// it at once instead of waiting for it to return: on a 401, Fetch calls
// Tokens.ForceRefresh, and tokens.go deliberately runs that refresh with
// context.WithoutCancel and its own 2-minute timeout, to protect token
// rotation from an unrelated stop. Blocking here on that call would make
// daemon stop wait up to 2 minutes past its SIGKILL deadline. The result
// channel is 1-buffered, so the abandoned goroutine's send never blocks
// once it finishes on its own; nothing reads it, so it is simply dropped.
func (p *Poller) poll(ctx context.Context, j job) {
	sent := p.cfg.Now()
	result := make(chan Outcome, 1)
	go func() { result <- p.cfg.Fetch(ctx, j.dir) }()
	var out Outcome
	select {
	case <-ctx.Done():
		p.mu.Lock()
		j.st.inflight = false
		p.release(j.st, false)
		p.mu.Unlock()
		return
	case out = <-result:
	}
	if ctx.Err() != nil {
		// A result that arrived at the same moment as the cancel: still
		// nothing is written or logged after this point, so nothing can
		// land after the sink closes.
		p.mu.Lock()
		j.st.inflight = false
		p.release(j.st, false)
		p.mu.Unlock()
		return
	}
	if out.NeedsLogin && p.registered(j) {
		p.mu.Lock()
		fn := p.cfg.OnNeedsLogin
		p.mu.Unlock()
		if fn != nil {
			// Outside p.mu: it writes the status cache and posts a notice.
			fn(j.name)
		}
	}
	var ap Applied
	if out.OK && p.registered(j) {
		ap = p.cfg.Apply(j.name, out.Result, sent)
	}
	fmt.Fprintf(p.cfg.Log, "chottag: usage poll %s %s %s %s\n", j.name, j.kind, out.Status, p.cfg.Now().Sub(sent).Round(time.Millisecond))
	p.finish(j, out, ap)
	if out.OK && ap.Written {
		p.mu.Lock()
		fn := p.cfg.OnOK
		p.mu.Unlock()
		if fn != nil {
			fn(j.name) // outside p.mu: it writes the status cache
		}
	}
}

// release wakes the PollNow callers waiting on st. Caller holds p.mu.
func (p *Poller) release(st *acct, ok bool) {
	for _, w := range st.waiters {
		w <- ok // 1-buffered
	}
	st.waiters = nil
}

// PollSkip says why PollNow would not poll account right now ("" when it
// would): not on the roster, needs login, limited, or backing off a 429. It
// changes nothing.
func (p *Poller) PollSkip(account string) string {
	now := p.cfg.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skipReasonLocked(p.accts[key(account)], now)
}

// skipReasonLocked is PollSkip's rule. Caller holds p.mu.
func (p *Poller) skipReasonLocked(st *acct, now time.Time) string {
	switch {
	case st == nil:
		return "not on the roster"
	case st.needsLogin:
		return "needs login"
	case st.limited:
		return "limited"
	}
	if e := st.pending; !st.inflight && e != nil && e.kind == kindIdle && e.step > 0 && e.due.After(now) {
		// Backing off a 429 (its Retry-After, else 60 then 120 minutes): the
		// server asked for quiet, and a caller that wants an answer sooner
		// does not get to override it.
		return "in a 429 backoff"
	}
	return ""
}

// PollNow asks for account's poll to run at once, ahead of its schedule,
// and waits for it to end (or ctx to be done). It reports whether a poll
// went out and parsed. The poll runs on the one worker, so it never
// overlaps another; an account that is limited, needs a login or is not on
// the roster is not polled (false). The pre-switch poll of an old target
// uses it (F268).
func (p *Poller) PollNow(ctx context.Context, account string) bool {
	now := p.cfg.Now()
	ch := make(chan bool, 1)
	p.mu.Lock()
	st := p.accts[key(account)]
	if p.skipReasonLocked(st, now) != "" {
		p.mu.Unlock()
		return false
	}
	st.waiters = append(st.waiters, ch)
	if !st.inflight {
		if st.pending == nil {
			st.pending = &entry{due: now, kind: kindIdle}
		} else {
			st.pending.due = now
		}
	}
	p.mu.Unlock()
	p.nudge()
	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		return false
	}
}

// registered reports whether j's account is still on the roster.
func (p *Poller) registered(j job) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accts[key(j.name)] == j.st
}

// finish records what a poll learned and schedules the account's next
// poll, unless an event already decided it while the poll was in flight.
func (p *Poller) finish(j job, out Outcome, ap Applied) {
	now := p.cfg.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Under the same lock as the reschedule, so a PollNow that arrives after
	// it cannot be handed this poll's answer.
	defer p.release(j.st, out.OK)
	j.st.inflight = false
	if p.accts[key(j.name)] != j.st || j.st.gen != j.gen || j.st.pending != nil {
		return // removed, observed or re-queued while in flight
	}
	if out.NeedsLogin {
		// Nothing is polled until the status row stops saying so, or the
		// account logs in again (F269).
		j.st.needsLogin = true
		return
	}
	if out.OK {
		if !ap.Written {
			return // fresher observed data won; its Observed call already scheduled
		}
		j.st.limited, j.st.until = ap.Limited, ap.Until
		switch {
		case !ap.Limited:
			j.st.pending = p.idleEntry(j.st, now, 0)
		case !ap.Until.IsZero() && ap.Until.Add(Grace).After(now):
			j.st.pending = &entry{due: ap.Until.Add(Grace), kind: kindReset}
		default:
			// Still limited with the reset unknown, or already past: back
			// off rather than poll again at once.
			j.st.pending = backoffEntry(now, j.step)
		}
		return
	}
	if out.NoToken && !j.tokenRetried {
		// Skipped, not failed: the token was refreshing in the background
		// (F159 for a start poll; the same one-time retry now applies to
		// a reset poll too (final review), so a limited account is not stuck for a
		// full backoff step just because the retry raced a refresh).
		// Keep kind and step; a second NoToken falls through below.
		j.st.pending = &entry{due: now.Add(TokenRetry), kind: j.kind, step: j.step, tokenRetried: true}
		return
	}
	if j.kind != kindReset {
		// A failed start or idle poll joins the idle schedule (F268): a 429
		// waits its Retry-After, else 60 then 120 minutes; any other failure
		// waits the ordinary interval.
		if out.Status == "429" || out.Status == "401>429" || out.GaveUp {
			// GaveUp: a 401 that survived a refresh costs a claude spawn per
			// retry, so it backs off like a 429 instead of every interval.
			wait := out.RetryAfter
			if wait <= 0 {
				wait = max(idleBackoff[min(j.step, len(idleBackoff)-1)], j.st.every())
			}
			j.st.pending = p.idleAt(j.st, now.Add(wait), j.step+1)
			return
		}
		j.st.pending = p.idleEntry(j.st, now, 0)
		return
	}
	if out.GaveUp {
		return // a 401 waits for the next event
	}
	j.st.pending = backoffEntry(now, j.step)
}

func backoffEntry(now time.Time, step int) *entry {
	return &entry{due: now.Add(backoff[min(step, len(backoff)-1)]), kind: kindReset, step: step + 1}
}
