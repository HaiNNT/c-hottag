package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageLedgerCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, usage string)
		status string
		want   string
	}{
		{"absent", func(t *testing.T, usage string) {}, StatusOK, "no usage ledger yet"},
		{"never imported", func(t *testing.T, usage string) { must(t, os.MkdirAll(usage, 0o700)) }, StatusInfo, "no history imported yet"},
		{"imported", func(t *testing.T, usage string) {
			must(t, os.MkdirAll(usage, 0o700))
			must(t, os.WriteFile(filepath.Join(usage, "import.json"), []byte("{}"), 0o600))
		}, StatusOK, "last transcript import"},
		{"read-only", func(t *testing.T, usage string) {
			must(t, os.MkdirAll(usage, 0o500))
			t.Cleanup(func() { os.Chmod(usage, 0o700) })
		}, StatusInfo, "not writable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			tc.setup(t, filepath.Join(ti.home, "usage"))
			r := rowByID(t, mustRun(t, ti.env, []Check{usageLedgerCheck()}, true), "usage-ledger")
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.want) {
				t.Fatalf("row = %+v, want status %s with %q", r, tc.status, tc.want)
			}
		})
	}
}

func TestUsageLedgerCheckNeverWrites(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.MkdirAll(filepath.Join(ti.home, "usage"), 0o700))
	before := ti.snapshot()
	mustRun(t, ti.env, []Check{usageLedgerCheck()}, true)
	assertChangedOnly(t, before, ti.snapshot())
}
