package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/store"
)

var labelRE = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

const setupUsage = "usage: chottag setup [--claude PATH] [--label NAME] [--name DIR=NAME]..."

const labelUsage = "usage: chottag setup [--claude PATH] [--label NAME] [--name DIR=NAME]...: NAME is 1-16 characters of a-z, 0-9 and -; --label \"\" clears it"

// parseSetupArgs parses setup's flags and returns the args to hand to adopt
// (its own --claude and --name); a stray positional is a usage error. --label is validated here, before setup touches anything; set is
// true when it was given, an empty value included (it clears the label).
func parseSetupArgs(args []string, r *reporter) (adoptArgs []string, label string, set bool, code int, err error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	labelFlag := fs.String("label", "", "label this install in notification titles and status")
	claudeBin := fs.String("claude", "", "path to the real claude binary (default: the real claude on PATH, never chottag's own shim)")
	names := nameFlags{}
	fs.Var(names, "name", "DIR=NAME: register the slot dir DIR under the account name NAME (repeatable)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, "", false, r.FlagError(err), err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "label":
			label, set = *labelFlag, true
		case "claude":
			adoptArgs = append(adoptArgs, "--claude", *claudeBin)
		}
	})
	if set && label != "" && !labelRE.MatchString(label) {
		return nil, "", false, r.Usage(labelUsage), errors.New(labelUsage)
	}
	dirs := make([]string, 0, len(names))
	for dir := range names {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		adoptArgs = append(adoptArgs, "--name", dir+"="+names[dir])
	}
	if len(positional) != 0 {
		return nil, "", false, r.Usage(setupUsage), errors.New("setup: unexpected arguments")
	}
	return adoptArgs, label, set, exit.OK, nil
}

// setupDirs are created directly under $CHOTTAG_HOME. "cache" holds
// cache/status.json (written by the daemon); nothing in this package writes
// it yet, but setup provisions the whole tree up front (§4.2) so later
// commands never have to MkdirAll a subdirectory of their own.
var setupDirs = []string{"ca", "bin", "accounts", "run", "cache"}

