package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallRecordRoundTrip(t *testing.T) {
	h := t.TempDir()
	want := installRecord{
		Repo:        "alice/c-hottag-fork",
		Version:     "0.3.0",
		Source:      "release",
		InstalledAt: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC),
	}
	if err := writeInstallRecord(h, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readInstallRecord(h)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true after a write")
	}
	if got != want {
		t.Errorf("readInstallRecord = %+v, want %+v", got, want)
	}
}

func TestInstallRecordAbsentIsOkFalseNilError(t *testing.T) {
	h := t.TempDir()
	rec, ok, err := readInstallRecord(h)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false for a home with no install.json (rec=%+v)", rec)
	}
}

func TestInstallRecordCorruptIsAnError(t *testing.T) {
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, installRecordFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readInstallRecord(h); err == nil {
		t.Error("err = nil, want an error for corrupt JSON")
	}
}

func TestWriteInstallRecordModeIs0600(t *testing.T) {
	h := t.TempDir()
	if err := writeInstallRecord(h, installRecord{Repo: defaultRepo, Version: "0.3.0", Source: "build"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(h, installRecordFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 0600", got)
	}
}

func TestValidRepo(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"HaiNNT/c-hottag", true},
		{"a-b/c.d_e", true},
		{"bad", false},
		{"a/b/c", false},
		{"a/b;x", false},
		{"/b", false},
		{"a/", false},
		{"", false},
	}
	for _, c := range cases {
		if got := validRepo(c.s); got != c.want {
			t.Errorf("validRepo(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestDefaultRepoMatchesInstallSh reads install.sh's REPO= line the same
// way test/consistency's shellVar does, so the two constants can never
// silently drift apart (part 0 keeps them equal; part 2's rename changes
// both at once).
func TestDefaultRepoMatchesInstallSh(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var repo string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "REPO="); ok {
			repo = v
			break
		}
	}
	if repo == "" {
		t.Fatal("install.sh has no REPO= line")
	}
	if repo != defaultRepo {
		t.Errorf("install.sh REPO=%q, want it to match defaultRepo %q", repo, defaultRepo)
	}
}
