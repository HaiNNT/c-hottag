package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func setSpread(t *testing.T, s store.Store, pin string, bNoRotate bool) {
	t.Helper()
	if _, err := s.Update(func(st *store.State) error {
		st.SetPolicy(store.PolicySpread)
		st.SetPin(pin)
		for i := range st.Accounts {
			if st.Accounts[i].Name == "B" {
				st.Accounts[i].NoRotate = bNoRotate
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStatusShowsThePolicyUnderSpread: spread adds one `policy: spread` line,
// with the pin and whether it can take a session now; serial adds nothing.
func TestStatusShowsThePolicyUnderSpread(t *testing.T) {
	_, s := autoHome(t)
	_, out, _ := runChottag(t, "status")
	if strings.Contains(out, "policy") || strings.Contains(out, "pin") {
		t.Fatalf("serial status mentions policy or pin:\n%s", out)
	}

	setSpread(t, s, "", false)
	_, out, _ = runChottag(t, "status")
	if !strings.Contains(out, "\npolicy: spread\n") {
		t.Errorf("spread, no pin: want a bare policy line:\n%s", out)
	}

	setSpread(t, s, "B", false)
	_, out, _ = runChottag(t, "status")
	if !strings.Contains(out, "\npolicy: spread · pin: B\n") {
		t.Errorf("spread, pin B:\n%s", out)
	}

	setSpread(t, s, "B", true)
	_, out, _ = runChottag(t, "status")
	if !strings.Contains(out, "\npolicy: spread · pin: B (not a candidate now)\n") {
		t.Errorf("spread, pin B out of rotation:\n%s", out)
	}
}

// TestStatusJSONCarriesPolicyAndPin: policy and pin are omitted under
// serial (the document is unchanged) and present under spread.
func TestStatusJSONCarriesPolicyAndPin(t *testing.T) {
	_, s := autoHome(t)
	code, out, _ := runChottag(t, "status", "--json")
	if code != exit.OK {
		t.Fatalf("status --json = %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["policy"]; ok {
		t.Errorf("serial document has policy: %v", doc["policy"])
	}
	if _, ok := doc["pin"]; ok {
		t.Errorf("serial document has pin: %v", doc["pin"])
	}

	setSpread(t, s, "B", false)
	_, out, _ = runChottag(t, "status", "--json")
	doc = nil
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["policy"] != "spread" || doc["pin"] != "B" {
		t.Errorf("policy/pin = %v/%v", doc["policy"], doc["pin"])
	}
}
