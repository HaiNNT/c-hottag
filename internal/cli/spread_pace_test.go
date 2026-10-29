package cli

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/store"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
)

// paceHeader is the unified rate-limit header set of a response at p5 percent
// of the 5-hour window, resetting at reset.
func paceHeader(p5 float64, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", strconv.FormatFloat(p5/100, 'f', 4, 64))
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// resets sets name's 5h reading: used percent, resetting in d.
func (r *spreadRig) resets(name string, p5 float64, d time.Duration) {
	r.usage(name, p5, 10)
	row := r.rows[name]
	u := *row.Usage
	u.FiveHourResetsAt = r.now.Add(d)
	row.Usage = &u
	r.rows[name] = row
}

func (r *spreadRig) paceBurn(name string) float64 {
	r.eng.paceMu.Lock()
	defer r.eng.paceMu.Unlock()
	b, _ := r.eng.pace.AccountBurn(name, autoswitch.Win5h, r.now)
	return b
}

// R192: the pace meter is fed for every account under every policy, from the
// usage hook; the serial planner's burn keeps its serving-only feed.
func TestPaceFedForEveryAccountUnderSpread(t *testing.T) {
	r := spreadAutoRig(t)
	for i := 0; i < 4; i++ {
		r.now = eligNow.Add(time.Duration(i) * 10 * time.Minute)
		r.as.onUsage("B", 200, paceHeader(float64(10+5*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
		r.as.onUsage("D", 200, paceHeader(float64(10+1*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
	}
	for _, name := range []string{"B", "D"} {
		r.as.spread.paceMu.Lock()
		b, ok := r.as.spread.pace.AccountBurn(name, autoswitch.Win5h, r.now)
		r.as.spread.paceMu.Unlock()
		if !ok || b <= 0 {
			t.Fatalf("pace burn of %s = %v, %v; want a measured burn", name, b, ok)
		}
	}
}

func TestPaceFedUnderSerialAndSerialBurnFeedUnchanged(t *testing.T) {
	r := newAutoRig(t)
	r.as.spread = newSpreadEngine(filepath.Join(r.home, "run", "placements.json"), filepath.Join(r.home, "run", "moves-applied.json"), r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now) // wired, but the policy is serial
	for i := 0; i <= 20; i++ {
		r.now = eligNow.Add(time.Duration(i) * time.Minute)
		r.as.onUsage("B", 200, paceHeader(float64(10+2*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{}) // not serving
	}
	r.as.spread.paceMu.Lock()
	b, ok := r.as.spread.pace.AccountBurn("B", autoswitch.Win5h, r.now)
	r.as.spread.paceMu.Unlock()
	if !ok || b <= 0 {
		t.Fatalf("a serial install did not feed the pace meter for B: %v, %v", b, ok)
	}
	r.as.mu.Lock()
	rate := r.as.burn.Rate()
	r.as.mu.Unlock()
	if rate != 0 {
		t.Fatalf("serial burn = %v after only non-serving responses, want the unfed 0", rate)
	}
	// the serving account still feeds it
	for i := 0; i <= 20; i++ {
		r.now = eligNow.Add(time.Duration(30+i) * time.Minute)
		r.as.onUsage("A", 200, paceHeader(float64(10+2*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
	}
	r.as.mu.Lock()
	rate = r.as.burn.Rate()
	r.as.mu.Unlock()
	if rate < 599 || rate > 601 {
		t.Fatalf("serial burn = %v after serving responses, want 600 (2 points a minute on a max5x)", rate)
	}
}

func TestPaceCreditsTheSessionOnInference(t *testing.T) {
	r := spreadAutoRig(t)
	r.as.onUsage("B", 200, paceHeader(10, r.now.Add(3*time.Hour)), proxy.UsageRequest{SID: "s9", Inference: true})
	r.now = r.now.Add(10 * time.Minute)
	r.as.onUsage("B", 200, paceHeader(20, r.now.Add(3*time.Hour)), proxy.UsageRequest{SID: "s9", Inference: true})
	r.as.onUsage("B", 200, paceHeader(30, r.now.Add(3*time.Hour)), proxy.UsageRequest{SID: "other", Inference: false})
	r.as.spread.paceMu.Lock()
	defer r.as.spread.paceMu.Unlock()
	if _, ok := r.as.spread.pace.Sessions["s9"]; !ok {
		t.Fatal("an inference response with a session id credited no session")
	}
	if _, ok := r.as.spread.pace.Sessions["other"]; ok {
		t.Fatal("a non-inference response credited a session")
	}
}

// B resets in 45 minutes with most of its window left; work has more headroom
// but must last 4h15m: the capacity about to be thrown away goes first.
func TestSpreadPlacesByPace(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	r.resets("B", 26, 45*time.Minute)
	r.resets("work", 9, 4*time.Hour+15*time.Minute)
	if got := r.place("s1"); got != "B" {
		t.Fatalf("placed on %s, want B (soon reset)", got)
	}
}

func TestSpreadBurstSpreads(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		counts[r.place("burst"+strconv.Itoa(i))]++
		r.advance(100 * time.Millisecond)
	}
	if len(counts) < 3 {
		t.Fatalf("five new sessions within a second landed on %v, want all three accounts used", counts)
	}
	for name, n := range counts {
		if n > 2 {
			t.Fatalf("%d of five sessions piled on %s: %v", n, name, counts)
		}
	}
}

// The cold-cache rebalance compares capped pace ratios (1.5x), not headroom:
// B has less headroom but resets in 20 minutes, so it is the better home.
func TestSpreadColdMoveUsesPaceRatio(t *testing.T) {
	setup := func() *spreadRig {
		r := newSpreadRig(t, "B", "work")
		for _, sid := range []string{"s1", "s2", "s3", "s4"} {
			r.place(sid) // B work B work
		}
		return r
	}
	t.Run("a soon reset keeps the session", func(t *testing.T) {
		r := setup()
		r.resets("B", 60, 20*time.Minute) // same use as the next case, but a refill is near
		r.convs["s1"] = 1
		if got := r.place("s1"); got != "B" {
			t.Fatalf("cold session moved to %s though B's ratio is the higher", got)
		}
	})
	t.Run("a ratio 1.5x better takes it", func(t *testing.T) {
		r := setup()
		r.resets("B", 60, 3*time.Hour)
		r.convs["s1"] = 1
		if got := r.place("s1"); got != "work" {
			t.Fatalf("cold session stayed on %s", got)
		}
	})
	t.Run("under 1.5x stays", func(t *testing.T) {
		r := setup()
		r.resets("B", 40, 3*time.Hour)
		r.resets("work", 30, 3*time.Hour)
		r.convs["s1"] = 1
		if got := r.place("s1"); got != "B" {
			t.Fatalf("cold session moved to %s for under 1.5x", got)
		}
	})
}

// R191 under the new score: a chosen session stays through the cold-cache
// rule and a switch point.
func TestChosenStillStaysUnderPace(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r.place(sid)
	}
	r.move("s3", "B")
	r.place("s3")
	r.resets("B", 60, 3*time.Hour)
	r.convs["s3"] = 1
	if got := r.place("s3"); got != "B" {
		t.Fatalf("the chosen cold session moved to %s", got)
	}
	r.usage("B", 95, 10)
	r.eng.mark("B", "B at its 5h switch point", r.now)
	if got := r.place("s3"); got != "B" {
		t.Fatalf("the chosen session left a switch point for %s", got)
	}
}

func paceSnapshot(p5 float64, reset time.Time, now time.Time) usagehdr.Snapshot {
	return usagehdr.Parse(paceHeader(p5, reset), now)
}

func TestPaceJSONPersistsAcrossRestart(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	r.eng.observe("B", 5, paceSnapshot(10, r.now.Add(3*time.Hour), r.now), proxy.UsageRequest{SID: "s1", Inference: true}, r.now)
	r.advance(10 * time.Minute)
	r.eng.observe("B", 5, paceSnapshot(30, r.now.Add(3*time.Hour), r.now), proxy.UsageRequest{SID: "s1", Inference: true}, r.now)
	before := r.paceBurn("B")
	if before <= 0 {
		t.Fatalf("no burn measured: %v", before)
	}
	r.eng.close()
	path := filepath.Join(filepath.Dir(r.path), "pace.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pace.json mode = %v, want 0600", info.Mode().Perm())
	}
	again := r.newEngine()
	again.paceMu.Lock()
	got, ok := again.pace.AccountBurn("B", autoswitch.Win5h, r.now)
	_, hasSession := again.pace.Sessions["s1"]
	again.paceMu.Unlock()
	if !ok || got != before || !hasSession {
		t.Fatalf("after a restart burn = %v, %v (want %v) and session kept %v", got, ok, before, hasSession)
	}
	if r.log.String() != "" {
		t.Fatalf("a clean load logged: %q", r.log.String())
	}
}

func TestPaceJSONSavesOnTheDebounceAndPrunesWithPlacements(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	r.place("s1")
	r.eng.observe("B", 5, paceSnapshot(10, r.now.Add(3*time.Hour), r.now), proxy.UsageRequest{SID: "s1", Inference: true}, r.now)
	r.eng.tick(r.now)
	path := filepath.Join(filepath.Dir(r.path), "pace.json")
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"s1"`) {
		t.Fatalf("tick did not save the session: %v %s", err, raw)
	}
	r.advance(sessions.Grace + time.Minute)
	r.eng.prune(func(string) bool { return false }, r.now)
	r.eng.tick(r.now)
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), `"s1"`) {
		t.Fatalf("an ended session stayed in pace.json: %s", raw)
	}
}

func TestPaceJSONCorruptStartsEmptyAndLogsOnce(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	path := filepath.Join(filepath.Dir(r.path), "pace.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := r.newEngine()
	if len(e.pace.Accounts) != 0 || len(e.pace.Sessions) != 0 {
		t.Fatalf("a corrupt pace.json gave %+v", e.pace)
	}
	if n := strings.Count(r.log.String(), "pace.json is not valid"); n != 1 {
		t.Fatalf("logged %d times: %q", n, r.log.String())
	}
}

// The status document names the pick, each candidate's pace and the pool's
// burn; a serial pool has no pick.
func TestSpreadPublishesNextAndPace(t *testing.T) {
	r := spreadAutoRig(t)
	r.update(func(st *store.State) {
		if err := st.AddPool("work"); err != nil {
			t.Fatal(err)
		}
		if err := st.JoinPool("C", "work"); err != nil {
			t.Fatal(err)
		}
	})
	for i := 0; i < 3; i++ {
		r.now = eligNow.Add(time.Duration(i) * 10 * time.Minute)
		r.as.onUsage("B", 200, paceHeader(float64(10+5*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
		r.as.onUsage("D", 200, paceHeader(float64(10+2*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
	}
	r.as.evaluate(r.now, false)
	pub := r.published()
	if pub.Next == nil || *pub.Next == "" || !strings.HasPrefix(pub.Decision, "spread: next session → "+*pub.Next+" (") {
		t.Fatalf("next %v decision %q", pub.Next, pub.Decision)
	}
	var want float64
	r.as.spread.paceMu.Lock()
	for _, n := range []string{"A", "B", "C", "D"} {
		b, _ := r.as.spread.pace.AccountBurn(n, autoswitch.Win5h, r.now)
		want += b
	}
	r.as.spread.paceMu.Unlock()
	if want <= 0 {
		t.Fatalf("no measured pace to publish: %v", want)
	}
	// "work" is serial here, so the top-level burnRate stays the serial
	// burn (0 until the planner has data), never the pace sum (spec §10)
	if pub.BurnRate == want {
		t.Fatalf("burnRate = %v: a serial pool exists, so it must not be the pace sum", pub.BurnRate)
	}
	if w, ok := pub.Pools["work"]; !ok || w.Next != nil {
		t.Fatalf("serial pool entry %+v, want no next", w)
	}
	f := r.sink.fileCopy()
	seen := 0
	for _, a := range f.Accounts {
		if a.Pace == nil {
			continue
		}
		seen++
		if a.Pace.Ratio < 0 || a.Pace.Ratio > autoswitch.RCap {
			t.Fatalf("%s pace ratio %v out of range", a.Name, a.Pace.Ratio)
		}
	}
	if seen == 0 {
		t.Fatal("no account carries a pace")
	}
}

// The decision line names the account a new session really gets (the pick),
// not servingFor's sticky choice.
func TestEvaluateSpreadDecisionNamesPick(t *testing.T) {
	r := spreadAutoRig(t)
	r.row(busyRow("D", 20, r.now, 3*time.Hour))
	r.row(busyRow("B", 30, r.now, 40*time.Minute))
	r.as.evaluate(r.now, false)
	pub := r.published()
	if pub.Next == nil {
		t.Fatal("spread published no next")
	}
	if !strings.HasPrefix(pub.Decision, "spread: next session → "+*pub.Next+" (") || !strings.HasSuffix(pub.Decision, ")") {
		t.Fatalf("decision %q does not name the pick %q", pub.Decision, *pub.Next)
	}
	in := r.as.spread.pickFor(store.DefaultPool, r.now)
	if !in.OK || in.Name != *pub.Next {
		t.Fatalf("pick %+v vs published next %q", in, *pub.Next)
	}
	if !strings.Contains(pub.Decision, "left") && !strings.Contains(pub.Decision, "soonest reset") && !strings.Contains(pub.Decision, "fewest") {
		t.Fatalf("decision %q has no reason", pub.Decision)
	}
}

// Under an all-spread install the published burnRate is the sum of the default
// pool's members' 5h pace burn, in Burn's units; the account pace shows whole
// numbers.
func TestBurnRateAllSpreadIsPaceSum(t *testing.T) {
	r := spreadAutoRig(t)
	for i := 0; i < 4; i++ {
		r.now = eligNow.Add(time.Duration(i) * 10 * time.Minute)
		r.as.onUsage("B", 200, paceHeader(float64(10+5*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
		r.as.onUsage("D", 200, paceHeader(float64(10+2*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
	}
	r.as.evaluate(r.now, false)
	pub := r.published()
	var want float64
	for _, n := range []string{"A", "B", "C", "D"} {
		r.as.spread.paceMu.Lock()
		b, _ := r.as.spread.pace.AccountBurn(n, autoswitch.Win5h, r.now)
		r.as.spread.paceMu.Unlock()
		want += b
	}
	if want <= 0 || pub.BurnRate != want {
		t.Fatalf("burnRate = %v, want the pace sum %v", pub.BurnRate, want)
	}
	for _, a := range r.sink.fileCopy().Accounts {
		if a.Pace != nil && a.Pace.Burn5h != math.Round(a.Pace.Burn5h) {
			t.Fatalf("%s burn5h %v is not a whole number", a.Name, a.Pace.Burn5h)
		}
	}
}

// Default spread plus a serial pool: burnRate stays autoswitch.Burn's rate,
// which the card's next and `next --pool` read.
func TestBurnRateMixedInstallStaysSerialBurn(t *testing.T) {
	r := spreadAutoRig(t)
	r.update(func(st *store.State) {
		if err := st.AddPool("work"); err != nil {
			t.Fatal(err)
		}
		if err := st.JoinPool("C", "work"); err != nil {
			t.Fatal(err)
		}
	})
	for i := 0; i <= 20; i++ {
		r.now = eligNow.Add(time.Duration(i) * time.Minute)
		r.as.mu.Lock()
		r.as.burn.Observe("C", 100, float64(10+2*i), r.now)
		r.as.mu.Unlock()
		r.as.onUsage("B", 200, paceHeader(float64(10+3*i), r.now.Add(3*time.Hour)), proxy.UsageRequest{})
	}
	r.as.evaluate(r.now, false)
	r.as.mu.Lock()
	want := r.as.burn.Rate()
	r.as.mu.Unlock()
	if want <= 0 {
		t.Fatalf("fixture: serial burn %v", want)
	}
	if got := r.published().BurnRate; got != want {
		t.Fatalf("burnRate = %v, want the serial burn %v", got, want)
	}
}

// Under spread the pick is always published ("" for none); a serial pool
// publishes none.
func TestPublishedNextPresentUnderSpreadAbsentUnderSerial(t *testing.T) {
	r := spreadAutoRig(t)
	r.as.evaluate(r.now, false)
	if r.published().Next == nil {
		t.Fatal("spread published no next")
	}
	r.update(func(st *store.State) { st.Policy = store.PolicySerial })
	r.as.evaluate(r.now.Add(time.Second), false)
	if n := r.published().Next; n != nil {
		t.Fatalf("serial published next %q", *n)
	}
	b, err := json.Marshal(r.published())
	if err != nil || strings.Contains(string(b), `"next"`) {
		t.Fatalf("serial json %s %v", b, err)
	}
}

// pace.json is written whenever the meter changed; placements.json only when
// the placements did.
func TestPlacementsJSONWrittenOnlyWhenPlacementsChange(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	pacePath := filepath.Join(filepath.Dir(r.path), "pace.json")
	r.eng.observe("B", 5, paceSnapshot(10, r.now.Add(3*time.Hour), r.now), proxy.UsageRequest{}, r.now)
	r.eng.tick(r.now)
	if _, err := os.Stat(pacePath); err != nil {
		t.Fatalf("pace.json not written: %v", err)
	}
	if _, err := os.Stat(r.path); err == nil {
		t.Fatal("placements.json written with no placement")
	}
	r.place("s1")
	r.advance(2 * time.Second)
	r.eng.tick(r.now)
	if _, err := os.Stat(r.path); err != nil {
		t.Fatalf("placements.json not written after a placement: %v", err)
	}
	if err := os.Remove(r.path); err != nil {
		t.Fatal(err)
	}
	r.advance(2 * time.Second)
	r.eng.observe("B", 5, paceSnapshot(20, r.now.Add(3*time.Hour), r.now), proxy.UsageRequest{}, r.now)
	r.eng.tick(r.now)
	if _, err := os.Stat(r.path); err == nil {
		t.Fatal("placements.json rewritten though only the pace changed")
	}
}
