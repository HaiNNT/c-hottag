package creds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

// Delete must ask the Keychain to remove the item for THIS slot, by the same
// service name Read looks it up under. A wrong service name would silently
// leave the login in place while reporting success.
//
// The fake Run reports one item deleted, then "not found" — the delete loop
// (see TestDeleteRemovesAllMatchingKeychainItems) must call Run a second
// time to confirm none remain before it can report success.
func TestDeleteRemovesTheKeychainItemForTheSlot(t *testing.T) {
	dir := t.TempDir()
	var got []string
	calls := 0
	d := Deleter{GOOS: "darwin", Run: func(name string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			got = append([]string{name}, args...)
			return nil, nil
		}
		return nil, fakeExit(securityItemNotFound)
	}}
	if err := d.Delete(dir); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("Run called %d times, want 2 (delete, then confirm none remain)", calls)
	}
	abs, _ := filepath.Abs(dir)
	want := []string{"/usr/bin/security", "delete-generic-password", "-s", KeychainService(abs)}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}

// Same as above, but with a relative slot path: KeychainService must be
// keyed by the absolutised path (matching Read), not the raw argument, or a
// caller invoking Delete with a relative configDir would target the wrong
// Keychain item and silently leave the real login in place.
func TestDeleteAbsolutisesARelativeConfigDirBeforeComparing(t *testing.T) {
	root := t.TempDir()
	slot := filepath.Join(root, "slot")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	var got []string
	calls := 0
	d := Deleter{GOOS: "darwin", Run: func(name string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			got = append([]string{name}, args...)
			return nil, nil
		}
		return nil, fakeExit(securityItemNotFound)
	}}
	if err := d.Delete("slot"); err != nil {
		t.Fatal(err)
	}
	// Compute want the same way Delete must: filepath.Abs resolved against
	// the (possibly symlinked) working directory, not against slot's
	// pre-chdir spelling — this test is about relative-path absolutising,
	// not about symlink canonicalization.
	abs, err := filepath.Abs("slot")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/security", "delete-generic-password", "-s", KeychainService(abs)}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}

// Already gone is success: logout and uninstall --purge are both finishing a
// job that may have been half-done, and a second run must not fail.
func TestDeleteIsIdempotentWhenTheItemIsAlreadyGone(t *testing.T) {
	d := Deleter{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) {
		return nil, fakeExit(securityItemNotFound)
	}}
	if err := d.Delete(t.TempDir()); err != nil {
		t.Errorf("Delete() = %v, want nil: errSecItemNotFound means the login is already gone", err)
	}
}

// Any other Keychain failure must surface. Reporting success while the
// credential is still there is the failure mode that matters: the caller
// deletes the slot next and the login becomes unreachable but live.
func TestDeleteReportsANonNotFoundKeychainFailure(t *testing.T) {
	d := Deleter{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) {
		return nil, fakeExit(securityAuthFailed)
	}}
	err := d.Delete(t.TempDir())
	if !errors.Is(err, ErrKeychain) {
		t.Errorf("Delete() = %v, want an error wrapping ErrKeychain", err)
	}
}

// delete-generic-password removes at most one matching item per call. If two
// items ever share a service name, one call is not enough: Delete must keep
// calling until the Keychain reports "not found", or the second item
// survives while Delete reports success.
func TestDeleteRemovesAllMatchingKeychainItems(t *testing.T) {
	calls := 0
	d := Deleter{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, nil // an item was deleted; another may remain
		}
		return nil, fakeExit(securityItemNotFound)
	}}
	if err := d.Delete(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("Run called %d times, want 3 (two deletes, then confirm none remain)", calls)
	}
}

// A Runner that never reports "not found" must not hang Delete forever — the
// delete loop has to be bounded, and exhausting the bound is a failure to
// report, not a success: "I deleted N and there may be more" is not done.
func TestDeleteBoundsRepeatedDeletesAndReportsExhaustion(t *testing.T) {
	calls := 0
	d := Deleter{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) {
		calls++
		return nil, nil // always "another one gone" — never "not found"
	}}
	if err := d.Delete(t.TempDir()); err == nil {
		t.Fatal("Delete() = nil, want an error: the delete loop must not silently give up")
	}
	if calls == 0 {
		t.Fatal("Run was never called")
	}
}

// ServiceOverride must reach the Keychain call Delete makes, the same as it
// does for Reader (TestReaderDarwinOverrideAndMissing) — otherwise a test or
// caller that sets it believes it is targeting one service while Delete
// silently targets another.
func TestDeleterDarwinUsesServiceOverride(t *testing.T) {
	var svc string
	d := Deleter{GOOS: "darwin", ServiceOverride: "Custom", Run: func(name string, args ...string) ([]byte, error) {
		svc = args[2]
		return nil, fakeExit(securityItemNotFound)
	}}
	if err := d.Delete(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if svc != "Custom" {
		t.Fatalf("svc = %q, want %q", svc, "Custom")
	}
}

// Mirrors TestReaderDarwinFailureClassification: Delete must sort a
// security(1) failure into the same sentinel Read would for the identical
// underlying exit code, via the shared classifyKeychainFailure helper —
// not a narrower comparison that happens to pass today's fixed case list.
func TestDeleteFailureClassification(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr error // nil means Delete() must return nil (item already gone)
	}{
		{"itemNotFound already gone", fakeExit(securityItemNotFound), nil},
		{"authFailed denial", fakeExit(securityAuthFailed), ErrKeychain},
		{"userCanceled denial", fakeExit(securityUserCanceled), ErrKeychain},
		{"interactionNotAllowed transient", fakeExit(securityInteractionNotAllowed), ErrKeychainUnavailable},
		{"exec failure transient", errors.New("exec: security: not found"), ErrKeychainUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Deleter{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) { return nil, c.err }}
			err := d.Delete(t.TempDir())
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("Delete() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("run error %v -> %v, want wrapping %v", c.err, err, c.wantErr)
			}
			other := ErrKeychain
			if c.wantErr == ErrKeychain {
				other = ErrKeychainUnavailable
			}
			if errors.Is(err, other) {
				t.Fatalf("run error %v -> %v, must not also wrap %v", c.err, err, other)
			}
		})
	}
}

func TestDeleteRemovesTheCredentialFileOffDarwin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := Deleter{GOOS: "linux", Run: func(string, ...string) ([]byte, error) {
		t.Fatal("Run must not be called off darwin")
		return nil, nil
	}}
	if err := d.Delete(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("credential file still present after Delete: stat err = %v", err)
	}
	if err := d.Delete(dir); err != nil {
		t.Errorf("second Delete() = %v, want nil (idempotent)", err)
	}
}

// A removal that fails for a reason other than "not there" must surface,
// not be swallowed by the idempotency branch.
func TestDeleteReportsARemovalFailureOffDarwin(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory where the credential file should be: os.Remove
	// fails with ENOTEMPTY, which is not fs.ErrNotExist.
	inner := filepath.Join(dir, ".credentials.json")
	if err := os.MkdirAll(filepath.Join(inner, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := Deleter{GOOS: "linux", Run: func(string, ...string) ([]byte, error) { return nil, nil }}
	if err := d.Delete(dir); err == nil {
		t.Error("Delete() = nil, want an error: a removal failure must not be reported as success")
	}
}
