package usagelog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// ImportState returns the per-file import marks; a missing file is empty.
func (l Ledger) ImportState() (map[string]FileMark, error) {
	b, err := os.ReadFile(filepath.Join(l.Dir, "import.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]FileMark{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]FileMark{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// SaveImportState replaces import.json atomically.
func (l Ledger) SaveImportState(m map[string]FileMark) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(l.Dir, "import.json"), b, 0o600)
}
