package proxy

import (
	"net"
	"sync"
)

// connSet holds a group of connections this proxy currently owns, so they
// can all be closed at once. Server keeps two separate instances, for two
// different populations with different lifetimes:
//
//   - conns holds UPSTREAM connections (dials to the real Anthropic host, or
//     to an upstream proxy). Server.CloseUpstreams drains this one on WAKE:
//     a laptop that slept has upstream sockets the kernel still believes in
//     but the peer has long forgotten.
//   - clientConns holds the CLIENT side of every live MITM tunnel.
//     Server.CloseClientTunnels drains this one on SHUTDOWN only: every
//     CONNECT tunnel is hijacked out of the outer http.Server and therefore
//     invisible to http.Server.Shutdown.
//
// Neither method touches the other's set — closing a client tunnel on wake
// would tear down a session the user is actively using.
//
// The zero value is ready to use.
type connSet struct {
	mu sync.Mutex
	m  map[*trackedConn]struct{}
}

// track registers c and returns a wrapper that deregisters itself when
// closed. Always use the returned connection; the original is still open
// underneath it but nothing will forget it.
func (s *connSet) track(c net.Conn) net.Conn {
	t := &trackedConn{Conn: c, set: s}
	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[*trackedConn]struct{})
	}
	s.m[t] = struct{}{}
	s.mu.Unlock()
	return t
}

// closeAll closes every tracked connection and reports how many it closed.
// A connection that closes concurrently is simply not counted.
func (s *connSet) closeAll() int {
	s.mu.Lock()
	all := make([]*trackedConn, 0, len(s.m))
	for t := range s.m {
		all = append(all, t)
	}
	s.m = nil
	s.mu.Unlock()

	// Closed outside the lock: a Close can block, and holding the lock
	// would stall every in-flight dial behind it.
	var n int
	for _, t := range all {
		if t.closeOnce() {
			n++
		}
	}
	return n
}

func (s *connSet) forget(t *trackedConn) {
	s.mu.Lock()
	delete(s.m, t)
	s.mu.Unlock()
}

func (s *connSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// trackedConn deregisters itself from its set exactly once, whether it is
// closed by its user or by closeAll.
type trackedConn struct {
	net.Conn
	set  *connSet
	once sync.Once
}

func (t *trackedConn) Close() error {
	t.set.forget(t)
	var err error
	t.once.Do(func() { err = t.Conn.Close() })
	return err
}

// closeOnce closes the underlying connection and reports whether this call
// was the one that did it.
func (t *trackedConn) closeOnce() bool {
	var did bool
	t.once.Do(func() { t.Conn.Close(); did = true })
	return did
}
