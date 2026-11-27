package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usageimport"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
	"github.com/HaiNNT/c-hottag/internal/usagestats"
)

const statsUsage = "usage: chottag stats [--by day|month] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--account NAME] [--weeks N] [--json] [--html [--out PATH]] [--reimport]"

// homeProjectsDir is where a Home Claude Code (no CLAUDE_CONFIG_DIR) keeps
// its transcripts. The import reads them O_RDONLY and never writes there. A
// func var so tests point it at a temp dir.
var homeProjectsDir = func() string {
	u, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(u, ".claude", "projects")
}

// statsNow is the clock `stats` reads; a func var so tests fix it.
var statsNow = timeNow

// usageSources are the transcript trees the ledger imports: each account
// slot's projects dir as "slot", Home's as "home".
func usageSources(st store.State) []usageimport.Source {
	var srcs []usageimport.Source
	for _, a := range st.Accounts {
		if a.Dir != "" {
			srcs = append(srcs, usageimport.Source{Dir: filepath.Join(a.Dir, "projects"), Src: "slot"})
		}
	}
	if d := homeProjectsDir(); d != "" {
		srcs = append(srcs, usageimport.Source{Dir: d, Src: "home"})
	}
	return srcs
}

// importLedger imports new transcript usage into the ledger. The caller holds
// the ledger lock. The first import (no import.json yet) takes every
// transcript line; later ones only lines newer than the raw retention cutoff,
// since older days are already rolled up.
func importLedger(ctx context.Context, l usagelog.Ledger, st store.State, now time.Time, reimport bool) (usageimport.Result, error) {
	cutoff := l.RawCutoff(now)
	if _, err := os.Stat(filepath.Join(l.Dir, "import.json")); err != nil {
		if !os.IsNotExist(err) {
			return usageimport.Result{}, err
		}
		cutoff = time.Time{}
	}
	if reimport {
		cutoff = time.Time{}
	}
	return usageimport.Import(ctx, l, usageSources(st), cutoff, reimport)
}

// safeImportErr describes an import failure without a transcript path (a path
// holds project folder names): a file error is reduced to its operation and
// cause, "read: permission denied". The importer's own errors carry no path.
func safeImportErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

// accountInfos maps registered accounts onto the stats input.
func accountInfos(st store.State) []usagestats.AccountInfo {
	out := make([]usagestats.AccountInfo, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		info := usagestats.AccountInfo{Name: a.Name, Rotates: !a.NoRotate}
		if t, ok := autoswitch.ParseTier(strings.ToLower(a.Plan)); ok {
			info.Tier = string(t)
		}
		if a.Units > 0 {
			info.Units = float64(a.Units)
		}
		if a.Price > 0 {
			info.Price, info.PriceSet = a.Price, true
		}
		out = append(out, info)
	}
	return out
}

func parseStatsDate(flagName, s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("--%s must be a date like 2026-11-01, not %q", flagName, s)
	}
	return t, nil
}

// statsResult is `stats --json`: the report's fields, plus the written HTML
// path when --html was given.
type statsResult struct {
	usagestats.Report
	HTML string `json:"html,omitempty"`
}

