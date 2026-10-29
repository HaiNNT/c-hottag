package autoswitch

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usage"
)

// The pace meter (R192 spec §3): decayed burn rates per account and window,
// and per session, in units per hour. Units are Burn's: a rise of p
// percentage points on an account of u plan units is p*u (a max20x 5h window
// is 2000, a pro one 100). It is pure: the clock arrives as an
// argument and nothing is read or written here. It is not safe for
// concurrent use; the daemon serialises it.
const (
	// RCap caps a pace ratio: past it, more headroom is not better.
	RCap = 3.0
	// MinSessionBurn floors the prior so idle sessions cannot drag it to 0.
	MinSessionBurn = 100.0
	// DefaultSessionBurn is the prior when no session is measured.
	DefaultSessionBurn = 200.0

	tau5h = 30 * time.Minute
	tau7d = 6 * time.Hour
	// tauSession is the decay constant of a session's rate.
	tauSession = 30 * time.Minute
	// measuredAfter is how long after its first credit a session counts as measured.
	measuredAfter = 15 * time.Minute
	// activeWithin is how recently a session must have been credited to feed the prior.
	activeWithin = 30 * time.Minute
	// minWall bounds the rate's denominator, so two readings seconds apart
	// do not read as a huge rate.
	minWall = time.Minute
	// tieEps is the pace-ratio difference below which two accounts tie.
	tieEps = 0.05
	// imminentWall is the 5h ttl below which an account is passed over.
	imminentWall = 15 * time.Minute
)

func tauOf(w Window) time.Duration {
	if w == Win7d {
		return tau7d
	}
	return tau5h
}

// Meter is one window's decayed rise and wall time on one account.
type Meter struct {
	Has   bool      `json:"has"`
	Pct   float64   `json:"pct"` // the baseline reading, 0-100
	At    time.Time `json:"at"`
	Reset time.Time `json:"reset,omitzero"`
	Rise  float64   `json:"rise"` // decayed units
	Wall  float64   `json:"wall"` // decayed seconds
}

func decay(dt, tau time.Duration) float64 {
	return math.Exp(-float64(dt) / float64(tau))
}

// feed takes one reading and returns the rise (units) it counted as burn:
// 0 for the first reading, an out-of-order one, a reset, an idle gap or
// header rounding.
func (m *Meter) feed(pct float64, reset, at time.Time, units float64, tau time.Duration) (counted float64) {
	if !m.Has {
		m.Has, m.Pct, m.At, m.Reset = true, pct, at, reset
		return 0
	}
	if at.Before(m.At) {
		if m.At.Sub(at) > tau {
			// far older than the last reading: the clock stepped back, so
			// the baseline restarts instead of being ignored for good
			*m = Meter{Has: true, Pct: pct, At: at, Reset: reset}
		}
		return 0
	}
	dt := at.Sub(m.At)
	d := decay(dt, tau)
	m.Rise *= d
	m.Wall = m.Wall*d + dt.Seconds()
	switch {
	case (!m.Reset.IsZero() && !at.Before(m.Reset)) || pct < m.Pct/2:
		m.Pct, m.Reset = pct, reset // a reset: restart, no burn
	case dt > 4*tau:
		m.Pct = pct // idle: the rise cannot be attributed
		if !reset.IsZero() {
			m.Reset = reset
		}
	case pct > m.Pct:
		counted = (pct - m.Pct) * units
		m.Rise += counted
		m.Pct = pct
		if !reset.IsZero() {
			m.Reset = reset
		}
	default: // header rounding: baseline stays at the higher reading
		if !reset.IsZero() {
			m.Reset = reset
		}
	}
	m.At = at
	return counted
}

func rate(rise, wall float64, last, now time.Time, tau time.Duration) float64 {
	dt := now.Sub(last)
	if dt < 0 {
		dt = 0
	}
	d := decay(dt, tau)
	w := max(wall*d+dt.Seconds(), minWall.Seconds())
	return rise * d / w * 3600
}

