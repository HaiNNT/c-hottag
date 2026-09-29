package leakscan

import (
	"testing"
	"time"
)

// TestExportPublicCheckOnHEAD pins M2: export-public --check on HEAD, the
// last commit. The gate runs before a change is committed, so this checks
// the commit before it, not the change itself: a change export-public
// would refuse fails here at the first gate run after its commit, and in
// CI before any tag, never first at export time. To refuse the change
// itself, before it is committed, a pre-commit step runs
// `scripts/export-public --check-index` instead (re-review R-M2): the same
// checks on the tree of the current index. test/export pins that flag on
// a temp repo; this test stays on HEAD, which a CI checkout's index
// equals anyway, and never writes to this repo's own index.
// Skips outside the private repo (M3's marker): a public snapshot has no
// docs/internal/leak-patterns.txt for --check to use by default, and is
// never itself re-exported.
func TestExportPublicCheckOnHEAD(t *testing.T) {
	r := root(t)
	if !privateRepoMarker(t) {
		t.Skip("no docs/internal/: a public snapshot, never re-exported")
	}
	start := time.Now()
	out, errb, code := script(t, r, "", "export-public", "--check", "HEAD")
	t.Logf("export-public --check HEAD took %s", time.Since(start))
	if code != 0 {
		t.Fatalf("exit %d; want 0 (HEAD must pass what export-public would refuse):\n%s%s", code, out, errb)
	}
}
