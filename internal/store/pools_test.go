package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func names(as []store.Account) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

func readState(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// defaultOnly is what the pre-M8 Save wrote for this state, byte for byte.
const defaultOnly = `{
  "version": 1,
  "accounts": [
    {
      "name": "a",
      "email": "alice@example.com",
      "dir": "/slots/a",
      "addedAt": "2026-01-02T03:04:05Z"
    },
    {
      "name": "b",
      "email": "bob@example.com",
      "dir": "/slots/b",
      "addedAt": "2026-01-02T03:04:05Z",
      "noRotate": true
    }
  ],
  "serving": "a",
  "remote": "a",
  "port": 47821,
  "policy": "spread",
  "pin": "b"
}
`

func TestDefaultOnlyStateRoundTripsByteIdentically(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(defaultOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (store.Store{Dir: dir}).Update(func(*store.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := readState(t, dir); got != defaultOnly {
		t.Fatalf("round trip changed the file:\n%s", got)
	}
	dir2 := t.TempDir()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := store.Store{Dir: dir2}.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "a", Email: "alice@example.com", Dir: "/slots/a", AddedAt: at, PoolList: []string{"default"}}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "b", Email: "bob@example.com", Dir: "/slots/b", AddedAt: at, NoRotate: true}); err != nil {
			return err
		}
		st.SetPolicy(store.PolicySpread)
		st.SetPin("b")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readState(t, dir2); got != defaultOnly {
		t.Fatalf("API-built state differs:\n%s", got)
	}
}

func TestVersionIsTwoOnlyWhileAnExtraPoolExists(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error { return st.AddPool("work") }); err != nil {
		t.Fatal(err)
	}
	if got := readState(t, dir); !strings.Contains(got, `"version": 2`) || !strings.Contains(got, `"pools"`) {
		t.Fatalf("with a pool:\n%s", got)
	}
	st, err := s.Load()
	if err != nil || !st.HasPool("work") {
		t.Fatalf("load v2: %+v %v", st, err)
	}
	if _, err := s.Update(func(st *store.State) error { return st.RemovePool("work") }); err != nil {
		t.Fatal(err)
	}
	if got := readState(t, dir); !strings.Contains(got, `"version": 1`) || strings.Contains(got, "pools") {
		t.Fatalf("after removing the pool:\n%s", got)
	}
}

func TestLoadAcceptsOneAndTwoRefusesThree(t *testing.T) {
	if store.Version != 2 {
		t.Fatalf("Version = %d", store.Version)
	}
	for v, ok := range map[string]bool{"1": true, "2": true, "3": false} {
		dir := t.TempDir()
		body := `{"version":` + v + `,"accounts":[],"port":1}`
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := store.Store{Dir: dir}.Load()
		if ok && err != nil {
			t.Errorf("v%s: %v", v, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "newer")) {
			t.Errorf("v%s: err = %v", v, err)
		}
	}
}

