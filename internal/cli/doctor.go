package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/doctor"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const doctorUsage = "usage: chottag doctor [--fix]"

// doctorResult is `doctor --json`'s fields when no problem is left (M2
// spec §2.3). With a problem left, the same fields travel in the error's
// details.
type doctorResult struct {
	Checks   []doctor.Row `json:"checks"`
	Problems int          `json:"problems"`
}

// doctorNet holds the three doctor seams that reach beyond the filesystem:
// the health probe, the health-version probe (public release design §2.4)
// and the free-port probe (a real bind). TestMain (invariants_test.go) arms
// panicking defaults through SetDoctorNetForTest, so no doctor test probes
// or binds for real. Doctor never starts or stops a daemon (controller
// ruling D13): a daemon that is simply not running is the normal state
// between launches, and the shim starts one on demand.
var doctorNet = struct {
	probe   func(port int) bool
	version func(port int) (bool, string)
	listen  func(addr string) (io.Closer, error)
}{
	probe:   func(port int) bool { ok, _ := shim.ProbeHealth(port); return ok },
	version: func(port int) (bool, string) { ok, h := shim.ProbeHealth(port); return ok, h.Version },
	listen:  func(addr string) (io.Closer, error) { return net.Listen("tcp", addr) },
}

// SetDoctorNetForTest swaps doctor's three seams and returns a func that
// restores whatever was installed at the moment of the call. It never
// restores production's funcs by name, so TestMain's panicking defaults
// stay armed (F130). It lives in a non-test file for the same reason
// SetSignalForTest does (F103).
func SetDoctorNetForTest(probe func(port int) bool, version func(port int) (bool, string), listen func(addr string) (io.Closer, error)) (restore func()) {
	orig := doctorNet
	doctorNet.probe, doctorNet.version, doctorNet.listen = probe, version, listen
	return func() { doctorNet = orig }
}

// daemonIdentity is doctor's and status's shared view of the health
// challenge (F221): the same VerifyHealth the shim uses before handing
// claude the secret, so there is one way to decide a daemon is ours
// (F103). "unknown" means a daemon answered but ca/proxy.secret itself
// could not be read (a permission problem, or missing) — VerifyHealth
// needs the secret to check the proof, so that case cannot be verified at
// all, and is left for the proxy-secret row to explain.
//
// A VerifyHealth verdict of legacy is reported as such only when
// shim.TrustLegacy also says so (Ruling 30, T11 fix round 1 finding 3):
// shim.Run refuses to start claude behind a proof-less listener unless
// THIS home's own daemon.lock is held by the exact pid that answered
// health, so doctor and status must never call the very same listener
// "legacy" — that would tell the user to run `chottag daemon restart`
// when the real problem is a squatter on the port (L4). Anything
// TrustLegacy does not trust is reported as a mismatch instead, the same
// verdict shim.Run's own refusal amounts to.
func daemonIdentity(h string, port int) string {
	s, err := proxyauth.Load(h)
	if err != nil {
		if ok, _ := shim.ProbeHealth(port); ok {
			return "unknown"
		}
		return string(shim.IdentityNone)
	}
	id, vh := shim.VerifyHealth(port, s, Version)
	if id == shim.IdentityLegacy {
		if trusted, _, _ := shim.TrustLegacy(h, vh.PID); !trusted {
			return string(shim.IdentityMismatch)
		}
	}
	return string(id)
}

// doctorIdentity is what newDoctorEnv wires into Env.DaemonIdentity. A var
// so TestMain (invariants_test.go) can arm a SAFE DEFAULT ("none", Ruling
// 24) rather than a panic: it is read-only, and — unlike doctorNet's
// probe, which every doctor test must stub — most existing doctor tests
// never touch it at all. A test that cares stubs it with
// SetDoctorIdentityForTest.
var doctorIdentity = daemonIdentity

// SetDoctorIdentityForTest swaps doctorIdentity and returns a func that
// restores whatever was installed at the moment of the call (F130), like
// SetDoctorNetForTest.
func SetDoctorIdentityForTest(fn func(h string, port int) string) (restore func()) {
	orig := doctorIdentity
	doctorIdentity = fn
	return func() { doctorIdentity = orig }
}

