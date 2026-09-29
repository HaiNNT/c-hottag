package notify

import (
	"fmt"
	"time"
)

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
		"The daemon resent a swapped request unchanged; the route table may not match this Claude Code version. Run: chottag trace on"
}

// switchedMessage is the auto-switch notice (M4 spec §7).
func switchedMessage(s Switch) (title, body string) {
	title = "chottag: switched to " + s.To
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

func noCandidateMessage(from string) (title, body string) {
	return "chottag: no account to switch to",
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
