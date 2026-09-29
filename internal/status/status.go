// Package status owns cache/status.json: chottag's derived view of each
// account's usage, limit and token state.
//
// This file is a CACHE, not settings. state.json holds what the user chose;
// this holds what chottag observed. Deleting it must lose nothing — traffic
// or a poll rebuilds it — so every read path treats a missing or corrupt
// file as "empty", never as an error.
//
// It is written to disk, so it holds account names, utilization fractions
// and reset times, and never a token, a body, or an organization id taken
// from a response header (§6.1, §6.3).
//
// A File is NOT safe for concurrent use. Observe runs from per-request proxy
// goroutines, so the caller serialises Observe, SetPassthrough, EnsureAccounts
// and Save behind its own lock — the same shape internal/store documents for
// its own state.
package status

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/usage"
)

// StaleAfter is how long an observation stays trustworthy. Past this, a
// consumer reports "unknown" rather than acting on it (§6.3).
const StaleAfter = 10 * time.Minute

// Version is the cache schema version.
const Version = 1

type Usage struct {
	// FiveHourPct and SevenDayPct are pointers because "unknown" and "0%"
	// are different values: usage.Window.HasUtilization false means the
	// server reported no utilization for that window, and writing 0 there
	// would render "0% of 5h used" to a user about to hit a wall.
	//
	// They hold 0-100, NOT the 0-1 fraction usage.Window carries: this is
	// the wire format for both the file on disk and `status --json`, and
	// `status --json` is the status-line interface — the obvious
	// `.usage.sevenDayPct + "%"` a script writes must not render "1%" for
	// an account sitting at its weekly wall.
	FiveHourPct *float64 `json:"fiveHourPct,omitempty"`
	SevenDayPct *float64 `json:"sevenDayPct,omitempty"`
	// omitzero, NOT omitempty: omitempty has no effect on a struct type, so a
	// zero time would serialise as "0001-01-01T00:00:00Z" and any non-Go
	// reader comparing it to the clock concludes the window already reset.
	FiveHourResetsAt time.Time `json:"fiveHourResetsAt,omitzero"`
	SevenDayResetsAt time.Time `json:"sevenDayResetsAt,omitzero"`
	UpdatedAt        time.Time `json:"updatedAt"`
	// Source is "observed" (from a response chottag proxied) or "polled"
	// (from a usage request chottag made). Observed is preferred: it is free
	// and never stale while the account is in use (§6.3).
	Source string `json:"source"`
}

type Account struct {
	Name string `json:"name"`
	// Dir is the account's slot dir (store.Account.Dir), which never follows
	// a rename: EnsureRoster matches rows by it first, so a renamed account
	// keeps its row (F171). Written by the daemon; `status --json` omits it.
	Dir   string `json:"dir,omitempty"`
	Email string `json:"email,omitempty"`
	// Org disambiguates two accounts that share an email (F16). It comes
	// from `claude auth status --json` at adopt time, never from a response
	// header.
	Org   string `json:"org,omitempty"`
	Usage *Usage `json:"usage,omitempty"`
	// Limited and LimitedUntil come from usage.Verdict. LimitedUntil zero
	// means the clearing time is UNKNOWN, not that the limit already
	// expired: readers must not compare it against the clock to conclude
	// the account recovered.
	Limited bool `json:"limited,omitempty"`
	// omitzero: zero means UNKNOWN. Written as "0001-01-01T00:00:00Z" it
	// reads as "expired in year 1" to jq, a statusline script or a future
	// poller — and chottag routes straight back to the refusing account.
	LimitedUntil time.Time `json:"limitedUntil,omitzero"`
	Window       string    `json:"window,omitempty"`
	// Reason is usage.Verdict.Reason: present exactly when the account's
	// last classified response was a refusal (usage.Verdict.Refusal),
	// whether or not that refusal was itself classified as a limit — it is
	// what an operator reads when chottag declines to classify a refusal
	// (R33), which is the common case (three of Classify's four refusal
	// branches decline). A healthy response clears it (Observe): a
	// recovered account must not keep showing a refusal reason that no
	// longer applies.
	//
	// usage.sanitize already bounds the classifier's own text to 32
	// characters of [A-Za-z0-9]-ish; this field is additionally truncated on
	// the way in, because §6.1 does not get to depend on a bound enforced in
	// another package.
	Reason string `json:"reason,omitempty"`
	// Token is a creds.TokenState, never a bare string: §6.1 forbids a
	// bearer ever landing in this file, and a named type makes that mistake
	// unrepresentable rather than one careless assignment away.
	Token creds.TokenState `json:"token,omitempty"`
	// TokenAt is when Token last actually CHANGED (SetToken's doc): a
	// re-login stamps store.Account.LoggedInAt, and internal/cli's
	// planAccounts compares the two to tell a needs-login row that predates
	// the login (cleared) from one recorded after it (still needs one) —
	// fix round 1 item 1, superseding plan ruling 3. omitzero: a Token that
	// has never changed carries no TokenAt, same reasoning as LimitedUntil
	// above.
	TokenAt time.Time `json:"tokenAt,omitzero"`
	Rotate  bool      `json:"rotate"`
	// Passthrough, when set, is why requests for this account are currently
	// going out on Home's login instead (F20). A user who cannot see this
	// believes they switched when they did not.
	Passthrough string `json:"passthrough,omitempty"`
	// Plan is the account's plan tier as `chottag status` shows it (M4
	// spec §7): a report overlay from state.json, like Email and Org,
	// never written by the daemon.
	Plan string `json:"plan,omitempty"`
	// Stale reports whether Usage is too old to act on (§6.3), computed
	// from Fresh at report time. It is not meaningful in the on-disk cache
	// itself (Save never sets it): like Email, Org and Rotate, a reporting
	// consumer overlays it fresh before marshalling, because "stale" is a
	// function of when it is read, not when it was written. It exists so
	// `status --json` states the same freshness the human table already
	// shows by printing "unknown" instead of a number — the table got this
	// right; the JSON kept the raw percentages with nothing saying not to
	// trust them.
	Stale bool `json:"stale"`
}

