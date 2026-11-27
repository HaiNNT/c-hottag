package usagestats

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagecost"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

func init() { loc = time.UTC }

var (
	now    = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	reset0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC) // a Monday
)

func fp(v float64) *float64 { return &v }

// peaks makes one snapshot pair per weekly window ending reset0 + i weeks:
// a low early reading and the peak.
func peaks(acct, tier string, ps ...float64) []usagelog.Snapshot {
	var out []usagelog.Snapshot
	for i, p := range ps {
		r := reset0.AddDate(0, 0, 7*i)
		out = append(out,
			usagelog.Snapshot{T: r.Add(-5 * 24 * time.Hour), Acct: acct, Tier: tier, P7: fp(p / 2), R7: r.Unix()},
			usagelog.Snapshot{T: r.Add(-time.Hour), Acct: acct, Tier: tier, P7: fp(p), R7: r.Unix()},
		)
	}
	return out
}

func opusDay(day, acct string, outTok int64) usagelog.Daily {
	return usagelog.Daily{Day: day, Acct: acct, Model: "claude-opus-5-5", Src: "chottag", N: 1, Out: outTok}
}

func rot(name, tier string) AccountInfo {
	return AccountInfo{Name: name, Tier: tier, Rotates: true}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestTotalsByModelAndPeriod(t *testing.T) {
	days := []usagelog.Daily{
		{Day: "2026-10-30", Acct: "B", Model: "claude-opus-5-5", Src: "chottag", In: 1000, Out: 2000, CR: 5000},
		{Day: "2026-11-03", Acct: "B", Model: "claude-sonnet-5-5", Src: "chottag", In: 4000, Out: 1000},
		{Day: "2026-11-04", Acct: "B", Model: "claude-opus-5-5", Src: "chottag", In: 10, Out: 20},
		{Day: "2026-11-05", Acct: "B", Model: "claude-mystery-9", Src: "chottag", In: 7, Out: 7},
	}
	r := Build(Input{
		Days: days, Accounts: []AccountInfo{rot("B", "max5x")}, Now: now,
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), By: "month",
	})
	cost := func(m string, in, out, cr int64) float64 {
		return usagecost.Cost(m, usagecost.Tokens{In: in, Out: out, CR: cr}).USD
	}
	oct := cost("claude-opus-5-5", 1000, 2000, 5000)
	nov := cost("claude-sonnet-5-5", 4000, 1000, 0) + cost("claude-opus-5-5", 10, 20, 0)
	if len(r.Periods) != 2 || r.Periods[0].Key != "2026-10" || r.Periods[1].Key != "2026-11" {
		t.Fatalf("periods = %+v", r.Periods)
	}
	if !near(r.Periods[0].Tok.USD, oct) || !near(r.Periods[1].Tok.USD, nov) {
		t.Errorf("period USD = %v, %v; want %v, %v", r.Periods[0].Tok.USD, r.Periods[1].Tok.USD, oct, nov)
	}
	if got := r.Periods[1].ByModel["claude-sonnet-5-5"].In; got != 4000 {
		t.Errorf("november sonnet in = %d", got)
	}
	wantOpus := oct + cost("claude-opus-5-5", 10, 20, 0)
	if !near(r.ByModel["claude-opus-5-5"].USD, wantOpus) {
		t.Errorf("opus USD = %v, want %v", r.ByModel["claude-opus-5-5"].USD, wantOpus)
	}
	if r.Total.Total != 1000+2000+5000+4000+1000+30+14 {
		t.Errorf("total tokens = %d", r.Total.Total)
	}
	if !r.Total.USDUnknown || !r.ByModel["claude-mystery-9"].USDUnknown {
		t.Errorf("unknown model not flagged: %+v", r.Total)
	}
	if !near(r.Total.USD, oct+nov) {
		t.Errorf("total USD = %v, want %v", r.Total.USD, oct+nov)
	}
	if !hasNote(r, "claude-mystery-9") {
		t.Errorf("no note naming the unknown model: %v", r.Notes)
	}
}

