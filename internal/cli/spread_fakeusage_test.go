//go:build chottag_fakeusage

package cli

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
)

// The M7 exit simulation (spec §9): the engine driven directly, with an
// injected clock and synthetic usage. Nothing here waits on real time.

func simNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("acc%d", i)
	}
	return out
}

func simSIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("sid%02d", i)
	}
	return out
}

func TestSpreadSimFiftySessionsOnTenAccounts(t *testing.T) {
	r := newSpreadRig(t, simNames(10)...)
	for _, sid := range simSIDs(50) {
		r.place(sid)
	}
	counts := r.counts()
	for _, n := range simNames(10) {
		if c := counts[n]; c < 4 || c > 6 {
			t.Errorf("%s has %d sessions, want 4 to 6 (all: %v)", n, c, counts)
		}
	}
}

// simMoves watches every session's account at each step and records its
// moves, failing on a move twice within 10 minutes or straight back to the
// account it just left.
type simMoves struct {
	t     *testing.T
	last  map[string]time.Time
	from  map[string]string
	count map[string]int
	total int
}

func newSimMoves(t *testing.T) *simMoves {
	return &simMoves{t: t, last: map[string]time.Time{}, from: map[string]string{}, count: map[string]int{}}
}

// saw records that sid went from before to after at now.
func (m *simMoves) saw(sid, before, after string, now time.Time) {
	if before == after {
		return
	}
	m.t.Helper()
	if prev, ok := m.last[sid]; ok {
		if gap := now.Sub(prev); gap < 10*time.Minute {
			m.t.Errorf("%s moved twice within %s (%s then %s)", sid, gap, prev.Format("15:04"), now.Format("15:04"))
		}
		if m.from[sid] == after && now.Sub(prev) < 10*time.Minute {
			m.t.Errorf("%s went straight back to %s at %s", sid, after, now.Format("15:04"))
		}
	}
	m.last[sid], m.from[sid] = now, before
	m.count[sid]++
	m.total++
}

func (m *simMoves) sessionsMovedAtLeast(n int) int {
	c := 0
	for _, v := range m.count {
		if v >= n {
			c++
		}
	}
	return c
}

// A sawtooth burn: every account's 5h use climbs 0.8% a minute from its own
// offset, crosses its switch point, and falls back to near zero (a new
// window), so accounts leave and re-enter candidacy and sessions have
// somewhere to go and, later, somewhere to return to.
func TestSpreadSimFiveHoursOfSteadyBurnOnlyMovesAtSwitchPoints(t *testing.T) {
	names := simNames(10)
	r := newSpreadRig(t, names...)
	params := autoParams(r.st)
	sids := simSIDs(30)
	mv := newSimMoves(t)
	use := func() {
		for i, n := range names {
			r.usage(n, math.Mod(float64(i*11)+0.8*r.now.Sub(eligNow).Minutes(), 110), 10)
		}
	}
	use()
	for _, sid := range sids {
		r.place(sid)
	}
	for end := eligNow.Add(5 * time.Hour); r.now.Before(end); {
		r.advance(time.Minute)
		use()
		for _, sid := range sids {
			before, _ := r.eng.placed(sid)
			f := r.file()
			a, _ := (spreadView{accts: planAccounts(&r.st, &f, r.now)}).find(before)
			wasCandidate := autoswitch.Candidate(a, params, r.now)
			after := r.place(sid)
			if after != before && wasCandidate {
				t.Errorf("%s moved off %s at %s, which was still a candidate (not a switch-point move)", sid, before, r.now.Format("15:04"))
			}
			mv.saw(sid, before, after, r.now)
		}
	}
	if mv.total == 0 {
		t.Fatal("no session ever moved: the burn never reached a switch point, so the test proves nothing")
	}
	if mv.sessionsMovedAtLeast(2) == 0 {
		t.Fatal("no session moved twice in five hours: the 10-minute rule was never in play")
	}
}

// Cold moves: conversations keep changing (every session's cache is cold at
// every request) and five accounts carry heavy use, so sessions on those may
// move to the light ones, but never twice in 10 minutes, never back, and
// sessions on the light accounts stay.
func TestSpreadSimColdCachesMoveOnlyOffTheHeavyAccountsAndNeverFlap(t *testing.T) {
	names := simNames(10)
	r := newSpreadRig(t, names...)
	sids := simSIDs(20)
	start := map[string]string{}
	for _, sid := range sids {
		start[sid] = r.place(sid)
	}
	heavy := map[string]bool{}
	for i, n := range names {
		if i < 5 {
			heavy[n] = true
			r.usage(n, 70, 10)
		}
	}
	mv := newSimMoves(t)
	for step := 0; step < 120; step++ {
		r.advance(time.Minute)
		for i, n := range names {
			if i < 5 {
				r.usage(n, 70, 10)
			}
		}
		for _, sid := range sids {
			r.convs[sid]++
			before, _ := r.eng.placed(sid)
			after := r.place(sid)
			if after != before && !heavy[before] {
				t.Errorf("%s left the light account %s for %s", sid, before, after)
			}
			if after != before && heavy[after] {
				t.Errorf("%s moved onto the heavy account %s", sid, after)
			}
			mv.saw(sid, before, after, r.now)
		}
	}
	if mv.total == 0 {
		t.Fatal("no cold move happened: the test proves nothing")
	}
}

func TestSpreadSimALimitMovesOnlyThatAccountsSessions(t *testing.T) {
	names := simNames(10)
	r := newSpreadRig(t, names...)
	sids := simSIDs(50)
	before := map[string]string{}
	for _, sid := range sids {
		before[sid] = r.place(sid)
	}
	r.advance(time.Minute)
	limited := names[3]
	r.limit(limited)
	gained := map[string]int{}
	moved := 0
	for _, sid := range sids {
		after := r.place(sid)
		if before[sid] != limited {
			if after != before[sid] {
				t.Errorf("%s on %s moved to %s, but only %s is limited", sid, before[sid], after, limited)
			}
			continue
		}
		moved++
		if after == limited {
			t.Errorf("%s is still on the limited account", sid)
		}
		gained[after]++
	}
	if moved == 0 {
		t.Fatal("no session was on the limited account")
	}
	for n, c := range gained {
		if c > 2 {
			t.Errorf("%s took %d of the %d moved sessions: not spread (%v)", n, c, moved, gained)
		}
	}
	if len(gained) < 3 {
		t.Errorf("the %d sessions went to only %d accounts: %v", moved, len(gained), gained)
	}
}