// Limits is the roll-up across accounts. AllLimited is only meaningful after
// EnsureAccounts has seeded a row for every configured account: computed
// over just the accounts the cache happens to have observed, one limited
// account among others that sit untouched with full quota reads as
// "all accounts limited".
type Limits struct {
	AllLimited bool `json:"allLimited"`
	// omitzero: zero means UNKNOWN (see LimitedUntil above), not "resets in
	// year 1".
	NextReset        time.Time `json:"nextReset,omitzero"`
	NextResetAccount string    `json:"nextResetAccount,omitempty"`
}

// DaemonStaleAfter is how long after its last heartbeat a daemon is no longer
// considered running. The daemon's heartbeat is rate-limited to roughly once
// every 30s when nothing has changed (F94 — the stamp is its own write, not
// a free ride on the 5-second roster tick's existing work, so it cannot fire
// every tick without fsyncing status.json every 5s forever on an idle
// daemon). 90s is three missed heartbeats' tolerance at that 30s cadence —
// the same "three missed beats" ratio this constant held at 15s/5s before
// F94 — enough to ride out a slow beat without pretending a killed daemon is
// alive for long.
const DaemonStaleAfter = 90 * time.Second

// Daemon carries daemon liveness and the production counters that were
// incremented and read by nobody before M1c5 (F59). Provenance is split and
// the split is deliberate (§5.1): Port, Heartbeat and the three counters are
// STAMPED BY THE DAEMON into the cache, while Running is computed at read
// time by DaemonRunningAt — a stored "running: true" would outlive the
// process it describes, so it is never trusted from the file, even though it
// IS marshalled to disk on every stamp (always as false: SetDaemon
// explicitly clears it on every call, fix round 3 R6 — a hand-edited or
// pre-F94 `running: true` loaded from disk must not survive untouched in a
// long-lived daemon process's in-memory copy). The real guarantee is
// narrower than "never stored": it is never READ BACK. Concretely, that
// means `status --json` (runStatus's `if *jsonOut` block, the only
// DaemonRunningAt caller): it overwrites whatever was loaded before
// marshalling to stdout. The human table (renderStatus) makes no claim
// either way — it never reads f.Daemon at all, so there is nothing there to
// overwrite.
//
// The counters are monotonic within a daemon GENERATION and reset to 0 on
// restart; they are not lifetime totals, so a reader seeing a decrease has
// found a restart, not an error.
//
// Considered and rejected (fix round 1, Item 4): tagging Running json:"-"
// so it is genuinely not stored, with a separate report-time projection
// type carrying it instead. Rejected because status.json's on-disk cache
// and `status --json`'s stdout output are the SAME marshalling of the SAME
// File today (runStatus loads the cache, overlays DaemonRunningAt, then
// marshals that same File straight to stdout) — json:"-" would silently
// drop running from the --json OUTPUT too, contradicting §5.1, which
// documents it as present there. Splitting the wire type is a real option
// but a bigger change than this fix round's scope; flagging it here rather
// than doing it silently.
type Daemon struct {
	// Running is stored on disk (always false — see Daemon's doc comment)
	// but never READ BACK: status --json (runStatus's `if *jsonOut` block)
	// recomputes it via DaemonRunningAt before anything looks at it; the
	// human table never reads this field at all. json-tagged without
	// omitempty so a consumer always sees an explicit true/false rather
	// than having to treat absence as false.
	Running bool `json:"running"`
	Port    int  `json:"port,omitempty"`
	// Heartbeat is the tick at which the daemon last stamped this object.
	// omitzero: zero means UNKNOWN, never "the epoch" (F25).
	Heartbeat         time.Time `json:"heartbeat,omitzero"`
	RouteDrift        uint64    `json:"routeDrift"`
	ZeroIDExtractions uint64    `json:"zeroIdExtractions"`
	OwnerWriteDrops   uint64    `json:"ownerWriteDrops"`
	// NotifyErrors counts desktop notifications that were not delivered: a
	// failed or timed-out send or a full queue (M2 spec §4). None is
	// retried. Stamped by the daemon, per generation, like the three
	// counters above, and always present. Notices dropped at shutdown are
	// counted in memory but never stamped here: dn.Close() runs after
	// runDaemon returns and the status sink has already been closed.
	NotifyErrors uint64 `json:"notifyErrors"`
	// Version and VersionMismatch are report overlays (public release
	// design §2.4), like Running: never written by the daemon or read back
	// from disk, only set fresh by a report consumer (`status --json`,
	// runStatus) that actually probed the running daemon's health document
	// just now. Version is the running daemon's health Version; omitempty
	// because it is absent whenever nothing was probed (no daemon, or a
	// stale heartbeat). VersionMismatch is Version != cli.Version — false
	// (and omitted) when they match or nothing was probed, so a consumer
	// reading it directly never sees a stale true survive from a prior
	// stamp the way a stored Running once could (SetDaemon's doc comment).
	Version         string `json:"version,omitempty"`
	VersionMismatch bool   `json:"versionMismatch,omitempty"`
	// Identity is a report overlay too (F221, part 1 T11), exactly like
	// Version: never written by the daemon or read back from disk, set
	// fresh only by a report consumer that actually verified the running
	// daemon's health challenge (cli.daemonIdentity's "none", "verified",
	// "legacy", "mismatch" or "unknown"). Empty whenever nothing was
	// probed, the same as Version's own omitempty.
	Identity string `json:"identity,omitempty"`
}

