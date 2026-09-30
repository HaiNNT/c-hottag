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

	"github.com/HaiNNT/c-hottag/internal/brand"
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
	Session          string   `json:"session"`
	Daemon           string   `json:"daemon"`
	Serving          string   `json:"serving"`
	Label            string   `json:"label,omitempty"`
	FiveHourPct      *float64 `json:"fiveHourPct,omitempty"`
	SevenDayPct      *float64 `json:"sevenDayPct,omitempty"`
	ResetsAt         string   `json:"resetsAt,omitempty"`
	OKAccounts       *int     `json:"okAccounts,omitempty"`
	RotationAccounts *int     `json:"rotationAccounts,omitempty"`

	limited bool
	resets  time.Time
}

// runStatusline prints one line for Claude Code's status line: whether this
// session goes through chottag, which account serves it, and its usage. It
// never reads stdin and always exits 0: a status line must never show an
// error. Nothing secret ever reaches the output.
func runStatusline(args []string, r *reporter) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage("usage: chottag statusline [--json]")
	}

	rep := statuslineResult{Session: "home", Daemon: "unknown"}
	h, herr := home()
	if herr == nil {
		rep = statuslineCheck(h)
	}
	if r.JSON() {
		return r.OK(rep)
	}
	mode := brand.ModeFromEnv(os.Getenv)
	fmt.Fprintln(r.Stdout(), renderStatusline(rep, mode))
	if herr == nil {
		if ws := os.Getenv("CMUX_WORKSPACE_ID"); ws != "" {
			sendPill(h, ws, renderStatusline(rep, brand.None), rep.Label)
		}
	}
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
	case rep.Serving == "":
		return head + " up"
	}
	field := func(name string, pct *float64) string {
		v := "–"
		if pct != nil {
			v = strconv.Itoa(int(math.Round(*pct))) + "%"
		}
		f := name + " " + v
		if rep.limited {
			f = brand.PaintLimited(f, mode)
		}
		return f
	}
	first := []string{head + " " + rep.Serving}
	if rep.Label != "" {
		first = []string{head, rep.Serving} // "c» dev · work": not one name
	}
	parts := append(first, field("5h", rep.FiveHourPct), field("7d", rep.SevenDayPct))
	if rep.ResetsAt != "" {
		now := statuslineNow()
		layout := "15:04"
		if rep.resets.Sub(now) > 24*time.Hour {
			layout = "Mon 15:04"
		}
		parts = append(parts, "\u21bb "+rep.resets.In(now.Location()).Format(layout))
	}
	if rep.RotationAccounts != nil && rep.OKAccounts != nil {
		parts = append(parts, fmt.Sprintf("%d/%d ok", *rep.OKAccounts, *rep.RotationAccounts))
	}
	return strings.Join(parts, " \u00b7 ")
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
	if !proxyEnvMatches(os.Getenv("HTTPS_PROXY"), port) && !hasLiveAncestor(h) {
		return rep
	}
	rep.Session = "routed"
	if !statuslineProbe(port) {
		rep.Daemon = "down"
		return rep
	}
	rep.Daemon, rep.Serving = "up", st.Serving
	fillUsage(&rep, st, h)
	return rep
}

// fillUsage adds the serving account's usage, its next reset and the pool
// health from the status cache. Unknown values stay unset.
func fillUsage(rep *statuslineResult, st store.State, h string) {
	now := statuslineNow()
	f, _ := status.Load(status.Path(h)) // a missing cache is not an error
	byName := make(map[string]status.Account, len(f.Accounts))
	for _, a := range f.Accounts {
		byName[strings.ToLower(a.Name)] = a
	}
	rotation, ok := 0, 0
	for _, a := range st.Accounts {
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
	if rep.Serving == "" {
		return
	}
	a, found := byName[strings.ToLower(rep.Serving)]
	if !found {
		return
	}
	rep.limited = a.Limited && (a.LimitedUntil.IsZero() || a.LimitedUntil.After(now))
	if a.Usage == nil || !f.Fresh(rep.Serving, now) {
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

// proxyEnvMatches reports whether v is http://chottag:<anything>@127.0.0.1:port,
// the URL the shim hands a session it launched.
func proxyEnvMatches(v string, port int) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.User == nil || u.User.Username() != "chottag" {
		return false
	}
	return u.Hostname() == "127.0.0.1" && u.Port() == strconv.Itoa(port)
}

// hasLiveAncestor reports whether one of this process's first
// statuslineMaxDepth ancestors is a claude session chottag launched. The
// shim registers the pid it then execs claude in, so that pid is claude.
// liveSessions is the reader `daemon stop` uses: Live() prunes dead entries
// on read.
func hasLiveAncestor(h string) bool {
	live, err := liveSessions(h)
	if err != nil || len(live) == 0 {
		return false
	}
	set := make(map[int]bool, len(live))
	for _, s := range live {
		set[s.PID] = true
	}
	pid := os.Getppid()
	for level := 0; level < statuslineMaxDepth; level++ {
		if set[pid] {
			return true
		}
		if level == statuslineMaxDepth-1 {
			break // the last pid is checked; no lookup whose result is dropped
		}
		next, err := parentPID(pid)
		if err != nil || next <= 1 || next == pid {
			return false
		}
		pid = next
	}
	return false
}
