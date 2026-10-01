package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// spreadRig is a spreadEngine over an in-memory state and status file, with
// the clock and the tracker's conversation counts under the test's control.
// A is the owner's remote-only account (rotation off, R90).
type spreadRig struct {
	t     *testing.T
	st    store.State
	rows  map[string]status.Account
	convs map[string]int
	now   time.Time
	path  string
	log   *syncBuf
	eng   *spreadEngine
}

func newSpreadRig(t *testing.T, names ...string) *spreadRig {
	t.Helper()
	if len(names) == 0 {
		names = []string{"A", "B", "work", "dev1"}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHOTTAG_HOME", home)
	r := &spreadRig{t: t, rows: map[string]status.Account{}, convs: map[string]int{}, now: eligNow,
		path: filepath.Join(home, "run", "placements.json"), log: newSyncBuf()}
	r.st = store.State{Policy: store.PolicySpread, Serving: names[0]}
	for _, n := range names {
		r.st.Accounts = append(r.st.Accounts, store.Account{Name: n, Dir: filepath.Join(home, "accounts", n), Plan: "max5x", NoRotate: n == "A"})
		r.rows[n] = freshRow(n, 10, 10, eligNow)
	}
	r.eng = r.newEngine()
	return r
}

func (r *spreadRig) newEngine() *spreadEngine {
	return newSpreadEngine(r.path, func() (store.State, error) { return r.st, nil }, r.file,
		func(sid, _ string) int { return r.convs[sid] }, r.log, r.now)
}

func (r *spreadRig) file() status.File {
	f := status.File{}
	for _, a := range r.st.Accounts {
		f.Accounts = append(f.Accounts, r.rows[a.Name])
	}
	return f
}

// usage sets name's 5h and 7d usage, fresh at the rig's current time.
func (r *spreadRig) usage(name string, p5, p7 float64) {
	row := freshRow(name, p5, p7, r.now)
	row.Limited, row.LimitedUntil = r.rows[name].Limited, r.rows[name].LimitedUntil
	r.rows[name] = row
}

func (r *spreadRig) limit(name string) {
	row := freshRow(name, 100, 10, r.now)
	row.Limited, row.LimitedUntil, row.Window = true, r.now.Add(2*time.Hour), "five_hour"
	r.rows[name] = row
}

// needsLogin sets or clears name's needs-login token state.
func (r *spreadRig) needsLogin(name string, on bool) {
	row := r.rows[name]
	row.Token = creds.StateOK
	if on {
		row.Token = creds.StateNeedsLogin
	}
	r.rows[name] = row
}

func (r *spreadRig) advance(d time.Duration) {
	r.now = r.now.Add(d)
	for n := range r.rows {
		row := r.rows[n]
		if row.Usage != nil {
			u := *row.Usage
			u.UpdatedAt = r.now
			row.Usage = &u
			r.rows[n] = row
		}
	}
}

func (r *spreadRig) place(sid string) string {
	r.t.Helper()
	got, ok := r.eng.account(sid, r.now)
	if !ok {
		r.t.Fatalf("account(%q) = no placement", sid)
	}
	return got
}

func (r *spreadRig) counts() map[string]int {
	out := map[string]int{}
	for _, p := range r.eng.snapshot() {
		out[p.Account]++
	}
	return out
}

func (r *spreadRig) noPlacementOn(name string) {
	r.t.Helper()
	for sid, p := range r.eng.snapshot() {
		if p.Account == name {
			r.t.Fatalf("session %s is placed on %s", sid, name)
		}
	}
}

func TestSpreadPlacesStickyAndLeastLoaded(t *testing.T) {
	r := newSpreadRig(t)
	want := []string{"B", "work", "dev1", "B"} // ties go to registration order; A never rotates
	for i, w := range want {
		if got := r.place(fmt.Sprintf("s%d", i+1)); got != w {
			t.Fatalf("session s%d placed on %s, want %s", i+1, got, w)
		}
	}
	// sticky: more load elsewhere changes nothing for a placed session.
	for i := range want {
		if got := r.place(fmt.Sprintf("s%d", i+1)); got != want[i] {
			t.Fatalf("session s%d moved to %s", i+1, got)
		}
	}
	r.noPlacementOn("A")
}

// Review focus 1: a placement survives a daemon restart.
func TestSpreadPlacementsSurviveARestart(t *testing.T) {
	r := newSpreadRig(t)
	want := map[string]string{}
	for _, sid := range []string{"s1", "s2", "s3", "s4", "s5"} {
		want[sid] = r.place(sid)
	}
	r.eng.close()
	info, err := os.Stat(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("placements.json mode = %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(r.path)
	var shape map[string]map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("placements.json: %v", err)
	}
	for _, key := range []string{"account", "at", "movedAt", "conversations"} {
		if _, ok := shape["s1"][key]; !ok {
			t.Fatalf("placements.json s1 lacks %q: %s", key, raw)
		}
	}
	// make the old account the worst choice, so a re-placement would show.
	r.usage("B", 50, 10)
	again := r.newEngine()
	for sid, w := range want {
		got, ok := again.account(sid, r.now)
		if !ok || got != w {
			t.Fatalf("after restart %s = %q, %v; want %q (its prompt cache is warm there)", sid, got, ok, w)
		}
	}
	if r.log.String() != "" {
		t.Fatalf("a clean load logged: %q", r.log.String())
	}
}

func TestSpreadCorruptPlacementsFileStartsEmptyAndLogsOnce(t *testing.T) {
	r := newSpreadRig(t)
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := r.newEngine()
	if got := len(e.snapshot()); got != 0 {
		t.Fatalf("%d placements from a corrupt file", got)
	}
	if n := strings.Count(r.log.String(), "placements.json"); n != 1 {
		t.Fatalf("logged %d times, want once: %q", n, r.log.String())
	}
	if _, ok := e.account("s1", r.now); !ok {
		t.Fatal("the engine does not place after a corrupt load")
	}
}

// Review focus 2: no candidate means no placement and the serving fallback.
func TestSpreadWithNoCandidateFallsBackAndTriesAgain(t *testing.T) {
	r := newSpreadRig(t)
	for _, n := range []string{"B", "work"} {
		r.usage(n, 97, 10)
	}
	r.limit("dev1")
	if got, ok := r.eng.account("s1", r.now); ok {
		t.Fatalf("placed on %q with every account over a point or limited", got)
	}
	if len(r.eng.snapshot()) != 0 {
		t.Fatal("a failed placement was stored")
	}
	r.usage("work", 10, 10)
	if got := r.place("s1"); got != "work" {
		t.Fatalf("next request placed on %q, want work", got)
	}
}

func TestSpreadSessionWithNowhereToGoStaysPut(t *testing.T) {
	r := newSpreadRig(t)
	if got := r.place("s1"); got != "B" {
		t.Fatal(got)
	}
	for _, n := range []string{"B", "work", "dev1"} {
		r.usage(n, 97, 10)
	}
	for i := 0; i < 3; i++ { // no loop, no failure: the session keeps its account
		if got, ok := r.eng.account("s1", r.now); !ok || got != "B" {
			t.Fatalf("request %d: %q, %v; want B", i, got, ok)
		}
	}
}

// Review focus 3: a rotation-off account never gets a session, on any path.
func TestSpreadNeverPlacesOnARotationOffAccount(t *testing.T) {
	t.Run("placement and the pin", func(t *testing.T) {
		r := newSpreadRig(t)
		r.st.Pin = "A"
		if got := r.place("s1"); got == "A" {
			t.Fatal("the pin put a session on the rotation-off account")
		}
		r.noPlacementOn("A")
	})
	t.Run("only rotation-off account left", func(t *testing.T) {
		r := newSpreadRig(t, "A")
		if got, ok := r.eng.account("s1", r.now); ok {
			t.Fatalf("placed on %q", got)
		}
	})
	t.Run("re-placement at a switch point", func(t *testing.T) {
		r := newSpreadRig(t, "A", "B", "work")
		r.st.Pin = "A"
		r.place("s1")
		r.usage("B", 97, 10)
		r.usage("work", 10, 10)
		r.eng.mark("B", "B at its 5h switch point", r.now)
		if got := r.place("s1"); got != "work" {
			t.Fatalf("re-placed on %q", got)
		}
		r.noPlacementOn("A")
	})
	t.Run("the cache-cold move", func(t *testing.T) {
		r := newSpreadRig(t, "A", "B", "work")
		r.st.Pin = "A"
		r.place("s1")
		r.usage("B", 80, 10) // B is a poor home now; A would be the best by score
		r.rows["A"] = freshRow("A", 0, 0, r.now)
		r.convs["s1"] = 1
		r.advance(20 * time.Minute)
		if got := r.place("s1"); got != "work" {
			t.Fatalf("cold move went to %q, want work", got)
		}
		r.noPlacementOn("A")
	})
	t.Run("the wall retry", func(t *testing.T) {
		r := newSpreadRig(t, "A", "B", "work")
		r.place("s1")
		r.limit("B")
		got, ok := r.eng.replace("s1", r.now)
		if !ok || got != "work" {
			t.Fatalf("replace = %q, %v; want work", got, ok)
		}
		r.limit("work")
		if got, ok := r.eng.replace("s1", r.now); ok {
			t.Fatalf("replace with only the rotation-off account left = %q", got)
		}
		r.noPlacementOn("A")
	})
	t.Run("a stored placement on it moves at once, even back to where it came from", func(t *testing.T) {
		r := newSpreadRig(t, "A", "B")
		raw := `{"s1":{"account":"A","at":"2026-09-26T09:00:00Z","movedAt":"2026-09-26T09:59:00Z","from":"B","conversations":0}}`
		if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		e := r.newEngine()
		if got, ok := e.account("s1", r.now); !ok || got != "B" {
			t.Fatalf("got %q, %v; want B: the move window does not hold a session on a rotation-off account", got, ok)
		}
	})
}

// Review focus 5: 50 first requests at once place without a race, exactly.
func TestSpreadConcurrentFirstRequestsCountExactly(t *testing.T) {
	names := make([]string, 10)
	for i := range names {
		names[i] = fmt.Sprintf("acct%02d", i)
	}
	r := newSpreadRig(t, names...)
	var wg sync.WaitGroup
	got := make([]string, 50)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = r.eng.account(fmt.Sprintf("s%02d", i), r.now)
		}()
	}
	wg.Wait()
	for i, g := range got {
		if g == "" {
			t.Fatalf("session %d got no account", i)
		}
	}
	counts := r.counts()
	for _, n := range names {
		if counts[n] != 5 {
			t.Fatalf("counts = %v, want 5 on each", counts)
		}
	}
	if len(r.eng.snapshot()) != 50 {
		t.Fatalf("%d placements, want 50", len(r.eng.snapshot()))
	}
}

