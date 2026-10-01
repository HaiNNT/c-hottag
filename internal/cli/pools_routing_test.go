package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// M8 Task 4: routing per pool. Accounts A, B, C, D; pool work is A and B,
// pool personal is B, C and D (B is in both). Every account also stays in
// default, so a request that falls back to default is told apart by its
// serving account, D.

func mustPools(t *testing.T, st *store.State, edit func(*store.State) error) {
	t.Helper()
	if err := edit(st); err != nil {
		t.Fatal(err)
	}
}

// withPools puts the pools and their roles into st: work serves A, remote A;
// personal serves C, remote D; default serves D.
func withPools(t *testing.T, st *store.State) {
	t.Helper()
	mustPools(t, st, func(st *store.State) error {
		for _, p := range []string{"work", "personal"} {
			if err := st.AddPool(p); err != nil {
				return err
			}
		}
		for _, j := range [][2]string{{"A", "work"}, {"B", "work"}, {"B", "personal"}, {"C", "personal"}, {"D", "personal"}} {
			if err := st.JoinPool(j[0], j[1]); err != nil {
				return err
			}
		}
		for _, s := range [][3]string{{"work", "A", "A"}, {"personal", "C", "D"}} {
			if err := st.SetPoolServing(s[0], s[1]); err != nil {
				return err
			}
			if err := st.SetPoolRemote(s[0], s[2]); err != nil {
				return err
			}
		}
		return st.SetPoolServing(store.DefaultPool, "D")
	})
}

func poolCtx(pool, sid string) context.Context {
	return proxy.ContextWithIdentityForTest(context.Background(), proxy.Identity{
		Caller: proxyauth.Caller{SID: sid, Pool: pool}, Inference: true,
	})
}

func inSet(name string, set ...string) bool {
	for _, s := range set {
		if s == name {
			return true
		}
	}
	return false
}

var (
	workMembers     = []string{"A", "B"}
	personalMembers = []string{"B", "C", "D"}
)

// poolSpreadRig is the spread rig over A..D with both pools on spread.
func poolSpreadRig(t *testing.T) *spreadRig {
	t.Helper()
	r := newSpreadRig(t, "A", "B", "C", "D")
	r.st.Accounts[0].NoRotate = false
	withPools(t, &r.st)
	for _, p := range []string{"work", "personal"} {
		mustPools(t, &r.st, func(st *store.State) error { return st.SetPoolPolicy(p, store.PolicySpread) })
	}
	return r
}

func (r *spreadRig) placeIn(pool, sid string) (string, bool) {
	r.t.Helper()
	return r.eng.accountFor(sid, "", pool, r.now)
}

func TestPoolSpreadPlacesOnlyOnMembers(t *testing.T) {
	r := poolSpreadRig(t)
	for i := 0; i < 12; i++ {
		sid := string(rune('a'+i)) + "w"
		if got, ok := r.placeIn("work", sid); !ok || !inSet(got, workMembers...) {
			t.Fatalf("work session %s placed on %q, %v; want A or B", sid, got, ok)
		}
		sid = string(rune('a'+i)) + "p"
		if got, ok := r.placeIn("personal", sid); !ok || !inSet(got, personalMembers...) {
			t.Fatalf("personal session %s placed on %q, %v; want B, C or D", sid, got, ok)
		}
	}
}

// Review focus 3: load counts the sessions of every pool on the account.
func TestPoolSpreadLoadCountsSessionsOfEveryPool(t *testing.T) {
	r := poolSpreadRig(t)
	mustPools(t, &r.st, func(st *store.State) error { return st.SetPoolPin("personal", "B") })
	for _, sid := range []string{"p1", "p2"} {
		if got, _ := r.placeIn("personal", sid); got != "B" {
			t.Fatalf("pinned personal session on %q, want B", got)
		}
	}
	// B carries two personal sessions: a work session sees that and prefers A,
	// then keeps preferring it until A carries as many.
	for _, sid := range []string{"w1", "w2"} {
		if got, _ := r.placeIn("work", sid); got != "A" {
			t.Fatalf("work session %s on %q, want A (B is loaded by personal sessions)", sid, got)
		}
	}
}

// Review focus 3: a limit seen on the shared account stops both pools
// choosing it, and the sessions already on it leave within their own pool.
func TestPoolSpreadALimitOnASharedAccountMovesBothPoolsWithinThem(t *testing.T) {
	r := poolSpreadRig(t)
	mustPools(t, &r.st, func(st *store.State) error {
		if err := st.SetPoolPin("work", "B"); err != nil {
			return err
		}
		return st.SetPoolPin("personal", "B")
	})
	r.placeIn("work", "w1")
	r.placeIn("personal", "p1")
	// C and D are far emptier than A: only the pool boundary keeps the work
	// session from them.
	r.usage("A", 60, 10)
	r.limit("B")
	if got, ok := r.placeIn("work", "w1"); !ok || got != "A" {
		t.Fatalf("work session left the limited B for %q, %v; want A", got, ok)
	}
	if got, ok := r.placeIn("personal", "p1"); !ok || !inSet(got, "C", "D") {
		t.Fatalf("personal session left the limited B for %q, %v; want C or D", got, ok)
	}
	if got, _ := r.placeIn("work", "w2"); got != "A" {
		t.Fatalf("a new work session got %q while B is limited", got)
	}
	if got, _ := r.placeIn("personal", "p2"); got == "B" {
		t.Fatal("a new personal session got the limited B")
	}
}

