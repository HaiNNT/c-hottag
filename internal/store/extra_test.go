package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A state.json with keys this binary does not know survives Load, Update and
// save with those keys unchanged, so an older binary never erases a newer
// one's fields.
func TestUpdateKeepsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	in := `{
  "version": 1,
  "accounts": [
    {"name": "B", "dir": "/slots/B", "addedAt": "2026-09-01T00:00:00Z", "badge": {"tier": "gold", "n": [1, 2]}},
    {"name": "work", "dir": "/slots/work", "addedAt": "2026-09-01T00:00:00Z"}
  ],
  "serving": "B",
  "port": 47821,
  "futureThing": {"a": 1, "b": ["x"]},
  "pools": null
}
`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir}
	if _, err := s.Update(func(st *State) error { st.SetPolicy(PolicySpread); st.Serving = "work"; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got, want struct {
		FutureThing json.RawMessage   `json:"futureThing"`
		Pools       json.RawMessage   `json:"pools"`
		Policy      string            `json:"policy"`
		Serving     string            `json:"serving"`
		Accounts    []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatal(err)
	}
	same := func(a, b json.RawMessage) bool {
		var x, y any
		return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
	}
	if !same(got.FutureThing, want.FutureThing) || !same(got.Pools, want.Pools) {
		t.Fatalf("a top-level unknown key was lost or changed:\n%s", raw)
	}
	var acct map[string]json.RawMessage
	if err := json.Unmarshal(got.Accounts[0], &acct); err != nil {
		t.Fatal(err)
	}
	if !same(acct["badge"], json.RawMessage(`{"tier": "gold", "n": [1, 2]}`)) {
		t.Fatalf("an unknown account key was lost or changed:\n%s", raw)
	}
	if got.Policy != PolicySpread || got.Serving != "work" {
		t.Fatalf("the update itself was not saved:\n%s", raw)
	}
	// and a state with nothing unknown writes nothing extra.
	clean := t.TempDir()
	cs := Store{Dir: clean}
	if _, err := cs.Update(func(st *State) error { return st.Add(Account{Name: "B", Dir: "/slots/B"}) }); err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	b, _ := os.ReadFile(filepath.Join(clean, "state.json"))
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		if !stateKeys[k] {
			t.Fatalf("unexpected key %q in a clean state", k)
		}
	}
}

// A key that differs from a known field only in case is that field to
// encoding/json, so it is not kept as unknown: otherwise it would override
// the field on the next load (a hand-edited "NoRotate" undoing `rotate off`).
func TestUnknownKeysIgnoreCaseVariantsOfKnownFields(t *testing.T) {
	extra, err := unknownKeys([]byte(`{"Serving":"A","noRotate":true,"NoRotate":false,"future":1}`), stateKeys)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extra["Serving"]; ok {
		t.Errorf("Serving kept as unknown: %v", extra)
	}
	extra, err = unknownKeys([]byte(`{"NoRotate":false,"future":1}`), accountKeys)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extra["NoRotate"]; ok || len(extra) != 1 {
		t.Errorf("extra = %v, want only future", extra)
	}
}