func hasNote(r Report, sub string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestPeriodsByDayAndClip(t *testing.T) {
	days := []usagelog.Daily{
		opusDay("2026-11-24", "B", 100), opusDay("2026-11-25", "B", 100),
		opusDay("2026-11-26", "B", 100), opusDay("2026-11-27", "B", 100), // after Now
		opusDay("2026-11-23", "B", 100), // before Since
	}
	r := Build(Input{
		Days: days, Accounts: []AccountInfo{rot("B", "pro")}, Now: now, By: "day",
		Since: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	})
	var keys []string
	for _, p := range r.Periods {
		keys = append(keys, p.Key)
	}
	if strings.Join(keys, ",") != "2026-11-24,2026-11-25,2026-11-26" {
		t.Errorf("keys = %v", keys)
	}
}

func TestUnattributedSeparate(t *testing.T) {
	days := []usagelog.Daily{
		opusDay("2026-11-03", "B", 1000),
		{Day: "2026-11-03", Acct: "B", Model: "claude-opus-5-5", Src: "home", Out: 500},
		{Day: "2026-11-03", Acct: "", Model: "claude-opus-5-5", Src: "chottag", Out: 300},
	}
	r := Build(Input{
		Days: days, Accounts: []AccountInfo{rot("B", "max5x")}, Now: now,
		Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	})
	if r.Total.Out != 1800 || r.Unattributed.Out != 800 {
		t.Errorf("total out %d, unattributed %d", r.Total.Out, r.Unattributed.Out)
	}
	if len(r.Accounts) != 1 || r.Accounts[0].Tok.Out != 1000 {
		t.Errorf("accounts = %+v", r.Accounts)
	}
}

func TestWeeksAndUSDPerPct(t *testing.T) {
	// windows end Nov 2, 9, 16, 23; peaks 40, 60, 5
	snaps := peaks("B", "max5x", 40, 60, 5)
	days := []usagelog.Daily{
		opusDay("2026-10-28", "B", 6_000_000), // $120, window ending Nov 2
		opusDay("2026-11-04", "B", 9_000_000), // $180, window ending Nov 9
		opusDay("2026-11-11", "B", 1_000_000), // $20, window ending Nov 16 (peak 5)
	}
	r := Build(Input{
		Days: days, Snaps: snaps, Accounts: []AccountInfo{rot("B", "max5x")}, Now: now,
		Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	})
	row := r.Accounts[0]
	if len(row.Weeks) != 3 {
		t.Fatalf("weeks = %+v", row.Weeks)
	}
	if row.Weeks[0].USDPerPct == nil || !near(*row.Weeks[0].USDPerPct, 3.0) || !near(row.Weeks[0].USD, 120) {
		t.Errorf("week0 = %+v", row.Weeks[0])
	}
	if row.Weeks[1].USDPerPct == nil || !near(*row.Weeks[1].USDPerPct, 3.0) {
		t.Errorf("week1 = %+v", row.Weeks[1])
	}
	if row.Weeks[2].USDPerPct != nil {
		t.Errorf("noise-floor week has $/pct: %+v", row.Weeks[2])
	}
	if row.USDPerPct == nil || !near(*row.USDPerPct, 3.0) {
		t.Errorf("mean $/pct = %v", row.USDPerPct)
	}
	if row.AvgPeak == nil || !near(*row.AvgPeak, 35) {
		t.Errorf("avg peak = %v", row.AvgPeak)
	}
}

func TestIncompleteWindowIgnored(t *testing.T) {
	snaps := append(peaks("B", "pro", 50), usagelog.Snapshot{T: now, Acct: "B", Tier: "pro", P7: fp(80), R7: now.Add(24 * time.Hour).Unix()})
	r := Build(Input{Snaps: snaps, Accounts: []AccountInfo{rot("B", "pro")}, Now: now})
	if len(r.Accounts[0].Weeks) != 1 {
		t.Errorf("weeks = %+v", r.Accounts[0].Weeks)
	}
}

func TestValueRatioProRated(t *testing.T) {
	// 30.4375 day period at $100 plan -> price $100; $300 of usage -> 3x
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Duration(30.4375 * 24 * float64(time.Hour)))
	r := Build(Input{
		Days: []usagelog.Daily{opusDay("2026-11-01", "B", 0), opusDay("2026-11-03", "B", 15_000_000)}, Accounts: []AccountInfo{rot("B", "max5x")},
		Now: until.Add(time.Hour), Since: since, Until: until,
	})
	row := r.Accounts[0]
	if row.From != "2026-11-01" {
		t.Errorf("from = %q", row.From)
	}
	if row.Price == nil || *row.Price != 100 || row.PriceSource != "list" {
		t.Fatalf("price = %v %q", row.Price, row.PriceSource)
	}
	if row.ValueRatio == nil || !near(*row.ValueRatio, 3) {
		t.Errorf("value ratio = %v", row.ValueRatio)
	}
	r = Build(Input{
		Days:     []usagelog.Daily{opusDay("2026-11-01", "B", 0), opusDay("2026-11-03", "B", 15_000_000)},
		Accounts: []AccountInfo{{Name: "B", Tier: "max5x", Price: 50, PriceSet: true, Rotates: true}},
		Now:      until.Add(time.Hour), Since: since, Until: until,
	})
	if r.Accounts[0].PriceSource != "override" || !near(*r.Accounts[0].ValueRatio, 6) {
		t.Errorf("override row = %+v", r.Accounts[0])
	}
}

