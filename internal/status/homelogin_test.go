package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

func TestSetHomeLoginRefusedStampsClearsAndReportsChange(t *testing.T) {
	var f status.File
	if f.SetHomeLoginRefused(time.Time{}) || f.Daemon != nil {
		t.Fatalf("clearing on a nil daemon object changed %+v", f.Daemon)
	}
	at := time.Date(2026, 10, 25, 15, 45, 3, 0, time.UTC)
	if !f.SetHomeLoginRefused(at) || f.Daemon == nil || f.Daemon.HomeLoginRefused != "2026-10-25T15:45:03Z" {
		t.Fatalf("Daemon = %+v, want HomeLoginRefused 2026-10-25T15:45:03Z", f.Daemon)
	}
	if f.SetHomeLoginRefused(at) {
		t.Error("the same start again reported a change")
	}
	if !f.SetHomeLoginRefused(time.Time{}) || f.Daemon.HomeLoginRefused != "" {
		t.Errorf("clearing left %q", f.Daemon.HomeLoginRefused)
	}
	b, _ := json.Marshal(status.Daemon{})
	if strings.Contains(string(b), "homeLoginRefused") {
		t.Errorf("zero value is marshalled: %s", b)
	}
	f.SetHomeLoginRefused(at)
	f.ClearDaemon()
	if f.Daemon.HomeLoginRefused != "" {
		t.Errorf("ClearDaemon left %q", f.Daemon.HomeLoginRefused)
	}
}
