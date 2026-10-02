package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/doctor"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// doctorPort is state.json's port in every doctor install. It is never
// bound: the port probe is fake.
const doctorPort = 47850

type doctorNop struct{}

func (doctorNop) Close() error { return nil }

// fakeDoctorNet stands in for doctor's two network seams. A "running"
// daemon is the real run/daemon.lock held in-process (TestMain treats every
// pid as alive), plus a health probe that says yes. Doctor never starts or
// stops a daemon (D13), so start/stop here only model the daemon's own
// lifecycle around a doctor run, never something doctor triggers.
type fakeDoctorNet struct {
	mu      sync.Mutex
	home    string
	healthy bool
	// version is the health version fakeDoctorNet reports while healthy
	// (public release design §2.4). installDoctorNet defaults it to
	// cli.Version so the new daemon-version check reads ok on every
	// existing doctorInstall; a test wanting a mismatch sets it directly.
	version string
	held    map[string]bool
	release func() error
}

func installDoctorNet(t *testing.T, home string) *fakeDoctorNet {
	t.Helper()
	f := &fakeDoctorNet{home: home, held: map[string]bool{}, version: Version}
	t.Cleanup(SetDoctorNetForTest(
		func(int) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.healthy },
		func(int) (bool, string) { f.mu.Lock(); defer f.mu.Unlock(); return f.healthy, f.version },
		func(addr string) (io.Closer, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.held[addr] {
				return nil, errors.New("bind: address already in use")
			}
			return doctorNop{}, nil
		},
	))
	t.Cleanup(SetDoctorClaudeVersionForTest(func(string) (string, error) { return "2.1.282 (Claude Code)\n", nil }))
	t.Cleanup(f.stop)
	return f
}

func (f *fakeDoctorNet) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release == nil {
		release, err := daemonlock.Acquire(f.home, daemonlock.Current())
		if err != nil {
			return err
		}
		f.release = release
	}
	f.healthy = true
	return nil
}

// setHeld marks addr as held (or free) by something else, under f.mu: the
// listen seam installed by installDoctorNet reads f.held from whatever
// goroutine doctor's port probe runs on, so a test must not write it
// unlocked.
func (f *fakeDoctorNet) setHeld(addr string, held bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[addr] = held
}

func (f *fakeDoctorNet) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release != nil {
		f.release()
		f.release = nil
	}
	f.healthy = false
}

