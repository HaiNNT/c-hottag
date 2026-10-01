package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

func listBackups(t *testing.T, h, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(h, "backups"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix+".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestBackupFileCopiesWithModesAndStamp(t *testing.T) {
	h := t.TempDir()
	src := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(src, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := backupNow
	backupNow = func() time.Time { return time.Date(2026, 10, 2, 15, 4, 5, 0, time.UTC) }
	t.Cleanup(func() { backupNow = orig })

	p, err := backupFile(h, src, "state.json")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h, "backups", "state.json.20261002T150405Z"); p != want {
		t.Errorf("path = %q, want %q", p, want)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"a":1}` {
		t.Errorf("content = %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(h, "backups")); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", fi.Mode().Perm())
	}
}

func TestBackupFileMissingSourceIsNotAnError(t *testing.T) {
	h := t.TempDir()
	p, err := backupFile(h, filepath.Join(h, "nope"), "state.json")
	if p != "" || err != nil {
		t.Errorf("got %q, %v; want \"\", nil", p, err)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); err == nil {
		t.Errorf("backups dir created for a missing source")
	}
}

func TestBackupFileKeepsNewestFivePerName(t *testing.T) {
	h := t.TempDir()
	src := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := backupNow
	t.Cleanup(func() { backupNow = orig })
	if err := os.MkdirAll(filepath.Join(h, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "backups", "state.json.extra.20200101T000000Z"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		backupNow = func() time.Time { return ts }
		if _, err := backupFile(h, src, "state.json"); err != nil {
			t.Fatal(err)
		}
	}
	// A different name, and a name sharing a prefix, are untouched.
	if _, err := backupFile(h, src, "zshrc"); err != nil {
		t.Fatal(err)
	}
	got := listBackups(t, h, "state.json")
	if len(got) != 6 { // 5 plus the "extra" lookalike
		t.Fatalf("state.json backups = %v, want 5 + the unrelated lookalike", got)
	}
	for _, old := range []string{"20261001T000000Z", "20261001T000100Z", "20261001T000200Z"} {
		if _, err := os.Stat(filepath.Join(h, "backups", "state.json."+old)); err == nil {
			t.Errorf("oldest backup %s survived pruning", old)
		}
	}
	if len(listBackups(t, h, "zshrc")) != 1 {
		t.Errorf("zshrc backup missing")
	}
}

func TestUpdateBacksUpStateJSONBeforeSetup(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	stateBefore, err := os.ReadFile(filepath.Join(h, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	sawBackup := false
	child := func(_ context.Context, bin string, args ...string) error {
		if len(args) == 1 && args[0] == "setup" {
			b := listBackups(t, h, "state.json")
			sawBackup = len(b) == 1
			if sawBackup {
				got, _ := os.ReadFile(filepath.Join(h, "backups", b[0]))
				if !bytes.Equal(got, stateBefore) {
					t.Errorf("backup content differs from state.json")
				}
			}
		}
		return nil
	}
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)

	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(true, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if !sawBackup {
		t.Errorf("state.json was not backed up before setup ran")
	}
	var doc struct {
		Backups []string `json:"backups"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Backups) != 1 || !strings.HasPrefix(doc.Backups[0], filepath.Join(h, "backups", "state.json.")) {
		t.Errorf("backups = %v", doc.Backups)
	}
}

func TestUpdateTextNamesTheBackup(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)
	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "backed up state.json to "+filepath.Join(h, "backups", "state.json.")) {
		t.Errorf("stdout = %q, want a backup line", out.String())
	}
}

func TestUpdateBackupFailureInstallsNothing(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	// A file where the backups dir must go makes the backup fail.
	if err := os.WriteFile(filepath.Join(h, "backups"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, calls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1; stdout=%q", code, out.String())
	}
	if !strings.Contains(out.String(), string(codeUpdateFailed)) || !strings.Contains(out.String(), "back up") {
		t.Errorf("stdout = %q, want update_failed naming the backup", out.String())
	}
	if len(*calls) != 0 {
		t.Errorf("child calls = %v, want none", *calls)
	}
	if _, ok, _ := readInstallRecord(h); ok {
		t.Errorf("install.json written despite the backup failure")
	}
}

func TestUpdateCheckMakesNoBackup(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, h, false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--check"}, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); err == nil {
		t.Errorf("--check created backups")
	}
}

