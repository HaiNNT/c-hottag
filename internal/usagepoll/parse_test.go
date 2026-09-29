package usagepoll

import (
	"errors"
	"math"
	"testing"
	"time"
)

// capturedShape is F156's captured key set with made-up values: every key
// the live capture recorded, so the test proves parse reads only the two
// windows and ignores the rest (extra_usage.utilization and limits[].percent
// included, which a careless decoder could mistake for the windows).
const capturedShape = `{
  "five_hour": {"utilization": 42, "resets_at": "2026-09-24T15:00:00.123456+00:00",
    "used_dollars": 1.5, "remaining_dollars": 2.5, "limit_dollars": 4, "locked_reason": null},
  "seven_day": {"utilization": 7.5, "resets_at": "2026-09-30T08:00:00Z",
    "used_dollars": 1, "remaining_dollars": 3, "limit_dollars": 4, "locked_reason": null},
  "seven_day_opus": null, "seven_day_sonnet": null, "seven_day_oauth_apps": null,
  "seven_day_cowork": null, "seven_day_omelette": null,
  "limits": [{"group": "g", "kind": "k", "percent": 99, "resets_at": "2026-09-24T15:00:00Z",
    "severity": "high", "is_active": true, "scope": "s"}],
  "seven_day_breakdown": {"as_of": "x", "window_started_at": "y",
    "rows": [{"key": "k", "display_name": "d", "percent": 50}]},
  "extra_usage": {"utilization": 100, "monthly_limit": 10, "spend_limit_reached": true},
  "spend": {}
}`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseReadsTheCapturedShape(t *testing.T) {
	r, err := parse([]byte(capturedShape), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !r.FiveHour.HasUtilization || !near(r.FiveHour.Utilization, 0.42) {
		t.Fatalf("five_hour = %+v, want utilization 0.42 (42 / scale 100)", r.FiveHour)
	}
	if want := time.Date(2026, 9, 24, 15, 0, 0, 123456000, time.UTC); !r.FiveHour.ResetsAt.Equal(want) {
		t.Fatalf("five_hour.ResetsAt = %v, want %v", r.FiveHour.ResetsAt, want)
	}
	if !r.SevenDay.HasUtilization || !near(r.SevenDay.Utilization, 0.075) {
		t.Fatalf("seven_day = %+v, want utilization 0.075", r.SevenDay)
	}
	if want := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC); !r.SevenDay.ResetsAt.Equal(want) {
		t.Fatalf("seven_day.ResetsAt = %v, want %v", r.SevenDay.ResetsAt, want)
	}
	if !r.FiveHour.Known || !r.SevenDay.Known {
		t.Fatalf("windows not Known: %+v", r)
	}
	// Scale 1 reads the same body as fractions already.
	r1, err := parse([]byte(capturedShape), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !near(r1.FiveHour.Utilization, 42) {
		t.Fatalf("scale 1: five_hour utilization = %v, want 42", r1.FiveHour.Utilization)
	}
}

// TestParseRefusesAnUnpinnedScale: until the live check pins
// UtilizationScale, every poll fails closed and writes nothing.
func TestParseRefusesAnUnpinnedScale(t *testing.T) {
	for _, scale := range []float64{0, -1, math.NaN()} {
		if _, err := parse([]byte(capturedShape), scale); !errors.Is(err, errUnpinned) {
			t.Errorf("parse(scale %v) err = %v, want errUnpinned", scale, err)
		}
	}
}

func TestParseTreatsABadWindowAsUnknown(t *testing.T) {
	good := `{"utilization": 10, "resets_at": "2026-09-30T08:00:00Z"}`
	for name, bad := range map[string]string{
		"null window":           `null`,
		"string utilization":    `{"utilization": "10", "resets_at": "2026-09-30T08:00:00Z"}`,
		"null utilization":      `{"utilization": null, "resets_at": "2026-09-30T08:00:00Z"}`,
		"missing utilization":   `{"resets_at": "2026-09-30T08:00:00Z"}`,
		"negative utilization":  `{"utilization": -5, "resets_at": "2026-09-30T08:00:00Z"}`,
		"window is not object":  `[10]`,
		"window is a bare word": `true`,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := parse([]byte(`{"five_hour": `+bad+`, "seven_day": `+good+`}`), 100)
			if err != nil {
				t.Fatalf("one bad window failed the whole poll: %v", err)
			}
			if r.FiveHour.HasUtilization {
				t.Fatalf("five_hour = %+v, want unknown utilization", r.FiveHour)
			}
			if !r.SevenDay.HasUtilization || !near(r.SevenDay.Utilization, 0.1) {
				t.Fatalf("seven_day = %+v, want 0.1", r.SevenDay)
			}
		})
	}
	r, err := parse([]byte(`{"seven_day": `+good+`}`), 100)
	if err != nil || r.FiveHour.Known || r.FiveHour.HasUtilization {
		t.Fatalf("missing five_hour: r = %+v, err = %v; want five_hour unknown and no error", r, err)
	}
}

func TestParseFailsABodyWithNoUsableWindow(t *testing.T) {
	for name, body := range map[string]string{
		"empty":           ``,
		"not json":        `<html>502</html>`,
		"truncated":       capturedShape[:40],
		"array":           `[]`,
		"null":            `null`,
		"no windows":      `{"limits": []}`,
		"both unknown":    `{"five_hour": null, "seven_day": {"utilization": "x"}}`,
		"only extra keys": `{"extra_usage": {"utilization": 100}}`,
	} {
		if _, err := parse([]byte(body), 100); err == nil {
			t.Errorf("%s: parse succeeded, want a failed poll", name)
		}
	}
}

func TestParseRejectsAnImplausibleReset(t *testing.T) {
	for name, reset := range map[string]string{
		"not a time": `"soon"`,
		"a number":   `1790000000`,
		"epoch":      `"1970-01-01T00:00:00Z"`,
		"null":       `null`,
	} {
		r, err := parse([]byte(`{"five_hour": {"utilization": 1, "resets_at": `+reset+`}}`), 100)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !r.FiveHour.ResetsAt.IsZero() {
			t.Errorf("%s: ResetsAt = %v, want zero (unknown)", name, r.FiveHour.ResetsAt)
		}
	}
}
