//go:build chottag_fakeusage

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

// fakeEnv is a getenv over a fixed map.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeRoster is a state func holding accounts A, B and C, A serving.
func fakeRoster() func() (store.State, error) {
	return func() (store.State, error) {
		st := store.State{}
		for _, n := range []string{"A", "B", "C"} {
			st.Accounts = append(st.Accounts, store.Account{Name: n, Dir: "/slots/" + n})
		}
		st.Serving, st.Remote = "A", "A"
		return st, nil
	}
}

// fakeSink is a sink over a temp home, seeded with A, B and C, whose
// coalescing window is closed for an hour: from here on only a flush
// reaches the disk, so a file that shows the limit proves the flush.
func fakeSink(t *testing.T) (*statusSink, string) {
	t.Helper()
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	seedRosterAtStartup(fakeRoster(), sink)
	sink.mu.Lock()
	sink.interval, sink.lastSave = time.Hour, time.Now()
	sink.mu.Unlock()
	return sink, home
}

func fakeRow(t *testing.T, f status.File, name string) status.Account {
	t.Helper()
	for _, a := range f.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no row %s in %+v", name, f.Accounts)
	return status.Account{}
}

// waitForFile polls status.json until ok accepts it, with a hang guard.
func waitForFile(t *testing.T, home string, ok func(status.File) bool) status.File {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := status.Load(status.Path(home))
		if err == nil && ok(f) {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("status.json never reached the expected state; last = %+v (err %v)", f.Accounts, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestApplyFakeLimitsMarksTheNamedAccounts(t *testing.T) {
	sink, home := fakeSink(t)
	// Non-zero nanoseconds so the LimitedUntil assertion below actually
	// exercises the Truncate(time.Second) in applyFakeLimits (fakeusage_on.go)
	// rather than passing vacuously on an already-whole second.
	now := time.Date(2026, 9, 24, 12, 0, 0, 123456789, time.UTC)
	var log bytes.Buffer
	applyFakeLimits(fakeEnv(map[string]string{
		"CHOTTAG_FAKE_LIMIT":     " b, C ,nobody,,B",
		"CHOTTAG_FAKE_LIMIT_TTL": "30m",
	}), fakeRoster(), sink, &log, now)

	until := now.Add(30 * time.Minute).Truncate(time.Second)
	sink.mu.Lock()
	f := sink.file
	for _, name := range []string{"B", "C"} {
		a := fakeRow(t, f, name)
		if !a.Limited || !a.LimitedUntil.Equal(until) || a.Window != "simulated" || a.Reason != "simulated (CHOTTAG_FAKE_LIMIT)" {
			t.Errorf("%s = %+v, want limited until %v, window simulated, the fake reason", name, a, until)
		}
		if a.Usage == nil || a.Usage.Source != "simulated" || !a.Usage.UpdatedAt.Equal(now) || a.Usage.FiveHourPct != nil || a.Usage.SevenDayPct != nil {
			t.Errorf("%s usage = %+v, want a fresh, empty, simulated stamp", name, a.Usage)
		}
	}
	if a := fakeRow(t, f, "A"); a.Limited || a.Usage != nil {
		t.Errorf("A = %+v, want it untouched", a)
	}
	// The roll-up is recomputed on the write path, as every write does.
	if f.Limits.AllLimited || f.Limits.NextResetAccount != "B" || !f.Limits.NextReset.Equal(until) {
		t.Errorf("limits = %+v, want not all limited, next reset B at %v", f.Limits, until)
	}
	sink.mu.Unlock()

	want := "chottag: fake limit: no account named \"nobody\"; ignored\n" +
		"chottag: fake limit (chottag_fakeusage build) B,C until 2026-09-24T12:30:00Z\n"
	if log.String() != want {
		t.Errorf("log = %q, want %q", log.String(), want)
	}
	// Flushed at once: the coalescing window is closed for an hour.
	waitForFile(t, home, func(f status.File) bool {
		for _, a := range f.Accounts {
			if a.Name == "C" && a.Limited {
				return true
			}
		}
		return false
	})
}

func TestApplyFakeLimitsTTLDefaultsToAnHour(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		ttl     string
		wantLog string
	}{
		{"", ""},
		{"soon", `CHOTTAG_FAKE_LIMIT_TTL="soon" is not a positive duration; using 1h0m0s`},
		{"-5m", `CHOTTAG_FAKE_LIMIT_TTL="-5m" is not a positive duration; using 1h0m0s`},
		{"0s", `CHOTTAG_FAKE_LIMIT_TTL="0s" is not a positive duration; using 1h0m0s`},
	} {
		t.Run(tc.ttl, func(t *testing.T) {
			sink, _ := fakeSink(t)
			var log bytes.Buffer
			applyFakeLimits(fakeEnv(map[string]string{"CHOTTAG_FAKE_LIMIT": "B", "CHOTTAG_FAKE_LIMIT_TTL": tc.ttl}), fakeRoster(), sink, &log, now)
			sink.mu.Lock()
			a := fakeRow(t, sink.file, "B")
			sink.mu.Unlock()
			if !a.LimitedUntil.Equal(now.Add(time.Hour)) {
				t.Errorf("until = %v, want now + 1h", a.LimitedUntil)
			}
			if tc.wantLog != "" && !strings.Contains(log.String(), tc.wantLog) {
				t.Errorf("log = %q, want %q", log.String(), tc.wantLog)
			}
			if tc.wantLog == "" && strings.Contains(log.String(), "duration") {
				t.Errorf("log = %q, want no TTL complaint for an unset TTL", log.String())
			}
		})
	}
}

func TestApplyFakeLimitsDoesNothingWithoutANamedAccount(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{"", " , ", "nobody"} {
		t.Run(raw, func(t *testing.T) {
			sink, _ := fakeSink(t)
			var log bytes.Buffer
			applyFakeLimits(fakeEnv(map[string]string{"CHOTTAG_FAKE_LIMIT": raw}), fakeRoster(), sink, &log, now)
			sink.mu.Lock()
			for _, a := range sink.file.Accounts {
				if a.Limited || a.Usage != nil {
					t.Errorf("%s = %+v, want untouched", a.Name, a)
				}
			}
			sink.mu.Unlock()
			if strings.Contains(log.String(), "until") {
				t.Errorf("log = %q, want no fake-limit line", log.String())
			}
		})
	}
	sink, _ := fakeSink(t)
	var log bytes.Buffer
	applyFakeLimits(fakeEnv(map[string]string{"CHOTTAG_FAKE_LIMIT": "B"}), func() (store.State, error) {
		return store.State{}, errors.New("state.json is unreadable")
	}, sink, &log, now)
	if log.String() != "chottag: fake limit: state.json is unreadable\n" {
		t.Errorf("log = %q, want the state error once", log.String())
	}
}

func TestTaggedBuildVersionSaysFakeusage(t *testing.T) {
	if !strings.HasSuffix(Version, "+fakeusage") {
		t.Fatalf("Version = %q, want the +fakeusage suffix in a tagged build", Version)
	}
	if code, out, _ := runChottag(t, "version"); code != 0 || !strings.HasSuffix(out, "+fakeusage\n") {
		t.Fatalf("version = %d %q, want it to end in +fakeusage", code, out)
	}
}

// TestFakeLimitAtDaemonStartMakesNextExit3 is the checklist item in
// miniature: A is really limited (an observed limit already in the cache),
// B and C are simulated by the tagged daemon at start, B serves. `next`
// then has no account to switch to. The poller is built after the hook,
// so its first look at the cache already sees the simulated limit.
func TestFakeLimitAtDaemonStartMakesNextExit3(t *testing.T) {
	// Every account ends up limited at startup below, so the daemon's
	// startup tick posts the all-limited notice; stub it before anything
	// runs, or TestMain's panicking osascript backstop takes this tagged
	// test binary down (PF1, M2b T5 preflight M1).
	rec := stubDaemonNotifier(t)
	// The daemon's warm pass records an account with no login as needs-login
	// (F269), which `next` would report instead of the simulated limit: give
	// every slot a usable token.
	prevRead := credsReadForTest
	t.Cleanup(func() { credsReadForTest = prevRead })
	credsReadForTest = func(configDir string) (creds.Token, error) {
		return creds.Token{AccessToken: "tok-" + filepath.Base(configDir), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	for _, n := range []string{"A", "B", "C"} {
		addSlotAccount(t, home, n)
	}
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		st.Serving = "B"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Round(0)
	limitA := status.File{Accounts: []status.Account{{
		Name: "A", Limited: true, LimitedUntil: now.Add(2 * time.Hour).Truncate(time.Second), Window: "seven_day",
		Usage: &status.Usage{UpdatedAt: now, Source: "observed"},
	}}}
	b, err := status.Marshal(limitA)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHOTTAG_FAKE_LIMIT", "B,c")

	seenByPoller := make(chan usagepoll.CacheView, 1)
	prev := newDaemonPoller
	t.Cleanup(func() { newDaemonPoller = prev })
	newDaemonPoller = func(_ io.Writer, _ usagepoll.TokenSource, sink *statusSink, _ *url.URL) *usagepoll.Poller {
		seenByPoller <- sink.cached("C", time.Now())
		return nil
	}

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	select {
	case v := <-seenByPoller:
		if !v.Limited || !v.Fresh || v.Until.Before(now.Add(59*time.Minute)) {
			t.Fatalf("the poller's first view of C = %+v, want the simulated limit already applied", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runProxyWithSignal never built the poller")
	}
	if n := strings.Count(errb.String(), "chottag: fake limit (chottag_fakeusage build) B,C until "); n != 1 {
		t.Fatalf("daemon.log = %q, want the fake-limit line once", errb.String())
	}
	waitForFile(t, home, func(f status.File) bool {
		n := 0
		for _, a := range f.Accounts {
			if a.Limited {
				n++
			}
		}
		return n == 3
	})

	// A unit-level twin of the controller's live checklist item (ruling 5):
	// every account limited at startup posted the all-limited notice to
	// the stub instead of osascript.
	select {
	case m := <-rec.got:
		if m.title != "chottag: all accounts limited" {
			t.Fatalf("first notice = %+v, want the all-limited notice", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon's startup tick never posted the all-limited notice")
	}

	code, out, errs := runChottag(t, "next", "--json")
	if code != exit.UserAction {
		t.Fatalf("next = %d, want %d; stdout=%q stderr=%q", code, exit.UserAction, out, errs)
	}
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Skipped []struct {
				Name, Reason string
			} `json:"skipped"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var skipped []string
	for _, s := range doc.Error.Skipped {
		skipped = append(skipped, s.Name+":"+s.Reason)
	}
	if doc.Error.Code != "no_candidate" || strings.Join(skipped, ",") != "C:limited,A:limited" {
		t.Fatalf("next --json error = %+v, want no_candidate skipping C then A (sorted by reset) as limited", doc.Error)
	}
	code, out, _ = runChottag(t, "status", "--json")
	if code != 0 || !strings.Contains(out, `"allLimited": true`) {
		t.Fatalf("status --json = %d %s, want allLimited true", code, out)
	}
}

// The control: the same start without A's real limit leaves A eligible,
// so the exit 3 above is the real limit and the fakes together.
func TestFakeLimitLeavesAnUnlimitedAccountEligible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	for _, n := range []string{"A", "B", "C"} {
		addSlotAccount(t, home, n)
	}
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		st.Serving = "B"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(store.Store{Dir: home})
	seedRosterAtStartup(cache.State, sink)
	applyFakeLimits(fakeEnv(map[string]string{"CHOTTAG_FAKE_LIMIT": "B,C"}), cache.State, sink, io.Discard, time.Now())
	sink.Close()
	if _, err := os.Stat(filepath.Join(home, "cache", "status.json")); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runChottag(t, "next")
	if code != exit.OK || !strings.Contains(out, "serving: A") {
		t.Fatalf("next = %d stdout=%q stderr=%q, want A to take over", code, out, errs)
	}
}

// Review Focus 3: a name is matched exactly (case-insensitively), never by
// the prefix or email resolution `tag` offers. A typo is reported, not
// guessed at.
func TestApplyFakeLimitsNeverMatchesAPrefix(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "acme-max", Email: "p@example.com", Dir: "/slots/p"}}}, nil
	}
	seedRosterAtStartup(state, sink)
	var log bytes.Buffer
	applyFakeLimits(fakeEnv(map[string]string{"CHOTTAG_FAKE_LIMIT": "acme,p@example.com"}), state, sink, &log, time.Now())
	sink.mu.Lock()
	limited := fakeRow(t, sink.file, "acme-max").Limited
	sink.mu.Unlock()
	if limited {
		t.Fatal("a prefix or an email matched acme-max")
	}
	for _, n := range []string{`"acme"`, `"p@example.com"`} {
		if !strings.Contains(log.String(), "no account named "+n+"; ignored") {
			t.Errorf("log = %q, want %s reported as unknown", log.String(), n)
		}
	}
}

// Review Focus 2: a simulated limit is an ordinary limit to `next`. It
// answers knownLimit exactly as an observed limit with the same stamp and
// reset does — under R54/F161, knownLimit honours a known future
// LimitedUntil however old the usage gets, so both stay limited past
// status.StaleAfter too.
func TestASimulatedLimitIsAnOrdinaryLimitForNext(t *testing.T) {
	sink, _ := fakeSink(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	applyFakeLimits(fakeEnv(map[string]string{"CHOTTAG_FAKE_LIMIT": "B"}), fakeRoster(), sink, io.Discard, now)
	sink.mu.Lock()
	f := sink.file
	sink.mu.Unlock()
	observed := status.File{Accounts: []status.Account{{
		Name: "B", Limited: true, LimitedUntil: now.Add(time.Hour), Window: "seven_day",
		Usage: &status.Usage{UpdatedAt: now, Source: "observed"},
	}}}
	for _, at := range []time.Duration{0, 5 * time.Minute, 11 * time.Minute, 61 * time.Minute} {
		su, sl := knownLimit(&f, "B", now.Add(at))
		ou, ol := knownLimit(&observed, "B", now.Add(at))
		if sl != ol || !su.Equal(ou) {
			t.Errorf("at +%v: simulated = (%v, %v), observed = (%v, %v); want the same answer", at, su, sl, ou, ol)
		}
	}
	if _, limited := knownLimit(&f, "B", now.Add(5*time.Minute)); !limited {
		t.Error("the simulated limit does not block next five minutes in")
	}
}
