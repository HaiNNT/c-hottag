package ca

import (
	"fmt"
	"os"
	"path/filepath"
)

// Load reads the CA in dir and never creates or writes anything. Doctor's
// detect step (M2 spec §2.2 row 3) uses it, because LoadOrCreate runs
// MkdirAll and, with both files missing, generates a new CA. A missing
// file's error wraps fs.ErrNotExist.
func Load(dir string) (*Authority, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	keyPEM, err := readKeyFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}
	return parse(certPEM, keyPEM)
}
