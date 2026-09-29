package cli

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
)

// statusProbe reports whether a daemon answers on port and its health
// version (public release design §2.4). A var, not a direct shim.ProbeHealth
// call, so TestMain (invariants_test.go) can arm a safe "no daemon" default
// rather than a panic: unlike doctorNet's probe, most existing status tests
// never stub this at all, and status --json's own contract already promises
// nothing else changes for them (spec §5.1, additive only).
var statusProbe = func(port int) (bool, string) { ok, h := shim.ProbeHealth(port); return ok, h.Version }

// SetStatusProbeForTest swaps statusProbe and returns a func that restores
// whatever was installed at the moment of the call, mirroring
// SetDoctorNetForTest (F130).
func SetStatusProbeForTest(fn func(port int) (bool, string)) (restore func()) {
	orig := statusProbe
	statusProbe = fn
	return func() { statusProbe = orig }
}

// statusIdentity is `status`'s daemon health identity (F221, part 1 T11):
// the same daemonIdentity doctor's own doctorIdentity seam wires (F103,
// one way to decide a daemon is ours). A var, like statusProbe above, so
// TestMain can arm a safe "none" default (Ruling 24) rather than a panic:
// most existing status tests never stub this either.
var statusIdentity = daemonIdentity

// SetStatusIdentityForTest swaps statusIdentity and returns a func that
// restores whatever was installed at the moment of the call, mirroring
// SetStatusProbeForTest (F130).
func SetStatusIdentityForTest(fn func(h string, port int) string) (restore func()) {
	orig := statusIdentity
	statusIdentity = fn
	return func() { statusIdentity = orig }
}

