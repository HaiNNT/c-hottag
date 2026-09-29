package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/refresh"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// ErrNoCandidate means no account can take the serving role. It is a
// sentinel because an exit code depends on it: M1b matched
// strings.Contains(err.Error(), "no account to switch to") instead, so
// rewording the message would have silently changed exit 3 into exit 1.
var ErrNoCandidate = errors.New("no account to switch to")

// tagResult is `tag NAME --json`'s fields (spec §5.3).
type tagResult struct {
	Serving  string `json:"serving"`
	Previous string `json:"previous,omitempty"`
}

// nextResult is `tag` / `next --json`'s fields. skipped is always an array
// (spec §5.3: arrays are never null).
type nextResult struct {
	Serving  string      `json:"serving"`
	Previous string      `json:"previous,omitempty"`
	Skipped  []skipEntry `json:"skipped"`
	// Fallback reports that Serving was picked via the fallback: above its
	// own switch point, with no account below its (item 2, review round
	// 3). Additive: absent (false) on every other outcome.
	Fallback bool `json:"fallback,omitempty"`
}

// skipEntry is one passed-over account on the wire: reason is a token
// (out_of_rotation | needs_login | limited | above_switch_point), and until
// is present only for a limited account with a known reset.
type skipEntry struct {
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	Until  time.Time `json:"until,omitzero"`
}

func skipEntries(skips []skip) []skipEntry {
	out := make([]skipEntry, 0, len(skips))
	for _, sk := range skips {
		reason := strings.ReplaceAll(sk.Reason, " ", "_")
		out = append(out, skipEntry{Name: sk.Name, Reason: reason, Until: sk.Until.UTC()})
	}
	return out
}

// tagState loads what tag and next both need: the store, the status cache
// and the clock. The cache's rows are matched to the roster by slot dir
// (F171), as `status` does, so a rename the daemon has not yet written
// (or no daemon at all) still reads the renamed account's own row, and a
// name swap never reads the other account's.
func tagState() (store.Store, status.File, time.Time, error) {
	h, err := home()
	if err != nil {
		return store.Store{}, status.File{}, time.Time{}, err
	}
	f, ferr := status.Load(status.Path(h))
	if ferr != nil {
		// A missing or unreadable cache is not an error: it is disposable
		// and rebuilt from traffic (§6.3). It means "unknown", and unknown
		// never skips an account.
		f = status.File{}
	}
	if st, err := (store.Store{Dir: h}).Load(); err == nil {
		f.EnsureRoster(accountMembers(st))
	}
	now := time.Now()
	// RollUpAt only recomputes f.Limits (the roll-up summary), which
	// neither nextCandidate nor knownLimit reads — it does not do the
	// "already reset" handling for a single account's LimitedUntil; that
	// lives entirely in knownLimit (candidate.go). Kept for parity with
	// other cache-consuming commands (status --json) that do read f.Limits.
	f.RollUpAt(now)
	return store.Store{Dir: h}, f, now, nil
}