// runStats imports any new transcript usage, then prints the usage report
// (R203). It reads the ledger under $CHOTTAG_HOME/usage and Claude Code's
// transcripts read-only; it writes only the ledger and, with --html, the
// report file.
func runStats(args []string, r *reporter) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	by := fs.String("by", "", "time series: month (default) or day")
	since := fs.String("since", "", "first day, YYYY-MM-DD")
	until := fs.String("until", "", "last day (inclusive), YYYY-MM-DD")
	account := fs.String("account", "", "only this account")
	weeks := fs.Int("weeks", 4, "complete 7d windows behind each verdict")
	html := fs.Bool("html", false, "write the HTML report")
	out := fs.String("out", "", "with --html: where to write it")
	reimport := fs.Bool("reimport", false, "re-read every transcript")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage(statsUsage)
	}
	bad := func(msg string) int { return r.Fail(exit.Usage, codeUsage, msg, nil) }
	if *by != "" && *by != "day" && *by != "month" {
		return bad(fmt.Sprintf("--by must be day or month, not %q", *by))
	}
	if *weeks < 1 || *weeks > 52 {
		return bad(fmt.Sprintf("--weeks must be a whole number from 1 to 52, not %d", *weeks))
	}
	if *out != "" && !*html {
		return bad("--out needs --html")
	}
	byGiven := *by != ""
	if !byGiven {
		*by = "month"
	}

	now := statsNow()
	var sinceT, untilT time.Time
	if *since != "" {
		if sinceT, err = parseStatsDate("since", *since); err != nil {
			return bad(err.Error())
		}
	} else {
		y, m, d := now.In(time.Local).Date()
		if *by == "day" {
			sinceT = time.Date(y, m, d-30, 0, 0, 0, 0, time.Local)
		} else {
			sinceT = time.Date(y, m-5, 1, 0, 0, 0, 0, time.Local)
		}
	}
	if !byGiven && *since != "" {
		// a short window reads better by day (spec 6.5)
		endT := now
		if *until != "" {
			if u, uerr := parseStatsDate("until", *until); uerr == nil {
				endT = u
			}
		}
		if endT.Sub(sinceT) <= 31*24*time.Hour {
			*by = "day"
		}
	}
	if *until == "" && sinceT.After(now) {
		return bad("--since is in the future")
	}
	if *until != "" {
		u, err := parseStatsDate("until", *until)
		if err != nil {
			return bad(err.Error())
		}
		untilT = u.AddDate(0, 0, 1) // the day itself is included
		if untilT.Before(sinceT) || untilT.Equal(sinceT) {
			return bad("--until is before --since")
		}
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	st, err := (store.Store{Dir: h}).Load()
	if err != nil {
		return r.FailErr(err)
	}
	canonical, pending := *account, error(nil)
	if *account != "" {
		if a, ferr := st.Find(*account); ferr == nil {
			canonical = a.Name
		} else if errors.Is(ferr, store.ErrNotFound) {
			pending = ferr // an account since removed may still be in the ledger
		} else {
			return r.FailErr(ferr)
		}
	}
	l := usagelog.Open(h)
	unlock, locked, err := l.TryLock()
	if err == nil && !locked {
		fmt.Fprintln(r.Stderr(), "chottag: waiting for the usage import in progress...")
		unlock, err = l.Lock()
	}
	if err != nil {
		return r.FailErr(err)
	}
	res, ierr := importLedger(context.Background(), l, st, now, *reimport)
	if ierr != nil {
		// Report from the ledger as it stands: a failed import must not
		// block stats for good.
		r.Warn(warnUsageImport, "chottag: usage import failed ("+safeImportErr(ierr)+"); showing the ledger as it stands")
	}
	if ierr == nil && res.Skipped > 0 {
		r.Warn(warnUsageSkipped, fmt.Sprintf("chottag: %d transcript files or folders could not be read and were skipped", res.Skipped))
	}
	if cerr := l.Compact(now); cerr != nil {
		r.Warn(warnUsageImport, "chottag: usage compaction failed ("+safeImportErr(cerr)+")")
	}
	days, derr := l.Days(time.Time{}, time.Time{})
	snaps, serr := l.Snapshots(time.Time{}, time.Time{})
	unlock()
	if derr != nil {
		return r.FailErr(derr)
	}
	if serr != nil {
		return r.FailErr(serr)
	}
	if pending != nil {
		for _, d := range days {
			if d.Acct != "" && strings.EqualFold(d.Acct, *account) {
				canonical, pending = d.Acct, nil
				break
			}
		}
		if pending != nil {
			return r.FailErr(pending)
		}
	}
	if *reimport && ierr == nil {
		r.Text("reimported %d transcript files (%d usage messages)\n", res.Files, res.Events)
	}

	rep := usagestats.Build(usagestats.Input{
		Days: days, Snaps: snaps, Accounts: accountInfos(st), Now: now,
		Since: sinceT, Until: untilT, By: *by, Weeks: *weeks, Account: canonical,
	})

	result := statsResult{Report: rep}
	if *html {
		path := *out
		if path == "" {
			path = filepath.Join(h, "usage", "report.html")
		}
		if path, err = filepath.Abs(path); err != nil {
			return r.FailErr(err)
		}
		if err := writeStatsHTML(path, rep); err != nil {
			return r.FailErr(err)
		}
		result.HTML = path
		r.Text("%s\n", path)
		return r.OK(result)
	}
	renderStatsText(r.Stdout(), rep, noRotation(st), now)
	return r.OK(result)
}

func writeStatsHTML(path string, rep usagestats.Report) error {
	var b strings.Builder
	if err := usagestats.RenderHTML(&b, rep); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(b.String()), 0o600)
}

// ---- text rendering ----

func humanTokens(n int64) string {
	f := float64(n)
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", f/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", f/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", f/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func money(t usagestats.Tok) string {
	if t.USDUnknown && t.USD == 0 {
		return "?"
	}
	cents := int64(math.Round(t.USD * 100))
	s := fmt.Sprintf("$%s.%02d", commas(cents/100), cents%100)
	if t.USDUnknown {
		s += "+"
	}
	if t.Estimated {
		s += "~"
	}
	return s
}

func tierLabel(t string) string {
	if tt, ok := autoswitch.ParseTier(t); ok {
		return strings.ToLower(tt.DisplayName())
	}
	return "-"
}

