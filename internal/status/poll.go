package status

import (
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usage"
)

// Poll folds one /api/oauth/usage result into the cache (spec §6.4) and
// reports whether it wrote anything. fiveHour and sevenDay are in
// usage.Window's units: Utilization is a 0-1 fraction, 1.0 exhausted.
//
//   - Fresher observed data wins: when the account's cached usage was
//     updated after sent (the moment the poll went out), nothing changes.
//   - A poll that learned neither window's utilization writes nothing: a
//     failed poll is never zero usage (the same rule Observe follows).
//   - Both windows are written, so a window the poll could not read becomes
//     unknown (nil), never 0%.
//   - A window at or over 100% limits the account until that window's
//     reset (the later one when both are); an unknown reset leaves the
//     clearing time unknown (zero). Both windows known and under 100%
//     clears a limit. One window unknown and the other under 100% leaves
//     the limit as it was.
//
// Unlike Observe, a poll creates the account's row when it is missing: a
// newly registered account has no row until the next EnsureAccounts, and
// the poll is exactly what fills it.
func (f *File) Poll(account string, fiveHour, sevenDay usage.Window, sent, at time.Time) bool {
	if account == "" || (!fiveHour.HasUtilization && !sevenDay.HasUtilization) {
		return false
	}
	for i := range f.Accounts {
		if strings.EqualFold(f.Accounts[i].Name, account) {
			if u := f.Accounts[i].Usage; u != nil && u.UpdatedAt.After(sent) {
				return false
			}
			break
		}
	}
	a := f.account(account)
	if a.Usage == nil {
		a.Usage = &Usage{}
	}
	a.Usage.UpdatedAt, a.Usage.Source = at, "polled"
	a.Usage.FiveHourPct, a.Usage.FiveHourResetsAt = pollPct(fiveHour), fiveHour.ResetsAt
	a.Usage.SevenDayPct, a.Usage.SevenDayResetsAt = pollPct(sevenDay), sevenDay.ResetsAt
	if limited, until, window := pollLimit(fiveHour, sevenDay); limited {
		a.Limited, a.LimitedUntil, a.Window = true, until, window
	} else if fiveHour.HasUtilization && sevenDay.HasUtilization {
		a.Limited, a.LimitedUntil, a.Window = false, time.Time{}, ""
	}
	f.rollUp()
	return true
}

// pollPct converts a window to the cache's 0-100 wire units, nil when the
// poll could not read it.
func pollPct(w usage.Window) *float64 {
	if !w.HasUtilization {
		return nil
	}
	p := w.Utilization * 100
	return &p
}

// pollLimit applies §6.2's poll rule: limited while any window is at or
// over 100%, until the latest such window's reset. Any limiting window
// with an unknown reset makes the clearing time unknown (zero), because
// the account may stay limited past every known reset.
func pollLimit(fiveHour, sevenDay usage.Window) (bool, time.Time, string) {
	limited, unknown := false, false
	var until time.Time
	var window string
	for _, w := range []struct {
		name string
		w    usage.Window
	}{{"five_hour", fiveHour}, {"seven_day", sevenDay}} {
		if !w.w.HasUtilization || w.w.Utilization < 1 {
			continue
		}
		limited = true
		if w.w.ResetsAt.IsZero() {
			unknown = true
			if window == "" {
				window = w.name
			}
			continue
		}
		if until.IsZero() || w.w.ResetsAt.After(until) {
			until, window = w.w.ResetsAt, w.name
		}
	}
	if unknown {
		until = time.Time{}
	}
	return limited, until, window
}