// runTag sets the serving account, or moves to the next account in rotation.
// `tag [NAME] [--force]` parses order-free (spec §5.3): a flag after NAME is
// parsed, never read as a second name.
func runTag(args []string, r *reporter) int {
	fs := flag.NewFlagSet("tag", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	force := fs.Bool("force", false, "switch even past a limit, a switch point or a needs-login state (never past rotation)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) > 1 {
		return r.Usage("usage: chottag tag [NAME] [--force]")
	}
	s, f, now, err := tagState()
	if err != nil {
		return r.FailErr(err)
	}
	if len(positional) == 0 {
		return runNext(s, &f, now, *force, r)
	}

	// positional[0] is what the user typed at the prompt: Find's unique
	// name/email/prefix resolution is the intended convenience here,
	// not an identity check — do not replace with an exact-only match.
	name := positional[0]
	var previous string
	st, err := s.Update(func(st *store.State) error {
		a, err := st.Find(name)
		if err != nil {
			return err
		}
		previous = st.Serving
		st.Serving = a.Name
		return nil
	})
	if err != nil {
		return r.FailErr(err)
	}
	r.Text("serving: %s\n", st.Serving)

	// tag WARNS, it does not refuse (spec §5): an explicit tag is a
	// deliberate override, and refusing it would take away the escape
	// hatch `next --force` exists for.
	if until, limited := knownLimit(&f, st.Serving, now); limited {
		if until.IsZero() {
			r.Warn(warnLimited, fmt.Sprintf("chottag: warning: %s is limited", st.Serving))
		} else {
			r.Warn(warnLimited, fmt.Sprintf("chottag: warning: %s is limited until %s", st.Serving, untilText(until, now)))
		}
	}
	if a, err := st.Find(st.Serving); err == nil && !a.Rotates() {
		r.Warn(warnOutOfRotation, fmt.Sprintf("chottag: warning: %s is out of rotation; `chottag next` will skip it", st.Serving))
	}
	return r.OK(tagResult{Serving: st.Serving, Previous: previous})
}

// runNextCmd is `chottag next [--force]`. Its only flag is --force, in any
// position; a NAME is a `tag` typo and must not silently switch to an
// unintended account, so any positional is exit 2 (spec §5.3).
func runNextCmd(args []string, r *reporter) int {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	force := fs.Bool("force", false, "switch even past a limit, a switch point or a needs-login state (never past rotation)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Fail(exit.Usage, codeUsage, "next takes no arguments (except --force)", nil)
	}
	s, f, now, err := tagState()
	if err != nil {
		return r.FailErr(err)
	}
	return runNext(s, &f, now, *force, r)
}

// runNext moves the serving role to the next eligible account in
// registration order (spec §5, §6.3, R29): in rotation, not needing a
// login, not known to be limited, and below its own switch points — where a
// stale reading is a lower bound, not "unknown" (autoswitch.Eligible, via
// nextCandidate). When no account is eligible, a hard request to move gets
// the LIMIT trigger's own fallback too (item 2, review round 3): any
// account with capacity at all, rather than leaving the user stuck — named
// as a fallback in the human output and nextResult.Fallback, not printed as
// a skip. --force overrides a limit, a switch point and a needs-login
// state, but never rotation: an account the user deliberately excluded is
// not a fallback target either.
func runNext(s store.Store, f *status.File, now time.Time, force bool, r *reporter) int {
	var skips []skip
	var fellBack bool
	var previous string
	st, err := s.Update(func(st *store.State) error {
		previous = st.Serving
		a, sk, fb, err := nextCandidate(st, f, now, force)
		skips, fellBack = sk, fb
		if err != nil {
			return err
		}
		st.Serving = a.Name
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoCandidate) {
			fmt.Fprintln(r.Stderr(), "chottag: no account to switch to")
			for _, sk := range skips {
				if sk.Until.IsZero() {
					fmt.Fprintf(r.Stderr(), "  %s: %s\n", sk.Name, sk.Reason)
					continue
				}
				fmt.Fprintf(r.Stderr(), "  %s: %s until %s\n", sk.Name, sk.Reason, untilText(sk.Until, now))
			}
			fmt.Fprintln(r.Stderr(), "chottag: `chottag next --force` switches anyway")
			return r.FailNoText(exit.UserAction, codeNoCandidate, "no account to switch to",
				map[string]any{"skipped": skipEntries(skips)})
		}
		return r.FailErr(err)
	}
	if len(skips) > 0 {
		// Spec §5: one comma-joined line, reason per account, carrying the
		// reset time for a limited skip ("skipped B (out of rotation), D
		// (limited until 17:00)") — the two reasons have different
		// remedies, and a limited skip's remedy is knowing when to retry.
		parts := make([]string, len(skips))
		for i, sk := range skips {
			reason := sk.Reason
			if !sk.Until.IsZero() {
				reason = fmt.Sprintf("%s until %s", sk.Reason, untilText(sk.Until, now))
			}
			parts[i] = fmt.Sprintf("%s (%s)", sk.Name, reason)
		}
		r.Text("skipped %s\n", strings.Join(parts, ", "))
	}
	if fellBack {
		// The fallback's pick is above its own switch point too — it was
		// just the least-bad account, not literally eligible — so say so
		// instead of leaving the "serving" line looking like an ordinary
		// switch (item 2, review round 3).
		r.Text("%s is above its switch point; no account was below its own\n", st.Serving)
	}
	r.Text("serving: %s\n", st.Serving)
	return r.OK(nextResult{Serving: st.Serving, Previous: previous, Skipped: skipEntries(skips), Fallback: fellBack})
}

// remoteResult is `remote --json`'s fields (spec §5.3). Remote is omitempty
// to match status --json's own convention for its empty remote/serving
// fields (fix round 4, item 5): an unset remote is absent, not an empty
// string.
type remoteResult struct {
	Remote  string `json:"remote,omitempty"`
	Changed bool   `json:"changed"`
}

// runRemote sets or shows the account that owns claude.ai objects. It takes
// no flags: a stray one is exit 2, never an account name (spec §5.3).
func runRemote(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\nusage: chottag remote [NAME]\n", err)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) > 1 {
		return r.Usage("usage: chottag remote [NAME]")
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}
	if len(args) == 0 {
		st, err := s.Load()
		if err != nil {
			return r.FailErr(err)
		}
		r.Text("remote: %s\n", st.Remote)
		return r.OK(remoteResult{Remote: st.Remote})
	}
	var previous string
	st, err := s.Update(func(st *store.State) error {
		// args[0] is what the user typed at the prompt: Find's unique
		// name/email/prefix resolution is the intended convenience here,
		// not an identity check — do not replace with an exact-only match.
		a, err := st.Find(args[0])
		if err != nil {
			return err
		}
		previous = st.Remote
		st.Remote = a.Name
		return nil
	})
	if err != nil {
		return r.FailErr(err)
	}
	r.Text("remote: %s\n", st.Remote)
	return r.OK(remoteResult{Remote: st.Remote, Changed: previous != st.Remote})
}

