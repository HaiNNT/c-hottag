package usagepoll

import (
	"math"
	"testing"
)

// The live check's evidence: account B's polled five_hour.utilization
// exactly as sent (daemon.log's raw5h, 11:35:14), and the
// Anthropic-Ratelimit-Unified-5h-Utilization fraction observed for B at
// 11:40:53 (status.json fiveHourPct / 100 with source "observed"). The 5.5
// minutes between them, and one small prompt, explain the 0.01 gap.
const (
	liveRaw5h          = 19.0
	liveObserved5hFrac = 0.20
)

// TestUtilizationScaleIsPinned keeps a guessed scale from shipping: it
// fails until UtilizationScale is set, and then fails if the constant
// disagrees with the recorded live evidence.
func TestUtilizationScaleIsPinned(t *testing.T) {
	scale := float64(UtilizationScale)
	if !(scale > 0) {
		t.Fatal("utilization scale is unpinned: run the M1c6b live check (plan Task 6) and set UtilizationScale in parse.go from its evidence; until then every poll fails closed and writes nothing")
	}
	if got := liveRaw5h / scale; math.Abs(got-liveObserved5hFrac) > 0.02 {
		t.Fatalf("live evidence: raw %v / scale %v = %v, but the observed header fraction was %v", liveRaw5h, scale, got, liveObserved5hFrac)
	}
}