func TestPoolSpreadAMoveNeverLeavesThePool(t *testing.T) {
	r := poolSpreadRig(t)
	mustPools(t, &r.st, func(st *store.State) error { return st.SetPoolPin("work", "B") })
	r.placeIn("work", "w1")
	r.usage("A", 50, 50)
	r.usage("B", 97, 50) // at its switch point
	r.usage("C", 0, 0)
	r.usage("D", 0, 0)
	r.eng.mark("B", "B at its 5h switch point", r.now)
	if got, _ := r.placeIn("work", "w1"); got != "A" {
		t.Fatalf("a marked B's work session moved to %q, want A", got)
	}
	// Nowhere in the pool: A is limited too, so the session is unplaced (the
	// caller falls back), not sent to C or D.
	r.limit("A")
	r.advance(spreadMoveGap + time.Minute)
	if got, ok := r.placeIn("work", "w1"); ok && !inSet(got, workMembers...) {
		t.Fatalf("a work session was moved to %q, outside work", got)
	}
	if got, ok := r.eng.replaceIn("w1", "work", r.now); ok {
		t.Fatalf("a wall retry re-placed a work session on %q with every work account limited", got)
	}
}

func TestPoolSpreadReplaceStaysInThePool(t *testing.T) {
	r := poolSpreadRig(t)
	mustPools(t, &r.st, func(st *store.State) error { return st.SetPoolPin("work", "A") })
	r.placeIn("work", "w1")
	if got, ok := r.eng.replaceIn("w1", "work", r.now); !ok || got != "B" {
		t.Fatalf("replace gave %q, %v; want B", got, ok)
	}
}

// Review focus 2: pool leave while sessions are placed on the account.
func TestPoolSpreadASessionOnAnAccountThatLeftItsPoolReplacesWithinThePool(t *testing.T) {
	r := poolSpreadRig(t)
	mustPools(t, &r.st, func(st *store.State) error {
		if err := st.SetPoolPin("work", "B"); err != nil {
			return err
		}
		return st.SetPoolPin("personal", "B")
	})
	r.placeIn("work", "w1")
	r.placeIn("personal", "p1")
	mustPools(t, &r.st, func(st *store.State) error { return st.LeavePool("B", "work") })
	if got, ok := r.placeIn("work", "w1"); !ok || got != "A" {
		t.Fatalf("a work session on B after B left work got %q, %v; want A", got, ok)
	}
	if got, _ := r.placeIn("personal", "p1"); got != "B" {
		t.Fatalf("a personal session lost B, got %q", got)
	}
	// The pool's own roles were handed over coherently by the store.
	if w := r.st.PoolOf("work"); !inSet(w.Serving, "A") || w.Remote != "A" || w.Pin != "" {
		t.Fatalf("work roles after the leave: %+v", w)
	}
}

func TestPoolSpreadFallbackIsPerPool(t *testing.T) {
	r := poolSpreadRig(t)
	// The pool's own serving rotates: the caller uses it.
	if got, ok := r.eng.fallback("work", r.now); ok {
		t.Fatalf("fallback gave %q although work's serving rotates", got)
	}
	// Work's serving stops rotating: the least-bad rotating work member, B,
	// even limited, and never the idle C and D.
	r.st.Accounts[0].NoRotate = true
	r.limit("B")
	if got, ok := r.eng.fallback("work", r.now); !ok || got != "B" {
		t.Fatalf("fallback = %q, %v; want the limited work member B", got, ok)
	}
	// No rotating member at all: nothing, the caller serves the pool's serving.
	r.st.Accounts[1].NoRotate = true
	if got, ok := r.eng.fallback("work", r.now); ok {
		t.Fatalf("fallback gave %q with no rotating work member", got)
	}
}

func TestPoolSpreadServingForIsPerPool(t *testing.T) {
	r := poolSpreadRig(t)
	r.limit("B")
	if got, ok := r.eng.servingFor("work", "B", r.now); !ok || got != "A" {
		t.Fatalf("work servingFor = %q, %v; want A", got, ok)
	}
	if got, ok := r.eng.servingFor("personal", "B", r.now); !ok || !inSet(got, "C", "D") {
		t.Fatalf("personal servingFor = %q, %v; want C or D", got, ok)
	}
	r.limit("A")
	if got, ok := r.eng.servingFor("work", "A", r.now); ok {
		t.Fatalf("work servingFor = %q with both work accounts limited", got)
	}
}

func TestPoolSpreadARotationOffAccountNeverServes(t *testing.T) {
	r := poolSpreadRig(t)
	r.st.Accounts[3].NoRotate = true // D
	for i := 0; i < 6; i++ {
		sid := string(rune('a' + i))
		if got, _ := r.placeIn("personal", sid); got == "D" {
			t.Fatal("a rotation-off account was given a session")
		}
	}
}