// doctorInstall builds a healthy install the way a user gets one:
//   - the real setup (tree, CA, bin symlinks to this test binary,
//     ~/.zshrc);
//   - one registered account, A;
//   - a cached real claude;
//   - bin/ first on PATH;
//   - a running, healthy (fake) daemon.
func doctorInstall(t *testing.T) (chottagHome, userHome string, f *fakeDoctorNet) {
	t.Helper()
	userHome, chottagHome = t.TempDir(), t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("CHOTTAG_HOME", chottagHome)
	t.Setenv("SHELL", "/bin/zsh")
	claude := putFakeClaudeOnPath(t)
	t.Setenv("PATH", filepath.Join(chottagHome, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != exit.OK {
		t.Fatalf("setup = %d", code)
	}
	addSlotAccount(t, chottagHome, "A")
	if _, err := (store.Store{Dir: chottagHome}).Update(func(st *store.State) error {
		st.Port, st.RealClaude = doctorPort, claude
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f = installDoctorNet(t, chottagHome)
	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	return chottagHome, userHome, f
}

// doctorSnapshot records every path under each labelled root: type and
// mode, a symlink's target, a regular file's hash and mtime.
func doctorSnapshot(t *testing.T, roots map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for label, root := range roots {
		err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			key := label + ":" + filepath.ToSlash(rel)
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				out[key] = "link " + target
			case info.IsDir():
				out[key] = fmt.Sprintf("dir %v", info.Mode().Perm())
			default:
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(b)
				out[key] = fmt.Sprintf("file %v %s %d", info.Mode().Perm(), hex.EncodeToString(sum[:]), info.ModTime().UnixNano())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// doctorAssertSame fails for every key that differs, ignoring keys under
// the given prefixes.
func doctorAssertSame(t *testing.T, before, after map[string]string, ignorePrefixes ...string) {
	t.Helper()
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
next:
	for _, k := range sorted {
		for _, p := range ignorePrefixes {
			if strings.HasPrefix(k, p) {
				continue next
			}
		}
		if before[k] != after[k] {
			t.Errorf("%s changed: %q -> %q", k, before[k], after[k])
		}
	}
}

// doctorRows parses the text output's rows (hint lines start with spaces).
func doctorRows(t *testing.T, out string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, " ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			t.Fatalf("malformed row %q", line)
		}
		rows[f[1]] = f[0]
	}
	return rows
}

var doctorIDs = []string{"setup", "tree", "ca", "proxy-secret", "bin", "rc-block", "path", "roles", "identities", "real-claude", "port", "daemon", "daemon-version", "daemon-identity", "token:A", "owners", "route-drift", "limits", "version-drift", "plan-unknown"}

func TestDoctorOnAHealthyInstallExitsZero(t *testing.T) {
	doctorInstall(t)
	code, out, errs := runChottag(t, "doctor")
	if code != exit.OK {
		t.Fatalf("doctor = %d; stdout %q stderr %q", code, out, errs)
	}
	rows := doctorRows(t, out)
	for _, id := range doctorIDs {
		if s := rows[id]; s != doctor.StatusOK && s != doctor.StatusInfo {
			t.Errorf("%s = %q, want ok or info\n%s", id, s, out)
		}
	}
	if len(rows) != len(doctorIDs) {
		t.Errorf("%d rows, want %d:\n%s", len(rows), len(doctorIDs), out)
	}
}

// TestDoctorWithoutFixChangesNoFile is spec §6's no-side-effects rule. With
// one thing broken in every group, a doctor run without --fix, in text and
// in JSON, changes no file in either temp tree and starts nothing.
func TestDoctorWithoutFixChangesNoFile(t *testing.T) {
	chottagHome, userHome, f := doctorInstall(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Remove(filepath.Join(chottagHome, "bin", "claude")))
	must(os.Remove(filepath.Join(chottagHome, "accounts")))
	must(os.WriteFile(filepath.Join(userHome, ".zshrc"), []byte("# mine\n"), 0o644))
	if _, err := (store.Store{Dir: chottagHome}).Update(func(st *store.State) error {
		st.Serving, st.RealClaude = "Gone", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.stop()
	f.setHeld(net.JoinHostPort("127.0.0.1", strconv.Itoa(doctorPort)), true)
	b, err := status.Marshal(status.File{Accounts: []status.Account{{Name: "A", Token: creds.StateNeedsLogin}}})
	must(err)
	must(status.WriteBytes(status.Path(chottagHome), b))
	must(owners.Edit(filepath.Join(chottagHome, "owners.json"), func(tx *owners.Tx) error {
		tx.Reassign(router.KindArtifact, "x", "Ghost", timeNow())
		return nil
	}))

	roots := map[string]string{"home": chottagHome, "user": userHome}
	before := doctorSnapshot(t, roots)
	for _, args := range [][]string{{"doctor"}, {"--json", "doctor"}} {
		if code, out, errs := runChottag(t, args...); code != exit.UserAction {
			t.Fatalf("%v = %d; stdout %q stderr %q", args, code, out, errs)
		}
	}
	doctorAssertSame(t, before, doctorSnapshot(t, roots))
}

// TestDoctorOnAnUninstalledShimExitsZeroAndWritesNothing is F176: `chottag
// uninstall` (no --purge) removes bin/'s links and the rc block but
// leaves state.json in place, so doctor on that box must read "the shim
// is not installed", not "broken" — exit 0, and no reinstall even under
// --fix (D3: doctor never performs a first install).
func TestDoctorOnAnUninstalledShimExitsZeroAndWritesNothing(t *testing.T) {
	chottagHome, userHome, _ := doctorInstall(t)
	if code := runUninstall(nil, strings.NewReader(""), newReporter(false, io.Discard, io.Discard)); code != exit.OK {
		t.Fatalf("uninstall = %d", code)
	}

	roots := map[string]string{"home": chottagHome, "user": userHome}
	before := doctorSnapshot(t, roots)
	for _, args := range [][]string{{"doctor"}, {"doctor", "--fix"}} {
		code, out, errs := runChottag(t, args...)
		if code != exit.OK {
			t.Fatalf("%v = %d; stdout %q stderr %q", args, code, out, errs)
		}
		rows := doctorRows(t, out)
		if rows["bin"] != doctor.StatusInfo || rows["rc-block"] != doctor.StatusInfo {
			t.Fatalf("%v rows = %v, want bin and rc-block info\n%s", args, rows, out)
		}
	}
	doctorAssertSame(t, before, doctorSnapshot(t, roots))
}

// TestDoctorFixRepairsABrokenInstallEndToEnd is the CLI's break-then-repair
// test, through the real provisionBinSymlinks and roles fix. D13: doctor
// never starts or stops a daemon, so the fake daemon stays running and
// healthy throughout, and its row never changes.
func TestDoctorFixRepairsABrokenInstallEndToEnd(t *testing.T) {
	chottagHome, userHome, _ := doctorInstall(t)
	roots := map[string]string{"home": chottagHome, "user": userHome}
	healthy := doctorSnapshot(t, roots)
	healthyState, err := (store.Store{Dir: chottagHome}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(chottagHome, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := (store.Store{Dir: chottagHome}).Update(func(st *store.State) error {
		st.Serving = "Gone"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runChottag(t, "doctor")
	rows := doctorRows(t, out)
	if code != exit.UserAction || rows["bin"] != "problem" || rows["roles"] != "problem" || rows["daemon"] != "ok" {
		t.Fatalf("doctor = %d:\n%s", code, out)
	}
	code, out, errs := runChottag(t, "doctor", "--fix")
	rows = doctorRows(t, out)
	if code != exit.OK || rows["bin"] != "fixed" || rows["roles"] != "fixed" || rows["daemon"] != "ok" {
		t.Fatalf("doctor --fix = %d:\n%s\nstderr %q", code, out, errs)
	}
	repairedState, err := (store.Store{Dir: chottagHome}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if repairedState.Serving != "A" {
		t.Fatalf("Serving = %q after the repair, want A (the first account in registration order)", repairedState.Serving)
	}
	if !reflect.DeepEqual(repairedState, healthyState) {
		t.Fatalf("state.json after the repair = %+v, want the healthy install's state back: %+v", repairedState, healthyState)
	}
	code, out, _ = runChottag(t, "doctor")
	if code != exit.OK || strings.Contains(out, "fixed") || strings.Contains(out, "problem") {
		t.Fatalf("doctor after the repair = %d:\n%s", code, out)
	}
	// home:state.json's mtime changes on every rewrite even when its
	// content re-marshals the same; home:run/ holds the fake daemon's lock
	// record, rewritten by each start.
	doctorAssertSame(t, healthy, doctorSnapshot(t, roots), "home:state.json", "home:run/")
}

// TestDoctorFixKeepsASymlinkedZshrcALink is Review Focus 2, through the
// real writeRCBlock.
func TestDoctorFixKeepsASymlinkedZshrcALink(t *testing.T) {
	chottagHome, userHome, _ := doctorInstall(t)
	rc := filepath.Join(userHome, ".zshrc")
	target := filepath.Join(userHome, "dotfiles", "zshrc")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, rc); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runChottag(t, "doctor"); code != exit.UserAction || doctorRows(t, out)["rc-block"] != "problem" {
		t.Fatalf("doctor = %d:\n%s", code, out)
	}
	if code, out, errs := runChottag(t, "doctor", "--fix"); code != exit.OK {
		t.Fatalf("doctor --fix = %d:\n%s\nstderr %q", code, out, errs)
	}
	info, err := os.Lstat(rc)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("~/.zshrc is no longer a symlink: %v %v", info, err)
	}
	if got, _ := os.Readlink(rc); got != target {
		t.Fatalf("link now points at %q", got)
	}
	b, err := os.ReadFile(target)
	if err != nil || !strings.HasPrefix(string(b), "# mine\n") || !strings.Contains(string(b), rcBlock(filepath.Join(chottagHome, "bin"), chottagHome)) {
		t.Fatalf("target = %q, %v", b, err)
	}
	if ti, _ := os.Stat(target); ti.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %v, want 0600 kept", ti.Mode().Perm())
	}
}

// TestDoctorJSONProblemDocument is Review Focus 5.
func TestDoctorJSONProblemDocument(t *testing.T) {
	chottagHome, _, _ := doctorInstall(t)
	if err := os.Remove(filepath.Join(chottagHome, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runChottag(t, "--json", "doctor")
	if code != exit.UserAction {
		t.Fatalf("exit = %d, want 3; stderr %q", code, errs)
	}
	doc := decodeOneDocument(t, out)
	assertDocumentHeader(t, doc, code)
	if _, ok := doc["checks"]; ok {
		t.Fatal("a failure document carries top-level checks; the rows travel in error.checks")
	}
	e := docError(t, doc)
	if e["code"] != string(codeDoctorProblems) || e["problems"] != float64(1) || e["message"] != "1 problem(s)" {
		t.Fatalf("error = %v", e)
	}
	checks, _ := e["checks"].([]any)
	if len(checks) != len(doctorIDs) {
		t.Fatalf("%d checks, want %d", len(checks), len(doctorIDs))
	}
	for i, c := range checks {
		m, _ := c.(map[string]any)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := []string{"detail", "hint", "id", "status"}
		if m["id"] == "version-drift" {
			want = []string{"detail", "hint", "id", "installed", "status"} // M2c row 15: installed, and lastTraced once a trace exists
		}
		if !slices.Equal(keys, want) || m["id"] != doctorIDs[i] {
			t.Errorf("check %d = %v, want id %s and exactly %v", i, m, doctorIDs[i], want)
		}
		if m["id"] == "bin" && (m["status"] != "problem" || m["hint"] != "chottag doctor --fix") {
			t.Errorf("bin = %v", m)
		}
	}
	if errs != "chottag: 1 problem(s)\n" {
		t.Fatalf("stderr = %q", errs)
	}
}

func TestDoctorUnreadableStateIsAnInternalError(t *testing.T) {
	chottagHome, _, _ := doctorInstall(t)
	if err := os.WriteFile(filepath.Join(chottagHome, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runChottag(t, "--json", "doctor")
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1 (D2)", code)
	}
	if got := docError(t, decodeOneDocument(t, out))["code"]; got != string(codeInternal) {
		t.Fatalf("code = %v, want internal", got)
	}
}

func TestDoctorRejectsAStrayArgument(t *testing.T) {
	doctorInstall(t)
	for _, args := range [][]string{{"doctor", "now"}, {"doctor", "--fixx"}} {
		if code, _, _ := runChottag(t, args...); code != exit.Usage {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
}

// TestDoctorNetSeamsPanicUnlessStubbed proves TestMain's defaults stay
// armed: an unstubbed doctor run is an internal error naming the seam,
// and nothing is really probed.
func TestDoctorNetSeamsPanicUnlessStubbed(t *testing.T) {
	userHome, chottagHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("CHOTTAG_HOME", chottagHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != exit.OK {
		t.Fatalf("setup = %d", code)
	}
	addSlotAccount(t, chottagHome, "A")
	code, _, errs := runChottag(t, "doctor")
	if code != exit.Error || !strings.Contains(errs, "SetDoctorNetForTest") {
		t.Fatalf("doctor = %d, stderr %q; want exit 1 naming the unstubbed seam", code, errs)
	}
}

func TestRenderDoctorAlignsRowsAndIndentsHints(t *testing.T) {
	var b strings.Builder
	renderDoctor(&b, []doctor.Row{
		{ID: "setup", Status: "ok", Detail: "/h"},
		{ID: "rc-block", Status: "problem", Detail: "no block", Hint: "chottag doctor --fix"},
		{ID: "path", Status: "info"},
	})
	want := "ok       setup     /h\n" +
		"problem  rc-block  no block\n" +
		strings.Repeat(" ", 19) + "→ chottag doctor --fix\n" +
		"info     path\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}
