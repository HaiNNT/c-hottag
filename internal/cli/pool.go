package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

const poolUsage = "usage: chottag pool [--json] | pool add NAME | pool join ACCOUNT POOL | pool leave ACCOUNT POOL | pool rm NAME"

// poolsSince is the first release that reads state.json's pools (M8).
const poolsSince = "0.8.0"

// errPoolAmbiguous is the command-level refusal of a name-taking command
// whose account is in several pools and was given no --pool.
var errPoolAmbiguous = errors.New("account is in several pools")

// failPool reports err with its pool code and exit 2 when it is one of the
// pool errors, and as FailErr otherwise. Each case spells its own literal
// exit and code, so the docs tests can read the pairing.
func failPool(r *reporter, err error) int {
	msg := err.Error()
	switch {
	case errors.Is(err, store.ErrNoPool):
		return r.Fail(exit.Usage, codeNoPool, msg, nil)
	case errors.Is(err, store.ErrBadPool):
		return r.Fail(exit.Usage, codeBadPool, msg, nil)
	case errors.Is(err, store.ErrPoolExists):
		return r.Fail(exit.Usage, codePoolExists, msg, nil)
	case errors.Is(err, store.ErrPoolNotEmpty):
		return r.Fail(exit.Usage, codePoolNotEmpty, msg, nil)
	case errors.Is(err, store.ErrPoolDefault):
		return r.Fail(exit.Usage, codePoolDefault, msg, nil)
	case errors.Is(err, store.ErrLastPool):
		return r.Fail(exit.Usage, codeLastPool, msg, nil)
	case errors.Is(err, store.ErrNotInPool):
		return r.Fail(exit.Usage, codeNotInPool, msg, nil)
	case errors.Is(err, errPoolAmbiguous):
		return r.Fail(exit.Usage, codePoolAmbiguous, msg, nil)
	}
	return r.FailErr(err)
}

// poolOrDefault is "default" when name is empty.
func poolOrDefault(name string) string {
	if name == "" {
		return store.DefaultPool
	}
	return name
}

// checkPool is the no_pool test for a command that names a pool.
func checkPool(st store.State, name string) error {
	if !st.HasPool(name) {
		return fmt.Errorf("%w: %q (`chottag pool` lists them)", store.ErrNoPool, name)
	}
	return nil
}

// accountPool picks the pool a command naming account a acts in: --pool when
// given (it must exist and hold a), else a's one pool; several need --pool.
func accountPool(st store.State, a store.Account, flagPool string) (string, error) {
	if flagPool != "" {
		if err := checkPool(st, flagPool); err != nil {
			return "", err
		}
		if !a.InPool(flagPool) {
			return "", fmt.Errorf("%w: %s is not in %s", store.ErrNotInPool, a.Name, flagPool)
		}
		return flagPool, nil
	}
	ps := a.InPools()
	if len(ps) == 1 {
		return ps[0], nil
	}
	return "", fmt.Errorf("%w: %s is in %s; say which with --pool", errPoolAmbiguous, a.Name, joinNames(ps))
}

// joinNames is "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// poolView is st seen as one pool: its members as the accounts and its roles
// as the top-level fields, so the single-pool code (nextCandidate, policyName)
// serves any pool. For the default pool of an install with no other pool it is
// st itself.
func poolView(st store.State, pool string) store.State {
	p := st.PoolOf(pool)
	v := st
	v.Accounts = st.Members(pool)
	v.Serving, v.Remote, v.Policy, v.Pin = p.Serving, p.Remote, p.Policy, p.Pin
	return v
}

// poolSpread reports whether pool's policy is spread.
func poolSpread(st store.State, pool string) bool {
	return st.PoolOf(pool).Policy == store.PolicySpread
}

// poolLabel is the " (pool X)" suffix a result line carries outside default.
func poolLabel(pool string) string {
	if pool == store.DefaultPool {
		return ""
	}
	return " (pool " + pool + ")"
}

// poolField is a result's pool member: absent for default, so a default-only
// install's documents are unchanged.
func poolField(pool string) string {
	if pool == store.DefaultPool {
		return ""
	}
	return pool
}

