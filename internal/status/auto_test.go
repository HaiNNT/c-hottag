package status

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoIsAbsentUntilSet(t *testing.T) {
	b, err := Marshal(File{Accounts: []Account{{Name: "A"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"auto"`) || strings.Contains(string(b), `"plan"`) {
		t.Fatalf("an empty document carries auto or plan:\n%s", b)
	}
}

func TestSetAutoReportsOnlyAChange(t *testing.T) {
	var f File
	at := time.Date(2026, 9, 26, 9, 12, 0, 0, time.UTC)
	a := Auto{Mode: "balanced", Decision: "staying on A (5h 40% < 93%)", BurnRate: 12.34,
		LastSwitch: &AutoSwitch{From: "C", To: "A", Trigger: "limit", Window: "5h", At: at}}
	if !f.SetAuto(a) {
		t.Fatal("the first SetAuto reported no change")
	}
	if f.Auto.BurnRate != 12.3 {
		t.Fatalf("BurnRate = %v, want 12.3 (one decimal)", f.Auto.BurnRate)
	}
	a.BurnRate = 12.31 // same to one decimal
	if f.SetAuto(a) {
		t.Fatal("a burn change in the second decimal reported a change")
	}
	a.LastSwitch.Retried = true // the caller's struct: SetAuto kept a copy
	if f.Auto.LastSwitch.Retried {
		t.Fatal("SetAuto stored the caller's LastSwitch pointer")
	}
	if !f.SetAuto(a) {
		t.Fatal("a changed lastSwitch reported no change")
	}
	a.Decision = "holding A (5h 96%, resets in 9m)"
	if !f.SetAuto(a) {
		t.Fatal("a changed decision reported no change")
	}
}

// TestAutoRoundTripsWithTheSpecKeys pins spec §7's status.json keys and
// omitzero on the switch time (F25).
func TestAutoRoundTripsWithTheSpecKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache", "status.json")
	f := File{Accounts: []Account{{Name: "A"}}}
	f.SetAuto(Auto{Mode: "cache-optimize", Decision: "off", UserChosen: true, BurnRate: 3,
		LastSwitch: &AutoSwitch{From: "A", To: "B", Trigger: "threshold", Window: "5h", Pct: 96}})
	b, err := Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Auto map[string]json.RawMessage `json:"auto"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mode", "decision", "lastSwitch", "userChosen", "burnRate"} {
		if _, ok := raw.Auto[k]; !ok {
			t.Errorf("auto lacks %q: %s", k, b)
		}
	}
	if strings.Contains(string(raw.Auto["lastSwitch"]), `"at"`) {
		t.Fatalf("a zero switch time was written: %s", raw.Auto["lastSwitch"])
	}
	if err := WriteBytes(path, b); err != nil {
		t.Fatal(err)
	}
	got, _ := Load(path)
	if got.Auto == nil || got.Auto.Mode != "cache-optimize" || got.Auto.LastSwitch.Pct != 96 || !got.Auto.UserChosen {
		t.Fatalf("round trip = %+v", got.Auto)
	}
}