func (m Meter) rate(now time.Time, tau time.Duration) (float64, bool) {
	if !m.Has {
		return 0, false
	}
	return rate(m.Rise, m.Wall, m.At, now, tau), true
}

// AccountPace is one account's two meters and its last 5h rise.
type AccountPace struct {
	W5       Meter   `json:"w5"`
	W7       Meter   `json:"w7"`
	LastRise float64 `json:"lastRise"` // units the last Observe counted as burn, until credited
}

// SessionPace is one session's decayed credited spend.
type SessionPace struct {
	First      time.Time `json:"first"`
	At         time.Time `json:"at"`
	LastCredit time.Time `json:"lastCredit"`
	Rise       float64   `json:"rise"`
	Wall       float64   `json:"wall"`
}

// Pace is the meter. The zero value is ready.
type Pace struct {
	Accounts map[string]*AccountPace `json:"accounts,omitempty"`
	Sessions map[string]*SessionPace `json:"sessions,omitempty"`
}

// Observe feeds both windows of account from one response.
func (p *Pace) Observe(account string, units float64, s usage.Snapshot, at time.Time) {
	if p.Accounts == nil {
		p.Accounts = map[string]*AccountPace{}
	}
	ap := p.Accounts[account]
	if ap == nil {
		ap = &AccountPace{}
		p.Accounts[account] = ap
	}
	ap.LastRise = 0
	if w := s.FiveHour; w.HasUtilization {
		ap.LastRise = ap.W5.feed(w.Utilization*100, w.ResetsAt, at, units, tau5h)
	}
	if w := s.SevenDay; w.HasUtilization {
		ap.W7.feed(w.Utilization*100, w.ResetsAt, at, units, tau7d)
	}
}

// Credit credits to sid the 5h rise (units) the last Observe of account
// counted as burn; units is the account's size, already applied by Observe.
func (p *Pace) Credit(sid, account string, _ float64, at time.Time) {
	var rise float64
	if ap := p.Accounts[account]; ap != nil {
		rise = ap.LastRise
		ap.LastRise = 0
	}
	if p.Sessions == nil {
		p.Sessions = map[string]*SessionPace{}
	}
	sp := p.Sessions[sid]
	if sp == nil {
		p.Sessions[sid] = &SessionPace{First: at, At: at, LastCredit: at, Rise: rise}
		return
	}
	if at.Before(sp.At) {
		return
	}
	dt := at.Sub(sp.At)
	d := decay(dt, tauSession)
	sp.Rise = sp.Rise*d + rise
	sp.Wall = sp.Wall*d + dt.Seconds()
	sp.At, sp.LastCredit = at, at
}

// AccountBurn is the account's decayed burn for w in units per hour; ok is
// false when the account was never observed.
func (p *Pace) AccountBurn(account string, w Window, now time.Time) (float64, bool) {
	ap := p.Accounts[account]
	if ap == nil {
		return 0, false
	}
	if w == Win7d {
		return ap.W7.rate(now, tau7d)
	}
	return ap.W5.rate(now, tau5h)
}

// SessionBurn is the session's rate in units per hour. measured is false for
// an unknown session or one less than 15 minutes past its first credit; the
// caller then uses Prior.
func (p *Pace) SessionBurn(sid string, now time.Time) (float64, bool) {
	sp := p.Sessions[sid]
	if sp == nil || now.Sub(sp.First) < measuredAfter {
		return 0, false
	}
	return rate(sp.Rise, sp.Wall, sp.At, now, tauSession), true
}

