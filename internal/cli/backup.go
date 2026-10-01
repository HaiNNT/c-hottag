package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// backupsKept is how many backups of one name backupFile leaves in place.
const backupsKept = 5

// backupStamp is the UTC timestamp suffix of a backup file name.
const backupStamp = "20060102T150405Z"

var backupStampRE = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

// backupNow is backupFile's clock; tests pin it.
var backupNow = time.Now

// backupPruneError is what backupFile returns, with the new backup's path,
// when the copy landed but an old backup could not be removed. The data is
// already safe, so callers warn instead of failing (errors.As finds it).
type backupPruneError struct{ err error }

func (e *backupPruneError) Error() string {
	return "could not remove an old backup: " + e.err.Error()
}
func (e *backupPruneError) Unwrap() error { return e.err }

// backupFile copies src to <home>/backups/<name>.<UTC stamp> (the directory
// 0700, the copy 0600: a rc file or state.json may name secrets) and then
// prunes so only the newest backupsKept backups of that name remain. A
// missing src is not an error: there is nothing to lose, so it returns "",
// nil and creates nothing. When only the prune fails it returns the new
// path with a *backupPruneError.
func backupFile(home, src, name string) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	dir := filepath.Join(home, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name+"."+backupNow().UTC().Format(backupStamp))
	if err := fsutil.WriteFileAtomic(dest, data, 0o600); err != nil {
		return "", err
	}
	if err := pruneBackups(dir, name); err != nil {
		return dest, &backupPruneError{err}
	}
	return dest, nil
}

// pruneBackups removes all but the newest backupsKept files named
// <name>.<stamp> in dir. A file that merely shares the prefix (its
// remainder is not a bare stamp) is never touched.
func pruneBackups(dir, name string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var mine []string
	for _, e := range ents {
		n := e.Name()
		if len(n) > len(name)+1 && n[:len(name)+1] == name+"." && backupStampRE.MatchString(n[len(name)+1:]) {
			mine = append(mine, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(mine))) // stamps sort chronologically
	for i := backupsKept; i < len(mine); i++ {
		if err := os.Remove(filepath.Join(dir, mine[i])); err != nil {
			return err
		}
	}
	return nil
}