// Chooser -------------------------------------------------------------

type poolChooseRig struct {
	*spreadRig
	ch  *chooser
	log *syncBuf
}

func newPoolChooseRig(t *testing.T, owner string) *poolChooseRig {
	t.Helper()
	r := newSpreadRig(t, "A", "B", "C", "D")
	r.st.Accounts[0].NoRotate = false
	r.st.Policy = ""
	withPools(t, &r.st)
	toks := fakeTokens2{}
	for _, a := range r.st.Accounts {
		toks[a.Dir] = "tok-" + a.Name
	}
	st := func() (store.State, error) { return r.st, nil }
	sel := selector.New(selector.Config{State: st, Tokens: toks, Owners: fakeOwners{owner: owner}})
	log := newSyncBuf()
	ch := &chooser{sel: sel, state: st, spread: r.eng, now: func() time.Time { return r.now }, log: log}
	return &poolChooseRig{spreadRig: r, ch: ch, log: log}
}

func (c *poolChooseRig) choose(ctx context.Context, d router.Decision) string {
	c.t.Helper()
	acct, tok, _, ok := c.ch.Choose(ctx, d, "")
	if !ok {
		return ""
	}
	if tok != "tok-"+acct {
		c.t.Fatalf("token %q is not %s's", tok, acct)
	}
	return acct
}

func TestChooserRoutesEachSessionThroughItsPool(t *testing.T) {
	c := newPoolChooseRig(t, "")
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{
		{poolCtx("work", "s1"), "A"},
		{poolCtx("personal", "s2"), "C"},
		{context.Background(), "D"}, // unidentified: default
		{poolCtx("default", "s3"), "D"},
	} {
		if got := c.choose(tc.ctx, servingDecision); got != tc.want {
			t.Errorf("serving request on %q, want %q", got, tc.want)
		}
	}
}

func TestChooserACreateGoesOutAsThePoolsRemote(t *testing.T) {
	c := newPoolChooseRig(t, "")
	create := router.Decision{Class: router.Remote, Object: router.KindSession}
	if got := c.choose(poolCtx("work", "s1"), create); got != "A" {
		t.Fatalf("a work create went out as %q, want A", got)
	}
	if got := c.choose(poolCtx("personal", "s2"), create); got != "D" {
		t.Fatalf("a personal create went out as %q, want D", got)
	}
}

func TestChooserAnOwnerLookupCrossesPools(t *testing.T) {
	c := newPoolChooseRig(t, "C") // C created it; C is not in work
	d := router.Decision{Class: router.Serving, Object: router.KindSession, ObjectID: "x"}
	if got := c.choose(poolCtx("work", "s1"), d); got != "C" {
		t.Fatalf("an owned object went out as %q, want its owner C", got)
	}
}

func TestChooserARemovedPoolIsDefaultAndLoggedOnce(t *testing.T) {
	c := newPoolChooseRig(t, "")
	for i := 0; i < 3; i++ {
		if got := c.choose(poolCtx("gone", "s1"), servingDecision); got != "D" {
			t.Fatalf("a session of a removed pool on %q, want default's D", got)
		}
	}
	if n := strings.Count(c.log.String(), `"gone"`); n != 1 {
		t.Fatalf("the removed pool was logged %d times, want once: %q", n, c.log.String())
	}
	c.choose(poolCtx("gone2", "s1"), servingDecision)
	if n := strings.Count(c.log.String(), "no longer exists"); n != 2 {
		t.Fatalf("a second removed pool was not logged: %q", c.log.String())
	}
}

// A work session is never served by a personal-only account on any serving
// path: placement, a lost placement (fallback), and a stale state.
func TestChooserAWorkSessionIsNeverServedByANonMember(t *testing.T) {
	c := newPoolChooseRig(t, "")
	mustPools(t, &c.st, func(st *store.State) error { return st.SetPoolPolicy("work", store.PolicySpread) })
	c.usage("A", 90, 10)
	c.usage("B", 90, 10)
	c.usage("C", 0, 0)
	c.usage("D", 0, 0)
	for i := 0; i < 6; i++ {
		sid := string(rune('a' + i))
		if got := c.choose(poolCtx("work", sid), servingDecision); !inSet(got, workMembers...) {
			t.Fatalf("a work session was served by %q", got)
		}
	}
	// Every work account limited: the unplaced session stays in work, or is
	// refused; never Home's login (an empty result without a refusal) and
	// never C or D.
	c.limit("A")
	c.limit("B")
	if got, refusal := c.chooseGuarded(poolCtx("work", "late"), servingDecision); got != "" && !inSet(got, workMembers...) || got == "" && refusal == "" {
		t.Fatalf("a work session got %q, refusal %q with work limited", got, refusal)
	}
	// A serving role that outlived its membership (hand-edited state) is
	// refused with the pool named, not sent as the non-member or as Home.
	c.st.Pools["work"] = store.Pool{Serving: "C", Remote: "A", Policy: ""}
	got, refusal := c.chooseGuarded(poolCtx("work", "s9"), servingDecision)
	if got != "" || !strings.Contains(refusal, `pool "work"`) {
		t.Fatalf("a work session got %q, refusal %q; want a refusal naming the pool", got, refusal)
	}
}