func findVerdict(t *testing.T, r Report, acct string) Verdict {
	t.Helper()
	for _, v := range r.Verdicts {
		if v.Account == acct {
			return v
		}
	}
	t.Fatalf("no verdict for %s in %+v", acct, r.Verdicts)
	return Verdict{}
}

func build(accts []AccountInfo, snaps ...[]usagelog.Snapshot) Report {
	var all []usagelog.Snapshot
	for _, s := range snaps {
		all = append(all, s...)
	}
	return Build(Input{Snaps: all, Accounts: accts, Now: now})
}

func TestVerdictUpgrade(t *testing.T) {
	r := build([]AccountInfo{rot("C", "max5x")}, peaks("C", "max5x", 92, 95, 88, 96))
	v := findVerdict(t, r, "C")
	if v.Kind != "upgrade" || !strings.Contains(v.Text, "ultra") {
		t.Errorf("verdict = %+v", v)
	}
	// limited twice but a low average
	r = build([]AccountInfo{rot("C", "max5x")}, peaks("C", "max5x", 100, 30, 100, 30))
	if v := findVerdict(t, r, "C"); v.Kind != "upgrade" {
		t.Errorf("limited verdict = %+v", v)
	}
}

func TestVerdictDowngradeUltraToMax5x(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max20x")}, peaks("B", "max20x", 10, 12, 15, 9))
	v := findVerdict(t, r, "B")
	if v.Kind != "downgrade" || !strings.Contains(v.Text, "max5x") {
		t.Errorf("verdict = %+v", v)
	}
	if !near(v.Basis["maxDemandUnits"], 3) {
		t.Errorf("basis = %v", v.Basis)
	}
}

func TestVerdictCancel(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max20x"), rot("C", "pro")},
		peaks("B", "max20x", 40, 30, 50, 20), peaks("C", "pro", 3, 5, 2, 4))
	if v := findVerdict(t, r, "C"); v.Kind != "cancel" {
		t.Errorf("C verdict = %+v", v)
	}
	// alone: nobody covers it
	r = build([]AccountInfo{rot("C", "pro")}, peaks("C", "pro", 3, 5, 2, 4))
	if v := findVerdict(t, r, "C"); v.Kind != "keep" {
		t.Errorf("lone C verdict = %+v", v)
	}
}

