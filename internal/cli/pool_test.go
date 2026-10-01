package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// poolsHome seeds A and B in default, C in work and D in personal.
func poolsHome(t *testing.T) (string, store.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, p := range []string{"work", "personal"} {
			if err := st.AddPool(p); err != nil {
				return err
			}
		}
		for _, a := range []struct {
			name  string
			pools []string
		}{{"A", nil}, {"B", nil}, {"C", []string{"work"}}, {"D", []string{"personal"}}} {
			if err := st.Add(store.Account{Name: a.name, Dir: filepath.Join(home, "accounts", a.name), PoolList: a.pools}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return home, s
}

func jsonOf(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	code, out, _ := runChottag(t, append([]string{"--json"}, args...)...)
	return code, decodeOneDocument(t, out)
}

func wantErrCode(t *testing.T, doc map[string]any, want string) {
	t.Helper()
	if got := docError(t, doc)["code"]; got != want {
		t.Fatalf("error code = %v, want %s (%v)", got, want, doc)
	}
}

func TestPoolListText(t *testing.T) {
	poolsHome(t)
	code, _, errs := runChottag(t, "pool")
	if code != exit.OK || errs != "" {
		t.Fatalf("pool = %d %q", code, errs)
	}
	runChottag(t, "pool", "add", "empty")
	_, out, _ := runChottag(t, "pool")
	if !strings.Contains(out, "empty: serving none, remote none, serial; accounts: none\n") {
		t.Errorf("empty pool line missing:\n%s", out)
	}
	for _, want := range []string{"default", "work", "personal", "serving A", "serving C", "remote C", "A, B"} {
		if !strings.Contains(out, want) {
			t.Errorf("pool output lacks %q:\n%s", want, out)
		}
	}
}

func TestPoolListJSON(t *testing.T) {
	poolsHome(t)
	code, doc := jsonOf(t, "pool")
	if code != exit.OK {
		t.Fatalf("exit %d %v", code, doc)
	}
	pools := doc["pools"].([]any)
	if len(pools) != 3 {
		t.Fatalf("pools = %v", pools)
	}
	d := pools[0].(map[string]any)
	if d["name"] != "default" || d["serving"] != "A" || d["remote"] != "A" || d["policy"] != "serial" || d["pin"] != "" {
		t.Fatalf("default = %v", d)
	}
	if acc := d["accounts"].([]any); len(acc) != 2 || acc[0] != "A" || acc[1] != "B" {
		t.Fatalf("default accounts = %v", acc)
	}
	if pools[1].(map[string]any)["name"] != "personal" || pools[2].(map[string]any)["name"] != "work" {
		t.Fatalf("order = %v", pools)
	}
}

func TestPoolListWithOnlyDefaultJSON(t *testing.T) {
	policyHome(t)
	code, doc := jsonOf(t, "pool")
	if code != exit.OK {
		t.Fatal(code, doc)
	}
	if p := doc["pools"].([]any); len(p) != 1 || p[0].(map[string]any)["name"] != "default" {
		t.Fatalf("pools = %v", p)
	}
}

func TestPoolAdd(t *testing.T) {
	policyHome(t)
	s := store.Store{Dir: mustHome(t)}
	code, out, _ := runChottag(t, "pool", "add", "work")
	if code != exit.OK || out != "added pool work\n" {
		t.Fatalf("add = %d %q", code, out)
	}
	if st, _ := s.Load(); !st.HasPool("work") {
		t.Fatal("pool not stored")
	}
	code, doc := jsonOf(t, "pool", "add", "work")
	if code != exit.Usage {
		t.Fatalf("duplicate exit = %d", code)
	}
	wantErrCode(t, doc, "pool_exists")
	code, doc = jsonOf(t, "pool", "add", "Bad Name")
	if code != exit.Usage {
		t.Fatalf("bad name exit = %d", code)
	}
	wantErrCode(t, doc, "bad_pool")
	code, doc = jsonOf(t, "pool", "add", "default")
	wantErrCode(t, doc, "pool_exists")
	if code != exit.Usage {
		t.Fatal(code)
	}
	if code, _, _ := runChottag(t, "pool", "add"); code != exit.Usage {
		t.Fatalf("no name = %d", code)
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	h, err := home()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPoolJoinSerialWarnsSharedAccount(t *testing.T) {
	_, s := poolsHome(t)
	code, out, errs := runChottag(t, "pool", "join", "A", "work")
	if code != exit.OK || out != "A joined work\n" {
		t.Fatalf("join = %d %q %q", code, out, errs)
	}
	want := "chottag: warning: A is now in default and work and shares one usage limit between them: if A serves both, they use up its 5-hour window together and switch away from it at the same time.\n"
	if errs != want {
		t.Fatalf("stderr = %q\nwant     %q", errs, want)
	}
	st, _ := s.Load()
	a, _ := st.Find("A")
	if !a.InPool("default") || !a.InPool("work") {
		t.Fatalf("pools = %v", a.InPools())
	}
}

func TestPoolJoinSpreadWordingWhenAnyPoolSpreads(t *testing.T) {
	_, s := poolsHome(t)
	if _, err := s.Update(func(st *store.State) error { return st.SetPoolPolicy("work", store.PolicySpread) }); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runChottag(t, "pool", "join", "A", "work")
	if code != exit.OK {
		t.Fatal(code, errs)
	}
	want := "chottag: warning: A is now in default and work and shares one usage limit between them: under spread, their sessions compete for it, and heavy use in one pool moves the other's sessions off A (their prompt caches go cold).\n"
	if errs != want {
		t.Fatalf("stderr = %q\nwant     %q", errs, want)
	}
}

func TestPoolJoinThreePoolsListsThemWithAnd(t *testing.T) {
	poolsHome(t)
	runChottag(t, "pool", "join", "A", "work")
	_, _, errs := runChottag(t, "pool", "join", "A", "personal")
	if !strings.Contains(errs, "A is now in default, personal and work and shares") {
		t.Fatalf("stderr = %q", errs)
	}
}

func TestPoolJoinJSONCarriesWarningAndResult(t *testing.T) {
	poolsHome(t)
	code, doc := jsonOf(t, "pool", "join", "B", "work")
	if code != exit.OK {
		t.Fatal(code, doc)
	}
	if doc["account"] != "B" || doc["pool"] != "work" {
		t.Fatalf("doc = %v", doc)
	}
	w := doc["warnings"].([]any)
	if len(w) != 1 || w[0].(map[string]any)["code"] != "shared_account" {
		t.Fatalf("warnings = %v", w)
	}
	if ps := doc["pools"].([]any); len(ps) != 2 {
		t.Fatalf("pools = %v", ps)
	}
}

func TestPoolJoinAnEmptyPoolIsNotShared(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "add", "extra")
	// D is only in personal; leave it there, join it to extra: shared. A
	// join into a pool from default only (nobody shares) is covered by
	// leaving first.
	runChottag(t, "pool", "join", "D", "extra")
	runChottag(t, "pool", "leave", "D", "personal")
	code, _, errs := runChottag(t, "pool", "join", "D", "extra")
	if code != exit.OK || errs != "" {
		t.Fatalf("rejoin = %d %q, want no warning for a one-pool account", code, errs)
	}
	st, _ := s.Load()
	if st.PoolOf("extra").Serving != "D" {
		t.Fatalf("extra = %+v", st.PoolOf("extra"))
	}
}

func TestPoolJoinErrors(t *testing.T) {
	poolsHome(t)
	code, doc := jsonOf(t, "pool", "join", "A", "nope")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "no_pool")
	code, doc = jsonOf(t, "pool", "join", "Z", "work")
	if code != exit.Error {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "unknown_account")
	if code, _, _ := runChottag(t, "pool", "join", "A"); code != exit.Usage {
		t.Fatalf("missing pool = %d", code)
	}
}

func TestPoolJoinAlreadyAMemberSaysSo(t *testing.T) {
	poolsHome(t)
	code, out, _ := runChottag(t, "pool", "join", "C", "work")
	if code != exit.OK || out != "C is already in work\n" {
		t.Fatalf("join = %d %q", code, out)
	}
}

func TestPoolLeave(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "A", "work")
	code, out, errs := runChottag(t, "pool", "leave", "A", "default")
	if code != exit.OK || out != "A left default\n" {
		t.Fatalf("leave = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if st.Serving != "B" {
		t.Fatalf("default serving = %q, want B", st.Serving)
	}
	code, doc := jsonOf(t, "pool", "leave", "A", "default")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "not_in_pool")
	code, doc = jsonOf(t, "pool", "leave", "C", "work")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "last_pool")
	code, doc = jsonOf(t, "pool", "leave", "A", "nope")
	wantErrCode(t, doc, "no_pool")
	if code != exit.Usage {
		t.Fatal(code)
	}
}