func TestSpreadSwitchPointMovesLazilyAndExcludesTheMarkedAccount(t *testing.T) {
	r := newSpreadRig(t)
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r.place(sid) // B work dev1 B
	}
	r.usage("B", 95, 10)
	res := r.eng.mark("B", "B at its 5h switch point", r.now)
	if !res.Newly || res.Sessions != 2 || !reflect.DeepEqual(res.To, []string{"work", "dev1"}) {
		t.Fatalf("mark = %+v, want newly, 2 sessions, to [work dev1]", res)
	}
	if again := r.eng.mark("B", "B at its 5h switch point", r.now); again.Newly {
		t.Fatal("a second mark of the same account counted as a new event")
	}
	if got := r.counts(); got["B"] != 2 {
		t.Fatalf("mark moved sessions eagerly: %v", got) // lazy: at the next request
	}
	if got := r.place("s2"); got != "work" {
		t.Fatalf("an unaffected session moved to %q", got)
	}
	moved := r.place("s1")
	if moved == "B" || moved == "A" {
		t.Fatalf("s1 re-placed on %q", moved)
	}
	if again := r.place("s1"); again != moved {
		t.Fatalf("s1 moved again: %q then %q", moved, again)
	}
	// the account is a candidate again: the mark clears, and nobody is evicted.
	r.usage("B", 10, 10)
	if got := r.place("s4"); got != "B" {
		t.Fatalf("s4 left B after B was a candidate again: %q", got)
	}
	r.eng.unmark("B")
	if res := r.eng.mark("B", "again", r.now); !res.Newly {
		t.Fatal("a mark after the account recovered was not a new event")
	}
}

