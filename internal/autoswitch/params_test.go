package autoswitch

import (
	"strings"
	"testing"
	"time"
)

func TestTierUnits(t *testing.T) {
	for tier, want := range map[Tier]float64{TierPro: 1, TierMax5x: 5, TierMax20x: 20, TierTeam: 5, "": 5, "max": 5} {
		if got := tier.Units(); got != want {
			t.Errorf("%q.Units() = %v, want %v", tier, got, want)
		}
	}
}

func TestParseTierAcceptsOnlyTheFourNames(t *testing.T) {
	for _, s := range []string{"pro", "max5x", "max20x", "team"} {
		if got, ok := ParseTier(s); !ok || string(got) != s {
			t.Errorf("ParseTier(%q) = %q, %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "max", "Pro", "enterprise", "max10x"} {
		if _, ok := ParseTier(s); ok {
			t.Errorf("ParseTier(%q) accepted it", s)
		}
	}
}

func TestParseModeAcceptsOnlyTheTwoModes(t *testing.T) {
	for _, s := range []string{"balanced", "cache-optimize"} {
		if got, ok := ParseMode(s); !ok || string(got) != s {
			t.Errorf("ParseMode(%q) = %q, %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "continuous", "simple", "Balanced"} {
		if _, ok := ParseMode(s); ok {
			t.Errorf("ParseMode(%q) accepted it", s)
		}
	}
}

// TestBalancedPresetIsTheSpecTable pins M4 spec §3's table and §6's
// balanced row verbatim.
func TestBalancedPresetIsTheSpecTable(t *testing.T) {
	p := Preset(ModeBalanced)
	want := map[string]int{
		"5h.pro": 88, "5h.max5x": 93, "5h.team": 93, "5h.max20x": 98,
		"7d.pro": 93, "7d.max5x": 98, "7d.team": 98, "7d.max20x": 99,
	}
	got := p.SwitchPoints()
	if len(got) != len(want) {
		t.Fatalf("SwitchPoints = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	if p.Mode != ModeBalanced || p.Order != OrderContinuity || p.Hold5h != 30*time.Minute || p.Hold7d != 3*time.Hour || p.Cooldown != 15*time.Minute {
		t.Fatalf("balanced = %+v, want continuity order, holds 30m/3h, cooldown 15m", p)
	}
}

func TestCacheOptimizePresetSwitchesOnlyAtTheWall(t *testing.T) {
	p := Preset(ModeCacheOptimize)
	for k, v := range p.SwitchPoints() {
		if v != 100 {
			t.Errorf("%s = %d, want 100", k, v)
		}
	}
	if p.Order != OrderRegistration || p.Hold5h != 0 || p.Hold7d != 0 || p.Cooldown != 0 {
		t.Fatalf("cache-optimize = %+v, want registration order, no holds, no cooldown", p)
	}
}

func TestAnUnknownModeIsBalanced(t *testing.T) {
	if p := Preset("continuous"); p.Mode != ModeBalanced {
		t.Fatalf("Preset(continuous).Mode = %q, want balanced (S1)", p.Mode)
	}
}

func TestWithOverridesOneSettingAndLeavesTheRest(t *testing.T) {
	base := Preset(ModeBalanced)
	p, err := base.With("5h.max20x", "95")
	if err != nil {
		t.Fatal(err)
	}
	if p.SwitchPoint(Win5h, TierMax20x) != 95 || p.SwitchPoint(Win7d, TierMax20x) != 99 {
		t.Fatalf("points = %v", p.SwitchPoints())
	}
	if base.SwitchPoint(Win5h, TierMax20x) != 98 {
		t.Fatal("With changed the Params it was called on")
	}
	p, err = p.With("cooldown", "0s")
	if err != nil || p.Cooldown != 0 {
		t.Fatalf("cooldown 0s = %v, %v", p.Cooldown, err)
	}
	p, err = p.With("hold7d", "24h")
	if err != nil || p.Hold7d != 24*time.Hour {
		t.Fatalf("hold7d 24h = %v, %v", p.Hold7d, err)
	}
}

// TestValidateSettingBounds pins spec §7: a switch point is an integer from
// 50 to 100, a duration from 0 to 24h; anything else is refused.
func TestValidateSettingBounds(t *testing.T) {
	good := [][2]string{{"5h.pro", "50"}, {"7d.team", "100"}, {"hold5h", "0s"}, {"hold7d", "24h"}, {"cooldown", "90s"}}
	for _, kv := range good {
		if err := ValidateSetting(kv[0], kv[1]); err != nil {
			t.Errorf("%s=%s refused: %v", kv[0], kv[1], err)
		}
	}
	bad := [][2]string{{"5h.pro", "49"}, {"5h.pro", "101"}, {"5h.pro", "95.5"}, {"5h.pro", "x"},
		{"hold5h", "-1m"}, {"hold5h", "24h1s"}, {"cooldown", "15"}, {"5h.max", "90"}, {"threshold", "95"}, {"mode", "balanced"}}
	for _, kv := range bad {
		if err := ValidateSetting(kv[0], kv[1]); err == nil {
			t.Errorf("%s=%s accepted", kv[0], kv[1])
		}
	}
}

func TestSettingKeysAreEveryKeyWithAccepts(t *testing.T) {
	keys := SettingKeys()
	if len(keys) != 11 {
		t.Fatalf("SettingKeys = %v, want 8 switch points and 3 durations", keys)
	}
	for _, k := range keys {
		v := "90"
		if !strings.Contains(k, ".") {
			v = "1m"
		}
		if err := ValidateSetting(k, v); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}