// adoptEntry and adoptSkip are `adopt --json`'s array elements (spec §5.3).
// A skip's reason is a token: invalid_slot | probe_failed | no_login |
// register_failed | name_conflict.
type adoptEntry struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

type adoptSkip struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
}

type adoptResult struct {
	Adopted []adoptEntry `json:"adopted"`
	Updated []adoptEntry `json:"updated"`
	Skipped []adoptSkip  `json:"skipped"`
}

// runAdopt registers the slot dirs that already hold a login, so the slots
// created before chottag had a CLI are usable without logging in again
// (roadmap D2). `chottag setup` (setup.go) folds this in as its last step,
// through a nested reporter.
func runAdopt(args []string, r *reporter) int {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	claudeBin := fs.String("claude", "claude", "path to the real claude binary")
	names := nameFlags{}
	fs.Var(names, "name", "DIR=NAME: register the slot dir DIR under the account name NAME (repeatable)")
	// parseInterspersed (spec §5.3): adopt takes no positional, and before
	// M1d-d a stray one silently ended flag parsing, dropping every flag
	// after it — `--claude` included.
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage("usage: chottag adopt [--claude PATH] [--name DIR=NAME]...")
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}
	entries, err := os.ReadDir(filepath.Join(h, "accounts"))
	if err != nil {
		return r.Fail(exit.Error, codeNoSlots, fmt.Sprintf("no account slots: %v", err), nil)
	}
	slots := make([]string, 0, len(entries))
	slotSet := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			slots = append(slots, e.Name())
			slotSet[e.Name()] = true
		}
	}
	sort.Strings(slots)

	// --name overrides a directory that isn't among the slots just listed:
	// most likely a typo, and silently doing nothing with it would hide
	// that from the user.
	overrideDirs := make([]string, 0, len(names))
	for dir := range names {
		overrideDirs = append(overrideDirs, dir)
	}
	sort.Strings(overrideDirs)
	for _, dir := range overrideDirs {
		if !slotSet[dir] {
			r.Warn(warnNameNoSlot, fmt.Sprintf("chottag: --name %s=%s: no such slot directory (found: %s)", dir, names[dir], strings.Join(slots, ", ")))
		}
	}

	res := adoptResult{Adopted: []adoptEntry{}, Updated: []adoptEntry{}, Skipped: []adoptSkip{}}
	added := 0
	for _, slotName := range slots {
		dir, err := s.SlotDir(slotName)
		if err != nil {
			fmt.Fprintf(r.Stderr(), "chottag: skipping %s: %v\n", slotName, err)
			res.Skipped = append(res.Skipped, adoptSkip{Dir: filepath.Join(h, "accounts", slotName), Reason: "invalid_slot"})
			continue
		}
		name := slotName
		if override, ok := names[slotName]; ok {
			if err := store.ValidName(override); err != nil {
				return r.Fail(exit.Usage, codeUsage, err.Error(), nil)
			}
			name = override
		}
		email, org, sub, loggedIn, err := slotEmail(*claudeBin, dir)
		if err != nil {
			fmt.Fprintf(r.Stderr(), "chottag: skipping %s: %v\n", name, err)
			res.Skipped = append(res.Skipped, adoptSkip{Dir: dir, Reason: "probe_failed"})
			continue
		}
		if !loggedIn {
			fmt.Fprintf(r.Stderr(), "chottag: skipping %s: no login in %s\n", name, dir)
			res.Skipped = append(res.Skipped, adoptSkip{Dir: dir, Reason: "no_login"})
			continue
		}

		// An exact-name match only: st.Find resolves a unique name *prefix*
		// (store/store.go), which would treat an unrelated account like
		// "Alpha" as if it were the already-registered account "A" and
		// silently drop the slot the user asked to adopt.
		var status string // "added" | "unchanged" | "updated" | "conflict"
		var existingDir, registeredName string
		if _, err := s.Update(func(st *store.State) error {
			now := time.Now()
			// Match by slot Dir FIRST (C1/F173): the Dir this slot resolves
			// to may already belong to a registered account under a
			// DIFFERENT name than the one requested here — e.g. `chottag
			// rename B Bee` keeps Dir at accounts/B, so a later `adopt` (or
			// `--name accounts/B=X`) resolving that same dir must recognize
			// it as Bee's own slot, report it unchanged (or updated) under
			// Bee, and never register a second account on top of it. By
			// file identity (NEW-2), not a string compare: a
			// case-insensitive filesystem's accounts/B and accounts/b are
			// one inode, and a symlinked $CHOTTAG_HOME can spell the same
			// dir two different ways too. Falling through to the
			// name-based match below for a dir that matches nothing keeps
			// every other case (including a genuine name_conflict) exactly
			// as before.
			for i := range st.Accounts {
				a := &st.Accounts[i]
				if same, err := sameSlotDir(a.Dir, dir); err != nil || !same {
					continue
				}
				registeredName = a.Name
				emailChanged := a.Email != "" && email != "" && !strings.EqualFold(a.Email, email)
				orgChanged := a.Org != "" && org != "" && a.Org != org
				if emailChanged || orgChanged {
					status = "updated"
				} else {
					status = "unchanged"
				}
				a.Email, a.Org = email, org
				prefillPlan(a, sub)
				a.LoggedInAt = now
				return nil
			}
			for i := range st.Accounts {
				a := &st.Accounts[i]
				if strings.EqualFold(a.Name, name) {
					existingDir = a.Dir
					if existingDir != dir {
						status = "conflict"
						return nil
					}
					// Report a CHANGED identity rather than calling it
					// unchanged: re-pointing a name at a different EMAIL (or
					// a different, previously-known ORG — F16: two accounts
					// can share an email and differ only by org, so either
					// field can carry the swap) is exactly the silent
					// identity swap F16 exists to prevent. Compare before
					// assigning; the assignment itself is unconditional as
					// it always was.
					//
					// A field moving from unknown (blank) to whatever was
					// just probed is the deliberate BACKFILL this branch has
					// always done (e.g. Org added after first adoption) and
					// must stay "unchanged" —
					// TestAdoptBackfillsOrgOnAnAlreadyRegisteredAccount pins
					// exactly that. Only a previously-known, non-blank value
					// actually moving to a different, itself non-blank value
					// counts as a change; email is compared case-insensitively
					// (providers treat it that way in practice) while org is
					// compared exactly — folding case on the identity-bearing
					// email field is safe (it never hides a real change),
					// where doing the same for org would risk masking one.
					//
					// The `email != ""`/`org != ""` half of each guard exists
					// because slotEmail (below) passes doc.Email/doc.OrgName
					// through verbatim from a JSON document chottag does not
					// control, and nothing anywhere guarantees either is
					// non-empty when loggedIn is true. Without this half, a
					// probe that comes back logged-in with a blank field
					// would report "updated" with an empty identity in the
					// message, while the unconditional assignment below wipes
					// the stored value — announcing an erasure as a
					// deliberate, successful update.
					emailChanged := a.Email != "" && email != "" && !strings.EqualFold(a.Email, email)
					orgChanged := a.Org != "" && org != "" && a.Org != org
					if emailChanged || orgChanged {
						status = "updated"
					} else {
						status = "unchanged"
					}
					a.Email, a.Org = email, org
					prefillPlan(a, sub)
					a.LoggedInAt = now
					return nil
				}
			}
			if err := st.Add(store.Account{Name: name, Email: email, Org: org, Plan: planFromSubscription(sub), Dir: dir, AddedAt: now, LoggedInAt: now}); err != nil {
				return err
			}
			status = "added"
			return nil
		}); err != nil {
			fmt.Fprintf(r.Stderr(), "chottag: %s: %v\n", name, err)
			res.Skipped = append(res.Skipped, adoptSkip{Dir: dir, Reason: "register_failed"})
			continue
		}
		// displayName is the account's OWN registered name when this dir
		// matched one by Dir (registeredName), never the requested/typed
		// name in that case: `unchanged`/`updated` report the identity the
		// dir actually has, e.g. after a rename.
		displayName := name
		if registeredName != "" {
			displayName = registeredName
		}
		switch status {
		case "added":
			r.Text("adopted %s (%s)\n", name, email)
			res.Adopted = append(res.Adopted, adoptEntry{Dir: dir, Name: name})
			added++
		case "unchanged":
			r.Text("already registered: %s (%s)\n", displayName, dir)
		case "updated":
			r.Text("updated %s (%s)\n", displayName, email)
			res.Updated = append(res.Updated, adoptEntry{Dir: dir, Name: displayName})
		case "conflict":
			fmt.Fprintf(r.Stderr(), "chottag: skipping %s: name %q is already registered for a different slot %s\n", dir, name, existingDir)
			res.Skipped = append(res.Skipped, adoptSkip{Dir: dir, Reason: "name_conflict"})
		}
	}
	st, err := s.Load()
	if err != nil {
		return r.FailErr(err)
	}
	r.Text("%d account(s) registered; serving: %s remote: %s\n", len(st.Accounts), st.Serving, st.Remote)
	if added == 0 && len(st.Accounts) == 0 {
		// The summary line above is this outcome's whole text, as before
		// M1d-d; the document names it.
		return r.FailNoText(exit.Error, codeNoAccounts, "no account slot holds a login, so nothing is registered", nil)
	}
	return r.OK(res)
}