// SetDaemon stamps the daemon's own view into the document. Called on the
// roster tick, under the sink's mutex, by the daemon process only.
//
// It explicitly CLEARS Running rather than merely never setting it (fix
// round 3, R6): newStatusSink loads whatever was already on disk, and
// without this, a hand-edited or pre-F94 `running: true` sitting in that
// loaded document would survive untouched in the sink's in-memory File and
// be re-marshalled as true on every subsequent stamp for the rest of this
// process's life — never becoming false until a restart. DaemonRunningAt's
// own overwrite only protects `status --json`'s separate load-then-report
// path (runStatus), not this long-lived in-memory copy, so it cannot be
// relied on here.
func (f *File) SetDaemon(port int, routeDrift, zeroIDs, ownerWriteDrops uint64, now time.Time) {
	if f.Daemon == nil {
		f.Daemon = &Daemon{}
	}
	f.Daemon.Running = false
	f.Daemon.Port = port
	f.Daemon.Heartbeat = now
	f.Daemon.RouteDrift = routeDrift
	f.Daemon.ZeroIDExtractions = zeroIDs
	f.Daemon.OwnerWriteDrops = ownerWriteDrops
}

// SetNotifyErrors stamps the notification error counter. It is separate
// from SetDaemon so that SetDaemon's callers keep their signature; the
// daemon calls it right after SetDaemon, under the same lock. A nil daemon
// object is created rather than dereferenced.
func (f *File) SetNotifyErrors(n uint64) {
	if f.Daemon == nil {
		f.Daemon = &Daemon{}
	}
	f.Daemon.NotifyErrors = n
}