func TestPoolRm(t *testing.T) {
	home, s := poolsHome(t)
	code, doc := jsonOf(t, "pool", "rm", "work")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "pool_not_empty")
	code, doc = jsonOf(t, "pool", "rm", "default")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "pool_default")
	code, doc = jsonOf(t, "pool", "rm", "nope")
	wantErrCode(t, doc, "no_pool")
	if code != exit.Usage {
		t.Fatal(code)
	}
	runChottag(t, "pool", "join", "C", "default")
	runChottag(t, "pool", "leave", "C", "work")
	runChottag(t, "pool", "join", "D", "default")
	runChottag(t, "pool", "leave", "D", "personal")
	if code, out, errs := runChottag(t, "pool", "rm", "work"); code != exit.OK || out != "removed pool work\n" {
		t.Fatalf("rm = %d %q %q", code, out, errs)
	}
	if code, _, errs := runChottag(t, "pool", "rm", "personal"); code != exit.OK {
		t.Fatalf("rm personal = %d %q", code, errs)
	}
	st, _ := s.Load()
	if len(st.Pools) != 0 {
		t.Fatalf("pools = %v", st.Pools)
	}
	var raw struct {
		Version int `json:"version"`
	}
	b, rerr := os.ReadFile(filepath.Join(home, "state.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err := json.Unmarshal(b, &raw); err != nil || raw.Version != 1 {
		t.Fatalf("version = %d (%v), want 1 once the last extra pool is gone", raw.Version, err)
	}
}