// runStatus prints chottag's derived view of each account: which is serving,
// which is remote, its usage windows and whether it is silently passing
// through on Home's login instead of the account the user chose (F20).
//
// Flag-parse errors and "chottag: ..." failures go to stderr, never stdout:
// stdout is the report itself, and with --json it is exactly one document,
// an error document on a failure (spec §5.3) — `chottag status --json | jq`
// never chokes on an error line mixed in.
func runStatus(home string, args []string, r *reporter) int {
	// --json is the global pre-pass's (spec §5.3), so this FlagSet has no
	// flags of its own; it still rejects an unknown one with exit 2. A bare
	// fs.Parse(args) would stop at the first non-flag token and silently
	// leave any trailing token — a bogus flag included — unparsed in
	// fs.Args() without ever being checked (fix round 4, item 2);
	// parseInterspersed, plus an explicit positional check below, catches
	// both.
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage("usage: chottag status")
	}

	st, err := (store.Store{Dir: home}).Load()
	if err != nil {
		return r.FailErr(err)
	}
	f, err := status.Load(status.Path(home))
	if err != nil {
		return r.FailErr(err)
	}

	// EnsureAccounts against the CONFIGURED account set (state.json), not
	// just the accounts the cache happens to have observed: otherwise an
	// account chottag has never seen traffic for would simply be missing
	// from the report, rather than shown as unknown (contract 2).
	f.EnsureRoster(accountMembers(st))
	for i := range f.Accounts {
		// The slot dir is the daemon's matching key, not part of the
		// report (F171).
		f.Accounts[i].Dir = ""
		if a, ok := findExact(&st, f.Accounts[i].Name); ok {
			f.Accounts[i].Email = a.Email
			f.Accounts[i].Org = a.Org
			f.Accounts[i].Rotate = !a.NoRotate
			f.Accounts[i].Plan = a.Plan
		}
	}
	f.Serving, f.Remote = st.Serving, st.Remote
	// Report in the account's configured (registration) order, not the
	// order the cache happens to have observed them in — a user reading
	// down the list expects it to match `chottag adopt`'s order, not
	// whichever account most recently sent a request.
	orderAccounts(&f, accountNames(st))

	now := time.Now()
	// Stale states each account's freshness explicitly (spec §6.3, contract
	// 5): the human table already renders "unknown" instead of a number for
	// a stale account (via Fresh, below), but --json used to keep emitting
	// the raw, no-longer-trustworthy percentages with nothing saying not to
	// trust them. Set for both surfaces from the same clock, so they always
	// agree.
	for i := range f.Accounts {
		f.Accounts[i].Stale = !f.Fresh(f.Accounts[i].Name, now)
	}

	// Daemon version overlay (public release design §2.4): a report overlay
	// like Running, set fresh here for BOTH surfaces (text and --json) before
	// either renders or marshals f. Probed only while the daemon reads as
	// running (the same heartbeat-freshness DaemonRunningAt already computes
	// for Running): a stale cache's stored port is not worth probing once
	// nothing is listening on it anymore, and a home that has never run a
	// daemon has no Daemon object to probe a port from at all.
	f.DaemonRunningAt(now)
	if f.Daemon != nil && f.Daemon.Running {
		if ok, version := statusProbe(f.Daemon.Port); ok {
			if version != "" {
				f.Daemon.Version = version
				f.Daemon.VersionMismatch = version != Version
			}
			// The identity overlay (F221, part 1 T11): run whenever
			// anything chottag-shaped answered, not only when it also
			// reported a version (final review N4) — a squatter that
			// answers `{"chottag":true}` with no version at all must still
			// get a mismatch verdict here, the same as doctor's own
			// daemon-identity row already gives it, rather than silently
			// no identity line at all.
			f.Daemon.Identity = statusIdentity(home, f.Daemon.Port)
		}
	}

	if r.JSON() {
		if f.Accounts == nil {
			// encoding/json renders a nil slice as null, not []: an empty
			// home would otherwise make `chottag status --json | jq
			// '.accounts[]'` fail on the very first run.
			f.Accounts = []status.Account{}
		}
		// RollUpAt, not the cache's own (clockless) Limits: a LimitedUntil
		// that has already passed must stop asserting a reset date that has
		// gone by (contract 2).
		f.RollUpAt(now)
		// Same rule as RollUpAt on the line above: liveness is a function of
		// when the document is READ. A stored running:true written moments
		// before the daemon was killed must not outlive it (§5.1).
		f.DaemonRunningAt(now)
		// The per-account `limited` flag is also made clock-aware here, in
		// the --json projection only (the cache file on disk keeps the raw
		// observed fact — Observe needs it, and this is the human table's
		// job for the table). A stale limitedUntil that never flips is the
		// same self-sustaining trap relocated from the human table to the
		// machine surface: `limits.allLimited` would say not every account
		// is limited while that account's own row still claims limited:
		// true, and a consumer reading .accounts[].limited directly (spec
		// §5.1: obvious fields get read, not the surrounding prose) would
		// never see it clear, because M1c has no poll and status just told
		// it not to send the traffic that would. limitedUntil itself is
		// left in the output so a consumer can still see what elapsed. A
		// zero limitedUntil is untouched: it means UNKNOWN, never expired.
		for i := range f.Accounts {
			a := &f.Accounts[i]
			if a.Limited && !a.LimitedUntil.IsZero() && !a.LimitedUntil.After(now) {
				a.Limited = false
			}
		}
		// The reporter splices the §5.3 header (ok, warnings) after f's own
		// version and writes the rest exactly as MarshalIndent did (§5.1:
		// additive only).
		return r.OK(f)
	}

	renderStatus(r.Stdout(), f, now)
	fmt.Fprintln(r.Stdout(), autoStatusLine(st, f, now))
	return r.OK(nil)
}

// autoStatusLine is `chottag status`'s auto-switch line (M4 spec §7):
// "auto: balanced · holding A (5h 96%, resets in 9m) · last C→A 09:12
// (limit)", or "auto: off". The decision is the daemon's, shown only while
// its heartbeat is fresh: a dead daemon's last decision is not what will
// happen next. The last switch stays true history either way.
func autoStatusLine(st store.State, f status.File, now time.Time) string {
	if !st.AutoOn() {
		return "auto: off"
	}
	parts := []string{"auto: " + string(autoParams(st).Mode)}
	if f.Auto != nil {
		f.DaemonRunningAt(now)
		if f.Auto.Decision != "" && f.Daemon != nil && f.Daemon.Running {
			parts = append(parts, f.Auto.Decision)
		}
		if ls := f.Auto.LastSwitch; ls != nil {
			parts = append(parts, lastSwitchText(*ls))
		}
	}
	return strings.Join(parts, " · ")
}

