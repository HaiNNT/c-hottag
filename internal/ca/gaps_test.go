package ca_test

import (
	"bytes"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/ca"
)

// Running sessions trust the CA on disk, so a CA that fails to parse is
// refused and left exactly as it is, never regenerated over. Each case
// damages one file of a good pair.
func TestLoadOrCreateRefusesADamagedCAAndLeavesItAlone(t *testing.T) {
	junkDER := func(typ string) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte("not der")}) }
	for _, c := range []struct {
		name, file string
		body       []byte
		wantErr    string
	}{
		{"cert is not PEM", "ca.pem", []byte("not pem"), "malformed"},
		{"key is not PEM", "ca.key", []byte("not pem"), "malformed"},
		{"cert DER does not parse", "ca.pem", junkDER("CERTIFICATE"), "x509"},
		{"key DER does not parse", "ca.key", junkDER("EC PRIVATE KEY"), "x509"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := ca.LoadOrCreate(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, c.file), c.body, 0o600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, f := range []string{"ca.pem", "ca.key"} {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				before[f] = b
			}
			for _, load := range []func(string) (*ca.Authority, error){ca.LoadOrCreate, ca.LoadOrCreateLocked, ca.Load} {
				if a, err := load(dir); err == nil || a != nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("load = (%v, %v), want (nil, an error naming %q)", a, err, c.wantErr)
				}
			}
			for f, want := range before {
				if got, err := os.ReadFile(filepath.Join(dir, f)); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s changed after a refused load (%v)", f, err)
				}
			}
		})
	}
}

// A CA dir that cannot be created is an error, with nothing written.
func TestLoadOrCreateLockedFailsWhenTheDirCannotBeMade(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "ca")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(string) (*ca.Authority, error){ca.LoadOrCreate, ca.LoadOrCreateLocked} {
		if a, err := load(filepath.Join(file, "sub")); err == nil || a != nil {
			t.Fatalf("load under a file = (%v, %v), want an error", a, err)
		}
	}
}

// First run in a directory that refuses new files: the key is written
// first, so its failure must leave no key, no cert and no temp file, and
// a later run, once the directory is writable, creates a whole CA
// instead of refusing a half-present one.
func TestLoadOrCreateLeavesNoHalfCAWhenTheFirstWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := ca.LoadOrCreate(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want fs.ErrPermission", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("a failed first write left %q", names)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatalf("LoadOrCreate once writable = %v, want a fresh CA", err)
	}
}
