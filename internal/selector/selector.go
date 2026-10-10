// Package selector turns a routing decision into the account whose token a
// request should carry: the object's creator when the owner map knows it,
// otherwise the serving or remote account from state.json.
package selector

import (
	"context"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
)

type Tokens interface {
	Token(ctx context.Context, slotDir string) (string, creds.Status, bool)
}

// Awaiter is Tokens' blocking counterpart: an implementation that can wait
// out a refresh already in flight instead of reporting the stale
// passthrough the instant it starts. *tokens.Manager implements it (see
// tokens.Manager.Await). Choose type-asserts Config.Tokens for it through
// this separate, optional interface — rather than folding Await into
// Tokens itself — so a fake that only implements Token (as most selector
// tests' fakeTokens does) still satisfies Config.Tokens unchanged.
type Awaiter interface {
	Await(ctx context.Context, slotDir string) (string, creds.Status, bool)
}

// DefaultAwaitTimeout bounds how long Choose waits on an in-flight refresh
// for a remote- or owner-routed request when Config.AwaitTimeout is unset.
const DefaultAwaitTimeout = 15 * time.Second

type Owners interface {
	Lookup(kind router.Kind, id string) (string, bool)
}

// preferringOwners is an Owners that can prefer an account when several
// recorded it (R179: a connector id listed by more than one account).
type preferringOwners interface {
	LookupPreferring(kind router.Kind, id, prefer string) (string, bool)
}

// Event reports why a request was not sent as the account it should have
// been. The daemon logs these; nothing here contains a token.
type Event struct {
	Kind    string // "passthrough" | "owner-unregistered"
	Account string
	Detail  string
	Status  creds.Status
	// Role is the role the account was chosen for (RoleServing,
	// RoleRemote, RoleOwner) when its token turned out unusable; "" for
	// the other events. Refused reports that the request was refused (R147)
	// instead of sent on the client's own login.
	Role    string
	Refused bool
}

type Config struct {
	State  func() (store.State, error)
	Tokens Tokens
	Owners Owners
	// AwaitTimeout bounds a remote- or owner-routed request's wait on an
	// in-flight refresh (see Awaiter). 0 means DefaultAwaitTimeout.
	AwaitTimeout time.Duration
	OnEvent      func(Event) // nil = ignore
}

type Selector struct{ cfg Config }

func New(cfg Config) *Selector { return &Selector{cfg: cfg} }

// Choice is the account to send a request as. Its zero value means "send the
// request unchanged", on whatever login Claude Code already used.
type Choice struct {
	Account string
	Token   string
	Role    string // RoleServing | RoleRemote | RoleOwner
	// Refused, when not "", says why no account of the session's pool can
	// serve it, with more than one pool in state.json (M8): the request must
	// not go out on the client's own login, whose account may be in another
	// pool or none. Account is "" then. With a single pool a request that
	// cannot be served is the plain zero Choice, as before.
	Refused string
	// RefusedAccount and RefusedRole name the remote or owner account whose
	// unusable token made the refusal (R147): such a request is refused with
	// any number of pools, never sent on the client's own login (Home's).
	// Refused is set then, and Account is "".
	RefusedAccount, RefusedRole string
	// StateErr reports that state.json could not be read: the caller, which
	// may know that several pools exist, decides whether to refuse.
	StateErr bool
}

// Role values for Choice.Role. RoleOwner is exported so a caller outside
// this package (proxy.Chooser's adapter, F241/R96) can tell an owner-mapped
// choice apart from the remote-pin or serving fallback without duplicating
// the literal.
const (
	RoleServing = "serving"
	RoleRemote  = "remote"
	RoleOwner   = "owner"
)

// Choose picks the account for one request. bodyID is the object id found in
// the request body for decisions that name a RequestField, or "".
//
// A remote or owner role waits out an in-flight refresh (via Awaiter)
// instead of taking Token's usual stale passthrough: F166 found that a
// remote object (an artifact) requested as the Home account — because
// Home's own refresh was still running — got a 403 from upstream, and
// Claude Code dropped the artifact's watch for good. A serving (inference)
// request has no such cost, and waiting on it is exactly what F37 removed;
// only remote/owner requests wait here.
func (s *Selector) Choose(ctx context.Context, d router.Decision, bodyID string) Choice {
	return s.choose(ctx, d, bodyID, store.DefaultPool, nil)
}