// lastSwitchText is "last C→A 09:12 (limit)"; the time is omitted when
// unknown (F25).
func lastSwitchText(ls status.AutoSwitch) string {
	s := "last " + ls.From + "→" + ls.To
	if !ls.At.IsZero() {
		s += " " + ls.At.Local().Format("15:04")
	}
	return s + " (" + ls.Trigger + ")"
}

func renderStatus(out io.Writer, f status.File, now time.Time) {
	fmt.Fprintf(out, "serving: %s   remote: %s\n\n", f.Serving, f.Remote)
	// The daemon version-mismatch line (public release design §2.4): shown
	// only when the overlay above (runStatus) found a running daemon whose
	// health version differs from this binary's. VersionMismatch is never
	// true without Version also being set (runStatus sets both together),
	// so this alone is enough to decide whether to print. Skipped for a
	// legacy daemon (review round 1 finding 7): Ruling 30 means a legacy
	// daemon's health version always differs from this build's, so the two
	// lines would otherwise always print together, both ending in the
	// identical "run: chottag daemon restart" — the identity line below
	// already says everything the version line would have added here.
	// Skipped for a mismatch too (final review N3): its own line below
	// already says everything this one would, "run: chottag daemon
	// restart" is the WRONG advice for a squatter (there is no legitimate
	// daemon on that port to restart), and Version here is that unverified
	// listener's own self-reported, unauthenticated claim.
	if f.Daemon != nil && f.Daemon.VersionMismatch && f.Daemon.Identity != string(shim.IdentityLegacy) && f.Daemon.Identity != string(shim.IdentityMismatch) {
		fmt.Fprintf(out, "daemon: running %s, installed %s (run: chottag daemon restart)\n\n", f.Daemon.Version, Version)
	}
	// The daemon-identity lines (F221, part 1 T11): shown only when the
	// overlay above found a running daemon that answered but did not
	// verify as this install's own. Independent of the version-mismatch
	// line above — a mismatched daemon may or may not also report a
	// different version — so both can print for that one (never for
	// legacy, per the skip above).
	switch {
	case f.Daemon != nil && f.Daemon.Identity == string(shim.IdentityLegacy):
		fmt.Fprint(out, "daemon: running without proxy authentication (run: chottag daemon restart)\n\n")
	case f.Daemon != nil && f.Daemon.Identity == string(shim.IdentityMismatch):
		fmt.Fprintf(out, "daemon: port %d answered but did not prove it is this install's daemon (run: chottag doctor)\n\n", f.Daemon.Port)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tPLAN\tORG\t5h\t7d\tSTATE")
	for _, a := range f.Accounts {
		fresh := f.Fresh(a.Name, now)
		five, seven := "unknown", "unknown"
		if fresh {
			five = pctOrUnknown(a.Usage.FiveHourPct)
			seven = pctOrUnknown(a.Usage.SevenDayPct)
		}
		state := "-"
		switch {
		case a.Passthrough != "":
			// Requests for this account are actually going out on Home's
			// login: say why, or the user believes they switched when they
			// did not (F20, contract 3).
			state = "passthrough: " + a.Passthrough
		case a.Limited:
			switch {
			case a.LimitedUntil.IsZero():
				// Zero means UNKNOWN, never "already expired" (contract 5):
				// do not print a date, and do not imply the limit has lifted.
				state = "limited (reset time unknown)"
			case !a.LimitedUntil.After(now):
				// The reset date has already gone by, and M1c has no poll to
				// confirm the account actually recovered: say so in words,
				// never print a date that has already passed (it reads as
				// "already expired," which is the opposite of what it means
				// here — unconfirmed, not cleared).
				state = "limited (reset time passed; unconfirmed)"
			default:
				state = "limited until " + a.LimitedUntil.Local().Format("Jan 2 15:04")
			}
		case fresh:
			state = "ok"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", a.Name, planLabel(store.Account{Plan: a.Plan}), a.Org, five, seven, state)
	}
	tw.Flush()
}

// pctOrUnknown renders a usage.Window's utilization. p is nil when the
// server reported no utilization for that window: render "unknown", never
// "0%" (contract 5). p is already 0-100 (status.Usage's wire units), not the
// 0-1 fraction usage.Window carries.
func pctOrUnknown(p *float64) string {
	if p == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// orderAccounts sorts f.Accounts to match order (state.json's registration
// order), stably, so any cache-only leftover (an account since removed)
// sorts after every currently configured one rather than disturbing it.
//
// The !iok/!jok branches below are currently unreachable from runStatus,
// this function's only caller (fix round 2, item 6): runStatus always calls
// EnsureRoster(accountMembers(st)) before orderAccounts(&f, accountNames(st))
// over the SAME roster, and EnsureRoster prunes every row whose account is
// not in it and gives each kept row its configured name — so by the time
// orderAccounts runs, every row in f.Accounts is already a member of order,
// and every rank lookup below hits. Kept rather than deleted: order is a
// plain parameter, not tied to EnsureRoster by
// the type system, so a future caller (or a reordering of runStatus's own
// two calls) that reaches this function with an unpruned f.Accounts would
// silently regress to whatever sort.SliceStable does with two zero ranks —
// this states the intended tie-break plainly instead. No test in this file
// can tell if these branches are ever deleted, the same shape as the
// known != nil guard documented at watchRoster.
func orderAccounts(f *status.File, order []string) {
	rank := make(map[string]int, len(order))
	for i, n := range order {
		rank[strings.ToLower(n)] = i
	}
	sort.SliceStable(f.Accounts, func(i, j int) bool {
		ri, iok := rank[strings.ToLower(f.Accounts[i].Name)]
		rj, jok := rank[strings.ToLower(f.Accounts[j].Name)]
		if !iok {
			ri = len(order)
		}
		if !jok {
			rj = len(order)
		}
		return ri < rj
	})
}

// accountMembers is the configured roster with each account's slot dir,
// for status.File.EnsureRoster (F171).
func accountMembers(st store.State) []status.Member {
	members := make([]status.Member, len(st.Accounts))
	for i, a := range st.Accounts {
		members[i] = status.Member{Name: a.Name, Dir: a.Dir}
	}
	return members
}

func accountNames(st store.State) []string {
	names := make([]string, len(st.Accounts))
	for i, a := range st.Accounts {
		names[i] = a.Name
	}
	return names
}

// statusSink guards internal/status.File for concurrent use from proxy
// request goroutines, and coalesces writes.
//
// status.File has no internal mutex, by design (see the package doc), and
// `proxy run`'s OnUsage hook is invoked concurrently, once per response,
// each on its own request goroutine: every call into the File (Observe,
// SetPassthrough, EnsureAccounts) must go through this sink's mutex, held
// for the full call including status.Marshal — a snapshot copy of the File
// struct still shares the Accounts slice's backing array with the live
// File, so marshalling a copy while another goroutine mutates an existing
// row is still a data race.
//
// The fsync-ing write itself does NOT happen under that mutex, or even on
// the calling goroutine: a writer goroutine (started in newStatusSink,
// stopped by Close) takes the marshalled bytes off the sink and writes them
// (F34) — a 100ms fsync must not add 100ms to the client-visible response.
type statusSink struct {
	mu       sync.Mutex
	path     string
	file     status.File
	interval time.Duration
	lastSave time.Time
	onError  func(error) // nil = ignore

	// pending is the newest marshalled document awaiting a write; wake
	// nudges the writer; done closes when the writer has stopped. All
	// three are guarded by mu. The writer only ever writes the NEWEST
	// pending document: a superseded intermediate is worth nothing, and
	// skipping it is what keeps a burst down to one fsync.
	pending []byte
	wake    chan struct{}
	done    chan struct{}
	closed  bool

	// write performs the actual write of a marshalled document; it exists
	// as a field, defaulting to status.WriteBytes in newStatusSink, so a
	// test can substitute its own to observe writes that actually happen
	// (what contract 4 is about) — or to inject a failure — without adding
	// mutable bookkeeping to the type whose invariants this task rests on.
	// Set once at construction (or by a test immediately after, before any
	// write is queued); writePending only ever reads it, on the writer
	// goroutine.
	write func(path string, b []byte) error

	// onObserved, when set, is told each account's limit state after an
	// observation that carried usage headers, so the usage poller can
	// cancel a no-data poll and move a reset poll (spec §6.4). It is
	// called with mu HELD, which fixes the lock order: sink.mu, then the
	// poller's own lock, never the reverse. It must return promptly and
	// must never call back into the sink. Set once by setOnObserved
	// before serving; guarded by mu.
	onObserved func(account string, limited bool, until time.Time)
}

// newStatusSink loads the existing cache (a missing or corrupt file is
// empty, never an error: the cache is disposable) and prepares to guard it.
func newStatusSink(home string, onError func(error)) (*statusSink, error) {
	f, err := status.Load(status.Path(home))
	if err != nil {
		return nil, err
	}
	c := &statusSink{
		path:     status.Path(home),
		file:     f,
		interval: time.Second,
		onError:  onError,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		write:    status.WriteBytes,
	}
	go c.writeLoop()
	return c, nil
}

// writeLoop performs the fsync-ing writes off the response path. It exits
// when Close closes wake, after one last pass so the newest queued
// document still lands.
func (c *statusSink) writeLoop() {
	defer close(c.done)
	for range c.wake {
		c.writePending()
	}
	// Currently redundant, the same way internal/owners' writeLoop
	// documents its own identical trailing call: queueLocked holds mu,
	// sets pending, and always attempts the capacity-1 send on wake, so
	// pending != nil always implies a token is (or still is) buffered, and
	// a closed buffered channel still delivers that token to `for range`
	// before the loop observes closed-and-empty — so the range loop above
	// alone already drains the newest document. Measured in
	// internal/owners: deleting the equivalent line there survives 40
	// combined -race runs. Kept anyway as defence-in-depth.
	c.writePending()
}

func (c *statusSink) writePending() {
	c.mu.Lock()
	b := c.pending
	c.pending = nil
	c.mu.Unlock()
	if b == nil {
		return
	}
	if err := c.write(c.path, b); err != nil && c.onError != nil {
		c.onError(err)
	}
}

// queueLocked marshals the current document and hands it to the writer.
// Marshalling stays under mu because a File shares its Accounts backing
// array with any copy of it; only the write leaves the lock. Caller holds
// c.mu.
func (c *statusSink) queueLocked() {
	if c.closed {
		return
	}
	b, err := status.Marshal(c.file)
	if err != nil {
		if c.onError != nil {
			c.onError(err)
		}
		return
	}
	c.pending = b
	select {
	case c.wake <- struct{}{}:
	default: // a wake is already queued; the writer will take the newest pending
	}
}

// Close stops the writer once the newest document has been written, and is
// safe to call more than once — including two overlapping calls: whichever
// loses the race to be the one that actually closes the writer still
// blocks until that writer has finished, rather than returning the instant
// it observes closed already true. The daemon's shutdown path must reach
// it, or the last observation is lost to the coalescing window (contract
// 4); it reaches Close through both a signal handler and a defer, so this
// is not a hypothetical: a Close that returns early lets the process exit
// while the deferred call's write is still in flight.
//
// Close queues the current document itself rather than only draining
// whatever queueLocked already queued: a run with no traffic at all still
// calls ensureAccounts at startup (contract 2), which does not itself
// queue a write, so Close is what must guarantee that seed reaches disk.
func (c *statusSink) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.done // a concurrent Close must still block until the drain finishes
		return
	}
	c.queueLocked()
	c.closed = true
	c.mu.Unlock()
	close(c.wake)
	<-c.done
}

// ensureAccounts seeds a row for every currently configured account, so
// Limits.AllLimited is computed over the full configured set rather than
// just the accounts traffic happens to have touched so far (contract 2).
func (c *statusSink) ensureAccounts(names []string) {
	c.mu.Lock()
	c.file.EnsureAccounts(names)
	c.mu.Unlock()
}

// ensureRoster is ensureAccounts with slot dirs, so a renamed account keeps
// its row (F171). The daemon's roster seeding uses it. A change (a rename,
// an added or removed account: rare) is written at once, so `next`, `tag`
// and `auto`, which read the file, see the carried row within the tick.
func (c *statusSink) ensureRoster(members []status.Member) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file.EnsureRoster(members) {
		c.lastSave = time.Now()
		c.queueLocked()
	}
}