func TestPoolUsageAndHelp(t *testing.T) {
	policyHome(t)
	if code, _, _ := runChottag(t, "pool", "bogus"); code != exit.Usage {
		t.Fatalf("bogus verb = %d", code)
	}
	code, out, _ := runChottag(t, "pool", "--help")
	if code != exit.OK || !strings.Contains(out, "pool join") {
		t.Fatalf("help = %d %q", code, out)
	}
}

// --- pool-scoped commands ------------------------------------------------

func TestTagNameActsInTheAccountsOnePool(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "B", "work") // work: C, B
	code, out, errs := runChottag(t, "tag", "D")
	if code != exit.OK || out != "serving: D (pool personal)\n" {
		t.Fatalf("tag D = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if st.PoolOf("personal").Serving != "D" || st.Serving != "A" {
		t.Fatalf("state = %+v / %+v", st.PoolOf("personal"), st.Serving)
	}
}

func TestTagNameInSeveralPoolsNeedsPool(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "B", "work")
	code, doc := jsonOf(t, "tag", "B")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "pool_ambiguous")
	code, out, errs := runChottag(t, "tag", "B", "--pool", "work")
	if code != exit.OK || !strings.HasPrefix(out, "serving: B") {
		t.Fatalf("tag B --pool work = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if st.PoolOf("work").Serving != "B" || st.Serving != "A" {
		t.Fatalf("serving: work %q default %q", st.PoolOf("work").Serving, st.Serving)
	}
	code, doc = jsonOf(t, "tag", "A", "--pool", "work")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "not_in_pool")
	code, doc = jsonOf(t, "tag", "A", "--pool", "nope")
	wantErrCode(t, doc, "no_pool")
	if code != exit.Usage {
		t.Fatal(code)
	}
}

