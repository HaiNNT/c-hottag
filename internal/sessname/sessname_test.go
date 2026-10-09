package sessname

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const sid = "3f2a1b4c-0d5e-4f60-8a7b-9c8d7e6f5a4b"

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBranchBase(t *testing.T) {
	root := t.TempDir()
	mk := func(name, head string) string {
		d := filepath.Join(root, name)
		write(t, filepath.Join(d, ".git", "HEAD"), head)
		return d
	}
	if got := BranchBase(mk("a", "ref: refs/heads/feat/m12-owner-discovery\n")); got != "m12-owner-discovery" {
		t.Errorf("branch: %q", got)
	}
	for _, b := range []string{"main", "master"} {
		if got := BranchBase(mk("b-"+b, "ref: refs/heads/"+b+"\n")); got != "" {
			t.Errorf("%s: %q", b, got)
		}
	}
	if got := BranchBase(mk("det", "0123456789abcdef0123456789abcdef01234567\n")); got != "" {
		t.Errorf("detached: %q", got)
	}
	if got := BranchBase(filepath.Join(root, "nogit")); got != "" {
		t.Errorf("no git: %q", got)
	}
	// A subdirectory of a checkout finds the checkout.
	sub := filepath.Join(mk("c", "ref: refs/heads/fix/x\n"), "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := BranchBase(sub); got != "x" {
		t.Errorf("subdir: %q", got)
	}
	// A worktree: .git is a gitdir: file, HEAD lives in that directory.
	gd := filepath.Join(root, "main", ".git", "worktrees", "r171")
	write(t, filepath.Join(gd, "HEAD"), "ref: refs/heads/feat/r171-session-names\n")
	wt := filepath.Join(root, "wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+gd+"\n")
	if got := BranchBase(wt); got != "r171-session-names" {
		t.Errorf("worktree: %q", got)
	}
	// A relative gitdir resolves against the checkout.
	wt2 := filepath.Join(root, "wt2")
	write(t, filepath.Join(wt2, ".git"), "gitdir: ../main/.git/worktrees/r171\n")
	if got := BranchBase(wt2); got != "r171-session-names" {
		t.Errorf("relative worktree: %q", got)
	}
}

func TestTranscriptTitles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, strings.Join([]string{
		`{"type":"user","message":"hi"}`,
		`{"type":"ai-title","aiTitle":"First","sessionId":"x"}`,
		`not json {{{`,
		`{"type":"ai-title","aiTitle":"Fix Acme login","sessionId":"x"}`,
		`{"type":"custom-title","customTitle":"mine","sessionId":"x"}`,
		`{"type":"ai-title","aiTitle":123}`,
	}, "\n"))
	ai, custom := Titles(p, 4<<20, 1<<20)
	if ai != "Fix Acme login" || custom != "mine" {
		t.Errorf("ai=%q custom=%q", ai, custom)
	}
	if Title(p) != "mine" {
		t.Errorf("custom should win: %q", Title(p))
	}
	if ai, custom := Titles(filepath.Join(t.TempDir(), "none"), 10, 10); ai != "" || custom != "" {
		t.Errorf("missing file: %q %q", ai, custom)
	}
}

func TestTranscriptBounds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	pad := strings.Repeat(`{"type":"user","message":"`+strings.Repeat("a", 90)+`"}`+"\n", 100)
	body := `{"type":"ai-title","aiTitle":"Head"}` + "\n" + pad + pad + pad + `{"type":"custom-title","customTitle":"Tail"}` + "\n"
	write(t, p, body)
	// Head 1 KiB sees only the first record; tail 1 KiB sees only the last.
	ai, custom := Titles(p, 1024, 1024)
	if ai != "Head" || custom != "Tail" {
		t.Errorf("ai=%q custom=%q", ai, custom)
	}
	// A middle-only title is outside both windows.
	write(t, p, pad+`{"type":"ai-title","aiTitle":"Mid"}`+"\n"+pad+pad+pad)
	if ai, _ := Titles(p, 1024, 1024); ai != "" {
		t.Errorf("middle title read: %q", ai)
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("a\x00b\x1b[31m\n\tc\u202ed"); got != "ab[31m c\u202ed" && got != "ab[31m cd" {
		// control characters become a space or vanish; none may remain
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("control char left in %q", got)
			}
		}
	}
	long := Sanitize(strings.Repeat("é", 200))
	if n := len([]rune(long)); n != 80 {
		t.Errorf("cap: %d runes", n)
	}
	if got := Sanitize("  hi  "); got != "hi" {
		t.Errorf("trim: %q", got)
	}
}

func TestCompose(t *testing.T) {
	if got := Compose("base", ""); got != "base" {
		t.Error(got)
	}
	if got := Compose("base", "Fix it"); got != "base · Fix it" {
		t.Error(got)
	}
	if got := len([]rune(Compose("base", strings.Repeat("x", 200)))); got != 80 {
		t.Error(got)
	}
}

