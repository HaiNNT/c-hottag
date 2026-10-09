package notify

import (
	"fmt"
	"strings"
	"time"
)

// poolTitle is a notice's title, naming the pool when there is one (M8):
// "chottag: work: switched to C".
func poolTitle(pool, title string) string {
	if pool == "" {
		return "chottag: " + title
	}
	return "chottag: " + pool + ": " + title
}

// Every notice names an account and a command, never an email, an org, a
// token or a path (spec §4 "Content"). The texts are fixed here, in one
// place, so the tests can hold them exactly.

func needsLoginMessage(account string) (title, body string) {
	return "chottag: " + account + " needs login",
		"Its requests use your own Claude login until you run: chottag login " + account
}

func allLimitedMessage(s LimitState, loc *time.Location) (title, body string) {
	title = "chottag: all accounts limited"
	if s.NextReset.IsZero() {
		return title, "No reset time is known yet. Run: chottag status"
	}
	when := s.NextReset.In(loc).Format("Mon 15:04")
	if s.NextResetAccount == "" {
		return title, "Next reset: " + when + ". Run: chottag status"
	}
	return title, "Next reset: " + s.NextResetAccount + " at " + when + ". Run: chottag status"
}

func availableMessage(account string) (title, body string) {
	title = "chottag: an account is available again"
	if account == "" {
		return title, "Run: chottag status"
	}
	return title, account + " is no longer limited. Run: chottag status"
}

func routeDriftMessage() (title, body string) {
	return "chottag: route drift",
		"A swapped request was refused even after a retry; the route table may not match this Claude Code version. Run: chottag trace on"
}

// unknownOwnerMessage is the notice for a claude.ai object that no account of
// the pool could open, whose owner chottag does not know (R168).
func unknownOwnerMessage(account, kind string) (title, body string) {
	article := "a"
	if kind != "" && strings.ContainsRune("aeiouAEIOU", rune(kind[0])) {
		article = "an"
	}
	return fmt.Sprintf("chottag: %s %s's owner is unknown", article, kind),
		account + " could not open it, and chottag doesn't know which account made it. This is not route drift."
}

// servingRefusedMessage is the notice for a swapped serving request that the
// account's login had refused twice (R158). resent says the request then went
// out on the client's own (Home) login.
func servingRefusedMessage(account string, status int, resent bool) (title, body string) {
	title = fmt.Sprintf("chottag: %s's login was refused (%d)", account, status)
	if resent {
		return title, "This request went out on your own (Home) login. If it repeats, run chottag login " + account + "."
	}
	return title, "This request was not resent on your own login. If it repeats, run chottag login " + account + "."
}

// switchedMessage is the auto-switch notice (M4 spec §7).
func switchedMessage(s Switch) (title, body string) {
	title = poolTitle(s.Pool, "switched to "+s.To)
	w := windowName(s.Window)
	switch {
	case s.Trigger == "threshold":
		body = fmt.Sprintf("%s reached %.0f%% of its %s limit.", s.From, s.Pct, w)
	case s.Retried:
		body = fmt.Sprintf("%s hit its %s limit; your request went to %s.", s.From, w, s.To)
	default:
		body = fmt.Sprintf("%s hit its %s limit. Resend your last message.", s.From, w)
	}
	return title, body
}

func noCandidateMessage(pool, from string) (title, body string) {
	return poolTitle(pool, "no account to switch to"),
		from + " needs to switch, but no other account can serve now. Run: chottag status"
}

// windowName spells a usage window for a notice.
func windowName(w string) string {
	switch w {
	case "5h":
		return "5-hour"
	case "7d":
		return "7-day"
	}
	return "usage"
}

// movedMessage is the spread notice (M7 spec §7): the sessions on an account
// that crossed a switch point or hit a limit, and where they will go.
func movedMessage(m Moved) (title, body string) {
	noun := "sessions"
	if m.Sessions == 1 {
		noun = "session"
	}
	w := windowName(m.Window)
	switch {
	case m.Limited:
		body = fmt.Sprintf("%s hit its %s limit.", m.From, w)
	case m.Window == "":
		body = m.From + " is over its switch point."
	default:
		body = fmt.Sprintf("%s reached its %s switch point.", m.From, w)
	}
	if len(m.To) == 0 {
		return poolTitle(m.Pool, fmt.Sprintf("%d %s on %s have no account to move to", m.Sessions, noun, m.From)), body
	}
	return poolTitle(m.Pool, fmt.Sprintf("moved %d %s from %s to %s", m.Sessions, noun, m.From, strings.Join(m.To, ", "))), body
}
