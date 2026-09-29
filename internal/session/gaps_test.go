package session_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/session"
)

// The registry dir is chottag's run/ dir, which daemon.lock shares
// (internal/daemonlock). Live reads only "<pid>.json" files, and must
// never prune anything else there.
func TestLiveLeavesFilesThatAreNotSessionEntriesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"daemon.lock", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "123.json"), 0o700); err != nil { // a dir with an entry's name
		t.Fatal(err)
	}
	if err := r.Add(os.Getpid(), 47821); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].PID != os.Getpid() {
		t.Fatalf("Live() = %+v, want only this process's entry", live)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"123.json", "daemon.lock", "notes.txt", strconv.Itoa(os.Getpid()) + ".json"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("run dir = %q after Live, want %q", names, want)
	}
}

// A corrupt entry, or one naming pid 0 or below, can never become valid:
// Live prunes it (the same as a dead session) and keeps the live ones.
func TestLivePrunesCorruptAndPidlessEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{"900001.json": "{not json", "900002.json": `{"pid":0,"port":1}`, "900003.json": `{"pid":-4,"port":1}`}
	for name, body := range bad {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Add(os.Getpid(), 47821); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].PID != os.Getpid() {
		t.Fatalf("Live() = %+v, want only this process's entry", live)
	}
	for name := range bad {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was not pruned (stat err %v)", name, err)
		}
	}
}

// An entry Live cannot read is skipped, never pruned: pruning on a
// transient read error would drop a live session (registry.go's
// asymmetry). chmod 000 stands in for the unreadable file.
func TestLiveSkipsAnUnreadableEntryWithoutPruningIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(os.Getpid(), 47821); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("Live() = %+v, want the unreadable entry skipped", live)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the unreadable entry was pruned: %v", err)
	}
}

// A run dir that is gone is an error, not "no sessions": `daemon stop`
// warns on the error (liveSessions) instead of stopping under a live
// session it could not see.
func TestLiveReportsAMissingRunDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if live, err := r.Live(); !errors.Is(err, fs.ErrNotExist) || live != nil {
		t.Fatalf("Live() = (%+v, %v), want (nil, fs.ErrNotExist)", live, err)
	}
}

func TestOpenFailsWhenTheRunDirIsAFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := session.Open(p); err == nil || r != nil {
		t.Fatalf("Open(file) = (%v, %v), want (nil, an error)", r, err)
	}
}

func TestRemoveOfAMissingEntryIsNotAnError(t *testing.T) {
	r, err := session.Open(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(os.Getpid()); err != nil {
		t.Fatalf("Remove(no entry) = %v, want nil", err)
	}
}

// Describe is what `daemon stop` prints when it refuses (daemon_stop.go).
func TestDescribe(t *testing.T) {
	for _, c := range []struct {
		in   []session.Session
		want string
	}{
		{nil, ""},
		{[]session.Session{{PID: 11, Port: 47821}}, "pid 11 on port 47821"},
		{[]session.Session{{PID: 11, Port: 1}, {PID: 12, Port: 2}}, "pid 11 on port 1, pid 12 on port 2"},
	} {
		if got := session.Describe(c.in); got != c.want {
			t.Errorf("Describe(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}
