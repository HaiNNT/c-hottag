package cli

// The update notice's shared record (R139). The notice for a new version is
// posted once, by whichever check finds the version first: the daemon's, or
// `chottag update --check`. Both processes decide through one small file,
// run/update-notified, holding the last version a notice was given (or due)
// for. A claim is a compare-and-set under an flock on run/update-notified.lock:
// the claimer that changes the file's value posts, every other check, in this
// process or another, reads its own version back and posts nothing. The file
// is written atomically, so a crash never leaves half a version.
//
// status.json's update.notified stays as the user-visible copy (and the seed
// when the file does not exist yet, so an upgrade does not repeat a notice
// the old daemon already gave). status.json is not the store: the daemon's
// sink rewrites the whole file from its own copy, and a CLI write there could
// be lost.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/notify"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

const updateNotifiedName = "update-notified"

// claimUpdateNotice records version as noticed and reports whether this call
// is the one that changed the record, i.e. whether the caller must post the
// notice. seed is the value to assume when the file does not exist yet.
// Safe against a concurrent claim from any process.
func claimUpdateNotice(h, version, seed string) (bool, error) {
	dir := filepath.Join(h, "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	unlock, err := fsutil.Lock(filepath.Join(dir, updateNotifiedName+".lock"))
	if err != nil {
		return false, err
	}
	defer unlock()
	path := filepath.Join(dir, updateNotifiedName)
	cur := seed
	switch b, err := os.ReadFile(path); {
	case err == nil:
		cur = strings.TrimSpace(string(b))
	case !os.IsNotExist(err):
		return false, err
	}
	if cur == version || seed == version {
		// Already given: by a claim, or by an older daemon that kept it only
		// in status.json (the seed). Never adds a notice, only suppresses.
		if cur != version {
			if err := fsutil.WriteFileAtomic(path, []byte(version+"\n"), 0o600); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if err := fsutil.WriteFileAtomic(path, []byte(version+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// updateNoticeText is the one notice every check posts.
func updateNoticeText(version string) (title, body string) {
	return "chottag: update available", fmt.Sprintf("chottag %s is available. Run: chottag update", version)
}

// noticeUpdateFromCLI posts the availability notice from `update --check`
// when it is the first check to see rel: through the same notifier, notify
// switch and label as the daemon's. A suppressed notice (notify off) still
// counts as given. Failures are warnings: a notice never fails a check.
func noticeUpdateFromCLI(h string, rel updatecheck.Release, r *reporter) {
	inst, _ := installedVersion(h)
	if !releaseAvailable(rel, Version, inst) {
		return
	}
	seed := ""
	if f, err := status.Load(status.Path(h)); err == nil && f.Update != nil {
		seed = f.Update.Notified
	}
	claimed, err := claimUpdateNotice(h, rel.Version, seed)
	if err != nil {
		r.Warn(warnUpdateCache, "chottag: could not record the update notice: "+err.Error())
		return
	}
	recordNotified(h, rel.Version, r)
	if !claimed {
		return
	}
	st, err := (store.Store{Dir: h}).Load()
	if err == nil && !st.NotifyOn() {
		return
	}
	label := ""
	if err == nil {
		label = st.Label
	}
	title, body := updateNoticeText(rel.Version)
	ctx, cancel := context.WithTimeout(context.Background(), notify.SendTimeout)
	defer cancel()
	if err := newDaemonNotifier().Send(ctx, labelTitle(title, label), body); err != nil {
		r.Warn(warnUpdateCache, "chottag: could not post the update notice: "+err.Error())
	}
}

// recordNotified shows version as notified in status.json's update, so
// `chottag status` is true at once rather than after the daemon's next check
// (the daemon's sink adopts it: mergeCLIUpdateLocked). Display and seed only;
// run/update-notified stays the record that decides.
func recordNotified(h, version string, r *reporter) {
	path := status.Path(h)
	f, err := status.Load(path)
	if err != nil {
		r.Warn(warnUpdateCache, "chottag: could not read status.json to record the notice: "+err.Error())
		return
	}
	if f.Update == nil || f.Update.Notified == version {
		return
	}
	f.Update.Notified = version
	b, err := status.Marshal(f)
	if err == nil {
		err = status.WriteBytes(path, b)
	}
	if err != nil {
		r.Warn(warnUpdateCache, "chottag: could not record the notice in status.json: "+err.Error())
	}
}
