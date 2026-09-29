package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/store"
)

var testNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// testPort is state.json's port in every test install. It is never bound:
// Listen is a fake.
const testPort = 47850

// testSelfVersion is Env.SelfVersion in every test install, and
// testInstall's daemonVersion defaults to the same value, so
// daemonVersionCheck reads ok on a healthy install without every existing
// test having to know about it. A test wanting a mismatch sets
// ti.daemonVersion (while healthy) directly.
const testSelfVersion = "0.3.1"

var testSetupDirs = []string{"ca", "bin", "accounts", "run", "cache"}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// testInstall is a healthy install over temp dirs: the tree, a real CA, bin
// symlinks to a fake chottag, an rc file holding the block, account A
// (serving and remote), a cached real claude, and a fake daemon that is
// running and healthy. The fields after env steer the fakes.
type testInstall struct {
	t                               *testing.T
	env                             *Env
	home, userHome, exe, claude, rc string
	healthy, running                bool
	daemonVersion                   string // what DaemonVersion reports while healthy (public release design §2.4)
	daemonIdentity                  string // what DaemonIdentity reports (F221, part 1 T11); "verified" by default so a healthy install's daemon-identity row reads ok, matching daemonVersion's own default
	inspectErr                      error
	live                            int
	held                            map[string]bool // addresses Listen refuses
	listens                         []string
	claudeVersion                   string   // what the ClaudeVersion seam prints
	claudeVersionErr                error    // what it returns
	versionCalls                    []string // the paths it was asked about
}

func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	must(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return p
}

func testProvisionBin(exe, binDir string) error {
	for _, name := range []string{"chottag", "claude"} {
		link := filepath.Join(binDir, name)
		if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Symlink(exe, link); err != nil {
			return err
		}
	}
	return nil
}

func testSameFile(a, b string) (bool, error) {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false, nil
	}
	return os.SameFile(ai, bi), nil
}

func testRCPathFor(shell, userHome string) string {
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(userHome, ".zshrc")
	case "bash":
		return filepath.Join(userHome, ".bashrc")
	}
	return ""
}

// testRCBlock is byte-identical to internal/cli's rcBlock when CHOTTAG_HOME
// is unset — the only shape this package's own tests need; tests that care
// about the CHOTTAG_HOME export line (e.g.
// TestRCBlockUnknownShellCarriesEveryExportLine) set Env.RCBlock directly
// instead of going through this fake.
func testRCBlock(binDir string) string {
	return "# >>> chottag >>>\nexport PATH=\"" + binDir + ":$PATH\"\n# <<< chottag <<<\n"
}

func testWriteRCBlock(rcPath, binDir string) error {
	target := rcPath
	if resolved, err := filepath.EvalSymlinks(rcPath); err == nil {
		target = resolved
	}
	b, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(target, append(b, []byte("\n"+testRCBlock(binDir))...), 0o644)
}

func newTestInstall(t *testing.T) *testInstall {
	t.Helper()
	ti := &testInstall{t: t, home: t.TempDir(), userHome: t.TempDir(), healthy: true, running: true, held: map[string]bool{}, claudeVersion: "2.1.282 (Claude Code)\n", daemonVersion: testSelfVersion, daemonIdentity: "verified"}
	for _, d := range testSetupDirs {
		must(t, os.MkdirAll(filepath.Join(ti.home, d), 0o700))
	}
	if _, err := ca.LoadOrCreate(filepath.Join(ti.home, "ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := proxyauth.LoadOrCreate(ti.home); err != nil {
		t.Fatal(err)
	}
	ti.exe = writeExecutable(t, t.TempDir(), "chottag")
	binDir := filepath.Join(ti.home, "bin")
	must(t, testProvisionBin(ti.exe, binDir))
	claudeDir := t.TempDir()
	ti.claude = writeExecutable(t, claudeDir, "claude")
	ti.rc = filepath.Join(ti.userHome, ".zshrc")
	must(t, os.WriteFile(ti.rc, []byte("# the user's own line\n\n"+testRCBlock(binDir)), 0o644))
	if _, err := (store.Store{Dir: ti.home}).Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(ti.home, "accounts", "A")}); err != nil {
			return err
		}
		st.Port, st.RealClaude = testPort, ti.claude
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ti.env = &Env{
		Home:          ti.home,
		UserHome:      ti.userHome,
		Shell:         "/bin/zsh",
		PATH:          binDir + string(os.PathListSeparator) + claudeDir,
		Executable:    ti.exe,
		SetupDirs:     testSetupDirs,
		ProvisionBin:  testProvisionBin,
		SameFile:      testSameFile,
		RCPathFor:     testRCPathFor,
		RCBlock:       testRCBlock,
		WriteRCBlock:  testWriteRCBlock,
		ResolveClaude: shim.ResolveClaude,
		ClaudeVersion: func(bin string) (string, error) {
			ti.versionCalls = append(ti.versionCalls, bin)
			return ti.claudeVersion, ti.claudeVersionErr
		},
		ProbeHealth:    func(int) bool { return ti.healthy },
		DaemonVersion:  func(int) (bool, string) { return ti.healthy, ti.daemonVersion },
		DaemonIdentity: func(int) string { return ti.daemonIdentity },
		SelfVersion:    testSelfVersion,
		Inspect: func() (daemonlock.Status, error) {
			if ti.inspectErr != nil {
				return daemonlock.Status{Running: true}, ti.inspectErr
			}
			if ti.running {
				return daemonlock.Status{Running: true, Record: daemonlock.Record{PID: 4242}}, nil
			}
			return daemonlock.Status{}, nil
		},
		LiveSessions: func() (int, error) { return ti.live, nil },
		Listen: func(addr string) (io.Closer, error) {
			ti.listens = append(ti.listens, addr)
			if ti.held[addr] {
				return nil, errors.New("bind: address already in use")
			}
			return nopCloser{}, nil
		},
		Now: func() time.Time { return testNow },
	}
	return ti
}