// DaemonRunningAt computes Daemon.Running from the heartbeat's age, for a
// report consumer deciding what to show right now. It is the daemon-object
// analogue of RollUpAt: freshness is a function of when it is READ, not when
// it was written, which is the same rule Account.Stale already follows.
//
// A nil Daemon stays nil: a home where no daemon ever ran reports no daemon
// object rather than one asserting "not running", because those are different
// statements and only one of them is known.
func (f *File) DaemonRunningAt(now time.Time) {
	if f.Daemon == nil {
		return
	}
	// The explicit !IsZero() guard below is INERT, and stays inert after
	// ClearDaemon: a zero Heartbeat makes now.Sub(...) saturate at roughly
	// 292 years (documented Sub behaviour), which is never <
	// DaemonStaleAfter, so the duration comparison alone already reports
	// false — immediately, not DaemonStaleAfter later. Deleting the guard
	// leaves the whole suite green, which is how this was established both
	// times. It stays because it states the intent plainly ("no heartbeat
	// is not running"), the same convention as the known != nil guard at
	// internal/cli/proxy.go's watchRoster.
	//
	// A fix round briefly replaced this with a comment claiming ClearDaemon
	// had MADE the guard load-bearing. It had not, and the claim was caught
	// by the mutation above (F99).
	f.Daemon.Running = !f.Daemon.Heartbeat.IsZero() &&
		now.Sub(f.Daemon.Heartbeat) < DaemonStaleAfter
}

// ClearDaemon marks the daemon as no longer running, for a shutdown path to
// call so `chottag own` and `chottag status --json` stop reporting a live
// daemon the instant this process exits, rather than up to DaemonStaleAfter
// (90s) later (fix round 2, item 1). It zeroes Heartbeat rather than nilling
// Daemon: DaemonRunningAt already reports a zero Heartbeat as not running
// (via the saturating duration comparison, not the IsZero guard — see its
// comment above), and keeping the Daemon object means Port and the three
// counters still show what the daemon last reported instead of a clean stop
// erasing that history a nil Daemon would.
//
// A nil Daemon (no daemon ever stamped this document) has nothing to clear
// and stays nil, for the same reason DaemonRunningAt leaves it alone: "never
// ran" and "ran, then stopped" are different statements.
func (f *File) ClearDaemon() {
	if f.Daemon == nil {
		return
	}
	f.Daemon.Running = false
	f.Daemon.Heartbeat = time.Time{}
}

// SetLastTraced records version as the last traced Claude Code version,
// first seen at at (kept to the second). It reports whether anything
// changed: the same version again changes nothing, so the first request's
// time is kept, and a different version replaces the pair. A fresh Trace
// is stored every time, so a copy of the File never sees the change.
func (f *File) SetLastTraced(version string, at time.Time) bool {
	if f.Trace != nil && f.Trace.LastTraced != nil && f.Trace.LastTraced.ClaudeVersion == version {
		return false
	}
	f.Trace = &Trace{LastTraced: &TracedVersion{ClaudeVersion: version, At: at.Round(0).Truncate(time.Second)}}
	return true
}

// LastTraced returns the recorded pair, if there is one.
func (f File) LastTraced() (TracedVersion, bool) {
	if f.Trace == nil || f.Trace.LastTraced == nil {
		return TracedVersion{}, false
	}
	return *f.Trace.LastTraced, true
}

// Trace is what the daemon learned while tracing (M2c spec §3, T7).
type Trace struct {
	LastTraced *TracedVersion `json:"lastTraced,omitempty"`
}

// TracedVersion is the Claude Code version the daemon saw on a traced
// request (from its User-Agent, by an anchored rule), and when it first
// saw it.
type TracedVersion struct {
	ClaudeVersion string    `json:"claudeVersion"`
	At            time.Time `json:"at"`
}

// Auto is the daemon's auto-switch view (M4 spec §7). It is derived state
// like the rest of this file: the daemon rewrites it, and deleting it
// loses nothing.
type Auto struct {
	Mode string `json:"mode"`
	// Decision is the planner's last one-line summary, e.g. "holding A
	// (5h 96%, resets in 9m)".
	Decision   string      `json:"decision,omitempty"`
	LastSwitch *AutoSwitch `json:"lastSwitch,omitempty"`
	// UserChosen: the serving account was put there by the user while at
	// or above a switch point, so only a hard limit moves it (S6).
	UserChosen bool `json:"userChosen"`
	// BurnRate is the measured burn in units per active hour, 0 while the
	// daemon has too little data (the planner then uses its default).
	BurnRate float64 `json:"burnRate"`
}

// AutoSwitch is the last switch the daemon made.
type AutoSwitch struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Trigger string `json:"trigger"` // "limit" | "threshold"
	Window  string `json:"window,omitempty"`
	// Pct is the utilization that triggered a threshold switch (0-100).
	Pct float64 `json:"pct,omitempty"`
	// omitzero: zero means unknown (F25).
	At time.Time `json:"at,omitzero"`
	// Retried: the request that hit the wall was resent on To (§4a).
	Retried bool `json:"retried,omitempty"`
}

