//go:build !chottag_fakeusage

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

// A release build ignores the variable completely: no row changes, no
// line is logged, and the version carries no suffix.
func TestReleaseBuildIgnoresTheFakeLimitVariable(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "B", Dir: "/slots/B"}}}, nil
	}
	seedRosterAtStartup(state, sink)
	var log bytes.Buffer
	getenv := func(k string) string {
		return map[string]string{"CHOTTAG_FAKE_LIMIT": "B", "CHOTTAG_FAKE_LIMIT_TTL": "bogus"}[k]
	}
	applyFakeLimits(getenv, state, sink, &log, time.Now())
	sink.mu.Lock()
	for _, a := range sink.file.Accounts {
		if a.Limited || a.Usage != nil {
			t.Errorf("%s = %+v, want untouched in a release build", a.Name, a)
		}
	}
	sink.mu.Unlock()
	if log.Len() != 0 {
		t.Errorf("log = %q, want nothing in a release build", log.String())
	}
	if strings.Contains(Version, "+fakeusage") {
		t.Errorf("Version = %q in a release build", Version)
	}
}
