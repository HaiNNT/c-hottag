package autoswitch

import (
	"math"
	"testing"
	"time"
)

func TestBurnReportsNothingUntilItHasSeenEnoughActiveTime(t *testing.T) {
	var b Burn
	if b.Rate() != 0 {
		t.Fatal("a new Burn has a rate")
	}
	// Two samples one minute apart: 1 minute of active time, under the
	// 15-minute minimum.
	b.Observe("A", 5, 10, t0)
	b.Observe("A", 5, 11, t0.Add(time.Minute))
	if got := b.Rate(); got != 0 {
		t.Fatalf("Rate after 1 active minute = %v, want 0 (the default applies)", got)
	}
}

// TestBurnMeasuresUnitsPerActiveHour feeds a steady max5x burn: one
// percentage point every 3 minutes is 20 points an hour, × 5 units = 100
// units an hour. Every sample decays the same way, so a steady input reads
// back exactly.
func TestBurnMeasuresUnitsPerActiveHour(t *testing.T) {
	var b Burn
	for i := 0; i <= 20; i++ { // 20 gaps of 3 minutes = 1 active hour
		b.Observe("A", 5, float64(10+i), t0.Add(time.Duration(3*i)*time.Minute))
	}
	if got := b.Rate(); math.Abs(got-100) > 1e-6 {
		t.Fatalf("Rate = %v, want 100 units an hour", got)
	}
}

// An idle gap (longer than BurnActiveGap) adds neither time nor usage, and
// a fall (a reset) adds time but no usage.
func TestBurnIgnoresIdleGapsAndResets(t *testing.T) {
	var b Burn
	at := t0
	for i := 0; i <= 10; i++ { // 10 gaps of 3m, 1 point each: 20 points/h × 1 unit
		b.Observe("A", 1, float64(i), at)
		at = at.Add(3 * time.Minute)
	}
	before := b.Rate()
	b.Observe("A", 1, 90, at.Add(2*time.Hour)) // idle for 2h: ignored
	if got := b.Rate(); got != before {
		t.Fatalf("an idle gap changed the rate %v -> %v", before, got)
	}
	b.Observe("A", 1, 0, at.Add(2*time.Hour+3*time.Minute)) // a reset: time, no burn
	if got := b.Rate(); got >= before {
		t.Fatalf("a reset raised or kept the rate %v -> %v; it adds active time with no burn", before, got)
	}
}

// TestBurnIgnoresAnOutOfOrderSample is review round 1's item 3: a sample
// that arrives with a timestamp before the last one recorded (two pollers
// racing) must not be treated as a new gap from the true last sample, nor
// overwrite it as the reference point for the next one.
func TestBurnIgnoresAnOutOfOrderSample(t *testing.T) {
	var b Burn
	for i := 0; i <= 20; i++ { // 1 active hour at 100 units/h (5 tier x 20 pts)
		b.Observe("A", 5, float64(10+i), t0.Add(time.Duration(3*i)*time.Minute))
	}
	before := b.Rate()
	// A sample arriving a second before the true last one (two pollers
	// racing), reporting a wildly different value: it must be discarded
	// outright, not kept as the reference point for the next sample.
	b.Observe("A", 5, 999, t0.Add(60*time.Minute).Add(-time.Second))
	if got := b.Rate(); got != before {
		t.Fatalf("an out-of-order sample changed the rate %v -> %v", before, got)
	}
	// The next legitimate sample must measure its gap and delta from the
	// true last point (pct 30 at t0+60m), not from the rejected sample: if
	// it were kept as the reference, this delta would read as a huge drop
	// (31-999), clipped to zero burn but still counting active time, and
	// dilute the rate well below the steady 100.
	b.Observe("A", 5, 31, t0.Add(63*time.Minute))
	if got := b.Rate(); math.Abs(got-100) > 1e-6 {
		t.Fatalf("Rate after the out-of-order sample = %v, want ~100 (unaffected)", got)
	}
}

