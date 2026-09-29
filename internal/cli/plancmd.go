package cli

import (
	"flag"
	"fmt"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const planUsage = "usage: chottag plan NAME pro|max5x|max20x|team [--units N]"

// Units bounds for `plan --units` (M4 spec §7).
const (
	minPlanUnits = 1
	maxPlanUnits = 1000
)

// planResult is `plan --json`'s fields: the account's registered name, the
// tier set, and the capacity units auto-switch now uses for it.
type planResult struct {
	Account string  `json:"account"`
	Plan    string  `json:"plan"`
	Units   float64 `json:"units"`
}

// runPlan sets an account's plan tier (M4 spec §2, §7): the tier's switch
// points and capacity units apply to it from the daemon's next decision.
// --units N overrides the tier's units; without it the tier's own apply, so
// re-running `plan` without --units also clears an earlier override. NAME
// resolves like `tag` (name, email or unique prefix).
func runPlan(args []string, r *reporter) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	units := fs.Int("units", 0, "capacity units per 1% (default: the tier's)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 2 {
		return r.Usage(planUsage)
	}
	tier, ok := autoswitch.ParseTier(positional[1])
	if !ok {
		return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("plan takes pro, max5x, max20x or team, not %q", positional[1]), nil)
	}
	unitsSet := false
	fs.Visit(func(f *flag.Flag) { unitsSet = unitsSet || f.Name == "units" })
	if unitsSet && (*units < minPlanUnits || *units > maxPlanUnits) {
		return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("--units must be a whole number from %d to %d, not %d", minPlanUnits, maxPlanUnits, *units), nil)
	}
	if !unitsSet {
		*units = 0
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
		a.Plan, a.Units = string(tier), *units
		got = *a
		return nil
	}); err != nil {
		return r.FailErr(err)
	}
	r.Text("plan %s: %s (%g units)\n", got.Name, got.Plan, unitsOf(got))
	return r.OK(planResult{Account: got.Name, Plan: got.Plan, Units: unitsOf(got)})
}
