package doctor

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// The range a moved port is chosen from (D7). store.DefaultPort, 47821, is
// just below it.
const (
	PortRangeFirst = 47822
	PortRangeLast  = 47841
)

// StateChecks are spec §2.2 rows 7-10, plus the daemon-version and
// daemon-identity checks (public release design §2.4; F221, part 1 T11),
// in order.
func StateChecks() []Check {
	return []Check{rolesCheck(), realClaudeCheck(), portCheck(), daemonCheck(), daemonVersionCheck(), daemonIdentityCheck()}
}

func portAddr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// rolesCheck is row 7: one row naming every dangling role, in every pool
// (M8). With only the default pool its texts are as they always were; with
// more, each role is prefixed by its pool, and a pool with members but none in
// rotation is a warning (info): its sessions get a 503 until one is.
func rolesCheck() Check {
	return Check{
		ID: "roles",
		Detect: func(e *Env) Finding {
			st, err := e.State()
			if err != nil {
				return Internal(err)
			}
			if len(st.Accounts) == 0 {
				return Finding{Status: StatusProblem, Detail: "no accounts are registered", Hint: "chottag login <name>"}
			}
			multi := len(st.PoolNames()) > 1
			var bad, ok, warn, targets []string
			hint := ""
			for _, pool := range st.PoolNames() {
				members, p := st.Members(pool), st.PoolOf(pool)
				prefix := ""
				if multi {
					prefix = pool + " "
				}
				var badHere bool
				if !roleOK(members, p.Serving, pool == store.DefaultPool || hasRotating(members)) {
					bad = append(bad, fmt.Sprintf("%sserving %q", prefix, p.Serving))
					targets = append(targets, firstServing(members, pool == store.DefaultPool))
					badHere = true
				}
				if !roleOK(members, p.Remote, true) {
					bad = append(bad, fmt.Sprintf("%sremote %q", prefix, p.Remote))
					targets = append(targets, firstMember(members))
					badHere = true
				}
				if badHere {
					continue
				}
				ok = append(ok, fmt.Sprintf("%s: serving %s, remote %s", pool, orNone(p.Serving), orNone(p.Remote)))
				if multi && len(members) > 0 && !hasRotating(members) {
					warn = append(warn, "pool "+pool+" has no account in rotation, so its sessions get a 503")
					if hint == "" {
						hint = "chottag rotate " + members[0].Name + " on"
					}
				}
			}
			if len(bad) > 0 {
				what := "names no registered account"
				if multi {
					what = "names no member of its pool"
				}
				return Finding{Status: StatusProblem, Detail: strings.Join(bad, " and ") + " " + what + "; " + fixTargets(targets)}
			}
			if !multi {
				p := st.PoolOf(store.DefaultPool)
				return Finding{Status: StatusOK, Detail: fmt.Sprintf("serving %s, remote %s", p.Serving, p.Remote)}
			}
			detail := strings.Join(ok, "; ")
			if len(warn) > 0 {
				return Finding{Status: StatusInfo, Detail: detail + "; warning: " + strings.Join(warn, "; warning: "), Hint: hint}
			}
			return Finding{Status: StatusOK, Detail: detail}
		},
		Fix: func(e *Env) error {
			_, err := e.Store().Update(func(st *store.State) error {
				if len(st.Accounts) == 0 {
					return store.ErrNoAccounts
				}
				for _, pool := range st.PoolNames() {
					members, p := st.Members(pool), st.PoolOf(pool)
					if !roleOK(members, p.Serving, pool == store.DefaultPool || hasRotating(members)) {
						p.Serving = firstServing(members, pool == store.DefaultPool)
					}
					if !roleOK(members, p.Remote, true) {
						p.Remote = firstMember(members)
					}
					if pool == store.DefaultPool {
						st.Serving, st.Remote = p.Serving, p.Remote
						continue
					}
					st.Pools[pool] = p
				}
				return nil
			})
			return err
		},
	}
}

// fixTargets says where --fix puts the roles: one name when they all go to the
// same account, as the check always said, else each in turn.
func fixTargets(targets []string) string {
	same := true
	for _, t := range targets {
		same = same && t == targets[0]
	}
	if same {
		if targets[0] == "" {
			return "the fix leaves it empty"
		}
		return "the fix gives it to " + targets[0]
	}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = orNone(t)
	}
	return "the fix gives them to " + strings.Join(names, " and ")
}

func firstMember(members []store.Account) string {
	if len(members) == 0 {
		return ""
	}
	return members[0].Name
}