func (c *poolChooseRig) chooseGuarded(ctx context.Context, d router.Decision) (string, string) {
	c.t.Helper()
	acct, _, _, ok, refusal := c.ch.ChooseGuarded(ctx, d, "")
	if !ok {
		acct = ""
	}
	return acct, refusal
}

func TestChooserRefusesAPoolOfRotationOffMembersAndNeverUsesHome(t *testing.T) {
	c := newPoolChooseRig(t, "")
	c.st.Accounts[0].NoRotate, c.st.Accounts[1].NoRotate = true, true // A and B: all of work
	c.st.Pools["work"] = store.Pool{Remote: "A"}                      // JoinPool never makes a rotation-off account serving
	got, refusal := c.chooseGuarded(poolCtx("work", "s1"), servingDecision)
	if got != "" || refusal != `chottag: no account in pool "work" can serve this request (run: chottag pool)` {
		t.Fatalf("got %q, %q; want the work refusal", got, refusal)
	}
	// its remote still serves creates
	if got, _ := c.chooseGuarded(poolCtx("work", "s1"), router.Decision{Class: router.Remote}); got != "A" {
		t.Fatalf("a work create went out as %q, want A", got)
	}
	// the refusal is logged once a minute per pool
	c.chooseGuarded(poolCtx("work", "s1"), servingDecision)
	if n := strings.Count(c.log.String(), "refused a request"); n != 1 {
		t.Fatalf("logged %d times, want once: %q", n, c.log.String())
	}
	c.now = c.now.Add(2 * time.Minute)
	c.chooseGuarded(poolCtx("work", "s1"), servingDecision)
	if n := strings.Count(c.log.String(), "refused a request"); n != 2 {
		t.Fatalf("logged %d times after a minute, want 2", n)
	}
}

// R90 in a serial non-default pool: a rotation-off serving account falls back
// to the pool's least-bad rotating member, never to C or D.
func TestChooserARotationOffServingFallsBackWithinTheSerialPool(t *testing.T) {
	c := newPoolChooseRig(t, "")
	c.st.Accounts[0].NoRotate = true // A, work's serving
	if got, refusal := c.chooseGuarded(poolCtx("work", "s1"), servingDecision); got != "B" || refusal != "" {
		t.Fatalf("got %q, %q; want B", got, refusal)
	}
	c.st.Accounts[1].NoRotate = true
	if got, refusal := c.chooseGuarded(poolCtx("work", "s1"), servingDecision); got != "" || refusal == "" {
		t.Fatalf("got %q, %q; want a refusal with no rotating member", got, refusal)
	}
}

func TestChooserARefusesWhenEveryMemberOfThePoolLacksALogin(t *testing.T) {
	c := newPoolChooseRig(t, "")
	st := func() (store.State, error) { return c.st, nil }
	toks := fakeTokens2{}
	for _, a := range c.st.Accounts {
		if a.Name == "A" { // every personal member is without a login
			toks[a.Dir] = "tok-" + a.Name
		}
	}
	c.ch.sel = selector.New(selector.Config{State: st, Tokens: toks, Owners: fakeOwners{}})
	if got, refusal := c.chooseGuarded(poolCtx("personal", "s1"), servingDecision); got != "" || refusal == "" {
		t.Fatalf("got %q, %q; want a refusal", got, refusal)
	}
	if c.ch.Guarded() != true {
		t.Fatal("the chooser does not report the boundary with several pools")
	}
}

// With only default, a request that cannot be served still passes through
// (the plain Choose false), with no refusal and no boundary.
func TestChooserWithOnlyDefaultStillPassesThrough(t *testing.T) {
	r := newSpreadRig(t, "A", "B")
	r.st.Policy = ""
	st := func() (store.State, error) { return r.st, nil }
	sel := selector.New(selector.Config{State: st, Tokens: fakeTokens2{}, Owners: fakeOwners{}})
	ch := &chooser{sel: sel, state: st, spread: r.eng, now: func() time.Time { return r.now }}
	acct, _, _, ok, refusal := ch.ChooseGuarded(idCtx("s1"), servingDecision, "")
	if acct != "" || ok || refusal != "" || ch.Guarded() {
		t.Fatalf("got %q ok=%v refusal=%q guarded=%v, want today's passthrough", acct, ok, refusal, ch.Guarded())
	}
}