func TestTagJSONNamesThePoolOnlyOutsideDefault(t *testing.T) {
	poolsHome(t)
	_, doc := jsonOf(t, "tag", "B")
	if _, has := doc["pool"]; has {
		t.Fatalf("default tag carries pool: %v", doc)
	}
	_, doc = jsonOf(t, "tag", "D")
	if doc["pool"] != "personal" || doc["serving"] != "D" {
		t.Fatalf("doc = %v", doc)
	}
}

func TestTagPinsInThePoolUnderSpread(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "B", "work")
	runChottag(t, "policy", "spread", "--pool", "work")
	code, out, errs := runChottag(t, "tag", "B", "--pool", "work")
	if code != exit.OK {
		t.Fatal(code, errs)
	}
	if !strings.Contains(out, "new sessions are pinned to B") {
		t.Fatalf("out = %q", out)
	}
	st, _ := s.Load()
	if st.PoolOf("work").Pin != "B" || st.Pin != "" {
		t.Fatalf("pins: work %q default %q", st.PoolOf("work").Pin, st.Pin)
	}
	if code, _, _ := runChottag(t, "tag", "--unpin", "--pool", "work"); code != exit.OK {
		t.Fatal("unpin")
	}
	if st, _ := s.Load(); st.PoolOf("work").Pin != "" {
		t.Fatalf("pin = %q", st.PoolOf("work").Pin)
	}
	// default is still serial: --unpin there refuses as before.
	if code, _, _ := runChottag(t, "tag", "--unpin"); code != exit.Usage {
		t.Fatalf("default unpin under serial = %d", code)
	}
}

func TestNextStaysInThePool(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "D", "work") // work: C, D
	code, out, errs := runChottag(t, "next", "--pool", "work")
	if code != exit.OK || out != "serving: D (pool work)\n" {
		t.Fatalf("next --pool work = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if st.PoolOf("work").Serving != "D" || st.Serving != "A" {
		t.Fatalf("serving: work %q default %q", st.PoolOf("work").Serving, st.Serving)
	}
	code, out, _ = runChottag(t, "next")
	if code != exit.OK || out != "serving: B\n" {
		t.Fatalf("default next = %d %q", code, out)
	}
	code, doc := jsonOf(t, "next", "--pool", "nope")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "no_pool")
}

func TestNextRefusedUnderSpreadOnlyInThatPool(t *testing.T) {
	poolsHome(t)
	runChottag(t, "pool", "join", "D", "work")
	runChottag(t, "policy", "spread", "--pool", "work")
	code, doc := jsonOf(t, "next", "--pool", "work")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "spread_next")
	if code, _, _ := runChottag(t, "next"); code != exit.OK {
		t.Fatalf("default next = %d", code)
	}
}

