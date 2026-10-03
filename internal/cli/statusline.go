package cli

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/brand"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// statuslineMaxDepth bounds the ancestor walk: a Claude Code status line
// command sits a few processes below claude, and a display must not chase a
// long chain.
const statuslineMaxDepth = 8

// statuslineProbeTimeout keeps the daemon probe short: a status line runs on
// every refresh and must not stall the display.
const parentPIDTimeout = 200 * time.Millisecond

const statuslineProbeTimeout = 300 * time.Millisecond

// parentPID is the parent of pid. A var so TestMain (invariants_test.go) can
// arm a panicking default: the real one runs ps(1).
var parentPID = func(pid int) (int, error) {
	// A hung ps must not stall a status-line redraw.
	ctx, cancel := context.WithTimeout(context.Background(), parentPIDTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// statuslineProbe reports whether chottag's daemon answers on port, within a
// short timeout. A var for the same reason as statusProbe.
var statuslineProbe = func(port int) bool {
	return shim.ProbeHealthWithin(port, statuslineProbeTimeout)
}

// SetParentPIDForTest swaps parentPID and returns a func that restores what
// was installed at the moment of the call (F130).
func SetParentPIDForTest(fn func(pid int) (int, error)) (restore func()) {
	orig := parentPID
	parentPID = fn
	return func() { parentPID = orig }
}

// SetStatuslineProbeForTest swaps statuslineProbe likewise.
func SetStatuslineProbeForTest(fn func(port int) bool) (restore func()) {
	orig := statuslineProbe
	statuslineProbe = fn
	return func() { statuslineProbe = orig }
}

// statuslineNow is the clock for the reset rule. A var so tests fix it.
var statuslineNow = time.Now

// SetStatuslineNowForTest swaps the statusline clock likewise.
func SetStatuslineNowForTest(fn func() time.Time) (restore func()) {
	orig := statuslineNow
	statuslineNow = fn
	return func() { statuslineNow = orig }
}

// cmuxSetStatus sets chottag's cmux sidebar pill for workspace ws. A var so
// TestMain (invariants_test.go) can arm a panicking default: the real one runs
// cmux(1).
var cmuxSetStatus = func(ctx context.Context, workspace, value, color string) error {
	ctx, cancel := context.WithTimeout(ctx, cmuxTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "cmux", "set-status", "chottag", value,
		"--color", color, "--priority", "50", "--workspace", workspace).Run()
}

// cmuxTimeout bounds the pill's cmux call: a redraw must not stall on it.
const cmuxTimeout = 300 * time.Millisecond

// SetCmuxSetStatusForTest swaps cmuxSetStatus likewise.
func SetCmuxSetStatusForTest(fn func(ctx context.Context, workspace, value, color string) error) (restore func()) {
	orig := cmuxSetStatus
	cmuxSetStatus = fn
	return func() { cmuxSetStatus = orig }
}

type statuslineResult struct {
	Session string `json:"session"`
	Daemon  string `json:"daemon"`
	Serving string `json:"serving"`
	// Pool is the session's pool when it is not default (M8); Serving and
	// Account are then that pool's.
	Pool string `json:"pool,omitempty"`
	// Account is the account this session uses: its own, once the daemon has
	// seen it make an inference request, else the serving account.
	Account     string   `json:"account,omitempty"`
	Label       string   `json:"label,omitempty"`
	FiveHourPct *float64 `json:"fiveHourPct,omitempty"`
	SevenDayPct *float64 `json:"sevenDayPct,omitempty"`
	ResetsAt    string   `json:"resetsAt,omitempty"`
	// FiveHourResetsAt and SevenDayResetsAt are the two windows' own resets
	// (RFC 3339, local), each left out when unknown or already past; the card
	// shows both. NearWindow is "5h" or "7d": the window closer to its switch
	// point (the card bolds it).
	FiveHourResetsAt string `json:"fiveHourResetsAt,omitempty"`
	SevenDayResetsAt string `json:"sevenDayResetsAt,omitempty"`
	NearWindow       string `json:"nearWindow,omitempty"`
	OKAccounts       *int   `json:"okAccounts,omitempty"`
	RotationAccounts *int   `json:"rotationAccounts,omitempty"`
	// UpdateAvailable is the newer release the daemon found, shown only
	// while this session is routed and the daemon is up.
	UpdateAvailable string `json:"updateAvailable,omitempty"`
	// RestartPending is the installed version the running daemon is not yet
	// using (R126), shown under the same conditions as UpdateAvailable.
	RestartPending string `json:"restartPending,omitempty"`
	// Limited is set while the session's account is limited (the card turns
	// its figures red). The facts below are for the mod's notices (M8b).
	Limited bool `json:"limited,omitempty"`
	// LastSwitch is the pool's last auto-switch: the account it moved to,
	// the one it left and why ("limit" or "threshold").
	LastSwitch *statuslineSwitch `json:"lastSwitch,omitempty"`
	// Remote is the pool's remote account: "" when it has none, and left out
	// when not computed (not routed, daemon down). A pointer so the mod can
	// tell "no remote" (0.9.0 and later) from an older chottag that never
	// says. RemoteToken is its token state when that is not healthy ("stale",
	// "needs-login").
	Remote      *string `json:"remote,omitempty"`
	RemoteToken string  `json:"remoteToken,omitempty"`

	resets time.Time
}

type statuslineSwitch struct {
	Account string `json:"account"`
	From    string `json:"from"`
	Reason  string `json:"reason"`
	At      string `json:"at,omitempty"`
}

// runStatusline prints one line for Claude Code's status line: whether this
// session goes through chottag, which account serves it, and its usage. It
// never reads stdin and always exits 0: a status line must never show an
// error. Nothing secret ever reaches the output. It has no side effect unless
// --cmux asks for the cmux sidebar pill (R121).
func runStatusline(args []string, r *reporter) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	cmux := fs.Bool("cmux", false, "also set the cmux sidebar pill")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage("usage: chottag statusline [--json] [--cmux]")
	}

	rep := statuslineResult{Session: "home", Daemon: "unknown"}
	h, herr := home()
	if herr == nil {
		rep = statuslineCheck(h)
	}
	// Without --cmux the command only prints: no cmux call, no throttle file.
	if *cmux && herr == nil {
		if ws := os.Getenv("CMUX_WORKSPACE_ID"); ws != "" {
			sendPill(h, ws, renderStatusline(rep, brand.None), rep.Label)
		}
	}
	if r.JSON() {
		return r.OK(rep)
	}
	mode := brand.ModeFromEnv(os.Getenv)
	fmt.Fprintln(r.Stdout(), renderStatusline(rep, mode))
	return r.OK(nil)
}

