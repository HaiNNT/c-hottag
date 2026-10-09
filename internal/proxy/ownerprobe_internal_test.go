package proxy

import (
	"strconv"
	"testing"
	"time"
)

func TestUnknownOwnersCacheCapsAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	u := newUnknownOwners(func() time.Time { return now })
	for i := 0; i < maxUnknownOwners+5; i++ {
		u.add("artifact:" + strconv.Itoa(i))
	}
	if u.size() != maxUnknownOwners {
		t.Fatalf("size = %d, want %d", u.size(), maxUnknownOwners)
	}
	if u.has("artifact:0") || !u.has("artifact:"+strconv.Itoa(maxUnknownOwners+4)) {
		t.Fatal("the oldest entry should be dropped, the newest kept")
	}
	now = now.Add(UnknownOwnerTTL)
	if u.has("artifact:" + strconv.Itoa(maxUnknownOwners+4)) {
		t.Fatal("entry should expire at the TTL")
	}
}

func TestUnknownOwnersReaddedEntryIsNotEvictedByItsStaleSlot(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	u := newUnknownOwners(func() time.Time { return now })
	u.add("artifact:x")
	now = now.Add(UnknownOwnerTTL)
	if u.has("artifact:x") {
		t.Fatal("should have expired")
	}
	for i := 0; i < maxUnknownOwners-1; i++ {
		u.add("artifact:" + strconv.Itoa(i))
	}
	u.add("artifact:x") // re-added late: its first slot is stale
	u.add("artifact:last")
	if u.size() != maxUnknownOwners {
		t.Fatalf("size = %d", u.size())
	}
	if !u.has("artifact:x") || u.has("artifact:0") {
		t.Fatal("eviction should drop the oldest live entry (0), never the fresh x via its stale slot")
	}
}
