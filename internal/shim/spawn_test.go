package shim

import (
	"slices"
	"strings"
	"testing"
)

// daemonSpawnArgs and daemonSpawnEnv are the parts of spawnDaemon that
// genuinely can be asserted without forking a process; spawnDaemon itself
// is deliberately unreachable by any test (TestMain's panicking spawnFn
// default, fix round 1 D5), so these direct tests are what stands between
// a wrong flag name, a wrong env key, or a credential back in argv, and
// shipping green (fix round 2 D2; fix round 3 D2).

const testUpstream = "http://bob:hunter2@corp:8080"

// TestDaemonSpawnArgsNeverCarriesTheUpstream pins fix round 3's D2: the
// spawned daemon's argv must never contain the upstream proxy, credentialed
// or not — argv is visible to `ps`/`/proc/<pid>/cmdline`, which is exactly
// the exposure the fix moved this value out of (whole-branch review D2).
func TestDaemonSpawnArgsNeverCarriesTheUpstream(t *testing.T) {
	got := daemonSpawnArgs()
	want := []string{"daemon", "run"}
	if !slices.Equal(got, want) {
		t.Errorf("daemonSpawnArgs() = %v, want %v", got, want)
	}
	for _, arg := range got {
		if strings.Contains(arg, "hunter2") || strings.Contains(arg, "upstream") {
			t.Errorf("daemonSpawnArgs() = %v: an upstream-shaped value reached argv", got)
		}
	}
}

func TestDaemonSpawnEnvSetsChottagHome(t *testing.T) {
	env := daemonSpawnEnv("/tmp/some-chottag-home", "")
	want := "CHOTTAG_HOME=/tmp/some-chottag-home"
	if !slices.Contains(env, want) {
		t.Errorf("daemonSpawnEnv env missing %q; got %v", want, env)
	}
}

// TestDaemonSpawnEnvCarriesTheUpstream pins the other half of fix round 3's
// D2: the value that used to be argv now reaches the spawned daemon through
// its environment instead, under UpstreamProxyEnvVar exactly (runDaemonCmd,
// internal/cli/daemon.go, reads this same key name as its fallback).
func TestDaemonSpawnEnvCarriesTheUpstream(t *testing.T) {
	env := daemonSpawnEnv("/tmp/some-chottag-home", testUpstream)
	want := UpstreamProxyEnvVar + "=" + testUpstream
	if !slices.Contains(env, want) {
		t.Errorf("daemonSpawnEnv env missing %q; got %v", want, env)
	}
}

func TestDaemonSpawnEnvOmitsUpstreamProxyWhenEmpty(t *testing.T) {
	env := daemonSpawnEnv("/tmp/some-chottag-home", "")
	prefix := UpstreamProxyEnvVar + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			t.Errorf("daemonSpawnEnv(\"\") set %q, want it absent when there is no upstream", kv)
		}
	}
}

// F137: a daemon the shim spawns is never supervised, but it inherits the
// environment of whatever started the shell. On macOS that includes
// XPC_SERVICE_NAME=0, and under a terminal that is a systemd unit it
// includes INVOCATION_ID, so the daemon would record a supervisor and
// `daemon stop` would refuse it forever.
func TestDaemonSpawnEnvDropsSupervisorMarkers(t *testing.T) {
	markers := []string{"INVOCATION_ID", "SYSTEMD_EXEC_PID", "JOURNAL_STREAM", "XPC_SERVICE_NAME"}
	for _, k := range markers {
		t.Setenv(k, "inherited")
	}
	for _, kv := range daemonSpawnEnv("/tmp/some-chottag-home", "") {
		for _, k := range markers {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("daemonSpawnEnv passed %q through to the spawned daemon", kv)
			}
		}
	}
}

// A CHOTTAG_UPSTREAM_PROXY already in this process's environment must not
// reach a daemon spawned with no upstream. The handoff is set for the
// spawn, never inherited from an earlier one.
func TestDaemonSpawnEnvDropsAStaleUpstreamHandoff(t *testing.T) {
	t.Setenv(UpstreamProxyEnvVar, "http://stale:1")
	for _, kv := range daemonSpawnEnv("/tmp/some-chottag-home", "") {
		if strings.HasPrefix(kv, UpstreamProxyEnvVar+"=") {
			t.Errorf("daemonSpawnEnv with no upstream passed %q through", kv)
		}
	}
	var got []string
	for _, kv := range daemonSpawnEnv("/tmp/some-chottag-home", "http://fresh:2") {
		if strings.HasPrefix(kv, UpstreamProxyEnvVar+"=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != UpstreamProxyEnvVar+"=http://fresh:2" {
		t.Errorf("daemonSpawnEnv handoff entries = %q, want exactly the fresh one", got)
	}
}
