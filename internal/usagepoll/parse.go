package usagepoll

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usage"
)

// UtilizationScale converts the endpoint's `utilization` into the 0-1
// fraction usage.Window carries: fraction = utilization / UtilizationScale.
// F156 captured the field's type (number) but not its scale.
//
// Pinned by a live check against real traffic: TestUtilizationScaleIsPinned
// holds the evidence. parse still refuses a non-positive scale, so a zero
// here can never write a guessed value.
const UtilizationScale = 100.0

var (
	errUnpinned = errors.New("utilization scale is not pinned")
	errNoWindow = errors.New("no usable five_hour or seven_day window")
)

// maxBody bounds how much of a response is read. The captured body is a
// few KB; anything past this is not the document F156 describes.
const maxBody = 1 << 20

// minPlausibleReset rejects a reset time that cannot be real: a past reset
// reads as "already cleared", which is worse than unknown.
var minPlausibleReset = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// parse reads five_hour and seven_day from a /api/oauth/usage body. Every
// other key is skipped by the decoder and never stored. A body that is not
// a JSON object, or that has neither window's utilization, is an error: a
// failed poll, never zero usage.
func parse(body []byte, scale float64) (Result, error) {
	if !(scale > 0) {
		return Result{}, errUnpinned
	}
	var doc struct {
		FiveHour json.RawMessage `json:"five_hour"`
		SevenDay json.RawMessage `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Result{}, err
	}
	r := Result{FiveHour: parseWindow(doc.FiveHour, scale), SevenDay: parseWindow(doc.SevenDay, scale)}
	if !r.FiveHour.HasUtilization && !r.SevenDay.HasUtilization {
		return Result{}, errNoWindow
	}
	return r, nil
}

// parseWindow reads one window. A missing, null or non-object window, and
// a missing, null, non-numeric or negative utilization, are unknown for
// that window (HasUtilization false), never 0%. resets_at is RFC 3339; an
// unparsable or implausible one is unknown (zero).
func parseWindow(raw json.RawMessage, scale float64) usage.Window {
	var w struct {
		Utilization json.RawMessage `json:"utilization"`
		ResetsAt    json.RawMessage `json:"resets_at"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return usage.Window{}
	}
	var out usage.Window
	var u *float64
	if json.Unmarshal(w.Utilization, &u) == nil && u != nil && !math.IsNaN(*u) && !math.IsInf(*u, 0) && *u >= 0 {
		out.Utilization, out.HasUtilization = *u/scale, true
	}
	var s string
	if json.Unmarshal(w.ResetsAt, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && !t.Before(minPlausibleReset) {
			out.ResetsAt = t.UTC()
		}
	}
	out.Known = out.HasUtilization || !out.ResetsAt.IsZero()
	return out
}