func TestRemoteInPools(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "D", "work")
	code, out, _ := runChottag(t, "remote", "--pool", "work")
	if code != exit.OK || out != "remote: C (pool work)\n" {
		t.Fatalf("remote --pool work = %d %q", code, out)
	}
	code, out, _ = runChottag(t, "remote")
	if code != exit.OK || out != "remote: A\n" {
		t.Fatalf("remote = %d %q", code, out)
	}
	// D is in personal and work: it needs --pool.
	code, doc := jsonOf(t, "remote", "D")
	if code != exit.Usage {
		t.Fatal(code)
	}
	wantErrCode(t, doc, "pool_ambiguous")
	code, doc = jsonOf(t, "remote", "D", "--pool", "work")
	if code != exit.OK || doc["remote"] != "D" || doc["pool"] != "work" || doc["changed"] != true {
		t.Fatalf("remote D --pool work = %d %v", code, doc)
	}
	st, _ := s.Load()
	if st.PoolOf("work").Remote != "D" || st.Remote != "A" {
		t.Fatalf("remotes: work %q default %q", st.PoolOf("work").Remote, st.Remote)
	}
	code, out, _ = runChottag(t, "remote", "C")
	if code != exit.OK || out != "remote: C (pool work)\n" {
		t.Fatalf("remote C (one pool) = %d %q", code, out)
	}
	code, doc = jsonOf(t, "remote", "A", "--pool", "work")
	wantErrCode(t, doc, "not_in_pool")
	if code != exit.Usage {
		t.Fatal(code)
	}
	code, doc = jsonOf(t, "remote", "--pool", "nope")
	wantErrCode(t, doc, "no_pool")
	if code != exit.Usage {
		t.Fatal(code)
	}
	if code, _, _ := runChottag(t, "remote", "--bogus"); code != exit.Usage {
		t.Fatal("stray flag")
	}
}

func TestPolicyPerPool(t *testing.T) {
	_, s := poolsHome(t)
	code, out, errs := runChottag(t, "policy", "spread", "--pool", "work")
	if code != exit.OK || out != "policy: spread (pool work)\n" {
		t.Fatalf("policy = %d %q %q", code, out, errs)
	}
	st, _ := s.Load()
	if st.PoolOf("work").Policy != store.PolicySpread || st.Policy != "" {
		t.Fatalf("policies: work %q default %q", st.PoolOf("work").Policy, st.Policy)
	}
	if code, out, _ := runChottag(t, "policy"); code != exit.OK || out != "policy: serial\n" {
		t.Fatalf("default policy = %d %q", code, out)
	}
	code, doc := jsonOf(t, "policy", "--pool", "work")
	if code != exit.OK || doc["policy"] != "spread" || doc["pool"] != "work" {
		t.Fatalf("doc = %v", doc)
	}
	_, doc = jsonOf(t, "policy")
	if _, has := doc["pool"]; has {
		t.Fatalf("default policy carries pool: %v", doc)
	}
	code, doc = jsonOf(t, "policy", "--pool", "nope")
	wantErrCode(t, doc, "no_pool")
	if code != exit.Usage {
		t.Fatal(code)
	}
}

func TestPolicySpreadRepeatsTheSharedWarning(t *testing.T) {
	poolsHome(t)
	runChottag(t, "pool", "join", "A", "work")
	runChottag(t, "pool", "join", "B", "work")
	code, _, errs := runChottag(t, "policy", "spread", "--pool", "work")
	if code != exit.OK {
		t.Fatal(code, errs)
	}
	for _, n := range []string{"A", "B"} {
		want := "chottag: warning: " + n + " is now in default and work and shares one usage limit between them: under spread,"
		if !strings.Contains(errs, want) {
			t.Errorf("stderr lacks the spread warning for %s:\n%s", n, errs)
		}
	}
	if strings.Contains(errs, "C is now") {
		t.Errorf("C is in one pool only:\n%s", errs)
	}
	code, doc := jsonOf(t, "policy", "spread", "--pool", "work")
	if code != exit.OK {
		t.Fatal(code)
	}
	n := 0
	for _, w := range doc["warnings"].([]any) {
		if w.(map[string]any)["code"] == "shared_account" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("shared_account warnings = %d, want 2", n)
	}
	// Reading the policy does not warn.
	if _, _, errs := runChottag(t, "policy", "--pool", "work"); errs != "" {
		t.Fatalf("show warned: %q", errs)
	}
}