// ChooseAs is Choose with serving standing in for state.json's serving
// account, for a serving-class request only: the owner and remote routing
// is unchanged and still comes first. The daemon's spread policy uses it to
// send a placed session as the account it was placed on.
func (s *Selector) ChooseAs(ctx context.Context, d router.Decision, bodyID, serving string) Choice {
	return s.choose(ctx, d, bodyID, store.DefaultPool, &serving)
}

// ChooseIn is Choose for a session of pool: the serving class uses the
// pool's serving account, or serving when it is not "" (the spread policy's
// placement), and the remote class, creates included, uses the pool's remote
// account. A pool that is not in state.json is default. Owner routing is
// unchanged and still comes first: an existing object goes out as its
// creator whatever the pool. Apart from that, an account outside the pool
// never serves: one that is not a member (a concurrent `pool leave`, a stale
// placement) sends the request unchanged.
func (s *Selector) ChooseIn(ctx context.Context, d router.Decision, bodyID, pool, serving string) Choice {
	if serving == "" {
		return s.choose(ctx, d, bodyID, pool, nil)
	}
	return s.choose(ctx, d, bodyID, pool, &serving)
}

// choose is Choose, ChooseAs and ChooseIn: serving, when non-nil, replaces
// the pool's serving account for the serving class.
func (s *Selector) choose(ctx context.Context, d router.Decision, bodyID, pool string, serving *string) Choice {
	if d.Class == router.Untouched || d.Class == "" {
		return Choice{}
	}
	st, err := s.cfg.State()
	if err != nil {
		s.emit(Event{Kind: "passthrough", Detail: "state: " + err.Error()})
		return Choice{StateErr: true}
	}
	guarded := len(st.PoolNames()) > 1

	if !st.HasPool(pool) {
		pool = store.DefaultPool
	}
	p := st.PoolOf(pool)
	st.Serving, st.Remote = p.Serving, p.Remote
	if serving != nil {
		st.Serving = *serving
	}
	name, role := s.byClass(&st, d)
	if owner, ok := s.owner(d, bodyID, st.Remote); ok {
		if _, ok := findExact(&st, owner); ok {
			name, role = owner, RoleOwner
		} else {
			// The creator was logged out: its objects fall back to the
			// current remote/serving account.
			s.emit(Event{Kind: "owner-unregistered", Account: owner, Detail: string(d.Object)})
		}
	}
	// deny is a request nothing can serve: unchanged, or, with several pools,
	// refused (see Choice.Refused), an owner lookup included: Home's login may
	// belong to another pool.
	deny := func(why string) Choice {
		if guarded {
			return Choice{Refused: why}
		}
		return Choice{}
	}
	if name == "" {
		s.emit(Event{Kind: "passthrough", Detail: "no " + role + " account set"})
		return deny("no " + role + " account set")
	}
	acct, ok := findExact(&st, name)
	if !ok {
		s.emit(Event{Kind: "passthrough", Account: name, Detail: "no such account"})
		return deny("no such account")
	}
	if role != RoleOwner && !acct.InPool(pool) {
		s.emit(Event{Kind: "passthrough", Account: name, Detail: "not in pool " + pool})
		return deny("not in pool")
	}
	if role == RoleServing && pool != store.DefaultPool && !acct.Rotates() {
		// R90 in a pool other than default: a rotation-off account never
		// serves a session (default keeps `tag`'s explicit override).
		s.emit(Event{Kind: "passthrough", Account: name, Detail: "rotation off"})
		return deny("rotation off")
	}
	tok, status, ok := s.token(ctx, acct.Dir, role)
	if !ok {
		never := role == RoleRemote || role == RoleOwner
		s.emit(Event{Kind: "passthrough", Account: acct.Name, Status: status, Role: role, Refused: never || guarded})
		if never {
			// R147: a remote or owner request never goes out on Home's
			// login, whatever the pool count.
			return Choice{Refused: "no usable token", RefusedAccount: acct.Name, RefusedRole: role}
		}
		return deny("no usable token")
	}
	return Choice{Account: acct.Name, Token: tok, Role: role}
}

// token is Choose's token lookup: Token for a serving-class request, or
// Await — bounded by AwaitTimeout — for a remote or owner role, when
// Config.Tokens implements Awaiter. See Choose's own doc comment for why
// (F166).
func (s *Selector) token(ctx context.Context, slotDir, role string) (string, creds.Status, bool) {
	if role != RoleRemote && role != RoleOwner {
		return s.cfg.Tokens.Token(ctx, slotDir)
	}
	return s.awaitToken(ctx, slotDir)
}

