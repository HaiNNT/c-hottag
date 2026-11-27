package usagestats

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func sampleReport() Report {
	f := func(v float64) *float64 { return &v }
	reset := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	return Report{
		Since: reset.AddDate(0, -1, 0), Until: reset, By: "month",
		Total:   Tok{Total: 1200, USD: 12.5},
		ByModel: map[string]Tok{"opus": {USD: 10}, "sonnet": {USD: 2.5}},
		Periods: []Period{
			{Key: "2026-10", Tok: Tok{Total: 500, USD: 5}, ByModel: map[string]Tok{"opus": {USD: 5}}},
			{Key: "2026-11", Tok: Tok{Total: 700, USD: 7.5}, ByModel: map[string]Tok{"opus": {USD: 5}, "sonnet": {USD: 2.5}}},
		},
		Accounts: []AccountRow{
			{Name: "<b>x", Tier: "max20x", Units: 20, Price: f(200), PriceSource: "list", Tok: Tok{Total: 900, USD: 10}, ValueRatio: f(0.05), AvgPeak: f(50),
				Weeks: []Week{{Reset: reset, PeakPct: 40, Units: 20}, {Reset: reset.AddDate(0, 0, 7), PeakPct: 100, Limited: true, Units: 20}}},
			{Name: "B", Tier: "pro", Units: 1, Tok: Tok{Total: 300, USD: 2.5, USDUnknown: true},
				Weeks: []Week{{Reset: reset, PeakPct: 20, Units: 1}, {Reset: reset.AddDate(0, 0, 7), PeakPct: 30, Units: 1}}},
		},
		Verdicts:   []Verdict{{Account: "<b>x", Kind: "keep", Text: "fine <i>"}},
		Overall:    Overall{Kind: "keep", Text: "all good"},
		Thresholds: map[string]float64{"weeks": 4, "upgradePeak": 90},
		Notes:      []string{"a note"},
	}
}

func TestRenderHTMLSelfContained(t *testing.T) {
	var b bytes.Buffer
	if err := RenderHTML(&b, sampleReport()); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if strings.Contains(s, "http://") || strings.Contains(s, "https://") {
		t.Fatal("output has an external URL")
	}
	if !strings.Contains(s, "<svg") {
		t.Fatal("no inline svg")
	}
	if !strings.Contains(s, "prefers-color-scheme") {
		t.Fatal("no dark mode")
	}
	if !strings.Contains(s, "ultra") {
		t.Fatal("max20x not shown as ultra")
	}
}

func TestRenderHTMLEscapes(t *testing.T) {
	var b bytes.Buffer
	if err := RenderHTML(&b, sampleReport()); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if strings.Contains(s, "<b>x") || strings.Contains(s, "<i>") {
		t.Fatal("unescaped markup")
	}
	if !strings.Contains(s, "&lt;b&gt;x") {
		t.Fatal("escaped name missing")
	}
}

func TestRenderHTMLEmptyReport(t *testing.T) {
	var b bytes.Buffer
	if err := RenderHTML(&b, Report{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no usage recorded yet") {
		t.Fatal("missing empty message")
	}
}

func TestRenderHTMLHeaderNeverShowsAZeroDate(t *testing.T) {
	r := sampleReport()
	r.Until = time.Time{}
	var b bytes.Buffer
	if err := RenderHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	if s := b.String(); strings.Contains(s, "0001-") || !strings.Contains(s, "to today") {
		t.Fatalf("header = %q", s)
	}
}

func TestRenderHTMLShowsSinceForALateAccount(t *testing.T) {
	r := sampleReport()
	r.Accounts[0].From = r.Since.AddDate(0, 0, 5).Format("2006-01-02")
	var b bytes.Buffer
	if err := RenderHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "since "+r.Accounts[0].From) {
		t.Fatal("no since date")
	}
}