// accountPools lists a's pools in pool-list order (default first, then sorted).
func accountPools(st store.State, a store.Account) []string {
	var out []string
	for _, p := range st.PoolNames() {
		if a.InPool(p) {
			out = append(out, p)
		}
	}
	return out
}

// warnShared prints R133's shared_account warning when a is in more
// than one pool: the spread wording if any of its pools spreads, else the
// serial one.
func warnShared(st store.State, a store.Account, r *reporter) {
	pools := accountPools(st, a)
	if len(pools) < 2 {
		return
	}
	spread := false
	for _, p := range pools {
		spread = spread || poolSpread(st, p)
	}
	head := fmt.Sprintf("chottag: warning: %s is now in %s and shares one usage limit between them: ", a.Name, joinNames(pools))
	if spread {
		r.Warn(warnSharedAccount, head+fmt.Sprintf("under spread, their sessions compete for it, and heavy use in one pool moves the other's sessions off %s (their prompt caches go cold).", a.Name))
		return
	}
	r.Warn(warnSharedAccount, head+fmt.Sprintf("if %s serves both, they use up its 5-hour window together and switch away from it at the same time.", a.Name))
}

// daemonPredatesPools reports the running daemon's version when it cannot read
// pools: older than 0.8.0, or reporting none (empty) or one this build cannot
// order. No daemon, the same version, or a newer one: false. A development
// build, which cannot order versions itself, never says so.
func daemonPredatesPools(st store.State) (string, bool) {
	up, ver := statusProbe(st.ResolvedPort())
	if !up || ver == Version || !updatecheck.Parses(Version) {
		return "", false
	}
	if ver != "" && updatecheck.Parses(ver) && !updatecheck.Newer(poolsSince, ver) {
		return "", false // the daemon already reads pools (0.8.0 or later)
	}
	if ver == "" {
		ver = "unknown"
	}
	return ver, true
}

// warnIfDaemonPredatesPools warns, without refusing, after `pool join`: the
// pool already exists, but such a daemon cannot read a version-2 state.json,
// so its requests go out on Home's own login until it is restarted.
func warnIfDaemonPredatesPools(st store.State, r *reporter) {
	if ver, old := daemonPredatesPools(st); old {
		r.Warn(warnDaemonPredatesPools, fmt.Sprintf(
			"chottag: warning: the running daemon (%s) predates pools: it cannot read the pools in state.json, so its requests go out on Home's own login. Run `chottag daemon restart` first.", ver))
	}
}

// rollbackLosesPools reports whether installing release ver (no leading v)
// would be a version that predates pools (M8 spec §2). A version that does not
// parse is not refused: --version's own check has already vetted it.
func rollbackLosesPools(ver string) bool {
	return updatecheck.Parses(ver) && updatecheck.Newer(poolsSince, ver)
}

// poolRow is one pool in `pool --json`.
type poolRow struct {
	Name     string   `json:"name"`
	Serving  string   `json:"serving"`
	Remote   string   `json:"remote"`
	Policy   string   `json:"policy"`
	Pin      string   `json:"pin"`
	Accounts []string `json:"accounts"`
}

type poolListResult struct {
	Pools []poolRow `json:"pools"`
}

type poolNameResult struct {
	Pool string `json:"pool"`
}

// poolMemberResult is `pool join` / `pool leave --json`'s fields: Pools is
// every pool the account is in afterwards.
type poolMemberResult struct {
	Account string   `json:"account"`
	Pool    string   `json:"pool"`
	Pools   []string `json:"pools"`
	Changed bool     `json:"changed"`
}

// runPool is `chottag pool [add|join|leave|rm]` (M8 spec §3).
func runPool(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, poolUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) == 0 {
		return runPoolList(r)
	}
	verbArgs := args[1:]
	switch args[0] {
	case "add":
		if len(verbArgs) != 1 {
			return r.Usage("usage: chottag pool add NAME")
		}
		return runPoolAdd(verbArgs[0], r)
	case "join":
		if len(verbArgs) != 2 {
			return r.Usage("usage: chottag pool join ACCOUNT POOL")
		}
		return runPoolMember(verbArgs[0], verbArgs[1], true, r)
	case "leave":
		if len(verbArgs) != 2 {
			return r.Usage("usage: chottag pool leave ACCOUNT POOL")
		}
		return runPoolMember(verbArgs[0], verbArgs[1], false, r)
	case "rm":
		if len(verbArgs) != 1 {
			return r.Usage("usage: chottag pool rm NAME")
		}
		return runPoolRm(verbArgs[0], r)
	}
	return r.Usage(poolUsage)
}