func TestSpreadMovesRespectTheTenMinuteAndNoMoveBackRules(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	r.place("s1") // B
	r.usage("B", 97, 10)
	r.eng.mark("B", "B at its 5h switch point", r.now)
	if got := r.place("s1"); got != "work" {
		t.Fatal(got)
	}
	// 2 minutes later work crosses its point too: a switch-point move waits out the window.
	r.advance(2 * time.Minute)
	r.usage("work", 97, 10)
	r.eng.mark("work", "work at its 5h switch point", r.now)
	if got := r.place("s1"); got != "work" {
		t.Fatalf("moved again within 10 minutes: %q", got)
	}
	// work needs a login: that is not a switch-point move, so it leaves at once,
	// and it does not go back to B (just left) while B is also over a point.
	r.rows["work"] = freshRow("work", 10, 10, r.now)
	r.usage("B", 10, 10)
	r.needsLogin("work", true)
	got := r.place("s1")
	if got == "work" || got == "B" {
		t.Fatalf("hard move went to %q: must not be the account it is on or the one it just left", got)
	}
	// after the window, B is allowed again.
	r.advance(11 * time.Minute)
	r.needsLogin("work", false)
	r.usage("dev1", 97, 10)
	r.eng.mark("dev1", "x", r.now)
	if got := r.place("s1"); got != "work" && got != "B" {
		t.Fatalf("after the window s1 stayed on %q (over its point)", got)
	}
}

