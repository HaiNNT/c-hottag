package cli

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
)

// F130: SetSignalForTest must restore whatever was installed before, never
// production's syscall.Kill by name. Otherwise the first stubbed test's
// cleanup disarms TestMain's panicking default for the rest of the binary,
// and a later test that forgets to stub would send real signals to pids
// read from a lock record.
func TestSignalSeamStillPanicsAfterAStubbedTestRestores(t *testing.T) {
	t.Run("stubbed", func(t *testing.T) {
		restore := SetSignalForTest(func(int, syscall.Signal) error { return nil })
		t.Cleanup(restore)
		if err := signalFn(math.MaxInt32, 0); err != nil {
			t.Fatal(err)
		}
	})

	defer func() {
		if recover() == nil {
			t.Error("signalFn did not panic after a stubbed subtest's cleanup ran: the restore put back the real syscall.Kill instead of TestMain's panicking default")
		}
	}()
	// Signal 0 to a pid above every platform's pid_max. Even if the mutation
	// this test exists to catch is live, the real kill(2) delivers nothing
	// and fails with ESRCH.
	signalFn(math.MaxInt32, 0)
}

func TestSupervisorCommand(t *testing.T) {
	launchd := daemonlock.Record{PID: 4242, Supervisor: "launchd", Label: "com.chottag.daemon"}
	systemd := daemonlock.Record{PID: 4242, Supervisor: "systemd"}
	cases := []struct {
		name string
		rec  daemonlock.Record
		verb string
		want string
	}{
		{"launchd_stop", launchd, "stop", "launchctl bootout gui/501/com.chottag.daemon"},
		{"launchd_restart", launchd, "restart", "launchctl kickstart -k gui/501/com.chottag.daemon"},
		{"systemd_stop", systemd, "stop", "systemctl --user stop chottag.service"},
		{"systemd_restart", systemd, "restart", "systemctl --user restart chottag.service"},
		{"unsupervised", daemonlock.Record{PID: 4242}, "stop", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := supervisorCommand(c.rec, c.verb, 501); got != c.want {
				t.Errorf("supervisorCommand = %q, want %q", got, c.want)
			}
		})
	}
}

// foreignDaemonMessage (F138) has no way to identify the process that
// answers the health probe (spec §4.8 rules out trusting the health
// document's pid), so the best it can do is hint how an operator finds it
// themselves: lsof against the port. It must only ever print that hint,
// never run it or signal anything on its own.
func TestForeignDaemonMessageHintsHowToFindTheProcess(t *testing.T) {
	got := foreignDaemonMessage(filepath.Join(t.TempDir(), "run"), 47821)
	want := "lsof -nP -iTCP:47821 -sTCP:LISTEN"
	if !strings.Contains(got, want) {
		t.Errorf("foreignDaemonMessage = %q, want it to contain %q", got, want)
	}
}

// The command stop/restart print must name the unit packaging/ ships.
func TestSystemdUnitIsThePackagedUnit(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", "packaging", "systemd", systemdUnit)); err != nil {
		t.Fatalf("systemdUnit %q is not a file in packaging/systemd: %v", systemdUnit, err)
	}
}
