package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// statsNow0 is the fixed clock of the stats tests (local time).
var statsNow0 = time.Date(2026, 11, 26, 12, 0, 0, 0, time.Local)

// statsFixture registers B (ultra), C (max5x) and A (team, rotation off: the
// remote account), fixes the clock, and fills the ledger: opus and sonnet
// usage for B and C, one unattributed event, and four complete 7d windows of
// snapshots for B.
func statsFixture(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", t.TempDir())
	prev := statsNow
	statsNow = func() time.Time { return statsNow0 }
	t.Cleanup(func() { statsNow = prev })
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		for _, a := range []store.Account{
			{Name: "B", Plan: "max20x", Dir: filepath.Join(home, "accounts", "B")},
			{Name: "C", Plan: "max5x", Dir: filepath.Join(home, "accounts", "C")},
			{Name: "A", Plan: "team", NoRotate: true, Dir: filepath.Join(home, "accounts", "A")},
		} {
			if err := st.Add(a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l := usagelog.Open(home)
	day := func(d int) time.Time { return statsNow0.AddDate(0, 0, -d) }
	var evs []usagelog.Event
	for i := 0; i < 10; i++ {
		evs = append(evs,
			usagelog.Event{T: day(i), MID: fmt.Sprintf("b%d", i), Src: "live", Acct: "B", Model: "claude-opus-5-5", In: 1000, Out: 200000, CR: 5_000_000},
			usagelog.Event{T: day(i), MID: fmt.Sprintf("c%d", i), Src: "live", Acct: "C", Model: "claude-sonnet-5-5", In: 1000, Out: 100000, CR: 2_000_000},
		)
	}
	evs = append(evs, usagelog.Event{T: day(2), MID: "u1", Src: "slot", Model: "claude-opus-5-5", In: 5000, Out: 300000})
	if err := l.AppendEvents(evs); err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= 4; k++ {
		reset := statsNow0.AddDate(0, 0, -7*k+1)
		p := 78.0
		if err := l.AppendSnapshot(usagelog.Snapshot{T: reset.Add(-24 * time.Hour), Acct: "B", Tier: "max20x", Units: 20, P7: &p, R7: reset.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestStatsTextGolden(t *testing.T) {
	statsFixture(t)
	code, out, errs := runChottag(t, "stats")
	if code != exit.OK {
		t.Fatalf("stats = %d %q", code, errs)
	}
	var last int
	for _, want := range []string{
		"Usage 2026-06-01 to 2026-11-26", "overall", "claude-opus-5-5", "claude-sonnet-5-5",
		"unknown account (history, Home)",
		"Accounts", "plan", "value/price", "B ", "ultra", "C ", "max5x", "A (remote only)",
		"Verdicts", "  B  ", "  overall  ",
		"Thresholds: ", "windows=4",
	} {
		i := strings.Index(out[last:], want)
		if i < 0 {
			t.Fatalf("output lacks %q after offset %d:\n%s", want, last, out)
		}
		last += i
	}
	if strings.Contains(errs, "chottag:") {
		t.Fatalf("stderr = %q", errs)
	}
}

func TestStatsJSONShape(t *testing.T) {
	statsFixture(t)
	code, out, errs := runChottag(t, "stats", "--json")
	if code != exit.OK {
		t.Fatalf("stats --json = %d %q", code, errs)
	}
	doc := decodeOneDocument(t, out)
	for _, k := range []string{"since", "until", "by", "total", "byModel", "unattributed", "periods", "accounts", "verdicts", "overall", "thresholds", "notes"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("document lacks %q: %s", k, out)
		}
	}
	if _, ok := doc["html"]; ok {
		t.Errorf("html present without --html")
	}
	if doc["by"] != "month" {
		t.Errorf("by = %v, want month", doc["by"])
	}
}

func TestStatsByDayDefaultsToThirtyDays(t *testing.T) {
	statsFixture(t)
	code, out, errs := runChottag(t, "stats", "--by", "day", "--json")
	if code != exit.OK {
		t.Fatalf("= %d %q", code, errs)
	}
	doc := decodeOneDocument(t, out)
	want := time.Date(2026, 10, 27, 0, 0, 0, 0, time.Local).Format(time.RFC3339)
	if doc["since"] != want {
		t.Errorf("since = %v, want %s", doc["since"], want)
	}
}

func TestStatsHTMLWritesFile(t *testing.T) {
	home := statsFixture(t)
	code, out, errs := runChottag(t, "stats", "--html")
	if code != exit.OK {
		t.Fatalf("= %d %q", code, errs)
	}
	path := filepath.Join(home, "usage", "report.html")
	if strings.TrimSpace(out) != path {
		t.Fatalf("stdout = %q, want the path %q", out, path)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
		t.Fatalf("report = %d bytes, %v", len(b), err)
	}

	custom := filepath.Join(t.TempDir(), "r.html")
	code, out, errs = runChottag(t, "stats", "--html", "--out", custom, "--json")
	if code != exit.OK {
		t.Fatalf("= %d %q", code, errs)
	}
	doc := decodeOneDocument(t, out)
	if doc["html"] != custom {
		t.Errorf("html = %v, want %s", doc["html"], custom)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Error(err)
	}
}

func TestStatsRejectsBadInput(t *testing.T) {
	statsFixture(t)
	for _, args := range [][]string{
		{"stats", "--by", "year"}, {"stats", "--since", "yesterday"}, {"stats", "--until", "2026-13-01"},
		{"stats", "--weeks", "0"}, {"stats", "--out", "x.html"}, {"stats", "extra"},
		{"stats", "--since", "2026-11-20", "--until", "2026-11-01"},
	} {
		if code, out, errs := runChottag(t, args...); code != exit.Usage || out != "" || errs == "" {
			t.Errorf("%q = %d %q %q, want exit 2", args, code, out, errs)
		}
	}
}

// writeTranscript writes a slot or Home transcript with one assistant line
// holding a distinctive text and cwd that must never reach the ledger.
func writeStatsTranscript(t *testing.T, dir, name, ts, mid string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/Users/alice/proj","sessionId":"sess-1","message":{"id":%q,"model":"claude-opus-5-5","content":[{"type":"text","text":"TRANSCRIPT-CANARY"}],"usage":{"input_tokens":10,"output_tokens":20}}}`+"\n", ts, mid)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStatsFirstImportTakesAll(t *testing.T) {
	home := statsFixture(t)
	hp := t.TempDir()
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })
	// A year-old line: far past the 90-day raw retention.
	writeStatsTranscript(t, filepath.Join(home, "accounts", "B", "projects", "p"), "s.jsonl", "2025-11-01T10:00:00Z", "old1")
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h.jsonl", "2026-11-20T10:00:00Z", "home1")

	if code, _, errs := runChottag(t, "stats", "--since", "2025-01-01"); code != exit.OK {
		t.Fatalf("= %d %q", code, errs)
	}
	days, err := usagelog.Open(home).Days(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var old, homeRows int
	for _, d := range days {
		if d.Src == "slot" && strings.HasPrefix(d.Day, "2025-11") {
			old++
		}
		if d.Src == "home" {
			homeRows++
		}
	}
	if old == 0 || homeRows == 0 {
		t.Fatalf("first import kept old=%d home=%d rows, want both: %+v", old, homeRows, days)
	}
}

func TestStatsLaterImportUsesCutoff(t *testing.T) {
	home := statsFixture(t)
	hp := t.TempDir()
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })
	if code, _, errs := runChottag(t, "stats"); code != exit.OK { // first import: marks import.json
		t.Fatalf("= %d %q", code, errs)
	}
	writeStatsTranscript(t, filepath.Join(home, "accounts", "B", "projects", "p"), "s.jsonl", "2025-11-01T10:00:00Z", "old2")
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h.jsonl", "2026-11-25T10:00:00Z", "new2")
	if code, _, errs := runChottag(t, "stats"); code != exit.OK {
		t.Fatalf("= %d %q", code, errs)
	}
	days, err := usagelog.Open(home).Days(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var old, fresh bool
	for _, d := range days {
		old = old || (d.Src == "slot" && strings.HasPrefix(d.Day, "2025-"))
		fresh = fresh || (d.Src == "home" && d.Day == "2026-11-25")
	}
	if old || !fresh {
		t.Fatalf("old=%v fresh=%v, want the year-old line skipped and the recent one kept: %+v", old, fresh, days)
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			m[p] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		m[p] = fmt.Sprintf("%d %s %s", len(b), fi.ModTime().Format(time.RFC3339Nano), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStatsNeverWritesHome(t *testing.T) {
	statsFixture(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	hp := filepath.Join(userHome, ".claude", "projects")
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h.jsonl", "2026-11-20T10:00:00Z", "home9")
	if err := os.WriteFile(filepath.Join(userHome, ".claude.json"), []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })

	before := snapshotTree(t, userHome)
	for _, args := range [][]string{{"stats"}, {"stats", "--reimport"}} {
		if code, _, errs := runChottag(t, args...); code != exit.OK {
			t.Fatalf("%v = %d %q", args, code, errs)
		}
	}
	if after := snapshotTree(t, userHome); !reflect.DeepEqual(before, after) {
		t.Fatalf("HOME changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestStatsNeverStoresTranscriptText(t *testing.T) {
	home := statsFixture(t)
	writeStatsTranscript(t, filepath.Join(home, "accounts", "B", "projects", "p"), "s.jsonl", "2026-11-20T10:00:00Z", "m1")
	if code, _, errs := runChottag(t, "stats", "--html"); code != exit.OK {
		t.Fatalf("= %d %q", code, errs)
	}
	scanTreeForString(t, home, "TRANSCRIPT-CANARY", filepath.Join(home, "accounts"))
	scanTreeForString(t, home, "/Users/alice/proj", filepath.Join(home, "accounts"))
	scanTreeForString(t, home, "sess-1", filepath.Join(home, "accounts"))
}

// scanTreeForString fails when any file under root, except those under skip,
// contains needle.
func scanTreeForString(t *testing.T, root, needle, skip string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == skip {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil // a socket or lock file
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("%s contains %q", p, needle)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "stats reports the ledger", command: "stats",
			setup: func(t *testing.T) []string { statsFixture(t); return []string{"stats"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["by"] != "month" || doc["accounts"] == nil {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "stats with a bad --by", command: "stats",
			setup:    func(t *testing.T) []string { statsFixture(t); return []string{"stats", "--by", "year"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}
