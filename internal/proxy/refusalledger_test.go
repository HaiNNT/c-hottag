package proxy

import (
	"sync"
	"testing"
	"time"
)

func TestRefusalLedgerKeysAreCaseInsensitive(t *testing.T) {
	l := newRefusalLedger()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	l.park(parkedRefusal{account: "Acme", status: 403, at: at})
	if got := l.take("aCME"); len(got) != 1 || got[0].account != "Acme" {
		t.Fatalf("take = %+v", got)
	}
	if !l.empty() || l.take("acme") != nil {
		t.Fatal("ledger not empty after take")
	}
}

func TestRefusalLedgerCapAndExpiry(t *testing.T) {
	l := newRefusalLedger()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for i := 0; i < maxParkedPerAccount; i++ {
		if !l.park(parkedRefusal{account: "C", at: at}) {
			t.Fatalf("park %d refused", i)
		}
	}
	if l.park(parkedRefusal{account: "c", at: at}) {
		t.Fatal("park beyond the cap accepted")
	}
	if !l.park(parkedRefusal{account: "B", at: at.Add(time.Minute)}) {
		t.Fatal("another account refused")
	}
	got := l.expired(at.Add(refusalSettleWait))
	if len(got) != maxParkedPerAccount {
		t.Fatalf("expired = %d, want %d", len(got), maxParkedPerAccount)
	}
	if l.empty() {
		t.Fatal("B should still be parked")
	}
	if got := l.expired(at.Add(time.Minute + refusalSettleWait)); len(got) != 1 || !l.empty() {
		t.Fatalf("second expiry = %d, empty %v", len(got), l.empty())
	}
}

func TestRefusalLedgerConcurrent(t *testing.T) {
	l := newRefusalLedger()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.park(parkedRefusal{account: "C", at: at})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.take("c")
				l.expired(at.Add(refusalSettleWait))
			}
		}()
	}
	wg.Wait()
	l.take("C")
	if !l.empty() {
		t.Fatalf("count = %d after draining", l.count.Load())
	}
}