func TestVerdictKeep(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max5x")}, peaks("B", "max5x", 60, 70, 55, 65))
	v := findVerdict(t, r, "B")
	if v.Kind != "keep" || len(v.Basis) == 0 {
		t.Errorf("verdict = %+v", v)
	}
	// top tier at a high peak can only keep
	r = build([]AccountInfo{rot("B", "max20x")}, peaks("B", "max20x", 95, 96, 97, 98))
	if v := findVerdict(t, r, "B"); v.Kind != "keep" {
		t.Errorf("ultra verdict = %+v", v)
	}
}

func TestVerdictInsufficient(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max5x")}, peaks("B", "max5x", 95, 96))
	if v := findVerdict(t, r, "B"); v.Kind != "insufficient" {
		t.Errorf("verdict = %+v", v)
	}
}

func TestNoVerdictForRemoteOrUnknownTier(t *testing.T) {
	for _, a := range []AccountInfo{
		{Name: "A", Tier: "team", Rotates: false},
		{Name: "A", Tier: "max5x", Rotates: false},
		{Name: "A", Tier: "max", Rotates: true},
		{Name: "A", Tier: "", Rotates: true},
	} {
		r := build([]AccountInfo{a}, peaks("A", a.Tier, 50, 50, 50, 50))
		v := findVerdict(t, r, "A")
		if v.Kind != "none" || !strings.Contains(v.Text, "chottag plan") {
			t.Errorf("%+v: verdict = %+v", a, v)
		}
		if len(r.Accounts[0].Weeks) != 0 || r.Accounts[0].ValueRatio != nil {
			t.Errorf("%+v: row has weeks or ratio: %+v", a, r.Accounts[0])
		}
	}
}

func TestOverallCheapestSet(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max20x"), rot("C", "max5x")},
		peaks("B", "max20x", 75, 74, 76, 75), peaks("C", "max5x", 20, 20, 20, 20))
	o := r.Overall
	if o.PayUSD != 300 || o.Units != 25 {
		t.Errorf("pay %v units %v", o.PayUSD, o.Units)
	}
	if !near(o.PeakDemand, 16.2) { // 0.76*20 + 0.2*5
		t.Errorf("peak demand = %v", o.PeakDemand)
	}
	if o.Suggest["max20x"] != 1 || o.Suggest["max5x"] != 0 || o.Suggest["pro"] != 0 || o.SuggestUSD != 200 {
		t.Errorf("suggest = %v $%v", o.Suggest, o.SuggestUSD)
	}
	if len(o.Changes) != 1 || o.Changes[0] != "cancel C" || o.Kind != "change" {
		t.Errorf("changes = %v kind %q", o.Changes, o.Kind)
	}
}

func TestOverallKeepAndChanges(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max20x"), rot("C", "max5x")},
		peaks("B", "max20x", 90, 90, 90, 90), peaks("C", "max5x", 90, 90, 90, 90))
	// demand 22.5 *1.15 = 25.9 -> needs 2 ultra (40 units, $400)? 1 ultra+2 max5x = 30u $400 -> tie; fewer accounts wins
	o := r.Overall
	if o.SuggestUSD <= 300 {
		t.Fatalf("suggest = %v $%v", o.Suggest, o.SuggestUSD)
	}
	if o.Kind != "change" || len(o.Changes) == 0 {
		t.Errorf("overall = %+v", o)
	}
	r = build([]AccountInfo{rot("B", "max20x"), rot("C", "max5x")},
		peaks("B", "max20x", 85, 85, 85, 85), peaks("C", "max5x", 85, 85, 85, 85))
	// demand 21.25 *1.15 = 24.4 -> 1 ultra + 1 max5x (25u, $300) is the cheapest; equals current
	o = r.Overall
	if o.Kind != "keep" || len(o.Changes) != 0 || o.SuggestUSD != 300 {
		t.Errorf("overall = %+v", o)
	}
}

