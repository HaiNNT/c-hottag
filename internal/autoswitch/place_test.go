package autoswitch

import (
	"math"
	"testing"
	"time"
)

var placeNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func placeAcct(name string, mod func(*Account)) Account {
	a := Account{Name: name, Tier: TierMax5x, Rotates: true, Fresh: true}
	if mod != nil {
		mod(&a)
	}
	return a
}

func TestCandidate(t *testing.T) {
	bal := Preset(ModeBalanced)
	tests := []struct {
		name string
		mod  func(*Account)
		p    Params
		want bool
	}{
		{"plain", nil, bal, true},
		{"not rotating", func(a *Account) { a.Rotates = false }, bal, false},
		{"needs login", func(a *Account) { a.NeedsLogin = true }, bal, false},
		{"limited", func(a *Account) { a.Limited = true }, bal, false},
		{"over 5h point", func(a *Account) { a.Has5h, a.Pct5h = true, 93 }, bal, false},
		{"under 5h point", func(a *Account) { a.Has5h, a.Pct5h = true, 92 }, bal, true},
		{"over 7d point", func(a *Account) { a.Has7d, a.Pct7d = true, 98 }, bal, false},
		{"stale over point, reset ahead", func(a *Account) {
			a.Fresh, a.Has5h, a.Pct5h, a.Reset5h = false, true, 95, placeNow.Add(time.Hour)
		}, bal, false},
		{"over point but reset passed", func(a *Account) {
			a.Has5h, a.Pct5h, a.Reset5h = true, 95, placeNow.Add(-time.Minute)
		}, bal, true},
		{"cache-optimize 95% is fine", func(a *Account) { a.Has5h, a.Pct5h = true, 95 }, Preset(ModeCacheOptimize), true},
		{"cache-optimize 100% is not", func(a *Account) { a.Has5h, a.Pct5h = true, 100 }, Preset(ModeCacheOptimize), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Candidate(placeAcct("A", tc.mod), tc.p, placeNow); got != tc.want {
				t.Fatalf("Candidate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHeadroom(t *testing.T) {
	bal := Preset(ModeBalanced)
	cache := Preset(ModeCacheOptimize)
	tests := []struct {
		name string
		mod  func(*Account)
		p    Params
		want float64
	}{
		{"fresh max5x balanced", nil, bal, 0.93 * 5},
		{"fresh pro", func(a *Account) { a.Tier = TierPro }, bal, 0.88},
		{"fresh max20x", func(a *Account) { a.Tier = TierMax20x }, bal, 0.98 * 20},
		{"explicit units", func(a *Account) { a.Units = 2 }, bal, 0.93 * 2},
		{"5h tighter", func(a *Account) {
			a.Has5h, a.Pct5h, a.Has7d, a.Pct7d = true, 43, true, 10
		}, bal, 0.5 * 5},
		{"7d tighter", func(a *Account) {
			a.Has5h, a.Pct5h, a.Has7d, a.Pct7d = true, 10, true, 48
		}, bal, 0.5 * 5},
		{"5h reset passed is fresh", func(a *Account) {
			a.Has5h, a.Pct5h, a.Reset5h = true, 90, placeNow
		}, bal, 0.93 * 5},
		{"5h reset ahead counts", func(a *Account) {
			a.Has5h, a.Pct5h, a.Reset5h = true, 63, placeNow.Add(time.Minute)
		}, bal, 0.3 * 5},
		{"over point clamps to 0", func(a *Account) { a.Has5h, a.Pct5h = true, 99 }, bal, 0},
		{"cache-optimize point is 100", nil, cache, 5},
		{"cache-optimize half used", func(a *Account) { a.Has7d, a.Pct7d = true, 50 }, cache, 0.5 * 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Headroom(placeAcct("A", tc.mod), tc.p, placeNow)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Headroom = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlaceTiers(t *testing.T) {
	// Same usage, different mode: balanced makes the point the ceiling.
	a := placeAcct("A", func(a *Account) { a.Has5h, a.Pct5h = true, 80 })
	b := placeAcct("B", func(a *Account) { a.Has5h, a.Pct5h = true, 50 })
	name, ok := Place([]Account{a, b}, nil, "", nil, Preset(ModeBalanced), placeNow)
	if !ok || name != "B" {
		t.Fatalf("balanced Place = %q,%v, want B", name, ok)
	}
	// Per tier: a pro at 80% (point 88, headroom .08) loses to max5x at 80% (.13).
	pro := placeAcct("P", func(a *Account) { a.Tier = TierPro; a.Units = 5; a.Has5h, a.Pct5h = true, 80 })
	mx := placeAcct("M", func(a *Account) { a.Has5h, a.Pct5h = true, 80 })
	name, _ = Place([]Account{pro, mx}, nil, "", nil, Preset(ModeBalanced), placeNow)
	if name != "M" {
		t.Fatalf("per-tier point Place = %q, want M", name)
	}
	// cache-optimize: pro at 80% has .2, max5x at 80% has .2; tie keeps earlier.
	name, _ = Place([]Account{pro, mx}, nil, "", nil, Preset(ModeCacheOptimize), placeNow)
	if name != "P" {
		t.Fatalf("cache-optimize tie Place = %q, want P", name)
	}
}

func TestPlaceSpreadsLoadEvenly(t *testing.T) {
	var accts []Account
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		accts = append(accts, placeAcct(n, nil))
	}
	load := map[string]int{}
	for i := 0; i < 50; i++ {
		name, ok := Place(accts, load, "", nil, Preset(ModeBalanced), placeNow)
		if !ok {
			t.Fatal("no placement")
		}
		load[name]++
	}
	for _, a := range accts {
		if load[a.Name] != 5 {
			t.Fatalf("load = %v, want 5 each", load)
		}
	}
}

func TestPlaceHeavierAccountTakesMore(t *testing.T) {
	big := placeAcct("big", func(a *Account) { a.Tier = TierMax20x })
	small := placeAcct("small", func(a *Account) { a.Tier = TierPro })
	load := map[string]int{}
	for i := 0; i < 20; i++ {
		name, _ := Place([]Account{small, big}, load, "", nil, Preset(ModeBalanced), placeNow)
		load[name]++
	}
	if load["big"] <= load["small"] {
		t.Fatalf("load = %v, want big > small", load)
	}
}

func TestPlacePinExcludeTies(t *testing.T) {
	bal := Preset(ModeBalanced)
	a := placeAcct("A", nil)
	b := placeAcct("B", nil)
	busy := map[string]int{"B": 9}
	over := placeAcct("O", func(a *Account) { a.Has5h, a.Pct5h = true, 99 })

	t.Run("tie goes to earlier", func(t *testing.T) {
		if n, ok := Place([]Account{a, b}, nil, "", nil, bal, placeNow); !ok || n != "A" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("pin wins over score", func(t *testing.T) {
		if n, ok := Place([]Account{a, b}, map[string]int{"B": 9}, "B", nil, bal, placeNow); !ok || n != "B" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("pin that is not a candidate is ignored", func(t *testing.T) {
		if n, ok := Place([]Account{a, over}, nil, "O", nil, bal, placeNow); !ok || n != "A" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("unknown pin is ignored", func(t *testing.T) {
		if n, ok := Place([]Account{a, b}, busy, "Z", nil, bal, placeNow); !ok || n != "A" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("excluded pin is ignored", func(t *testing.T) {
		if n, ok := Place([]Account{a, b}, nil, "A", map[string]bool{"A": true}, bal, placeNow); !ok || n != "B" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("exclude skips", func(t *testing.T) {
		if n, ok := Place([]Account{a, b}, nil, "", map[string]bool{"A": true}, bal, placeNow); !ok || n != "B" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("load tips the choice", func(t *testing.T) {
		if n, ok := Place([]Account{a, b}, map[string]int{"A": 1}, "", nil, bal, placeNow); !ok || n != "B" {
			t.Fatalf("got %q,%v", n, ok)
		}
	})
	t.Run("no candidate", func(t *testing.T) {
		if n, ok := Place([]Account{over}, nil, "", nil, bal, placeNow); ok || n != "" {
			t.Fatalf("got %q,%v", n, ok)
		}
		if n, ok := Place([]Account{a}, nil, "", map[string]bool{"A": true}, bal, placeNow); ok || n != "" {
			t.Fatalf("all excluded: got %q,%v", n, ok)
		}
		if n, ok := Place(nil, nil, "", nil, bal, placeNow); ok || n != "" {
			t.Fatalf("empty: got %q,%v", n, ok)
		}
	})
}

// TestPlaceNeverPicksANonCandidate pins R90 at the Place level: an account
// with rotation off, or one needing a login or limited, is never returned,
// not even as the pin or as the only account.
func TestPlaceNeverPicksANonCandidate(t *testing.T) {
	p := Preset(ModeBalanced)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	bad := map[string]func(*Account){
		"rotation off": func(a *Account) { a.Rotates = false },
		"needs login":  func(a *Account) { a.NeedsLogin = true },
		"limited":      func(a *Account) { a.Limited = true },
	}
	for name, mod := range bad {
		t.Run(name, func(t *testing.T) {
			if got, ok := Place([]Account{placeAcct("A", mod)}, nil, "A", nil, p, now); ok {
				t.Fatalf("Place = %q, true; want no candidate", got)
			}
			got, ok := Place([]Account{placeAcct("A", mod), placeAcct("B", nil)}, nil, "A", nil, p, now)
			if !ok || got != "B" {
				t.Fatalf("Place = %q, %v; want B, true", got, ok)
			}
		})
	}
}

func TestPlaceNegativeLoadCountsAsZero(t *testing.T) {
	p := Preset(ModeBalanced)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	got, ok := Place([]Account{placeAcct("A", nil), placeAcct("B", nil)}, map[string]int{"A": -1}, "", nil, p, now)
	if !ok || got != "A" {
		t.Fatalf("Place = %q, %v; want A (tie, earlier), true", got, ok)
	}
}
