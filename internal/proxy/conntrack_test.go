package proxy

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestConnSetTracksAndClosesOnce(t *testing.T) {
	var set connSet
	a, b := net.Pipe()
	defer b.Close()

	tracked := set.track(a)
	if got := set.len(); got != 1 {
		t.Fatalf("len = %d, want 1", got)
	}
	if n := set.closeAll(); n != 1 {
		t.Errorf("closeAll = %d, want 1", n)
	}
	if got := set.len(); got != 0 {
		t.Errorf("len after closeAll = %d, want 0", got)
	}
	// Closing a tracked conn again is harmless and does not double-count.
	tracked.Close()
	if n := set.closeAll(); n != 0 {
		t.Errorf("second closeAll = %d, want 0", n)
	}
}

func TestConnSetForgetsOnClose(t *testing.T) {
	var set connSet
	a, b := net.Pipe()
	defer b.Close()

	tracked := set.track(a)
	tracked.Close()
	if got := set.len(); got != 0 {
		t.Errorf("a closed conn stayed tracked: len = %d, want 0", got)
	}
}

func TestConnSetIsSafeUnderConcurrency(t *testing.T) {
	var set connSet
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, b := net.Pipe()
			defer b.Close()
			c := set.track(a)
			c.Close()
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); set.closeAll() }()
	wg.Wait()
	set.closeAll()
	if got := set.len(); got != 0 {
		t.Errorf("len = %d, want 0", got)
	}
}

func TestNewSetsKeepAliveProbes(t *testing.T) {
	s := New(Config{})
	d, ok := s.dialer()
	if !ok {
		t.Skip("a custom DialContext was configured; nothing to assert")
	}
	ka := d.KeepAliveConfig
	if !ka.Enable {
		t.Error("keepalive is not enabled")
	}
	if ka.Idle != 15*time.Second {
		t.Errorf("Idle = %v, want 15s", ka.Idle)
	}
	if ka.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", ka.Interval)
	}
	if ka.Count != 3 {
		t.Errorf("Count = %d, want 3", ka.Count)
	}
}