func TestOverallDowngradeAndAdd(t *testing.T) {
	// demand 3 units * 1.15 = 3.45: four pros ($80) are within the tie band
	// of one max5x ($100), and fewer accounts win
	r := build([]AccountInfo{rot("B", "max20x")}, peaks("B", "max20x", 10, 12, 15, 9))
	if got := r.Overall.Changes; strings.Join(got, "|") != "downgrade B to max5x" {
		t.Errorf("changes = %v", got)
	}
	// demand 20 * 1.15 = 23 units: ultra + 3 pro ($260) ties with ultra + max5x ($300)
	r = build([]AccountInfo{rot("B", "max20x")}, peaks("B", "max20x", 100, 100, 100, 100))
	if got := r.Overall.Changes; strings.Join(got, "|") != "add one max5x" {
		t.Errorf("changes = %v", got)
	}
	r = build([]AccountInfo{rot("B", "pro"), rot("C", "pro")}, peaks("B", "pro", 100, 100, 100, 100), peaks("C", "pro", 100, 100, 100, 100))
	// demand 2*1.15 = 2.3 -> 3 pros ($60), or a max5x ($100) is outside the band
	if got := r.Overall.Changes; len(got) != 1 || got[0] != "add one pro" {
		t.Errorf("changes = %v", got)
	}
}

func TestOverallInsufficientWhenAnyAccountShort(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max20x"), rot("C", "max5x")}, peaks("B", "max20x", 75, 74, 76, 75))
	o := r.Overall
	if o.Kind != "insufficient" || len(o.Changes) != 0 || !strings.Contains(o.Text, "C") {
		t.Errorf("overall = %+v", o)
	}
	if strings.Contains(o.Text, "B has") {
		t.Errorf("text names the account with enough history: %q", o.Text)
	}
}

func TestWindowUnitsFollowPlanHistory(t *testing.T) {
	// B was max5x for the first two windows (peak 95%), ultra since
	snaps := append(peaks("B", "max5x", 95, 95), peaks("B", "max20x", 10, 10)...)
	// the second batch reuses reset0.. so shift it two weeks on
	for i := 4; i < len(snaps); i++ {
		snaps[i].R7 += int64(14 * 24 * 3600)
		snaps[i].T = snaps[i].T.AddDate(0, 0, 14)
	}
	r := build([]AccountInfo{rot("B", "max20x")}, snaps)
	row := r.Accounts[0]
	if len(row.Weeks) != 4 || row.Weeks[0].Units != 5 || row.Weeks[3].Units != 20 {
		t.Fatalf("weeks = %+v", row.Weeks)
	}
	if !near(r.Overall.PeakDemand, 4.75) {
		t.Errorf("peak demand = %v, want 4.75", r.Overall.PeakDemand)
	}
	if v := findVerdict(t, r, "B"); !near(v.Basis["maxDemandUnits"], 4.75) {
		t.Errorf("basis = %v", v.Basis)
	}
}

func TestSnapshotUnitsWin(t *testing.T) {
	snaps := peaks("B", "max5x", 50, 50, 50, 50)
	for i := range snaps {
		snaps[i].Units = 8
	}
	r := build([]AccountInfo{rot("B", "max5x")}, snaps)
	if r.Accounts[0].Weeks[0].Units != 8 {
		t.Errorf("weeks = %+v", r.Accounts[0].Weeks)
	}
}

