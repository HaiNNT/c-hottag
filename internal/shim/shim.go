package shim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// execFn and spawnFn are the shim's only irreversible acts, behind seams so
// every other behaviour is testable without performing them. Production
// values are set here; tests swap them. Same reason wireProxyConfig was
// extracted in M1c5: the wiring a test cannot reach is the wiring that
// breaks.
var (
	execFn func(bin string, args, env []string) error = syscallExec
	// spawnFn's upstream is this shell's pre-existing HTTPS_PROXY, if any
	// (fix round 1, D2): spawnDaemon passes it through as --upstream-proxy
	// so a freshly-started daemon's upstream actually matches what the
	// upstream-mismatch message tells the operator to expect after a
	// restart.
	spawnFn func(exe, home, upstream string) error = spawnDaemon
)

// inspectLock is daemonlock.Inspect, called as a seam so it can be pinned in
// this package's own doc comments alongside execFn/spawnFn even though it is
// never swapped: legacy identity (a daemon that answers health with no
// proof at all, Ruling 9) is trusted only while THIS home's daemon.lock is
// held, because a process belonging to another user cannot take a flock
// inside a 0700 home — that is what stands in for the proof a pre-part-1
// daemon has no way to give. Tests hold a REAL lock (daemonlock.Acquire)
// rather than swapping this var: a fabricated Status would prove nothing
// about what an actual flock can and cannot be forged.
var inspectLock = daemonlock.Inspect

// TrustLegacy is Ruling 30's one decision for whether a proof-less
// "legacy" health answer (VerifyHealth's IdentityLegacy) may be trusted:
// only while THIS home's own daemon.lock is held, by the EXACT pid that
// answered health. inspectLock's own doc comment above explains why real
// callers never fake it; this just gives that one decision a name so two
// packages that both need it — Run below, which turns it into claude's
// launch decision, and internal/cli's daemonIdentity, which turns it into
// doctor's and status's report — share it rather than risking two
// definitions that quietly drift apart (F103, T11 fix round 1 finding 3).
//
// healthPID is the PID field FROM THE HEALTH DOCUMENT that just answered
// (VerifyHealth's second return value), never this process's own pid.
// status and err are Inspect's own return values, passed through so a
// caller that needs its own wording (Run's three distinct stderr messages
// below) can still build it from them; trusted alone is enough for a
// caller that only ever needs a yes/no.
func TrustLegacy(home string, healthPID int) (trusted bool, status daemonlock.Status, err error) {
	status, err = inspectLock(home)
	if err != nil {
		return false, status, err
	}
	return status.Running && healthPID == status.Record.PID, status, nil
}

// pollInterval and pollBudget bound the wait for a just-spawned daemon to
// answer its health endpoint (§4.3 step 3).
const (
	pollInterval = 50 * time.Millisecond
	pollBudget   = 2 * time.Second
)

// syscallExec is execFn's production value. It replaces this process with
// bin, so on success it never returns.
func syscallExec(bin string, args, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, args...), env)
}

// daemonSpawnArgs builds spawnDaemon's argv: always just `daemon run`.
//
// upstream USED to be appended here as --upstream-proxy <upstream> (fix
// round 1, D2) so a freshly-spawned daemon's upstream matched what the
// mismatch check compares against. That put a shell's raw HTTPS_PROXY —
// which may carry user:pass@host, F115's own framing — into this process's
// **argv**, world-readable via /proc/<pid>/cmdline on Linux and visible to
// the same user via `ps -ww` on macOS. --upstream-proxy typed by a user by
// hand was opt-in and visible to them; doing the identical thing here,
// automatically, for every user with HTTPS_PROXY set, was not (whole-branch
// review D2). daemonSpawnEnv below now carries it instead: an env var is
// not argv, and a process's own environ is not world-readable
// (/proc/<pid>/environ is 0400 owner-only on Linux, and `ps` does not show
// it at all).
//
// Pulled out of spawnDaemon, which also calls Setsid and cmd.Start(), so
// this — the part that genuinely can be asserted without forking a
// process — has a direct test. TestMain's panicking spawnFn default (fix
// round 1, D5) makes spawnDaemon itself deliberately unreachable by any
// test: `go tool cover` reported it at 0.0%, and a reviewer proved the
// consequence by renaming the flag AND inverting the empty-check at once
// and finding the whole suite still green (fix round 2, D2) — which is why
// this stays a direct, self-contained test rather than trusting spawnDaemon
// itself to exercise it.
func daemonSpawnArgs() []string {
	return []string{"daemon", "run"}
}