// Prior is the median rate of the measured live sessions credited in the
// last 30 minutes, floored at MinSessionBurn; DefaultSessionBurn with none.
func (p *Pace) Prior(live func(sid string) bool, now time.Time) float64 {
	var rs []float64
	for sid, sp := range p.Sessions {
		if !live(sid) || now.Sub(sp.LastCredit) > activeWithin {
			continue
		}
		if r, ok := p.SessionBurn(sid, now); ok {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return DefaultSessionBurn
	}
	sort.Float64s(rs)
	med := rs[len(rs)/2]
	if len(rs)%2 == 0 {
		med = (rs[len(rs)/2-1] + rs[len(rs)/2]) / 2
	}
	return max(med, MinSessionBurn)
}

// Prune drops sessions that are not live.
func (p *Pace) Prune(live func(sid string) bool) {
	for sid := range p.Sessions {
		if !live(sid) {
			delete(p.Sessions, sid)
		}
	}
}

// PaceInput is one placement or move question.
type PaceInput struct {
	Accounts []Account
	// Sessions maps an account name to its live placed session ids.
	Sessions map[string][]string
	Pace     *Pace
	// Subject is the session being placed or moved ("" if unknown);
	// SubjectFrom is its current account for a move, "" for a new one.
	Subject     string
	SubjectFrom string
	Pin         string
	Exclude     map[string]bool
	Params      Params
	Now         time.Time
}

type paceEval struct {
	name    string
	idx     int
	ratio   float64 // the score
	binding Window
	ttl5    time.Duration // unbounded as a huge value
	tBind   time.Duration // to the binding window's reset
	t5      time.Duration // hours to the 5h reset (a full window when refilled or unknown)
	left    float64       // 5h% left (to 100)
	hasRst  bool
	nSess   int
}

func (in PaceInput) pace() *Pace {
	if in.Pace == nil {
		return &Pace{}
	}
	return in.Pace
}

func (in PaceInput) live() func(string) bool {
	set := map[string]bool{}
	for _, l := range in.Sessions {
		for _, s := range l {
			set[s] = true
		}
	}
	if in.Subject != "" {
		set[in.Subject] = true
	}
	return func(s string) bool { return set[s] }
}

func (in PaceInput) eval(i int, prior float64) paceEval {
	a, p := in.Accounts[i], in.pace()
	rateOf := func(sid string) float64 {
		if r, ok := p.SessionBurn(sid, in.Now); ok {
			return r
		}
		return prior
	}
	var sum float64
	var n int
	for _, sid := range in.Sessions[a.Name] {
		if sid == in.Subject && in.Subject != "" {
			continue // counted once, below
		}
		sum += rateOf(sid)
		n++
	}
	var bs float64
	if in.Subject != "" {
		bs = rateOf(in.Subject)
	} else {
		bs = prior
	}
	m5, _ := p.AccountBurn(a.Name, Win5h, in.Now)
	m7, _ := p.AccountBurn(a.Name, Win7d, in.Now)
	// The weekly factor uses only the account's measured 7d burn (a
	// wall-time rate); session rates never feed it. Unmeasured: neutral.
	measured7 := m7 > 0
	bs7 := bs / WeekScale
	if m5 > 0 && m7 > 0 {
		bs7 = bs * m7 / m5
	}
	if a.Name == in.SubjectFrom && in.SubjectFrom != "" {
		// the measurements already carry the subject
		m5 = max(m5-bs, 0)
		m7 = max(m7-bs7, 0)
	}
	b5 := max(m5, sum)
	b7 := m7
	ev := paceEval{name: a.Name, idx: i, binding: Win5h, nSess: n}
	var r5, r7 float64
	for _, w := range []Window{Win5h, Win7d} {
		full := w.Length()
		used, has := a.effective(w, in.Now)
		T := full
		if r := a.reset(w); has && !r.IsZero() && r.After(in.Now) {
			T = r.Sub(in.Now)
		}
		if !has {
			used = 0
		}
		point := float64(in.Params.SwitchPoint(w, a.Tier))
		room := max(point-used, 0) * a.units()
		B, b := b5, bs
		if w == Win7d {
			B, b = b7, bs7
		}
		ttlPre, ttl := math.Inf(1), math.Inf(1) // hours; neutral when nothing is measured
		if (w == Win5h || measured7) && B+b > 0 {
			ttlPre = room / (B + b)
			ttl = ttlPre
		}
		T = max(T, time.Minute)
		if w == Win5h && ttlPre >= T.Hours() && !math.IsInf(ttlPre, 1) {
			// it reaches its reset first, then has a whole window (spec §10)
			ttl = T.Hours() + point*a.units()/(B+b)
		}
		r := ttl / T.Hours()
		if w == Win5h {
			r = ttl / full.Hours()
			ev.t5 = T
			if math.IsInf(ttlPre, 1) || ttlPre > 1000 {
				ev.ttl5 = 1000 * time.Hour
			} else {
				ev.ttl5 = time.Duration(ttlPre * float64(time.Hour))
			}
		}
		if w == Win5h {
			r5 = r
		} else {
			r7 = r
		}
		// the 5h window describes the account unless the weekly one lowers the score
		if w == Win5h || r7 < 1 {
			ev.binding = w
			ev.left = 100 - used
			ev.hasRst = has && !a.reset(w).IsZero() && a.reset(w).After(in.Now)
			ev.tBind = T
		}
	}
	// the 5h ratio ranks; the weekly one only penalises (spec §4)
	ev.ratio = min(r5, RCap) * min(r7, 1)
	return ev
}

// PaceRatio is the score of account for the subject,
// min(r5h, RCap) * min(r7d, 1), and the window shown in the reason: 5h, or
// 7d when the weekly factor is below 1 and lowered the score. An unknown account gives 0, "".
func PaceRatio(in PaceInput, account string) (float64, Window) {
	prior := in.pace().Prior(in.live(), in.Now)
	for i, a := range in.Accounts {
		if a.Name == account {
			ev := in.eval(i, prior)
			return ev.ratio, ev.binding
		}
	}
	return 0, ""
}

// PlacePace picks the account for a new or moved session (spec §4): a pin
// that names a candidate wins; accounts about to hit a wall are passed over
// unless all are; the highest score wins; a tie goes to the
// soonest 5h reset, then fewer live sessions, then registration order. reason
// is the decision-line reason.
func PlacePace(in PaceInput) (name, reason string, ok bool) {
	prior := in.pace().Prior(in.live(), in.Now)
	var evs []paceEval
	for i, a := range in.Accounts {
		if in.Exclude[a.Name] || !Candidate(a, in.Params, in.Now) {
			continue
		}
		if in.Pin != "" && a.Name == in.Pin {
			return a.Name, "pinned", true
		}
		evs = append(evs, in.eval(i, prior))
	}
	if len(evs) == 0 {
		return "", "", false
	}
	var keep []paceEval
	for _, e := range evs {
		// imminent wall: 5h ttl < 15m while the 5h reset is more than 15m away
		if e.ttl5 < imminentWall && e.t5 > imminentWall {
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) > 0 {
		evs = keep
	}
	best := evs[0].ratio
	for _, e := range evs {
		best = max(best, e.ratio)
	}
	var tied []paceEval
	for _, e := range evs {
		if e.ratio >= best-tieEps {
			tied = append(tied, e)
		}
	}
	win, why := tied[0], ""
	for _, e := range tied[1:] {
		switch {
		case e.t5 != win.t5:
			if e.t5 < win.t5 {
				win = e
			}
			why = "soonest reset"
		case e.nSess != win.nSess:
			if e.nSess < win.nSess {
				win = e
			}
			if why == "" {
				why = "fewest sessions"
			}
		}
	}
	if len(tied) > 1 && why != "" {
		return win.name, why, true
	}
	r := "5h"
	if win.binding == Win7d {
		r = "7d"
	}
	reason = r + " " + pctText(win.left) + " left"
	if win.hasRst {
		reason += ", resets in " + shortDur(win.tBind)
	}
	return win.name, reason, true
}

func pctText(p float64) string { return strconv.Itoa(int(math.Round(p))) + "%" }
