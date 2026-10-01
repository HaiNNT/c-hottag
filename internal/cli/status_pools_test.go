package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// statusPoolsHome: work holds A and B (remote A), personal holds B, C and D
// (remote C); B is in both. default holds nothing.
func statusPoolsHome(t *testing.T) (string, store.Store) {
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
		}{{"A", []string{"work"}}, {"B", []string{"work", "personal"}}, {"C", []string{"personal"}}, {"D", []string{"personal"}}} {
			if err := st.Add(store.Account{Name: a.name, Dir: filepath.Join(home, "accounts", a.name), PoolList: a.pools}); err != nil {
				return err
			}
		}
		return st.SetPoolRemote("personal", "C")
	}); err != nil {
		t.Fatal(err)
	}
	var f status.File
	f.SetAuto(status.Auto{Mode: "balanced", Pools: map[string]status.PoolAuto{
		"work":     {Decision: "holding A", LastSwitch: &status.AutoSwitch{From: "B", To: "A", Trigger: "limit", Window: "5h"}},
		"personal": {Decision: "holding B"},
	}})
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	return home, s
}

func TestStatusWithPoolsHasAPoolsColumnAndALinePerPool(t *testing.T) {
	statusPoolsHome(t)
	code, out, errs := runChottag(t, "status")
	if code != 0 || errs != "" {
		t.Fatalf("status = %d %q", code, errs)
	}
	for _, want := range []string{
		"POOLS",
		"personal,work",
		"\npool default · serving none · remote none · serial · 0 live sessions\n",
		"\npool work · serving A · remote A · serial · 0 live sessions · last B→A (limit) (B shared with personal)\n",
		"\npool personal · serving B · remote C · serial · 0 live sessions (B shared with work)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusPoolLinesShowSpreadAndPin(t *testing.T) {
	_, s := statusPoolsHome(t)
	if _, err := s.Update(func(st *store.State) error {
		if err := st.SetPoolPolicy("personal", store.PolicySpread); err != nil {
			return err
		}
		return st.SetPoolPin("personal", "D")
	}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runChottag(t, "status")
	if want := "\npool personal · serving B · remote C · spread · pin D · 0 live sessions (B shared with work)\n"; !strings.Contains(out, want) {
		t.Errorf("status lacks %q:\n%s", want, out)
	}
}

func TestStatusWithPoolsJSON(t *testing.T) {
	statusPoolsHome(t)
	code, out, _ := runChottag(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var doc struct {
		Accounts []struct {
			Name   string   `json:"name"`
			Pools  []string `json:"pools"`
			Shared bool     `json:"shared"`
		} `json:"accounts"`
		Pools []struct {
			Name         string   `json:"name"`
			Serving      string   `json:"serving"`
			Remote       string   `json:"remote"`
			Policy       string   `json:"policy"`
			Accounts     []string `json:"accounts"`
			LiveSessions int      `json:"liveSessions"`
			Decision     string   `json:"decision"`
			LastSwitch   *struct {
				From, To string
			} `json:"lastSwitch"`
		} `json:"pools"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Pools) != 3 {
		t.Fatalf("pools = %+v", doc.Pools)
	}
	byName := map[string]int{}
	for i, p := range doc.Pools {
		byName[p.Name] = i
	}
	w := doc.Pools[byName["work"]]
	if w.Serving != "A" || w.Remote != "A" || w.Policy != "serial" || strings.Join(w.Accounts, ",") != "A,B" || w.LastSwitch == nil || w.LastSwitch.To != "A" {
		t.Errorf("work = %+v", w)
	}
	for _, a := range doc.Accounts {
		if a.Name == "B" && (!a.Shared || strings.Join(a.Pools, ",") != "personal,work") {
			t.Errorf("B = %+v", a)
		}
		if a.Name == "A" && (a.Shared || strings.Join(a.Pools, ",") != "work") {
			t.Errorf("A = %+v", a)
		}
	}
}

// With only default, the document gains no pools, shared or accounts' pools.
func TestStatusDefaultOnlyHasNoPoolFields(t *testing.T) {
	_, _ = autoHome(t)
	_, out, _ := runChottag(t, "status", "--json")
	_, text, _ := runChottag(t, "status")
	if strings.Contains(out, `"pools"`) || strings.Contains(out, `"shared"`) || strings.Contains(text, "POOLS") || strings.Contains(text, "pool ") {
		t.Fatalf("a default-only status mentions pools:\n%s\n%s", text, out)
	}
}

// Live sessions are counted per pool; an unidentified session is default's.
func TestStatusPoolsCountLiveSessionsByPool(t *testing.T) {
	_, s := statusPoolsHome(t)
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := []sessionRow{{PID: 1, SID: "aaaa", Pool: "work"}, {PID: 2, SID: "bbbb", Pool: "work"}, {PID: 3}, {PID: 4, SID: "cccc", Pool: "personal"}}
	got := map[string]int{}
	for _, p := range poolStatuses(st, status.File{}, rows) {
		got[p.Name] = p.LiveSessions
	}
	if got["work"] != 2 || got["default"] != 1 || got["personal"] != 1 {
		t.Fatalf("live sessions = %v", got)
	}
	lines := poolStatusLines(st, status.File{}, rows, time.Now())
	if want := "pool default · serving none · remote none · serial · 1 live session"; lines[0] != want {
		t.Fatalf("default line = %q, want %q", lines[0], want)
	}
}

// A session of a pool that no longer exists is counted under default, as
// routing treats it.
func TestStatusPoolsCountARemovedPoolsSessionUnderDefault(t *testing.T) {
	_, s := statusPoolsHome(t)
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := []sessionRow{{PID: 1, SID: "aaaa", Pool: "gone"}, {PID: 2, SID: "bbbb", Pool: "work"}}
	got := map[string]int{}
	for _, p := range poolStatuses(st, status.File{}, rows) {
		got[p.Name] = p.LiveSessions
	}
	if got["default"] != 1 || got["work"] != 1 {
		t.Fatalf("live sessions = %v", got)
	}
}
