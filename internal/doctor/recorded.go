package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/status"
)

// needsLogin is creds.StateNeedsLogin's value. Doctor never imports creds
// (spec §2.4); recorded_test.go pins the two together.
const needsLogin = "needs-login"

// ReportChecks are spec §2.2 rows 11-15: one token check per name, in the
// order given (registration order), then owners, route-drift, limits and
// version-drift. None has a Fix, and none reads a credential (D8).
func ReportChecks(names []string) []Check {
	checks := make([]Check, 0, len(names)+5)
	for _, n := range names {
		checks = append(checks, tokenCheck(n))
	}
	return append(checks, ownersCheck(), routeDriftCheck(), limitsCheck(), versionDriftCheck(), planUnknownCheck())
}

// loadStatus reads cache/status.json. A missing or unparseable cache is an
// empty one (status.Load never errors).
func loadStatus(e *Env) status.File {
	f, _ := status.Load(status.Path(e.Home))
	return f
}

// tokenCheck is row 11 for one account. It reports the daemon's recorded
// state only. That state carries no reason, so keychain-denied can't be
// told apart from a missing login.
func tokenCheck(name string) Check {
	return Check{ID: "token:" + name, Detect: func(e *Env) Finding {
		for _, a := range loadStatus(e).Accounts {
			if !strings.EqualFold(a.Name, name) {
				continue
			}
			switch {
			case string(a.Token) == needsLogin:
				return Finding{Status: StatusProblem, Detail: "the daemon recorded that this account needs a login", Hint: fmt.Sprintf("chottag login %s; allow Keychain access if macOS asks", name)}
			case a.Token != "":
				return Finding{Status: StatusOK, Detail: fmt.Sprintf("last recorded token state: %s (doctor never reads credentials)", a.Token)}
			}
		}
		return Finding{Status: StatusOK, Detail: "not checked (no traffic yet)"}
	}}
}

// ownersCheck is row 12. An entry naming no registered account is info,
// not a problem: logout leaves its account's entries behind by design
// (F170), and requests on those objects fall back to the remote account.
// An interrupted rename leaves the same trace, and the hint names the
// re-run. It counts per name and never prints an id (§2.4).
func ownersCheck() Check {
	return Check{ID: "owners", Detect: func(e *Env) Finding {
		counts, err := owners.CountByAccount(filepath.Join(e.Home, "owners.json"))
		switch {
		case errors.Is(err, owners.ErrCorrupt):
			return Finding{Status: StatusInfo, Detail: "owners.json is not valid JSON; the daemon moves it aside and starts empty at its next start"}
		case err != nil:
			return Finding{Status: StatusInfo, Detail: "cannot read owners.json: " + err.Error()}
		}
		st, err := e.State()
		if err != nil {
			return Internal(err)
		}
		var orphans []string
		total, orphaned := 0, 0
		for name, n := range counts {
			total += n
			if !registered(st, name) {
				orphans = append(orphans, name)
				orphaned += n
			}
		}
		if len(orphans) == 0 {
			return Finding{Status: StatusOK, Detail: fmt.Sprintf("%d entries, each naming a registered account", total)}
		}
		sort.Strings(orphans)
		parts := make([]string, len(orphans))
		for i, name := range orphans {
			parts[i] = fmt.Sprintf("%d name %q", counts[name], name)
		}
		return Finding{
			Status: StatusInfo,
			Detail: fmt.Sprintf("%d of %d entries name no registered account (%s); requests on those objects use the remote account", orphaned, total, strings.Join(parts, ", ")),
			Hint:   "chottag own <kind> <id> <account> re-attributes one; after an interrupted rename, run chottag rename <old> <new> again",
		}
	}}
}

// routeDriftCheck is row 13. The counter is per daemon generation and
// survives a stop, so only a running daemon's count is a problem.
func routeDriftCheck() Check {
	return Check{ID: "route-drift", Detect: func(e *Env) Finding {
		f := loadStatus(e)
		if f.Daemon == nil {
			return Finding{Status: StatusInfo, Detail: "no daemon has reported yet"}
		}
		f.DaemonRunningAt(e.Now())
		d := f.Daemon
		switch {
		case d.RouteDrift == 0:
			return Finding{Status: StatusOK, Detail: "no swapped request had to be resent unchanged"}
		case d.Running:
			return Finding{Status: StatusProblem, Detail: fmt.Sprintf("the running daemon resent %d swapped request(s) unchanged: the route table may not match this Claude Code version", d.RouteDrift), Hint: "chottag trace on"}
		}
		return Finding{Status: StatusInfo, Detail: fmt.Sprintf("the last daemon resent %d swapped request(s) unchanged; it is not running now", d.RouteDrift), Hint: "chottag trace on"}
	}}
}

// limitsCheck is row 14: detail only, never a fault.
func limitsCheck() Check {
	return Check{ID: "limits", Detect: func(e *Env) Finding {
		f := loadStatus(e)
		f.RollUpAt(e.Now())
		l := f.Limits
		switch {
		case len(f.Accounts) == 0:
			return Finding{Status: StatusOK, Detail: "no usage recorded yet"}
		case !l.AllLimited:
			return Finding{Status: StatusOK, Detail: "at least one account is not limited"}
		case l.NextReset.IsZero():
			return Finding{Status: StatusInfo, Detail: "every account is limited; no reset time is known"}
		}
		return Finding{Status: StatusInfo, Detail: fmt.Sprintf("every account is limited; the earliest reset is %s (%s)", l.NextReset.Local().Format("Jan 2 15:04"), l.NextResetAccount)}
	}}
}

// planUnknownCheck is M4's info row (spec §7, widened by review round 2
// item 5): an account whose plan tier chottag does not actually know —
// either a Max account whose size login/adopt could not tell (its plan is
// "max", F200), or an account with no plan set at all ("", tierOf's other
// default case). Auto-switch treats both the same way, as max5x, the
// safer mistake (S2): a max20x left unset switches a little early and is
// weighted a quarter of its capacity. Never a problem: nothing is broken,
// and only the user knows the real tier.
func planUnknownCheck() Check {
	return Check{ID: "plan-unknown", Detect: func(e *Env) Finding {
		st, err := e.State()
		if err != nil {
			return Internal(err)
		}
		var names []string
		for _, a := range st.Accounts {
			if a.Plan == "max" || a.Plan == "" {
				names = append(names, a.Name)
			}
		}
		if len(names) == 0 {
			return Finding{Status: StatusOK, Detail: "every account has a known plan"}
		}
		return Finding{
			Status: StatusInfo,
			Detail: fmt.Sprintf("account(s) with no known plan, treated as max5x by auto-switch: %s", strings.Join(names, ", ")),
			Hint:   fmt.Sprintf("chottag plan %s max5x (or max20x)", names[0]),
		}
	}}
}