// observe folds one response's usage into the cache. Being called at all is
// proof this account's request was actually answered on its own
// credential — not passing through on Home's login — so it also clears any
// stale passthrough mark for that account (contract 3).
//
// A limit that just started, or a passthrough mark that just changed, is
// flushed immediately rather than left to the coalescing window (contract
// 4, spec §6.3): a limit learned in the second before the process is
// killed must survive the restart, or chottag routes straight back to the
// account that just refused it.
func (c *statusSink) observe(account string, s usagehdr.Snapshot, v usagehdr.Verdict) {
	c.mu.Lock()
	wasLimited := c.limitedLocked(account)
	hadPassthrough := c.passthroughLocked(account)
	c.file.SetPassthrough(account, "")
	c.file.Observe(account, s, v)
	nowLimited := c.limitedLocked(account)
	if s.Known && c.onObserved != nil {
		// Under mu, so two observations of one account reach the poller
		// in the order the cache applied them (spec §6.4).
		_, until := c.limitStateLocked(account)
		c.onObserved(account, nowLimited, until)
	}
	c.mu.Unlock()

	if (!wasLimited && nowLimited) || hadPassthrough != "" {
		c.flush()
		return
	}
	c.maybeSave()
}

// setPassthrough records why this account's requests are not being swapped
// (F20): a stale token silently falling back to Home's login must be
// visible in `chottag status`, not just a throttled line on stderr. Like
// observe, a change to the mark is flushed immediately (contract 4): F20
// must survive a crash between the mark being set and the next coalesced
// write, or the user believes chottag switched when the cache on disk
// still shows the last thing it actually observed.
func (c *statusSink) setPassthrough(account, reason string) {
	if account == "" {
		return
	}
	c.mu.Lock()
	changed := c.passthroughLocked(account) != reason
	c.file.SetPassthrough(account, reason)
	c.mu.Unlock()

	if changed {
		c.flush()
		return
	}
	c.maybeSave()
}

