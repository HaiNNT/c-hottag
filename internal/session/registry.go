// Package session tracks the `claude` processes chottag launched.
//
// It was built for one decision (spec §4.6, superseded by §4.8): when the
// daemon's fixed port was already taken at start, chottag could pick a new
// one ONLY if no managed session was alive, since a running session's
// HTTPS_PROXY was fixed at exec and moving the port out from under it would
// point it at nothing. §4.8 replaced that guard: the daemon now binds
// state.json's port or fails outright, and never moves it, so there is
// nothing left to guard.
//
// Add is called by internal/shim (shim.go) immediately before it execs the
// real `claude`: because that exec replaces the shim's own process image,
// this same pid *becomes* the `claude` process, so registering right before
// the exec — rather than after, which would never run — is what makes the
// entry correct (spec §4.3 step 6).
//
// Remove has no caller by design, not because nothing has gotten around to
// it: Live() prunes dead entries when read instead, which is the cleanup
// this registry was built around, so there is no moment at which removing
// an entry eagerly would do anything Live() does not already do lazily
// (spec §4.3 step 6). Live() itself has no production reader yet — that is
// a later milestone's diagnostic or guard to write, once one exists to
// consult it.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type Session struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// Registry is a directory of one small file per managed session, named by
// PID. A directory rather than one shared file: each session writes only its
// own entry, so two sessions starting at once cannot lose each other's write,
// and a crashed session leaves one prunable file rather than a corrupt map.
type Registry struct{ dir string }

func Open(dir string) (*Registry, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Registry{dir: dir}, nil
}

func (r *Registry) path(pid int) string {
	return filepath.Join(r.dir, strconv.Itoa(pid)+".json")
}

func (r *Registry) Add(pid, port int) error {
	b, err := json.Marshal(Session{PID: pid, Port: port})
	if err != nil {
		return err
	}
	return os.WriteFile(r.path(pid), b, 0o600)
}

func (r *Registry) Remove(pid int) error {
	err := os.Remove(r.path(pid))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Live returns every managed session whose process still exists, pruning the
// entries of those that do not. Pruning on read is what keeps a crashed
// session from holding the port forever.
func (r *Registry) Live() ([]Session, error) {
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(r.dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			// Deliberately asymmetric with the unmarshal branch below, which
			// prunes: a file that fails to unmarshal is known-bad and will
			// never become readable, so pruning is the only way it ever
			// leaves. A file that fails to *read* is most likely a benign
			// race with a concurrent Remove (deleted between ReadDir and
			// ReadFile here), and pruning on a transient read error would
			// throw away a live session's entry — the dangerous direction
			// documented on alive() above. Skipping is the conservative
			// choice. Residual: an entry permanently unreadable for some
			// other reason stays invisible and un-pruned forever; no
			// reachable way to produce that case via this package's own
			// write path was found.
			continue
		}
		var s Session
		if err := json.Unmarshal(b, &s); err != nil || s.PID <= 0 {
			os.Remove(p) // unreadable entry: prune rather than keep forever
			continue
		}
		if !alive(s.PID) {
			os.Remove(p)
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// alive reports whether pid exists. Signal 0 performs the permission and
// existence checks without delivering anything.
//
// This collapses two different errors into "dead": ESRCH (no such process)
// and EPERM (the process exists but is owned by another user). That's safe
// here only because every PID in this registry is a `claude` process chottag
// itself launched, so it always runs as the same user and EPERM is never
// reachable. If this registry ever comes to hold a PID chottag did not
// launch, this line must start distinguishing ESRCH from EPERM.
//
// The two ways this can still be wrong fail in opposite directions. Calling
// a live process dead is the dangerous one: Live() would prune its entry and
// let the port it's still pointed at be handed to someone else. Calling a
// dead process live (PID reuse — inherent to any PID-based liveness check,
// not fixable here) is the safe one: chottag just refuses to reassign a port
// nobody holds, and the user sees an error instead of a broken session.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Describe renders the live sessions for an operator message.
func Describe(ss []Session) string {
	parts := make([]string, 0, len(ss))
	for _, s := range ss {
		parts = append(parts, fmt.Sprintf("pid %d on port %d", s.PID, s.Port))
	}
	return strings.Join(parts, ", ")
}