func TestRotateRenameApplyInEveryPool(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "A", "work")
	if code, _, errs := runChottag(t, "rotate", "A", "off"); code != exit.OK {
		t.Fatal(code, errs)
	}
	st, _ := s.Load()
	if a, _ := st.Find("A"); a.Rotates() {
		t.Fatal("rotate off did not stick")
	}
	runChottag(t, "tag", "A", "--pool", "work")
	runChottag(t, "rename", "A", "Alpha")
	st, _ = s.Load()
	if st.Serving != "Alpha" || st.PoolOf("work").Serving != "Alpha" {
		t.Fatalf("rename left serving: default %q work %q", st.Serving, st.PoolOf("work").Serving)
	}
}

func TestLogoutRefusesWhileAnyPoolServesItAndForceMovesEveryRole(t *testing.T) {
	home, s := poolsHome(t)
	if err := os.MkdirAll(filepath.Join(home, "accounts", "D"), 0o700); err != nil {
		t.Fatal(err)
	}
	stubCredsDelete(t, nil)
	runChottag(t, "pool", "join", "D", "work") // work: C (serving), D
	runChottag(t, "tag", "D", "--pool", "work")
	code, doc := jsonOf(t, "logout", "D", "--yes")
	if code != exit.UserAction {
		t.Fatalf("logout D = %d %v, want role_held", code, doc)
	}
	wantErrCode(t, doc, "role_held")

	stubAuthExec(t, func(_, _, _ string) error { return nil })
	var out, errb bytes.Buffer
	if got := runLogout([]string{"--force", "--yes", "D"}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK {
		t.Fatalf("logout --force = %d %q", got, errb.String())
	}
	st, _ := s.Load()
	if _, err := st.Find("D"); err == nil {
		t.Fatal("D still registered")
	}
	if st.PoolOf("work").Serving != "C" {
		t.Fatalf("work serving = %q, want C", st.PoolOf("work").Serving)
	}
	if st.PoolOf("personal").Serving != "" || st.PoolOf("personal").Remote != "" {
		t.Fatalf("personal = %+v, want empty roles", st.PoolOf("personal"))
	}
	if !strings.Contains(out.String(), "moved serving to C in work\n") {
		t.Fatalf("out = %q, want the moved roles named with their pool", out.String())
	}
}

func TestUpdateVersionBelow08RefusedWhileExtraPoolsExist(t *testing.T) {
	poolsHome(t)
	code, doc := jsonOf(t, "update", "--version", "v0.7.0")
	if code != exit.Usage {
		t.Fatalf("exit = %d %v", code, doc)
	}
	wantErrCode(t, doc, "pools_block_rollback")
	if msg, _ := docError(t, doc)["message"].(string); !strings.Contains(msg, "chottag pool rm") {
		t.Fatalf("message = %q", msg)
	}
}

func TestPoolsNeed08(t *testing.T) {
	for v, want := range map[string]bool{"0.7.0": true, "0.7.9": true, "0.8.0": false, "0.8.1": false, "0.10.0": false, "1.0.0": false} {
		if got := rollbackLosesPools(v); got != want {
			t.Errorf("rollbackLosesPools(%q) = %v, want %v", v, got, want)
		}
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "pool lists", command: "pool",
			setup: func(t *testing.T) []string { poolsHome(t); return []string{"pool"} },
		},
		jsonCase{
			name: "pool add twice", command: "pool",
			setup: func(t *testing.T) []string {
				policyHome(t)
				runChottag(t, "pool", "add", "work")
				return []string{"pool", "add", "work"}
			},
			wantExit: exit.Usage, wantCode: codePoolExists,
		},
	)
}