func TestValidSessionID(t *testing.T) {
	for _, bad := range []string{"", "../x", sid + "/..", "abc", sid + "x"} {
		if ValidSessionID(bad) {
			t.Errorf("%q valid", bad)
		}
	}
	if !ValidSessionID(sid) {
		t.Error("uuid invalid")
	}
}

func TestStore(t *testing.T) {
	h := t.TempDir()
	if _, ok := LoadState(h, sid); ok {
		t.Error("state from nothing")
	}
	if err := SaveState(h, sid, State{Hash: HashOf("n"), Done: true}); err != nil {
		t.Fatal(err)
	}
	st, ok := LoadState(h, sid)
	if !ok || st.Hash != HashOf("n") || !st.Done {
		t.Errorf("%+v %v", st, ok)
	}
	if err := SaveState(h, "../evil", State{}); err == nil {
		t.Error("bad id saved")
	}
	b, _ := os.ReadFile(filepath.Join(h, "sessions", "names", sid))
	if strings.Contains(string(b), "n\"") && strings.Contains(string(b), `"n"`) {
		t.Error("name text stored")
	}
}

func TestTranscriptAllowed(t *testing.T) {
	cfg := t.TempDir()
	in := filepath.Join(cfg, "projects", "-Users-alice-Acme", sid+".jsonl")
	write(t, in, "")
	if !TranscriptAllowed(in, cfg) {
		t.Error("inside refused")
	}
	out := filepath.Join(t.TempDir(), "x.jsonl")
	write(t, out, "")
	if TranscriptAllowed(out, cfg) {
		t.Error("outside allowed")
	}
	if TranscriptAllowed(filepath.Join(cfg, "projects", "..", "x.jsonl"), cfg) {
		t.Error("dotdot allowed")
	}
	link := filepath.Join(cfg, "projects", "link.jsonl")
	if err := os.Symlink(out, link); err == nil && TranscriptAllowed(link, cfg) {
		t.Error("symlink out allowed")
	}
	if TranscriptAllowed("", cfg) || TranscriptAllowed("relative.jsonl", cfg) {
		t.Error("empty or relative allowed")
	}
}

func TestFindTranscript(t *testing.T) {
	cfg := t.TempDir()
	p := filepath.Join(cfg, "projects", "-Users-alice-Acme", sid+".jsonl")
	write(t, p, `{"type":"ai-title","aiTitle":"T"}`)
	if got := FindTranscript(cfg, sid); got != p {
		t.Errorf("got %q", got)
	}
	if FindTranscript(cfg, "../x") != "" || FindTranscript(cfg, "") != "" {
		t.Error("bad id found")
	}
	if FindTranscript(cfg, "0d5e1b4c-3f2a-4f60-8a7b-9c8d7e6f5a4b") != "" {
		t.Error("phantom")
	}
}

// ---- Decide ----

type env struct {
	home, cfg, repo, tr string
	now                 time.Time
}

func newEnv(t *testing.T, head string) env {
	t.Helper()
	e := env{home: t.TempDir(), cfg: t.TempDir(), repo: filepath.Join(t.TempDir(), "Acme"),
		now: time.Date(2026, 10, 8, 14, 5, 0, 0, time.Local)}
	write(t, filepath.Join(e.repo, ".git", "HEAD"), head)
	e.tr = filepath.Join(e.cfg, "projects", "-p", sid+".jsonl")
	write(t, e.tr, `{"type":"user"}`+"\n")
	return e
}

func (e env) in(event, source, title string) Input {
	return Input{Event: event, SessionID: sid, TranscriptPath: e.tr, Cwd: e.repo, Source: source, SessionTitle: title}
}

func (e env) decide(in Input) (string, bool) {
	return Decide(in, Env{Home: e.home, ConfigDir: e.cfg, Now: func() time.Time { return e.now }})
}

