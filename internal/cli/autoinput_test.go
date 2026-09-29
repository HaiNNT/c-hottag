package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

var eligNow = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func pctp(v float64) *float64 { return &v }

// eligHome seeds A (serving) and B, C, D in a temp home, with plans.
func eligHome(t *testing.T) (string, store.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, a := range []store.Account{
			{Name: "A", Plan: "max5x"}, {Name: "B", Plan: "max5x"}, {Name: "C", Plan: "pro"}, {Name: "D", Plan: "max20x"},
		} {
			a.Dir = filepath.Join(home, "accounts", a.Name)
			if err := st.Add(a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return home, s
}

// writeEligStatus writes cache/status.json rows whose usage is stamped at.
func writeEligStatus(t *testing.T, home string, rows ...status.Account) {
	t.Helper()
	writeNotifyStatus(t, home, rows...)
}

func freshRow(name string, pct5, pct7 float64, at time.Time) status.Account {
	return status.Account{Name: name, Usage: &status.Usage{
		FiveHourPct: pctp(pct5), SevenDayPct: pctp(pct7), UpdatedAt: at, Source: "observed",
		FiveHourResetsAt: at.Add(3 * time.Hour), SevenDayResetsAt: at.Add(72 * time.Hour),
	}}
}

func TestTierOfUnitsAndLabel(t *testing.T) {
	cases := []struct {
		a     store.Account
		tier  autoswitch.Tier
		units float64
		label string
	}{
		{store.Account{}, autoswitch.TierMax5x, 5, "-"},
		{store.Account{Plan: "max"}, autoswitch.TierMax5x, 5, "max?"},
		{store.Account{Plan: "pro"}, autoswitch.TierPro, 1, "pro"},
		{store.Account{Plan: "max20x"}, autoswitch.TierMax20x, 20, "max20x"},
		{store.Account{Plan: "team", Units: 8}, autoswitch.TierTeam, 8, "team"},
		{store.Account{Plan: "enterprise"}, autoswitch.TierMax5x, 5, "enterprise"},
	}
	for _, c := range cases {
		if got := tierOf(c.a); got != c.tier {
			t.Errorf("tierOf(%+v) = %q, want %q", c.a, got, c.tier)
		}
		if got := unitsOf(c.a); got != c.units {
			t.Errorf("unitsOf(%+v) = %v, want %v", c.a, got, c.units)
		}
		if got := planLabel(c.a); got != c.label {
			t.Errorf("planLabel(%+v) = %q, want %q", c.a, got, c.label)
		}
	}
}

func TestPlanFromSubscription(t *testing.T) {
	for sub, want := range map[string]string{"pro": "pro", "team": "team", "max": "max", "Max": "max", "enterprise": "", "": ""} {
		if got := planFromSubscription(sub); got != want {
			t.Errorf("planFromSubscription(%q) = %q, want %q", sub, got, want)
		}
	}
}

// TestAutoParamsSkipsAnInvalidStoredOverride is Review Focus 5: a
// hand-edited state.json with an unknown mode, an out-of-range switch
// point, an unknown key and an unparseable duration keeps every valid
// override and the preset value for each bad one.
func TestAutoParamsSkipsAnInvalidStoredOverride(t *testing.T) {
	st := store.Default()
	st.Auto = &store.Auto{
		Mode:         "turbo",
		SwitchPoints: map[string]int{"5h.pro": 20, "5h.max5x": 90, "5h.max": 90},
		Hold5h:       "10m",
		Cooldown:     "forever",
	}
	p := autoParams(st)
	if p.Mode != autoswitch.ModeBalanced {
		t.Fatalf("mode = %q, want balanced for an unknown mode", p.Mode)
	}
	if p.SwitchPoint(autoswitch.Win5h, autoswitch.TierPro) != 88 || p.SwitchPoint(autoswitch.Win5h, autoswitch.TierMax5x) != 90 {
		t.Fatalf("points = %v, want 5h.pro kept at 88 and 5h.max5x overridden to 90", p.SwitchPoints())
	}
	if p.Hold5h != 10*time.Minute || p.Cooldown != 15*time.Minute {
		t.Fatalf("hold5h %v cooldown %v, want 10m and the preset 15m", p.Hold5h, p.Cooldown)
	}
}

func TestAutoParamsAppliesOverridesOnTopOfEitherPreset(t *testing.T) {
	st := store.Default()
	st.SetAutoMode("cache-optimize")
	_ = st.SetAutoSetting("5h.max20x", "95")
	p := autoParams(st)
	if p.Mode != autoswitch.ModeCacheOptimize || p.SwitchPoint(autoswitch.Win5h, autoswitch.TierMax20x) != 95 || p.SwitchPoint(autoswitch.Win5h, autoswitch.TierPro) != 100 {
		t.Fatalf("params = %+v %v", p, p.SwitchPoints())
	}
}

func TestPlanAccountsReadsStateAndCache(t *testing.T) {
	st := store.Default()
	for _, a := range []store.Account{{Name: "A", Plan: "max20x"}, {Name: "B", NoRotate: true, Units: 7}, {Name: "C"}} {
		a.Dir = "/slots/" + a.Name
		st.Accounts = append(st.Accounts, a)
	}
	a := freshRow("A", 91, 40, eligNow.Add(-time.Minute))
	a.Limited, a.LimitedUntil, a.Window = true, eligNow.Add(time.Hour), "five_hour"
	b := freshRow("B", 50, 50, eligNow.Add(-time.Hour)) // stale
	b.Token = creds.StateNeedsLogin
	f := &status.File{Accounts: []status.Account{a, b}}
	got := planAccounts(&st, f, eligNow)
	if len(got) != 3 {
		t.Fatalf("accounts = %+v", got)
	}
	ga, gb, gc := got[0], got[1], got[2]
	if ga.Tier != autoswitch.TierMax20x || ga.Units != 20 || !ga.Fresh || !ga.Has5h || ga.Pct5h != 91 || !ga.Has7d || ga.Pct7d != 40 ||
		!ga.Limited || !ga.LimitedUntil.Equal(eligNow.Add(time.Hour)) || ga.LimitWindow != autoswitch.Win5h ||
		!ga.Reset5h.Equal(eligNow.Add(-time.Minute).Add(3*time.Hour)) || !ga.Reset7d.Equal(eligNow.Add(-time.Minute).Add(72*time.Hour)) {
		t.Fatalf("A = %+v", ga)
	}
	if gb.Rotates || gb.Units != 7 || gb.Fresh || !gb.NeedsLogin || gb.Limited {
		t.Fatalf("B = %+v, want out of rotation, 7 units, stale, needs login", gb)
	}
	if gc.Has5h || gc.Fresh || !gc.Rotates || gc.Tier != autoswitch.TierMax5x {
		t.Fatalf("C = %+v, want unknown usage, in rotation, max5x", gc)
	}
	if none := planAccounts(&st, nil, eligNow); len(none) != 3 || none[0].Fresh || none[0].Limited {
		t.Fatalf("no cache = %+v, want every account unknown", none)
	}
}

// TestPlanAccountsClearsANeedsLoginStaleFromBeforeTheLastLogin is the
// controller ruling that supersedes plan ruling 3: only the daemon's
// passthrough hook writes status.json's needs-login token state, and
// nothing else clears it, so a `chottag login` must not leave a stale
// needs-login outliving the re-login it just did. planAccounts compares
// the account's own LoggedInAt (store.json) against the row's TokenAt
// (status.json, fix round 1 item 1).
func TestPlanAccountsClearsANeedsLoginStaleFromBeforeTheLastLogin(t *testing.T) {
	base := eligNow.Add(-time.Hour)
	cases := []struct {
		name       string
		tokenAt    time.Time
		loggedInAt time.Time
		want       bool
	}{
		{"needs-login recorded, then a login", base, base.Add(30 * time.Minute), false},
		{"a login, then needs-login recorded later", base, base.Add(-30 * time.Minute), true},
		{"an old row with zero TokenAt and a non-zero LoggedInAt", time.Time{}, base, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := store.Default()
			st.Accounts = []store.Account{{Name: "B", Dir: "/slots/B", LoggedInAt: c.loggedInAt}}
			row := status.Account{Name: "B", Token: creds.StateNeedsLogin, TokenAt: c.tokenAt}
			f := &status.File{Accounts: []status.Account{row}}
			got := planAccounts(&st, f, eligNow)
			if got[0].NeedsLogin != c.want {
				t.Fatalf("NeedsLogin = %v, want %v", got[0].NeedsLogin, c.want)
			}
		})
	}
}

// TestNextPicksAnAccountAfterAReLoginClearsItsStaleNeedsLogin drives
// nextCandidate end to end for the first case above: B's needs-login row
// predates its later login, so next must treat it as eligible again.
func TestNextPicksAnAccountAfterAReLoginClearsItsStaleNeedsLogin(t *testing.T) {
	st := store.Default()
	for _, a := range []store.Account{{Name: "A"}, {Name: "B", LoggedInAt: eligNow}} {
		a.Dir = "/slots/" + a.Name
		st.Accounts = append(st.Accounts, a)
	}
	st.Serving = "A"
	row := status.Account{Name: "B", Token: creds.StateNeedsLogin, TokenAt: eligNow.Add(-time.Hour)}
	f := &status.File{Accounts: []status.Account{row}}
	got, skips, _, err := nextCandidate(&st, f, eligNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "B" || len(skips) != 0 {
		t.Fatalf("next = %s, skips %v; want B with no skips", got.Name, skips)
	}
}

// TestTierOfPlanLabelAndModeAreCaseInsensitive is fix round 1 item 5: a
// hand-edited state.json can carry "Max20x" or "Cache-Optimize", and every
// stored-value parser here must fold case before comparing.
func TestTierOfPlanLabelAndModeAreCaseInsensitive(t *testing.T) {
	if got := tierOf(store.Account{Plan: "Max20x"}); got != autoswitch.TierMax20x {
		t.Errorf("tierOf(Max20x) = %q, want max20x", got)
	}
	if got := planLabel(store.Account{Plan: "MAX"}); got != "max?" {
		t.Errorf("planLabel(MAX) = %q, want max?", got)
	}
	st := store.Default()
	st.Auto = &store.Auto{Mode: "Cache-Optimize"}
	if p := autoParams(st); p.Mode != autoswitch.ModeCacheOptimize {
		t.Errorf("mode = %q, want cache-optimize for a hand-edited Cache-Optimize", p.Mode)
	}
}

// TestNextAndThePlannerAgreeOnEligibility is spec §9's agreement test:
// `next` skips exactly the accounts autoswitch.Eligible rejects, for the
// same reasons; and in cache-optimize (registration order) the planner's
// wall target is the account `next` picks.
func TestNextAndThePlannerAgreeOnEligibility(t *testing.T) {
	st := store.Default()
	for _, a := range []store.Account{
		{Name: "A", Plan: "max5x"}, {Name: "B", Plan: "max5x"}, {Name: "C", NoRotate: true},
		{Name: "D", Plan: "pro"}, {Name: "E"}, {Name: "F", Plan: "max20x"}, {Name: "G"},
	} {
		a.Dir = "/slots/" + a.Name
		st.Accounts = append(st.Accounts, a)
	}
	st.Serving = "A"
	at := eligNow.Add(-time.Minute)
	servingRow := freshRow("A", 100, 50, at)
	servingRow.Limited, servingRow.LimitedUntil, servingRow.Window = true, eligNow.Add(time.Hour), "five_hour"
	b := freshRow("B", 95, 10, at) // above max5x's 93
	d := freshRow("D", 10, 10, at)
	d.Token = creds.StateNeedsLogin
	e := freshRow("E", 10, 10, at)
	e.Limited, e.LimitedUntil = true, eligNow.Add(2*time.Hour)
	f := &status.File{Accounts: []status.Account{servingRow, b, d, e, freshRow("F", 97, 10, at)}}

	params := autoParams(st)
	accts := planAccounts(&st, f, eligNow)
	want := map[string]string{}
	for _, a := range accts[1:] {
		if r := autoswitch.Eligible(a, params, eligNow); r != "" {
			want[a.Name] = r
		}
	}
	got, skips, _, err := nextCandidate(&st, f, eligNow, false)
	if err != nil {
		t.Fatal(err)
	}
	gotSkips := map[string]string{}
	for _, sk := range skips {
		gotSkips[sk.Name] = sk.Reason
	}
	// next stops at the first eligible account (F: max20x at 97% < 98%),
	// so it has seen B, C, D, E; G was never reached.
	delete(want, "G")
	if got.Name != "F" || len(gotSkips) != len(want) {
		t.Fatalf("next = %s, skips %v; the planner rejects %v", got.Name, gotSkips, want)
	}
	for name, reason := range want {
		if gotSkips[name] != reason {
			t.Errorf("%s: next says %q, the planner %q", name, gotSkips[name], reason)
		}
	}
	st.SetAutoMode("cache-optimize")
	d2 := autoswitch.Plan(autoswitch.Input{Now: eligNow, Enabled: true, Params: autoParams(st), Serving: "A", Accounts: planAccounts(&st, f, eligNow)})
	n2, _, _, err := nextCandidate(&st, f, eligNow, false)
	if err != nil || d2.Action != autoswitch.ActionSwitch || d2.Target != n2.Name {
		t.Fatalf("cache-optimize: planner %+v, next %s (%v); want the same target", d2, n2.Name, err)
	}
}

// TestNextSkipsAnAccountAboveItsSwitchPoint drives the command: B is at
// 95% of its 5h window, over max5x's 93%, so next goes to C and says why.
func TestNextSkipsAnAccountAboveItsSwitchPoint(t *testing.T) {
	home, s := eligHome(t)
	now := time.Now()
	writeEligStatus(t, home, freshRow("B", 95, 10, now))
	code, out, errs := runChottag(t, "next")
	if code != exit.OK || !strings.Contains(out, "skipped B (above switch point)") || !strings.Contains(out, "serving: C") {
		t.Fatalf("next = %d %q; stderr %q", code, out, errs)
	}
	if st, _ := s.Load(); st.Serving != "C" {
		t.Fatalf("serving = %q", st.Serving)
	}
}

// TestNextSkipsAnAccountAboveItsWeeklySwitchPoint pins the 7d mapping end to
// end (fix round 1 item 2): B's 5h is comfortably below its point, but its
// 7d is at max5x's 98% point, so next skips it on Win7d the same way.
func TestNextSkipsAnAccountAboveItsWeeklySwitchPoint(t *testing.T) {
	home, s := eligHome(t)
	now := time.Now()
	writeEligStatus(t, home, freshRow("B", 10, 99, now))
	code, out, errs := runChottag(t, "next")
	if code != exit.OK || !strings.Contains(out, "skipped B (above switch point)") || !strings.Contains(out, "serving: C") {
		t.Fatalf("next = %d %q; stderr %q", code, out, errs)
	}
	if st, _ := s.Load(); st.Serving != "C" {
		t.Fatalf("serving = %q", st.Serving)
	}
}

// The JSON skip token for the two new reasons.
func TestNextJSONNamesTheNewSkipReasons(t *testing.T) {
	home, _ := eligHome(t)
	now := time.Now()
	c := freshRow("C", 10, 10, now)
	c.Token = creds.StateNeedsLogin
	writeEligStatus(t, home, freshRow("B", 95, 10, now), c)
	code, out, _ := runChottag(t, "next", "--json")
	if code != exit.OK {
		t.Fatalf("next --json = %d %s", code, out)
	}
	var doc struct {
		Serving string `json:"serving"`
		Skipped []struct {
			Name, Reason string
		} `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Serving != "D" || len(doc.Skipped) != 2 || doc.Skipped[0].Reason != "above_switch_point" || doc.Skipped[1].Reason != "needs_login" {
		t.Fatalf("doc = %+v, want D with B above_switch_point and C needs_login", doc)
	}
}

// --force ignores switch points, login state and limits, and still honours
// rotation (ruling 6): D also carries a needs-login token, and --force
// still lands on it.
func TestNextForceIgnoresSwitchPointsButNotRotation(t *testing.T) {
	home, s := eligHome(t)
	if _, err := s.Update(func(st *store.State) error { st.Accounts[2].NoRotate = true; return nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	d := freshRow("D", 10, 10, now)
	d.Token = creds.StateNeedsLogin
	writeEligStatus(t, home, freshRow("B", 99, 10, now), d)
	if code, out, errs := runChottag(t, "next", "--force"); code != exit.OK || !strings.Contains(out, "serving: B") {
		t.Fatalf("next --force = %d %q %q, want B", code, out, errs)
	}
	if code, out, _ := runChottag(t, "next", "--force"); code != exit.OK || !strings.Contains(out, "skipped C (out of rotation)") || !strings.Contains(out, "serving: D") {
		t.Fatalf("second next --force = %d %q, want C skipped and D despite its needs-login token", code, out)
	}
}
