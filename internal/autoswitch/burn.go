package autoswitch

import (
	"math"
	"strings"
	"time"
)

// The burn-rate average's shape (M4 spec §2).
const (
	// BurnActiveGap is the longest gap between two observations of one
	// account that still counts as active time. A longer gap is idle: it
	// adds neither time nor usage.
	BurnActiveGap = 5 * time.Minute
	// BurnDecay is the average's time constant, in active time: an
	// observation an hour of activity old weighs 1/e of a new one.
	BurnDecay = time.Hour
	// BurnMinActive is how much active time the average needs before Rate
	// reports it. Until then Rate is 0 and the planner uses DefaultBurn.
	BurnMinActive = 15 * time.Minute
)

// Burn is the burn-rate estimate: tier-normalised units per active hour,
// as a moving average over active minutes. The daemon feeds it the serving
// account's 5-hour utilization from each response. The zero value is ready
// to use. It is not safe for concurrent use; the caller serialises it.
type Burn struct {
	last  map[string]burnSample
	units float64 // decayed units burned
	hours float64 // decayed active hours
}

type burnSample struct {
	pct float64
	at  time.Time
}

// Observe records account's 5-hour utilization (0-100) at at. units is the
// account's capacity units. A rise over an active gap counts as burn; a
// fall is a reset and counts as active time with no burn.
func (b *Burn) Observe(account string, units, pct5h float64, at time.Time) {
	if math.IsNaN(units) || math.IsNaN(pct5h) {
		// A corrupt reading: ignore it outright, rather than storing it as
		// the reference point. A NaN pct5h kept as prev.pct would poison
		// every future delta (and so b.units) with NaN forever.
		return
	}
	if b.last == nil {
		b.last = map[string]burnSample{}
	}
	key := strings.ToLower(account)
	prev, ok := b.last[key]
	if !ok {
		b.last[key] = burnSample{pct: pct5h, at: at}
		return
	}
	gap := at.Sub(prev.at)
	if gap <= 0 {
		// An out-of-order sample: keep the newer prev as the reference
		// point rather than overwriting it with an older one.
		return
	}
	b.last[key] = burnSample{pct: pct5h, at: at}
	if gap > BurnActiveGap {
		return
	}
	decay := math.Exp(-gap.Hours() / BurnDecay.Hours())
	b.units = b.units*decay + max(0, pct5h-prev.pct)*units
	b.hours = b.hours*decay + gap.Hours()
}

// Rate is the average in units per active hour, or 0 while there is less
// than BurnMinActive of active time behind it.
func (b *Burn) Rate() float64 {
	if b.hours < BurnMinActive.Hours() {
		return 0
	}
	return b.units / b.hours
}
