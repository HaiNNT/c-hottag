// Package usagelog is the usage ledger under $CHOTTAG_HOME/usage: raw
// per-message token events by month, daily rollups kept forever, and
// utilization snapshots. Only ids, model names and token counts are stored.
package usagelog

import "time"

// RawRetention is how long raw events are kept before Compact rolls them
// into daily.jsonl.
const RawRetention = 90 * 24 * time.Hour

// loc is the zone for month files and Day keys. Tests override it.
var loc = time.Local

// Event is one assistant message's token usage.
type Event struct {
	T     time.Time `json:"t"`
	MID   string    `json:"mid,omitempty"`
	RID   string    `json:"rid,omitempty"`
	Src   string    `json:"src"`            // "live" | "slot" | "home"
	Acct  string    `json:"acct,omitempty"` // "" = unknown
	Model string    `json:"model"`
	Speed string    `json:"speed,omitempty"`
	In    int64     `json:"in,omitempty"`
	Out   int64     `json:"out,omitempty"`
	CR    int64     `json:"cr,omitempty"`
	CW5m  int64     `json:"cw5m,omitempty"`
	CW1h  int64     `json:"cw1h,omitempty"`
	Web   int64     `json:"web,omitempty"`
}

// Daily is one rollup row per (Day, Acct, Model, Src, Speed).
type Daily struct {
	Day   string `json:"day"` // "2006-01-02", local time
	Acct  string `json:"acct,omitempty"`
	Model string `json:"model,omitempty"`
	Src   string `json:"src,omitempty"`
	Speed string `json:"speed,omitempty"`
	N     int64  `json:"n"` // messages
	In    int64  `json:"in,omitempty"`
	Out   int64  `json:"out,omitempty"`
	CR    int64  `json:"cr,omitempty"`
	CW5m  int64  `json:"cw5m,omitempty"`
	CW1h  int64  `json:"cw1h,omitempty"`
	Web   int64  `json:"web,omitempty"`
}

// Snapshot is a utilization sample for one account.
type Snapshot struct {
	T     time.Time `json:"t"`
	Acct  string    `json:"acct"`
	Tier  string    `json:"tier,omitempty"`
	Units float64   `json:"units,omitempty"`
	P5    *float64  `json:"p5,omitempty"`
	P7    *float64  `json:"p7,omitempty"`
	R7    int64     `json:"r7,omitempty"` // 7d reset, unix seconds
}

// FileMark records how far a transcript file was imported.
type FileMark struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Offset  int64     `json:"offset"`
}
