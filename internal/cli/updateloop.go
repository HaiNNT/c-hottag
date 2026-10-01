package cli

// The daemon's update loop (R124, spec §2, §3, §5): a daily check of the
// repo's latest release, one notification per new version, and, only when
// the user opted in, an install through the very same `chottag update` a
// person runs. The daemon never restarts itself here: the child runs with
// --no-restart on every path.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

const (
	// updateFirstCheck is how long after the daemon starts the first check runs.
	updateFirstCheck = 2 * time.Minute
	// updateInterval separates two checks, before the jitter.
	updateInterval = 24 * time.Hour
	// updateJitterMax bounds the random extra wait, so installs don't all
	// ask GitHub at once.
	updateJitterMax = time.Hour
	// updateSoak is how old a release must be before the daemon installs it,
	// and how long a tried version waits before it is tried again.
	updateSoak = 24 * time.Hour
	// updateChildLimit caps how much of the child's JSON is read.
	updateChildLimit = 1 << 20
	// noUpdateCheckEnv turns the daemon's check off when it is "1".
	noUpdateCheckEnv = "CHOTTAG_NO_UPDATE_CHECK"
)

// updateLoopClock is the loop's clock. TestMain installs a panicking
// default: a test sets its own.
var updateLoopClock = time.Now

// updateJitter is the random extra wait after a 24 h interval, in
// [0, updateJitterMax). TestMain installs a panicking default.
var updateJitter = func() time.Duration {
	return time.Duration(rand.Int64N(int64(updateJitterMax)))
}

// newUpdateTimer starts a one-shot timer, the way newRosterTicker starts a
// ticker: a test injects its own channel so no test waits on real time.
// TestMain installs a panicking default.
var newUpdateTimer = func(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// autoRun is what one auto-install child reported.
type autoRun struct {
	// OK is the child's `ok`, with a clean exit.
	OK bool
	// Installed is the child's `installed`: false with OK means it found the
	// version already in place and changed nothing.
	Installed bool
	// Reason is the failure's short reason: the child's error.code, or, for
	// the generic update_failed, its sanitized error.message.
	Reason string
	// Stderr is the last line the child wrote to stderr, sanitized and
	// capped. For the daemon log only.
	Stderr string
}

// autoInstalling is on while this daemon's own auto-install child runs: the
// restart loop never restarts the daemon in that window.
var autoInstalling atomic.Bool

// autoUpdateTimeout bounds one auto-install child.
var autoUpdateTimeout = 15 * time.Minute

// autoUpdateRun runs the install child and reports what it said. TestMain
// installs a panicking default.
var autoUpdateRun = runAutoUpdateChild

// installedVersion is the version of the chottag installed under home h, as
// opposed to the one running: the version directory <home>/bin/chottag
// resolves into (only when that directory sits directly under
// <home>/versions), falling back to install.json's. It errors when neither names a release. TestMain
// installs a panicking default.
var installedVersion = installedVersionOf

// linkedVersionOf is the version <home>/bin/chottag links into, when its
// target is <home>/versions/<version>/chottag with a version-shaped name.
func linkedVersionOf(h string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(h, "bin", "chottag"))
	if err != nil {
		return "", false
	}
	vdir := filepath.Dir(resolved)
	root, err := filepath.EvalSymlinks(filepath.Join(h, "versions"))
	if err != nil || filepath.Dir(vdir) != root {
		return "", false
	}
	d := filepath.Base(vdir)
	if !updatecheck.Parses(d) {
		return "", false
	}
	return strings.TrimPrefix(d, "v"), true
}

