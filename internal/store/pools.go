package store

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DefaultPool is the pool today's top-level serving, remote, policy and pin
// fields describe. It always exists and can't be removed (M8 spec §2).
const DefaultPool = "default"

var (
	ErrNoPool       = errors.New("no such pool")
	ErrBadPool      = errors.New("invalid pool name: use 1-16 lowercase letters, digits or '-'")
	ErrPoolExists   = errors.New("pool already exists")
	ErrPoolNotEmpty = errors.New("pool still has accounts")
	ErrPoolDefault  = errors.New("the default pool cannot be removed")
	ErrLastPool     = errors.New("an account must stay in at least one pool")
	ErrNotInPool    = errors.New("account is not in that pool")
)

var poolRe = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// ValidPoolName reports whether name can name a pool.
func ValidPoolName(name string) bool { return poolRe.MatchString(name) }

// Pool is an extra pool's roles. The default pool's live in State's
// top-level Serving, Remote, Policy and Pin.
type Pool struct {
	Serving string `json:"serving,omitempty"`
	Remote  string `json:"remote,omitempty"`
	Policy  string `json:"policy,omitempty"`
	Pin     string `json:"pin,omitempty"`
}

// InPools returns the pools the account is in: ["default"] when none is
// listed. The result is a copy.
func (a Account) InPools() []string {
	if len(a.PoolList) == 0 {
		return []string{DefaultPool}
	}
	return append([]string(nil), a.PoolList...)
}

// InPool reports whether the account is in pool.
func (a Account) InPool(pool string) bool {
	if len(a.PoolList) == 0 {
		return pool == DefaultPool
	}
	for _, p := range a.PoolList {
		if p == pool {
			return true
		}
	}
	return false
}

// normalizePools stores exactly ["default"] as absent, so a state that never
// used pools writes no key.
func normalizePools(list []string) []string {
	if len(list) == 0 || (len(list) == 1 && list[0] == DefaultPool) {
		return nil
	}
	return list
}

// PoolNames lists "default" first, then the other pools sorted.
func (s State) PoolNames() []string {
	out := []string{DefaultPool}
	rest := make([]string, 0, len(s.Pools))
	for n := range s.Pools {
		rest = append(rest, n)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// HasPool reports whether pool exists; default always does.
func (s State) HasPool(name string) bool {
	if name == DefaultPool {
		return true
	}
	_, ok := s.Pools[name]
	return ok
}

// PoolOf returns pool's roles; the zero Pool for an unknown pool.
func (s State) PoolOf(name string) Pool {
	if name == DefaultPool {
		return Pool{Serving: s.Serving, Remote: s.Remote, Policy: s.Policy, Pin: s.Pin}
	}
	return s.Pools[name]
}

func (st *State) putPool(name string, p Pool) {
	if name == DefaultPool {
		st.Serving, st.Remote, st.Policy, st.Pin = p.Serving, p.Remote, p.Policy, p.Pin
		return
	}
	st.Pools[name] = p
}

func (st *State) editPool(name string, fn func(*Pool)) {
	p := st.PoolOf(name)
	fn(&p)
	st.putPool(name, p)
}

// Members lists the accounts in pool, in registration order.
func (s State) Members(pool string) []Account {
	var out []Account
	for _, a := range s.Accounts {
		if a.InPool(pool) {
			out = append(out, a)
		}
	}
	return out
}

func (st *State) needPool(pool string) error {
	if !st.HasPool(pool) {
		return fmt.Errorf("%w: %q", ErrNoPool, pool)
	}
	return nil
}

// member resolves account (exact, case-insensitive name) as a member of pool
// and returns its registered spelling; "" resolves to "".
func (st *State) member(pool, account string) (string, error) {
	if err := st.needPool(pool); err != nil {
		return "", err
	}
	if account == "" {
		return "", nil
	}
	i := st.index(account)
	if i < 0 {
		return "", fmt.Errorf("%w: %q", ErrNotFound, account)
	}
	a := st.Accounts[i]
	if !a.InPool(pool) {
		return "", fmt.Errorf("%w: %s is not in %s", ErrNotInPool, a.Name, pool)
	}
	return a.Name, nil
}

// SetPoolServing makes account (a member of pool; "" clears) the pool's
// serving account.
func (st *State) SetPoolServing(pool, account string) error {
	n, err := st.member(pool, account)
	if err != nil {
		return err
	}
	st.editPool(pool, func(p *Pool) { p.Serving = n })
	return nil
}

// SetPoolRemote makes account the pool's remote account.
func (st *State) SetPoolRemote(pool, account string) error {
	n, err := st.member(pool, account)
	if err != nil {
		return err
	}
	st.editPool(pool, func(p *Pool) { p.Remote = n })
	return nil
}

// SetPoolPin pins new sessions of the pool to account; "" clears.
func (st *State) SetPoolPin(pool, account string) error {
	n, err := st.member(pool, account)
	if err != nil {
		return err
	}
	st.editPool(pool, func(p *Pool) { p.Pin = n })
	return nil
}

// SetPoolPolicy records the pool's policy; serial is stored as absent.
func (st *State) SetPoolPolicy(pool, policy string) error {
	if err := st.needPool(pool); err != nil {
		return err
	}
	if policy != PolicySpread {
		policy = ""
	}
	st.editPool(pool, func(p *Pool) { p.Policy = policy })
	return nil
}

// AddPool creates an empty pool.
func (st *State) AddPool(name string) error {
	if !ValidPoolName(name) {
		return fmt.Errorf("%w: %q", ErrBadPool, name)
	}
	if st.HasPool(name) {
		return fmt.Errorf("%w: %s", ErrPoolExists, name)
	}
	if st.Pools == nil {
		st.Pools = map[string]Pool{}
	}
	st.Pools[name] = Pool{}
	return nil
}

// RemovePool removes an empty pool other than default. Removing the last
// extra pool leaves no pools key, so the file goes back to version 1.
func (st *State) RemovePool(name string) error {
	if name == DefaultPool {
		return ErrPoolDefault
	}
	if err := st.needPool(name); err != nil {
		return err
	}
	if len(st.Members(name)) > 0 {
		return fmt.Errorf("%w: %s", ErrPoolNotEmpty, name)
	}
	delete(st.Pools, name)
	if len(st.Pools) == 0 {
		st.Pools = nil
	}
	return nil
}

// after returns the member of pool that follows account in registration
// order, wrapping, other than account itself; rotating restricts it to
// accounts that rotate. ok is false when there is none.
func (st *State) after(pool, account string, rotating bool) (Account, bool) {
	ms := st.Members(pool)
	start := -1
	for i, m := range ms {
		if strings.EqualFold(m.Name, account) {
			start = i
		}
	}
	for k := 1; k <= len(ms); k++ {
		m := ms[(start+k)%len(ms)]
		if strings.EqualFold(m.Name, account) {
			continue
		}
		if rotating && !m.Rotates() {
			continue
		}
		return m, true
	}
	return Account{}, false
}

// NextInPool returns the rotating member of pool after cur, wrapping; an
// empty or unknown cur yields the first. With a single rotating member that
// member is returned.
func (st *State) NextInPool(pool, cur string) (Account, error) {
	if err := st.needPool(pool); err != nil {
		return Account{}, err
	}
	ms := st.Members(pool)
	if len(ms) == 0 {
		return Account{}, ErrNoAccounts
	}
	start := -1
	for i, m := range ms {
		if strings.EqualFold(m.Name, cur) {
			start = i
		}
	}
	for k := 1; k <= len(ms); k++ {
		if m := ms[(start+k)%len(ms)]; m.Rotates() {
			return m, nil
		}
	}
	return Account{}, ErrNoAccounts
}

// JoinPool adds account to pool (it stays in its others). In pool it becomes
// serving if pool has none and it rotates (R90), and remote if pool has none.
// Already a member: nothing changes.
func (st *State) JoinPool(account, pool string) error {
	if err := st.needPool(pool); err != nil {
		return err
	}
	i := st.index(account)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, account)
	}
	a := &st.Accounts[i]
	if a.InPool(pool) {
		return nil
	}
	a.PoolList = append(a.InPools(), pool)
	st.fillRoles(pool, *a)
	return nil
}

