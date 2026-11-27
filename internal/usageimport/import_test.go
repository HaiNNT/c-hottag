package usageimport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

func aline(ts, mid string, out int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"req_1","sessionId":"SESSION-CANARY-9a1","cwd":"/Users/alice/x","message":{"id":%q,"model":"claude-opus-5-5","content":[{"type":"text","text":"SECRET-CANARY-7f3"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":10,"cache_read_input_tokens":5,"output_tokens":%d,"cache_creation":{"ephemeral_1h_input_tokens":7,"ephemeral_5m_input_tokens":3},"server_tool_use":{"web_search_requests":1},"speed":"standard"}}}`+"\n", ts, mid, out)
}

type env struct {
	dir  string // the projects dir
	path string
	l    usagelog.Ledger
}

func newEnv(t *testing.T) env {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(dir, "p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return env{dir, filepath.Join(dir, "p1", "a.jsonl"), usagelog.Open(t.TempDir())}
}

func (e env) write(t *testing.T, s string, appendTo bool) {
	t.Helper()
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(e.path, flag, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func (e env) run(t *testing.T, cutoff time.Time) Result {
	t.Helper()
	r, err := Import(context.Background(), e.l, []Source{{Dir: e.dir, Src: "slot"}}, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e env) events(t *testing.T) []usagelog.Event {
	t.Helper()
	evs, err := e.l.Events(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

const ts = "2026-11-26T09:12:03.123Z"

func TestImportOneLinePerMessage(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1)+aline(ts, "msg_1", 1)+
		`{"type":"user","message":{"content":"hi"}}`+"\n"+
		strings.Replace(aline(ts, "msg_2", 5), "claude-opus-5-5", "<synthetic>", 1)+
		aline(ts, "msg_1", 84), false)
	r := e.run(t, time.Time{})
	evs := e.events(t)
	if len(evs) != 1 || r.Events != 1 || r.Files != 1 {
		t.Fatalf("%+v %+v", r, evs)
	}
	g := evs[0]
	if g.Out != 84 || g.Src != "slot" || g.Acct != "" || g.MID != "msg_1" || g.RID != "req_1" ||
		g.In != 2 || g.CR != 5 || g.CW5m != 3 || g.CW1h != 7 || g.Web != 1 || g.Speed != "standard" {
		t.Fatalf("%+v", g)
	}
}

func TestImportFlatCacheCreationIs5m(t *testing.T) {
	e := newEnv(t)
	l := strings.Replace(aline(ts, "msg_1", 1), `"cache_creation":{"ephemeral_1h_input_tokens":7,"ephemeral_5m_input_tokens":3},`, "", 1)
	e.write(t, l, false)
	e.run(t, time.Time{})
	if g := e.events(t)[0]; g.CW5m != 10 || g.CW1h != 0 {
		t.Fatalf("%+v", g)
	}
}

func TestImportIncremental(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	e.write(t, aline(ts, "msg_2", 2)+aline(ts, "msg_3", 3), true)
	r := e.run(t, time.Time{})
	if r.Events != 2 || r.Lines != 2 || len(e.events(t)) != 3 {
		t.Fatalf("%+v", r)
	}
	st, err := e.l.ImportState()
	if err != nil || len(st) != 1 {
		t.Fatalf("%v %v", st, err)
	}
	if m := st[markKey(e.path)]; m.Offset != m.Size || m.Size == 0 {
		t.Fatalf("mark %+v", m)
	}
	if r := e.run(t, time.Time{}); r.Files != 0 || r.Events != 0 {
		t.Fatalf("unchanged file re-read: %+v", r)
	}
}

func TestImportShrunkRereads(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1)+aline(ts, "msg_2", 2), false)
	e.run(t, time.Time{})
	e.write(t, aline(ts, "msg_9", 9), false)
	if r := e.run(t, time.Time{}); r.Events != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestImportPartialLastLine(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1)+strings.TrimSuffix(aline(ts, "msg_2", 2), "\n"), false)
	e.run(t, time.Time{})
	if n := len(e.events(t)); n != 1 {
		t.Fatalf("partial imported: %d", n)
	}
	e.write(t, "\n", true)
	r := e.run(t, time.Time{})
	if r.Events != 1 || len(e.events(t)) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestImportCutoff(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline("2026-11-01T00:00:00Z", "msg_1", 1)+aline(ts, "msg_2", 2), false)
	cut, _ := time.Parse(time.RFC3339, "2026-11-10T00:00:00Z")
	e.run(t, cut)
	if evs := e.events(t); len(evs) != 1 || evs[0].MID != "msg_2" {
		t.Fatalf("%+v", evs)
	}
}

func TestImportMissingDir(t *testing.T) {
	l := usagelog.Open(t.TempDir())
	r, err := Import(context.Background(), l, []Source{{Dir: filepath.Join(t.TempDir(), "nope"), Src: "home"}}, time.Time{}, false)
	if err != nil || r != (Result{}) {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestImportReimportIgnoresMarks(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	r, err := Import(context.Background(), e.l, []Source{{Dir: e.dir, Src: "slot"}}, time.Time{}, true)
	if err != nil || r.Files != 1 || r.Events != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestImportNeverKeepsText(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	n := 0
	err := filepath.WalkDir(e.l.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range []string{"SECRET-CANARY-7f3", "SESSION-CANARY-9a1", "/Users/alice"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("walk %v n=%d", err, n)
	}
}

// openFile is only ever used for reading: the handle it returns refuses writes.
func TestImportReadOnly(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	opened := 0
	orig := openFile
	openFile = func(path string) (*os.File, error) {
		f, err := orig(path)
		if err != nil {
			return nil, err
		}
		opened++
		if _, werr := f.Write([]byte("x")); werr == nil {
			t.Error("transcript handle is writable")
		}
		return f, nil
	}
	defer func() { openFile = orig }()
	e.run(t, time.Time{})
	if opened == 0 {
		t.Fatal("openFile never used")
	}
}

func TestImportStopsOnACancelledContext(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline("2026-11-20T10:00:00Z", "m1", 5), false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Import(ctx, e.l, []Source{{Dir: e.dir, Src: "slot"}}, time.Time{}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if evs := e.events(t); len(evs) != 0 {
		t.Fatalf("a cancelled import appended %+v", evs)
	}
	if _, err := os.Stat(filepath.Join(e.l.Dir, "import.json")); err == nil {
		t.Fatal("a cancelled import saved its marks")
	}
}

func TestImportMarksAreHashedAndStillSkip(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	b, err := os.ReadFile(filepath.Join(e.l.Dir, "import.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/") || strings.Contains(string(b), "p1") || strings.Contains(string(b), "a.jsonl") {
		t.Fatalf("import.json holds a path: %s", b)
	}
	if r := e.run(t, time.Time{}); r.Files != 0 || r.Events != 0 {
		t.Fatalf("re-read after hashing: %+v", r)
	}
}

func TestImportMigratesPathKeys(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	st, _ := e.l.ImportState()
	old := map[string]usagelog.FileMark{e.path: st[markKey(e.path)]}
	if err := e.l.SaveImportState(old); err != nil {
		t.Fatal(err)
	}
	if r := e.run(t, time.Time{}); r.Files != 0 {
		t.Fatalf("migrated mark lost: %+v", r)
	}
	b, _ := os.ReadFile(filepath.Join(e.l.Dir, "import.json"))
	if strings.Contains(string(b), "/") {
		t.Fatalf("path key kept: %s", b)
	}
}

func TestImportSkipsUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	bad := filepath.Join(e.dir, "locked")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o755) })
	r := e.run(t, time.Time{})
	if r.Skipped < 1 || r.Events != 1 || len(e.events(t)) != 1 {
		t.Fatalf("%+v", r)
	}
	if st, _ := e.l.ImportState(); len(st) != 1 {
		t.Fatalf("marks not saved: %v", st)
	}
}

func TestImportSkipsFileGoneAfterWalk(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	gone := filepath.Join(e.dir, "p1", "b.jsonl")
	if err := os.WriteFile(gone, []byte(aline(ts, "msg_2", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := openFile
	openFile = func(path string) (*os.File, error) {
		if path == gone {
			os.Remove(gone)
		}
		return orig(path)
	}
	defer func() { openFile = orig }()
	r := e.run(t, time.Time{})
	if r.Events != 1 || r.Skipped != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestImportForgetsDeletedTranscripts(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	e.run(t, time.Time{})
	if st, _ := e.l.ImportState(); len(st) != 0 {
		t.Fatalf("mark of a deleted file kept: %v", st)
	}
}

func TestImportKeepsMarksWhenSourceMissing(t *testing.T) {
	e := newEnv(t)
	e.write(t, aline(ts, "msg_1", 1), false)
	e.run(t, time.Time{})
	gone := filepath.Join(t.TempDir(), "nope")
	if _, err := Import(context.Background(), e.l, []Source{{Dir: e.dir, Src: "slot"}, {Dir: gone, Src: "home"}}, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), e.l, []Source{{Dir: gone, Src: "home"}}, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.l.ImportState(); len(st) != 1 {
		t.Fatalf("marks dropped though no source was walked: %v", st)
	}
}
