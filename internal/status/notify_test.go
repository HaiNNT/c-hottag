package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

var notifyNow = time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)

func TestLimitsAtLeavesTheStoredRollUpAlone(t *testing.T) {
	stored := status.Limits{AllLimited: true, NextReset: notifyNow.Add(-time.Hour), NextResetAccount: "A"}
	f := status.File{
		Accounts: []status.Account{{Name: "A", Limited: true, LimitedUntil: notifyNow.Add(-time.Hour)}},
		Limits:   stored,
	}
	if got := f.LimitsAt(notifyNow); got.AllLimited {
		t.Errorf("LimitsAt = %+v, want not all limited: A's reset has passed", got)
	}
	if f.Limits != stored {
		t.Errorf("f.Limits = %+v after LimitsAt, want the stored %+v untouched", f.Limits, stored)
	}
}

func TestLimitsAtMatchesRollUpAt(t *testing.T) {
	f := status.File{Accounts: []status.Account{
		{Name: "A", Limited: true, LimitedUntil: notifyNow.Add(2 * time.Hour)},
		{Name: "B", Limited: true, LimitedUntil: notifyNow.Add(time.Hour)},
	}}
	got := f.LimitsAt(notifyNow)
	if !got.AllLimited || !got.NextReset.Equal(notifyNow.Add(time.Hour)) || got.NextResetAccount != "B" {
		t.Fatalf("LimitsAt = %+v, want all limited, next reset B in 1h", got)
	}
	g := f
	g.RollUpAt(notifyNow)
	if g.Limits != got {
		t.Fatalf("LimitsAt = %+v, RollUpAt = %+v: they must agree", got, g.Limits)
	}
}

func TestAvailableAtNamesTheFirstAccountNotConfirmedLimited(t *testing.T) {
	f := status.File{Accounts: []status.Account{
		{Name: "A", Limited: true}, // zero LimitedUntil: unknown, still limited
		{Name: "B", Limited: true, LimitedUntil: notifyNow.Add(-time.Minute)},
		{Name: "C"},
	}}
	if got := f.AvailableAt(notifyNow); got != "B" {
		t.Errorf("AvailableAt = %q, want B: its reset has passed", got)
	}
	f.Accounts[1].LimitedUntil = notifyNow.Add(time.Minute)
	if got := f.AvailableAt(notifyNow); got != "C" {
		t.Errorf("AvailableAt = %q, want C", got)
	}
	f.Accounts[2].Limited, f.Accounts[2].LimitedUntil = true, notifyNow.Add(time.Hour)
	if got := f.AvailableAt(notifyNow); got != "" {
		t.Errorf("AvailableAt = %q, want \"\" with every account limited", got)
	}
	if got := (status.File{}).AvailableAt(notifyNow); got != "" {
		t.Errorf("AvailableAt on no accounts = %q, want \"\"", got)
	}
}

func TestSetNotifyErrorsStampsTheDaemonObject(t *testing.T) {
	var f status.File
	f.SetDaemon(47821, 1, 2, 3, notifyNow)
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"notifyErrors": 0`) {
		t.Errorf("daemon object = %s, want notifyErrors present at 0, like routeDrift", b)
	}
	f.SetNotifyErrors(4)
	if f.Daemon.NotifyErrors != 4 || f.Daemon.RouteDrift != 1 || f.Daemon.ZeroIDExtractions != 2 || f.Daemon.OwnerWriteDrops != 3 {
		t.Errorf("daemon = %+v, want notifyErrors 4 and the other counters untouched", *f.Daemon)
	}
	b, err = status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back status.File
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Daemon == nil || back.Daemon.NotifyErrors != 4 {
		t.Errorf("round trip = %+v, want notifyErrors 4", back.Daemon)
	}
	var g status.File
	g.SetNotifyErrors(1)
	if g.Daemon == nil || g.Daemon.NotifyErrors != 1 {
		t.Errorf("SetNotifyErrors on a nil daemon object = %+v, want one created", g.Daemon)
	}
}