// SetAuto records the daemon's auto-switch view and reports whether it
// changed, so the caller writes only on a change. BurnRate is kept to one
// decimal: a burn average that moves in the third decimal is not worth a
// write. The stored value is a fresh copy.
func (f *File) SetAuto(a Auto) bool {
	a.BurnRate = math.Round(a.BurnRate*10) / 10
	if a.LastSwitch != nil {
		ls := *a.LastSwitch
		a.LastSwitch = &ls
	}
	if f.Auto != nil && sameAuto(*f.Auto, a) {
		return false
	}
	f.Auto = &a
	return true
}

func sameAuto(x, y Auto) bool {
	if x.Mode != y.Mode || x.Decision != y.Decision || x.UserChosen != y.UserChosen || x.BurnRate != y.BurnRate {
		return false
	}
	if (x.LastSwitch == nil) != (y.LastSwitch == nil) {
		return false
	}
	if x.LastSwitch == nil {
		return true
	}
	a, b := *x.LastSwitch, *y.LastSwitch
	return a.From == b.From && a.To == b.To && a.Trigger == b.Trigger && a.Window == b.Window &&
		a.Pct == b.Pct && a.At.Equal(b.At) && a.Retried == b.Retried
}

type File struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
	Serving  string    `json:"serving,omitempty"`
	Remote   string    `json:"remote,omitempty"`
	Limits   Limits    `json:"limits"`
	// Daemon is absent until a daemon has stamped it at least once (§5.1).
	Daemon *Daemon `json:"daemon,omitempty"`
	// Trace is absent until a daemon has traced a request (M2c).
	Trace *Trace `json:"trace,omitempty"`
	// Auto is absent until a daemon has made an auto-switch decision (M4).
	Auto *Auto `json:"auto,omitempty"`
}

// Path is the cache's location under a chottag home.
func Path(home string) string { return filepath.Join(home, "cache", "status.json") }

// Load reads the cache. A missing or unparseable file is not an error: the
// cache is disposable and rebuilt from traffic.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{Version: Version}, nil
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{Version: Version}, nil
	}
	if f.Version == 0 {
		f.Version = Version
	}
	return f, nil
}

// Observe folds one response's worth of evidence into the cache.
//
// A snapshot that carried no unified headers (Known=false) updates nothing:
// it must never clear a limit chottag already knows about, because "I learned
// nothing" is not evidence that the account recovered. It must also not
// create a row: an empty row for an account chottag never actually observed
// would flip AllLimited and persist a name that may be a typo.
func (f *File) Observe(account string, s usage.Snapshot, v usage.Verdict) {
	if account == "" {
		return
	}
	if !s.Known && !f.has(account) {
		return
	}
	a := f.account(account)
	if s.Known {
		if a.Usage == nil {
			a.Usage = &Usage{}
		}
		a.Usage.UpdatedAt = s.At
		a.Usage.Source = "observed"
		// Merge per window, rather than replacing Usage wholesale: a window
		// this snapshot says nothing about (Known false) must leave what
		// chottag already knew alone. A snapshot carrying only
		// Unified-Status: allowed (5h/7d headers absent) is exactly the
		// "I learned nothing about this window" case the headerless-snapshot
		// guard above already protects at the whole-response level — this is
		// the same guard, per window, since a response can be Known (it
		// named an overall status) while still saying nothing about one of
		// the two windows.
		if s.FiveHour.Known {
			a.Usage.FiveHourResetsAt = s.FiveHour.ResetsAt
			if s.FiveHour.HasUtilization {
				pct := s.FiveHour.Utilization * 100
				a.Usage.FiveHourPct = &pct
			} else {
				a.Usage.FiveHourPct = nil
			}
		}
		if s.SevenDay.Known {
			a.Usage.SevenDayResetsAt = s.SevenDay.ResetsAt
			if s.SevenDay.HasUtilization {
				pct := s.SevenDay.Utilization * 100
				a.Usage.SevenDayPct = &pct
			} else {
				a.Usage.SevenDayPct = nil
			}
		}
		if v.Limited {
			// v.Until zero means UNKNOWN, not expired. Store it as zero and
			// let readers treat zero as unknown; never substitute now() or a
			// past time, or the limit lifts instantly and chottag routes
			// straight back to the account that just refused.
			a.Limited, a.LimitedUntil, a.Window = true, v.Until, v.Window
		} else if strings.EqualFold(s.Overall, "allowed") {
			// Positive evidence of health clears a stale limit — but only
			// when it is evidence about the window that did the limiting.
			// s.Overall alone is not enough: a response can report
			// Unified-Status: allowed while the window named by a.Window
			// still separately reports rejected (the mirror of the window
			// merge above: it is the SAME window silently going unreported,
			// or itself still saying "rejected", that must not read as
			// recovery). An unrecognised or empty a.Window (not currently
			// limited) simply never matches, which is a harmless no-op.
			if w, ok := windowByName(s, a.Window); ok && w.Known && strings.EqualFold(w.Status, "allowed") {
				a.Limited, a.LimitedUntil, a.Window = false, time.Time{}, ""
			}
		}
	}
	// Reason is only meaningful on a refusal (v.Refusal, NOT the same set as
	// v.Limited — three of Classify's four refusal branches decline to
	// classify, which is exactly the case R33 needs a reason for). This must
	// run whether or not s.Known: Classify's headerless-refusal branch is BY
	// CONSTRUCTION the s.Known == false case, and that is the branch R33
	// exists for — a 500, a 401, a gateway error, where the operator has
	// nothing else to read. Gating this on s.Known, as an earlier version
	// did, made reason unreachable for its own motivating case. The top
	// guard above still refuses to create a row for an account chottag has
	// never actually observed, so this only ever touches a row that already
	// exists. A healthy response clears it: a recovered account must not
	// keep displaying the reason for a refusal that no longer applies.
	if v.Refusal {
		a.Reason = boundReason(v.Reason)
	} else {
		a.Reason = ""
	}
	f.rollUp()
}