func installedVersionOf(h string) (string, error) {
	if v, ok := linkedVersionOf(h); ok {
		return v, nil
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil {
		return "", err
	}
	if ok && updatecheck.Parses(rec.Version) {
		return strings.TrimPrefix(rec.Version, "v"), nil
	}
	return "", errors.New("the installed version is unknown")
}

// updateChildWaitDelay is how long, after the context ends, the child gets
// to exit and release its pipes before Go kills it and stops waiting. A var
// only so a test can shorten nothing it does not need to.
var updateChildWaitDelay = 5 * time.Second

// updateChildStderrTail is how much of the child's stderr is kept.
const updateChildStderrTail = 4 << 10

// autoUpdateArgs is the install child's argv after the binary: the same
// `chottag update` a person runs, never restarting the daemon.
func autoUpdateArgs(tag string) []string {
	return []string{"update", "--version", tag, "--no-restart", "--json"}
}

// runAutoUpdateChild is autoUpdateRun's production body. self is the
// installed binary, resolved by the loop: never a path from the network.
//
// On shutdown the child gets SIGTERM, then (after updateChildWaitDelay) a
// kill. An install cut off that way may leave a download unpacked or setup
// unfinished; the update lock is released by the kernel, and the next
// `chottag update`, which re-checks the installed version, completes it.
func runAutoUpdateChild(ctx context.Context, self, tag string) autoRun {
	// self is <home>/bin/chottag's target and tag passed checkVersionTag:
	// nothing here comes from the network unvalidated, and no shell is used.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, self, autoUpdateArgs(tag)...)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, left: updateChildLimit}
	errTail := &tailWriter{max: updateChildStderrTail}
	cmd.Stderr = errTail
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = updateChildWaitDelay
	runErr := cmd.Run()
	res := parseUpdateChildJSON(out.Bytes(), runErr)
	if !res.OK {
		res.Stderr = sanitizeGHStderr(errTail.text())
	}
	return res
}

// limitedWriter keeps the first left bytes and drops the rest, so a child
// can neither fail the write nor grow the buffer.
type limitedWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left > 0 {
		if len(p) > l.left {
			p = p[:l.left]
		}
		l.w.Write(p)
		l.left -= len(p)
	}
	return n, nil
}

// tailWriter keeps the last max bytes written and never fails.
type tailWriter struct {
	buf     []byte
	max     int
	dropped bool
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
		t.dropped = true
	}
	return len(p), nil
}

// text is the kept tail. When bytes were dropped it starts at the first full
// line: a line cut mid-way can begin inside a URL, past what redaction
// recognises. With no newline left there is nothing safe to show.
func (t *tailWriter) text() string {
	if !t.dropped {
		return string(t.buf)
	}
	i := bytes.IndexByte(t.buf, '\n')
	if i < 0 {
		return ""
	}
	return string(t.buf[i+1:])
}

// parseUpdateChildJSON reads `ok`, `installed`, `error.code` and
// `error.message` from the child's one JSON document. A document that is not
// JSON, or that says ok without the child exiting cleanly, is a failure: the
// install is only trusted when both agree.
func parseUpdateChildJSON(out []byte, runErr error) autoRun {
	var doc struct {
		OK        bool `json:"ok"`
		Installed bool `json:"installed"`
		Error     struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return autoRun{Reason: string(codeUpdateFailed)}
	}
	if doc.OK && runErr == nil {
		return autoRun{OK: true, Installed: doc.Installed}
	}
	switch {
	case doc.Error.Code == "" || (doc.OK && runErr != nil):
		return autoRun{Reason: string(codeUpdateFailed)}
	case doc.Error.Code == string(codeUpdateFailed) && doc.Error.Message != "":
		// The generic code says nothing; its message does (gh missing, a
		// checksum mismatch). One line, URLs redacted, capped.
		if m := sanitizeGHStderr(doc.Error.Message); m != "" {
			return autoRun{Reason: m}
		}
	}
	return autoRun{Reason: doc.Error.Code}
}

// installedChottag is the binary the auto-install child runs: <home>/bin/
// chottag, resolved now. It is the one place the daemon learns the path, so
// a network-supplied value can never reach it.
func installedChottag(h string) (string, error) {
	p, err := filepath.EvalSymlinks(filepath.Join(h, "bin", "chottag"))
	if err != nil {
		return "", fmt.Errorf("the installed chottag is not found under %s", filepath.Join(h, "bin"))
	}
	return p, nil
}