// renderStatusline builds the line for rep. Only the mark and the label are
// coloured, and a limited serving account's two usage fields.
func renderStatusline(rep statuslineResult, mode brand.ColorMode) string {
	head := brand.Mark
	if rep.Label != "" {
		head += " " + rep.Label
	}
	head = brand.Paint(head, rep.Label, mode)
	switch {
	case rep.Session != "routed":
		return head + " off"
	case rep.Daemon != "up":
		return head + " down"
	case rep.Account == "":
		return head + " up"
	}
	field := func(name string, pct *float64) string {
		v := "–"
		if pct != nil {
			v = strconv.Itoa(int(math.Round(*pct))) + "%"
		}
		f := name + " " + v
		if rep.Limited {
			f = brand.PaintLimited(f, mode)
		}
		return f
	}
	acct := rep.Account
	if rep.Pool != "" {
		acct += " [" + rep.Pool + "]"
	}
	first := []string{head + " " + acct}
	if rep.Label != "" {
		first = []string{head, acct} // "c» dev · work": not one name
	}
	parts := append(first, field("5h", rep.FiveHourPct), field("7d", rep.SevenDayPct))
	if rep.ResetsAt != "" {
		parts = append(parts, "\u21bb "+shortReset(rep.resets, statuslineNow()))
	}
	if rep.RotationAccounts != nil && rep.OKAccounts != nil {
		parts = append(parts, fmt.Sprintf("%d/%d ok", *rep.OKAccounts, *rep.RotationAccounts))
	}
	if rep.UpdateAvailable != "" {
		parts = append(parts, "\u2191"+rep.UpdateAvailable)
	}
	if rep.RestartPending != "" {
		parts = append(parts, "\u27f3"+rep.RestartPending)
	}
	return strings.Join(parts, " \u00b7 ")
}

// shortReset is how long until at, in the status line's short form and in
// now's zone: "42m" under an hour (rounded down; "<1m" under a minute),
// "3h20m" under a day, "Mon 18:00" within 6 days, "Oct 9 18:00" beyond. The
// card in the Claude Code mod shows the same brackets in long form.
func shortReset(at, now time.Time) string {
	at = at.In(now.Location())
	d := at.Sub(now)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	case d < 6*24*time.Hour:
		return at.Format("Mon 15:04")
	}
	return at.Format("Jan 2 15:04")
}

// pillName keeps only [A-Za-z0-9_-] of a workspace id for a file name.
func pillName(ws string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, ws)
}