func TestSpreadLimitReplaceIgnoresTheMoveWindowAndMayGoBack(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	r.place("s1") // B
	r.usage("B", 97, 10)
	r.eng.mark("B", "x", r.now)
	if got := r.place("s1"); got != "work" {
		t.Fatal(got)
	}
	r.advance(time.Minute)
	r.usage("B", 10, 10)
	r.limit("work")
	got, ok := r.eng.replace("s1", r.now)
	if !ok || got != "B" {
		t.Fatalf("replace = %q, %v; want a move straight back to B inside the window", got, ok)
	}
	if again := r.place("s1"); again != "B" {
		t.Fatalf("the next request chose %q, not the replaced account", again)
	}
}

func TestSpreadCacheColdMove(t *testing.T) {
	setup := func() *spreadRig {
		r := newSpreadRig(t, "B", "work")
		r.place("s1")
		r.place("s2") // work
		r.place("s3") // B
		r.place("s4") // work
		return r
	}
	t.Run("a clearly better account takes it", func(t *testing.T) {
		r := setup()
		r.usage("B", 60, 10) // B: 0.33*5/3 vs work: 0.9*5/3
		r.convs["s1"] = 1
		if got := r.place("s1"); got != "work" {
			t.Fatalf("cold session stayed on %q", got)
		}
	})
	t.Run("a warm session does not move", func(t *testing.T) {
		r := setup()
		r.usage("B", 60, 10)
		if got := r.place("s1"); got != "B" {
			t.Fatalf("a session whose conversation did not change moved to %q", got)
		}
	})
	t.Run("not clearly better stores the count and stays", func(t *testing.T) {
		r := setup()
		r.usage("B", 20, 10) // 0.73 vs 0.9: below 1.5x
		r.convs["s1"] = 1
		if got := r.place("s1"); got != "B" {
			t.Fatalf("moved to %q for under 1.5x", got)
		}
		if p := r.eng.snapshot()["s1"]; p.Conversations != 1 {
			t.Fatalf("stored conversations = %d, want 1", p.Conversations)
		}
		r.usage("B", 60, 10) // now better by far, but the conversation did not change again
		if got := r.place("s1"); got != "B" {
			t.Fatalf("moved to %q without a new conversation", got)
		}
	})
	t.Run("never inside the move window", func(t *testing.T) {
		r := setup()
		r.usage("B", 97, 10)
		r.eng.mark("B", "x", r.now)
		if got := r.place("s1"); got != "work" {
			t.Fatal(got)
		}
		r.advance(time.Minute)
		r.usage("work", 80, 10)
		r.usage("B", 0, 0)
		r.convs["s1"] = 1
		if got := r.place("s1"); got != "work" {
			t.Fatalf("a cold move inside the window went to %q", got)
		}
	})
}

func TestSpreadPrunesDeadSessionsAfterTheGrace(t *testing.T) {
	r := newSpreadRig(t)
	r.place("s1")
	r.place("s2")
	live := func(sid string) bool { return sid == "s2" }
	r.advance(5 * time.Minute)
	r.eng.prune(live, r.now)
	if len(r.eng.snapshot()) != 2 {
		t.Fatal("pruned inside the grace")
	}
	r.advance(6 * time.Minute)
	r.eng.prune(live, r.now)
	snap := r.eng.snapshot()
	if _, ok := snap["s1"]; ok || len(snap) != 1 {
		t.Fatalf("after the grace: %v", snap)
	}
	// a sid the daemon keeps seeing is never pruned for lack of a registry entry.
	r.eng.account("s2", r.now)
	r.advance(30 * time.Minute)
	r.eng.account("s2", r.now)
	r.eng.prune(func(string) bool { return false }, r.now)
	if len(r.eng.snapshot()) != 1 {
		t.Fatal("a session seen just now was pruned")
	}
}