// defaultDoctorTempDirs is os.TempDir() plus /tmp and /private/tmp,
// deduplicated: on macOS os.TempDir() is a per-user directory under
// /var/folders, distinct from both /tmp and its /private/tmp resolution,
// and a scratch binary (`go run`, a manually built binary) can land in
// either (F176 — the user's own doctor run missed a binary under
// /private/tmp because the guard only ever checked os.TempDir()).
func defaultDoctorTempDirs() []string {
	dirs := []string{os.TempDir(), "/tmp", "/private/tmp"}
	seen := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		c := filepath.Clean(d)
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, d)
	}
	return out
}

// doctorTempDirs is what newDoctorEnv wires into doctor.Env.TempDirs (I3's
// bin-check refusal to link at a temporary binary). A var, not an inline
// call, because internal/cli's own test binary IS itself a `go test` temp
// binary living under os.TempDir() — wiring the real value here would
// trip I3's refusal on every test in this package that breaks bin/ and
// expects doctor --fix to repair it, none of which are testing I3 itself
// (internal/doctor's own tests own that). TestMain (invariants_test.go)
// disables it for the whole binary; a test in this package has no reason
// to re-enable it, since I3's own behaviour is covered where the seam it
// depends on (Env.TempDirs) is real: package doctor.
var doctorTempDirs = defaultDoctorTempDirs()

// SetDoctorTempDirsForTest overrides doctorTempDirs and returns a func
// that restores whatever was installed at the moment of the call,
// mirroring SetDoctorNetForTest. nil disables the refusal entirely, the
// same way "" used to on the old single-string seam.
func SetDoctorTempDirsForTest(dirs []string) (restore func()) {
	orig := doctorTempDirs
	doctorTempDirs = dirs
	return func() { doctorTempDirs = orig }
}

// claudeVersionTimeout and claudeVersionMaxBytes bound doctor's one
// subprocess, `claude --version` (M2c spec §5): it makes no network call
// and prints one line.
const (
	claudeVersionTimeout  = 5 * time.Second
	claudeVersionMaxBytes = 4096
)

// doctorClaudeVersionTimeout is defaultDoctorClaudeVersion's timeout, held
// in its own var (rather than passing claudeVersionTimeout inline) so a
// test can assert the production default stays pinned to exactly
// claudeVersionTimeout without invoking the seam itself.
var doctorClaudeVersionTimeout = claudeVersionTimeout

// defaultDoctorClaudeVersion is doctorClaudeVersion's production value:
// what newDoctorEnv reaches whenever nothing has stubbed doctorClaudeVersion.
// Named, not an inline closure, so a test can restore exactly this value
// with SetDoctorClaudeVersionForTest and assert newDoctorEnv's wiring
// really reaches it.
func defaultDoctorClaudeVersion(bin string) (string, error) {
	return runClaudeVersion(bin, doctorClaudeVersionTimeout)
}

// doctorClaudeVersion is what newDoctorEnv wires into Env.ClaudeVersion.
// A var so TestMain (invariants_test.go) can arm a panicking default: no
// test in this package ever runs the real claude.
var doctorClaudeVersion = defaultDoctorClaudeVersion

// SetDoctorClaudeVersionForTest swaps doctorClaudeVersion and returns a
// func that restores whatever was installed at the moment of the call
// (F130), like SetDoctorNetForTest.
func SetDoctorClaudeVersionForTest(fn func(bin string) (string, error)) (restore func()) {
	orig := doctorClaudeVersion
	doctorClaudeVersion = fn
	return func() { doctorClaudeVersion = orig }
}

// runClaudeVersion runs `bin --version` and returns at most
// claudeVersionMaxBytes of its stdout. Stdin and stderr are nil
// (/dev/null). WaitDelay bounds the wait for a child that keeps stdout
// open after the kill.
func runClaudeVersion(bin string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// bin is doctor's row 8 resolved real claude (Env.ResolveClaude), the
	// same trust boundary as refresh.Claude and runClaudeAuth's bin: an
	// operator-installed binary path, never request- or network-derived.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, bin, "--version")
	out := &cappedBuffer{max: claudeVersionMaxBytes}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s --version did not finish within %s", bin, timeout)
	}
	if err != nil {
		return "", err
	}
	return string(out.b), nil
}