// sendPill sets the cmux pill when line differs from the last one sent for
// the workspace. The cache is written only after a successful send; every
// failure is ignored: the pill is a courtesy, not part of the status line.
func sendPill(h, ws, line, label string) {
	path := filepath.Join(h, "run", "pill-"+pillName(ws)+".txt")
	if b, err := os.ReadFile(path); err == nil && string(b) == line {
		return
	}
	color := "#005FAF"
	if label != "" {
		color = "#FFAF00"
	}
	if err := cmuxSetStatus(context.Background(), ws, line, color); err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = os.WriteFile(path, []byte(line), 0o600)
	}
}

// statuslineCheck decides routed or home for home h, and probes the daemon
// only when routed. Any error means "home": a status line has no error
// channel.
func statuslineCheck(h string) statuslineResult {
	rep := statuslineResult{Session: "home", Daemon: "unknown"}
	if _, err := os.Stat(filepath.Join(h, "state.json")); err != nil {
		return rep
	}
	st, err := (store.Store{Dir: h}).Load()
	if err != nil {
		return rep
	}
	rep.Label = st.Label
	port := st.ResolvedPort()
	// The sid comes from the credential's user name in HTTPS_PROXY; the
	// password is never read. Without one, the ancestor walk's registry entry.
	routed, sid, pool := proxyEnvSession(os.Getenv("HTTPS_PROXY"), port)
	if !routed {
		if live, ok := liveAncestor(h); ok {
			routed, sid, pool = true, live.SID, live.Pool
		}
	}
	if !st.HasPool(pool) {
		pool = store.DefaultPool // none, or a pool since removed
	}
	if !routed {
		return rep
	}
	rep.Session = "routed"
	if !statuslineProbe(port) {
		rep.Daemon = "down"
		return rep
	}
	serving := st.PoolOf(pool).Serving
	rep.Daemon, rep.Serving = "up", serving
	if pool != store.DefaultPool {
		rep.Pool = pool
	}
	f, _ := status.Load(status.Path(h)) // a missing cache is not an error
	rep.Account = sessionAccount(f, sid, serving)
	fillUsage(&rep, st, pool, f)
	fillNotices(&rep, st, pool, f)
	if rep.Account != "" {
		rep.UpdateAvailable = availableUpdate(f)
		if f.Daemon != nil {
			rep.RestartPending = f.Daemon.RestartPending
		}
	}
	return rep
}

// sessionAccount is the account sid's session uses per the cache, else
// serving (no sid, an unknown sid, or no inference request seen yet).
func sessionAccount(f status.File, sid, serving string) string {
	if sid != "" {
		for _, a := range f.Sessions {
			if a.SID == sid && a.Account != "" {
				return a.Account
			}
		}
	}
	return serving
}

// fillNotices adds what the mod's notices compare between refreshes: the
// pool's last switch, its remote account and that account's token state when
// it is not healthy. Unknown values stay unset.
func fillNotices(rep *statuslineResult, st store.State, pool string, f status.File) {
	if f.Auto != nil {
		ls := f.Auto.LastSwitch
		if pool != store.DefaultPool {
			ls = nil
			if p, ok := f.Auto.Pools[pool]; ok {
				ls = p.LastSwitch
			}
		}
		if ls != nil {
			sw := &statuslineSwitch{Account: ls.To, From: ls.From, Reason: ls.Trigger}
			if !ls.At.IsZero() {
				sw.At = ls.At.In(statuslineNow().Location()).Format(time.RFC3339)
			}
			rep.LastSwitch = sw
		}
	}
	remote := st.PoolOf(pool).Remote
	rep.Remote = &remote
	if remote == "" {
		return
	}
	for _, a := range f.Accounts {
		if strings.EqualFold(a.Name, remote) && (a.Token == creds.StateStale || a.Token == creds.StateNeedsLogin) {
			rep.RemoteToken = string(a.Token)
		}
	}
}