// UpstreamProxyEnvVar is the environment variable spawnDaemon uses to hand
// a freshly-spawned daemon this shell's pre-existing upstream proxy — see
// daemonSpawnArgs's doc comment for why this moved out of argv (whole-branch
// review D2). Exported because internal/cli's runDaemonCmd (daemon.go) reads
// it as a fallback for --upstream-proxy, with the explicit flag still
// winning when both are present: the flag is a documented, stable public
// interface; this is an internal handoff between two processes of the same
// install, and naming it once here rather than repeating the literal in
// both packages is what keeps them from drifting apart (F103, F114's
// lesson applied to a string instead of a function).
const UpstreamProxyEnvVar = "CHOTTAG_UPSTREAM_PROXY"

// daemonSpawnEnv builds spawnDaemon's environment: the current process's
// own environment with CHOTTAG_HOME set to home (so the detached daemon
// resolves its own home, cli.go's home(), from what the shim resolved
// rather than depending on the caller's environment for it), and, when
// upstream is non-empty, daemonUpstreamEnvKey set to it (fix round 3, D2 —
// see daemonSpawnArgs's doc comment for why this is the environment and
// not argv). Same testability reasoning as always: a wrong key here would
// silently send the daemon to the wrong home, or leave it unable to learn
// its upstream at all. It also drops supervisorEnvKeys and any inherited
// UpstreamProxyEnvVar (see supervisorEnvKeys).
func daemonSpawnEnv(home, upstream string) []string {
	env := dropEnv(os.Environ(), supervisorEnvKeys...)
	env = setEnv(env, "CHOTTAG_HOME", home)
	if upstream != "" {
		return setEnv(env, UpstreamProxyEnvVar, upstream)
	}
	// Never inherited: a handoff left in this process's environment by an
	// earlier spawn must not become this daemon's upstream.
	return dropEnv(env, UpstreamProxyEnvVar)
}

// supervisorEnvKeys are the variables a service manager sets on a process
// it starts. A daemon the shim spawns is never supervised, but it would
// otherwise inherit them from whatever started this shell: XPC_SERVICE_NAME=0
// in every macOS Terminal shell, and INVOCATION_ID under a terminal that
// is itself a systemd unit. internal/daemonlock would then record a
// supervisor, and `daemon stop` would refuse the daemon forever (F137).
var supervisorEnvKeys = []string{"INVOCATION_ID", "SYSTEMD_EXEC_PID", "JOURNAL_STREAM", "XPC_SERVICE_NAME"}