// cappedBuffer keeps the first max bytes written and discards the rest,
// while reporting every write as complete, so the child never blocks on a
// full pipe.
type cappedBuffer struct {
	max int
	b   []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// newDoctorEnv is the real doctor.Env for home h. It reads the environment
// the way setup does: $HOME for the rc, $SHELL, and this process's PATH.
func newDoctorEnv(h string) (*doctor.Env, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// chottagHomeExport mirrors runSetup's own (setup.go): h itself when
	// CHOTTAG_HOME was set in the environment, so doctor's rc-block check
	// and `--fix` agree with what setup actually wrote (item 9) instead of
	// reporting a correctly-installed rc as a problem.
	chottagHomeExport := ""
	if os.Getenv("CHOTTAG_HOME") != "" {
		chottagHomeExport = h
	}
	return &doctor.Env{
		Home:           h,
		UserHome:       os.Getenv("HOME"),
		Shell:          os.Getenv("SHELL"),
		PATH:           os.Getenv("PATH"),
		Executable:     exe,
		TempDirs:       doctorTempDirs,
		SetupDirs:      setupDirs,
		ProvisionBin:   provisionBinSymlinks,
		SameFile:       sameFile,
		RCPathFor:      rcPathFor,
		RCBlock:        func(binDir string) string { return rcBlock(binDir, chottagHomeExport) },
		WriteRCBlock:   func(rcPath, binDir string) error { return writeRCBlock(rcPath, binDir, chottagHomeExport) },
		ResolveClaude:  shim.ResolveClaude,
		ClaudeVersion:  func(bin string) (string, error) { return doctorClaudeVersion(bin) },
		ProbeHealth:    func(port int) bool { return doctorNet.probe(port) },
		DaemonVersion:  func(port int) (bool, string) { return doctorNet.version(port) },
		DaemonIdentity: func(port int) string { return doctorIdentity(h, port) },
		SelfVersion:    Version,
		Inspect:        func() (daemonlock.Status, error) { return daemonlock.Inspect(h) },
		LiveSessions: func() (int, error) {
			ss, err := liveSessions(h)
			return len(ss), err
		},
		Listen: func(addr string) (io.Closer, error) { return doctorNet.listen(addr) },
		Now:    timeNow,
	}, nil
}

// runDoctor is `chottag doctor [--fix]` (M2 spec §2). Without --fix it
// writes nothing. It exits:
//   - 0 when no row is a problem;
//   - 3 when one remains (D2);
//   - 1 for an internal error: an unreadable state.json, or a check that
//     panicked.
func runDoctor(args []string, r *reporter) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	fix := fs.Bool("fix", false, "repair what doctor can repair safely")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage(doctorUsage)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	st, err := store.Store{Dir: h}.Load()
	if err != nil {
		return r.FailErr(err)
	}
	env, err := newDoctorEnv(h)
	if err != nil {
		return r.FailErr(err)
	}
	rows, runErr := doctor.Run(env, doctor.All(accountNames(st)), *fix)
	renderDoctor(r.Stdout(), rows)
	if runErr != nil {
		return r.Fail(exit.Error, codeInternal, "doctor: internal error: "+runErr.Error(), map[string]any{"checks": rows})
	}
	problems := 0
	for _, row := range rows {
		if row.Status == doctor.StatusProblem {
			problems++
		}
	}
	if problems == 0 {
		return r.OK(doctorResult{Checks: rows, Problems: 0})
	}
	return r.Fail(exit.UserAction, codeDoctorProblems, fmt.Sprintf("%d problem(s)", problems), map[string]any{"checks": rows, "problems": problems})
}

// renderDoctor writes one `status  id  detail` row per check, then an
// indented `→ hint` when there is one (M2 spec §2.3). Hint lines have no
// columns, so the widths are computed here rather than by tabwriter.
func renderDoctor(out io.Writer, rows []doctor.Row) {
	sw, iw := 0, 0
	for _, row := range rows {
		sw, iw = max(sw, len(row.Status)), max(iw, len(row.ID))
	}
	indent := strings.Repeat(" ", sw+2+iw+2)
	for _, row := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %s", sw, row.Status, iw, row.ID, row.Detail)
		fmt.Fprintln(out, strings.TrimRight(line, " "))
		if row.Hint != "" {
			fmt.Fprintf(out, "%s→ %s\n", indent, row.Hint)
		}
	}
}