func TestWriteRCBlockBacksUpAChangedRC(t *testing.T) {
	h := t.TempDir()
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("export A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, err := writeRCBlock(h, rc, "/u/.chottag/bin", "")
	if err != nil {
		t.Fatal(err)
	}
	if p == "" || !strings.HasPrefix(p, filepath.Join(h, "backups", "zshrc.")) {
		t.Fatalf("backup path = %q", p)
	}
	if b, _ := os.ReadFile(p); string(b) != "export A=1\n" {
		t.Errorf("backup = %q, want the pre-change rc", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", fi.Mode().Perm())
	}
	// Unchanged second run: no new backup.
	p2, _, err := writeRCBlock(h, rc, "/u/.chottag/bin", "")
	if err != nil || p2 != "" {
		t.Errorf("unchanged run: %q, %v; want no backup", p2, err)
	}
	if n := len(listBackups(t, h, "zshrc")); n != 1 {
		t.Errorf("zshrc backups = %d, want 1", n)
	}
}

func TestWriteRCBlockNewFileMakesNoBackup(t *testing.T) {
	h := t.TempDir()
	rc := filepath.Join(t.TempDir(), ".zshrc")
	p, _, err := writeRCBlock(h, rc, "/u/.chottag/bin", "")
	if err != nil || p != "" {
		t.Fatalf("%q, %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); err == nil {
		t.Errorf("backups dir made for a new rc")
	}
}

func TestWriteRCBlockBacksUpTheSymlinkTarget(t *testing.T) {
	h, dir := t.TempDir(), t.TempDir()
	target := filepath.Join(dir, "dotfiles-zshrc")
	if err := os.WriteFile(target, []byte("export B=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	p, _, err := writeRCBlock(h, link, "/u/.chottag/bin", "")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "export B=2\n" {
		t.Errorf("backup = %q, want the target's content", b)
	}
	if !strings.HasPrefix(filepath.Base(p), "zshrc.") {
		t.Errorf("backup name = %q, want zshrc.<stamp>", filepath.Base(p))
	}
}

func TestWriteRCBlockBackupFailureLeavesRCUntouched(t *testing.T) {
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "backups"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("export A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeRCBlock(h, rc, "/u/.chottag/bin", ""); err == nil {
		t.Fatal("want an error")
	}
	if b, _ := os.ReadFile(rc); string(b) != "export A=1\n" {
		t.Errorf("rc changed: %q", b)
	}
}

func TestBackupFilePruneFailureIsDistinctAndKeepsTheNewBackup(t *testing.T) {
	h := t.TempDir()
	src := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(src, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := backupNow
	t.Cleanup(func() { backupNow = orig })
	// Six old backups, the oldest a non-empty directory os.Remove cannot delete.
	oldest := filepath.Join(h, "backups", "state.json.20200101T000000Z")
	if err := os.MkdirAll(filepath.Join(oldest, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 6; i++ {
		f := filepath.Join(h, "backups", "state.json.2020010"+string(rune('0'+i))+"T000000Z")
		if err := os.WriteFile(f, []byte("o"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backupNow = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
	p, err := backupFile(h, src, "state.json")
	var pe *backupPruneError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *backupPruneError", err)
	}
	if b, rerr := os.ReadFile(p); rerr != nil || string(b) != "s" {
		t.Errorf("new backup missing: %v", rerr)
	}
}

func stuckPruneSetup(t *testing.T, h string) {
	t.Helper()
	oldest := filepath.Join(h, "backups", "state.json.20200101T000000Z")
	if err := os.MkdirAll(filepath.Join(oldest, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 6; i++ {
		f := filepath.Join(h, "backups", "state.json.2020010"+string(rune('0'+i))+"T000000Z")
		if err := os.WriteFile(f, []byte("o"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdatePruneFailureWarnsAndStillInstalls(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	stuckPruneSetup(t, h)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, calls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)
	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(true, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d; stdout=%q", code, out.String())
	}
	var doc struct {
		Warnings []struct{ Code string } `json:"warnings"`
		Backups  []string                `json:"backups"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range doc.Warnings {
		found = found || w.Code == string(warnPruneFailed)
	}
	if !found || len(doc.Backups) != 1 {
		t.Errorf("doc = %+v, want a prune_failed warning and one backup", doc)
	}
	if len(*calls) == 0 {
		t.Errorf("setup never ran")
	}
}

func TestUpdateUpToDateMakesNoBackup(t *testing.T) {
	withVersion(t, "0.3.1")
	h := updateHome(t)
	if err := os.MkdirAll(filepath.Join(h, "versions", "0.3.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "versions", "0.3.1", "chottag"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); err == nil {
		t.Errorf("an up-to-date run made a backup")
	}
}

func TestUpdateChecksumFailureMakesNoBackup(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	fx.checksums = []byte(strings.Repeat("0", 64) + "  " + releaseAssetName("0.3.1") + "\n")
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(true, &out, &errb)); code != exit.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); err == nil {
		t.Errorf("a failed verification made a backup")
	}
}

func TestUpdateBackupFailureExtractsNothing(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	if err := os.WriteFile(filepath.Join(h, "backups"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)
	var out, errb bytes.Buffer
	runUpdate(nil, newReporter(true, &out, &errb))
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1")); err == nil {
		t.Errorf("versions/0.3.1 exists after a failed backup")
	}
}

func setupSandbox(t *testing.T) (h, rc string) {
	t.Helper()
	h, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", h)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")
	rc = filepath.Join(fakeHome, ".zshrc")
	if err := os.WriteFile(rc, []byte("export A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, rc
}

func TestSetupJSONReportsRCBackup(t *testing.T) {
	h, _ := setupSandbox(t)
	var out bytes.Buffer
	if code := runSetup(nil, newReporter(true, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d; %s", code, out.String())
	}
	var doc struct {
		RCBackup string `json:"rcBackup"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.RCBackup, filepath.Join(h, "backups", "zshrc.")) {
		t.Errorf("rcBackup = %q", doc.RCBackup)
	}
}

func TestSetupFailsWhenTheRCBackupFailsLeavingTheRC(t *testing.T) {
	h, rc := setupSandbox(t)
	if err := os.WriteFile(filepath.Join(h, "backups"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runSetup(nil, newReporter(true, &out, io.Discard)); code == 0 {
		t.Fatalf("setup succeeded despite a failed rc backup")
	}
	if b, _ := os.ReadFile(rc); string(b) != "export A=1\n" {
		t.Errorf("rc changed: %q", b)
	}
}

func TestSetupPruneFailureWarns(t *testing.T) {
	h, _ := setupSandbox(t)
	for i := 0; i <= 5; i++ {
		d := filepath.Join(h, "backups", "zshrc.2020010"+string(rune('0'+i))+"T000000Z")
		if err := os.MkdirAll(filepath.Join(d, "inner"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := runSetup(nil, newReporter(true, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d; %s", code, out.String())
	}
	if !strings.Contains(out.String(), string(warnPruneFailed)) {
		t.Errorf("stdout = %s, want a prune_failed warning", out.String())
	}
}