func TestLoginPoolPutsANewAccountInThatPoolOnly(t *testing.T) {
	_, s := poolsHome(t)
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"new@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })
	var out, errb bytes.Buffer
	if got := runLogin([]string{"E", "--pool", "work", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK {
		t.Fatalf("login = %d %q", got, errb.String())
	}
	st, _ := s.Load()
	e, err := st.Find("E")
	if err != nil {
		t.Fatal(err)
	}
	if ps := e.InPools(); len(ps) != 1 || ps[0] != "work" {
		t.Fatalf("E pools = %v, want [work]", ps)
	}
	// Without --pool a new account is in default only.
	if got := runLogin([]string{"F", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK {
		t.Fatalf("login F = %d %q", got, errb.String())
	}
	st, _ = s.Load()
	f, _ := st.Find("F")
	if ps := f.InPools(); len(ps) != 1 || ps[0] != "default" {
		t.Fatalf("F pools = %v, want [default]", ps)
	}
}

func TestLoginExistingAccountKeepsItsPools(t *testing.T) {
	_, s := poolsHome(t)
	runChottag(t, "pool", "join", "C", "personal")
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"c@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })
	var out, errb bytes.Buffer
	if got := runLogin([]string{"C", "--pool", "default", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK {
		t.Fatalf("login = %d %q", got, errb.String())
	}
	want := "chottag: C is already registered; --pool applies only to a new account. To add it to default, run: chottag pool join C default\n"
	if errb.String() != want {
		t.Fatalf("stderr = %q, want %q", errb.String(), want)
	}
	st, _ := s.Load()
	c, _ := st.Find("C")
	if !c.InPool("work") || !c.InPool("personal") || c.InPool("default") {
		t.Fatalf("C pools = %v, want work and personal kept", c.InPools())
	}
}

func TestLoginUnknownPoolIsRefusedBeforeTheBrowser(t *testing.T) {
	poolsHome(t)
	stubAuthExec(t, func(_, _, _ string) error { t.Fatal("login must not start"); return nil })
	var out, errb bytes.Buffer
	if got := runLogin([]string{"E", "--pool", "nope"}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.Usage {
		t.Fatalf("login = %d %q", got, errb.String())
	}
}

func TestLoginEmailRegisteredHintNamesPoolJoin(t *testing.T) {
	_, s := poolsHome(t)
	if _, err := s.Update(func(st *store.State) error {
		a, _ := st.Find("A")
		a.Email = "shared@example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"shared@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })
	var out, errb bytes.Buffer
	if got := runLogin([]string{"E", "--pool", "work", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK {
		t.Fatalf("login = %d %q", got, errb.String())
	}
	want := "chottag: warning: shared@example.com is already account A; to use it in another pool, run: chottag pool join A work\n"
	if errb.String() != want {
		t.Fatalf("stderr = %q, want %q", errb.String(), want)
	}
}

func TestLoginExistingAccountAlreadyInThePoolDoesNotWarn(t *testing.T) {
	poolsHome(t)
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"c@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })
	var out, errb bytes.Buffer
	if got := runLogin([]string{"C", "--pool", "work", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK || errb.String() != "" {
		t.Fatalf("login = %d %q", got, errb.String())
	}
}

func TestLoginExistingAccountPoolWarningIsInJSON(t *testing.T) {
	poolsHome(t)
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"c@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })
	code, doc := jsonOf(t, "login", "C", "--pool", "personal", "--claude", fake)
	if code != exit.OK {
		t.Fatal(code, doc)
	}
	w := doc["warnings"].([]any)
	if len(w) != 1 || w[0].(map[string]any)["code"] != "pool_not_changed" {
		t.Fatalf("warnings = %v", w)
	}
}

func TestPoolJoinRepeatDoesNotWarnAgain(t *testing.T) {
	poolsHome(t)
	runChottag(t, "pool", "join", "A", "work")
	_, _, errs := runChottag(t, "pool", "join", "A", "work")
	if errs != "" {
		t.Fatalf("repeat join warned: %q", errs)
	}
}

func TestLoginEmailHintPicksAPoolTheAccountIsNotIn(t *testing.T) {
	_, s := poolsHome(t)
	if _, err := s.Update(func(st *store.State) error {
		a, _ := st.Find("C") // in work
		a.Email = "shared@example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"shared@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })
	var out, errb bytes.Buffer
	// --pool work: C is already there, so the hint names another pool.
	if got := runLogin([]string{"E", "--pool", "work", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errb)); got != exit.OK {
		t.Fatalf("login = %d %q", got, errb.String())
	}
	want := "chottag: warning: shared@example.com is already account C; to use it in another pool, run: chottag pool join C personal\n"
	if errb.String() != want {
		t.Fatalf("stderr = %q, want %q", errb.String(), want)
	}
}

func TestLoginPoolRemovedDuringTheBrowserIsNoPool(t *testing.T) {
	policyHome(t)
	runChottag(t, "pool", "add", "work")
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"new@example.com","orgName":"Acme"}`)
	stubAuthExec(t, func(_, _, _ string) error {
		if code, _, errs := runChottag(t, "pool", "rm", "work"); code != exit.OK {
			t.Fatalf("pool rm = %d %q", code, errs)
		}
		return nil
	})
	var out, errb bytes.Buffer
	r := newReporter(true, &out, &errb)
	if got := runLogin([]string{"E", "--pool", "work", "--claude", fake}, strings.NewReader(""), r); got != exit.Usage {
		t.Fatalf("login = %d %q", got, errb.String())
	}
	wantErrCode(t, decodeOneDocument(t, out.String()), "no_pool")
}

func TestRotateOffWarnsWhenAPoolLosesItsLastRotatingAccount(t *testing.T) {
	poolsHome(t)
	code, _, errs := runChottag(t, "rotate", "C", "off")
	if code != exit.OK || !strings.Contains(errs, "no account left in rotation in pool work") {
		t.Fatalf("rotate = %d %q", code, errs)
	}
	_, _, errs = runChottag(t, "rotate", "A", "off")
	if strings.Contains(errs, "in pool") {
		t.Fatalf("default pool still has B: %q", errs)
	}
}

// A daemon older than 0.8.0 cannot read a version-2 state.json, so `pool add`
// refuses to write one under it (daemon_predates_pools, exit 2) and `pool join`
// (the pool already exists) only warns. An empty or unknown version counts as
// older; no daemon, or one that reads pools, is fine.
func TestPoolAddRefusesAndJoinWarnsWhenTheRunningDaemonPredatesPools(t *testing.T) {
	_, s := poolsHome(t)
	origVersion := Version
	Version = "0.8.0"
	t.Cleanup(func() { Version = origVersion })
	const code = "daemon_predates_pools"
	cases := []struct {
		name string
		up   bool
		ver  string
		old  bool
	}{
		{"older daemon", true, "0.7.0", true},
		{"0.6 daemon", true, "0.6.0", true},
		{"unparseable daemon version", true, "weird", true},
		{"empty daemon version", true, "", true},
		{"same version", true, "0.8.0", false},
		{"newer daemon", true, "0.9.0", false},
		{"no daemon", false, "", false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := SetStatusProbeForTest(func(int) (bool, string) { return c.up, c.ver })
			defer restore()
			name := fmt.Sprintf("new%d", i)
			exitCode, doc := jsonOf(t, "pool", "add", name)
			st, _ := s.Load()
			if c.old {
				if exitCode != exit.Usage {
					t.Fatalf("pool add exit %d: %v", exitCode, doc)
				}
				wantErrCode(t, doc, code)
				if msg, _ := docError(t, doc)["message"].(string); !strings.Contains(msg, "chottag daemon restart") {
					t.Fatalf("message lacks the restart advice: %q", msg)
				}
				if st.HasPool(name) {
					t.Fatal("the pool was written under an older daemon")
				}
			} else if exitCode != exit.OK || !st.HasPool(name) {
				t.Fatalf("pool add exit %d: %v", exitCode, doc)
			}
			_, out, errs := runChottag(t, "--json", "pool", "join", "A", "work")
			if got := strings.Contains(out, code); got != c.old {
				t.Fatalf("join warning present = %v, want %v: %s", got, c.old, out)
			}
			if c.old && !strings.Contains(errs, "chottag daemon restart") {
				t.Fatalf("stderr lacks the restart advice: %q", errs)
			}
			runChottag(t, "pool", "leave", "A", "work")
		})
	}
}
