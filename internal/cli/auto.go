package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const autoUsage = "usage: chottag auto [on|off | mode balanced|cache-optimize | set KEY VALUE | reset]"

// autoAccount is one account in `auto --json`: its plan as stored ("" when
// unknown), the tier and units the planner uses, and its usage (0-100,
// absent when unknown; stale means too old to act on).
type autoAccount struct {
	Name             string    `json:"name"`
	Plan             string    `json:"plan,omitempty"`
	Tier             string    `json:"tier"`
	Units            float64   `json:"units"`
	FiveHourPct      *float64  `json:"fiveHourPct,omitempty"`
	FiveHourResetsAt time.Time `json:"fiveHourResetsAt,omitzero"`
	SevenDayPct      *float64  `json:"sevenDayPct,omitempty"`
	SevenDayResetsAt time.Time `json:"sevenDayResetsAt,omitzero"`
	Stale            bool      `json:"stale"`
}

// autoResult is `auto --json`'s fields (spec §7): the effective settings,
// which of them the user overrode, each account, and the daemon's view.
type autoResult struct {
	Enabled      bool           `json:"enabled"`
	Mode         string         `json:"mode"`
	SwitchPoints map[string]int `json:"switchPoints"`
	Hold5h       string         `json:"hold5h"`
	Hold7d       string         `json:"hold7d"`
	Cooldown     string         `json:"cooldown"`
	// Overrides lists the keys `auto set` stored, which `auto reset`
	// clears. Always an array.
	Overrides []string      `json:"overrides"`
	Accounts  []autoAccount `json:"accounts"`
	// Decision is the daemon's last decision, present only while the
	// daemon is running (its heartbeat is fresh).
	Decision   string             `json:"decision,omitempty"`
	LastSwitch *status.AutoSwitch `json:"lastSwitch,omitempty"`
}

// runAuto shows or changes the auto-switch settings (M4 spec §7). With no
// verb it only reads. Every verb writes state.json through store.Update and
// then shows the result; the daemon reads state.json per decision, so a
// change applies without a restart.
func runAuto(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, autoUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	var edit func(*store.State) error
	switch {
	case len(args) == 0:
	case len(args) == 1 && (args[0] == "on" || args[0] == "off"):
		on := args[0] == "on"
		edit = func(st *store.State) error { st.SetAutoEnabled(on); return nil }
	case len(args) == 2 && args[0] == "mode":
		m, ok := autoswitch.ParseMode(args[1])
		if !ok {
			return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("auto mode takes balanced or cache-optimize, not %q", args[1]), nil)
		}
		edit = func(st *store.State) error { st.SetAutoMode(string(m)); return nil }
	case len(args) == 3 && args[0] == "set":
		if err := autoswitch.ValidateSetting(args[1], args[2]); err != nil {
			return r.Fail(exit.Usage, codeUsage, err.Error(), nil)
		}
		edit = func(st *store.State) error { return st.SetAutoSetting(args[1], args[2]) }
	case len(args) == 1 && args[0] == "reset":
		edit = func(st *store.State) error { st.ResetAutoSettings(); return nil }
	default:
		return r.Usage(autoUsage)
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}
	var st store.State
	if edit == nil {
		st, err = s.Load()
	} else {
		st, err = s.Update(edit)
	}
	if err != nil {
		return r.FailErr(err)
	}
	f, _ := status.Load(status.Path(h))
	f.EnsureRoster(accountMembers(st)) // rows follow slot dirs (F171)
	now := time.Now()
	res := buildAutoResult(st, f, now)
	if !r.JSON() {
		renderAuto(r.Stdout(), res, now)
	}
	return r.OK(res)
}