// updateLoop is one daemon generation's update checker. A nil *updateLoop
// is valid and does nothing.
type updateLoop struct {
	home     string
	repo     string
	version  string
	upstream *url.URL
	state    func() (store.State, error)
	sink     *statusSink
	// notify posts one notice, honouring the notify switch and the install's
	// label. nil posts nothing.
	notify func(title, body string)
	log    io.Writer
	// restartNotified, if non-nil, tells the restart loop that the user was
	// already told version is installed.
	restartNotified func(version string)
	// processed, if non-nil, receives once per handled timer fire. Tests only.
	processed chan<- struct{}
}

// newUpdateLoop returns the loop, or nil when this daemon never checks: a
// version that is not a release (a dev build) or CHOTTAG_NO_UPDATE_CHECK=1.
func newUpdateLoop(h string, upstream *url.URL, state func() (store.State, error), sink *statusSink, dn *daemonNotify, log io.Writer) *updateLoop {
	if !updatecheck.Parses(Version) || os.Getenv(noUpdateCheckEnv) == "1" {
		return nil
	}
	l := &updateLoop{home: h, repo: updateRepo(h), version: Version, upstream: upstream, state: state, sink: sink, log: log}
	if dn != nil {
		l.notify = dn.post
	}
	return l
}

// Run checks 2 minutes after it starts, then every 24 h plus a random
// 0-60 min, until ctx ends. A tick in flight is cancelled with ctx.
func (l *updateLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	wait := updateFirstCheck
	for {
		c, stop := newUpdateTimer(wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-c:
		}
		stop()
		l.tick(ctx)
		if l.processed != nil {
			select {
			case l.processed <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		wait = updateInterval + updateJitter()
	}
}

func (l *updateLoop) logf(format string, a ...any) {
	if l.log != nil {
		fmt.Fprintf(l.log, "chottag: update: "+format+"\n", a...)
	}
}

func (l *updateLoop) post(title, body string) {
	if l.notify != nil {
		l.notify(title, body)
	}
}

// tick is one check, and the auto-install it may lead to. Every failure ends
// the tick quietly: the daemon's requests never wait on it.
func (l *updateLoop) tick(ctx context.Context) {
	st, err := l.state()
	if err != nil {
		l.logf("state.json unreadable, skipping this check: %v", err)
		return
	}
	if !st.UpdateCheckOn() {
		return
	}
	u := l.sink.updateCopy()
	rel, ferr := updateFetch(ctx, l.upstream, l.repo)
	if ctx.Err() != nil {
		return
	}
	now := updateLoopClock()
	u.CheckedAt = now.UTC()
	if ferr != nil {
		u.Error = ferr.Error()
		l.sink.setUpdate(&u)
		l.logf("check failed: %v", ferr)
		return
	}
	u.Error = ""
	u.Latest = rel.Version
	u.PublishedAt = rel.PublishedAt
	u.Available = releaseAvailable(rel, l.version, l.installedOrUnknown())
	auto := u.Available && st.AutoUpdateOn() && l.autoAllowed(u, rel, now)
	// An install about to run is its own news: no "run chottag update" first.
	if u.Available && u.Notified != rel.Version && !auto {
		l.post("chottag: update available", fmt.Sprintf("chottag %s is available. Run: chottag update", rel.Version))
		u.Notified = rel.Version
	}
	if auto {
		u.Notified = rel.Version
	}
	l.sink.setUpdate(&u)
	if auto {
		l.autoInstall(ctx, u, rel, st.RestartOn())
	}
}

// installedOrUnknown is the installed version, or "" when it cannot be told
// (logged): then only the running version counts.
func (l *updateLoop) installedOrUnknown() string {
	inst, err := installedVersion(l.home)
	if err != nil {
		l.logf("installed version unknown: %v", err)
		return ""
	}
	return inst
}

// releaseAvailable is whether rel is an update worth telling about: a
// published release (no draft, no pre-release flag, no semver pre-release
// tag) newer than the running version and than the installed one, which can
// be ahead of the running daemon (an update with --no-restart, or one a
// person ran). installed "" means unknown: only the running version counts.
// The daemon's check and `update --check`'s cache write share this rule.
func releaseAvailable(rel updatecheck.Release, running, installed string) bool {
	if rel.Draft || rel.Prerelease || updatecheck.IsPrerelease(rel.Version) || !updatecheck.Newer(rel.Version, running) {
		return false
	}
	return installed == "" || updatecheck.Newer(rel.Version, installed)
}

// autoAllowed is spec §5's conditions 1 to 3. Condition 4 (the update lock)
// is the child's own: it answers update_in_progress. A release with no
// published time is never old enough.
func (l *updateLoop) autoAllowed(u status.Update, rel updatecheck.Release, now time.Time) bool {
	if rel.Draft || rel.Prerelease || updatecheck.IsPrerelease(rel.Version) || !updatecheck.Newer(rel.Version, l.version) || !updatecheck.SameMajor(rel.Version, l.version) {
		return false
	}
	if rel.PublishedAt.IsZero() || now.Sub(rel.PublishedAt) < updateSoak {
		return false
	}
	if a := u.Auto; a != nil && a.Version == rel.Version && (a.OK || now.Sub(a.At) < updateSoak) {
		return false // done, or tried within the last day
	}
	return true
}

// autoInstall runs the child and records the outcome. A success that
// installed something is announced once. A failure is announced once per
// version while it keeps failing, whatever its reason (a reason can embed a
// per-run path): it is retried quietly each day, and the record keeps the
// latest full reason.
func (l *updateLoop) autoInstall(ctx context.Context, u status.Update, rel updatecheck.Release, restartOn bool) {
	var res autoRun
	if _, valid := checkVersionTag(rel.Tag); !valid || !versionFlagPrefixPattern.MatchString(rel.Tag) {
		res.Reason = "release tag is not vX.Y.Z"
	} else if self, err := installedChottag(l.home); err != nil {
		res.Reason = err.Error()
	} else {
		res = l.runChild(ctx, self, rel.Tag)
	}
	if ctx.Err() != nil {
		return // shutting down: a cancelled attempt is not an outcome
	}
	if !res.OK && res.Reason == string(codeUpdateInProgress) {
		// Another update holds the lock: a precondition, not a failure. No
		// notice and no record, so the next tick tries again.
		l.logf("another update is running: %s waits for the next check", rel.Version)
		return
	}
	prev := u.Auto
	a := status.AutoAttempt{Version: rel.Version, At: updateLoopClock().UTC(), OK: res.OK}
	switch {
	case res.OK && res.Installed:
		if restartOn {
			// R126: the daemon restarts itself once idle. This is the one
			// notice for the version: the restart loop's own is skipped.
			if l.restartNotified != nil {
				l.restartNotified(rel.Version)
			}
			l.post("chottag: updated", fmt.Sprintf("chottag updated to %s. The daemon switches to it when Claude Code is idle.", rel.Version))
		} else {
			l.post("chottag: updated", fmt.Sprintf("chottag updated to %s. The daemon uses it after: chottag daemon restart", rel.Version))
		}
		l.logf("installed %s", rel.Version)
	case res.OK:
		l.logf("%s was already in place", rel.Version)
	default:
		a.Error = res.Reason
		if res.Stderr != "" {
			l.logf("update child: %s", res.Stderr)
		}
		l.logf("could not install %s: %s", rel.Version, res.Reason)
		if prev == nil || prev.OK || prev.Version != a.Version {
			l.post("chottag: update failed", fmt.Sprintf("chottag could not update to %s: %s. Run: chottag update", rel.Version, res.Reason))
		}
	}
	u.Auto = &a
	l.sink.setUpdate(&u)
}

// runChild runs the install child, flagged as running (the restart loop
// stands off meanwhile) and under autoUpdateTimeout: a child that outlives
// it is stopped and reported as a timeout, so a stalled download cannot hold
// the update lock and this loop forever.
func (l *updateLoop) runChild(ctx context.Context, self, tag string) autoRun {
	autoInstalling.Store(true)
	defer autoInstalling.Store(false)
	cctx, cancel := context.WithTimeout(ctx, autoUpdateTimeout)
	defer cancel()
	res := autoUpdateRun(cctx, self, tag)
	if !res.OK && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return autoRun{Reason: "timeout", Stderr: res.Stderr}
	}
	return res
}
