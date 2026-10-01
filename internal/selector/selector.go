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
const DefaultAwaitTimeout = 20 * time.Second

type Owners interface {
	Lookup(kind router.Kind, id string) (string, bool)
}

// Event reports why a request was not sent as the account it should have
// been. The daemon logs these; nothing here contains a token.
type Event struct {
	Kind    string // "passthrough" | "owner-unregistered"
	Account string
	Detail  string
	Status  creds.Status
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
	return s.choose(ctx, d, bodyID, nil)
}

// ChooseAs is Choose with serving standing in for state.json's serving
// account, for a serving-class request only: the owner and remote routing
// is unchanged and still comes first. The daemon's spread policy uses it to
// send a placed session as the account it was placed on.
func (s *Selector) ChooseAs(ctx context.Context, d router.Decision, bodyID, serving string) Choice {
	return s.choose(ctx, d, bodyID, &serving)
}

// choose is Choose and ChooseAs: serving, when non-nil, replaces
// st.Serving for the serving class.
func (s *Selector) choose(ctx context.Context, d router.Decision, bodyID string, serving *string) Choice {
	if d.Class == router.Untouched || d.Class == "" {
		return Choice{}
	}
	st, err := s.cfg.State()
	if err != nil {
		s.emit(Event{Kind: "passthrough", Detail: "state: " + err.Error()})
		return Choice{}
	}

	if serving != nil {
		st.Serving = *serving
	}
	name, role := s.byClass(&st, d)
	if owner, ok := s.owner(d, bodyID); ok {
		if _, ok := findExact(&st, owner); ok {
			name, role = owner, RoleOwner
		} else {
			// The creator was logged out: its objects fall back to the
			// current remote/serving account.
			s.emit(Event{Kind: "owner-unregistered", Account: owner, Detail: string(d.Object)})
		}
	}
	if name == "" {
		s.emit(Event{Kind: "passthrough", Detail: "no " + role + " account set"})
		return Choice{}
	}
	acct, ok := findExact(&st, name)
	if !ok {
		s.emit(Event{Kind: "passthrough", Account: name, Detail: "no such account"})
		return Choice{}
	}
	tok, status, ok := s.token(ctx, acct.Dir, role)
	if !ok {
		s.emit(Event{Kind: "passthrough", Account: acct.Name, Status: status})
		return Choice{}
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

func (s *Selector) byClass(st *store.State, d router.Decision) (name, role string) {
	if d.Class == router.Remote {
		return st.Remote, RoleRemote
	}
	return st.Serving, RoleServing
}

// owner returns the account that created the object this request names.
func (s *Selector) owner(d router.Decision, bodyID string) (string, bool) {
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
