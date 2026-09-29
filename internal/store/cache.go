package store

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Cache serves state.json to per-request callers, re-reading it only when
// the file's modification time or size changes. A `chottag tag` in another
// process is picked up by the next request; an in-flight request keeps the
// state it started with.
type Cache struct {
	store Store

	mu     sync.Mutex
	loaded bool
	state  State
	err    error
	mod    time.Time
	size   int64
	loads  int
}

func NewCache(s Store) *Cache { return &Cache{store: s} }

// Invalidate drops the cached copy, so the next State call re-reads the file.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loaded = false
}

// State returns the current state, reloading it if the file changed.
func (c *Cache) State() (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mod, size, exists := c.stat()
	if c.loaded && exists == !c.mod.IsZero() && mod.Equal(c.mod) && size == c.size {
		return c.state, c.err
	}
	c.state, c.err = c.store.Load()
	c.loads++
	c.loaded = true
	c.mod, c.size = mod, size
	return c.state, c.err
}

// stat returns state.json's modification time and size; exists is false (and
// mod zero) when the file is absent, which is a valid state (Default()).
func (c *Cache) stat() (mod time.Time, size int64, exists bool) {
	fi, err := os.Stat(c.store.path())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// An unreadable directory looks like a change, so the next
			// State call surfaces the real error from Load.
			return time.Time{}, -1, false
		}
		return time.Time{}, 0, false
	}
	return fi.ModTime(), fi.Size(), true
}
