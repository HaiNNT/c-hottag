package usagecost

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var million = Tokens{In: 1_000_000, Out: 1_000_000, CR: 1_000_000, CW5m: 1_000_000, CW1h: 1_000_000}

func TestCostOpus55(t *testing.T) {
	q := Cost("claude-opus-5-5", million)
	if !near(q.USD, 4+20+0.20+5+8) || !q.Known || q.Estimated {
		t.Fatalf("got %+v", q)
	}
}

func TestCostFastOpus55(t *testing.T) {
	tk := million
	tk.Speed = "fast"
	q := Cost("claude-opus-5-5", tk)
	if !near(q.USD, 74.40) || !q.Known || q.Estimated {
		t.Fatalf("got %+v", q)
	}
}

func TestCostFastWithoutFastModeUsesStd(t *testing.T) {
	tk := million
	tk.Speed = "fast"
	if q := Cost("claude-sonnet-5-5", tk); !near(q.USD, 2+10+0.10+2.5+4) {
		t.Fatalf("got %+v", q)
	}
}

func TestCostHaiku55LongPrompt(t *testing.T) {
	q := Cost("claude-haiku-5-5", Tokens{In: 150_000, Out: 1_000_000})
	if !near(q.USD, 0.075+2.5) {
		t.Fatalf("got %+v", q)
	}
	q = Cost("claude-haiku-5-5", Tokens{In: 100_000, Out: 1_000_000})
	if !near(q.USD, 0.01+0.5) {
		t.Fatalf("at the threshold: got %+v", q)
	}
}

func TestCostDateSuffix(t *testing.T) {
	q := Cost("claude-sonnet-5-5-20261201", Tokens{In: 1})
	if q.Priced != "claude-sonnet-5-5" || q.Estimated || !q.Known {
		t.Fatalf("got %+v", q)
	}
}

func TestCostFamilyFallback(t *testing.T) {
	q := Cost("claude-opus-6", Tokens{In: 1_000_000})
	if !q.Known || !q.Estimated || q.Priced != "claude-opus-5-5" || !near(q.USD, 4) {
		t.Fatalf("got %+v", q)
	}
}

func TestCostUnknown(t *testing.T) {
	for _, m := range []string{"gpt-x", "<synthetic>", ""} {
		q := Cost(m, Tokens{In: 1_000_000})
		if q.Known || q.USD != 0 {
			t.Fatalf("%q: got %+v", m, q)
		}
	}
}

func TestWebSearch(t *testing.T) {
	q := Cost("claude-opus-5-5", Tokens{Web: 3})
	if !near(q.USD, 0.03) {
		t.Fatalf("got %+v", q)
	}
}

func TestPlanPrice(t *testing.T) {
	for tier, want := range map[string]float64{"pro": 20, "max5x": 100, "max20x": 200, "ultra": 200} {
		if got, ok := PlanPrice(tier); !ok || got != want {
			t.Fatalf("%s: %v %v", tier, got, ok)
		}
	}
	for _, tier := range []string{"team", "max", ""} {
		if _, ok := PlanPrice(tier); ok {
			t.Fatalf("%q should have no price", tier)
		}
	}
}

func TestCostSumNeverUsesTheLongTier(t *testing.T) {
	tk := Tokens{In: 300_000}
	if got := Cost("claude-haiku-5-5", tk).USD; !near(got, 0.5*0.3) {
		t.Fatalf("per-message long tier: %v", got)
	}
	if got := CostSum("claude-haiku-5-5", tk).USD; !near(got, 0.1*0.3) {
		t.Fatalf("sum priced with the tier: %v", got)
	}
}