// Two snapshots: the engine places a work session on B from one view, then a
// `pool leave` lands before the selector's read. The chooser re-resolves once
// within the pool instead of refusing or serving Home.
func TestChooserReResolvesOnceAcrossAPoolLeave(t *testing.T) {
	c := newPoolChooseRig(t, "")
	mustPools(t, &c.st, func(st *store.State) error {
		if err := st.SetPoolPolicy("work", store.PolicySpread); err != nil {
			return err
		}
		return st.SetPoolPin("work", "B")
	})
	left := c.st
	left.Pools = map[string]store.Pool{}
	for k, v := range c.st.Pools {
		left.Pools[k] = v
	}
	left.Accounts = append([]store.Account(nil), c.st.Accounts...)
	mustPools(t, &left, func(st *store.State) error { return st.LeavePool("B", "work") })
	// the engine reads the live c.st; the selector's first read flips it
	var reads int
	selState := func() (store.State, error) {
		reads++
		if reads == 1 {
			c.st = left
		}
		return c.st, nil
	}
	toks := fakeTokens2{}
	for _, a := range c.st.Accounts {
		toks[a.Dir] = "tok-" + a.Name
	}
	c.ch.sel = selector.New(selector.Config{State: selState, Tokens: toks, Owners: fakeOwners{}})
	acct, refusal := c.chooseGuarded(poolCtx("work", "s1"), servingDecision)
	if acct != "A" || refusal != "" {
		t.Fatalf("got %q, %q after the leave race; want A within work", acct, refusal)
	}
}

// Auto-switch --------------------------------------------------------

// A rotation-off, idle, in-pool member is never a serial pool's switch target
// (R90 through poolView).
func TestAutoARotationOffMemberIsNeverAPoolsSwitchTarget(t *testing.T) {
	r := poolAutoRig(t)
	r.update(func(st *store.State) {
		for i := range st.Accounts {
			if st.Accounts[i].Name == "B" {
				st.Accounts[i].NoRotate = true
			}
		}
	})
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if got := r.poolServing("work"); got == "B" {
		t.Fatal("work switched to a rotation-off member")
	}
	retry, done := r.as.wallRetry(poolCtx("work", "s1"), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if retry && done != nil {
		done("", 0)
	}
	if got := r.poolServing("work"); got == "B" || !inSet(got, workMembers...) {
		t.Fatalf("after the wall retry work serves from %q", got)
	}
}

// A removed pool leaves no memory behind.
func TestAutoARemovedPoolsMemoryIsPruned(t *testing.T) {
	r := poolAutoRig(t)
	r.as.mu.Lock()
	r.as.poolState("work").known = "A"
	r.as.mu.Unlock()
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error {
			if err := st.LeavePool("A", "work"); err != nil {
				return err
			}
			if err := st.LeavePool("B", "work"); err != nil {
				return err
			}
			return st.RemovePool("work")
		})
	})
	r.as.evaluate(r.now, false)
	r.as.mu.Lock()
	defer r.as.mu.Unlock()
	if len(r.as.others) != 1 || r.as.others["personal"] == nil {
		t.Fatalf("pool memory after the removal: %v", r.as.others)
	}
}

func poolAutoRig(t *testing.T, rows ...status.Account) *autoRig {
	t.Helper()
	r := newAutoRig(t, rows...)
	r.update(func(st *store.State) { withPools(t, st) })
	return r
}

func limitedRow(name string, at time.Time) status.Account {
	row := freshRow(name, 100, 10, at)
	row.Limited, row.LimitedUntil, row.Window = true, at.Add(2*time.Hour), "five_hour"
	return row
}

func (r *autoRig) poolServing(pool string) string {
	r.t.Helper()
	st, err := r.s.Load()
	if err != nil {
		r.t.Fatal(err)
	}
	return st.PoolOf(pool).Serving
}

func TestAutoEachSerialPoolSwitchesOnItsOwnWithinItsMembers(t *testing.T) {
	r := poolAutoRig(t)
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if got := r.poolServing("work"); got != "B" {
		t.Fatalf("work serving = %q after A passed its point, want B", got)
	}
	if got := r.poolServing("personal"); got != "C" {
		t.Fatalf("personal serving = %q, want C untouched", got)
	}
	if got := r.serving(); got != "D" {
		t.Fatalf("default serving = %q, want D untouched", got)
	}
}