// limitedLocked and passthroughLocked read a single account's current
// state. Callers must hold c.mu.
func (c *statusSink) limitedLocked(account string) bool {
	for _, a := range c.file.Accounts {
		if strings.EqualFold(a.Name, account) {
			return a.Limited
		}
	}
	return false
}

func (c *statusSink) passthroughLocked(account string) string {
	for _, a := range c.file.Accounts {
		if strings.EqualFold(a.Name, account) {
			return a.Passthrough
		}
	}
	return ""
}

// maybeSave queues a write at most once per interval: Observe runs per
// response, and queuing a write on every one of them would thrash a busy
// session's disk for no benefit (contract 4).
func (c *statusSink) maybeSave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastSave.IsZero() && time.Since(c.lastSave) < c.interval {
		return
	}
	c.lastSave = time.Now()
	c.queueLocked()
}

// flush queues a write immediately, bypassing the interval: a new limit or
// a changed passthrough reason is what the user is about to read.
func (c *statusSink) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSave = time.Now()
	c.queueLocked()
}

// setTokenState records the token state a real credential lookup already
// made on the product's own request flow — the selector's Token() call
// (internal/selector) — found for this account (F92, §6.1). It must never
// be fed by a new probe made just to populate this field: the Global
// Constraints forbid probing credentials outside the product flow, and this
// state is already being computed there.
//
// Shaped like setDaemon, not setPassthrough: it queues unconditionally
// under the lock rather than computing "did this change" to decide between
// flush and maybeSave. That is fine here because every caller (the
// passthrough hook) already calls setPassthrough for the same event, which
// itself flushes immediately on a change (F20, contract 3) — this call
// rides that same queued write rather than needing its own throttle
// decision.
func (c *statusSink) setTokenState(account string, s creds.TokenState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.file.SetToken(account, s, time.Now())
	c.queueLocked()
}