// fillRoles gives a the roles pool has empty: serving only if a rotates.
func (st *State) fillRoles(pool string, a Account) {
	st.editPool(pool, func(p *Pool) {
		if p.Serving == "" && a.Rotates() {
			p.Serving = a.Name
		}
		if p.Remote == "" {
			p.Remote = a.Name
		}
	})
}

// LeavePool removes account from pool. It must stay in at least one pool.
// A serving role goes to the next rotating member and a remote role to the
// next member, or to none; a pin naming it is cleared.
func (st *State) LeavePool(account, pool string) error {
	if err := st.needPool(pool); err != nil {
		return err
	}
	i := st.index(account)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, account)
	}
	a := st.Accounts[i]
	if !a.InPool(pool) {
		return fmt.Errorf("%w: %s is not in %s", ErrNotInPool, a.Name, pool)
	}
	if len(a.InPools()) == 1 {
		return fmt.Errorf("%w: %s", ErrLastPool, a.Name)
	}
	srv, hasSrv := st.after(pool, a.Name, true)
	rem, hasRem := st.after(pool, a.Name, false)
	st.editPool(pool, func(p *Pool) {
		if strings.EqualFold(p.Serving, a.Name) {
			p.Serving = ""
			if hasSrv {
				p.Serving = srv.Name
			}
		}
		if strings.EqualFold(p.Remote, a.Name) {
			p.Remote = ""
			if hasRem {
				p.Remote = rem.Name
			}
		}
		if strings.EqualFold(p.Pin, a.Name) {
			p.Pin = ""
		}
	})
	var rest []string
	for _, p := range a.InPools() {
		if p != pool {
			rest = append(rest, p)
		}
	}
	st.Accounts[i].PoolList = normalizePools(rest)
	return nil
}

// SwapPoolServing is SwapServing for one pool: serving moves from `from` to
// `to` only if it is still `from`. to must be a member of pool.
func (s Store) SwapPoolServing(pool, from, to string) (State, error) {
	return s.Update(func(st *State) error {
		if err := st.needPool(pool); err != nil {
			return err
		}
		if cur := st.PoolOf(pool).Serving; !strings.EqualFold(cur, from) {
			return fmt.Errorf("%w: serving is %q, not %q", ErrServingChanged, cur, from)
		}
		return st.SetPoolServing(pool, to)
	})
}

// normalizeAccountPools repairs a hand-edited file at Load: each account's
// pools lose duplicates and names of pools that don't exist, and an empty
// result means default; a "default" entry in Pools is dropped (default's
// roles are the top-level fields).
func (st *State) normalizeAccountPools() {
	delete(st.Pools, DefaultPool)
	if len(st.Pools) == 0 {
		st.Pools = nil
	}
	for i := range st.Accounts {
		var out []string
		seen := map[string]bool{}
		for _, p := range st.Accounts[i].PoolList {
			if seen[p] || !st.HasPool(p) {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
		st.Accounts[i].PoolList = normalizePools(out)
	}
}
