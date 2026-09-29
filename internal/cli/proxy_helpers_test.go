package cli

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// syncBuf is an io.Writer safe to read from a different goroutine than the
// one writing to it, synchronized by closing done on the first Write: since
// runProxy makes exactly one stdout write (the two export lines, in a
// single fmt.Fprint call) before it blocks serving, waiting on done gives a
// race-free point at which to read the buffer's content.
type syncBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	done chan struct{}
}

func newSyncBuf() *syncBuf { return &syncBuf{done: make(chan struct{})} }

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.buf.Write(p)
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
	return n, err
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitForListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}