// setDaemon stamps the daemon's own view into the cached document and queues
// a write: mutation of c.file goes through this mutex, held for the full
// call, the same rule every other c.file mutator here follows (see the
// comment above the sink type). It deliberately does NOT mirror
// setPassthrough/flush on the axis that matters, though: setPassthrough
// goes through flush/maybeSave, which sets c.lastSave and so respects
// interval's throttle (contract 4); setDaemon calls queueLocked directly
// and sets no lastSave, bypassing that throttle entirely. That is
// deliberate here, not an oversight: the caller (runDaemon's stamp
// closure, proxy.go) already rate-limits how often setDaemon is even
// called — on change, or at most once per daemonHeartbeatInterval (F94) —
// so a second, independent throttle in the sink would only ever fire
// redundantly, never protectively.
// notifyErrors is stamped alongside, through status.File.SetNotifyErrors
// (M2b).
func (c *statusSink) setDaemon(port int, routeDrift, zeroIDs, drops, notifyErrors uint64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.file.SetDaemon(port, routeDrift, zeroIDs, drops, now)
	c.file.SetNotifyErrors(notifyErrors)
	c.queueLocked()
}

// clearDaemon takes the lock and calls status.File.ClearDaemon, then queues
// the result (fix round 2, item 1). serve (proxy.go) calls this immediately
// before its final sink.Close() on both of its exit paths, so a clean
// shutdown stops `chottag own` and `chottag status --json` reporting a live
// daemon within this process's own exit rather than up to DaemonStaleAfter
// (90s) later. Queues unconditionally under the lock, like setDaemon above,
// rather than going through flush/maybeSave: this only ever runs once, at
// shutdown, so there is no burst for a throttle to protect against.
func (c *statusSink) clearDaemon() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.file.ClearDaemon()
	c.queueLocked()
}