// provisionBinSymlinks points binDir/chottag and binDir/claude at exe,
// replacing whatever was there before — except when exe already IS the
// file at that link (plausible: runSetup puts binDir first on PATH, and
// `go build -o $CHOTTAG_HOME/bin/chottag` is a natural thing to type). In
// that case os.Remove(link) would delete the running binary and
// os.Symlink(exe, link) would then create a symlink pointing at the path
// it was just deleted from — a self-referential link, ELOOP on every
// subsequent chottag and claude invocation (whole-branch review D7/F-J).
// sameFile (daemon.go) is best-effort — it returns false, not an error,
// when os.Stat can't compare inodes yet — which is fine here: the case
// being guarded against, exe already sitting at link, is exactly what
// os.Stat resolves.
//
// Pulled out of runSetup, which calls os.Executable() (not a seam), so
// this — the part that can actually be driven with a controlled exe and a
// controlled link — has a direct test, the same reason daemonSpawnArgs was
// pulled out of spawnDaemon (internal/shim/shim.go).
func provisionBinSymlinks(exe, binDir string) error {
	for _, name := range []string{"chottag", "claude"} {
		link := filepath.Join(binDir, name)
		if same, err := sameFile(exe, link); err != nil {
			return err
		} else if same {
			continue
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(exe, link); err != nil {
			return err
		}
	}
	return nil
}

// setupResult is `setup --json`'s fields (spec §5.3): the bin dir the
// symlinks went into, whether the PATH line was written, and where.
type setupResult struct {
	Installed string `json:"installed"`
	RCUpdated bool   `json:"rcUpdated"`
	RCPath    string `json:"rcPath,omitempty"`
	// RCBackup is where the rc file's previous content was copied before
	// setup changed it (R123); absent when nothing was backed up.
	RCBackup string `json:"rcBackup,omitempty"`
	// Adopt is the inner adopt's own result, the same fields as
	// `adopt --json` (F152). It is present with empty arrays when there
	// was nothing to adopt (not a warning — fix round 1), and absent for
	// any other adopt failure, which is then an adopt_failed warning.
	Adopt *adoptResult `json:"adopt,omitempty"`
}

// runSetup installs the shim: the ~/.chottag tree, the local CA, the
// bin/chottag and bin/claude symlinks (one binary, dispatched by argv[0] —
// §4.3), and a PATH line in the user's shell rc. It then runs the existing
// adopt flow so slots that already hold a login (from before chottag had a
// CLI) are registered without a fresh `claude auth login`.
//
// Like its sibling runAdopt (accounts.go), it reads the environment rather
// than taking home/rc paths as parameters: home() for $CHOTTAG_HOME, $HOME
// for the rc file's directory, $SHELL for which rc to edit.
//
// adopt runs through a nested reporter (spec §5.3): its text and warnings
// appear exactly as before, but only setup writes the one document.
func runSetup(args []string, r *reporter) int {
	args, label, setLabel, code, err := parseSetupArgs(args, r)
	if err != nil {
		return code
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	for _, sub := range setupDirs {
		if err := os.MkdirAll(filepath.Join(h, sub), 0o700); err != nil {
			return r.FailErr(err)
		}
	}
	if _, err := ca.LoadOrCreate(filepath.Join(h, "ca")); err != nil {
		return r.FailErr(err)
	}
	// The proxy secret (F221): created here so the daemon's first health
	// probe and the shim's first launch both find one already in place,
	// and so `chottag doctor`'s proxy-secret row (T11) reads ok straight
	// after setup rather than "missing" until the first `claude` invocation.
	// A *proxyauth.PermError already names `chottag doctor --fix` in its
	// own Error() text; ErrMalformed does not (it just names the path), so
	// that hint is appended here for it explicitly (fix round 1 item 3).
	if _, err := proxyauth.LoadOrCreate(h); err != nil {
		if errors.Is(err, proxyauth.ErrMalformed) {
			err = fmt.Errorf("%w; run: chottag doctor --fix", err)
		}
		return r.FailErr(err)
	}

	if setLabel {
		if _, err := (store.Store{Dir: h}).Update(func(st *store.State) error {
			st.Label = label
			return nil
		}); err != nil {
			return r.FailErr(err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return r.FailErr(err)
	}
	binDir := filepath.Join(h, "bin")
	if err := provisionBinSymlinks(exe, binDir); err != nil {
		return r.FailErr(err)
	}
	r.Text("installed %s\n", binDir)
	res := setupResult{Installed: binDir}

	// chottagHomeExport is h itself (already absolute) when the user set
	// CHOTTAG_HOME, so a new shell's `chottag` resolves the same home
	// instead of falling back to a second, empty ~/.chottag (fix round,
	// item 9); "" — and so no export line at all — when they did not.
	chottagHomeExport := ""
	if os.Getenv("CHOTTAG_HOME") != "" {
		chottagHomeExport = h
	}

	rcPath := rcPathFor(os.Getenv("SHELL"), os.Getenv("HOME"))
	if rcPath == "" {
		// On stdout in text mode, exactly as before; a warning carrying the
		// block under --json, so a script can still show it.
		r.TextWarn(warnRCNotWritten, strings.TrimSuffix("unrecognised $SHELL; add this to your shell's rc yourself:\n"+rcBlock(binDir, chottagHomeExport), "\n"))
	} else {
		backup, pruneWarn, err := writeRCBlock(h, rcPath, binDir, chottagHomeExport)
		if err != nil {
			return r.FailErr(err)
		}
		if pruneWarn != nil {
			r.Warn(warnPruneFailed, "chottag: "+pruneWarn.Error())
		}
		if backup != "" {
			r.Text("backed up %s to %s\n", rcPath, backup)
			res.RCBackup = backup
		}
		r.Text("updated %s to put %s first on PATH\n", rcPath, binDir)
		res.RCUpdated, res.RCPath = true, rcPath
	}

	// adopt's own codeNoAccounts (an empty accounts dir it could read fine)
	// is not a setup failure — a fresh install has no pre-existing slots
	// yet — and, since every first install hits it, it is also not a
	// warning: a script or agent (R44) treats any warning as a problem, and
	// a fresh install is not one (fix round 1). The empty adopt object is
	// what shows nothing was there. codeNoSlots is different: setup always
	// MkdirAll's the accounts dir itself before adopt runs, so adopt's
	// ReadDir failing there can only be a real failure (EACCES, for one),
	// never "nothing to adopt yet" — it falls to the adopt_failed warning
	// branch below (fix round 2). A usage error from adopt itself (exit 2)
	// is propagated as adopt's own document; bad flags and stray positionals
	// are already caught by setup's own flag set, before anything is written. Any OTHER adopt failure,
	// codeNoSlots included, is an adopt_failed warning: its text is already
	// on stderr, so text mode is unchanged (F152). The decision is always
	// adopt's error CODE, never its message text.
	ar := r.Nested()
	switch code := runAdopt(args, ar); {
	case code == exit.Usage:
		return r.Relay(ar, code)
	case code != exit.OK && ar.failed != nil && ar.failed.code == codeNoAccounts:
		res.Adopt = &adoptResult{Adopted: []adoptEntry{}, Updated: []adoptEntry{}, Skipped: []adoptSkip{}}
	case code != exit.OK:
		msg := fmt.Sprintf("adopt exited %d", code)
		if ar.failed != nil {
			msg = ar.failed.message
		}
		r.RecordWarn(warnAdoptFailed, "adopt failed: "+msg)
	default:
		if adopted, ok := ar.result.(adoptResult); ok {
			res.Adopt = &adopted
		}
	}
	return r.OK(res)
}