func TestUnregisteredAccountText(t *testing.T) {
	r := Build(Input{
		Days: []usagelog.Daily{opusDay("2026-11-03", "Z", 10)}, Now: now,
		Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	v := findVerdict(t, r, "Z")
	if v.Kind != "none" || !strings.Contains(v.Text, "not a registered account") || strings.Contains(v.Text, "rotation is off") {
		t.Errorf("verdict = %+v", v)
	}
}

func TestOverallNotesListPricesWithOverride(t *testing.T) {
	a := AccountInfo{Name: "B", Tier: "max5x", Price: 50, PriceSet: true, Rotates: true}
	r := build([]AccountInfo{a}, peaks("B", "max5x", 50, 50, 50, 50))
	if !strings.Contains(r.Overall.Text, "list prices") {
		t.Errorf("text = %q", r.Overall.Text)
	}
	r = build([]AccountInfo{rot("B", "max5x")}, peaks("B", "max5x", 50, 50, 50, 50))
	if strings.Contains(r.Overall.Text, "list prices") {
		t.Errorf("text = %q", r.Overall.Text)
	}
}

func TestOverallInsufficientWithoutSnapshots(t *testing.T) {
	r := build([]AccountInfo{rot("B", "max5x")})
	if r.Overall.Kind != "insufficient" {
		t.Errorf("overall = %+v", r.Overall)
	}
}

func TestUnitsOverride(t *testing.T) {
	a := AccountInfo{Name: "B", Tier: "max5x", Units: 8, Rotates: true}
	r := build([]AccountInfo{a}, peaks("B", "max5x", 50, 50, 50, 50))
	if !near(r.Overall.PeakDemand, 4) || r.Overall.Units != 8 || r.Accounts[0].Units != 8 {
		t.Errorf("overall = %+v row units %v", r.Overall, r.Accounts[0].Units)
	}
	if v := findVerdict(t, r, "B"); !near(v.Basis["maxDemandUnits"], 4) {
		t.Errorf("basis = %v", v.Basis)
	}
}

func TestNotesAndThresholds(t *testing.T) {
	r := Build(Input{
		Days:     []usagelog.Daily{{Day: "2026-11-03", Acct: "B", Model: "claude-haiku-5-5", Src: "chottag", In: 5}},
		Accounts: []AccountInfo{rot("B", "pro")}, Now: now,
		Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	if !hasNote(r, "capacity model") || !hasNote(r, "claude-haiku-5-5") {
		t.Errorf("notes = %v", r.Notes)
	}
	if r.Thresholds["windows"] != 4 || r.Thresholds["headroom"] != 1.15 || r.Thresholds["upgradeAvgPeak"] != 90 {
		t.Errorf("thresholds = %v", r.Thresholds)
	}
}

func TestAccountFilter(t *testing.T) {
	r := Build(Input{
		Snaps:    append(peaks("B", "max5x", 50, 50, 50, 50), peaks("C", "pro", 50, 50, 50, 50)...),
		Accounts: []AccountInfo{rot("B", "max5x"), rot("C", "pro")}, Now: now, Account: "C",
	})
	if len(r.Accounts) != 1 || r.Accounts[0].Name != "C" || len(r.Verdicts) != 1 || r.Verdicts[0].Account != "C" {
		t.Errorf("accounts %+v verdicts %+v", r.Accounts, r.Verdicts)
	}
	if r.Overall.Units != 6 {
		t.Errorf("overall still fleet-wide, got units %v", r.Overall.Units)
	}
}

func TestCalendarWeekMondayStart(t *testing.T) {
	fixed := time.FixedZone("X", 7*3600)
	old := loc
	loc = fixed
	defer func() { loc = old }()
	// Sunday 23:00 +7 and Monday 01:00 +7 are different calendar weeks
	sun := time.Date(2026, 11, 8, 23, 0, 0, 0, fixed)
	mon := time.Date(2026, 11, 9, 1, 0, 0, 0, fixed)
	if weekKey(sun) == weekKey(mon) {
		t.Errorf("sunday and monday share a week")
	}
	if weekKey(mon) != weekKey(time.Date(2026, 11, 15, 22, 0, 0, 0, fixed)) {
		t.Errorf("monday and next sunday differ")
	}
}

func TestValueRatioStartsAtFirstEvidence(t *testing.T) {
	until := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	since := until.AddDate(0, 0, -178)
	// B first appears 10 days before until: $100 plan over 10 days, not 178.
	days := []usagelog.Daily{opusDay("2026-11-10", "B", 15_000_000)}
	r := Build(Input{Days: days, Accounts: []AccountInfo{rot("B", "max5x")}, Now: until, Since: since, Until: until})
	row := r.Accounts[0]
	if row.From != "2026-11-10" || row.ValueRatio == nil {
		t.Fatalf("row = %+v", row)
	}
	want := 300 / (100 * 10 / daysPerMonth)
	if !near(*row.ValueRatio, want) {
		t.Errorf("ratio = %v, want %v", *row.ValueRatio, want)
	}
	// a snapshot earlier than the first day counts as evidence
	snap := usagelog.Snapshot{T: time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC), Acct: "B", P7: fp(5), R7: 1}
	r = Build(Input{Days: days, Snaps: []usagelog.Snapshot{snap}, Accounts: []AccountInfo{rot("B", "max5x")}, Now: until, Since: since, Until: until})
	if r.Accounts[0].From != "2026-11-05" {
		t.Errorf("from = %q", r.Accounts[0].From)
	}
}

func TestResetsWithinTwoHoursAreOneWindow(t *testing.T) {
	r1 := reset0
	mk := func(off time.Duration, p float64, units float64, at time.Time) usagelog.Snapshot {
		return usagelog.Snapshot{T: at, Acct: "B", Tier: "pro", P7: fp(p), R7: r1.Add(off).Unix(), Units: units}
	}
	snaps := []usagelog.Snapshot{
		mk(0, 40, 1, r1.Add(-48*time.Hour)),
		mk(time.Second, 60, 1, r1.Add(-24*time.Hour)),
		mk(30*time.Minute, 50, 1, r1.Add(-time.Hour)),
		mk(24*7*time.Hour, 20, 1, r1.AddDate(0, 0, 10)),
	}
	r := Build(Input{Snaps: snaps, Accounts: []AccountInfo{rot("B", "pro")}, Now: now})
	w := r.Accounts[0].Weeks
	if len(w) != 2 || w[0].PeakPct != 60 || !w[0].Reset.Equal(r1.Add(30*time.Minute)) {
		t.Fatalf("weeks = %+v", w)
	}
}

func TestHaikuDailySumsNeverUseTheLongTier(t *testing.T) {
	d := usagelog.Daily{Day: "2026-11-20", Acct: "B", Model: "claude-haiku-5-5", Src: "chottag", In: 1_000_000}
	r := Build(Input{Days: []usagelog.Daily{d}, Accounts: []AccountInfo{rot("B", "pro")}, Now: now})
	if !near(r.Total.USD, 0.10) {
		t.Errorf("usd = %v, want the standard rate 0.10", r.Total.USD)
	}
	found := false
	for _, n := range r.Notes {
		found = found || strings.Contains(n, "understated")
	}
	if !found {
		t.Errorf("notes = %v", r.Notes)
	}
}

func TestUSDPerUnitPct(t *testing.T) {
	days := []usagelog.Daily{opusDay("2026-11-05", "B", 1_000_000)}
	r := Build(Input{Days: days, Snaps: peaks("B", "max5x", 50), Accounts: []AccountInfo{rot("B", "max5x")}, Now: now})
	row := r.Accounts[0]
	if row.USDPerPct == nil || row.USDPerUnitPct == nil || !near(*row.USDPerUnitPct, *row.USDPerPct/5) {
		t.Errorf("row = %+v", row)
	}
}

func TestValueRatioCountsAtLeastOneDay(t *testing.T) {
	// B is first seen 2 hours before now: the price is pro-rated over one day,
	// not two hours, so the ratio is not inflated twelvefold.
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	snap := usagelog.Snapshot{T: now.Add(-2 * time.Hour), Acct: "B", P7: fp(1), R7: 1}
	days := []usagelog.Daily{opusDay(now.In(loc).Format("2006-01-02"), "B", 15_000_000)}
	r := Build(Input{Days: days, Snaps: []usagelog.Snapshot{snap}, Accounts: []AccountInfo{rot("B", "max5x")}, Now: now, Since: now.AddDate(0, 0, -30), Until: now})
	row := r.Accounts[0]
	if row.ValueRatio == nil {
		t.Fatalf("row = %+v", row)
	}
	want := row.Tok.USD / (100 * 1 / daysPerMonth)
	if !near(*row.ValueRatio, want) {
		t.Errorf("ratio = %v, want %v (one-day floor)", *row.ValueRatio, want)
	}
}