func TestAutoTwoPoolsMayShareAServingAccountAndBothLeaveItWithinThemselves(t *testing.T) {
	r := poolAutoRig(t)
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error {
			if err := st.SetPoolServing("work", "B"); err != nil {
				return err
			}
			return st.SetPoolServing("personal", "B")
		})
	})
	// A is empty and C, D are busier than B would be; B is at its point.
	r.row(busyRow("B", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if got := r.poolServing("work"); got != "A" {
		t.Fatalf("work left B for %q, want A", got)
	}
	if got := r.poolServing("personal"); !inSet(got, "C", "D") {
		t.Fatalf("personal left B for %q, want C or D", got)
	}
}

func TestAutoAWorkPoolNeverSwitchesToANonMember(t *testing.T) {
	// A and B are both over their points; C and D are idle. The planner's
	// fallback may keep work on a member, never move it to C or D.
	r := poolAutoRig(t, busyRow("A", 99.5, eligNow, 3*time.Hour), busyRow("B", 99.5, eligNow, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if got := r.poolServing("work"); !inSet(got, workMembers...) {
		t.Fatalf("work serving = %q, outside the pool", got)
	}
}

// R72 within the pool: a limit on the work session's serving account moves
// work's serving to a work member only.
func TestAutoWallRetryStaysInTheSessionsPool(t *testing.T) {
	r := poolAutoRig(t, onlyB(eligNow)...) // C and D unusable; B fine
	retry, done := r.as.wallRetry(poolCtx("work", "s1"), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit on work's serving was not retried")
	}
	if got := r.poolServing("work"); got != "B" {
		t.Fatalf("work serving = %q, want B", got)
	}
	done("B", 200)
	// The resend marks work's own published last switch, not default's.
	p := r.published()
	if w := p.Pools["work"]; w.LastSwitch == nil || !w.LastSwitch.Retried || w.LastSwitch.To != "B" {
		t.Fatalf("work's lastSwitch = %+v, want a retried A->B", w.LastSwitch)
	}
	if p.LastSwitch != nil && p.LastSwitch.Retried {
		t.Fatalf("default's lastSwitch = %+v: the resend was work's", p.LastSwitch)
	}
}

// A new daemon generation republishes each pool's last switch from
// status.json's auto.pools, and a threshold one restarts its cooldown.
func TestAutoARestartSeedsEachPoolsLastSwitch(t *testing.T) {
	r := poolAutoRig(t)
	at := eligNow.Add(-5 * time.Minute)
	r.sink.setAuto(status.Auto{Mode: "balanced", Pools: map[string]status.PoolAuto{
		"work": {LastSwitch: &status.AutoSwitch{From: "A", To: "B", Trigger: "threshold", Window: "5h", Pct: 95, At: at}},
	}}, true)
	cache := store.NewCache(r.s)
	as := newAutoSwitcher(r.s, cache, r.sink, r.dn, newSyncBuf(), nil)
	as.now = func() time.Time { return eligNow }
	as.mu.Lock()
	w := as.others["work"]
	as.mu.Unlock()
	if w == nil || w.lastSwitch == nil || w.lastSwitch.To != "B" || !w.lastSoft.Equal(at) {
		t.Fatalf("work's memory not seeded: %+v", w)
	}
	as.evaluate(eligNow, false)
	if ls := r.sink.auto().Pools["work"].LastSwitch; ls == nil || ls.To != "B" {
		t.Fatalf("work's last switch was not republished: %+v", ls)
	}
	if as.lastSwitch != nil {
		t.Fatalf("default's lastSwitch = %+v", as.lastSwitch)
	}
}

// Each pool's no-candidate episode is keyed by the pool, so removing a pool
// ends its episode and a pool added again under the name announces again.
func TestAutoNoCandidateEpisodesFollowPoolRemovalAndReAdd(t *testing.T) {
	r := poolAutoRig(t)
	r.as.evaluate(r.now, false) // every pool has its memory, as after a real decision
	r.notices()
	titles := func() []string {
		var out []string
		for _, n := range r.notices() {
			out = append(out, n.title)
		}
		return out
	}
	r.as.notify.noCandidate("work", "work", "A", false)
	r.as.notify.noCandidate("work", "work", "A", false)
	if got := titles(); len(got) != 1 || got[0] != "chottag: work: no account to switch to" {
		t.Fatalf("notices = %q", got)
	}
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error {
			if err := st.LeavePool("A", "work"); err != nil {
				return err
			}
			if err := st.LeavePool("B", "work"); err != nil {
				return err
			}
			return st.RemovePool("work")
		})
	})
	r.as.evaluate(r.now, false)
	r.update(func(st *store.State) { mustPools(t, st, func(st *store.State) error { return st.AddPool("work") }) })
	r.as.notify.noCandidate("work", "work", "A", false)
	if got := titles(); len(got) != 1 {
		t.Fatalf("a re-added pool did not announce again: %q", got)
	}
}

func TestAutoWallRetryWithNoMemberLeftNeverMovesToANonMember(t *testing.T) {
	r := poolAutoRig(t)
	r.row(limitedRow("B", r.now))
	retry, done := r.as.wallRetry(poolCtx("work", "s1"), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if retry && done != nil {
		done("", 0)
	}
	if got := r.poolServing("work"); !inSet(got, workMembers...) {
		t.Fatalf("work serving = %q after a limit with B limited, outside the pool", got)
	}
}

// Review focus 3: a limit met on the shared account by a session of one pool
// moves the other pool off it too.
func TestAutoALimitOnASharedAccountSeenViaOnePoolMovesTheOther(t *testing.T) {
	r := poolAutoRig(t)
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error {
			if err := st.SetPoolServing("work", "B"); err != nil {
				return err
			}
			return st.SetPoolServing("personal", "B")
		})
	})
	retry, done := r.as.wallRetry(poolCtx("work", "s1"), "B", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry {
		t.Fatal("not retried")
	}
	if done != nil {
		done("A", 200)
	}
	if got := r.poolServing("personal"); got == "B" {
		t.Fatal("personal still serves from the limited B")
	}
	if got := r.poolServing("work"); got != "A" {
		t.Fatalf("work serving = %q, want A", got)
	}
}

