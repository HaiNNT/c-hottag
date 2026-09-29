package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
	"github.com/HaiNNT/c-hottag/internal/tracesum"
)

func traceCmdHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	return home
}

// untilWindow bounds trace.go's `until = now.Add(d).Truncate(time.Second)`
// against the two real timestamps a test can actually observe: before,
// captured immediately before the command runs, and after, captured
// immediately once it returns. Never a fixed wall-clock fudge margin
// (F155/F185: a fixed guess flakes under load, and a production duration
// is never a test's own correctness bound). The lower bound truncates
// before to the second too, to account for `until`'s own truncation: the
// command's internal now is always >= before, but Truncate can floor
// now.Add(d) to just under before.Add(d) when the two are within the same
// second and before itself carries a sub-second fraction.
func untilWindow(before, after time.Time, d time.Duration) (time.Time, time.Time) {
	return before.Truncate(time.Second).Add(d), after.Add(d)
}

func readTraceUntil(t *testing.T, home string) (time.Time, bool) {
	t.Helper()
	st, err := store.Store{Dir: home}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Trace == nil {
		return time.Time{}, false
	}
	return st.Trace.Until, true
}

func traceMarks(t *testing.T, home string) []string {
	t.Helper()
	recs, err := tracelog.ReadAll(filepath.Join(home, "trace.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if r.Kind == "mark" {
			out = append(out, r.Mark)
		}
	}
	return out
}