// maxReason bounds Account.Reason independently of usage.sanitize's own cap.
//
// 80, not 64: Classify's headerless-refusal reason ("refusal carried no
// unified rate-limit headers; not treated as a limit") is 69 bytes, and the
// final fix round is what made that branch reachable at all. At 64 an
// operator read it cut mid-phrase — "...not treated as a " — on the one
// refusal where they have nothing else to go on, which is the whole reason
// this field exists (R33). TestTheHeaderlessRefusalReasonSurvivesTheBound-
// Intact drives the real Classify, so it fails if either number moves.
const maxReason = 80

// boundReason truncates on a rune boundary, not a byte offset: it must not
// depend on usage.sanitize's own output being pure ASCII, which is an
// invariant that lives in another package (§6.1's independence is the point
// of this function — see the doc comment on Account.Reason).
func boundReason(s string) string {
	if len(s) <= maxReason {
		return s
	}
	cut := maxReason
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// windowByName resolves a Verdict/Account.Window name ("five_hour" or
// "seven_day") back to that window in a Snapshot. ok is false for an
// unrecognised or empty name, so Observe's clearing guard simply does not
// match rather than comparing against a zero Window that would read as
// Known=false anyway.
func windowByName(s usage.Snapshot, name string) (usage.Window, bool) {
	switch name {
	case "five_hour":
		return s.FiveHour, true
	case "seven_day":
		return s.SevenDay, true
	default:
		return usage.Window{}, false
	}
}

// SetToken records the token state a credential lookup already made in the
// product flow found for this account — the selector's real Token() call on
// a request, never a new probe made just to populate this field (Global
// Constraints forbid probing credentials outside the product flow; F92).
//
// at stamps TokenAt, but only when s actually differs from the state
// already on file: a caller that re-asserts the same state on every request
// (the common case) must not keep pushing TokenAt forward, or it would
// always read as "just happened" and defeat planAccounts' before/after
// comparison against LoggedInAt (fix round 1 item 1).
func (f *File) SetToken(account string, s creds.TokenState, at time.Time) {
	if account == "" {
		return
	}
	a := f.account(account)
	if a.Token == s {
		if s == creds.StateNeedsLogin {
			// A needs-login re-observed after a re-login must still count
			// (item 4, review round 2): planAccounts treats a needs-login
			// as cleared once LoggedInAt is after TokenAt (fix round 1 item
			// 1), so an unchanged needs-login must still re-stamp TokenAt —
			// otherwise a needs-login that persists across a re-login reads
			// as stale and silently clears, even though the daemon just
			// confirmed it again.
			a.TokenAt = at
		}
		return
	}
	a.Token = s
	a.TokenAt = at
}

// SetPassthrough records why this account's requests are not being swapped.
func (f *File) SetPassthrough(account, reason string) {
	if account == "" {
		return
	}
	// Clearing a passthrough on an account with no row is a no-op, not a
	// create. This is what makes Observe's own do-not-create guard reachable
	// in production: statusSink.observe calls SetPassthrough(account, "")
	// one line before Observe, and while that created the row, a headerless
	// response about an unknown account could never be turned away.
	if reason == "" && !f.has(account) {
		return
	}
	f.account(account).Passthrough = reason
	f.rollUp()
}

// Member is one configured account as state.json names it: Dir is its slot
// dir, "" when the caller knows only the name.
type Member struct {
	Name, Dir string
}

// EnsureAccounts is EnsureRoster for callers that know only names: each row
// is matched by name alone, and a row's Dir is left as it is.
func (f *File) EnsureAccounts(names []string) {
	members := make([]Member, 0, len(names))
	for _, n := range names {
		members = append(members, Member{Name: n})
	}
	f.EnsureRoster(members)
}

// EnsureRoster seeds a zero-value row for every configured account that
// isn't already present, and REMOVES any row whose account is no longer
// configured. Call it before reading Limits.AllLimited: without the seeding,
// AllLimited is computed only over the accounts the cache happens to have
// observed; without the pruning, a logged-out account's row still counts
// toward AllLimited and can still win NextResetAccount, announcing a reset
// for an account the user no longer has (R27).
//
// members is the authority — it comes from state.json, the configured set.
// An empty members prunes every row: state.json is written atomically, so an
// empty configured set is the user having removed their last account, not a
// torn read.
//
// A row is matched by slot dir first, since the dir never follows a rename
// (F171): a renamed account keeps its row, and two accounts that swap names
// keep their own. Then by name, but only where one side has no dir yet (a
// row written before rows carried one, or a names-only caller): a row whose
// dir differs from the member's belongs to a removed slot, and a new slot
// reusing its name must not inherit its limit. A matched row takes the
// configured spelling (F175) and, when the member has one, its dir.
//
// It reports whether any row was seeded, pruned, respelled or re-dirred,
// so the daemon can write a carried row at once rather than at its next
// heartbeat.
func (f *File) EnsureRoster(members []Member) (changed bool) {
	before := make([]Member, len(f.Accounts))
	for i, a := range f.Accounts {
		before[i] = Member{Name: a.Name, Dir: a.Dir}
	}
	defer func() {
		if len(before) != len(f.Accounts) {
			changed = true
			return
		}
		for i, a := range f.Accounts {
			if before[i] != (Member{Name: a.Name, Dir: a.Dir}) {
				changed = true
				return
			}
		}
	}()
	match := make([]int, len(members)) // member -> row index, -1 if none
	used := make([]bool, len(f.Accounts))
	for m := range members {
		match[m] = -1
		if members[m].Name == "" || members[m].Dir == "" {
			continue
		}
		for i := range f.Accounts {
			if !used[i] && f.Accounts[i].Dir == members[m].Dir {
				match[m], used[i] = i, true
				break
			}
		}
	}
	// strings.EqualFold here, as in every other lookup in this file. It is
	// NOT equivalent to strings.ToLower in general (simple Unicode folding
	// matches U+017F long-s against "s"); that only stays harmless because
	// store.ValidName keeps every account name ASCII (F97).
	for m := range members {
		if match[m] >= 0 || members[m].Name == "" {
			continue
		}
		for i := range f.Accounts {
			a := &f.Accounts[i]
			if !used[i] && strings.EqualFold(a.Name, members[m].Name) && (a.Dir == "" || members[m].Dir == "") {
				match[m], used[i] = i, true
				break
			}
		}
	}
	kept := make([]Account, 0, len(members))
	for i := range f.Accounts {
		for m := range members {
			if match[m] == i {
				a := f.Accounts[i]
				a.Name = members[m].Name
				if members[m].Dir != "" {
					a.Dir = members[m].Dir
				}
				kept = append(kept, a)
				break
			}
		}
	}
	seen := make(map[string]bool, len(members))
	for _, a := range kept {
		seen[strings.ToLower(a.Name)] = true
	}
	for m, mem := range members {
		if match[m] >= 0 || mem.Name == "" || seen[strings.ToLower(mem.Name)] {
			continue
		}
		seen[strings.ToLower(mem.Name)] = true
		kept = append(kept, Account{Name: mem.Name, Dir: mem.Dir})
	}
	f.Accounts = kept
	f.rollUp()
	return changed // set by the deferred comparison above
}

func (f *File) has(name string) bool {
	for i := range f.Accounts {
		if strings.EqualFold(f.Accounts[i].Name, name) {
			return true
		}
	}
	return false
}

func (f *File) account(name string) *Account {
	for i := range f.Accounts {
		if strings.EqualFold(f.Accounts[i].Name, name) {
			return &f.Accounts[i]
		}
	}
	f.Accounts = append(f.Accounts, Account{Name: name})
	return &f.Accounts[len(f.Accounts)-1]
}

// Fresh reports whether this account's usage is recent enough to act on.
func (f *File) Fresh(account string, now time.Time) bool {
	for i := range f.Accounts {
		if strings.EqualFold(f.Accounts[i].Name, account) {
			u := f.Accounts[i].Usage
			return u != nil && now.Sub(u.UpdatedAt) < StaleAfter
		}
	}
	return false
}

func (f *File) rollUp() {
	if len(f.Accounts) == 0 {
		f.Limits = Limits{}
		return
	}
	all := true
	var next time.Time
	var nextName string
	for _, a := range f.Accounts {
		if !a.Limited {
			all = false
			continue
		}
		// Skip a zero LimitedUntil: it means the clearing time is UNKNOWN,
		// and letting it win regardless of observation order would zero out
		// NextReset for an account whose real reset time is known.
		if !a.LimitedUntil.IsZero() && (next.IsZero() || a.LimitedUntil.Before(next)) {
			next, nextName = a.LimitedUntil, a.Name
		}
	}
	f.Limits = Limits{AllLimited: all, NextReset: next, NextResetAccount: nextName}
}

// RollUpAt recomputes f.Limits the same way the write path's rollUp does,
// except a LimitedUntil that has already passed no longer counts as
// confirmed limited: M1c has no poll, so nothing else would ever clear it,
// and letting AllLimited/NextReset assert a reset date that has already gone
// by is a self-sustaining trap — the user never sends the traffic that would
// let a fresh Observe correct it, because status just told them not to.
//
// A zero LimitedUntil is untouched: it means UNKNOWN, never expired, exactly
// as in rollUp.
//
// Callers on the write path (Observe, SetPassthrough, EnsureAccounts) must
// keep using the clockless rollUp: RollUpAt is for a report consumer
// (`status --json`) computing what to show right now, not for what the
// cache durably records happened.
func (f *File) RollUpAt(now time.Time) {
	if len(f.Accounts) == 0 {
		f.Limits = Limits{}
		return
	}
	all := true
	var next time.Time
	var nextName string
	for _, a := range f.Accounts {
		if !confirmedLimitedAt(a, now) {
			all = false
			continue
		}
		if !a.LimitedUntil.IsZero() && (next.IsZero() || a.LimitedUntil.Before(next)) {
			next, nextName = a.LimitedUntil, a.Name
		}
	}
	f.Limits = Limits{AllLimited: all, NextReset: next, NextResetAccount: nextName}
}

// confirmedLimitedAt is RollUpAt's rule for one account: limited, with a
// reset that is unknown (zero) or still ahead. A reset that has passed no
// longer counts, because nothing but traffic would ever clear it.
func confirmedLimitedAt(a Account, now time.Time) bool {
	return a.Limited && (a.LimitedUntil.IsZero() || a.LimitedUntil.After(now))
}

// LimitsAt returns what RollUpAt would compute at now, without writing it:
// f is a copy, so the stored Limits the write path records stay as they
// are. The daemon's notifier reads it on the roster tick (M2b), so an
// account whose reset time passes with no traffic still reads as available.
func (f File) LimitsAt(now time.Time) Limits {
	f.RollUpAt(now)
	return f.Limits
}

// AvailableAt names the first account, in file order, that is not a
// confirmed limit at now, or "" when every account is (or there are none).
func (f File) AvailableAt(now time.Time) string {
	for _, a := range f.Accounts {
		if !confirmedLimitedAt(a, now) {
			return a.Name
		}
	}
	return ""
}

// Marshal renders the document exactly as Save writes it, trailing newline
// and Version default included. Split out so a caller holding a lock can
// marshal under it and write outside it: the write fsyncs, and an fsync
// does not belong on a response path (F34).
func Marshal(f File) ([]byte, error) {
	if f.Version == 0 {
		f.Version = Version
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteBytes writes a document Marshal produced, atomically and 0600,
// creating its directory.
//
// It delegates to fsutil.WriteFileAtomic rather than hand-rolling
// temp+rename: that helper is already tested, already used by
// internal/store, and calls Sync() before the rename, so the cache gets the
// same durability guarantee as state.json instead of a quietly weaker one.
func WriteBytes(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, b, 0o600)
}
