package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// writeAutoStatus writes a status.json whose daemon heartbeat is at beat,
// carrying this auto object.
func writeAutoStatus(t *testing.T, home string, beat time.Time, a status.Auto) {
	t.Helper()
	f := status.File{Version: status.Version, Daemon: &status.Daemon{Heartbeat: beat}}
	f.SetAuto(a)
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

// lastLine is out's last non-empty line.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestStatusShowsThePlanColumn: the PLAN column shows each account's
// tier, "max?" for a Max account whose size is unknown, and "-" when
// unset (M4 spec §7).
func TestStatusShowsThePlanColumn(t *testing.T) {
	_, s := autoHome(t) // A max20x, B unset
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "C", Plan: "max", Dir: st.Accounts[0].Dir + "-c"})
	}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runChottag(t, "status")
	if code != exit.OK {
		t.Fatalf("status = %d", code)
	}
	fields := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			fields[f[0]] = f
		}
	}
	for name, want := range map[string]string{"NAME": "PLAN", "A": "max20x", "B": "-", "C": "max?"} {
		if f := fields[name]; len(f) < 2 || f[1] != want {
			t.Errorf("row %s = %q, want PLAN %q:\n%s", name, f, want, out)
		}
	}
}

// TestStatusAutoLine pins spec §7's one line: mode, the running daemon's
// decision, the last switch; the decision drops when the heartbeat is
// stale, and auto off is just "auto: off".
func TestStatusAutoLine(t *testing.T) {
	home, s := autoHome(t)
	now := time.Now()
	at := now.Add(-time.Hour)
	a := status.Auto{Mode: "balanced", Decision: "holding A (5h 96%, resets in 9m)",
		LastSwitch: &status.AutoSwitch{From: "C", To: "A", Trigger: "limit", Window: "5h", At: at}}

	writeAutoStatus(t, home, now, a)
	_, out, _ := runChottag(t, "status")
	want := "auto: balanced · holding A (5h 96%, resets in 9m) · last C→A " + at.Local().Format("15:04") + " (limit)"
	if got := lastLine(out); got != want {
		t.Fatalf("auto line = %q, want %q", got, want)
	}

	// item 7 (F185, review round 2): margin past status.DaemonStaleAfter
	// itself, not an unrelated hardcoded duration that happens to exceed
	// today's value — so a future change to the constant cannot silently
	// stop this from actually testing "stale".
	writeAutoStatus(t, home, now.Add(-status.DaemonStaleAfter-time.Minute), a)
	_, out, _ = runChottag(t, "status")
	if got, want := lastLine(out), "auto: balanced · last C→A "+at.Local().Format("15:04")+" (limit)"; got != want {
		t.Fatalf("stale daemon: auto line = %q, want %q", got, want)
	}

	if _, err := s.Update(func(st *store.State) error { st.SetAutoMode("cache-optimize"); return nil }); err != nil {
		t.Fatal(err)
	}
	writeAutoStatus(t, home, now, status.Auto{Mode: "cache-optimize"})
	if _, out, _ = runChottag(t, "status"); lastLine(out) != "auto: cache-optimize" {
		t.Fatalf("no decision yet: %q", lastLine(out))
	}

	if _, err := s.Update(func(st *store.State) error { st.SetAutoEnabled(false); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, out, _ = runChottag(t, "status"); lastLine(out) != "auto: off" {
		t.Fatalf("auto off: %q", lastLine(out))
	}
}

// TestStatusJSONCarriesPlanAndAuto: --json overlays each account's stored
// plan and passes the daemon's auto object through (additive, §5.1).
func TestStatusJSONCarriesPlanAndAuto(t *testing.T) {
	home, _ := autoHome(t)
	now := time.Now()
	writeAutoStatus(t, home, now, status.Auto{Mode: "balanced", Decision: "staying on A (5h 10% < 98%)", BurnRate: 42})
	code, out, _ := runChottag(t, "status", "--json")
	if code != exit.OK {
		t.Fatalf("status --json = %d", code)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
		Auto     map[string]any   `json:"auto"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Accounts[0]["plan"] != "max20x" {
		t.Errorf("A = %v, want plan max20x", doc.Accounts[0])
	}
	if _, ok := doc.Accounts[1]["plan"]; ok {
		t.Errorf("B = %v, want no plan key", doc.Accounts[1])
	}
	if doc.Auto["mode"] != "balanced" || doc.Auto["decision"] != "staying on A (5h 10% < 98%)" || doc.Auto["burnRate"] != float64(42) {
		t.Errorf("auto = %v", doc.Auto)
	}
}
