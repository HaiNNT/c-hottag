//go:build chottag_fakeusage

// This file is compiled only into a binary built with
// -tags chottag_fakeusage (M4 spec §8, S12), like fakeusage_on.go. It lets
// the live check drive an account's utilization to a chosen percentage,
// which real usage cannot do on demand. fakeutil_off.go is its no-op twin,
// and cmd/chottag's TestReleaseBinaryHasNoFakeLimitHook proves the twin is
// what ships.

package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const fakeUtilEnv = "CHOTTAG_FAKE_UTIL"

// Default resets when a spec gives none: a 5-hour window resets in 3h, a
// 7-day window in 3 days.
const (
	fakeUtilReset5h = 3 * time.Hour
	fakeUtilReset7d = 72 * time.Hour
)

// fakeUtilSpec is one account's simulated utilization, with absolute reset
// times fixed at daemon start.
type fakeUtilSpec struct {
	name         string
	has5, has7   bool
	pct5, pct7   float64
	reset5       time.Time
	reset7       time.Time
	name5, name7 string // for the log line
}

// parseFakeUtil reads CHOTTAG_FAKE_UTIL's value, one account per
// ';'-separated item: NAME:5h=PCT[,7d=PCT][@reset=+DURATION]. A reset
// applies to every window the item names. Names are returned as written;
// the caller resolves them against the roster.
func parseFakeUtil(raw string, now time.Time) ([]fakeUtilSpec, error) {
	var out []fakeUtilSpec
	for _, item := range strings.Split(raw, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, body, ok := strings.Cut(item, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%q: want NAME:5h=PCT[,7d=PCT][@reset=+DUR]", item)
		}
		windows, opts, _ := strings.Cut(body, "@")
		var resetIn time.Duration
		if opts != "" {
			v, ok := strings.CutPrefix(opts, "reset=+")
			d, err := time.ParseDuration(v)
			if !ok || err != nil || d <= 0 {
				return nil, fmt.Errorf("%q: the reset must be @reset=+DURATION, for example @reset=+20m", item)
			}
			resetIn = d
		}
		sp := fakeUtilSpec{name: strings.TrimSpace(name)}
		for _, w := range strings.Split(windows, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(w), "=")
			pct, err := strconv.ParseFloat(v, 64)
			if !ok || err != nil || pct < 0 || pct > 100 {
				return nil, fmt.Errorf("%q: %q is not 5h=PCT or 7d=PCT with PCT from 0 to 100", item, w)
			}
			switch k {
			case "5h":
				sp.has5, sp.pct5, sp.reset5 = true, pct, now.Add(fakeUtilOr(resetIn, fakeUtilReset5h))
				sp.name5 = fmt.Sprintf("5h=%.0f%%", pct)
			case "7d":
				sp.has7, sp.pct7, sp.reset7 = true, pct, now.Add(fakeUtilOr(resetIn, fakeUtilReset7d))
				sp.name7 = fmt.Sprintf("7d=%.0f%%", pct)
			default:
				return nil, fmt.Errorf("%q: unknown window %q (5h or 7d)", item, k)
			}
		}
		out = append(out, sp)
	}
	return out, nil
}

func fakeUtilOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// applyFakeUtil reads CHOTTAG_FAKE_UTIL once, at daemon start, and returns
// the function the auto-switcher calls before every decision: it writes
// each named account's simulated utilization into the status cache,
// freshly stamped, until that window's reset passes. From then on real
// traffic owns the row again. getenv is os.Getenv in production.
func applyFakeUtil(getenv func(string) string, state func() (store.State, error), sink *statusSink, log io.Writer, now time.Time) func(time.Time) {
	noop := func(time.Time) {}
	raw := getenv(fakeUtilEnv)
	if strings.TrimSpace(raw) == "" {
		return noop
	}
	specs, err := parseFakeUtil(raw, now.Truncate(time.Second))
	if err != nil {
		fmt.Fprintf(log, "chottag: fake util: %v; ignored\n", err)
		return noop
	}
	st, err := state()
	if err != nil {
		fmt.Fprintf(log, "chottag: fake util: %v\n", err)
		return noop
	}
	var kept []fakeUtilSpec
	for _, sp := range specs {
		a, ok := findExact(&st, sp.name) // exact, never Find's prefix match
		if !ok {
			fmt.Fprintf(log, "chottag: fake util: no account named %q; ignored\n", sp.name)
			continue
		}
		sp.name = a.Name
		kept = append(kept, sp)
		parts := strings.TrimSpace(sp.name5 + " " + sp.name7)
		until := sp.reset5
		if !sp.has5 {
			until = sp.reset7
		}
		fmt.Fprintf(log, "chottag: fake util (chottag_fakeusage build) %s %s until %s\n", sp.name, parts, until.Format(time.RFC3339))
	}
	if len(kept) == 0 {
		return noop
	}
	return func(at time.Time) { sink.fakeUtilOverride(kept, at) }
}

// fakeUtilOverride writes the simulated windows under the sink's mutex,
// for every window whose reset is still ahead of at.
func (c *statusSink) fakeUtilOverride(specs []fakeUtilSpec, at time.Time) {
	c.mu.Lock()
	for _, sp := range specs {
		live5 := sp.has5 && at.Before(sp.reset5)
		live7 := sp.has7 && at.Before(sp.reset7)
		if !live5 && !live7 {
			continue
		}
		i := -1
		for j := range c.file.Accounts {
			if strings.EqualFold(c.file.Accounts[j].Name, sp.name) {
				i = j
				break
			}
		}
		if i < 0 {
			c.file.Accounts = append(c.file.Accounts, status.Account{Name: sp.name})
			i = len(c.file.Accounts) - 1
		}
		a := &c.file.Accounts[i]
		u := status.Usage{}
		if a.Usage != nil {
			u = *a.Usage
		}
		u.UpdatedAt, u.Source = at.Round(0), "simulated"
		if live5 {
			p := sp.pct5
			u.FiveHourPct, u.FiveHourResetsAt = &p, sp.reset5
		}
		if live7 {
			p := sp.pct7
			u.SevenDayPct, u.SevenDayResetsAt = &p, sp.reset7
		}
		a.Usage = &u
	}
	c.mu.Unlock()
	c.maybeSave()
}