func dollars(p *float64) string {
	if p == nil {
		return "-"
	}
	if *p >= 100 || *p == math.Trunc(*p) {
		return fmt.Sprintf("$%.0f", *p)
	}
	return fmt.Sprintf("$%.2f", *p)
}

func pctText(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func ratioText(p *float64) string {
	if p == nil {
		return "-"
	}
	if *p < 1 {
		return fmt.Sprintf("%.2fx", *p)
	}
	return fmt.Sprintf("%.1fx", *p)
}

const sparkRunes = "▁▂▃▄▅▆▇█"

func sparkline(vals []float64) string {
	hi := 0.0
	for _, v := range vals {
		hi = math.Max(hi, v)
	}
	rs := []rune(sparkRunes)
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if hi > 0 {
			i = int(math.Round(v / hi * float64(len(rs)-1)))
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}

// noRotation is the registered accounts kept out of rotation (the remote
// account, R90).
func noRotation(st store.State) map[string]bool {
	m := map[string]bool{}
	for _, a := range st.Accounts {
		if a.NoRotate {
			m[a.Name] = true
		}
	}
	return m
}

func renderStatsText(w io.Writer, rep usagestats.Report, norot map[string]bool, now time.Time) {
	end := rep.Until
	if end.IsZero() || !end.Before(now) {
		end = now
	} else {
		end = end.AddDate(0, 0, -1)
	}
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Usage %s to %s\ttokens\t$ (API)\tshare\n", rep.Since.Format("2006-01-02"), end.Format("2006-01-02"))
	share := func(t usagestats.Tok) string {
		if rep.Total.USD <= 0 {
			return ""
		}
		return fmt.Sprintf("%.0f%%", t.USD/rep.Total.USD*100)
	}
	fmt.Fprintf(tw, "  overall\t%s\t%s\t\n", humanTokens(rep.Total.Total), money(rep.Total))
	models := make([]string, 0, len(rep.ByModel))
	for m := range rep.ByModel {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		a, b := rep.ByModel[models[i]], rep.ByModel[models[j]]
		if a.USD != b.USD {
			return a.USD > b.USD
		}
		return models[i] < models[j]
	})
	for _, m := range models {
		t := rep.ByModel[m]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", m, humanTokens(t.Total), money(t), share(t))
	}
	if rep.Unattributed.Total > 0 || rep.Unattributed.USD > 0 {
		fmt.Fprintf(tw, "  unknown account (history, Home)\t%s\t%s\t\n", humanTokens(rep.Unattributed.Total), money(rep.Unattributed))
	}
	tw.Flush()

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Accounts\tplan\tprice\t$ value\tvalue/price\tavg 7d peak\t$ per 1% (7d)\t$ per 1% of a Pro")
	for _, a := range rep.Accounts {
		name := a.Name
		if norot[a.Name] {
			name += " (remote only)"
		}
		ratio := ratioText(a.ValueRatio)
		if a.ValueRatio != nil && a.From > rep.Since.Format("2006-01-02") {
			ratio += " (since " + a.From + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, tierLabel(a.Tier), dollars(a.Price),
			valueText(a), ratio, pctText(a.AvgPeak), dollars(a.USDPerPct), dollars(a.USDPerUnitPct))
	}
	tw.Flush()
	for _, a := range rep.Accounts {
		if len(a.Weeks) == 0 {
			continue
		}
		peaks := make([]string, len(a.Weeks))
		for i, wk := range a.Weeks {
			peaks[i] = fmt.Sprintf("%.0f", wk.PeakPct)
		}
		fmt.Fprintf(w, "  %s  weekly peak %%: %s\n", a.Name, strings.Join(peaks, " "))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Verdicts")
	for _, v := range rep.Verdicts {
		fmt.Fprintf(w, "  %s  %s\n", v.Account, v.Text)
	}
	if rep.Overall.Text != "" {
		fmt.Fprintf(w, "  overall  %s\n", rep.Overall.Text)
	}

	if len(rep.Periods) > 1 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "By %s\n", rep.By)
		toks := make([]float64, len(rep.Periods))
		for i, p := range rep.Periods {
			toks[i] = float64(p.Tok.Total)
		}
		spark := []rune(sparkline(toks))
		tw = tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
		for i, p := range rep.Periods {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.Key, humanTokens(p.Tok.Total), money(p.Tok), string(spark[i]))
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Thresholds: %s\n", thresholdsText(rep.Thresholds))
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "Note: %s\n", n)
	}
}

func valueText(a usagestats.AccountRow) string {
	if a.Tok.Total == 0 && !a.Tok.USDUnknown {
		return "-"
	}
	return money(a.Tok)
}

func thresholdsText(t map[string]float64) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%g", k, t[k])
	}
	return strings.Join(parts, ", ")
}