func TestAutoSpreadPoolKeepsItsServingInsideItsMembers(t *testing.T) {
	r := poolAutoRig(t)
	r.as.spread = newSpreadEngine(r.home+"/run/placements.json", r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now)
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error { return st.SetPoolPolicy("work", store.PolicySpread) })
	})
	r.as.evaluate(r.now, false) // seeds
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if got := r.poolServing("work"); got != "B" {
		t.Fatalf("spread work serving = %q, want B", got)
	}
	if got := r.poolServing("personal"); got != "C" {
		t.Fatalf("serial personal serving = %q, want C", got)
	}
}

// Review focus 5: pool join/leave racing the auto-switch never leaves a
// pool's serving account outside the pool.
func TestAutoConcurrentJoinAndLeaveNeverLeavesServingOutsideThePool(t *testing.T) {
	r := poolAutoRig(t)
	st, _ := r.state()
	if !st.HasPool("work") {
		t.Fatal("setup")
	}
	var wg sync.WaitGroup
	violation := make(chan string, 1)
	check := func() {
		st, err := r.s.Load()
		if err != nil {
			return
		}
		for _, pool := range []string{"work", "personal"} {
			sv := st.PoolOf(pool).Serving
			ok := sv == ""
			for _, m := range st.Members(pool) {
				if m.Name == sv {
					ok = true
				}
			}
			if !ok {
				select {
				case violation <- pool + " serves " + sv:
				default:
				}
			}
		}
	}
	wg.Add(3)
	go func() { // membership churn on B and C
		defer wg.Done()
		for i := 0; i < 40; i++ {
			for _, op := range []func(*store.State) error{
				func(st *store.State) error { return st.LeavePool("B", "work") },
				func(st *store.State) error { return st.JoinPool("B", "work") },
				func(st *store.State) error { return st.LeavePool("C", "personal") },
				func(st *store.State) error { return st.JoinPool("C", "personal") },
			} {
				_, _ = r.s.Update(op) // ErrLastPool and friends are fine
				check()
			}
		}
	}()
	go func() { // the auto-switch, under moving usage
		defer wg.Done()
		for i := 0; i < 80; i++ {
			pct := 10.0
			if i%2 == 0 {
				pct = 97
			}
			for _, n := range []string{"A", "B", "C"} {
				r.row(busyRow(n, pct, r.now, 3*time.Hour))
			}
			r.as.evaluate(r.now.Add(time.Duration(i)*time.Hour), false)
			check()
		}
	}()
	go func() { // the daemon's own cached view, as a request would read it
		defer wg.Done()
		for i := 0; i < 80; i++ {
			r.as.invalidate()
			check()
		}
	}()
	wg.Wait()
	check()
	select {
	case v := <-violation:
		t.Fatalf("a pool's serving account was outside the pool: %s", v)
	default:
	}
}

// R72 for a spread pool: the session re-places within its pool.
func TestAutoSpreadWallRetryReplacesWithinTheSessionsPool(t *testing.T) {
	r := poolAutoRig(t)
	r.as.spread = newSpreadEngine(r.home+"/run/placements.json", r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now)
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error {
			if err := st.SetPoolPolicy("work", store.PolicySpread); err != nil {
				return err
			}
			return st.SetPoolPin("work", "A")
		})
	})
	if got, ok := r.as.spread.accountFor("s1", "", "work", r.now); !ok || got != "A" {
		t.Fatalf("s1 placed on %q, %v; want the pinned A", got, ok)
	}
	r.row(limitedRow("A", r.now)) // C and D stay idle
	retry, done := r.as.wallRetry(poolCtx("work", "s1"), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry {
		t.Fatal("a limit on a placed work session was not retried")
	}
	if done != nil {
		done("B", 200)
	}
	if got, _ := r.as.spread.placed("s1"); got != "B" {
		t.Fatalf("s1 re-placed on %q, want B (a work member)", got)
	}
}

// A state read error on the chooser's own read must not let the selector's
// later, successful read route a work session as default.
func TestChooserAStateReadErrorRefusesTheCallersPool(t *testing.T) {
	c := newPoolChooseRig(t, "")
	good := c.ch.state
	fail := true
	c.ch.state = func() (store.State, error) {
		if fail {
			fail = false
			return store.State{}, errors.New("stat: too many open files")
		}
		return good()
	}
	// no good read yet (multi unknown): the identity names a non-default pool
	got, refusal := c.chooseGuarded(poolCtx("work", "s1"), servingDecision)
	if got != "" || !strings.Contains(refusal, `pool "work"`) {
		t.Fatalf("got %q, %q; want a refusal naming work, not default's account", got, refusal)
	}
	// once pools are known, an unidentified caller is refused too, as default
	if got, _ := c.chooseGuarded(poolCtx("work", "s1"), servingDecision); got == "" {
		t.Fatal("the next read is good: want work's serving")
	}
	fail = true
	got, refusal = c.chooseGuarded(context.Background(), servingDecision)
	if got != "" || !strings.Contains(refusal, `pool "default"`) {
		t.Fatalf("got %q, %q; want a refusal naming default", got, refusal)
	}
}