func writeLastTraced(t *testing.T, home, version string, at time.Time) {
	t.Helper()
	var f status.File
	f.SetLastTraced(version, at)
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

func writeTraceRecords(t *testing.T, home string, recs []tracelog.Record) {
	t.Helper()
	w, err := tracelog.Open(filepath.Join(home, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	base := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for i, r := range recs {
		r.T = base.Add(time.Duration(i) * time.Second)
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
}

// assertAbsent is T3's own (PF11): it fails the test unless every name in
// names has nothing at all at <home>/name.
func assertAbsent(t *testing.T, home string, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(home, n)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists (stat: %v), want nothing written", n, err)
		}
	}
}

func TestTraceShowsOffOnAFreshHomeAndWritesNothing(t *testing.T) {
	home := traceCmdHome(t)
	code, out, errs := runChottag(t, "trace")
	if code != exit.OK || out != "trace off\nlast traced: never\n" {
		t.Fatalf("trace = %d %q; stderr %q", code, out, errs)
	}
	assertAbsent(t, home, "state.json", "trace.jsonl")
}

func TestTraceOnDefaultsToOneHourAndAppendsOneMark(t *testing.T) {
	home := traceCmdHome(t)
	before := time.Now()
	code, out, errs := runChottag(t, "trace", "on")
	after := time.Now()
	if code != exit.OK || !strings.HasPrefix(out, "trace on (until ") || !strings.HasSuffix(out, "\nlast traced: never\n") {
		t.Fatalf("trace on = %d %q; stderr %q", code, out, errs)
	}
	until, ok := readTraceUntil(t, home)
	lower, upper := untilWindow(before, after, time.Hour)
	if !ok || until.Before(lower) || until.After(upper) {
		t.Fatalf("until = %v (set %v), want now+1h within [%v, %v]", until, ok, lower, upper)
	}
	if got := traceMarks(t, home); len(got) != 1 || got[0] != tracesum.TraceOnMark {
		t.Fatalf("marks = %q, want exactly one %q", got, tracesum.TraceOnMark)
	}
}

func TestTraceOnForSetsTheWindow(t *testing.T) {
	// 24h is the cap itself and is accepted; TestTraceOnRejectsBadDurations
	// has 24h1s, one second over.
	for _, c := range []struct {
		arg string
		d   time.Duration
	}{{"90m", 90 * time.Minute}, {"24h", 24 * time.Hour}} {
		t.Run(c.arg, func(t *testing.T) {
			home := traceCmdHome(t)
			before := time.Now()
			if code, _, errs := runChottag(t, "trace", "on", "--for", c.arg); code != exit.OK {
				t.Fatalf("trace on --for %s = %d; stderr %q", c.arg, code, errs)
			}
			after := time.Now()
			until, _ := readTraceUntil(t, home)
			lower, upper := untilWindow(before, after, c.d)
			if until.Before(lower) || until.After(upper) {
				t.Fatalf("until = %v, want within [%v, %v] (now+%v)", until, lower, upper, c.d)
			}
		})
	}
}

func TestTraceOnRejectsBadDurationsAndWritesNothing(t *testing.T) {
	// 24h1s is one second over the 24h cap; 0, -1m and 500ms are under the 1s floor.
	for _, arg := range []string{"0", "-1m", "500ms", "25h", "24h1s", "abc"} {
		t.Run(arg, func(t *testing.T) {
			home := traceCmdHome(t)
			code, out, errs := runChottag(t, "trace", "on", "--for", arg)
			if code != exit.Usage || out != "" {
				t.Fatalf("trace on --for %s = %d %q; stderr %q, want exit 2 and no stdout", arg, code, out, errs)
			}
			assertAbsent(t, home, "state.json", "trace.jsonl")
		})
	}
}

// TestTraceOnWhileOnMovesTheWindowWithoutASecondMark is Review Focus 2.
func TestTraceOnWhileOnMovesTheWindowWithoutASecondMark(t *testing.T) {
	home := traceCmdHome(t)
	if code, _, errs := runChottag(t, "trace", "on", "--for", "10m"); code != exit.OK {
		t.Fatalf("trace on = %d; %q", code, errs)
	}
	for _, c := range []struct {
		arg string
		d   time.Duration
	}{{"2h", 2 * time.Hour}, {"1m", time.Minute}} { // extend, then shorten
		before := time.Now()
		if code, _, errs := runChottag(t, "trace", "on", "--for", c.arg); code != exit.OK {
			t.Fatalf("trace on --for %s = %d; %q", c.arg, code, errs)
		}
		after := time.Now()
		until, _ := readTraceUntil(t, home)
		lower, upper := untilWindow(before, after, c.d)
		if until.Before(lower) || until.After(upper) {
			t.Fatalf("--for %s: until = %v, want within [%v, %v] (now+%v)", c.arg, until, lower, upper, c.d)
		}
	}
	if got := traceMarks(t, home); len(got) != 1 {
		t.Fatalf("marks = %q, want one: moving an open window must not restart summarize's window", got)
	}
}

func TestTraceOnAfterTheWindowEndedOpensANewWindowWithAMark(t *testing.T) {
	home := traceCmdHome(t)
	// A window that ended a minute ago: nothing cleared it, and it is off.
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		st.SetTraceUntil(time.Now().Add(-time.Minute))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := runChottag(t, "trace"); !strings.HasPrefix(out, "trace off\n") {
		t.Fatalf("trace = %q, want off for an ended window", out)
	}
	if code, _, errs := runChottag(t, "trace", "on"); code != exit.OK {
		t.Fatalf("trace on = %d; %q", code, errs)
	}
	if got := traceMarks(t, home); len(got) != 1 {
		t.Fatalf("marks = %q, want one for the new window", got)
	}
}

func TestTraceOffRemovesTheWindow(t *testing.T) {
	home := traceCmdHome(t)
	if code, _, errs := runChottag(t, "trace", "on"); code != exit.OK {
		t.Fatalf("trace on = %d; %q", code, errs)
	}
	code, out, errs := runChottag(t, "trace", "off")
	if code != exit.OK || out != "trace off\nlast traced: never\n" {
		t.Fatalf("trace off = %d %q; stderr %q", code, out, errs)
	}
	b, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["trace"]; ok {
		t.Fatalf("state.json = %s, want no trace key after trace off", b)
	}
}

func TestTraceOffOnAFreshHomeCreatesState(t *testing.T) {
	home := traceCmdHome(t)
	if code, _, errs := runChottag(t, "trace", "off"); code != exit.OK {
		t.Fatalf("trace off = %d; %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); err != nil {
		t.Fatalf("state.json: %v, want it created (as notify does, M2b ruling 4)", err)
	}
}

func TestTraceShowsTheLastTracedVersion(t *testing.T) {
	home := traceCmdHome(t)
	writeLastTraced(t, home, "2.1.282", time.Date(2026, 9, 25, 10, 40, 0, 0, time.Local))
	_, out, _ := runChottag(t, "trace")
	if want := "trace off\nlast traced: Claude Code 2.1.282 at Sep 25 10:40\n"; out != want {
		t.Fatalf("trace = %q, want %q", out, want)
	}
}

func TestTraceJSONDocument(t *testing.T) {
	home := traceCmdHome(t)
	code, out, _ := runChottag(t, "--json", "trace")
	if code != exit.OK {
		t.Fatalf("--json trace = %d", code)
	}
	doc := decodeOneDocument(t, out)
	for _, k := range []string{"tracing", "until", "lastTraced"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("doc = %v, want key %q present (null when unknown)", doc, k)
		}
	}
	if doc["tracing"] != false || doc["until"] != nil || doc["lastTraced"] != nil {
		t.Fatalf("doc = %v, want tracing false, until null, lastTraced null", doc)
	}
	writeLastTraced(t, home, "2.1.282", time.Date(2026, 9, 25, 10, 40, 0, 0, time.UTC))
	before := time.Now()
	code, out, _ = runChottag(t, "--json", "trace", "on", "--for", "30m")
	after := time.Now()
	if code != exit.OK {
		t.Fatalf("--json trace on = %d", code)
	}
	doc = decodeOneDocument(t, out)
	if doc["tracing"] != true {
		t.Fatalf("doc = %v, want tracing true", doc)
	}
	until, err := time.Parse(time.RFC3339, doc["until"].(string))
	lower, upper := untilWindow(before, after, 30*time.Minute)
	if err != nil || until.Before(lower) || until.After(upper) {
		t.Fatalf("until = %v (%v), want within [%v, %v] as RFC3339", doc["until"], err, lower, upper)
	}
	lt, _ := doc["lastTraced"].(map[string]any)
	if lt["claudeVersion"] != "2.1.282" || lt["at"] != "2026-09-25T10:40:00Z" {
		t.Fatalf("lastTraced = %v", doc["lastTraced"])
	}
}

func TestTraceSummarizeReadsSinceTheLastTraceOn(t *testing.T) {
	home := traceCmdHome(t)
	req := func(path string) tracelog.Record {
		return tracelog.Record{Kind: "req", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: path, Class: "serving", Auth: "oauth-access", Status: 200}
	}
	writeTraceRecords(t, home, []tracelog.Record{
		req("/old/one"),
		{Kind: "mark", Mark: tracesum.TraceOnMark},
		req("/old/two"),
		{Kind: "mark", Mark: tracesum.TraceOnMark},
		req("/new/three"),
	})
	code, out, errs := runChottag(t, "trace", "summarize")
	if code != exit.OK || !strings.Contains(out, "/new/three") || strings.Contains(out, "/old/one") || strings.Contains(out, "/old/two") {
		t.Fatalf("summarize = %d; want only /new/three:\n%s\nstderr %q", code, out, errs)
	}
	code, out, _ = runChottag(t, "trace", "summarize", "--all")
	for _, p := range []string{"/old/one", "/old/two", "/new/three"} {
		if code != exit.OK || !strings.Contains(out, p) {
			t.Fatalf("summarize --all = %d, missing %s:\n%s", code, p, out)
		}
	}
}

func TestTraceSummarizeWithNoTraceOnMarkReadsTheWholeFile(t *testing.T) {
	home := traceCmdHome(t)
	writeTraceRecords(t, home, []tracelog.Record{
		{Kind: "mark", Mark: "open usage"},
		{Kind: "req", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: "/run/one", Class: "serving", Status: 200},
	})
	if _, out, _ := runChottag(t, "trace", "summarize"); !strings.Contains(out, "/run/one") || !strings.Contains(out, "open usage") {
		t.Fatalf("summarize = %s, want the whole trace run log", out)
	}
}

func TestTraceOnWithACorruptStateWritesNoMark(t *testing.T) {
	home := cliJSONCorruptState(t)
	code, _, errs := runChottag(t, "trace", "on")
	if code != exit.Error || !strings.Contains(errs, "corrupt") {
		t.Fatalf("trace on = %d; stderr %q, want exit 1 naming the corrupt state.json", code, errs)
	}
	assertAbsent(t, home, "trace.jsonl")
}

func TestTraceUnknownVerbAndStrayArgsAreUsageErrors(t *testing.T) {
	traceCmdHome(t)
	code, _, errs := runChottag(t, "trace", "nosuch")
	if code != exit.Usage || !strings.Contains(errs, `unknown trace command "nosuch"`) || !strings.Contains(errs, "trace [on [--for DUR]|off]") {
		t.Fatalf("trace nosuch = %d; stderr %q", code, errs)
	}
	for _, args := range [][]string{{"trace", "on", "extra"}, {"trace", "off", "extra"}, {"trace", "off", "--for", "1h"}} {
		if code, _, errs := runChottag(t, args...); code != exit.Usage {
			t.Errorf("%q = %d; stderr %q, want exit 2", args, code, errs)
		}
	}
}

func TestFormatTraceUntil(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.Local)
	if got := formatTraceUntil(now.Add(90*time.Minute), now); got != "11:30" {
		t.Errorf("same day = %q, want 11:30", got)
	}
	if got := formatTraceUntil(now.Add(23*time.Hour), now); got != "Sep 26 09:00" {
		t.Errorf("next day = %q, want Sep 26 09:00", got)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "trace shows the switch", command: "trace",
			setup: func(t *testing.T) []string { traceCmdHome(t); return []string{"trace"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["tracing"] != false {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "trace with a corrupt state.json", command: "trace",
			setup:    func(t *testing.T) []string { cliJSONCorruptState(t); return []string{"trace"} },
			wantExit: exit.Error, wantCode: codeInternal,
		},
		jsonCase{
			name: "trace on", command: "trace on",
			setup: func(t *testing.T) []string { traceCmdHome(t); return []string{"trace", "on", "--for", "5m"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["tracing"] != true {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "trace on over the cap", command: "trace on",
			setup:    func(t *testing.T) []string { traceCmdHome(t); return []string{"trace", "on", "--for", "25h"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			name: "trace off", command: "trace off",
			setup: func(t *testing.T) []string { traceCmdHome(t); return []string{"trace", "off"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["tracing"] != false {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "trace off with a corrupt state.json", command: "trace off",
			setup:    func(t *testing.T) []string { cliJSONCorruptState(t); return []string{"trace", "off"} },
			wantExit: exit.Error, wantCode: codeInternal,
		},
		cliJSONRefusal("trace run", "trace", "run"),
		cliJSONRefusal("trace env", "trace", "env"),
		cliJSONRefusal("trace mark", "trace", "mark", "x"),
		cliJSONRefusal("trace summarize", "trace", "summarize"),
	)
}
