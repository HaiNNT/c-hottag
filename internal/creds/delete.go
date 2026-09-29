package creds

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Deleter removes the credential a slot holds. It mirrors Reader field for
// field — the same GOOS/Run/ServiceOverride seam — so a test never touches
// the real Keychain and the two halves of the credential boundary read the
// same way.
type Deleter struct {
	GOOS            string
	Run             Runner
	ServiceOverride string
}

// maxKeychainDeletes bounds the "delete until gone" loop in Delete.
// security(1)'s delete-generic-password removes at most one matching item
// per call and exits 0 on success, so a service name shared by more than
// one item needs more than one call to actually clear it. The bound exists
// only so a Runner that never reports "not found" (a test double, or some
// Keychain state this package has not seen) makes Delete return an error
// instead of looping forever; 16 is far more than any real slot should ever
// accumulate.
const maxKeychainDeletes = 16

// Delete removes the login stored for configDir.
//
// On darwin the credential is a Keychain item keyed by the absolute slot
// path, NOT a file inside configDir (§4.5), so removing the directory does
// not remove the login: uninstall --purge did exactly that and orphaned
// every item it claimed to destroy.
//
// It is idempotent — a credential that is already gone is success — because
// both callers (logout, uninstall --purge) may be finishing a half-done job.
// Errors never include store contents, same rule as Read.
func (d Deleter) Delete(configDir string) error {
	abs, err := filepath.Abs(configDir)
	if err != nil {
		return err
	}
	if d.GOOS == "darwin" {
		svc := d.ServiceOverride
		if svc == "" {
			svc = KeychainService(abs)
		}
		// delete-generic-password removes at most one match: keep calling
		// until the Keychain reports none remain, so a service name that
		// (against expectation) collides across two items never leaves one
		// of them alive while Delete reports success.
		for i := 0; i < maxKeychainDeletes; i++ {
			_, err := d.Run("/usr/bin/security", "delete-generic-password", "-s", svc)
			if err == nil {
				continue
			}
			var ec exitCoder
			if errors.As(err, &ec) && ec.ExitCode() == securityItemNotFound {
				return nil
			}
			// err is safe to include for the same reason as in Read:
			// exec.ExitError.Error() is just "exit status N".
			return fmt.Errorf("delete keychain item %q: %v: %w", svc, err, classifyKeychainFailure(err))
		}
		return fmt.Errorf("delete keychain item %q: still present after %d delete calls", svc, maxKeychainDeletes)
	}
	if err := os.Remove(filepath.Join(abs, ".credentials.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
