package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
	"github.com/HaiNNT/c-hottag/internal/usagestats"
)

func TestStatsAccountResolvesLikeOtherCommands(t *testing.T) {
	home := statsFixture(t)
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		a, err := st.Find("C")
		if err != nil {
			return err
		}
		a.Email = "carol@example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"C", "c", "carol@example.com"} {
		code, out, errs := runChottag(t, "stats", "--account", name, "--json")
		if code != exit.OK {
			t.Fatalf("--account %s = %d %q", name, code, errs)
		}
		doc := decodeOneDocument(t, out)
		accts, _ := doc["accounts"].([]any)
		if len(accts) != 1 || accts[0].(map[string]any)["name"] != "C" {
			t.Errorf("--account %s accounts = %v, want only C", name, accts)
		}
	}
	code, out, errs := runChottag(t, "stats", "--account", "Zed", "--json")
	if code != exit.Error || errs == "" {
		t.Fatalf("unknown account = %d %q", code, errs)
	}
	if doc := decodeOneDocument(t, out); doc["error"].(map[string]any)["code"] != string(codeUnknownAccount) {
		t.Fatalf("doc = %v, want unknown_account", doc)
	}
	// A name only the ledger knows (an account since removed) is allowed.
	if err := usagelog.Open(home).AppendEvents([]usagelog.Event{{T: statsNow0, MID: "gone1", Src: "live", Acct: "Gone", Model: "claude-opus-5-5", Out: 10}}); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runChottag(t, "stats", "--account", "Gone"); code != exit.OK {
		t.Fatalf("ledger-only account = %d %q", code, errs)
	}
}

func TestStatsSinceInTheFutureIsUsage(t *testing.T) {
	statsFixture(t)
	code, out, errs := runChottag(t, "stats", "--since", "2027-01-01")
	if code != exit.Usage || out != "" || !strings.Contains(errs, "future") {
		t.Fatalf("= %d %q %q", code, out, errs)
	}
}

func TestMoneyRoundsToCentsFirst(t *testing.T) {
	for _, tc := range []struct {
		usd  float64
		want string
	}{{5.999, "$6.00"}, {1904.1, "$1,904.10"}, {0.004, "$0.00"}, {999.996, "$1,000.00"}} {
		if got := money(usagestats.Tok{USD: tc.usd}); got != tc.want {
			t.Errorf("money(%v) = %q, want %q", tc.usd, got, tc.want)
		}
	}
}

