package owners

import (
	"sort"
	"strings"
)

// dirty is what this process changed since its last SUCCESSFUL write.
//
// It exists because owners.json has more than one writer (§4.7). Writing a
// whole-map snapshot means the last writer wins and every other process's
// change is lost with no error and no collision (F96) — so the writer
// applies these changes to whatever the file currently holds instead.
//
// A key is in AT MOST ONE of the three maps: each setter removes it from
// the other two, so the map a key sits in is the last operation this
// process performed on it. mergeInto relies on that — with a key in two
// maps its result would depend on mergeInto's own ordering rather than on
// what the caller did.
type dirty struct {
	// recorded came from Record, and applies only where the FILE has no
	// entry for the key: first writer wins (F19), judged against the file.
	recorded map[string]entry
	// forced came from Reassign and always applies: an explicit owner
	// change is intent, not observation.
	forced map[string]entry
	// forgotten maps key -> the account that was forgotten, NOT a bare set
	// of keys. The delete applies only if the file still shows that
	// account, so a Forget for a logged-out account cannot delete an object
	// someone reassigned to a live account in the meantime.
	forgotten map[string]string
}

func newDirty() dirty {
	return dirty{
		recorded:  map[string]entry{},
		forced:    map[string]entry{},
		forgotten: map[string]string{},
	}
}

func (d *dirty) record(k string, e entry) {
	delete(d.forced, k)
	delete(d.forgotten, k)
	d.recorded[k] = e
}

func (d *dirty) force(k string, e entry) {
	delete(d.recorded, k)
	delete(d.forgotten, k)
	d.forced[k] = e
}

func (d *dirty) forget(k, account string) {
	delete(d.recorded, k)
	delete(d.forced, k)
	d.forgotten[k] = account
}

func (d dirty) empty() bool {
	return len(d.recorded) == 0 && len(d.forced) == 0 && len(d.forgotten) == 0
}

// take returns d's contents and leaves d empty, so the writer can work on a
// snapshot while new mutations accumulate behind it.
func (d *dirty) take() dirty {
	taken := *d
	*d = newDirty()
	return taken
}

// foldUnder puts older's operations UNDERNEATH d's: a key d already carries
// wins, because d's operation happened later. The writer calls this when a
// write fails, to put the taken set back without clobbering anything that
// arrived while the write was in flight.
func (d *dirty) foldUnder(older dirty) {
	for k, e := range older.recorded {
		if d.has(k) {
			continue
		}
		d.recorded[k] = e
	}
	for k, e := range older.forced {
		if d.has(k) {
			continue
		}
		d.forced[k] = e
	}
	for k, a := range older.forgotten {
		if d.has(k) {
			continue
		}
		d.forgotten[k] = a
	}
}

func (d dirty) has(k string) bool {
	if _, ok := d.recorded[k]; ok {
		return true
	}
	if _, ok := d.forced[k]; ok {
		return true
	}
	_, ok := d.forgotten[k]
	return ok
}

// mergeInto applies d to base — the document as it currently exists on disk
// — and returns the merged map. It mutates base in place and returns it,
// EXCEPT when base is nil, where it allocates and returns a new map; so
// callers must use the return value rather than assuming their own
// reference was updated. Every caller does.
//
// The three loops' order does not matter, because a key is in at most one
// of the three maps (see dirty). They are ordered forgotten-recorded-forced
// only so the reading order matches §4.7's.
func mergeInto(base map[string]entry, d dirty) map[string]entry {
	if base == nil {
		base = map[string]entry{}
	}
	for k, account := range d.forgotten {
		// Only if the file still shows the account we forgot. EqualFold
		// because Forget itself matches accounts case-insensitively.
		if e, ok := base[k]; ok && strings.EqualFold(e.Account, account) {
			delete(base, k)
		}
	}
	for k, e := range d.recorded {
		if _, ok := base[k]; !ok {
			base[k] = e
		}
	}
	for k, e := range d.forced {
		base[k] = e
	}
	return base
}

// evictMap drops the oldest entries beyond max. Split out of Map.evict so
// the writer can evict a MERGED document, which may exceed max even when
// this process's own map does not.
//
// Entries with an identical At are ordered arbitrarily, and which of them
// survives differs between runs: sort.Slice is not stable, and the key
// order it starts from comes from Go's deliberately randomised map
// iteration. That is inherited unchanged from Map.evict (owners.go), and
// it is NOT the exotic case it looks like — Record stamps a single `now`
// across every id in one call, so any multi-id Record creates a whole
// group of exact ties. What saves it from mattering: eviction only runs at
// max (10,000 entries), and within a tied group every entry is equally old
// by the only measure this map has, so there is no "right" one to keep.
// Tests must therefore use strictly distinct timestamps to be
// deterministic, which TestEvictMapDropsTheOldestBeyondMax does.
func evictMap(m map[string]entry, max int) {
	if len(m) <= max {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].At.Before(m[keys[j]].At) })
	for _, k := range keys[:len(m)-max] {
		delete(m, k)
	}
}
