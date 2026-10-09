package store_test

import (
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestNamesModeDefaultsToOnAndReadsRemovedModelAsOn(t *testing.T) {
	cases := map[string]string{"": "on", "on": "on", "model": "on", "off": "off", "junk": "on"}
	for in, want := range cases {
		if got := (store.State{Names: in}).NamesMode(); got != want {
			t.Errorf("Names %q: mode %q, want %q", in, got, want)
		}
	}
	var st store.State
	st.SetNames("on")
	if st.Names != "" {
		t.Errorf("on stored as %q, want absent", st.Names)
	}
	st.SetNames("off")
	if st.NamesMode() != "off" {
		t.Errorf("mode %q after set off", st.NamesMode())
	}
}
