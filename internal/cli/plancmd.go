package cli

import (
	"flag"
	"fmt"
	"math"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagecost"
)

const planUsage = "usage: chottag plan NAME pro|max5x|team|ultra [--units N] [--price N]\n       chottag plan NAME --price N"

// Units bounds for `plan --units` (M4 spec §7).
const (
	minPlanUnits = 1
	maxPlanUnits = 1000
)

// maxPlanPrice bounds `plan --price` in USD per month; 0 resets to the list
// price (R203).
const maxPlanPrice = 100000

// planResult is `plan --json`'s fields: the account's registered name, the
// tier set, the capacity units auto-switch now uses for it, and the price
// the stats value comparison uses (effective: the --price override, else the
// tier's list price, else null; PriceSource is "set", "list" or "none").
type planResult struct {
	Account     string   `json:"account"`
	Plan        string   `json:"plan"`
	Units       float64  `json:"units"`
	Price       *float64 `json:"price"`
	PriceSource string   `json:"priceSource"`
}

// effectivePrice is the monthly USD price the stats comparison uses for a.
func effectivePrice(a store.Account) (*float64, string) {
	if a.Price > 0 {
		p := a.Price
		return &p, "set"
	}
	if p, ok := usagecost.PlanPrice(a.Plan); ok {
		return &p, "list"
	}
	return nil, "none"
}

// runPlan sets an account's plan tier (M4 spec §2, §7): the tier's switch
// points and capacity units apply to it from the daemon's next decision.
// --units N overrides the tier's units; without it the tier's own apply, so
// re-running `plan` without --units also clears an earlier override.
// --price N sets the monthly USD price the stats comparison uses (0 resets to
// the list price); `plan NAME --price N` changes only the price, and a later
// `plan NAME TIER` keeps it. NAME resolves like `tag` (name, email or unique
// prefix).
func runPlan(args []string, r *reporter) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	units := fs.Int("units", 0, "capacity units per 1% (default: the tier's)")
	price := fs.Float64("price", 0, "monthly price in USD for the stats comparison (0: the list price)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	unitsSet, priceSet := false, false
	fs.Visit(func(f *flag.Flag) {
		unitsSet = unitsSet || f.Name == "units"
		priceSet = priceSet || f.Name == "price"
	})
	priceOnly := len(positional) == 1 && priceSet && !unitsSet
	if len(positional) != 2 && !priceOnly {
		return r.Usage(planUsage)
	}
	var tier autoswitch.Tier
	if !priceOnly {
		var ok bool
		if tier, ok = autoswitch.ParseTier(positional[1]); !ok {
			return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("plan takes pro, max5x, team or ultra, not %q", positional[1]), nil)
		}
	}
	if unitsSet && (*units < minPlanUnits || *units > maxPlanUnits) {
		return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("--units must be a whole number from %d to %d, not %d", minPlanUnits, maxPlanUnits, *units), nil)
	}
	if !unitsSet {
		*units = 0
	}
	if priceSet && (math.IsNaN(*price) || *price < 0 || *price > maxPlanPrice) {
		return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("--price must be a number of USD per month from 0 to %d (0 resets to the list price), not %g", maxPlanPrice, *price), nil)
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	var got store.Account
	if _, err := (store.Store{Dir: h}).Update(func(st *store.State) error {
		a, err := st.Find(positional[0])
		if err != nil {
			return err
		}
		if !priceOnly {
			a.Plan, a.Units = string(tier), *units
		}
		if priceSet {
			a.Price = *price
		}
		got = *a
		return nil
	}); err != nil {
		return r.FailErr(err)
	}
	extra := ""
	if got.Price > 0 {
		extra = fmt.Sprintf(", $%g/month", got.Price)
	}
	r.Text("plan %s: %s (%g units%s)\n", got.Name, tierOf(got).DisplayName(), unitsOf(got), extra)
	p, src := effectivePrice(got)
	return r.OK(planResult{Account: got.Name, Plan: got.Plan, Units: unitsOf(got), Price: p, PriceSource: src})
}