// dropEnv returns a copy of env without any entry for keys.
func dropEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// spawnDaemon is spawnFn's production value. It starts `<exe> daemon run`
// detached in a new session so it outlives this shim's process, including
// the exec in §4.3 step 7.
func spawnDaemon(exe, home, upstream string) error {
	// exe is os.Executable() (chottag's own resolved path, via the
	// executablePath seam), not attacker- or user-controlled input; args
	// and env are built by daemonSpawnArgs/daemonSpawnEnv above, both
	// tested directly. Setsid and cmd.Start() below are the deliberate
	// untestable remnant, shrunk to exactly what cannot be asserted
	// without actually forking a process — which TestMain's panicking
	// spawnFn default exists to prevent (fix round 1, D5; fix round 2,
	// D2).
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command(exe, daemonSpawnArgs()...)
	cmd.Env = daemonSpawnEnv(home, upstream)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// Run is the shim's entire launch sequence (§4.3): CHOTTAG_BYPASS escape
// hatch, resolve the real claude, confirm (or start) the daemon, build the
// child environment, hand off to cmux's own claude wrapper if warranted
// (F243, decided early but acted on only once the child environment is
// complete — see the hand-off's own comment below), register the session,
// and exec. args are the
// arguments `claude` was invoked with; env is the environment it was
// invoked in (a slice, not the process's own os.Environ(), so a test can
// hand it an arbitrary one). version is this running chottag build's own
// version (cli.Version): internal/shim cannot import internal/cli, which
// imports it, so the caller passes it explicitly rather than through a
// package var — cli.go's dispatch is Run's one production caller, and it
// passes cli.Version (fix round 2, N2: a package var could silently be set
// to the wrong build's version and no test would catch it; a parameter
// fails loud the moment a caller forgets it, and a test can pin the exact
// value threaded through). It returns the process exit code; execFn only
// returns on failure, since success replaces this process.
//
// stdout is kept, not used: cli.go's dispatch calls every command with the
// same (args, stdout, stderr) shape, and the shim must stay silent on the
// happy path anyway — it is about to become `claude` (same shape and reason
// as internal/cli/daemon.go's runDaemonCmd).
func Run(args []string, home string, env []string, version string, stdout, stderr io.Writer) int {
	// Step 1: the escape hatch. Nothing above this line may run, and
	// nothing below it may run when this fires — not state.json, not the
	// CA, not the daemon. It exists for when any of those is broken.
	if envGet(env, "CHOTTAG_BYPASS") == "1" {
		real, err := ResolveClaude(envGet(env, "PATH"), filepath.Join(home, "bin"), "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exit.Error
		}
		if err := execFn(real, args, env); err != nil {
			fmt.Fprintln(stderr, "chottag:", err)
			return exit.Error
		}
		return exit.OK
	}

	// cmuxTarget is F243's cmux hand-off decision (cmuxHandoffTarget's own
	// doc comment): computed here, cheaply, right after the escape hatch,
	// but not ACTED on until far below — see the comment at the exec site
	// for why the hand-off itself must wait for the fully-built child
	// environment (fix round 1, F243-R1).
	cmuxTarget := cmuxHandoffTarget(env, home)

	st, err := (store.Store{Dir: home}).Load()
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	port := st.ResolvedPort()

	real, err := ResolveClaude(envGet(env, "PATH"), filepath.Join(home, "bin"), st.RealClaude)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exit.Error
	}
	if real != st.RealClaude {
		// Caching the resolution is an optimisation, not a correctness
		// requirement (ResolveClaude re-derives it every time regardless):
		// a failure to persist is reported but does not stop the launch.
		if _, err := (store.Store{Dir: home}).Update(func(s *store.State) error {
			s.RealClaude = real
			return nil
		}); err != nil {
			fmt.Fprintln(stderr, "chottag: could not save the resolved claude path:", err)
		}
	}

	// secret is this install's proxy secret (F221): only a daemon that
	// proves it holds this exact value, via VerifyHealth below, is ever
	// handed it back inside HTTPS_PROXY. Loaded here, not only by setup, so
	// a home whose secret file is missing (an install that predates part
	// 1, or one a user deleted) still gets one before any session
	// launches. A *PermError already names `chottag doctor --fix` in its
	// own Error() text; ErrMalformed does not (it just names the path), so
	// that hint is appended here for it explicitly (fix round 1 item 3).
	secret, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, proxyauth.ErrMalformed) {
			msg += "; run: chottag doctor --fix"
		}
		fmt.Fprintln(stderr, "chottag:", msg)
		return exit.Error
	}

	// shellProxy is this shell's pre-existing HTTPS_PROXY, read once and
	// reused below to compare against the confirmed daemon's upstream.
	// Telling a freshly-spawned daemon its upstream is StartDaemon's own
	// concern now (it reads env and resolves SpawnUpstream itself), not
	// this variable's.
	shellProxy := envGet(env, "HTTPS_PROXY")

	confirmed, health := ProbeHealth(port)
	if !confirmed {
		var spawnErr error
		confirmed, health, spawnErr = StartDaemon(home, port, env)
		if !confirmed {
			fmt.Fprintln(stderr, couldNotConfirmMsg(port, spawnErr))
			return exit.Error
		}
	}

	// id and vh establish WHO answered, not merely THAT something did:
	// VerifyHealth issues a fresh nonce and checks the returned HMAC proof
	// against secret (F221, security review L4, Ruling 9). version is
	// passed so a proof-less daemon reporting the SAME version as this
	// running build resolves as a mismatch, not legacy: T10 guarantees a
	// part-1-or-later daemon never SERVES at all without its secret loading
	// (its start fails first), so the same build answering health with no
	// proof at all cannot legitimately be a part-1+ daemon that merely
	// hasn't been asked yet — something else is wrong, which is exactly
	// what Mismatch already means to the caller (controller ruling, fix
	// round 1 item 5, reaffirmed fix round 2 N4) — only a daemon of a
	// genuinely different (older, necessarily pre-part-1) version can be
	// legacy.
	//
	//   - IdentityVerified is the only case ever handed the secret in
	//     HTTPS_PROXY.
	//   - IdentityLegacy (a proof-less daemon of a different, necessarily
	//     older version) is trusted only while THIS home's OWN daemon.lock
	//     is held by the EXACT process that answered health (inspectLock,
	//     and vh.PID must equal the lock's own record — fix round 1 item
	//     4): the flock a process of another user cannot take inside a
	//     0700 home stands in for the proof a legacy daemon cannot give,
	//     but only for the process that actually holds it, never merely
	//     "something holds it". Without both, a proof-less listener is a
	//     squatter, and claude is not started.
	//   - IdentityMismatch (a proof for a DIFFERENT secret, or a proof-less
	//     daemon of this SAME version) is always fatal.
	//   - IdentityNone here means the daemon that just confirmed liveness
	//     via ProbeHealth stopped answering by the time this second,
	//     nonce-carrying probe ran — rare, but handled the same as never
	//     having confirmed at all.
	id, vh := VerifyHealth(port, secret, version)
	var httpsProxy string
	switch id {
	case IdentityVerified:
		health = vh
		httpsProxy = secret.ProxyURL("127.0.0.1:" + strconv.Itoa(port))
	case IdentityLegacy:
		trusted, lockStatus, lerr := TrustLegacy(home, vh.PID)
		switch {
		case lerr != nil:
			fmt.Fprintf(stderr, "chottag: a process on port %d answers like a chottag daemon but its lock in %s/run could not be read (%v), so it cannot prove it holds this install's secret; claude was not started (find it: lsof -nP -iTCP:%d -sTCP:LISTEN)\n", port, home, lerr, port)
			return exit.Error
		case !lockStatus.Running:
			fmt.Fprintf(stderr, "chottag: a process on port %d answers like a chottag daemon but holds no lock in %s/run and cannot prove it holds this install's secret; claude was not started (find it: lsof -nP -iTCP:%d -sTCP:LISTEN)\n", port, home, port)
			return exit.Error
		case !trusted:
			// The lock is held, but by a DIFFERENT pid than the one that
			// answered health on port: the lock proves nothing about the
			// listener itself (fix round 1 item 4; fix round 2 N3 gives
			// this its own message rather than reusing "holds no lock",
			// which would be misleading — a lock genuinely IS held here).
			fmt.Fprintf(stderr, "chottag: the daemon holding %s is pid %d, but port %d is answered by pid %d; claude was not started\n", daemonlock.Path(home), lockStatus.Record.PID, port, vh.PID)
			return exit.Error
		}
		health = vh
		httpsProxy = ourProxyURL(port)
		fmt.Fprintf(stderr, "chottag: the running daemon (%s) predates proxy authentication; this session runs without it until you run: chottag daemon restart\n", vh.Version)
	case IdentityMismatch:
		fmt.Fprintf(stderr, "chottag: the process on port %d did not prove it holds this install's proxy secret (%s), so claude was not started. If you replaced that file, run: chottag daemon restart. Otherwise another program holds the port (lsof -nP -iTCP:%d -sTCP:LISTEN).\n", port, proxyauth.Path(home), port)
		return exit.Error
	default: // IdentityNone
		fmt.Fprintln(stderr, couldNotConfirmMsg(port, nil))
		return exit.Error
	}

	// A pre-existing HTTPS_PROXY is the daemon's upstream (proxy.go's
	// UpstreamProxy doc), and the daemon accepts one only at startup — a
	// daemon left running from an earlier shell may hold a different one.
	// Proceeding would route this shell's traffic through an upstream the
	// operator did not choose in this shell, so a disagreement is fatal.
	//
	// A shellProxy that already names THIS daemon's own loopback address —
	// exactly what `chottag trace`'s export snippet sets, and what any
	// nested `claude` inherits — is not an upstream at all, so it is never
	// a conflict: it is already configured, and step 7 below rewrites it to
	// the identical value regardless (fix round 1, D3).
	//
	// health.Upstream is scheme://host with userinfo already removed
	// server-side (proxy.Health's doc comment, fix round 3 D1) — the daemon
	// never serialises a password to this unauthenticated loopback
	// endpoint at all. shellProxy is reduced the identical way before the
	// comparison, so a difference in password alone is never reported as a
	// conflict; that is deliberate, not a gap — this check exists to catch
	// a genuinely different upstream, not to compare whose password it is.
	//
	// Beyond the userinfo reduction, the comparison is not otherwise
	// normalised: WithoutUserinfo drops everything but scheme://host, so a
	// `http://localhost:<port>` spelling or a trailing slash or path no
	// longer reads as a conflict on that basis alone — only the IsOwnProxy
	// exemption below still catches the shim's own exact spelling. That is
	// acceptable: WithoutUserinfo exists to compare WHICH proxy, not to
	// distinguish spellings of the same one (fix round 3, D1).
	//
	// A shellProxy WithoutUserinfo cannot reduce (empty, unparsable, opaque,
	// hostless — see its doc comment) is NOT the same as no upstream
	// configured, even though both reduce to "". Fix round 3's D1 fix
	// collapsed those two distinct cases onto the same empty string:
	// a scheme-less HTTPS_PROXY like "corp-proxy:8080" (the shape
	// http_proxy and `curl -x` accept, F81's own documented example of what
	// users actually type) reduced to "", compared equal to a daemon
	// reporting no upstream, and the shim proceeded — turning the one check
	// that exists to stop a user's traffic silently bypassing their
	// corporate proxy into a fail-open, in a milestone whose stated
	// property is that every failure path fails closed (whole-branch fix
	// round 4, D1). reducedShellProxy is computed once and unreducible is
	// checked explicitly so a non-empty, unreducible shellProxy is always a
	// conflict — we cannot prove it agrees with the daemon, so we do not
	// proceed — unless IsOwnProxy(shellProxy, port) is true (the exemption
	// above): with or without userinfo, e.g. a nested claude that inherited
	// our own secret URL.
	reducedShellProxy := redact.WithoutUserinfo(shellProxy)
	unreducible := shellProxy != "" && reducedShellProxy == ""
	if (unreducible || reducedShellProxy != health.Upstream) && !IsOwnProxy(shellProxy, port) {
		fmt.Fprintf(stderr, "chottag: HTTPS_PROXY=%s conflicts with the running daemon's upstream %s; "+
			"the daemon accepts an upstream only at startup, so stop it (it will restart with the new one) "+
			"or unset HTTPS_PROXY\n", redact.UpstreamProxy(shellProxy), redact.UpstreamProxy(health.Upstream))
		return exit.Error
	}

	childEnv := setEnv(env, "HTTPS_PROXY", httpsProxy)

	caDir := filepath.Join(home, "ca")
	nodeCA := filepath.Join(caDir, "ca.pem")
	if theirsPath := envGet(env, "NODE_EXTRA_CA_CERTS"); theirsPath != "" {
		if nodeCAAlreadyOurs(theirsPath, caDir) {
			// theirsPath already names a file directly inside caDir —
			// chottag's own ca.pem, or a bundle.pem THIS SAME home already
			// wrote. That is exactly what a cmux hand-off's second pass
			// inherits (F243-R2): cmux's wrapper passes the environment it
			// was exec'd with straight through, untouched, so pass 1's own
			// NODE_EXTRA_CA_CERTS (already merged, if it needed to be)
			// comes back as pass 2's. Re-merging it here would read that
			// value back in as "theirs" and concatenate chottag's own CA
			// into it a SECOND time — on every single cmux launch, not
			// just a coincidental repeat — so it is reused as-is instead.
			nodeCA = theirsPath
		} else {
			theirs, err := os.ReadFile(theirsPath)
			if err != nil {
				fmt.Fprintln(stderr, "chottag: could not read NODE_EXTRA_CA_CERTS", theirsPath+":", err)
				return exit.Error
			}
			ours, err := os.ReadFile(nodeCA)
			if err != nil {
				fmt.Fprintln(stderr, "chottag:", err)
				return exit.Error
			}
			var bundle bytes.Buffer
			writeNewlineTerminated(&bundle, theirs)
			writeNewlineTerminated(&bundle, ours)
			bundlePath := filepath.Join(caDir, "bundle.pem")
			if err := fsutil.WriteFileAtomic(bundlePath, bundle.Bytes(), 0o644); err != nil {
				fmt.Fprintln(stderr, "chottag:", err)
				return exit.Error
			}
			nodeCA = bundlePath
		}
	}
	childEnv = setEnv(childEnv, "NODE_EXTRA_CA_CERTS", nodeCA)

	// registerSession is §4.3 step 7's registration, before whichever exec
	// below actually runs: that exec replaces this process's image, so this
	// same pid BECOMES the `claude` process — registering before it, not
	// after (which would never run), is what makes the entry correct. A
	// failure is reported, not fatal — refusing to launch `claude` over a
	// bookkeeping file is worse than a missing registry entry. Registry.Add
	// (over)writes the one file named by this pid, so calling this more
	// than once in the same Run (the hand-off exec below, AND the
	// fall-through exec further down) is idempotent: never a second entry,
	// just the same one confirmed again (F243-R2).
	registerSession := func() {
		if reg, err := session.Open(filepath.Join(home, "run")); err != nil {
			fmt.Fprintln(stderr, "chottag: could not open the session registry:", err)
		} else if err := reg.Add(os.Getpid(), port); err != nil {
			fmt.Fprintln(stderr, "chottag: could not register the session:", err)
		}
	}

	// The cmux hand-off (F243) happens HERE, not right after the escape
	// hatch above — deliberately late, with childEnv (HTTPS_PROXY and
	// NODE_EXTRA_CA_CERTS both already set), never the original env (fix
	// round 1, F243-R1). cmux's own wrapper does not reliably come back to
	// chottag as "the real claude": its "Claude Binary Path" setting
	// (CMUX_CUSTOM_CLAUDE_PATH), when the user has set one, wins over the
	// PATH walk chottag's own rc block relies on, and cmuxHandoffTarget
	// already refuses to hand off when that setting bypasses chottag (its
	// own doc comment, point 6). But nothing stops a user from pointing it
	// straight at chottag itself, or leaving it unset, in which case the
	// wrapper DOES come back — and whichever `claude` it ends up running,
	// this session must already be proxied, or it silently loses chottag
	// entirely rather than merely losing cmux's hooks (the bug an early
	// hand-off, carrying only the bare env, would still have). A failure
	// here is reported, not fatal: the session must not fail to start just
	// because cmux's own shim is broken, so it falls back to registering
	// and exec'ing the real claude in this same pass, exactly as if
	// cmuxTarget had been "" all along.
	if cmuxTarget != "" {
		// Registered here too (F243-R2), not only in the fall-through
		// below: exec keeps this same pid, so if cmux's wrapper resolves
		// some OTHER claude than chottag's shim — a case cmuxHandoffTarget
		// did not anticipate — the session is still registered, rather than
		// lost entirely because this process image is about to become
		// something else.
		registerSession()
		if err := execFn(cmuxTarget, args, setEnv(childEnv, cmuxHandoffEnvVar, "1")); err != nil {
			fmt.Fprintf(stderr, "chottag: could not hand off to cmux's claude wrapper (%v); starting claude without it\n", err)
		} else {
			return exit.OK
		}
	}
	// Dropped unconditionally (a no-op when absent): the marker must never
	// reach the real Claude Code, or a `claude` started from inside that
	// session would see it already set and skip its own hand-off. This is
	// the SECOND pass's own childEnv (cmuxTarget == "" because point 3 of
	// cmuxHandoffTarget's doc comment already matched the marker), or the
	// hand-off's own failed attempt just above falling back in this pass.
	childEnv = dropEnv(childEnv, cmuxHandoffEnvVar)

	registerSession()

	if err := execFn(real, args, childEnv); err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	return exit.OK
}