// With only default and a failed chooser read, the pre-M8 path stays: the
// selector reads for itself and serves.
func TestChooserAStateReadErrorWithOnlyDefaultLetsTheSelectorServe(t *testing.T) {
	r := newSpreadRig(t, "A", "B")
	r.st.Policy = ""
	sel := selector.New(selector.Config{State: func() (store.State, error) { return r.st, nil }, Tokens: fakeTokens2{r.st.Accounts[0].Dir: "tok-A"}, Owners: fakeOwners{}})
	ch := &chooser{sel: sel, state: func() (store.State, error) { return store.State{}, errors.New("boom") }}
	for _, ctx := range []context.Context{context.Background(), idCtx("s1")} {
		acct, tok, _, ok, refusal := ch.ChooseGuarded(ctx, servingDecision, "")
		if !ok || acct != "A" || tok != "tok-A" || refusal != "" {
			t.Fatalf("got %q ok=%v refusal=%q, want the selector's own answer A", acct, ok, refusal)
		}
	}
}

// A rotating serving account that cannot serve (no login) hands over to the
// pool's healthy member before the request fails closed.
func TestChooserAServingWithoutALoginFallsBackToAHealthyMember(t *testing.T) {
	c := newPoolChooseRig(t, "")
	st := func() (store.State, error) { return c.st, nil }
	toks := fakeTokens2{}
	for _, a := range c.st.Accounts {
		if a.Name != "A" { // work's serving has no login
			toks[a.Dir] = "tok-" + a.Name
		}
	}
	c.ch.sel = selector.New(selector.Config{State: st, Tokens: toks, Owners: fakeOwners{}})
	if got, refusal := c.chooseGuarded(poolCtx("work", "s1"), servingDecision); got != "B" || refusal != "" {
		t.Fatalf("got %q, %q; want the healthy member B", got, refusal)
	}
	// and a spread pool whose placement has no login
	mustPools(t, &c.st, func(st *store.State) error { return st.SetPoolPolicy("work", store.PolicySpread) })
	mustPools(t, &c.st, func(st *store.State) error { return st.SetPoolPin("work", "A") })
	if got, refusal := c.chooseGuarded(poolCtx("work", "s2"), servingDecision); got != "B" || refusal != "" {
		t.Fatalf("spread: got %q, %q; want B", got, refusal)
	}
	// no healthy member: refused
	delete(toks, c.st.Accounts[1].Dir)
	if got, refusal := c.chooseGuarded(poolCtx("work", "s3"), servingDecision); got != "" || refusal == "" {
		t.Fatalf("got %q, %q; want a refusal", got, refusal)
	}
}

// status.json's auto: the default pool's decision and last switch stay in
// place, and each other pool carries its own (M8). The notice names the pool.
func TestAutoPublishesEachPoolsDecisionAndLastSwitch(t *testing.T) {
	r := poolAutoRig(t)
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	p := r.published()
	if p.LastSwitch != nil {
		t.Fatalf("default's lastSwitch = %+v: only work switched", p.LastSwitch)
	}
	w, ok := p.Pools["work"]
	if !ok || w.LastSwitch == nil || w.LastSwitch.From != "A" || w.LastSwitch.To != "B" || w.Decision == "" {
		t.Fatalf("work's auto = %+v", p.Pools)
	}
	if _, ok := p.Pools["default"]; ok {
		t.Fatalf("default is published in place, not under pools: %+v", p.Pools)
	}
	if _, ok := p.Pools["personal"]; !ok {
		t.Fatalf("personal has no entry: %+v", p.Pools)
	}
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: work: switched to B" {
		t.Fatalf("notices = %q", got)
	}
}

// Two members whose logins are stale (status still shows them healthy) must
// not 503 a pool whose third member can serve: the fallback tries each member
// once before it fails closed.
func TestChooserFallbackTriesEveryMemberBeforeFailingClosed(t *testing.T) {
	c := newPoolChooseRig(t, "")
	st := func() (store.State, error) { return c.st, nil }
	toks := fakeTokens2{}
	for _, a := range c.st.Accounts {
		if a.Name == "D" { // personal serves C; C and B have no usable token
			toks[a.Dir] = "tok-D"
		}
	}
	c.ch.sel = selector.New(selector.Config{State: st, Tokens: toks, Owners: fakeOwners{}})
	if got, refusal := c.chooseGuarded(poolCtx("personal", "s1"), servingDecision); got != "D" || refusal != "" {
		t.Fatalf("got %q, %q; want D", got, refusal)
	}
	// and with D gone too it still fails closed
	c.ch.sel = selector.New(selector.Config{State: st, Tokens: fakeTokens2{}, Owners: fakeOwners{}})
	if got, refusal := c.chooseGuarded(poolCtx("personal", "s1"), servingDecision); got != "" || refusal == "" {
		t.Fatalf("got %q, %q; want a refusal", got, refusal)
	}
}
