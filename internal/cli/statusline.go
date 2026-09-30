package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// statuslineMaxDepth bounds the ancestor walk: a Claude Code status line
// command sits a few processes below claude, and a display must not chase a
// long chain.
const statuslineMaxDepth = 8

// statuslineProbeTimeout keeps the daemon probe short: a status line runs on
// every refresh and must not stall the display.
const parentPIDTimeout = 200 * time.Millisecond

const statuslineProbeTimeout = 300 * time.Millisecond

// parentPID is the parent of pid. A var so TestMain (invariants_test.go) can
// arm a panicking default: the real one runs ps(1).
var parentPID = func(pid int) (int, error) {
	// A hung ps must not stall a status-line redraw.
	ctx, cancel := context.WithTimeout(context.Background(), parentPIDTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// statuslineProbe reports whether chottag's daemon answers on port, within a
// short timeout. A var for the same reason as statusProbe.
var statuslineProbe = func(port int) bool {
	return shim.ProbeHealthWithin(port, statuslineProbeTimeout)
}

// SetParentPIDForTest swaps parentPID and returns a func that restores what
// was installed at the moment of the call (F130).
func SetParentPIDForTest(fn func(pid int) (int, error)) (restore func()) {
	orig := parentPID
	parentPID = fn
	return func() { parentPID = orig }
}

// SetStatuslineProbeForTest swaps statuslineProbe likewise.
func SetStatuslineProbeForTest(fn func(port int) bool) (restore func()) {
	orig := statuslineProbe
	statuslineProbe = fn
	return func() { statuslineProbe = orig }
}

type statuslineResult struct {
	Session string `json:"session"`
	Daemon  string `json:"daemon"`
	Serving string `json:"serving"`
}

// runStatusline prints one line for Claude Code's status line: whether this
// session goes through chottag, and which account serves it. It never reads
// stdin and always exits 0: a status line must never show an error. Nothing
// secret ever reaches the output.
func runStatusline(args []string, r *reporter) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage("usage: chottag statusline [--json]")
	}

	rep := statuslineResult{Session: "home", Daemon: "unknown"}
	if h, err := home(); err == nil {
		rep = statuslineCheck(h)
	}
	if r.JSON() {
		return r.OK(rep)
	}
	line := "chottag: off"
	switch {
	case rep.Session != "routed":
	case rep.Daemon != "up":
		line = "chottag: down"
	case rep.Serving != "":
		line = "chottag: " + rep.Serving
	default:
		line = "chottag: up"
	}
	fmt.Fprintln(r.Stdout(), line)
	return r.OK(nil)
}

// statuslineCheck decides routed or home for home h, and probes the daemon
// only when routed. Any error means "home": a status line has no error
// channel.
func statuslineCheck(h string) statuslineResult {
	rep := statuslineResult{Session: "home", Daemon: "unknown"}
	if _, err := os.Stat(filepath.Join(h, "state.json")); err != nil {
		return rep
	}
	st, err := (store.Store{Dir: h}).Load()
	if err != nil {
		return rep
	}
	port := st.ResolvedPort()
	if !proxyEnvMatches(os.Getenv("HTTPS_PROXY"), port) && !hasLiveAncestor(h) {
		return rep
	}
	rep.Session = "routed"
	if statuslineProbe(port) {
		rep.Daemon, rep.Serving = "up", st.Serving
	} else {
		rep.Daemon = "down"
	}
	return rep
}

// proxyEnvMatches reports whether v is http://chottag:<anything>@127.0.0.1:port,
// the URL the shim hands a session it launched.
func proxyEnvMatches(v string, port int) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.User == nil || u.User.Username() != "chottag" {
		return false
	}
	return u.Hostname() == "127.0.0.1" && u.Port() == strconv.Itoa(port)
}

// hasLiveAncestor reports whether one of this process's first
// statuslineMaxDepth ancestors is a claude session chottag launched. The
// shim registers the pid it then execs claude in, so that pid is claude.
// liveSessions is the reader `daemon stop` uses: Live() prunes dead entries
// on read.
func hasLiveAncestor(h string) bool {
	live, err := liveSessions(h)
	if err != nil || len(live) == 0 {
		return false
	}
	set := make(map[int]bool, len(live))
	for _, s := range live {
		set[s.PID] = true
	}
	pid := os.Getppid()
	for level := 0; level < statuslineMaxDepth; level++ {
		if set[pid] {
			return true
		}
		if level == statuslineMaxDepth-1 {
			break // the last pid is checked; no lookup whose result is dropped
		}
		next, err := parentPID(pid)
		if err != nil || next <= 1 || next == pid {
			return false
		}
		pid = next
	}
	return false
}