// awaitToken is the bounded wait on an in-flight (or forced) refresh.
func (s *Selector) awaitToken(ctx context.Context, slotDir string) (string, creds.Status, bool) {
	aw, ok := s.cfg.Tokens.(Awaiter)
	if !ok {
		return s.cfg.Tokens.Token(ctx, slotDir)
	}
	timeout := s.cfg.AwaitTimeout
	if timeout == 0 {
		timeout = DefaultAwaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return aw.Await(ctx, slotDir)
}

// Why is the typed reason a named account could not answer a request, for the
// daemon's log. Its String is the clause that follows the account's name.
type Why int

const (
	WhyNone Why = iota
	WhyNeedsLogin
	WhyStaleRefreshFailed
	WhyNotRegistered
	WhyNotInPool
	WhyRotationOff
	WhyStateError
	WhyIdentityChanged
)

func (w Why) String() string {
	switch w {
	case WhyNeedsLogin:
		return "needs login"
	case WhyStaleRefreshFailed:
		return "could not refresh its login"
	case WhyNotRegistered:
		return "is no longer registered"
	case WhyNotInPool:
		return "is not in the session's pool"
	case WhyRotationOff:
		return "has rotation off"
	case WhyStateError:
		return "could not be checked: state.json is unreadable"
	case WhyIdentityChanged:
		return "was logged in again as another account"
	}
	return ""
}

// ChooseNamed sends a serving-class request of a session of pool as the named
// account, when that account can answer it (R160): it is registered, in the
// pool, not rotation-off outside default, and has a login, the bounded refresh
// wait included, so a stale but refreshable token is renewed, not given up on.
// Otherwise the zero Choice and the reason. It never falls back to another
// account and emits no passthrough event: the caller decides what happens next.
func (s *Selector) ChooseNamed(ctx context.Context, pool, name string) (Choice, Why) {
	st, err := s.cfg.State()
	if err != nil {
		return Choice{}, WhyStateError
	}
	if !st.HasPool(pool) {
		pool = store.DefaultPool
	}
	acct, ok := findExact(&st, name)
	if !ok {
		return Choice{}, WhyNotRegistered
	}
	if !acct.InPool(pool) {
		return Choice{}, WhyNotInPool
	}
	// R90: outside default a rotation-off account never serves a session. In
	// default `tag`'s explicit override lets it, as for any serving request.
	if pool != store.DefaultPool && !acct.Rotates() {
		return Choice{}, WhyRotationOff
	}
	tok, status, ok := s.awaitToken(ctx, acct.Dir)
	if !ok {
		if status.State == creds.StateNeedsLogin {
			return Choice{}, WhyNeedsLogin
		}
		return Choice{}, WhyStaleRefreshFailed
	}
	return Choice{Account: acct.Name, Token: tok, Role: RoleServing}, WhyNone
}

func (s *Selector) byClass(st *store.State, d router.Decision) (name, role string) {
	if d.Class == router.Remote {
		return st.Remote, RoleRemote
	}
	return st.Serving, RoleServing
}

// owner returns the account that created the object this request names.
//
// remote is the session pool's remote account: a connector id that account
// listed goes to it, however many accounts listed the id (R179).
func (s *Selector) owner(d router.Decision, bodyID, remote string) (string, bool) {
	if d.Object == "" {
		return "", false
	}
	id := d.ObjectID
	if id == "" {
		id = bodyID
	}
	if id == "" {
		return "", false
	}
	if p, ok := s.cfg.Owners.(preferringOwners); ok {
		return p.LookupPreferring(d.Object, id, remote)
	}
	return s.cfg.Owners.Lookup(d.Object, id)
}

func (s *Selector) emit(e Event) {
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(e)
	}
}

// findExact resolves name to a registered account by exact name, matching
// case-insensitively — the same scan store.State.index and Add's duplicate
// check already use, so this lookup can never disagree with them. Unlike
// st.Find, it never falls back to a unique name prefix: a caller here is
// checking identity ("is this account still registered"), not resolving
// what a human typed at a prompt (F15 — Find is prefix-matching, not an
// identity check).
func findExact(st *store.State, name string) (*store.Account, bool) {
	for i, a := range st.Accounts {
		if strings.EqualFold(a.Name, name) {
			return &st.Accounts[i], true
		}
	}
	return nil, false
}