// statusSaveErrorThrottle prints cache/status.json write failures to
// stderr, at most once every 10s, so a full disk doesn't flood the
// terminal (mirrors logErrorThrottle for the trace log).
func statusSaveErrorThrottle(stderr io.Writer) func(error) {
	return throttle(10*time.Second, func(err error) {
		fmt.Fprintf(stderr, "chottag: status cache write failed: %v\n", err)
	})
}

// ownersSaveErrorThrottle prints owners.json write failures to stderr, at
// most once every 10s (mirrors statusSaveErrorThrottle above and
// logErrorThrottle for the trace log). own.SetOnError needs this rather
// than reusing logErrorThrottle: that one prints verbatim
// "chottag: trace log write failed: %v", which would tell the user their
// TRACE LOG failed when the actual failure is a durability loss in the
// owner map this task exists to protect (F72) — the wrong file, for the
// wrong reason.
func ownersSaveErrorThrottle(stderr io.Writer) func(error) {
	return throttle(10*time.Second, func(err error) {
		fmt.Fprintf(stderr, "chottag: owners.json write failed: %v\n", err)
	})
}

// usageErrorThrottle prints usage-callback failures to stderr, at most once
// every 10s (mirrors statusSaveErrorThrottle and ownersSaveErrorThrottle).
func usageErrorThrottle(stderr io.Writer) func(error) {
	return throttle(10*time.Second, func(err error) {
		fmt.Fprintf(stderr, "chottag: usage callback failed: %v\n", err)
	})
}

// ownersReloadErrorThrottle prints owners.json RELOAD failures (the roster
// tick's owners.Map.Reload call, e.g. a corrupt file) to stderr, at most
// once every 10s — mirrors the three throttles above. This is distinct from
// ownersSaveErrorThrottle: that one labels a WRITE failure and is wired to
// own.SetOnError at internal/cli/proxy.go:127, inside runProxyWithSignal
// (own.go has no SetOnError call at all); this one labels a READ/reload
// failure. Without a throttle here, a corrupt owners.json errored on every
// 5s tick forever, through runDaemon's unthrottled "chottag: roster watch:"
// sink — wrong interval AND the wrong label, blaming the roster watch for
// an owners-file problem it has nothing to do with.
func ownersReloadErrorThrottle(stderr io.Writer) func(error) {
	return throttle(10*time.Second, func(err error) {
		fmt.Fprintf(stderr, "chottag: owners.json reload failed: %v\n", err)
	})
}

// seedRosterAtStartup seeds sink with every account state currently
// registers, before `proxy run` starts serving. Extracted so a test can
// call it directly rather than only being able to observe its effect
// through a full `runProxy` invocation (contract 2).
func seedRosterAtStartup(state func() (store.State, error), sink *statusSink) {
	if st, err := state(); err == nil {
		sink.ensureRoster(accountMembers(st))
	}
}

// newUsageHook builds proxy.Config.OnUsage: parses and classifies each
// swapped response's unified rate-limit headers and folds them into sink,
// re-seeding the configured account roster from state first on every call
// (contract 2) — a `chottag adopt`/`remove` between requests must not leave
// Limits.AllLimited computed over a stale roster.
//
// Extracted into a named function, rather than an inline closure in
// runProxy, so a test can call it directly against a fake state and a real
// statusSink and assert on the cache it produces, instead of only being
// able to observe its effect by driving requests through a live listener.
//
// dn, if non-nil, also gets every call (M2b T5): a 2xx is the account's
// "seen ok", which re-arms its needs-login notice (notifyUsageHook's doc).
// nil is what every pre-M2b caller passes, and behaves exactly as before.
func newUsageHook(state func() (store.State, error), sink *statusSink, dn *daemonNotify) func(account string, code int, hdr http.Header) {
	return notifyUsageHook(dn, func(account string, code int, hdr http.Header) {
		now := time.Now()
		seedRosterAtStartup(state, sink)
		sink.observe(account, usagehdr.Parse(hdr, now), usagehdr.Classify(code, hdr, now))
	})
}