func (ti *testInstall) update(fn func(st *store.State) error) {
	ti.t.Helper()
	if _, err := ti.env.Store().Update(fn); err != nil {
		ti.t.Fatal(err)
	}
}

// snapshot records every path under each labelled root: type and mode, a
// symlink's target, a regular file's content hash and mtime. Two snapshots
// taken around code that must not write compare equal. A missing root
// records nothing.
func snapshot(t *testing.T, roots map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for label, root := range roots {
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
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

func (ti *testInstall) snapshot() map[string]string {
	return snapshot(ti.t, map[string]string{"home": ti.home, "user": ti.userHome})
}

// assertChangedOnly fails for every key that differs between before and
// after (appeared, vanished or changed) and is not in allowed.
func assertChangedOnly(t *testing.T, before, after map[string]string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
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
	for _, k := range sorted {
		if before[k] != after[k] && !ok[k] {
			t.Errorf("%s changed: %q -> %q", k, before[k], after[k])
		}
	}
}

func rowByID(t *testing.T, rows []Row, id string) Row {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", id, rows)
	return Row{}
}

func mustRun(t *testing.T, env *Env, checks []Check, fix bool) []Row {
	t.Helper()
	rows, err := Run(env, checks, fix)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rows
}

// breakThenRepair is spec §6's test for one fixable check. On a healthy
// install, breakIt breaks exactly one thing. Then:
//   - doctor without --fix reports it as a problem with the --fix hint, and
//     writes nothing;
//   - doctor --fix reports it fixed and every other row ok or info;
//   - doctor again reports it ok;
//   - only the allowed snapshot keys changed since the break.
func breakThenRepair(t *testing.T, ti *testInstall, checks []Check, id string, breakIt func(), allowed ...string) {
	t.Helper()
	breakIt()
	broken := ti.snapshot()
	if r := rowByID(t, mustRun(t, ti.env, checks, false), id); r.Status != StatusProblem || r.Hint != FixHint {
		t.Fatalf("after the break: %+v, want a problem with hint %q", r, FixHint)
	}
	assertChangedOnly(t, broken, ti.snapshot())
	for _, r := range mustRun(t, ti.env, checks, true) {
		switch {
		case r.ID == id && r.Status != StatusFixed:
			t.Errorf("doctor --fix: %+v, want fixed", r)
		case r.ID != id && r.Status != StatusOK && r.Status != StatusInfo:
			t.Errorf("doctor --fix: %+v, want ok or info", r)
		}
	}
	if r := rowByID(t, mustRun(t, ti.env, checks, false), id); r.Status != StatusOK {
		t.Errorf("after the repair: %+v, want ok", r)
	}
	assertChangedOnly(t, broken, ti.snapshot(), allowed...)
}

// TestSnapshotSeesEveryKindOfChange keeps the no-write assertions honest.
func TestSnapshotSeesEveryKindOfChange(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	must(t, os.WriteFile(f, []byte("abc"), 0o600))
	must(t, os.Symlink(f, filepath.Join(dir, "l")))
	roots := map[string]string{"d": dir}
	base := snapshot(t, roots)
	for name, change := range map[string]func(){
		"content": func() { must(t, os.WriteFile(f, []byte("abd"), 0o600)) },
		"mtime":   func() { must(t, os.Chtimes(f, testNow, testNow)) },
		"mode":    func() { must(t, os.Chmod(f, 0o644)) },
		"new":     func() { must(t, os.WriteFile(filepath.Join(dir, "g"), nil, 0o600)) },
		"link":    func() { must(t, os.Remove(filepath.Join(dir, "l"))); must(t, os.Symlink(dir, filepath.Join(dir, "l"))) },
	} {
		change()
		after := snapshot(t, roots)
		same := len(after) == len(base)
		for k, v := range base {
			same = same && after[k] == v
		}
		if same {
			t.Errorf("%s: the snapshot did not change", name)
		}
		base = after
	}
}
