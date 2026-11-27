// Package usagestats aggregates the usage ledger into the report behind
// `chottag stats`: totals, per-account value, weekly peaks and verdicts
// (R203). It is pure: no files, no clock, no network.
package usagestats

import (
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// AccountInfo is one registered account as the caller knows it. Units 0 means
// the tier's default capacity; Price is used only when PriceSet (Price 0 and
// not PriceSet means the list price).
type AccountInfo struct {
	Name, Tier string
	Units      float64
	Price      float64
	PriceSet   bool
	Rotates    bool
}

// Input is everything Build needs. Days and Snaps are the whole ledger:
// Since/Until select the reported period, the weekly windows look wider.
type Input struct {
	Days              []usagelog.Daily
	Snaps             []usagelog.Snapshot
	Accounts          []AccountInfo
	Now, Since, Until time.Time
	By                string // "day" or "month" (default month)
	Weeks             int    // complete 7d windows per verdict; default 4
	Account           string // filter: only this account's row and verdict
}

// Tok is a token and dollar total. Total is in+out+cr+cw5m+cw1h.
type Tok struct {
	In         int64   `json:"in"`
	Out        int64   `json:"out"`
	CR         int64   `json:"cr"`
	CW5m       int64   `json:"cw5m"`
	CW1h       int64   `json:"cw1h"`
	Web        int64   `json:"web"`
	Total      int64   `json:"total"`
	USD        float64 `json:"usd"`
	USDUnknown bool    `json:"usdUnknown"`
	Estimated  bool    `json:"estimated"`
}

// Period is one day or month of the time series.
type Period struct {
	Key     string         `json:"key"`
	Tok     Tok            `json:"tok"`
	ByModel map[string]Tok `json:"byModel"`
}

// Week is one complete 7d window of an account.
type Week struct {
	Reset     time.Time `json:"reset"`
	PeakPct   float64   `json:"peakPct"`
	Limited   bool      `json:"limited"`
	Units     float64   `json:"units"` // capacity of the plan the window ran on
	USD       float64   `json:"usd"`
	USDPerPct *float64  `json:"usdPerPct"`
}

// AccountRow is one account's line of the report.
type AccountRow struct {
	Name        string   `json:"name"`
	Tier        string   `json:"tier"`
	Units       float64  `json:"units"`
	Price       *float64 `json:"price"`
	PriceSource string   `json:"priceSource"` // "override", "list" or ""
	Tok         Tok      `json:"tok"`
	ValueRatio  *float64 `json:"valueRatio"`
	Weeks       []Week   `json:"weeks"`
	AvgPeak     *float64 `json:"avgPeak"`
	USDPerPct   *float64 `json:"usdPerPct"`
	// USDPerUnitPct is USDPerPct per unit of capacity (what 1% of a Pro
	// costs), so tiers compare.
	USDPerUnitPct *float64 `json:"usdPerUnitPct"`
	// From is the first day the price is pro-rated over: the later of the
	// report's Since and the account's first evidence day (YYYY-MM-DD).
	From string `json:"from"`
}

// Verdict is the advice for one account. Kind is keep, upgrade, downgrade,
// cancel, insufficient or none.
type Verdict struct {
	Account string             `json:"account"`
	Kind    string             `json:"kind"`
	Text    string             `json:"text"`
	Basis   map[string]float64 `json:"basis"`
}

// Overall is the fleet-wide advice. Kind is keep, change or insufficient.
type Overall struct {
	PayUSD     float64        `json:"payUSD"`
	Units      float64        `json:"units"`
	PeakDemand float64        `json:"peakDemand"`
	Suggest    map[string]int `json:"suggest"`
	SuggestUSD float64        `json:"suggestUSD"`
	Changes    []string       `json:"changes"`
	Kind       string         `json:"kind"`
	Text       string         `json:"text"`
}

// Report is the whole result; `stats --json` prints it.
type Report struct {
	Since        time.Time          `json:"since"`
	Until        time.Time          `json:"until"`
	By           string             `json:"by"`
	Total        Tok                `json:"total"`
	ByModel      map[string]Tok     `json:"byModel"`
	Unattributed Tok                `json:"unattributed"`
	Periods      []Period           `json:"periods"`
	Accounts     []AccountRow       `json:"accounts"`
	Verdicts     []Verdict          `json:"verdicts"`
	Overall      Overall            `json:"overall"`
	Thresholds   map[string]float64 `json:"thresholds"`
	Notes        []string           `json:"notes"`
}
