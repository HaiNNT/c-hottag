package owners

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// A key must appear in at most ONE of the three maps. Each setter therefore
// removes the key from the other two: the LAST operation on a key is the
// only one that can still be true of this process's intent. Without this,
// a Record followed by a Reassign would leave both recorded[k] and
// forced[k] set, and mergeInto's outcome would depend on its internal
// ordering rather than on what the caller actually did.
func TestDirtyKeepsOnlyTheLastOperationPerKey(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		ops  func(d *dirty)
		want string // "recorded" | "forced" | "forgotten"
	}{
		{"record then force", func(d *dirty) {
			d.record("artifact:x", entry{Account: "A", At: now})
			d.force("artifact:x", entry{Account: "B", At: now})
		}, "forced"},
		{"force then forget", func(d *dirty) {
			d.force("artifact:x", entry{Account: "B", At: now})
			d.forget("artifact:x", "B")
		}, "forgotten"},
		{"forget then record", func(d *dirty) {
			d.forget("artifact:x", "A")
			d.record("artifact:x", entry{Account: "C", At: now})
		}, "recorded"},
		{"forget then force", func(d *dirty) {
			d.forget("artifact:x", "A")
			d.force("artifact:x", entry{Account: "C", At: now})
		}, "forced"},
		{"record then record", func(d *dirty) {
			d.record("artifact:x", entry{Account: "A", At: now})
			d.record("artifact:x", entry{Account: "A", At: now})
		}, "recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDirty()
			tc.ops(&d)
			_, inRecorded := d.recorded["artifact:x"]
			_, inForced := d.forced["artifact:x"]
			_, inForgotten := d.forgotten["artifact:x"]
			got := map[bool]string{true: "recorded"}[inRecorded]
			if inForced {
				got = "forced"
			}
			if inForgotten {
				got = "forgotten"
			}
			n := 0
			for _, in := range []bool{inRecorded, inForced, inForgotten} {
				if in {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("key is in %d maps (recorded=%v forced=%v forgotten=%v), want exactly 1",
					n, inRecorded, inForced, inForgotten)
			}
			if got != tc.want {
				t.Errorf("key landed in %q, want %q", got, tc.want)
			}
		})
	}
}

// The three merge rules, each against a file that disagrees with us. These
// are §4.7's rules verbatim; if one of these changes, the spec changes with
// it.
func TestMergeIntoAppliesTheThreeRules(t *testing.T) {
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		base map[string]entry
		ops  func(d *dirty)
		want map[string]string // key -> account; absent key means deleted
	}{
		{
			name: "recorded defers to the file: first writer wins, judged against disk",
			base: map[string]entry{"artifact:x": {Account: "B", At: old}},
			ops:  func(d *dirty) { d.record("artifact:x", entry{Account: "A", At: now}) },
			want: map[string]string{"artifact:x": "B"},
		},
		{
			name: "recorded applies when the file has no entry",
			base: map[string]entry{},
			ops:  func(d *dirty) { d.record("artifact:x", entry{Account: "A", At: now}) },
			want: map[string]string{"artifact:x": "A"},
		},
		{
			name: "forced overrides the file",
			base: map[string]entry{"artifact:x": {Account: "B", At: old}},
			ops:  func(d *dirty) { d.force("artifact:x", entry{Account: "A", At: now}) },
			want: map[string]string{"artifact:x": "A"},
		},
		{
			name: "forgotten deletes when the file still shows that account",
			base: map[string]entry{"artifact:x": {Account: "A", At: old}},
			ops:  func(d *dirty) { d.forget("artifact:x", "A") },
			want: map[string]string{},
		},
		{
			name: "forgotten deletes case-insensitively, as Forget matches",
			base: map[string]entry{"artifact:x": {Account: "alice", At: old}},
			ops:  func(d *dirty) { d.forget("artifact:x", "Alice") },
			want: map[string]string{},
		},
		{
			name: "forgotten does NOT delete an object reassigned to another account",
			base: map[string]entry{"artifact:x": {Account: "B", At: old}},
			ops:  func(d *dirty) { d.forget("artifact:x", "A") },
			want: map[string]string{"artifact:x": "B"},
		},
		{
			name: "keys the file has and we never touched are preserved",
			base: map[string]entry{"artifact:keep": {Account: "C", At: old}},
			ops:  func(d *dirty) { d.record("artifact:new", entry{Account: "A", At: now}) },
			want: map[string]string{"artifact:keep": "C", "artifact:new": "A"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDirty()
			tc.ops(&d)
			got := mergeInto(tc.base, d)
			if len(got) != len(tc.want) {
				t.Fatalf("merged map has %d keys %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for k, account := range tc.want {
				e, ok := got[k]
				if !ok {
					t.Errorf("key %q missing from merged map", k)
					continue
				}
				if e.Account != account {
					t.Errorf("key %q owned by %q, want %q", k, e.Account, account)
				}
			}
		})
	}
}

// foldUnder is what stops a FAILED write from losing data: the taken set
// goes back underneath whatever arrived while the write was in flight, and
// the newer operation on a key wins because it happened later.
func TestFoldUnderKeepsTheNewerOperationPerKey(t *testing.T) {
	now := time.Now()
	older := newDirty()
	older.record("artifact:x", entry{Account: "A", At: now})
	older.record("artifact:y", entry{Account: "A", At: now})

	newer := newDirty()
	newer.force("artifact:x", entry{Account: "B", At: now})
	newer.foldUnder(older)

	if _, ok := newer.recorded["artifact:x"]; ok {
		t.Error("foldUnder resurrected the older recorded op for a key the newer set already owns")
	}
	if e, ok := newer.forced["artifact:x"]; !ok || e.Account != "B" {
		t.Errorf("newer forced op for artifact:x = %+v (present=%v), want B", e, ok)
	}
	if e, ok := newer.recorded["artifact:y"]; !ok || e.Account != "A" {
		t.Errorf("older-only key artifact:y = %+v (present=%v), want it folded in as A", e, ok)
	}
}

// evictMap must trim a MERGED document, which can exceed max even when
// this process's own map does not — the file may carry entries from
// another process.
func TestEvictMapDropsTheOldestBeyondMax(t *testing.T) {
	base := map[string]entry{}
	for i := 0; i < 5; i++ {
		base[fmt.Sprintf("artifact:%d", i)] = entry{
			Account: "A",
			At:      time.Date(2026, 9, 1+i, 0, 0, 0, 0, time.UTC),
		}
	}
	evictMap(base, 3)
	if len(base) != 3 {
		t.Fatalf("map has %d entries after evictMap(_, 3), want 3", len(base))
	}
	for _, dropped := range []string{"artifact:0", "artifact:1"} {
		if _, ok := base[dropped]; ok {
			t.Errorf("%s survived eviction; the two oldest must go first", dropped)
		}
	}
}

// lockPathFor is now the single derivation of owners.json's lock path, so
// the only thing left to get wrong is the derivation itself. Asserting the
// literal expected path is what catches that; comparing two expressions that
// both call lockPathFor cannot, because it restates the extraction rather
// than testing it.
func TestLockPathIsBesideOwnersJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	want := filepath.Join(dir, "owners.lock")

	if got := lockPathFor(path); got != want {
		t.Errorf("lockPathFor(%q) = %q, want %q", path, got, want)
	}

	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Checks Map's own composition end to end, not just that it delegates.
	if got := m.lockPath(); got != want {
		t.Errorf("Map.lockPath() = %q, want %q", got, want)
	}
}
