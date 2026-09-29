package ca_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/ca"
)

func TestLoadReadsWhatLoadOrCreateWrote(t *testing.T) {
	dir := t.TempDir()
	made, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ca.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.CertPEM(), made.CertPEM()) {
		t.Fatal("Load returned a different CA")
	}
}

func TestLoadNeverCreatesAnything(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if _, err := ca.Load(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load created %s", dir)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Load(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("Load wrote %v", entries)
	}
}

func TestLoadRejectsAMismatchedPair(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		if _, err := ca.LoadOrCreate(d); err != nil {
			t.Fatal(err)
		}
	}
	key, err := os.ReadFile(filepath.Join(b, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "ca.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Load(a); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want a key/cert mismatch", err)
	}
}