// buildAutoResult is `chottag auto`'s document: the effective parameters
// (autoParams: the preset plus every valid override), the planner's view of
// each account, and the daemon's last decision and switch.
func buildAutoResult(st store.State, f status.File, now time.Time) autoResult {
	p := autoParams(st)
	res := autoResult{
		Enabled: st.AutoOn(), Mode: string(p.Mode), SwitchPoints: p.SwitchPoints(),
		Hold5h: durText(p.Hold5h), Hold7d: durText(p.Hold7d), Cooldown: durText(p.Cooldown),
		Overrides: []string{}, Accounts: []autoAccount{},
	}
	a := st.AutoSettings()
	for k := range a.SwitchPoints {
		res.Overrides = append(res.Overrides, k)
	}
	sort.Strings(res.Overrides)
	for _, kv := range [][2]string{{"hold5h", a.Hold5h}, {"hold7d", a.Hold7d}, {"cooldown", a.Cooldown}} {
		if kv[1] != "" {
			res.Overrides = append(res.Overrides, kv[0])
		}
	}
	for i, pa := range planAccounts(&st, &f, now) {
		acc := st.Accounts[i]
		aa := autoAccount{Name: pa.Name, Plan: acc.Plan, Tier: string(pa.Tier), Units: pa.Units, Stale: !pa.Fresh}
		if pa.Has5h {
			aa.FiveHourPct = autoPct(pa.Pct5h)
		}
		if pa.Has7d {
			aa.SevenDayPct = autoPct(pa.Pct7d)
		}
		aa.FiveHourResetsAt, aa.SevenDayResetsAt = pa.Reset5h, pa.Reset7d
		res.Accounts = append(res.Accounts, aa)
	}
	if f.Auto != nil {
		f.DaemonRunningAt(now)
		if f.Daemon != nil && f.Daemon.Running {
			res.Decision = f.Auto.Decision
		}
		res.LastSwitch = f.Auto.LastSwitch
	}
	return res
}

// autoPct returns a pointer to a copy of v.
func autoPct(v float64) *float64 { return &v }

// durText renders a setting's duration the way it is typed: "30m", "3h",
// "1h30m", "0s".
func durText(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// renderAuto is `chottag auto`'s text.
func renderAuto(out io.Writer, res autoResult, now time.Time) {
	state := "off"
	if res.Enabled {
		state = "on"
	}
	fmt.Fprintf(out, "auto: %s · mode %s\n", state, res.Mode)
	for _, w := range []autoswitch.Window{autoswitch.Win5h, autoswitch.Win7d} {
		parts := make([]string, 0, 4)
		for _, t := range autoswitch.Tiers() {
			parts = append(parts, fmt.Sprintf("%s %d%%", t, res.SwitchPoints[autoswitch.PointKey(w, t)]))
		}
		label := "switch points:"
		if w == autoswitch.Win7d {
			label = "              "
		}
		fmt.Fprintf(out, "%s %s %s\n", label, w, strings.Join(parts, " · "))
	}
	fmt.Fprintf(out, "holds: 5h %s · 7d %s · cooldown %s\n", res.Hold5h, res.Hold7d, res.Cooldown)
	if len(res.Overrides) > 0 {
		fmt.Fprintf(out, "overrides: %s (chottag auto reset clears them)\n", strings.Join(res.Overrides, ", "))
	}
	if len(res.Accounts) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tPLAN\t5h\tRESETS\t7d\tRESETS")
		for _, a := range res.Accounts {
			plan := a.Plan
			switch plan {
			case "":
				plan = "-"
			case "max":
				plan = "max?"
			}
			five, seven := "unknown", "unknown"
			if !a.Stale {
				five, seven = pctOrUnknown(a.FiveHourPct), pctOrUnknown(a.SevenDayPct)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", a.Name, plan, five, whenText(a.FiveHourResetsAt, now), seven, whenText(a.SevenDayResetsAt, now))
		}
		tw.Flush()
	}
	if res.Decision != "" {
		fmt.Fprintf(out, "decision: %s\n", res.Decision)
	}
	if ls := res.LastSwitch; ls != nil {
		fmt.Fprintf(out, "last switch: %s\n", lastSwitchLine(*ls))
	}
}

// whenText is a reset time in local time: "14:05" within a day, "Mon
// 09:00" further out, "-" when unknown or already past.
func whenText(t, now time.Time) string {
	if t.IsZero() || !t.After(now) {
		return "-"
	}
	if t.Sub(now) < 24*time.Hour {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Mon 15:04")
}

// lastSwitchLine is "C -> A 09:12 (limit 5h)" or "A -> B 09:12
// (threshold 5h 96%), resent".
func lastSwitchLine(ls status.AutoSwitch) string {
	why := ls.Trigger
	if ls.Window != "" {
		why += " " + ls.Window
	}
	if ls.Trigger == autoswitch.TriggerThreshold && ls.Pct > 0 {
		why += fmt.Sprintf(" %.0f%%", ls.Pct)
	}
	line := fmt.Sprintf("%s -> %s", ls.From, ls.To)
	if !ls.At.IsZero() {
		line += " " + ls.At.Local().Format("15:04")
	}
	line += " (" + why + ")"
	if ls.Retried {
		line += ", request resent"
	}
	return line
}