// nameFlags collects repeated --name DIR=NAME options.
type nameFlags map[string]string

func (n nameFlags) String() string { return fmt.Sprint(map[string]string(n)) }

func (n nameFlags) Set(v string) error {
	dir, name, ok := strings.Cut(v, "=")
	if !ok || dir == "" || name == "" {
		return fmt.Errorf("want DIR=NAME, got %q", v)
	}
	n[dir] = name
	return nil
}

// slotEmail asks Claude Code who is logged in inside a slot. It is the only
// thing chottag reads from a slot besides the token state. err is set only
// for a problem with running bin itself (a bad --claude path, a binary that
// can't start); a slot that simply has no login is loggedIn=false, err=nil,
// so the two are never confused in the caller's message.
//
// org is `claude auth status --json`'s orgName field, stored for display
// only (F16: two accounts can share an email and differ only by org). It is
// never an identity key and never comes from a response header.
//
// sub is its subscriptionType (F200: "pro", "max", "team", ...), which
// login and adopt use to pre-fill the account's plan tier (M4 spec §2).
func slotEmail(bin, slotDir string) (email, org, sub string, loggedIn bool, err error) {
	// bin is the --claude flag: an operator-supplied, trusted binary path
	// (never request- or network-derived), same trust boundary as
	// refresh.Claude and creds.ExecRunner.
	cmd := exec.Command(bin, "auth", "status", "--json")
	cmd.Env = refresh.ChildEnv(slotDir)
	out, runErr := cmd.Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			// bin could not even be started: not found, not executable, etc.
			return "", "", "", false, fmt.Errorf("run %s auth status: %w", bin, runErr)
		}
		// bin ran and exited non-zero: treat the same as "not logged in".
		return "", "", "", false, nil
	}
	var doc struct {
		LoggedIn         bool   `json:"loggedIn"`
		Email            string `json:"email"`
		OrgName          string `json:"orgName"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return "", "", "", false, fmt.Errorf("%s auth status --json: invalid JSON output", bin)
	}
	return doc.Email, doc.OrgName, doc.SubscriptionType, doc.LoggedIn, nil
}
