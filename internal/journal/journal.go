// Package journal keeps a durable, per-session record of the claude sessions
// the shim launched, so a crash or reboot can be recovered with a resume.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

const (
	MaxAge      = 7 * 24 * time.Hour
	MaxEntries  = 500
	BatchWindow = 2 * time.Minute
	// RecentWindow is how long after it ended a lost session still counts
	// for the default batch and the status hint: older ones are most likely
	// tabs the user closed on purpose.
	RecentWindow  = 24 * time.Hour
	OutcomeExited = "exited"
	OutcomeLost   = "lost"
)

// Entry is one launched session.
type Entry struct {
	PID           int       `json:"pid"`
	PPID          int       `json:"ppid"`
	Started       time.Time `json:"started"`
	SID           string    `json:"sid,omitempty"`
	Pool          string    `json:"pool,omitempty"`
	Dir           string    `json:"dir"`
	CmuxWorkspace string    `json:"cmuxWorkspace,omitempty"`
	CmuxSurface   string    `json:"cmuxSurface,omitempty"`
	ResumeOf      string    `json:"resumeOf,omitempty"`
	Fork          bool      `json:"fork,omitempty"`
	Native        string    `json:"native,omitempty"`
	Ended         time.Time `json:"ended,omitzero"`
	Outcome       string    `json:"outcome,omitempty"`
	Resumed       time.Time `json:"resumed,omitzero"`
}

// Key names the entry's file: pid and start second.
func (e Entry) Key() string { return fmt.Sprintf("%d-%d", e.PID, e.Started.Unix()) }

// Open reports whether the session has not ended.
func (e Entry) Open() bool { return e.Ended.IsZero() }

// Journal is a directory of entry files.
type Journal struct{ dir string }

var nameRE = regexp.MustCompile(`^[0-9]+-[0-9]+\.json$`)

// Open creates dir (0700) and returns the journal over it.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Journal{dir: dir}, nil
}

func (j *Journal) path(key string) string { return filepath.Join(j.dir, key+".json") }

func (j *Journal) lock() (func() error, error) {
	return fsutil.Lock(filepath.Join(j.dir, "journal.lock"))
}

func (j *Journal) write(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(j.path(e.Key()), b, 0o600)
}

func (j *Journal) listLocked() ([]Entry, error) {
	ents, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range ents {
		if d.IsDir() || !nameRE.MatchString(d.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(j.dir, d.Name()))
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Started.Equal(out[b].Started) {
			return out[a].Started.After(out[b].Started)
		}
		return out[a].PID > out[b].PID
	})
	return out, nil
}

// List returns the entries, newest Started first. Unreadable files are skipped.
func (j *Journal) List() ([]Entry, error) {
	unlock, err := j.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return j.listLocked()
}

// Put writes e, replacing any open entry of the same pid.
func (j *Journal) Put(e Entry) error {
	unlock, err := j.lock()
	if err != nil {
		return err
	}
	defer unlock()
	es, err := j.listLocked()
	if err != nil {
		return err
	}
	for _, o := range es {
		if o.Open() && o.PID == e.PID && o.Key() != e.Key() {
			if err := os.Remove(j.path(o.Key())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return j.write(e)
}

// Update applies fn to the entry with key and writes it when fn returns true.
// A missing key is not an error. The write always goes to the original key's
// file, even if fn changes PID or Started.
func (j *Journal) Update(key string, fn func(*Entry) bool) error {
	unlock, err := j.lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, err := os.ReadFile(j.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return err
	}
	if !fn(&e) {
		return nil
	}
	nb, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(j.path(key), nb, 0o600)
}

// Sweep ends every open entry whose claude pid is dead: exited when its shell
// is still alive, lost otherwise. An entry started before bootTime (the
// machine's boot, zero when unknown) is dead whatever alive says: its pid may
// have been reused since the reboot. It returns how many it ended.
func (j *Journal) Sweep(now, bootTime time.Time, alive func(pid int) bool) (int, error) {
	unlock, err := j.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	es, err := j.listLocked()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		beforeBoot := !bootTime.IsZero() && e.Started.Before(bootTime)
		if !e.Open() || (!beforeBoot && alive(e.PID)) {
			continue
		}
		e.Ended = now
		if !beforeBoot && e.PPID > 1 && alive(e.PPID) {
			e.Outcome = OutcomeExited
		} else {
			e.Outcome = OutcomeLost
		}
		if err := j.write(e); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SetNatives records the native session id on open entries, rewriting only
// those that change. It returns how many it rewrote.
func (j *Journal) SetNatives(bySID map[string]string) (int, error) {
	unlock, err := j.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	es, err := j.listLocked()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		if !e.Open() || e.SID == "" {
			continue
		}
		nat := bySID[e.SID]
		if !ValidNative(nat) || nat == e.Native {
			continue
		}
		e.Native = nat
		if err := j.write(e); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Prune deletes ended entries older than MaxAge, then the oldest beyond
// MaxEntries (open entries last).
func (j *Journal) Prune(now time.Time) error {
	unlock, err := j.lock()
	if err != nil {
		return err
	}
	defer unlock()
	es, err := j.listLocked()
	if err != nil {
		return err
	}
	remove := func(e Entry) error {
		if err := os.Remove(j.path(e.Key())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var keep []Entry
	for _, e := range es {
		if !e.Open() && now.Sub(e.Ended) > MaxAge {
			if err := remove(e); err != nil {
				return err
			}
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) <= MaxEntries {
		return nil
	}
	// Deletion order: ended before open, then oldest Started first.
	sort.SliceStable(keep, func(a, b int) bool {
		if keep[a].Open() != keep[b].Open() {
			return !keep[a].Open()
		}
		return keep[a].Started.Before(keep[b].Started)
	})
	for _, e := range keep[:len(keep)-MaxEntries] {
		if err := remove(e); err != nil {
			return err
		}
	}
	return nil
}
