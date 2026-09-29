package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/status"
)

// runningDaemon writes a status cache with a daemon whose heartbeat is
// fresh enough for status.File.DaemonRunningAt to read it as running (spec
// §5.1), so runStatus's daemon-version overlay actually probes.
func runningDaemon(t *testing.T, home string) {
	t.Helper()
	var f status.File
	f.SetDaemon(47821, 0, 0, 0, time.Now())
	saveStatus(t, home, f)
}

// TestStatusShowsTheDaemonVersionMismatch pins the main case (public
// release design §2.4): a running daemon whose health version differs from
// this binary's own shows the warning line in text, and daemon.version /
// daemon.versionMismatch in --json.
func TestStatusShowsTheDaemonVersionMismatch(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	runningDaemon(t, home)
	restoreProbe := cli.SetStatusProbeForTest(func(int) (bool, string) { return true, "0.3.0" })
	defer restoreProbe()
	origVersion := cli.Version
	cli.Version = "0.3.1"
	defer func() { cli.Version = origVersion }()

	_, out, _ := runHome(t, home, "status")
	want := "daemon: running 0.3.0, installed 0.3.1 (run: chottag daemon restart)"
	if !strings.Contains(out, want) {
		t.Fatalf("output = %q, want it to contain %q", out, want)
	}

	_, jsonOut, _ := runHome(t, home, "status", "--json")
	var got struct {
		Daemon struct {
			Version         string `json:"version"`
			VersionMismatch bool   `json:"versionMismatch"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut)
	}
	if got.Daemon.Version != "0.3.0" || !got.Daemon.VersionMismatch {
		t.Fatalf("daemon = %+v, want version 0.3.0 and versionMismatch true", got.Daemon)
	}
}

// TestStatusShowsNoLineWhenVersionsMatch pins the second case: matching
// versions print nothing, and --json omits versionMismatch entirely
// (omitempty), not merely false.
func TestStatusShowsNoLineWhenVersionsMatch(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	runningDaemon(t, home)
	restoreProbe := cli.SetStatusProbeForTest(func(int) (bool, string) { return true, "0.3.1" })
	defer restoreProbe()
	origVersion := cli.Version
	cli.Version = "0.3.1"
	defer func() { cli.Version = origVersion }()

	_, out, _ := runHome(t, home, "status")
	if strings.Contains(out, "daemon: running") {
		t.Fatalf("output = %q, want no daemon-version line when versions match", out)
	}

	_, jsonOut, _ := runHome(t, home, "status", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut)
	}
	daemon, _ := got["daemon"].(map[string]any)
	if _, ok := daemon["versionMismatch"]; ok {
		t.Fatalf("daemon = %+v, want versionMismatch omitted (omitempty) when versions match", daemon)
	}
}

// TestStatusShowsNoLineWithNoDaemon pins the third case: with no daemon at
// all (no cache Daemon object, so DaemonRunningAt leaves it nil), the probe
// is never even reached, and no line prints.
func TestStatusShowsNoLineWithNoDaemon(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	restoreProbe := cli.SetStatusProbeForTest(func(int) (bool, string) {
		t.Fatal("statusProbe reached with no daemon in the cache")
		return false, ""
	})
	defer restoreProbe()

	_, out, _ := runHome(t, home, "status")
	if strings.Contains(out, "daemon: running") {
		t.Fatalf("output = %q, want no daemon-version line with no daemon", out)
	}
}

// staleDaemon writes a status cache with a daemon whose heartbeat is well
// past status.DaemonStaleAfter, so status.File.DaemonRunningAt reads it as
// NOT running — the same "killed daemon, cache not yet cleared" case
// TestStatusJSONRecomputesDaemonRunningFromHeartbeat already pins for the
// Running field itself.
func staleDaemon(t *testing.T, home string) {
	t.Helper()
	f := status.File{Daemon: &status.Daemon{
		Running:   true, // deliberately the opposite of what a stale heartbeat implies (see the cited test's own reasoning)
		Heartbeat: time.Now().Add(-2 * status.DaemonStaleAfter),
		Port:      47821,
	}}
	saveStatus(t, home, f)
}

// TestStatusShowsNoLineWithAStaleHeartbeat pins runStatus's Running guard: a
// Daemon object whose heartbeat has gone stale must never be probed for a
// version at all, even though the cache still has a Port to probe. The
// probe stub here reports a real mismatch, so this fails loudly if the
// `f.Daemon.Running` guard around the probe call is ever dropped (checked
// by temporarily removing it: the line then appears).
func TestStatusShowsNoLineWithAStaleHeartbeat(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	staleDaemon(t, home)
	restoreProbe := cli.SetStatusProbeForTest(func(int) (bool, string) { return true, "0.3.0" })
	defer restoreProbe()
	origVersion := cli.Version
	cli.Version = "0.3.1"
	defer func() { cli.Version = origVersion }()

	_, out, _ := runHome(t, home, "status")
	if strings.Contains(out, "daemon: running") {
		t.Fatalf("output = %q, want no daemon-version line while the heartbeat is stale (Running false)", out)
	}

	_, jsonOut, _ := runHome(t, home, "status", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut)
	}
	daemon, _ := got["daemon"].(map[string]any)
	if _, ok := daemon["versionMismatch"]; ok {
		t.Fatalf("daemon = %+v, want versionMismatch omitted while the heartbeat is stale", daemon)
	}
}

// TestStatusShowsNoLineWhenTheProbeReportsAnEmptyVersion pins runStatus's
// `version != ""` guard: a daemon that answers but reports no usable
// version must not be treated as a mismatch (which would otherwise print
// the malformed "daemon: running , installed <v> ..."). Fails loudly if
// that guard is ever dropped (checked by temporarily removing it: the line
// then appears, built from the empty string).
func TestStatusShowsNoLineWhenTheProbeReportsAnEmptyVersion(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	runningDaemon(t, home)
	restoreProbe := cli.SetStatusProbeForTest(func(int) (bool, string) { return true, "" })
	defer restoreProbe()
	origVersion := cli.Version
	cli.Version = "0.3.1"
	defer func() { cli.Version = origVersion }()

	_, out, _ := runHome(t, home, "status")
	if strings.Contains(out, "daemon: running") {
		t.Fatalf("output = %q, want no daemon-version line when the probe reports an empty version", out)
	}

	_, jsonOut, _ := runHome(t, home, "status", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut)
	}
	daemon, _ := got["daemon"].(map[string]any)
	if _, ok := daemon["versionMismatch"]; ok {
		t.Fatalf("daemon = %+v, want versionMismatch omitted when the probe reports an empty version", daemon)
	}
	if v, ok := daemon["version"]; ok {
		t.Fatalf("daemon = %+v, want version omitted too (empty, and omitempty) when the probe reports none: got %v", daemon, v)
	}
}

// TestStatusShowsMismatchAgainstADevBuild pins the fourth case (spec §2.4):
// a "dev" build against a released daemon version is still a mismatch, and
// must still show the line.
func TestStatusShowsMismatchAgainstADevBuild(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	runningDaemon(t, home)
	restoreProbe := cli.SetStatusProbeForTest(func(int) (bool, string) { return true, "0.3.0" })
	defer restoreProbe()
	origVersion := cli.Version
	cli.Version = "dev"
	defer func() { cli.Version = origVersion }()

	_, out, _ := runHome(t, home, "status")
	want := "daemon: running 0.3.0, installed dev (run: chottag daemon restart)"
	if !strings.Contains(out, want) {
		t.Fatalf("output = %q, want it to contain %q", out, want)
	}
}