// couldNotConfirmMsg is Run's fatal message when nothing ever proved it is
// chottag: either a fresh spawn never answered (spawnErr names why starting
// it automatically failed, when that was tried), or, more rarely, a daemon
// that had just confirmed liveness (ProbeHealth) stopped answering by the
// time the very next probe — VerifyHealth's, carrying a nonce — ran.
func couldNotConfirmMsg(port int, spawnErr error) string {
	msg := fmt.Sprintf("chottag: could not confirm the chottag daemon on port %d; run `chottag daemon run` to see the real startup error", port)
	if spawnErr != nil {
		msg += fmt.Sprintf(" (starting it automatically failed: %v)", spawnErr)
	}
	return msg
}

// nodeCAAlreadyOurs reports whether theirsPath already names a file directly
// inside caDir: chottag's own ca.pem, or a bundle.pem this same home already
// wrote. See its one call site's own comment (F243-R2) for why this matters:
// without it, a cmux hand-off's second pass — which inherits pass 1's own
// already-merged NODE_EXTRA_CA_CERTS unchanged — would merge it again.
// realPath resolves symlinks before comparing, the same technique isSelf
// uses, so a symlinked caDir or a symlinked theirsPath still compares
// correctly.
func nodeCAAlreadyOurs(theirsPath, caDir string) bool {
	return filepath.Dir(realPath(theirsPath)) == realPath(caDir)
}

// writeNewlineTerminated appends b to buf, adding a trailing newline when b
// doesn't already end in one, so concatenating two certs never fuses the
// last line of one with the first line of the next.
func writeNewlineTerminated(buf *bytes.Buffer, b []byte) {
	buf.Write(b)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		buf.WriteByte('\n')
	}
}

// envGet returns key's value in env (a KEY=VALUE slice, e.g. Run's env
// parameter), or "" if key is absent.
func envGet(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
}

// setEnv returns a copy of env with key set to val, replacing any existing
// entry for key in place or appending one if key was absent.
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, len(env), len(env)+1)
	copy(out, env)
	for i, kv := range out {
		if strings.HasPrefix(kv, prefix) {
			out[i] = prefix + val
			return out
		}
	}
	return append(out, prefix+val)
}
