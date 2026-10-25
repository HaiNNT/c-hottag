package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/status"
)

// R183: while the running daemon has an open episode of Home's login being
// refused, `status` says so, and --json carries daemon.homeLoginRefused. A
// daemon that is not running makes no such statement.
func TestStatusSaysHomeLoginWasRefused(t *testing.T) {
	since := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	for _, running := range []bool{true, false} {
		home := t.TempDir()
		seedState(t, home, "D", "A")
		var f status.File
		f.SetDaemon(47821, 0, 0, 0, time.Now())
		f.SetHomeLoginRefused(since)
		if !running {
			f.ClearDaemon()
			f.Daemon.HomeLoginRefused = since.UTC().Format(time.RFC3339)
		}
		saveStatus(t, home, f)
		defer cli.SetStatusProbeForTest(func(int) (bool, string) { return true, cli.Version })()

		_, out, _ := runHome(t, home, "status")
		want := "Home's login was refused since " + since.Local().Format("15:04") + ": run /login in a Claude Code session"
		if got := strings.Contains(out, want); got != running {
			t.Fatalf("running=%v: output contains %q = %v:\n%s", running, want, got, out)
		}
		_, jsonOut, _ := runHome(t, home, "status", "--json")
		var got struct {
			Daemon struct {
				HomeLoginRefused string `json:"homeLoginRefused"`
			} `json:"daemon"`
		}
		if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
			t.Fatal(err)
		}
		if wantJSON := since.UTC().Format(time.RFC3339); running && got.Daemon.HomeLoginRefused != wantJSON {
			t.Fatalf("daemon.homeLoginRefused = %q, want %q", got.Daemon.HomeLoginRefused, wantJSON)
		} else if !running && got.Daemon.HomeLoginRefused != "" {
			t.Fatalf("a stopped daemon's episode is shown: %q", got.Daemon.HomeLoginRefused)
		}
	}
}
