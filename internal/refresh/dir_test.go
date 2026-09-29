package refresh_test

// L5: refresh's child runs in the slot dir, not wherever the daemon happens
// to be running from, so a relative path a login flow writes (there is none
// today, but a future one might) lands in the slot rather than a stray cwd.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/refresh"
)

func TestRefreshRunsInTheSlotDir(t *testing.T) {
	slot, dir := t.TempDir(), t.TempDir()
	out := filepath.Join(dir, "pwd.txt")
	script := filepath.Join(dir, "claude")
	os.WriteFile(script, []byte("#!/bin/sh\npwd -P > "+out+"\n"), 0o755)
	if err := (refresh.Claude{Bin: script}).Refresh(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	want, _ := filepath.EvalSymlinks(slot)
	if strings.TrimSpace(string(b)) != want {
		t.Fatalf("refresh ran in %q, want the slot %q (L5)", b, want)
	}
}