func TestDecideBranchThenTitleThenDone(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/m12-x\n")
	name, ok := e.decide(e.in("UserPromptSubmit", "", ""))
	if !ok || name != "m12-x" {
		t.Fatalf("first: %q %v", name, ok)
	}
	// Same name echoed back, no title yet: nothing.
	if name, ok := e.decide(e.in("UserPromptSubmit", "", "m12-x")); ok {
		t.Errorf("repeat printed %q", name)
	}
	write(t, e.tr, `{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n")
	name, ok = e.decide(e.in("UserPromptSubmit", "", "m12-x"))
	if !ok || name != "m12-x · Fix Acme login" {
		t.Fatalf("upgrade: %q %v", name, ok)
	}
	// Done: no more reads, even when the title changes.
	write(t, e.tr, `{"type":"ai-title","aiTitle":"Other"}`+"\n")
	if name, ok := e.decide(e.in("UserPromptSubmit", "", "m12-x · Fix Acme login")); ok {
		t.Errorf("after done printed %q", name)
	}
}

func TestDecideFallbackKeepsFirstTime(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/main\n")
	name, ok := e.decide(e.in("UserPromptSubmit", "", ""))
	if !ok || name != "Acme 14:05" {
		t.Fatalf("%q %v", name, ok)
	}
	e.now = e.now.Add(3 * time.Hour)
	write(t, e.tr, `{"type":"ai-title","aiTitle":"T"}`+"\n")
	name, ok = e.decide(e.in("UserPromptSubmit", "", "Acme 14:05"))
	if !ok || name != "Acme 14:05 · T" {
		t.Fatalf("%q %v", name, ok)
	}
}

func TestDecideUserRenameLeftAlone(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	if _, ok := e.decide(e.in("UserPromptSubmit", "", "")); !ok {
		t.Fatal("first")
	}
	write(t, e.tr, `{"type":"ai-title","aiTitle":"T"}`+"\n")
	if name, ok := e.decide(e.in("UserPromptSubmit", "", "my own name")); ok {
		t.Errorf("renamed session got %q", name)
	}
	// Ever again, even if the name later looks like ours.
	if name, ok := e.decide(e.in("UserPromptSubmit", "", "x")); ok {
		t.Errorf("after rename got %q", name)
	}
}

func TestDecideForeignNamedFromStart(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	if _, ok := e.decide(e.in("UserPromptSubmit", "", "plan name")); ok {
		t.Error("named session was renamed")
	}
}

func TestDecideSessionStartPrintsNothing(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	for _, src := range []string{"startup", "resume", "fork", "clear", "compact", ""} {
		if name, ok := e.decide(e.in("SessionStart", src, "")); ok {
			t.Errorf("source %q printed %q", src, name)
		}
	}
	if _, ok := e.decide(e.in("UserPromptSubmit", "", "")); !ok {
		t.Error("a prompt printed nothing")
	}
}

func TestDecideSourcesAndGuards(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	bad := e.in("UserPromptSubmit", "", "")
	bad.SessionID = "../x"
	if _, ok := e.decide(bad); ok {
		t.Error("bad id")
	}
	if _, ok := e.decide(e.in("Stop", "", "")); ok {
		t.Error("other event")
	}
}

func TestDecideTranscriptOutsideProjectsIgnored(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	outside := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, outside, `{"type":"ai-title","aiTitle":"Leak"}`+"\n")
	in := e.in("UserPromptSubmit", "", "")
	in.TranscriptPath = outside
	name, ok := e.decide(in)
	if !ok || name != "x" {
		t.Errorf("%q %v", name, ok)
	}
}

func TestDecideSanitisesAndCaps(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	write(t, e.tr, `{"type":"ai-title","aiTitle":"a\u0000b\nc `+strings.Repeat("z", 200)+`"}`+"\n")
	name, ok := e.decide(e.in("UserPromptSubmit", "", ""))
	if !ok || len([]rune(name)) != 80 {
		t.Fatalf("%q %v", name, ok)
	}
	for _, r := range name {
		if r < 0x20 {
			t.Errorf("control in %q", name)
		}
	}
}

func TestNonRegularFilesAreNeverRead(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "t.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if ai, c := Titles(fifo, 10, 10); ai != "" || c != "" {
			t.Error("fifo read")
		}
		// A FIFO named HEAD must not block BranchBase either.
		repo := filepath.Join(dir, "repo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Error(err)
			return
		}
		if err := syscall.Mkfifo(filepath.Join(repo, ".git", "HEAD"), 0o600); err != nil {
			t.Error(err)
			return
		}
		if got := BranchBase(repo); got != "" {
			t.Errorf("fifo HEAD: %q", got)
		}
	}()
	<-done
}

func TestSanitizeDropsLineAndParagraphSeparators(t *testing.T) {
	if got := Sanitize("a b c"); got != "abc" {
		t.Errorf("%q", got)
	}
}

func TestDecideNeedsAnAbsoluteCwd(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	for _, cwd := range []string{"", "Acme", "./x"} {
		in := e.in("UserPromptSubmit", "", "")
		in.Cwd = cwd
		if name, ok := e.decide(in); ok {
			t.Errorf("cwd %q named %q", cwd, name)
		}
	}
}

func TestDecideNeverOverwritesWhenTheTitleIsNotReported(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	if _, ok := e.decide(e.in("UserPromptSubmit", "", "")); !ok {
		t.Fatal("first")
	}
	write(t, e.tr, `{"type":"ai-title","aiTitle":"T"}`+"\n")
	if name, ok := e.decide(e.in("UserPromptSubmit", "", "")); ok {
		t.Errorf("named %q over an unreported title", name)
	}
}

func TestDecideUserRenameBeforeFirstPromptIsLeftAlone(t *testing.T) {
	e := newEnv(t, "ref: refs/heads/feat/x\n")
	e.decide(e.in("UserPromptSubmit", "", ""))
	if name, ok := e.decide(e.in("UserPromptSubmit", "", "mine")); ok {
		t.Errorf("printed %q", name)
	}
}

func TestOldStateWithLiveStillParses(t *testing.T) {
	h := t.TempDir()
	write(t, filepath.Join(h, "sessions", "names", sid), `{"sha256":"`+HashOf("n")+`","done":true,"live":true}`)
	st, ok := LoadState(h, sid)
	if !ok || st.Hash != HashOf("n") || !st.Done {
		t.Errorf("%+v %v", st, ok)
	}
}