// fillUsage adds the serving account's usage, its next reset and the pool
// health from the status cache. Unknown values stay unset.
func fillUsage(rep *statuslineResult, st store.State, pool string, f status.File) {
	now := statuslineNow()
	byName := make(map[string]status.Account, len(f.Accounts))
	for _, a := range f.Accounts {
		byName[strings.ToLower(a.Name)] = a
	}
	rotation, ok := 0, 0
	for _, a := range st.Members(pool) {
		if a.NoRotate {
			continue
		}
		rotation++
		if c := byName[strings.ToLower(a.Name)]; !(c.Limited && (c.LimitedUntil.IsZero() || c.LimitedUntil.After(now))) {
			ok++
		}
	}
	if rotation > 0 {
		rep.RotationAccounts, rep.OKAccounts = &rotation, &ok
	}
	if rep.Account == "" {
		return
	}
	a, found := byName[strings.ToLower(rep.Account)]
	if !found {
		return
	}
	rep.Limited = a.Limited && (a.LimitedUntil.IsZero() || a.LimitedUntil.After(now))
	if a.Usage == nil || !f.Fresh(rep.Account, now) {
		return
	}
	u := a.Usage
	rep.FiveHourPct, rep.SevenDayPct = u.FiveHourPct, u.SevenDayPct
	// The reset rule: the 5h reset when the 5h window is nearly used up,
	// otherwise whichever known reset comes first. A reset already past is
	// unknown, not "now".
	var five, seven time.Time
	if u.FiveHourResetsAt.After(now) {
		five = u.FiveHourResetsAt
	}
	if u.SevenDayResetsAt.After(now) {
		seven = u.SevenDayResetsAt
	}
	if !five.IsZero() {
		rep.FiveHourResetsAt = five.In(now.Location()).Format(time.RFC3339)
	}
	if !seven.IsZero() {
		rep.SevenDayResetsAt = seven.In(now.Location()).Format(time.RFC3339)
	}
	rep.NearWindow = nearWindow(st, rep.Account, u)
	var at time.Time
	switch {
	case !five.IsZero() && u.FiveHourPct != nil && *u.FiveHourPct >= 80:
		at = five
	case five.IsZero():
		at = seven
	case seven.IsZero() || five.Before(seven):
		at = five
	default:
		at = seven
	}
	if !at.IsZero() {
		rep.resets = at
		rep.ResetsAt = at.In(now.Location()).Format(time.RFC3339)
	}
}

// proxyEnvSession reports whether v is http://<user>:<anything>@127.0.0.1:port,
// the URL the shim hands a session it launched, and the sid its user names:
// "chottag" (the legacy form) is routed with no sid; "chottag.<pool>.<sid>"
// with a valid pool and sid is routed with that sid and pool. Only the user name is
// read: the password never leaves url.Parse's result.
func proxyEnvSession(v string, port int) (routed bool, sid, pool string) {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.User == nil {
		return false, "", ""
	}
	if u.Hostname() != "127.0.0.1" || u.Port() != strconv.Itoa(port) {
		return false, "", ""
	}
	user := u.User.Username()
	if user == "chottag" {
		return true, "", ""
	}
	parts := strings.Split(user, ".")
	if len(parts) == 3 && parts[0] == "chottag" && proxyauth.ValidPool(parts[1]) && proxyauth.ValidSID(parts[2]) {
		return true, parts[2], parts[1]
	}
	return false, "", ""
}

// liveAncestor returns the registry entry of the claude session chottag
// launched that is one of this process's first statuslineMaxDepth ancestors.
// The shim registers the pid it then execs claude in, so that pid is claude.
// liveSessions is the reader `daemon stop` uses: Live() prunes dead entries
// on read.
func liveAncestor(h string) (session.Session, bool) {
	live, err := liveSessions(h)
	if err != nil || len(live) == 0 {
		return session.Session{}, false
	}
	set := make(map[int]session.Session, len(live))
	for _, s := range live {
		set[s.PID] = s
	}
	pid := os.Getppid()
	for level := 0; level < statuslineMaxDepth; level++ {
		if s, ok := set[pid]; ok {
			return s, true
		}
		if level == statuslineMaxDepth-1 {
			break // the last pid is checked; no lookup whose result is dropped
		}
		next, err := parentPID(pid)
		if err != nil || next <= 1 || next == pid {
			return session.Session{}, false
		}
		pid = next
	}
	return session.Session{}, false
}

// nearWindow is the window ("5h" or "7d") whose use is closest to the account's
// switch point, or "" when no use is known. A tie is the 5h window.
func nearWindow(st store.State, account string, u *status.Usage) string {
	tier := autoswitch.TierMax5x
	for _, a := range st.Accounts {
		if strings.EqualFold(a.Name, account) {
			tier = tierOf(a)
		}
	}
	p := autoParams(st)
	best, bestGap := "", math.Inf(1)
	for _, w := range []struct {
		win autoswitch.Window
		pct *float64
	}{{autoswitch.Win5h, u.FiveHourPct}, {autoswitch.Win7d, u.SevenDayPct}} {
		if w.pct == nil {
			continue
		}
		if gap := float64(p.SwitchPoint(w.win, tier)) - *w.pct; gap < bestGap {
			best, bestGap = string(w.win), gap
		}
	}
	return best
}
