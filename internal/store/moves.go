package store

import "time"

// Move is a request to put a running session on a chosen account (R191). The
// daemon's spread engine honours it and records the account it placed.
type Move struct {
	Account string    `json:"account"`
	Dir     string    `json:"dir,omitempty"`
	At      time.Time `json:"at"`
}

const moveTTL = 7 * 24 * time.Hour

// SetMove records m for session sid (the full sid).
func (st *State) SetMove(sid string, m Move) {
	if st.Moves == nil {
		st.Moves = map[string]Move{}
	}
	st.Moves[sid] = m
}

// PruneMoves drops moves of sessions that are no longer live and moves older
// than seven days. Moves is nil afterwards when nothing is left.
func (st *State) PruneMoves(live func(sid string) bool, now time.Time) {
	for sid, m := range st.Moves {
		if !live(sid) || now.Sub(m.At) > moveTTL {
			delete(st.Moves, sid)
		}
	}
	if len(st.Moves) == 0 {
		st.Moves = nil
	}
}