func TestSpreadSavesDebouncedToOnceASecond(t *testing.T) {
	r := newSpreadRig(t)
	var mu sync.Mutex
	writes := 0
	r.eng.write = func(path string, data []byte, perm os.FileMode) error {
		mu.Lock()
		writes++
		mu.Unlock()
		return os.WriteFile(path, data, perm)
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return writes }
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		t.Fatal(err)
	}
	r.place("s1") // a request only marks the placements dirty
	r.place("s2")
	r.place("s3")
	if count() != 0 {
		t.Fatalf("%d writes from the request path, want 0", count())
	}
	r.eng.tick(r.now) // the roster tick writes, the first change at once
	r.place("s4")
	if count() != 1 {
		t.Fatalf("%d writes, want 1", count())
	}
	r.advance(500 * time.Millisecond)
	r.eng.tick(r.now)
	if count() != 1 {
		t.Fatalf("%d writes before the second is up", count())
	}
	r.advance(time.Second)
	r.eng.tick(r.now)
	if count() != 2 {
		t.Fatalf("%d writes after the debounce, want 2", count())
	}
	r.eng.tick(r.now.Add(time.Hour))
	if count() != 2 {
		t.Fatal("an unchanged engine wrote again")
	}
	raw, _ := os.ReadFile(r.path)
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		if !strings.Contains(string(raw), sid) {
			t.Fatalf("%s missing from the file: %s", sid, raw)
		}
	}
}

// chooser wiring -------------------------------------------------------

type failingPlacer struct{ t *testing.T }

func (f failingPlacer) accountFor(string, string, string, time.Time) (string, bool) {
	f.t.Helper()
	f.t.Error("the chooser asked the spread engine")
	return "", false
}

func (f failingPlacer) fallbackExcluding(string, map[string]bool, time.Time) (string, bool) {
	f.t.Helper()
	f.t.Error("the chooser asked the spread engine for a fallback")
	return "", false
}

type chooseRig struct {
	*spreadRig
	ch *chooser
}

func newChooseRig(t *testing.T, names ...string) *chooseRig {
	r := newSpreadRig(t, names...)
	toks := fakeTokens2{}
	for _, a := range r.st.Accounts {
		toks[a.Dir] = "tok-" + a.Name
	}
	st := func() (store.State, error) { return r.st, nil }
	sel := selector.New(selector.Config{State: st, Tokens: toks, Owners: fakeOwners{}})
	ch := &chooser{sel: sel, state: st, spread: r.eng, now: func() time.Time { return r.now }}
	return &chooseRig{spreadRig: r, ch: ch}
}

// fakeTokens2 maps slot dir to token.
type fakeTokens2 map[string]string

func (f fakeTokens2) Token(_ context.Context, dir string) (string, creds.Status, bool) {
	if tok, ok := f[dir]; ok {
		return tok, creds.Status{State: creds.StateOK}, true
	}
	return "", creds.Status{State: creds.StateNeedsLogin}, false
}

func idCtx(sid string) context.Context {
	return proxy.ContextWithIdentityForTest(context.Background(), proxy.Identity{
		Caller: proxyauth.Caller{SID: sid, Pool: "default"}, Inference: true,
	})
}

var servingDecision = router.Decision{Class: router.Serving}

func (c *chooseRig) choose(ctx context.Context, d router.Decision) string {
	c.t.Helper()
	acct, tok, _, ok := c.ch.Choose(ctx, d, "")
	if !ok {
		c.t.Fatal("Choose: no account")
	}
	if tok != "tok-"+acct {
		c.t.Fatalf("token %q is not %s's", tok, acct)
	}
	return acct
}