// newPassthroughHook builds selector.Config.OnEvent: routes every
// passthrough event into the status cache regardless of print's own
// throttling — a stale-token account silently falling back to Home's login
// must always be visible in `chottag status`, not just noisy on the
// terminal (F20, contract 3) — and forwards every event to print
// unconditionally, preserving the existing (throttled) stderr line.
//
// Extracted for the same reason as newUsageHook: a test can call the
// returned function directly with a synthetic selector.Event and assert on
// the resulting cache.
func newPassthroughHook(sink *statusSink, print func(selector.Event)) func(selector.Event) {
	return func(e selector.Event) {
		if e.Kind == "passthrough" {
			sink.setPassthrough(e.Account, passthroughReason(e))
			// This event hook, not a periodic per-account sweep, is
			// Account.Token's writer (F92): a sweep calling
			// tokens.Manager.Status for every configured account on the
			// roster tick would, once its 30s ReadTTL cache lapses, reach
			// Manager.read's m.cfg.Read(dir) — a macOS Keychain read — on a
			// timer with no request behind it, which is exactly probing
			// credentials outside the product flow (Global Constraints).
			// This hook instead reuses the Status the selector's own
			// Token() call already computed as a side effect of a real
			// request (internal/selector.Choose).
			//
			// Cost of that choice: Token is absent on the wire for any
			// account that has not yet hit this passthrough path this run
			// (never used, or every request so far swapped cleanly) — it is
			// updated on refusal, not polled.
			//
			// Status.State is only ever set when the selector's own Token()
			// call is what produced this event. A passthrough caused by,
			// e.g., "no serving account set" carries no Status at all: its
			// zero creds.TokenState ("") is not one of creds.StateOK|
			// Expiring|Stale|NeedsLogin, and writing it would claim a token
			// state for an account chottag never actually checked this
			// request.
			if e.Status.State != "" {
				sink.setTokenState(e.Account, e.Status.State)
			}
		}
		print(e)
	}
}

// newSelectorConfig builds the selector.Config runProxy hands to
// selector.New. Extracted for the same reason as newUsageHook and
// newPassthroughHook: newPassthroughHook's own behaviour was already
// pinned by a test that calls it directly, but nothing pinned that
// runProxy's OnEvent field is actually SET to that hook rather than a bare
// printer — `OnEvent: newPassthroughHook(sink, printEvent)` silently
// shrinking to `OnEvent: printEvent` still compiles and leaves the rest of
// the suite green, and is exactly contract 3's failure mode: a stale slot
// keeps sending requests out on Home's login while `chottag status`
// reports everything normal. A test can now build this exact Config and
// assert on the OnEvent it contains.
//
// dn, if non-nil, also gets every passthrough event whose token state is
// needs-login (M2b T5): notifyEventHook's doc. nil is what every pre-M2b
// caller passes, and behaves exactly as before.
func newSelectorConfig(state func() (store.State, error), tokens selector.Tokens, owners selector.Owners, sink *statusSink, printEvent func(selector.Event), dn *daemonNotify) selector.Config {
	return selector.Config{
		State:   state,
		Tokens:  tokens,
		Owners:  owners,
		OnEvent: newPassthroughHook(sink, notifyEventHook(dn, printEvent)),
	}
}

// passthroughReason turns a selector.Event into the text `chottag status`
// shows on that account's row: readable, and never a token (Event never
// carries one).
func passthroughReason(e selector.Event) string {
	switch {
	case e.Status.State != "" && e.Detail != "":
		return "token " + string(e.Status.State) + ": " + e.Detail
	case e.Status.State != "":
		return "token " + string(e.Status.State)
	case e.Detail != "":
		return e.Detail
	default:
		return "passthrough"
	}
}