func poolStore(r *reporter) (store.Store, bool) {
	h, err := home()
	if err != nil {
		r.FailErr(err)
		return store.Store{}, false
	}
	return store.Store{Dir: h}, true
}

func runPoolList(r *reporter) int {
	s, ok := poolStore(r)
	if !ok {
		return exit.Error
	}
	st, err := s.Load()
	if err != nil {
		return r.FailErr(err)
	}
	rows := []poolRow{}
	for _, name := range st.PoolNames() {
		v := poolView(st, name)
		row := poolRow{Name: name, Serving: v.Serving, Remote: v.Remote, Policy: policyName(v), Accounts: []string{}}
		if v.PolicySpread() {
			row.Pin = v.Pin
		}
		for _, a := range v.Accounts {
			row.Accounts = append(row.Accounts, a.Name)
		}
		rows = append(rows, row)
		line := fmt.Sprintf("%s: serving %s, remote %s, %s", name, orNone(v.Serving), orNone(v.Remote), row.Policy)
		if row.Pin != "" {
			line += ", pin " + row.Pin
		}
		r.Text("%s; accounts: %s\n", line, orNone(strings.Join(row.Accounts, ", ")))
	}
	return r.OK(poolListResult{Pools: rows})
}

func runPoolAdd(name string, r *reporter) int {
	s, ok := poolStore(r)
	if !ok {
		return exit.Error
	}
	// A daemon older than 0.8.0 would meet the version-2 file this writes and
	// send every request on Home's login, and it cannot restart itself onto
	// 0.8.0 once it cannot read state.json: refuse before writing.
	if cur, err := s.Load(); err == nil {
		if ver, old := daemonPredatesPools(cur); old {
			return r.Fail(exit.Usage, codeDaemonPredatesPools, fmt.Sprintf(
				"the running daemon (%s) predates pools and cannot read the file pools write; run: chottag daemon restart, then chottag pool add %s", ver, name), nil)
		}
	}
	if _, err := s.Update(func(st *store.State) error { return st.AddPool(name) }); err != nil {
		return failPool(r, err)
	}
	r.Text("added pool %s\n", name)
	return r.OK(poolNameResult{Pool: name})
}

func runPoolRm(name string, r *reporter) int {
	s, ok := poolStore(r)
	if !ok {
		return exit.Error
	}
	if _, err := s.Update(func(st *store.State) error { return st.RemovePool(name) }); err != nil {
		return failPool(r, err)
	}
	r.Text("removed pool %s\n", name)
	return r.OK(poolNameResult{Pool: name})
}

// runPoolMember is `pool join` (join true) and `pool leave`.
func runPoolMember(account, pool string, join bool, r *reporter) int {
	s, ok := poolStore(r)
	if !ok {
		return exit.Error
	}
	var name string
	var changed bool
	st, err := s.Update(func(st *store.State) error {
		a, err := st.Find(account)
		if err != nil {
			return err
		}
		name = a.Name
		if err := checkPool(*st, pool); err != nil {
			return err
		}
		changed = a.InPool(pool) != join
		if join {
			return st.JoinPool(name, pool)
		}
		return st.LeavePool(name, pool)
	})
	if err != nil {
		return failPool(r, err)
	}
	a, err := st.Find(name)
	if err != nil {
		return r.FailErr(err)
	}
	switch {
	case join && changed:
		r.Text("%s joined %s\n", name, pool)
	case join:
		r.Text("%s is already in %s\n", name, pool)
	default:
		r.Text("%s left %s\n", name, pool)
	}
	if join && changed {
		warnShared(st, *a, r)
		warnIfDaemonPredatesPools(st, r)
	}
	return r.OK(poolMemberResult{Account: name, Pool: pool, Pools: accountPools(st, *a), Changed: changed})
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