func TestChooserPlacesAnIdentifiedSessionUnderSpread(t *testing.T) {
	c := newChooseRig(t)
	c.st.Serving = "dev1"
	if got := c.choose(idCtx("s1"), servingDecision); got != "B" {
		t.Fatalf("identified session on %q, want its placement B (not serving dev1)", got)
	}
	if got := c.choose(idCtx("s2"), servingDecision); got != "work" {
		t.Fatalf("second session on %q, want work", got)
	}
	// an unidentified session uses serving.
	if got := c.choose(context.Background(), servingDecision); got != "dev1" {
		t.Fatalf("unidentified session on %q, want serving dev1", got)
	}
}

func TestChooserRemoteAndOwnerRoutingComeFirst(t *testing.T) {
	c := newChooseRig(t)
	c.st.Remote = "A"
	if got := c.choose(idCtx("s1"), router.Decision{Class: router.Remote}); got != "A" {
		t.Fatalf("remote class on %q, want A", got)
	}
	// owner-routed: the object's creator, and no placement is made for it.
	own := fakeOwners{owner: "work"}
	c.ch.sel = selector.New(selector.Config{State: func() (store.State, error) { return c.st, nil }, Tokens: fakeTokens2{c.st.Accounts[2].Dir: "tok-work", c.st.Accounts[0].Dir: "tok-A"}, Owners: own})
	d := router.Decision{Class: router.Serving, Object: router.KindSession, ObjectID: "x"}
	if got := c.choose(idCtx("s2"), d); got != "work" {
		t.Fatalf("owner-routed on %q, want its creator work", got)
	}
	if len(c.eng.snapshot()) != 0 {
		t.Fatalf("remote and owner requests placed sessions: %v", c.eng.snapshot())
	}
}

func TestChooserWithNoCandidateServesTheServingAccount(t *testing.T) {
	c := newChooseRig(t)
	for _, n := range []string{"B", "work", "dev1"} {
		c.usage(n, 97, 10)
	}
	c.st.Serving = "work"
	for i := 0; i < 2; i++ { // never fails, never loops, never stores
		if got := c.choose(idCtx("s1"), servingDecision); got != "work" {
			t.Fatalf("with no candidate the session went to %q, want serving work", got)
		}
	}
	if len(c.eng.snapshot()) != 0 {
		t.Fatal("a fallback was stored as a placement")
	}
}

// Review focus 4: serial never consults the engine, and flipping back and
// forth leaves nothing that matters.
func TestChooserSerialNeverConsultsTheEngine(t *testing.T) {
	c := newChooseRig(t)
	c.ch.spread = failingPlacer{t}
	c.st.Policy = ""
	c.st.Serving = "dev1"
	for _, ctx := range []context.Context{idCtx("s1"), idCtx("s2"), context.Background()} {
		if got := c.choose(ctx, servingDecision); got != "dev1" {
			t.Fatalf("serial request on %q, want serving dev1", got)
		}
	}
}

func TestSwitchingBackToSerialPutsEverySessionOnServing(t *testing.T) {
	c := newChooseRig(t)
	c.st.Serving = "dev1"
	b := c.choose(idCtx("s1"), servingDecision)
	w := c.choose(idCtx("s2"), servingDecision)
	if b != "B" || w != "work" {
		t.Fatalf("placed on %s, %s", b, w)
	}
	c.st.Policy = ""
	for _, sid := range []string{"s1", "s2"} {
		if got := c.choose(idCtx(sid), servingDecision); got != "dev1" {
			t.Fatalf("serial: %s on %q, want serving dev1", sid, got)
		}
	}
	// back to spread: the still-valid placements are reused, warm.
	c.st.Policy = store.PolicySpread
	if got := c.choose(idCtx("s1"), servingDecision); got != "B" {
		t.Fatalf("after spread again s1 is on %q, want B", got)
	}
	// and an invalid one (B now needs a login) is re-placed instead of reused.
	c.needsLogin("B", true)
	if got := c.choose(idCtx("s1"), servingDecision); got == "B" || got == "A" {
		t.Fatalf("s1 reused an invalid placement: %q", got)
	}
}

// Fix round 1 ------------------------------------------------------------

