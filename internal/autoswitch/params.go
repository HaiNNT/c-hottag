// Package autoswitch is M4's planner: it decides when chottag should move
// the serving role off an account and where to move it. It is pure: every
// input, the clock included, arrives in an Input, and nothing here reads a
// file, the network or the time.
package autoswitch

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Tier is an account's plan size (M4 spec §2).
type Tier string

const (
	TierPro    Tier = "pro"
	TierMax5x  Tier = "max5x"
	TierMax20x Tier = "max20x"
	TierTeam   Tier = "team"
)

// Tiers lists every tier in the order the spec's §3 table shows them.
func Tiers() []Tier { return []Tier{TierPro, TierMax5x, TierTeam, TierMax20x} }

// ParseTier accepts exactly the four tier names.
func ParseTier(s string) (Tier, bool) {
	for _, t := range Tiers() {
		if s == string(t) {
			return t, true
		}
	}
	return "", false
}

// Units is how much a 1% step of this tier is worth (M4 spec §2).
func (t Tier) Units() float64 {
	switch t {
	case TierPro:
		return 1
	case TierMax20x:
		return 20
	default: // max5x, team, and an unknown tier (S2: the safer mistake)
		return 5
	}
}

// Window is a usage window: "5h" or "7d".
type Window string

const (
	Win5h Window = "5h"
	Win7d Window = "7d"
)

// Length is how long the window lasts before it refills.
func (w Window) Length() time.Duration {
	if w == Win7d {
		return 7 * 24 * time.Hour
	}
	return 5 * time.Hour
}

// Mode is a planner preset (M4 spec §6).
type Mode string

const (
	ModeBalanced      Mode = "balanced"
	ModeCacheOptimize Mode = "cache-optimize"
)

// ParseMode accepts exactly the two mode names.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case ModeBalanced, ModeCacheOptimize:
		return Mode(s), true
	}
	return "", false
}

// Order is how a mode picks the target among the candidates.
type Order int

const (
	// OrderContinuity takes the highest continuity score (balanced).
	OrderContinuity Order = iota
	// OrderRegistration takes the next account in registration order
	// (cache-optimize).
	OrderRegistration
)

// Limits on the settings `chottag auto set` accepts (M4 spec §7).
const (
	MinSwitchPoint = 50
	MaxSwitchPoint = 100
	MaxDuration    = 24 * time.Hour
)

// Params are one resolved set of planner parameters: a mode's preset with
// the user's overrides applied (S4).
type Params struct {
	Mode  Mode
	Order Order
	// points holds a switch point for every "5h.<tier>" and "7d.<tier>"
	// key. Unexported so a Params always carries the whole table.
	points   map[string]int
	Hold5h   time.Duration
	Hold7d   time.Duration
	Cooldown time.Duration
}

// defaultPoints is the M4 spec §3 table.
var defaultPoints = map[string]int{
	"5h.pro": 88, "5h.max5x": 93, "5h.team": 93, "5h.max20x": 98,
	"7d.pro": 93, "7d.max5x": 98, "7d.team": 98, "7d.max20x": 99,
}

// PointKey is the settings key for a window and tier, e.g. "5h.max20x".
func PointKey(w Window, t Tier) string { return string(w) + "." + string(t) }

// DefaultSwitchPoint is the §3 table's value.
func DefaultSwitchPoint(w Window, t Tier) int {
	if p, ok := defaultPoints[PointKey(w, t)]; ok {
		return p
	}
	return defaultPoints[PointKey(w, TierMax5x)]
}

// Preset returns a mode's parameters (M4 spec §6). An unknown mode is
// balanced, the default (S1).
func Preset(m Mode) Params {
	p := Params{points: map[string]int{}}
	if m == ModeCacheOptimize {
		p.Mode, p.Order = ModeCacheOptimize, OrderRegistration
		for k := range defaultPoints {
			p.points[k] = MaxSwitchPoint // at the wall only
		}
		return p
	}
	p.Mode, p.Order = ModeBalanced, OrderContinuity
	for k, v := range defaultPoints {
		p.points[k] = v
	}
	p.Hold5h, p.Hold7d, p.Cooldown = 30*time.Minute, 3*time.Hour, 15*time.Minute
	return p
}

// SwitchPoint is the utilization (0-100) at which the serving account
// should be left for this window and tier. 100 means switch only at the
// wall: a 100% reading or a refusal, never a threshold below it.
func (p Params) SwitchPoint(w Window, t Tier) int {
	if v, ok := p.points[PointKey(w, t)]; ok {
		return v
	}
	return DefaultSwitchPoint(w, t)
}

// SwitchPoints returns a copy of the whole table, keyed like the settings.
func (p Params) SwitchPoints() map[string]int {
	out := make(map[string]int, len(defaultPoints))
	for k := range defaultPoints {
		w, t, _ := strings.Cut(k, ".")
		out[k] = p.SwitchPoint(Window(w), Tier(t))
	}
	return out
}

// Hold is the cache-hold time for a window.
func (p Params) Hold(w Window) time.Duration {
	if w == Win7d {
		return p.Hold7d
	}
	return p.Hold5h
}

// SettingKeys lists every key `chottag auto set` accepts, in display order.
func SettingKeys() []string {
	var keys []string
	for _, w := range []Window{Win5h, Win7d} {
		for _, t := range Tiers() {
			keys = append(keys, PointKey(w, t))
		}
	}
	return append(keys, "hold5h", "hold7d", "cooldown")
}

// ValidateSetting checks one `auto set` pair without applying it: a switch
// point is an integer from 50 to 100, and a duration (Go syntax, e.g.
// "30m") is from 0 to 24h.
func ValidateSetting(key, value string) error {
	_, err := Preset(ModeBalanced).With(key, value)
	return err
}

// With returns p with one setting overridden.
func (p Params) With(key, value string) (Params, error) {
	switch key {
	case "hold5h", "hold7d", "cooldown":
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 || d > MaxDuration {
			return p, fmt.Errorf("%s must be a duration from 0 to 24h (for example 30m), not %q", key, value)
		}
		switch key {
		case "hold5h":
			p.Hold5h = d
		case "hold7d":
			p.Hold7d = d
		default:
			p.Cooldown = d
		}
		return p, nil
	}
	if _, ok := defaultPoints[key]; !ok {
		return p, fmt.Errorf("unknown setting %q; the settings are %s", key, strings.Join(SettingKeys(), ", "))
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < MinSwitchPoint || n > MaxSwitchPoint {
		return p, fmt.Errorf("%s must be a whole number from %d to %d, not %q", key, MinSwitchPoint, MaxSwitchPoint, value)
	}
	points := make(map[string]int, len(p.points)+1)
	for k, v := range p.points {
		points[k] = v
	}
	points[key] = n
	p.points = points
	return p, nil
}