func TestPoolKeysAreKnownAndUnknownKeysSurvive(t *testing.T) {
	dir := t.TempDir()
	in := `{"version":2,"accounts":[{"name":"a","dir":"/slots/a","addedAt":"2026-01-02T03:04:05Z","Pools":["work"],"future":1}],"port":1,"Pools":{"work":{"serving":"a"}},"newer":{"x":true}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Store{Dir: dir}.Update(func(*store.State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.Accounts[0].PoolList, []string{"work"}) || st.Pools["work"].Serving != "a" {
		t.Fatalf("not decoded: %+v", st)
	}
	got := readState(t, dir)
	for _, want := range []string{`"future": 1`, `"newer"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"Pools"`) || strings.Count(got, `"pools"`) != 2 {
		t.Errorf("pool keys kept as extras or duplicated:\n%s", got)
	}
}

func TestValidPoolName(t *testing.T) {
	for n, want := range map[string]bool{
		"work": true, "a-1": true, "0": true, "": false, "Work": false, "a_b": false,
		"a b": false, "abcdefghijklmnop": true, "abcdefghijklmnopq": false,
	} {
		if got := store.ValidPoolName(n); got != want {
			t.Errorf("ValidPoolName(%q) = %v", n, got)
		}
	}
}

func TestAddAndRemovePool(t *testing.T) {
	st := store.Default()
	if err := st.AddPool("Bad Name"); !errors.Is(err, store.ErrBadPool) {
		t.Errorf("bad name: %v", err)
	}
	if err := st.AddPool("default"); !errors.Is(err, store.ErrPoolExists) {
		t.Errorf("default: %v", err)
	}
	if err := st.AddPool("work"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPool("work"); !errors.Is(err, store.ErrPoolExists) {
		t.Errorf("dup: %v", err)
	}
	if err := st.AddPool("alpha"); err != nil {
		t.Fatal(err)
	}
	if got := st.PoolNames(); !reflect.DeepEqual(got, []string{"default", "alpha", "work"}) {
		t.Errorf("PoolNames = %v", got)
	}
	if !st.HasPool("default") || !st.HasPool("work") || st.HasPool("nope") {
		t.Error("HasPool")
	}
	if err := st.RemovePool("default"); !errors.Is(err, store.ErrPoolDefault) {
		t.Errorf("remove default: %v", err)
	}
	if err := st.RemovePool("nope"); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("remove missing: %v", err)
	}
	if err := st.Add(store.Account{Name: "w1", Dir: "/slots/w1", PoolList: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RemovePool("work"); !errors.Is(err, store.ErrPoolNotEmpty) {
		t.Errorf("remove non-empty: %v", err)
	}
	if err := st.RemovePool("alpha"); err != nil || st.HasPool("alpha") {
		t.Errorf("remove empty: %v", err)
	}
}

func poolState(t *testing.T) store.State {
	t.Helper()
	st := store.Default()
	if err := st.AddPool("work"); err != nil {
		t.Fatal(err)
	}
	w := []string{"work"}
	for _, a := range []store.Account{
		{Name: "d1", Dir: "/slots/d1"},
		{Name: "d2", Dir: "/slots/d2", NoRotate: true},
		{Name: "d3", Dir: "/slots/d3"},
		{Name: "w1", Dir: "/slots/w1", PoolList: w},
		{Name: "w2", Dir: "/slots/w2", PoolList: w, NoRotate: true},
		{Name: "w3", Dir: "/slots/w3", PoolList: w},
	} {
		if err := st.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestMembershipAndAddFillsEachPoolsFirstRoles(t *testing.T) {
	st := poolState(t)
	if p := st.PoolOf("default"); p.Serving != "d1" || p.Remote != "d1" || st.Serving != "d1" {
		t.Errorf("default = %+v", p)
	}
	if p := st.PoolOf("work"); p.Serving != "w1" || p.Remote != "w1" {
		t.Errorf("work = %+v", p)
	}
	if got := names(st.Members("work")); !reflect.DeepEqual(got, []string{"w1", "w2", "w3"}) {
		t.Errorf("work members = %v", got)
	}
	if got := names(st.Members("default")); !reflect.DeepEqual(got, []string{"d1", "d2", "d3"}) {
		t.Errorf("default members = %v", got)
	}
	d, w := st.Accounts[0], st.Accounts[3]
	if !reflect.DeepEqual(d.InPools(), []string{"default"}) || !d.InPool("default") || d.InPool("work") {
		t.Errorf("default account pools = %v", d.InPools())
	}
	if !reflect.DeepEqual(w.InPools(), []string{"work"}) || w.InPool("default") || !w.InPool("work") {
		t.Errorf("work account pools = %v", w.InPools())
	}
	if err := st.Add(store.Account{Name: "x", Dir: "/slots/x", PoolList: []string{"nope"}}); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("add into a missing pool: %v", err)
	}
	// An account added to two pools fills both pools' empty roles.
	_ = st.AddPool("two")
	_ = st.AddPool("three")
	if err := st.Add(store.Account{Name: "m", Dir: "/slots/m", PoolList: []string{"two", "three"}}); err != nil {
		t.Fatal(err)
	}
	if st.PoolOf("two").Serving != "m" || st.PoolOf("three").Remote != "m" {
		t.Errorf("roles: %+v %+v", st.PoolOf("two"), st.PoolOf("three"))
	}
}

func TestPoolSettersScopeToThePool(t *testing.T) {
	st := poolState(t)
	if err := st.SetPoolServing("work", "w3"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPoolRemote("work", "w2"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPoolPolicy("work", store.PolicySpread); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPoolPin("work", "w3"); err != nil {
		t.Fatal(err)
	}
	want := store.Pool{Serving: "w3", Remote: "w2", Policy: store.PolicySpread, Pin: "w3"}
	if got := st.PoolOf("work"); got != want {
		t.Errorf("work = %+v", got)
	}
	if st.Serving != "d1" || st.Policy != "" || st.Pin != "" {
		t.Errorf("default touched: %+v", st)
	}
	if err := st.SetPoolServing("default", "d3"); err != nil || st.Serving != "d3" {
		t.Errorf("default serving: %v %q", err, st.Serving)
	}
	if err := st.SetPoolPolicy("work", store.PolicySerial); err != nil || st.PoolOf("work").Policy != "" {
		t.Errorf("serial not stored as absent: %v", err)
	}
	if err := st.SetPoolServing("nope", "w1"); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("missing pool: %v", err)
	}
	if err := st.SetPoolServing("work", "d1"); !errors.Is(err, store.ErrNotInPool) {
		t.Errorf("non-member: %v", err)
	}
	if err := st.SetPoolServing("work", "zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	if err := st.SetPoolPin("work", ""); err != nil || st.PoolOf("work").Pin != "" {
		t.Errorf("clear pin: %v", err)
	}
}

func TestNextInPoolRotatesAmongRotatingMembers(t *testing.T) {
	st := poolState(t)
	for cur, want := range map[string]string{"w1": "w3", "w3": "w1", "w2": "w3", "": "w1", "d1": "w1"} {
		got, err := st.NextInPool("work", cur)
		if err != nil || got.Name != want {
			t.Errorf("NextInPool(work, %q) = %q, %v; want %q", cur, got.Name, err, want)
		}
	}
	if _, err := st.NextInPool("nope", ""); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("missing pool: %v", err)
	}
	_ = st.AddPool("empty")
	if _, err := st.NextInPool("empty", ""); !errors.Is(err, store.ErrNoAccounts) {
		t.Errorf("empty pool: %v", err)
	}
	if got, err := st.NextInPool("default", "d1"); err != nil || got.Name != "d3" {
		t.Errorf("default next = %q, %v", got.Name, err)
	}
}

func TestJoinPoolFillsEmptyRolesAndHonoursR90(t *testing.T) {
	st := poolState(t)
	_ = st.AddPool("new")
	// A rotation-off account joining an empty pool is its remote, never serving.
	if err := st.JoinPool("d2", "new"); err != nil {
		t.Fatal(err)
	}
	if p := st.PoolOf("new"); p.Serving != "" || p.Remote != "d2" {
		t.Errorf("new = %+v", p)
	}
	if err := st.JoinPool("d3", "new"); err != nil {
		t.Fatal(err)
	}
	if p := st.PoolOf("new"); p.Serving != "d3" || p.Remote != "d2" {
		t.Errorf("new = %+v", p)
	}
	d2, _ := st.Find("d2")
	if !reflect.DeepEqual(d2.InPools(), []string{"default", "new"}) {
		t.Errorf("d2 pools = %v", d2.InPools())
	}
	// It stays in default with its roles there untouched.
	if st.Serving != "d1" || st.Remote != "d1" {
		t.Errorf("default roles changed")
	}
	// Joining a pool already in, a missing pool, a missing account.
	before := st.PoolOf("new")
	if err := st.JoinPool("d3", "new"); err != nil || st.PoolOf("new") != before {
		t.Errorf("rejoin: %v", err)
	}
	if err := st.JoinPool("d1", "nope"); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("missing pool: %v", err)
	}
	if err := st.JoinPool("zzz", "work"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing account: %v", err)
	}
	// Joining default after leaving it: back to ["default"] only when alone.
	if err := st.JoinPool("w1", "default"); err != nil {
		t.Fatal(err)
	}
	if err := st.LeavePool("w1", "work"); err != nil {
		t.Fatal(err)
	}
	if w1, _ := st.Find("w1"); w1.PoolList != nil {
		t.Errorf("exactly [default] must be stored as absent: %v", w1.PoolList)
	}
}

func TestLeavePoolHandsOverRoles(t *testing.T) {
	st := poolState(t)
	_ = st.JoinPool("d1", "work")
	_ = st.SetPoolServing("work", "d1")
	_ = st.SetPoolRemote("work", "d1")
	_ = st.SetPoolPin("work", "d1")
	// work members: w1 w2 w3 plus d1 (registration order: d1 w1 w2 w3).
	if err := st.LeavePool("d1", "work"); err != nil {
		t.Fatal(err)
	}
	// Serving goes to the next rotating member (w1); remote to the next (w1).
	if p := st.PoolOf("work"); p.Serving != "w1" || p.Remote != "w1" || p.Pin != "" {
		t.Errorf("work = %+v", p)
	}
	// Leaving a pool doesn't touch the others' roles.
	if st.Serving != "d1" || st.Remote != "d1" {
		t.Errorf("default roles = %q %q", st.Serving, st.Remote)
	}
	// Remote may go to a rotation-off member; serving skips it (R90).
	_ = st.JoinPool("d1", "work")
	_ = st.SetPoolServing("work", "w3")
	_ = st.SetPoolRemote("work", "w3")
	if err := st.LeavePool("w3", "work"); !errors.Is(err, store.ErrLastPool) {
		t.Fatalf("w3 is only in work: err = %v", err)
	}
	_ = st.JoinPool("w3", "default")
	if err := st.LeavePool("w3", "work"); err != nil {
		t.Fatal(err)
	}
	// After w3 (last) it wraps: d1 is rotating -> serving d1; remote d1.
	if p := st.PoolOf("work"); p.Serving != "d1" || p.Remote != "d1" {
		t.Errorf("work = %+v", p)
	}
	// Serving skips rotation-off members, remote takes them.
	_ = st.SetPoolServing("work", "w1")
	_ = st.SetPoolRemote("work", "w1")
	_ = st.JoinPool("w1", "default")
	_ = st.SetPoolServing("work", "w1")
	_ = st.LeavePool("d1", "work")
	if err := st.LeavePool("w1", "work"); err != nil {
		t.Fatal(err)
	}
	// Remaining: w2 (rotation off) only: serving none, remote w2.
	if p := st.PoolOf("work"); p.Serving != "" || p.Remote != "w2" {
		t.Errorf("work = %+v", p)
	}
}

func TestLeavePoolErrors(t *testing.T) {
	st := poolState(t)
	if err := st.LeavePool("d1", "default"); !errors.Is(err, store.ErrLastPool) {
		t.Errorf("last pool: %v", err)
	}
	if err := st.LeavePool("d1", "work"); !errors.Is(err, store.ErrNotInPool) {
		t.Errorf("not a member: %v", err)
	}
	if err := st.LeavePool("d1", "nope"); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("missing pool: %v", err)
	}
	if err := st.LeavePool("zzz", "work"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing account: %v", err)
	}
	// Leaving the only-other member empties roles.
	_ = st.AddPool("solo")
	_ = st.Add(store.Account{Name: "s", Dir: "/slots/s", PoolList: []string{"solo"}})
	_ = st.JoinPool("s", "default")
	if err := st.LeavePool("s", "solo"); err != nil {
		t.Fatal(err)
	}
	if p := st.PoolOf("solo"); p != (store.Pool{}) {
		t.Errorf("solo = %+v", p)
	}
}

func TestRenameAndRemoveFollowEveryPoolsRoles(t *testing.T) {
	st := poolState(t)
	_ = st.JoinPool("w3", "default")
	_ = st.SetPoolPin("work", "w3")
	_ = st.SetPoolServing("work", "w3")
	_ = st.SetPoolRemote("default", "w3")
	from, roles, err := st.Rename("w3", "w9")
	if err != nil || from != "w3" || !reflect.DeepEqual(roles, []string{"serving", "remote"}) {
		t.Fatalf("rename: %q %v %v", from, roles, err)
	}
	if p := st.PoolOf("work"); p.Serving != "w9" || p.Pin != "w9" || p.Remote != "w1" {
		t.Errorf("work = %+v", p)
	}
	if st.Remote != "w9" {
		t.Errorf("default remote = %q", st.Remote)
	}
	if err := st.Remove("w9"); !errors.Is(err, store.ErrInUse) {
		t.Errorf("remove with roles: %v", err)
	}
	_ = st.SetPoolRemote("default", "d1")
	if err := st.Remove("w9"); !errors.Is(err, store.ErrInUse) {
		t.Errorf("remove serving in work: %v", err)
	}
	_ = st.SetPoolServing("work", "w1")
	if err := st.Remove("w9"); err != nil {
		t.Fatal(err)
	}
	if st.PoolOf("work").Pin != "" {
		t.Error("pin kept after remove")
	}
}

func TestSwapPoolServing(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(st *store.State) error { *st = poolState(t); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := s.SwapPoolServing("work", "w1", "w3")
	if err != nil || st.PoolOf("work").Serving != "w3" || st.Serving != "d1" {
		t.Fatalf("swap: %+v %v", st.PoolOf("work"), err)
	}
	if _, err := s.SwapPoolServing("work", "w1", "w2"); !errors.Is(err, store.ErrServingChanged) {
		t.Errorf("stale: %v", err)
	}
	if _, err := s.SwapPoolServing("work", "w3", "d1"); !errors.Is(err, store.ErrNotInPool) {
		t.Errorf("non-member: %v", err)
	}
	if _, err := s.SwapPoolServing("nope", "a", "b"); !errors.Is(err, store.ErrNoPool) {
		t.Errorf("missing pool: %v", err)
	}
	if st, err = s.SwapPoolServing("default", "d1", "d3"); err != nil || st.Serving != "d3" {
		t.Errorf("default swap: %v %q", err, st.Serving)
	}
}

func TestAddNeverMakesARotationOffAccountServing(t *testing.T) {
	st := store.Default()
	_ = st.AddPool("work")
	if err := st.Add(store.Account{Name: "r", Dir: "/slots/r", NoRotate: true, PoolList: []string{"default", "work"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"default", "work"} {
		if got := st.PoolOf(p); got.Serving != "" || got.Remote != "r" {
			t.Errorf("%s = %+v", p, got)
		}
	}
	if err := st.Add(store.Account{Name: "s", Dir: "/slots/s"}); err != nil {
		t.Fatal(err)
	}
	if st.Serving != "s" {
		t.Errorf("a rotating account takes the empty serving: %q", st.Serving)
	}
}

func TestDefaultFormsOnlyTouchDefaultMembers(t *testing.T) {
	st := poolState(t)
	// Next walks default's members only.
	if got, err := st.Next("d3"); err != nil || got.Name != "d1" {
		t.Errorf("Next(d3) = %q, %v", got.Name, err)
	}
	if got, err := st.Next("w1"); err != nil || got.Name != "d1" {
		t.Errorf("Next(w1) = %q, %v", got.Name, err)
	}
	onlyWork := store.Default()
	_ = onlyWork.AddPool("work")
	_ = onlyWork.Add(store.Account{Name: "w", Dir: "/slots/w", PoolList: []string{"work"}})
	if _, err := onlyWork.Next(""); !errors.Is(err, store.ErrNoAccounts) {
		t.Errorf("empty default: %v", err)
	}
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(x *store.State) error { *x = poolState(t); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SwapServing("d1", "w1"); !errors.Is(err, store.ErrNotInPool) {
		t.Errorf("SwapServing to a non-member: %v", err)
	}
	if _, err := s.SwapServing("d1", "zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SwapServing to unknown: %v", err)
	}
	if st, err := s.SwapServing("d1", "d3"); err != nil || st.Serving != "d3" {
		t.Errorf("SwapServing: %v %q", err, st.Serving)
	}
}

func TestLoadNormalisesAccountPools(t *testing.T) {
	dir := t.TempDir()
	in := `{"version":2,"port":1,"pools":{"work":{},"default":{"serving":"x"}},"accounts":[
 {"name":"dup","dir":"/s/dup","addedAt":"2026-01-01T00:00:00Z","pools":["work","work"]},
 {"name":"gone","dir":"/s/gone","addedAt":"2026-01-01T00:00:00Z","pools":["nope"]},
 {"name":"mix","dir":"/s/mix","addedAt":"2026-01-01T00:00:00Z","pools":["work","nope","default","work"]},
 {"name":"empty","dir":"/s/empty","addedAt":"2026-01-01T00:00:00Z","pools":[]},
 {"name":"explicit","dir":"/s/explicit","addedAt":"2026-01-01T00:00:00Z","pools":["default"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Store{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"dup": {"work"}, "gone": nil, "mix": {"work", "default"}, "empty": nil, "explicit": nil,
	}
	for _, a := range st.Accounts {
		if !reflect.DeepEqual(a.PoolList, want[a.Name]) {
			t.Errorf("%s pools = %#v, want %#v", a.Name, a.PoolList, want[a.Name])
		}
	}
	if _, ok := st.Pools["default"]; ok {
		t.Error("a default entry in pools must be dropped")
	}
	// LeavePool on the deduplicated account is refused as its last pool.
	if err := st.LeavePool("dup", "work"); !errors.Is(err, store.ErrLastPool) {
		t.Errorf("LeavePool(dup): %v", err)
	}
}

func TestMarshalOmitsAnExplicitDefaultOnlyList(t *testing.T) {
	st := store.Default()
	_ = st.Add(store.Account{Name: "a", Dir: "/slots/a"})
	st.Accounts[0].PoolList = []string{"default"}
	b, err := st.MarshalJSON()
	if err != nil || strings.Contains(string(b), `"pools"`) {
		t.Errorf("%s %v", b, err)
	}
}