func TestSpreadColdMoveNeverPingPongsBetweenEqualAccounts(t *testing.T) {
	t.Run("a lone session never moves on new conversations", func(t *testing.T) {
		r := newSpreadRig(t, "B", "work")
		r.place("s1")
		for i := 1; i <= 5; i++ {
			r.advance(11 * time.Minute)
			r.convs["s1"] = i
			if got := r.place("s1"); got != "B" {
				t.Fatalf("conversation %d: a lone session moved to %q for no gain", i, got)
			}
		}
	})
	t.Run("three sessions on two equal accounts do not swap", func(t *testing.T) {
		r := newSpreadRig(t, "B", "work")
		for _, sid := range []string{"s1", "s2", "s3"} {
			r.place(sid) // B work B
		}
		before := r.counts()
		for round := 1; round <= 3; round++ {
			r.advance(11 * time.Minute)
			for _, sid := range []string{"s1", "s2", "s3"} {
				r.convs[sid] = round
				r.place(sid)
			}
		}
		if got := r.counts(); !reflect.DeepEqual(got, before) {
			t.Fatalf("counts changed from %v to %v with nothing to gain", before, got)
		}
		for _, p := range r.eng.snapshot() {
			if !p.MovedAt.IsZero() {
				t.Fatalf("a session moved: %+v", p)
			}
		}
	})
}

func TestSpreadChooserColdPathUsesTheTrackerEndToEnd(t *testing.T) {
	c := newChooseRig(t, "B", "work")
	tr := sessions.NewTracker()
	c.ch.tracker = tr
	c.eng = newSpreadEngine(c.path, func() (store.State, error) { return c.st, nil }, c.file, tr.Peek, c.log, c.now)
	c.ch.spread = c.eng
	ctx := func(native string) context.Context {
		return proxy.ContextWithIdentityForTest(context.Background(), proxy.Identity{
			Caller: proxyauth.Caller{SID: "s1", Pool: "default"}, NativeID: native, Inference: true,
		})
	}
	if got := c.choose(ctx("n1"), servingDecision); got != "B" {
		t.Fatal(got)
	}
	c.usage("B", 70, 10)
	if got := c.choose(ctx("n1"), servingDecision); got != "B" {
		t.Fatalf("same conversation moved to %q", got)
	}
	if got := c.choose(ctx("n2"), servingDecision); got != "work" {
		t.Fatalf("a new conversation on a poor account stayed on %q", got)
	}
}

// R90: with no candidate, a spread session never lands on a rotation-off
// serving account.
func TestChooserNoCandidateFallbackNeverUsesARotationOffServing(t *testing.T) {
	c := newChooseRig(t, "A", "B", "work", "dev1")
	c.st.Serving = "A" // rotation off, carried over from serial
	c.usage("B", 97, 10)
	c.usage("work", 95, 10)
	c.limit("dev1")
	if got := c.choose(idCtx("s1"), servingDecision); got != "work" {
		t.Fatalf("fallback went to %q, want the least-bad rotating account work", got)
	}
	if len(c.eng.snapshot()) != 0 {
		t.Fatal("a fallback was stored as a placement")
	}
	// prefer an account that is not limited over a lower-usage limited one.
	c.usage("B", 99, 10)
	c.usage("work", 99, 10)
	c.limit("dev1")
	c.rows["dev1"] = func() status.Account { x := c.rows["dev1"]; x.Usage.FiveHourPct = pctp(1); return x }()
	if got := c.choose(idCtx("s2"), servingDecision); got != "B" && got != "work" {
		t.Fatalf("fallback went to limited %q", got)
	}
	// a rotating serving account is used as before.
	c.st.Serving = "work"
	if got := c.choose(idCtx("s3"), servingDecision); got != "work" {
		t.Fatalf("rotating serving not used: %q", got)
	}
	// with no rotating account at all, serving is all there is.
	d := newChooseRig(t, "A")
	if got := d.choose(idCtx("s1"), servingDecision); got != "A" {
		t.Fatalf("only account A: got %q", got)
	}
}

