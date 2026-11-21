package proxy

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// refusalSettleWait is how long a fallback-route refusal from an account the
// daemon has not marked is parked before it is settled without a serving
// outcome (R201).
const refusalSettleWait = 10 * time.Minute

// maxParkedPerAccount caps the parked refusals of one account; the overflow is
// settled as a route refusal at once.
const maxParkedPerAccount = 32

// parkedRefusal is a refused fallback-route request waiting to learn whether
// its account's login is refused (a later serving refusal) or the route is
// (a later serving success). It carries no token and no body.
type parkedRefusal struct {
	account string
	status  int
	errType string
	resent  bool
	method  string
	path    string
	at      time.Time
}

// refusalLedger holds parked refusals per account (lower-cased key).
type refusalLedger struct {
	mu     sync.Mutex
	parked map[string][]parkedRefusal
	count  atomic.Int32 // total parked, for a lock-free empty check
}

func newRefusalLedger() *refusalLedger {
	return &refusalLedger{parked: map[string][]parkedRefusal{}}
}

// empty reports, without locking, that nothing is parked.
func (l *refusalLedger) empty() bool { return l.count.Load() == 0 }

// park stores p; false means the account is at maxParkedPerAccount and p was
// not stored.
func (l *refusalLedger) park(p parkedRefusal) bool {
	key := strings.ToLower(p.account)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.parked[key]) >= maxParkedPerAccount {
		return false
	}
	l.parked[key] = append(l.parked[key], p)
	l.count.Add(1)
	return true
}

// take removes and returns the account's parked refusals.
func (l *refusalLedger) take(account string) []parkedRefusal {
	if l.empty() {
		return nil
	}
	key := strings.ToLower(account)
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.parked[key]
	delete(l.parked, key)
	l.count.Add(-int32(len(out)))
	return out
}

// expired removes and returns every parked refusal parked refusalSettleWait or
// longer before now.
func (l *refusalLedger) expired(now time.Time) []parkedRefusal {
	if l.empty() {
		return nil
	}
	var out []parkedRefusal
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, list := range l.parked {
		keep := list[:0:0]
		for _, p := range list {
			if now.Sub(p.at) >= refusalSettleWait {
				out = append(out, p)
			} else {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			delete(l.parked, key)
		} else {
			l.parked[key] = keep
		}
	}
	l.count.Add(-int32(len(out)))
	return out
}

// reportLoginRefusal reports p as its account's login being refused.
func (s *Server) reportLoginRefusal(p parkedRefusal) {
	if s.cfg.OnServingRefusal != nil {
		s.cfg.OnServingRefusal(p.account, p.status, p.errType, p.resent, p.method, p.path)
	}
}

// reportRouteRefusal counts p as route drift, and as an unclassified route
// when it was a 403 (or a 401 permission_error): R186.
func (s *Server) reportRouteRefusal(p parkedRefusal) {
	s.drift.Add(1)
	if p.status == http.StatusForbidden || (p.status == http.StatusUnauthorized && p.errType == "permission_error") {
		s.driftUnclassified.Add(1)
		if s.cfg.OnUnclassifiedRefusal != nil {
			s.cfg.OnUnclassifiedRefusal(p.account, p.status, p.method, p.path)
		}
	}
}

func (s *Server) settleAsLogin(list []parkedRefusal) {
	for _, p := range list {
		s.reportLoginRefusal(p)
	}
}

func (s *Server) settleAsRoute(list []parkedRefusal) {
	for _, p := range list {
		s.reportRouteRefusal(p)
	}
}

// SettleRefusals settles every parked fallback refusal older than the settle
// wait: as a login refusal when Config.LoginRefused now marks its account,
// else as route drift. The daemon calls it from its periodic stamp.
func (s *Server) SettleRefusals(now time.Time) {
	for _, p := range s.ledger.expired(now) {
		if s.cfg.LoginRefused != nil && s.cfg.LoginRefused(p.account) {
			s.reportLoginRefusal(p)
		} else {
			s.reportRouteRefusal(p)
		}
	}
}

func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}
