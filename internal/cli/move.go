package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

// moveSince is the first release whose daemon applies move requests (R191).
const moveSince = "0.10.10"

// moveMinSID is the shortest --session prefix accepted.
const moveMinSID = 4

// moveResult is `chottag move --json` beside the usual envelope. A refusal's
// reason rides in the error document instead (move_refused).
type moveResult struct {
	Session string `json:"session"`
	Pool    string `json:"pool,omitempty"`
	Account string `json:"account"`
	From    string `json:"from"`
}

// runMove is `chottag move NAME [--session SID] [--force]`: it writes a move
// request into state.json, which the daemon applies at the session's next
// serving-class request (R191).
func runMove(args []string, r *reporter) int {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	sidFlag := fs.String("session", "", "the session to move (a sid prefix of at least 4 characters; default: this session)")
	force := fs.Bool("force", false, "move even to an account above its switch point")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	const usageLine = "usage: chottag move NAME [--session SID] [--force]"
	if len(positional) != 1 {
		return r.Usage(usageLine)
	}
	if *sidFlag != "" && len(*sidFlag) < moveMinSID {
		return r.Usage(fmt.Sprintf("chottag: --session needs at least %d characters of the sid (see `chottag status --json`)", moveMinSID))
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}
	st, err := s.Load()
	if err != nil {
		return r.FailErr(err)
	}

	sid, pool, code := moveSession(h, st, *sidFlag, r)
	if code != 0 {
		return code
	}
	if !st.HasPool(pool) {
		pool = store.DefaultPool // a pool since removed routes as default
	}
	if !poolSpread(st, pool) {
		return r.Fail(exit.Usage, codeMoveSerial, fmt.Sprintf(
			"pool %s places every session on its serving account, so there is nothing to move: use `chottag policy spread` first, or `chottag tag NAME` to move every session", pool), nil)
	}
	a, err := st.Find(positional[0])
	if err != nil {
		return r.FailErr(err)
	}
	if !a.InPool(pool) {
		return failPool(r, fmt.Errorf("%w: %s is not in %s", store.ErrNotInPool, a.Name, pool))
	}

	f, ferr := status.Load(status.Path(h))
	if ferr != nil {
		f = status.File{} // disposable: unknown never refuses
	}
	now := time.Now()
	from := ""
	for _, act := range f.Sessions {
		if act.SID == sid {
			from = act.Placed
			if from == "" {
				from = act.Account
			}
		}
	}
	for _, pa := range planAccounts(&st, &f, now) {
		if !strings.EqualFold(pa.Name, a.Name) {
			continue
		}
		if reason := moveRefusal(pa, autoParams(st), now, *force); reason != "" {
			msg := fmt.Sprintf("%s cannot take a moved session: %s", a.Name, moveRefusalDetail(pa, autoParams(st), reason, now))
			if reason == autoswitch.ReasonAboveSwitch {
				msg += "; --force moves anyway"
			}
			return r.Fail(exit.Usage, codeMoveRefused, msg, map[string]any{"session": sid, "account": a.Name, "reason": reason})
		}
	}

	name := a.Name
	dir := a.Dir
	updated, err := s.Update(func(st *store.State) error {
		st.PruneMoves(liveKeep(h), now)
		st.SetMove(sid, store.Move{Account: name, Dir: dir, At: now})
		return nil
	})
	if err != nil {
		return r.FailErr(err)
	}

	short := sid
	if len(short) > 8 {
		short = short[:8]
	}
	if strings.EqualFold(from, name) {
		r.Text("session %s is already on %s; it is now kept there\n", short, name)
	} else {
		fromText := ""
		if from != "" {
			fromText = " (from " + from + ")"
		}
		r.Text("moved session %s to %s%s; it switches at its next request, and its prompt cache starts cold on %s\n", short, name, fromText, name)
	}
	warnMoveDaemon(updated, r)
	return r.OK(moveResult{Session: sid, Pool: poolField(pool), Account: name, From: from})
}

// moveSession finds the session to move: --session's unique live prefix, else
// the session this command runs in. A non-zero code is the reported failure.
func moveSession(h string, st store.State, prefix string, r *reporter) (sid, pool string, code int) {
	hint := "pass --session SID (the `sid` in `chottag status --json`, `sessions[]`) to name one"
	if prefix == "" {
		_, sid, pool = proxyEnvSession(os.Getenv("HTTPS_PROXY"), st.ResolvedPort())
		if sid == "" {
			if live, ok := liveAncestor(h); ok {
				sid, pool = live.SID, live.Pool
			}
		}
		if sid == "" {
			return "", "", r.Fail(exit.Usage, codeNoSession, "this is not a session chottag launched, so it has no session to move; "+hint, nil)
		}
		return sid, pool, 0
	}
	live, err := liveSessions(h)
	if err != nil {
		return "", "", r.FailErr(err)
	}
	var hits []session.Session
	for _, l := range live {
		if l.SID != "" && strings.HasPrefix(l.SID, prefix) {
			hits = append(hits, l)
		}
	}
	switch len(hits) {
	case 0:
		return "", "", r.Fail(exit.Usage, codeNoSession, fmt.Sprintf("no live session's sid starts with %q (`chottag status --json` lists them)", prefix), nil)
	case 1:
		return hits[0].SID, hits[0].Pool, 0
	}
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.SID
		if len(ids[i]) > 8 {
			ids[i] = ids[i][:8]
		}
	}
	sort.Strings(ids)
	return "", "", r.Fail(exit.Usage, codeSessionAmbiguous, fmt.Sprintf("%q matches %d sessions (%s): give more of the sid", prefix, len(hits), strings.Join(ids, ", ")), nil)
}

// warnMoveDaemon warns when the request will not be applied soon: no daemon
// (it applies when one starts), or one that predates moves and ignores them.
func warnMoveDaemon(st store.State, r *reporter) {
	up, ver := statusProbe(st.ResolvedPort())
	if !up {
		r.Warn(warnDaemonNotRunning, "chottag: warning: the daemon is not running: the move applies when it starts and the session next makes a request")
		return
	}
	if ver == "" || ver == Version || !updatecheck.Parses(Version) {
		return
	}
	if updatecheck.Parses(ver) && !updatecheck.Newer(moveSince, ver) {
		return // the daemon already applies moves
	}
	r.Warn(warnDaemonPredatesMove, fmt.Sprintf(
		"chottag: warning: the running daemon (%s) predates move: it ignores the request. Run `chottag daemon restart`.", ver))
}

// moveRefusalDetail is reason with what the user needs to act on it: when a
// limit clears, or which window is past which switch point.
func moveRefusalDetail(pa autoswitch.Account, p autoswitch.Params, reason string, now time.Time) string {
	switch reason {
	case autoswitch.ReasonLimited:
		if !pa.LimitedUntil.IsZero() {
			return reason + " until " + untilText(pa.LimitedUntil, now)
		}
	case autoswitch.ReasonAboveSwitch:
		if w, pt, pct, over := autoswitch.TargetOver(pa, p, now); over {
			return fmt.Sprintf("%s (%s %.0f%% \u2265 %d%%)", reason, w, pct, pt)
		}
	}
	return reason
}
