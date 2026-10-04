// Package stickyval remembers which account answered a Claude Code session's
// POST /api/oauth/validate, so the same account keeps answering it for the
// session's life (R160, F267). The map is persisted so a daemon restart keeps
// it. It holds session ids, account names and emails only, never a token.
package stickyval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

const (
	// MaxEntries bounds the map; the entries unused longest are dropped.
	MaxEntries = 500
	// MaxAge drops an entry its session has not used for this long.
	MaxAge = 7 * 24 * time.Hour
	// WarmWithin is how recently an entry must have been used for its account
	// to be kept fresh by the warm loop.
	WarmWithin = 24 * time.Hour
	// touchEvery is how stale a recorded last use may get before a hit
	// rewrites the file: a session validates rarely, but a hit must not cost
	// a disk sync each time.
	touchEvery = time.Hour

	fileVersion = 2
)

// Entry is what is recorded for one session.
type Entry struct {
	Account string    `json:"account"`
	Email   string    `json:"email,omitempty"` // the account's email when recorded, "" if it had none
	Used    time.Time `json:"used"`
}

type fileFormat struct {
	Version  int              `json:"version"`
	Sessions map[string]Entry `json:"sessions"`
	// Gone remembers the account of a session whose entry was dropped (aged
	// out or evicted), so a later pin to another account can be logged.
	Gone map[string]Entry `json:"gone,omitempty"`
}

// State says how a Lookup found a session.
type State int

const (
	Absent  State = iota // never recorded
	Live                 // recorded and within MaxAge
	Dropped              // recorded once, dropped for age or size
)

// Map is the sid -> account map. It is safe for concurrent use.
type Map struct {
	path string

	mu      sync.Mutex
	m       map[string]Entry
	gone    map[string]Entry
	seq     uint64 // bumped by every change
	dirty   bool   // the file may be behind the map: a write failed
	pending bool   // a retry is scheduled
	wmu     sync.Mutex
	tried   uint64 // the highest seq whose write was attempted, under wmu

	// Seams: the file write, and a one-shot timer. Tests replace them.
	writeFile func(path string, data []byte, perm os.FileMode) error
	after     func(d time.Duration, f func())
}

// retryAfter is how long a failed write waits before it is tried again when no
// validate comes sooner.
const retryAfter = time.Minute

// Open loads the map from path. A missing file is an empty map. An unreadable,
// malformed or wrong-version file returns an empty, usable Map together with
// the error, so the caller can report it and carry on.
func Open(path string) (*Map, error) {
	s := &Map{path: path, m: map[string]Entry{}, gone: map[string]Entry{},
		writeFile: fsutil.WriteFileAtomic,
		after:     func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("stickyval: %w", err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return s, fmt.Errorf("stickyval: %s is malformed: %w", filepath.Base(path), err)
	}
	if f.Version != fileVersion {
		return s, fmt.Errorf("stickyval: %s has version %d, want %d", filepath.Base(path), f.Version, fileVersion)
	}
	for sid, e := range f.Sessions {
		if sid != "" && e.Account != "" {
			s.m[sid] = e
		}
	}
	for sid, e := range f.Gone {
		if sid != "" && e.Account != "" {
			s.gone[sid] = e
		}
	}
	return s, nil
}

// Lookup is the entry for sid. Live carries it; Dropped carries the last
// known entry of a session whose record was dropped; Absent has none.
func (s *Map) Lookup(sid string, now time.Time) (Entry, State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[sid]; ok {
		if now.Sub(e.Used) <= MaxAge {
			return e, Live
		}
		return e, Dropped
	}
	if e, ok := s.gone[sid]; ok {
		return e, Dropped
	}
	return Entry{}, Absent
}

// Get is the account recorded for sid, false when none or when it has been
// unused for MaxAge as of now.
func (s *Map) Get(sid string, now time.Time) (string, bool) {
	e, st := s.Lookup(sid, now)
	return e.Account, st == Live
}

// Accounts lists the accounts of sessions used within d of now, each once.
func (s *Map) Accounts(now time.Time, d time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range s.m {
		if k := strings.ToLower(e.Account); now.Sub(e.Used) <= d && !seen[k] {
			seen[k] = true
			out = append(out, e.Account)
		}
	}
	sort.Strings(out)
	return out
}

// Set records account (and its email, "" if unknown) for sid as used now,
// drops expired entries, and keeps the newest MaxEntries. It writes the file,
// except when the entry is unchanged and was recorded less than an hour ago.
// The write happens outside the map's lock, so a Get never waits on a disk
// sync. The in-memory map is updated even when the write fails; the error is
// returned.
func (s *Map) Set(sid, account, email string, now time.Time) error {
	if sid == "" || account == "" {
		return errors.New("stickyval: empty session id or account")
	}
	s.mu.Lock()
	if e, ok := s.m[sid]; ok && e.Account == account && e.Email == email && !s.dirty && now.Sub(e.Used) < touchEvery && now.After(e.Used) {
		s.mu.Unlock()
		return nil
	}
	s.m[sid] = Entry{Account: account, Email: email, Used: now.UTC()}
	delete(s.gone, sid)
	s.pruneLocked(now)
	seq, data, err := s.snapshotLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.write(seq, data)
}

// snapshotLocked bumps the sequence and marshals the map.
func (s *Map) snapshotLocked() (uint64, []byte, error) {
	s.seq++
	data, err := json.Marshal(fileFormat{Version: fileVersion, Sessions: s.m, Gone: s.gone})
	return s.seq, data, err
}

// write puts the snapshot on disk unless a newer one was already attempted, so
// an older snapshot never lands after a newer one, failed or not. A failure
// marks the map dirty: the next Set writes again, and so does a timer within
// retryAfter, whichever comes first.
func (s *Map) write(seq uint64, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if seq < s.tried {
		return nil
	}
	s.tried = seq
	err := os.MkdirAll(filepath.Dir(s.path), 0o700)
	if err == nil {
		err = s.writeFile(s.path, data, 0o600)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.dirty = true
		if !s.pending {
			s.pending = true
			s.after(retryAfter, s.retry)
		}
		return err
	}
	if seq == s.seq {
		s.dirty = false
	}
	return nil
}

// retry writes the map again if a failed write left the file behind.
func (s *Map) retry() {
	s.mu.Lock()
	s.pending = false
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	seq, data, err := s.snapshotLocked()
	s.mu.Unlock()
	if err == nil {
		s.write(seq, data)
	}
}

// pruneLocked moves entries aged past MaxAge, and the oldest beyond
// MaxEntries, to the dropped list, which is itself bounded.
func (s *Map) pruneLocked(now time.Time) {
	for sid, e := range s.m {
		if now.Sub(e.Used) > MaxAge {
			s.gone[sid] = e
			delete(s.m, sid)
		}
	}
	if len(s.m) > MaxEntries {
		for _, sid := range oldest(s.m, len(s.m)-MaxEntries) {
			s.gone[sid] = s.m[sid]
			delete(s.m, sid)
		}
	}
	if len(s.gone) > MaxEntries {
		for _, sid := range oldest(s.gone, len(s.gone)-MaxEntries) {
			delete(s.gone, sid)
		}
	}
}

// oldest is the n sids of m used longest ago.
func oldest(m map[string]Entry, n int) []string {
	all := make([]string, 0, len(m))
	for sid := range m {
		all = append(all, sid)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := m[all[i]], m[all[j]]
		if !a.Used.Equal(b.Used) {
			return a.Used.Before(b.Used)
		}
		return all[i] < all[j]
	})
	return all[:n]
}

// Len is the number of live recorded sessions.
func (s *Map) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
