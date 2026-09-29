package shim

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
)

// ourProxyURL is this daemon's own loopback address, spelled exactly as the
// shim writes it into a child's HTTPS_PROXY (Run, §4.3 step 5) for a legacy
// daemon (Ruling 9): no secret to carry, since a legacy daemon never proved
// it holds one.
func ourProxyURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// IsOwnProxy reports whether raw names this daemon's own address —
// http://127.0.0.1:<port>, http://localhost:<port> or http://[::1]:<port>,
// with or without userinfo — what the shim writes into a child's
// HTTPS_PROXY (with the secret since part 1), and so what a nested claude,
// `chottag update` or `daemon restart` run inside a session inherits. Never
// an upstream — the F221 review's own top risk (a shell whose HTTPS_PROXY
// already carries our own secret must never read as a conflicting
// upstream, nor be forwarded to a freshly-spawned daemon as one). Any
// query or fragment (even an empty query, "?") makes it NOT our own
// spelling, so it is still compared as a possible upstream (fix round 1
// item 6 pins localhost/::1 and these two "not ours" shapes).
func IsOwnProxy(raw string, port int) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	portStr := strconv.Itoa(port)
	switch u.Host {
	case "127.0.0.1:" + portStr, "localhost:" + portStr, "[::1]:" + portStr:
	default:
		return false
	}
	return (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// SpawnUpstream is the upstream a freshly-spawned daemon is handed: env's
// HTTPS_PROXY, unless it already names this daemon's own address. That is
// what `chottag trace` exports and what a nested claude inherits, and
// forwarding it would make the daemon dial itself for every request (fix
// round 2, D1). Exported so `chottag daemon start|restart` resolve the
// upstream exactly as the shim does (F103).
func SpawnUpstream(env []string, port int) string {
	shellProxy := envGet(env, "HTTPS_PROXY")
	if IsOwnProxy(shellProxy, port) {
		return ""
	}
	return shellProxy
}

// StartDaemon starts `chottag daemon run` for home, detached (spawnFn), and
// polls ProbeHealth(port) until it confirms or pollBudget elapses (§4.3
// step 3). It polls even when the spawn failed, because another launch may
// have started the daemon meanwhile. spawnErr is reported alongside the
// result, never instead of it. Run calls this, and so do `chottag daemon
// start|restart`: one way to start the daemon, not two (F103).
func StartDaemon(home string, port int, env []string) (confirmed bool, health proxy.Health, spawnErr error) {
	if exe, err := daemonExecutable(); err != nil {
		spawnErr = err
	} else {
		spawnErr = spawnFn(exe, home, SpawnUpstream(env, port))
	}
	deadline := time.Now().Add(pollBudget)
	for !confirmed && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		confirmed, health = ProbeHealth(port)
	}
	return confirmed, health, spawnErr
}

// daemonExecutable is the binary StartDaemon spawns as `daemon run`: this
// process's executable with symlinks resolved (F239). On macOS
// os.Executable returns the path the process was started by, so a shim
// started as bin/claude sees .../bin/claude, and a daemon spawned by that
// name would run as the shim and spawn another: a fork chain. The resolved
// target is chottag itself. A path that cannot be resolved is used as is
// (the spawn then reports its own error). A binary that is itself named
// claude is refused: whatever it is, it would run as the shim.
func daemonExecutable() (string, error) {
	exe, err := executablePath()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if filepath.Base(exe) == "claude" {
		return "", fmt.Errorf("chottag: not starting the daemon from %s: a binary named claude runs as the shim and would start another daemon", exe)
	}
	return exe, nil
}
