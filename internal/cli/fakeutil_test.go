//go:build chottag_fakeusage

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

var fakeUtilNow = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

// fakeUtilRoster is a state func holding accounts A, B and C.
func fakeUtilRoster() func() (store.State, error) {
	return func() (store.State, error) {
		st := store.Default()
		for _, n := range []string{"A", "B", "C"} {
			st.Accounts = append(st.Accounts, store.Account{Name: n, Dir: "/slots/" + n})
		}
		st.Serving, st.Remote = "C", "C"
		return st, nil
	}
}

func fakeUtilSink(t *testing.T) *statusSink {
	t.Helper()
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	seedRosterAtStartup(fakeUtilRoster(), sink)
	return sink
}

func fakeUtilRow(t *testing.T, sink *statusSink, name string) status.Account {
	t.Helper()
	f := sink.fileCopyForFakeUtilTest()
	for _, a := range f.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no row %s", name)
	return status.Account{}
}

// fileCopyForFakeUtilTest reads the sink's document under its lock, as a
// deep copy (a JSON round trip).
func (c *statusSink) fileCopyForFakeUtilTest() status.File {
	c.mu.Lock()
	b, err := status.Marshal(c.file)
	c.mu.Unlock()
	var f status.File
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		panic(err)
	}
	return f
}

func TestParseFakeUtil(t *testing.T) {
	specs, err := parseFakeUtil("C:5h=96,7d=40@reset=+20m; A:7d=99", fakeUtilNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %+v", specs)
	}
	c, a := specs[0], specs[1]
	if c.name != "C" || !c.has5 || c.pct5 != 96 || !c.reset5.Equal(fakeUtilNow.Add(20*time.Minute)) ||
		!c.has7 || c.pct7 != 40 || !c.reset7.Equal(fakeUtilNow.Add(20*time.Minute)) {
		t.Fatalf("C = %+v, want 5h 96 and 7d 40, both resetting in 20m", c)
	}
	if a.name != "A" || a.has5 || !a.has7 || a.pct7 != 99 || !a.reset7.Equal(fakeUtilNow.Add(72*time.Hour)) {
		t.Fatalf("A = %+v, want 7d 99 resetting in the default 3 days", a)
	}
	for _, bad := range []string{"C", "C:5h", "C:5h=101", "C:1d=50", "C:5h=90@reset=20m", "C:5h=90@reset=+0s", ":5h=9"} {
		if _, err := parseFakeUtil(bad, fakeUtilNow); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestApplyFakeUtilHoldsTheValueUntilTheReset: the override is written
// fresh at every call, so real traffic in between cannot undo it; once the
// window's reset passes, it stops.
func TestApplyFakeUtilHoldsTheValueUntilTheReset(t *testing.T) {
	sink := fakeUtilSink(t)
	var log bytes.Buffer
	env := map[string]string{"CHOTTAG_FAKE_UTIL": "C:5h=96@reset=+10m"}
	apply := applyFakeUtil(func(k string) string { return env[k] }, fakeUtilRoster(), sink, &log, fakeUtilNow)
	if !strings.Contains(log.String(), "chottag: fake util (chottag_fakeusage build) C 5h=96% until 2026-09-26T10:10:00Z") {
		t.Fatalf("log = %q", log.String())
	}
	apply(fakeUtilNow.Add(time.Minute))
	row := fakeUtilRow(t, sink, "C")
	if row.Usage == nil || row.Usage.FiveHourPct == nil || *row.Usage.FiveHourPct != 96 ||
		!row.Usage.UpdatedAt.Equal(fakeUtilNow.Add(time.Minute)) || !row.Usage.FiveHourResetsAt.Equal(fakeUtilNow.Add(10*time.Minute)) {
		t.Fatalf("C = %+v, want 5h 96%% stamped at +1m, resetting at +10m", row.Usage)
	}
	// Real traffic says 20%; the next call puts 96% back.
	low := 20.0
	sink.mu.Lock()
	for i := range sink.file.Accounts {
		if sink.file.Accounts[i].Name == "C" {
			sink.file.Accounts[i].Usage.FiveHourPct = &low
		}
	}
	sink.mu.Unlock()
	apply(fakeUtilNow.Add(2 * time.Minute))
	if row := fakeUtilRow(t, sink, "C"); *row.Usage.FiveHourPct != 96 {
		t.Fatalf("5h = %v, want the simulated 96 again", *row.Usage.FiveHourPct)
	}
	// Past the reset: the knob stops, and the last real value stays.
	sink.mu.Lock()
	for i := range sink.file.Accounts {
		if sink.file.Accounts[i].Name == "C" {
			sink.file.Accounts[i].Usage.FiveHourPct = &low
		}
	}
	sink.mu.Unlock()
	apply(fakeUtilNow.Add(10 * time.Minute))
	if row := fakeUtilRow(t, sink, "C"); *row.Usage.FiveHourPct != 20 {
		t.Fatalf("5h = %v after the reset, want the knob to have stopped", *row.Usage.FiveHourPct)
	}
}

func TestApplyFakeUtilIgnoresAnUnknownNameAndABadSpec(t *testing.T) {
	sink := fakeUtilSink(t)
	var log bytes.Buffer
	apply := applyFakeUtil(func(string) string { return "Zed:5h=96" }, fakeUtilRoster(), sink, &log, fakeUtilNow)
	apply(fakeUtilNow)
	if !strings.Contains(log.String(), `no account named "Zed"; ignored`) {
		t.Fatalf("log = %q", log.String())
	}
	log.Reset()
	applyFakeUtil(func(string) string { return "C:5h=lots" }, fakeUtilRoster(), sink, &log, fakeUtilNow)(fakeUtilNow)
	if !strings.Contains(log.String(), "chottag: fake util:") || !strings.Contains(log.String(), "ignored") {
		t.Fatalf("log = %q", log.String())
	}
	if row := fakeUtilRow(t, sink, "C"); row.Usage != nil {
		t.Fatalf("C = %+v, want untouched", row.Usage)
	}
}

// A prefix never matches, as with CHOTTAG_FAKE_LIMIT.
func TestApplyFakeUtilNeverMatchesAPrefix(t *testing.T) {
	sink := fakeUtilSink(t)
	var log bytes.Buffer
	applyFakeUtil(func(string) string { return "c:5h=96" }, fakeUtilRoster(), sink, &log, fakeUtilNow)(fakeUtilNow)
	if row := fakeUtilRow(t, sink, "C"); row.Usage == nil {
		t.Fatal("an exact, case-insensitive name did not match")
	}
	log.Reset()
	roster := func() (store.State, error) {
		st, _ := fakeUtilRoster()()
		st.Accounts = append(st.Accounts, store.Account{Name: "Carol", Dir: "/slots/Carol"})
		return st, nil
	}
	applyFakeUtil(func(string) string { return "Ca:5h=96" }, roster, sink, &log, fakeUtilNow)(fakeUtilNow)
	if !strings.Contains(log.String(), `no account named "Ca"`) {
		t.Fatalf("log = %q; a prefix must not match", log.String())
	}
}