// roleOK reports whether v is a sound value for a role of a pool holding
// members: a member's name, or empty when the pool has no one to hold it
// (needed false: no rotating member for a serving role).
func roleOK(members []store.Account, v string, needed bool) bool {
	if v == "" {
		return len(members) == 0 || !needed
	}
	for _, a := range members {
		if strings.EqualFold(a.Name, v) {
			return true
		}
	}
	return false
}

func hasRotating(members []store.Account) bool {
	for _, a := range members {
		if a.Rotates() {
			return true
		}
	}
	return false
}

// firstServing is the first member in rotation. The default pool falls back to
// its first member, as the fix always did.
func firstServing(members []store.Account, isDefault bool) string {
	for _, a := range members {
		if a.Rotates() {
			return a.Name
		}
	}
	if isDefault && len(members) > 0 {
		return members[0].Name
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// realClaudeCheck is row 8, a reconcile. The shim re-resolves on every
// launch anyway.
func realClaudeCheck() Check {
	return Check{
		ID: "real-claude",
		Detect: func(e *Env) Finding {
			st, err := e.State()
			if err != nil {
				return Internal(err)
			}
			resolved, err := e.ResolveClaude(e.PATH, e.BinDir(), st.RealClaude)
			switch {
			case err != nil:
				return Finding{Status: StatusProblem, Detail: strings.TrimPrefix(err.Error(), "chottag: "), Hint: "install Claude Code, or put the directory holding its claude on PATH"}
			case resolved == st.RealClaude:
				return Finding{Status: StatusOK, Detail: "claude is " + resolved}
			case st.RealClaude == "":
				return Finding{Status: StatusInfo, Detail: "not cached yet; the first claude launch caches it (PATH resolves " + resolved + ")"}
			}
			return Finding{Status: StatusProblem, Detail: fmt.Sprintf("state.json's cached claude %s is stale; PATH resolves %s", st.RealClaude, resolved)}
		},
		Fix: func(e *Env) error {
			st, err := e.State()
			if err != nil {
				return err
			}
			resolved, err := e.ResolveClaude(e.PATH, e.BinDir(), st.RealClaude)
			if err != nil {
				return err
			}
			_, err = e.Store().Update(func(st *store.State) error { st.RealClaude = resolved; return nil })
			return err
		},
	}
}

// portCheck is row 9. The daemon never searches for a port (§4.8), so a
// taken port is moved only here, under --fix, and only with zero live
// sessions and no daemon: live sessions hold the old port in HTTPS_PROXY
// (D7).
func portCheck() Check {
	return Check{
		ID: "port",
		Detect: func(e *Env) Finding {
			st, err := e.State()
			if err != nil {
				return Internal(err)
			}
			port := st.ResolvedPort()
			if e.ProbeHealth(port) {
				return Finding{Status: StatusOK, Detail: fmt.Sprintf("the chottag daemon answers on port %d", port)}
			}
			if l, err := e.Listen(portAddr(port)); err == nil {
				l.Close()
				return Finding{Status: StatusOK, Detail: fmt.Sprintf("port %d is free for the daemon", port)}
			}
			return Finding{Status: StatusProblem, Detail: fmt.Sprintf("port %d is taken, and no chottag daemon answers on it", port)}
		},
		Fix: func(e *Env) error {
			st, err := e.State()
			if err != nil {
				return err
			}
			old := st.ResolvedPort()
			ds, err := e.Inspect()
			if err != nil && !errors.Is(err, daemonlock.ErrUnreadableRecord) {
				return err
			}
			if ds.Running {
				return fmt.Errorf("a chottag daemon holds %s; doctor never moves the port under a running daemon (run chottag daemon restart, or stop it first)", daemonlock.Path(e.Home))
			}
			n, err := e.LiveSessions()
			if err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%d live claude session(s) were started with port %d; close them, then run chottag doctor --fix again", n, old)
			}
			for p := PortRangeFirst; p <= PortRangeLast; p++ {
				if p == old {
					continue
				}
				l, err := e.Listen(portAddr(p))
				if err != nil {
					continue
				}
				l.Close()
				_, err = e.Store().Update(func(st *store.State) error { st.Port = p; return nil })
				return err
			}
			return fmt.Errorf("no free port in %d-%d", PortRangeFirst, PortRangeLast)
		},
	}
}

// daemonCheck is row 10. It never fixes anything: a daemon that is simply
// not running is the normal state between launches (the shim starts one on
// demand, R38), and no other case is something ensureStarted or any fix
// could safely repair; doctor never stops a daemon (§2.4, controller
// ruling D13).
func daemonCheck() Check {
	return Check{
		ID: "daemon",
		Detect: func(e *Env) Finding {
			st, err := e.State()
			if err != nil {
				return Internal(err)
			}
			port := st.ResolvedPort()
			ds, ierr := e.Inspect()
			if ierr != nil && !errors.Is(ierr, daemonlock.ErrUnreadableRecord) {
				return Internal(ierr)
			}
			healthy := e.ProbeHealth(port)
			lock := daemonlock.Path(e.Home)
			switch {
			case ierr != nil:
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("a daemon holds %s, but its record is unreadable, so its pid is unknown", lock), Hint: "lsof " + lock}
			case ds.Running && healthy:
				return Finding{Status: StatusOK, Detail: fmt.Sprintf("running (pid %d) on port %d", ds.Record.PID, port)}
			case ds.Running:
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("the daemon (pid %d) holds %s but does not answer on port %d", ds.Record.PID, lock, port), Hint: "chottag daemon restart"}
			case healthy:
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("a chottag daemon answers on port %d, but none holds %s: an older chottag, or another CHOTTAG_HOME", port, lock), Hint: fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", port)}
			}
			return Finding{Status: StatusInfo, Detail: fmt.Sprintf("not running (port %d); the next claude launch starts it", port), Hint: "chottag daemon start"}
		},
	}
}

