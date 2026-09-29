package cli

// installRecord is ${CHOTTAG_HOME}/install.json's shape (spec §2.2, §2.3):
// install.sh writes it atomically, mode 0600, after a successful install,
// and `chottag update` (update.go) reads it to learn which repo an install
// came from, then rewrites it after installing a new version.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

type installRecord struct {
	Repo        string    `json:"repo"`
	Version     string    `json:"version"`
	Source      string    `json:"source"` // "release" or "build"
	InstalledAt time.Time `json:"installedAt"`
}

// installRecordFile is install.json's name, directly under CHOTTAG_HOME.
const installRecordFile = "install.json"

// defaultRepo is the repo `update` falls back to when neither --repo nor
// install.json names one: install.sh's own REPO. TestDefaultRepoMatchesInstallSh
// reads install.sh itself so the two can never drift apart, and
// test/consistency pins both to HaiNNT/c-hottag (part 2, F226).
const defaultRepo = "HaiNNT/c-hottag"

// validRepoPattern is spec §2.2's --repo shape: an owner and a name, each
// restricted to the characters GitHub allows there.
var validRepoPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// validRepo reports whether s looks like OWNER/NAME.
func validRepo(s string) bool { return validRepoPattern.MatchString(s) }

// readInstallRecord reads h's install.json. ok is false, with a nil error,
// when the file does not exist: a chottag home from before install.sh
// started writing one, or one `update` has never touched.
func readInstallRecord(h string) (installRecord, bool, error) {
	b, err := os.ReadFile(filepath.Join(h, installRecordFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return installRecord{}, false, nil
		}
		return installRecord{}, false, err
	}
	var rec installRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return installRecord{}, false, err
	}
	return rec, true, nil
}

// writeInstallRecord writes rec to h's install.json atomically, mode 0600,
// the same mode install.sh writes it with (it names nothing secret, but
// there is no reason to leave it group/world readable either).
func writeInstallRecord(h string, rec installRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(h, installRecordFile), b, 0o600)
}
