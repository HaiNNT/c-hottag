package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/router"
)

// Owner discovery (M12/R168). An object request whose owner chottag does not
// know goes to the pool's remote account. When that account refuses a GET or
// HEAD with 403 or 404, the safety net tries the pool's other accounts once
// each and records whichever answers as the owner.
const (
	// MaxOwnerProbes caps the accounts tried for one request.
	MaxOwnerProbes = 8
	// OwnerCandidatesBudget is the one overall deadline for listing the
	// candidates: an account whose token is not ready by then is dropped.
	OwnerCandidatesBudget = 15 * time.Second
	// UnknownOwnerTTL is how long an object nobody here could open is not
	// probed again.
	UnknownOwnerTTL = time.Hour
	// maxUnknownOwners caps the negative cache; the oldest entry is dropped.
	maxUnknownOwners = 1000
)

// unknownOwners is the negative cache: objects no account of the pool could
// open, keyed kind:id, in memory only. The key holds the raw id, which stays
// in memory and is never logged.
type unknownOwners struct {
	now   func() time.Time
	mu    sync.Mutex
	until map[string]time.Time
	seq   map[string]uint64 // insertion sequence of each live key
	next  uint64
	order []seqKey // insertion order, oldest first (may hold stale slots)
}

type seqKey struct {
	key string
	seq uint64
}

func newUnknownOwners(now func() time.Time) *unknownOwners {
	if now == nil {
		now = time.Now
	}
	return &unknownOwners{now: now, until: map[string]time.Time{}, seq: map[string]uint64{}}
}

func unknownKey(kind router.Kind, id string) string { return string(kind) + ":" + id }

// has reports whether key is cached and unexpired; an expired entry is removed.
func (u *unknownOwners) has(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	t, ok := u.until[key]
	if !ok {
		return false
	}
	if !u.now().Before(t) {
		delete(u.until, key)
		delete(u.seq, key)
		return false
	}
	return true
}

func (u *unknownOwners) add(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.next++
	u.until[key] = u.now().Add(UnknownOwnerTTL)
	u.seq[key] = u.next
	u.order = append(u.order, seqKey{key, u.next})
	// Evict the oldest live entries; a slot whose sequence no longer matches
	// its key's (cleared, expired or re-added) is stale and just skipped.
	for len(u.until) > maxUnknownOwners && len(u.order) > 0 {
		old := u.order[0]
		u.order = u.order[1:]
		if u.seq[old.key] == old.seq {
			delete(u.until, old.key)
			delete(u.seq, old.key)
		}
	}
	if len(u.order) > 2*maxUnknownOwners {
		kept := make([]seqKey, 0, len(u.until))
		for _, o := range u.order {
			if u.seq[o.key] == o.seq {
				kept = append(kept, o)
			}
		}
		u.order = kept
	}
}

func (u *unknownOwners) clear(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.until, key)
	delete(u.seq, key)
}

func (u *unknownOwners) size() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.until)
}

// objectProbe is the safety net's view of an unknown-owner object request.
type objectProbe struct {
	kind   router.Kind
	id     string
	prober OwnerProber // nil: no discovery, only the drift exemption
	cache  *unknownOwners
	// onUnknown marks the trace record and calls Config.OnUnknownOwner with
	// the first refusal's status.
	onUnknown func(status int)
	// onFound marks the trace record and calls Config.OnOwnerFound.
	onFound func(found string, status int)
}

func (p *objectProbe) key() string { return unknownKey(p.kind, p.id) }

func probeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// discover tries the pool's other accounts for req. It returns the first
// non-refusal answer (found true; first is already drained and the owner is
// recorded), or found false when every candidate refused, there were none, or
// one failed at the transport level; first is then still unread, to be
// returned to the client as the remote's own answer.
func (s *safetyNet) discover(req *http.Request, body []byte, first *http.Response) (*http.Response, bool) {
	p := s.probe
	// One deadline for the whole lookup: a slow account's token is dropped,
	// not waited for one after another.
	cctx, cancel := context.WithTimeout(req.Context(), OwnerCandidatesBudget)
	cands := p.prober.OwnerCandidates(cctx, s.account)
	cancel()
	if len(cands) > MaxOwnerProbes {
		cands = cands[:MaxOwnerProbes]
	}
	for _, c := range cands {
		if c.Token == "" || strings.EqualFold(c.Account, s.account) {
			continue
		}
		if req.Context().Err() != nil {
			return nil, false
		}
		try := clone(req, body)
		try.Header.Set("Authorization", "Bearer "+c.Token)
		try.Header.Del("X-Api-Key")
		resp, err := s.base.RoundTrip(try)
		if err != nil {
			return nil, false
		}
		if refused(resp.StatusCode) {
			drain(resp)
			continue
		}
		status := first.StatusCode
		drain(first)
		if resp.StatusCode < 400 {
			// Only a success proves ownership; a 429 or 5xx is just the
			// answer to hand back.
			s.chooser.Record(p.kind, []string{p.id}, c.Account)
			p.cache.clear(p.key())
			p.onFound(c.Account, status)
		} else if s.onRetarget != nil {
			// The record and usage belong to the account that answered.
			s.onRetarget(c.Account)
		}
		return resp, true
	}
	// Every candidate refused, or there were none: remember it. A transport
	// error or a gone client returned above without caching.
	p.cache.add(p.key())
	return nil, false
}