func TestStatsSkipsUnreadableTranscriptAndWarns(t *testing.T) {
	home := statsFixture(t)
	dir := filepath.Join(home, "accounts", "B", "projects", "SecretProject")
	writeStatsTranscript(t, dir, "s.jsonl", "2026-11-20T10:00:00Z", "m1")
	if err := os.Chmod(filepath.Join(dir, "s.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runChottag(t, "stats")
	if code != exit.OK || !strings.Contains(out, "overall") {
		t.Fatalf("= %d %q %q", code, out, errs)
	}
	if !strings.Contains(errs, "could not be read") || strings.Contains(errs, "SecretProject") {
		t.Fatalf("stderr = %q, want a warning without the transcript path", errs)
	}
	code, out, _ = runChottag(t, "stats", "--json")
	doc := decodeOneDocument(t, out)
	w, _ := doc["warnings"].([]any)
	if code != exit.OK || len(w) != 1 || w[0].(map[string]any)["code"] != "usage_import_skipped" {
		t.Fatalf("warnings = %v", doc["warnings"])
	}
}

func TestUsageTickLogsNoTranscriptPath(t *testing.T) {
	home := t.TempDir()
	hp := t.TempDir()
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })
	dir := filepath.Join(hp, "SecretProject")
	writeStatsTranscript(t, dir, "s.jsonl", "2026-11-20T10:00:00Z", "m1")
	if err := os.Chmod(filepath.Join(dir, "s.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	u := &usageTick{home: home, log: &log, now: func() time.Time { return statsNow0 }}
	u.pass(context.Background())
	if !strings.Contains(log.String(), "could not be read") || strings.Contains(log.String(), "SecretProject") {
		t.Fatalf("log = %q, want a count without the path", log.String())
	}
	n := log.Len()
	u.pass(context.Background())
	if log.Len() != n {
		t.Fatalf("the same skipped count was logged twice: %q", log.String())
	}
}

func TestUsageTickReturnsPromptlyOnACancelledContext(t *testing.T) {
	home := t.TempDir()
	hp := t.TempDir()
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h.jsonl", "2026-11-20T10:00:00Z", "t1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u := &usageTick{home: home, log: &bytes.Buffer{}, now: func() time.Time { return statsNow0 }, tick: make(chan time.Time)}
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	<-done // no timer: the test hangs only if Run ignores the context
}

func TestUsageTickSkipsABusyLedgerAndRetries(t *testing.T) {
	home := t.TempDir()
	hp := t.TempDir()
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h.jsonl", "2026-11-20T10:00:00Z", "t1")
	l := usagelog.Open(home)
	unlock, err := l.Lock()
	if err != nil {
		t.Fatal(err)
	}
	passed := make(chan struct{})
	retry := make(chan time.Time)
	u := &usageTick{home: home, log: &bytes.Buffer{}, now: func() time.Time { return statsNow0 },
		tick: make(chan time.Time), retry: func() <-chan time.Time { return retry }, passed: passed}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.Run(ctx) }()
	<-passed // the start pass returned at once although the lock is held
	if _, err := os.Stat(filepath.Join(home, "usage", "import.json")); err == nil {
		t.Fatal("a busy pass imported")
	}
	unlock()
	retry <- statsNow0
	<-passed
	if _, err := os.Stat(filepath.Join(home, "usage", "import.json")); err != nil {
		t.Fatalf("the retry did not import: %v", err)
	}
	cancel()
	<-done
}

func TestUsageTickShutdownIsNotBlockedByABusyLedger(t *testing.T) {
	home := t.TempDir()
	l := usagelog.Open(home)
	unlock, err := l.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	passed := make(chan struct{})
	u := &usageTick{home: home, log: &bytes.Buffer{}, now: func() time.Time { return statsNow0 },
		tick: make(chan time.Time), retry: func() <-chan time.Time { return make(chan time.Time) }, passed: passed}
	done := make(chan struct{})
	go func() { defer close(done); u.Run(ctx) }()
	<-passed
	cancel()
	<-done
}

func TestStatsDefaultsToDayForAShortSince(t *testing.T) {
	statsFixture(t)
	_, out, _ := runChottag(t, "stats", "--since", "2026-11-10", "--json")
	if by := decodeOneDocument(t, out)["by"]; by != "day" {
		t.Fatalf("by = %v, want day", by)
	}
	_, out, _ = runChottag(t, "stats", "--since", "2026-01-10", "--json")
	if by := decodeOneDocument(t, out)["by"]; by != "month" {
		t.Fatalf("by = %v, want month", by)
	}
	_, out, _ = runChottag(t, "stats", "--since", "2026-11-10", "--by", "month", "--json")
	if by := decodeOneDocument(t, out)["by"]; by != "month" {
		t.Fatalf("explicit --by lost: %v", by)
	}
}

func TestStatsNeverEmitsAZeroUntil(t *testing.T) {
	statsFixture(t)
	_, out, _ := runChottag(t, "stats", "--json")
	doc := decodeOneDocument(t, out)
	for _, k := range []string{"since", "until"} {
		if s, _ := doc[k].(string); s == "" || strings.HasPrefix(s, "0001") {
			t.Fatalf("%s = %q", k, s)
		}
	}
	_, text, _ := runChottag(t, "stats")
	if strings.Contains(text, "0001-") {
		t.Fatalf("text has a zero date: %s", text)
	}
}

func TestStatsTextShowsWeeklyPeaks(t *testing.T) {
	var b bytes.Buffer
	f := func(v float64) *float64 { return &v }
	rep := usagestats.Report{
		Since: statsNow0.AddDate(0, 0, -20), Until: statsNow0, By: "day",
		ByModel: map[string]usagestats.Tok{}, Thresholds: map[string]float64{},
		Accounts: []usagestats.AccountRow{{Name: "B", Tier: "max5x", Units: 5, USDPerPct: f(2), USDPerUnitPct: f(0.4), From: statsNow0.AddDate(0, 0, -10).Format("2006-01-02"), ValueRatio: f(0.88),
			Weeks: []usagestats.Week{{PeakPct: 41}, {PeakPct: 63}, {PeakPct: 88}, {PeakPct: 97}}}},
	}
	renderStatsText(&b, rep, nil, statsNow0)
	out := b.String()
	if !strings.Contains(out, "B  weekly peak %: 41 63 88 97") || !strings.Contains(out, "$0.40") || !strings.Contains(out, "since ") {
		t.Fatalf("out = %s", out)
	}
}
