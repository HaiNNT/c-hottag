package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

// R183: the home-login row is a problem only while the running daemon has an
// episode of Home's login being refused that is at least 30 s old.
func TestHomeLoginCheck(t *testing.T) {
	stamp := func(ago time.Duration) string { return testNow.Add(-ago).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name   string
		daemon *status.Daemon
		status string
		detail string
	}{
		{"no daemon", nil, StatusInfo, "no daemon has reported yet"},
		{"no episode", &status.Daemon{Heartbeat: testNow.Add(-10 * time.Second)}, StatusOK, "was not refused"},
		{"old episode", &status.Daemon{Heartbeat: testNow.Add(-10 * time.Second), HomeLoginRefused: stamp(5 * time.Minute)}, StatusProblem, "Login expired"},
		{"fresh episode", &status.Daemon{Heartbeat: testNow.Add(-10 * time.Second), HomeLoginRefused: stamp(10 * time.Second)}, StatusOK, "was not refused"},
		{"daemon stopped", &status.Daemon{HomeLoginRefused: stamp(5 * time.Minute)}, StatusInfo, "not running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			writeTestStatus(t, ti.home, status.File{Daemon: tc.daemon})
			r := rowByID(t, mustRun(t, ti.env, []Check{homeLoginCheck()}, true), "home-login")
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("home-login = %+v, want %s containing %q", r, tc.status, tc.detail)
			}
			if tc.status == StatusProblem && r.Hint != "run /login in a Claude Code session" {
				t.Fatalf("hint = %q", r.Hint)
			}
		})
	}
}