// daemonVersionCheck (public release design §2.4) warns when a running
// daemon's health version differs from this chottag binary's own — the same
// signal `status`'s report overlay shows, surfaced here too since not every
// user runs `status` day to day. No Fix: a restart interrupts every live
// session, so it stays the user's call (chottag daemon restart), never
// something --fix does silently.
func daemonVersionCheck() Check {
	return Check{
		ID: "daemon-version",
		Detect: func(e *Env) Finding {
			st, err := e.State()
			if err != nil {
				return Internal(err)
			}
			running, version := e.DaemonVersion(st.ResolvedPort())
			switch {
			case !running:
				return Finding{Status: StatusOK, Detail: "no daemon running"}
			case version == "":
				// Mirrors status.go's own probe guard (runStatus's `ok &&
				// version != ""`): an empty health version is "answered, but
				// said nothing usable", never grounds for "the daemon runs ,
				// this chottag is <v>" — a malformed detail, and a false
				// problem for a case that isn't one.
				return Finding{Status: StatusInfo, Detail: "the daemon did not report its version"}
			case version == e.SelfVersion:
				return Finding{Status: StatusOK, Detail: fmt.Sprintf("daemon runs %s", version)}
			default:
				return Finding{
					Status: StatusProblem,
					Detail: fmt.Sprintf("the daemon runs %s, this chottag is %s", version, e.SelfVersion),
					Hint:   "chottag daemon restart",
				}
			}
		},
	}
}

// daemonIdentityCheck (F221, part 1 T11) is the health-challenge judgement
// `status` overlays too (Env.DaemonIdentity, cli.daemonIdentity): whether a
// daemon answering this install's port proved it holds ca/proxy.secret. No
// Fix, for the same reason daemonVersionCheck has none: a restart
// interrupts every live session, so it stays the user's call.
func daemonIdentityCheck() Check {
	return Check{
		ID: "daemon-identity",
		Detect: func(e *Env) Finding {
			st, err := e.State()
			if err != nil {
				return Internal(err)
			}
			port := st.ResolvedPort()
			switch id := e.DaemonIdentity(port); id {
			case "none":
				return Finding{Status: StatusOK, Detail: "no daemon running"}
			case "verified":
				return Finding{Status: StatusOK, Detail: "the daemon proved it holds this install's proxy secret"}
			case "legacy":
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("the daemon on port %d predates proxy authentication", port), Hint: "chottag daemon restart"}
			case "mismatch":
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("the process on port %d did not prove it holds ca/proxy.secret", port), Hint: fmt.Sprintf("chottag daemon restart (if you replaced the secret); otherwise lsof -nP -iTCP:%d -sTCP:LISTEN", port)}
			case "unknown":
				return Finding{Status: StatusInfo, Detail: "a daemon answers, but ca/proxy.secret is unreadable, so it cannot be verified (see the proxy-secret row)"}
			default:
				return Internal(fmt.Errorf("daemon-identity: unrecognised identity %q", id))
			}
		},
	}
}
