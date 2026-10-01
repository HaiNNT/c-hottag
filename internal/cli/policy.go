package cli

import (
	"flag"
	"fmt"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

const policyUsage = "usage: chottag policy [serial|spread] [--pool POOL]"

// spreadNextMessage is `next`'s refusal under spread (M7 spec §5): a
// machine-wide "next" has no meaning when sessions sit on different accounts.
const spreadNextMessage = "under spread, chottag places sessions itself: `chottag tag NAME` pins new sessions; `chottag policy serial` returns to one serving account."

// policyResult is `policy --json`'s fields. Pin is always present: "" when
// unset.
type policyResult struct {
	Policy string `json:"policy"`
	Pin    string `json:"pin"`
	// Pool is the pool acted in; absent for default (M8).
	Pool string `json:"pool,omitempty"`
}

func policyName(st store.State) string {
	if st.PolicySpread() {
		return store.PolicySpread
	}
	return store.PolicySerial
}

// runPolicy shows or sets how new sessions are placed, in one pool (--pool,
// default without it). Its only flag is --pool. Switching policy changes
// nothing about rotation or the stored pin.
func runPolicy(args []string, r *reporter) int {
	fs := flag.NewFlagSet("policy", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	poolArg := fs.String("pool", "", "the pool whose policy to show or set (default: default)")
	args, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	poolFlag := *poolArg
	if len(args) > 1 {
		return r.Usage(policyUsage)
	}
	if len(args) == 1 && args[0] != store.PolicySerial && args[0] != store.PolicySpread {
		return r.Fail(exit.Usage, codeBadPolicy,
			fmt.Sprintf("policy takes serial or spread, not %q", args[0]), nil)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}
	pool := poolOrDefault(poolFlag)
	var all store.State
	if len(args) == 0 {
		all, err = s.Load()
		if err == nil {
			err = checkPool(all, pool)
		}
	} else {
		all, err = s.Update(func(st *store.State) error {
			return st.SetPoolPolicy(pool, args[0])
		})
	}
	if err != nil {
		return failPool(r, err)
	}
	st := poolView(all, pool)
	if len(args) == 1 && st.PolicySpread() {
		warnIfDaemonPredatesSpread(all, r)
		for _, a := range st.Accounts {
			warnShared(all, a, r)
		}
	}
	r.Text("policy: %s%s\n", policyName(st), poolLabel(pool))
	// The pin has an effect only under spread, so a stored pin is shown
	// only then.
	pin := ""
	if st.PolicySpread() {
		pin = st.Pin
		if pin != "" {
			r.Text("pin: %s\n", pin)
		}
	}
	return r.OK(policyResult{Policy: policyName(st), Pin: pin, Pool: poolField(pool)})
}

// spreadSince is the first release whose daemon places sessions under spread.
const spreadSince = "0.7.0"

// warnIfDaemonPredatesSpread warns, without refusing, when the running
// daemon is older than this binary (or its version is not one this binary can
// order): a daemon from before 0.7.0 ignores the policy and, the next time it
// writes state.json, drops it. Restarting the daemon first makes the policy
// take effect. No daemon, the same version, or a newer one: nothing to say.
func warnIfDaemonPredatesSpread(st store.State, r *reporter) {
	up, ver := statusProbe(st.ResolvedPort())
	if !up || ver == "" || ver == Version || !updatecheck.Parses(Version) {
		return
	}
	if updatecheck.Parses(ver) && !updatecheck.Newer(spreadSince, ver) {
		return // the daemon already has spread (0.7.0 or later)
	}
	r.Warn(warnDaemonPredatesSpread, fmt.Sprintf(
		"chottag: warning: the running daemon (%s) predates spread: it ignores the policy and may reset it to serial. Run `chottag daemon restart` first.", ver))
}