// TestBurnActiveGapAtExactlyTheLimitStillCounts is item 7: a gap of
// exactly BurnActiveGap is active, not idle (the check is a strict
// "longer than", not "at least").
func TestBurnActiveGapAtExactlyTheLimitStillCounts(t *testing.T) {
	var b Burn
	at := t0
	for i := 0; i < 5; i++ { // 4 gaps of exactly BurnActiveGap = 20m active
		b.Observe("A", 5, float64(10+i), at)
		at = at.Add(BurnActiveGap)
	}
	if got := b.Rate(); got == 0 {
		t.Fatal("Rate = 0, want nonzero: a gap of exactly BurnActiveGap must still count as active")
	}
}

// TestBurnResetGivesTheExactExpectedRate is item 7: the max(0, ...) clamp
// on a reset (a fall in utilization) must produce a specific, correctly
// signed value, not merely one lower than before.
func TestBurnResetGivesTheExactExpectedRate(t *testing.T) {
	var b Burn
	gap := 3 * time.Minute
	at := t0
	for i := 0; i < 21; i++ { // 20 gaps of 3m = 1h active, well past the 15m minimum
		b.Observe("A", 5, float64(90+6*i), at) // a steady ratio: 5 x 6pts / 3m = 600 units/h
		at = at.Add(gap)
	}
	if got := b.Rate(); math.Abs(got-600) > 1e-6 {
		t.Fatalf("setup rate = %v, want 600", got)
	}
	unitsBefore, hoursBefore := b.units, b.hours
	b.Observe("A", 5, 0, at) // a reset: pct falls to 0
	decay := math.Exp(-gap.Hours() / BurnDecay.Hours())
	wantUnits := unitsBefore * decay // max(0, 0-96) = 0: no burn added
	wantHours := hoursBefore*decay + gap.Hours()
	wantRate := wantUnits / wantHours
	if wantRate < 0 {
		t.Fatalf("test setup produced a negative expected rate %v", wantRate)
	}
	if got := b.Rate(); math.Abs(got-wantRate) > 1e-9 {
		t.Fatalf("Rate after reset = %v, want %v", got, wantRate)
	}
}

// TestBurnIgnoresANaNSample is re-review item 4: a NaN pct5h (or units)
// reading must be discarded outright, not stored as the reference point.
// Kept as prev.pct, a NaN would poison every future delta (and so
// b.units) with NaN forever.
func TestBurnIgnoresANaNSample(t *testing.T) {
	var b Burn
	for i := 0; i <= 20; i++ { // 1 active hour at 100 units/h, the steady baseline
		b.Observe("A", 5, float64(10+i), t0.Add(time.Duration(3*i)*time.Minute))
	}
	before := b.Rate()
	b.Observe("A", 5, math.NaN(), t0.Add(61*time.Minute)) // a corrupt pct5h
	if got := b.Rate(); got != before || math.IsNaN(got) {
		t.Fatalf("a NaN pct5h changed the rate %v -> %v", before, got)
	}
	b.Observe("A", math.NaN(), 31, t0.Add(62*time.Minute)) // a corrupt units
	if got := b.Rate(); got != before || math.IsNaN(got) {
		t.Fatalf("a NaN units changed the rate %v -> %v", before, got)
	}
	// The rate must remain finite and correctly measured from the true
	// last point (30 at t0+60m), once legitimate samples resume.
	b.Observe("A", 5, 31, t0.Add(63*time.Minute))
	if got := b.Rate(); math.IsNaN(got) || math.Abs(got-100) > 1e-6 {
		t.Fatalf("Rate after the NaN samples = %v, want ~100 (finite)", got)
	}
}

func TestBurnKeepsOneSeriesPerAccount(t *testing.T) {
	var b Burn
	// A and B interleave; each account's own series decides its burn.
	for i := 0; i <= 10; i++ {
		at := t0.Add(time.Duration(3*i) * time.Minute)
		b.Observe("A", 1, float64(i), at)
		b.Observe("b", 1, 50, at.Add(time.Second))
	}
	// A: 10 points over 10 × 3m; B: nothing. Both add active time, so the
	// rate is A's 10 units over 2 × 30 active minutes (decayed alike).
	if got := b.Rate(); got <= 0 || got >= 20 {
		t.Fatalf("Rate = %v, want between 0 and A's own 20/h", got)
	}
	b.Observe("B", 1, 60, t0.Add(31*time.Minute)) // "B" is "b": 10 points in ~1 minute
	if got := b.Rate(); got <= 10 {
		t.Fatalf("Rate = %v: account names must match case-insensitively", got)
	}
}
