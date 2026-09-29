package status_test

import (
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/usage"
)

var pollBase = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func polled(frac float64, reset time.Time) usage.Window {
	return usage.Window{Utilization: frac, HasUtilization: true, ResetsAt: reset, Known: true}
}

func row(t *testing.T, f status.File, name string) status.Account {
	t.Helper()
	for _, a := range f.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no row for %s in %+v", name, f.Accounts)
	return status.Account{}
}

func TestPollWritesUsageAsPolled(t *testing.T) {
	var f status.File
	r5, r7 := pollBase.Add(3*time.Hour), pollBase.Add(96*time.Hour)
	at := pollBase.Add(time.Second)
	if !f.Poll("A", polled(0.42, r5), polled(0.075, r7), pollBase, at) {
		t.Fatal("Poll wrote nothing")
	}
	a := row(t, f, "A")
	u := a.Usage
	if u == nil || u.Source != "polled" || !u.UpdatedAt.Equal(at) {
		t.Fatalf("usage = %+v, want source polled, updatedAt %v", u, at)
	}
	if u.FiveHourPct == nil || *u.FiveHourPct < 41.999 || *u.FiveHourPct > 42.001 || u.SevenDayPct == nil || *u.SevenDayPct < 7.499 || *u.SevenDayPct > 7.501 {
		t.Fatalf("pcts = %v / %v, want 42 / 7.5 (0-100 units)", u.FiveHourPct, u.SevenDayPct)
	}
	if !u.FiveHourResetsAt.Equal(r5) || !u.SevenDayResetsAt.Equal(r7) {
		t.Fatalf("resets = %v / %v", u.FiveHourResetsAt, u.SevenDayResetsAt)
	}
	if a.Limited || !f.Fresh("A", at) {
		t.Fatalf("limited = %v, fresh = %v; want unlimited and fresh", a.Limited, f.Fresh("A", at))
	}
}

func TestPollDropsWhenObservedDataIsFresher(t *testing.T) {
	var f status.File
	observedAt := pollBase.Add(time.Minute)
	f.Observe("A", usage.Snapshot{Known: true, At: observedAt, Overall: "allowed",
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 0.1, Status: "allowed"}}, usage.Verdict{})
	if f.Poll("A", polled(1, pollBase.Add(time.Hour)), polled(0.5, pollBase.Add(time.Hour)), pollBase, observedAt.Add(time.Second)) {
		t.Fatal("a poll sent before the newest observation was written")
	}
	a := row(t, f, "A")
	if a.Usage.Source != "observed" || a.Limited {
		t.Fatalf("row = %+v, want the observation untouched", a)
	}
	// Sent after the observation: the poll is the newer evidence.
	if !f.Poll("A", polled(0.2, time.Time{}), polled(0.3, time.Time{}), observedAt.Add(time.Second), observedAt.Add(2*time.Second)) {
		t.Fatal("a poll sent after the newest observation was dropped")
	}
	if got := row(t, f, "A").Usage.Source; got != "polled" {
		t.Fatalf("source = %q, want polled", got)
	}
}

func TestPollLimitsUntilTheLaterReset(t *testing.T) {
	var f status.File
	r5, r7 := pollBase.Add(2*time.Hour), pollBase.Add(50*time.Hour)
	f.Poll("A", polled(1, r5), polled(1.02, r7), pollBase, pollBase)
	a := row(t, f, "A")
	if !a.Limited || !a.LimitedUntil.Equal(r7) || a.Window != "seven_day" {
		t.Fatalf("row = %+v, want limited until the 7d reset %v", a, r7)
	}
	if !f.Limits.AllLimited || !f.Limits.NextReset.Equal(r7) {
		t.Fatalf("limits = %+v, want the roll-up recomputed", f.Limits)
	}

	var g status.File
	g.Poll("B", polled(1, r5), polled(0.4, r7), pollBase, pollBase)
	if b := row(t, g, "B"); !b.Limited || !b.LimitedUntil.Equal(r5) || b.Window != "five_hour" {
		t.Fatalf("row = %+v, want limited until the 5h reset %v", b, r5)
	}
}

func TestPollWithAnUnknownResetLimitsWithUnknownUntil(t *testing.T) {
	var f status.File
	f.Poll("A", polled(1, time.Time{}), polled(1, pollBase.Add(50*time.Hour)), pollBase, pollBase)
	a := row(t, f, "A")
	if !a.Limited || !a.LimitedUntil.IsZero() {
		t.Fatalf("row = %+v, want limited with an unknown (zero) clearing time", a)
	}
}

func TestPollBelowTheWallClearsALimit(t *testing.T) {
	var f status.File
	f.Poll("A", polled(1, pollBase.Add(time.Hour)), polled(0.5, pollBase.Add(time.Hour)), pollBase, pollBase)
	f.Poll("A", polled(0.01, pollBase.Add(6*time.Hour)), polled(0.5, pollBase.Add(time.Hour)), pollBase.Add(2*time.Hour), pollBase.Add(2*time.Hour))
	a := row(t, f, "A")
	if a.Limited || !a.LimitedUntil.IsZero() || a.Window != "" {
		t.Fatalf("row = %+v, want the limit cleared", a)
	}
	if f.Limits.AllLimited {
		t.Fatal("AllLimited still true after the only account cleared")
	}
}

func TestPollWithOneUnknownWindowKeepsTheLimit(t *testing.T) {
	var f status.File
	until := pollBase.Add(time.Hour)
	f.Poll("A", polled(1, until), polled(0.5, until), pollBase, pollBase)
	f.Poll("A", polled(0.2, until), usage.Window{}, pollBase.Add(time.Minute), pollBase.Add(time.Minute))
	a := row(t, f, "A")
	if !a.Limited || !a.LimitedUntil.Equal(until) {
		t.Fatalf("row = %+v, want the limit kept: an unknown window is not evidence of recovery", a)
	}
	if a.Usage.SevenDayPct != nil {
		t.Fatalf("sevenDayPct = %v, want nil (unknown), never a stale or zero value", *a.Usage.SevenDayPct)
	}
}

func TestPollThatLearnedNothingWritesNothing(t *testing.T) {
	var f status.File
	if f.Poll("A", usage.Window{}, usage.Window{ResetsAt: pollBase, Known: true}, pollBase, pollBase) {
		t.Fatal("Poll reported a write with no utilization in either window")
	}
	if len(f.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want no row created", f.Accounts)
	}
	if f.Poll("", polled(0.1, pollBase), polled(0.1, pollBase), pollBase, pollBase) {
		t.Fatal("Poll wrote a row for an empty account name")
	}
}
