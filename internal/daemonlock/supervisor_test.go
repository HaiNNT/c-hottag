package daemonlock

import (
	"os"
	"testing"
	"time"
)

// F137: a daemon records a supervisor only when a supervisor really started
// it. Every macOS Terminal shell has XPC_SERVICE_NAME=0, and a shell inside
// a terminal that is itself a systemd unit inherits INVOCATION_ID. The shim
// passes its environment to the daemon it spawns, so the naive rule ("set
// means supervised") marks every daemon supervised, and `daemon stop`
// always exits 3.
func TestDetectSupervisor(t *testing.T) {
	const pid = 4242
	cases := []struct {
		name      string
		env       map[string]string
		ppid      int
		wantSup   string
		wantLabel string
	}{
		{"plain_shell", nil, 777, "", ""},
		{"systemd_service", map[string]string{"INVOCATION_ID": "abc", "SYSTEMD_EXEC_PID": "4242"}, 900, "systemd", ""},
		{"systemd_before_v248", map[string]string{"INVOCATION_ID": "abc"}, 900, "systemd", ""},
		{"inherited_INVOCATION_ID", map[string]string{"INVOCATION_ID": "abc", "SYSTEMD_EXEC_PID": "999"}, 900, "", ""},
		{"launchd_job", map[string]string{"XPC_SERVICE_NAME": "com.chottag.daemon"}, 1, "launchd", "com.chottag.daemon"},
		{"terminal_XPC_SERVICE_NAME_0", map[string]string{"XPC_SERVICE_NAME": "0"}, 1, "", ""},
		{"hand_run_in_an_app_terminal", map[string]string{"XPC_SERVICE_NAME": "application.com.example.term.1.2"}, 777, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			getenv := func(k string) string { return c.env[k] }
			sup, label := detectSupervisor(getenv, pid, c.ppid)
			if sup != c.wantSup || label != c.wantLabel {
				t.Errorf("detectSupervisor = (%q, %q), want (%q, %q)", sup, label, c.wantSup, c.wantLabel)
			}
		})
	}
}

func TestCurrentDescribesThisProcess(t *testing.T) {
	rec := Current()
	if rec.PID != os.Getpid() {
		t.Errorf("Current().PID = %d, want %d", rec.PID, os.Getpid())
	}
	if d := time.Since(rec.Started); d < 0 || d > time.Minute {
		t.Errorf("Current().Started = %v, want about now", rec.Started)
	}
}