func TestSpreadRenameKeepsSessionsWhereTheyAre(t *testing.T) {
	r := newSpreadRig(t)
	if got := r.place("s1"); got != "B" {
		t.Fatal(got)
	}
	if got := r.place("s2"); got != "work" {
		t.Fatal(got)
	}
	r.eng.close()
	// rename B to personal: same slot dir.
	r.st.Accounts[1].Name = "personal"
	r.rows["personal"] = freshRow("personal", 10, 10, r.now)
	delete(r.rows, "B")
	r.st.Serving = "personal"
	e := r.newEngine()
	e.seen["s1"], e.seen["s2"] = r.now, r.now
	if got, ok := e.account("s1", r.now); !ok || got != "personal" {
		t.Fatalf("after the rename s1 = %q, %v; want it to stay on personal", got, ok)
	}
	if p := e.snapshot()["s1"]; !p.MovedAt.IsZero() || p.From != "" {
		t.Fatalf("a rename counted as a move: %+v", p)
	}
	// a file from before dirs resolves by name.
	raw := `{"s9":{"account":"work","at":"2026-09-26T09:00:00Z","movedAt":"0001-01-01T00:00:00Z","conversations":0}}`
	if err := os.WriteFile(r.path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	old := r.newEngine()
	old.seen["s9"] = r.now
	if got, ok := old.account("s9", r.now); !ok || got != "work" {
		t.Fatalf("old-format placement = %q, %v", got, ok)
	}
	if old.snapshot()["s9"].Dir == "" {
		t.Fatal("the dir was not filled in")
	}
}

func TestSpreadLoadCountsOnlyLiveSessions(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r.eng.placements[sid] = &placement{Account: "B", Dir: r.st.Accounts[0].Dir, At: r.now}
	}
	r.eng.dirty = true
	r.eng.close() // simulated: four sessions placed on B before a restart
	e := r.newEngine()
	// none of them has been seen since the restart and the registry does not list them.
	if got := e.snapshot(); len(got) != 4 {
		t.Fatalf("loaded %d placements", len(got))
	}
	if got, ok := e.account("new1", r.now); !ok || got != "B" {
		t.Fatalf("new session placed on %q: stale placements inflated B's load", got)
	}
	e.prune(func(sid string) bool { return sid == "s1" }, r.now.Add(time.Minute))
	if got, _ := e.account("new2", r.now.Add(time.Minute)); got != "work" {
		t.Fatalf("with s1 live and new1 placed, B has 2: new2 on %q, want work", got)
	}
	// past the grace the dead ones are gone.
	e.prune(func(sid string) bool { return sid == "s1" }, r.now.Add(20*time.Minute))
	if n := len(e.snapshot()); n != 1 {
		t.Fatalf("%d placements after the grace, want only s1", n)
	}
}

// Final review fixes ------------------------------------------------------

func TestChooserFallbackPrefersLimitedOverNeedsLogin(t *testing.T) {
	c := newChooseRig(t, "A", "B", "work")
	c.st.Serving = "A"
	c.limit("B")          // limited at 100%
	c.usage("work", 5, 5) // an old low reading
	c.needsLogin("work", true)
	if got := c.choose(idCtx("s1"), servingDecision); got != "B" {
		t.Fatalf("fallback went to %q: a needs-login account has no token, a limited one draws a clean 429", got)
	}
}

func TestSpreadTurningOnKeepsARunningSessionOnItsAccount(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	last := map[string]string{"s1": "dev1", "s2": "dev1", "s3": "work"}
	r.eng.lastAccount = func(sid string) string { return last[sid] }
	for sid, want := range last {
		if got := r.place(sid); got != want {
			t.Fatalf("%s placed on %s, want its running account %s", sid, got, want)
		}
	}
	// a running session on an account that is not a candidate is placed normally.
	r.usage("dev1", 97, 10)
	last["s4"] = "dev1"
	if got := r.place("s4"); got == "dev1" {
		t.Fatal("a session was seeded onto an account over its switch point")
	}
	// a session the tracker never saw is placed by score.
	if got := r.place("new"); got == "" {
		t.Fatal("no placement")
	}
}

func TestSpreadLoadIgnoresEndedSessionsOnceTheRegistryIsKnown(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	for _, sid := range []string{"g1", "g2", "g3"} {
		if got := r.place(sid); got == "" {
			t.Fatal("no placement")
		}
	}
	r.advance(time.Minute)
	// the registry lists nobody: the three have ended (short jobs), though
	// they were seen a minute ago, inside the prune grace.
	r.eng.prune(func(string) bool { return false }, r.now)
	if len(r.eng.snapshot()) != 3 {
		t.Fatal("pruned inside the grace")
	}
	if got := r.eng.counts("g1", r.now); got {
		t.Fatal("an ended session still counts as load")
	}
	// a brand-new session the snapshot does not list yet still counts.
	r.place("fresh")
	if !r.eng.counts("fresh", r.now) {
		t.Fatal("a session seen just now did not count")
	}
}
