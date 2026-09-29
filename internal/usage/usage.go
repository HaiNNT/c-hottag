// Package usage turns Anthropic's unified rate-limit response headers into a
// typed snapshot, and decides whether a refusal means the account is out of
// quota.
//
// The header names and semantics here were captured from real traffic on
// 2026-09-20, not inferred: the headers ride on every response, successes
// included, and they state the verdict outright, so nothing here guesses.
//
// Three rules this package exists to enforce:
//
//   - error.type NEVER decides a limit. A short burst throttle carries the
//     same error.type as real exhaustion, so trusting it would rotate a user
//     off a healthy account (spec §6.2). Classify does not accept it as a
//     parameter, so it cannot be wired in later by accident.
//   - An unrecognised refusal is NOT a limit. A false "limited" silently
//     moves the user off their chosen account; a false "not limited" only
//     means they meet the wall themselves.
//   - Unknown is not zero. A window that reported no utilization says so
//     (HasUtilization), and an unknown reset stays the zero time rather than
//     becoming a past one.
package usage

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const prefix = "Anthropic-Ratelimit-Unified-"

// Window is one quota window (5 h or 7 d) as the server reported it.
type Window struct {
	Utilization float64 // 0.0-1.0; 1.0 means exhausted. Read only with HasUtilization.
	// HasUtilization is false when the server reported no usable utilization
	// for this window. Callers must render "unknown", never "0%".
	HasUtilization bool
	Status         string    // "allowed" | "rejected"
	ResetsAt       time.Time // when this window rolls over; zero when unknown
	Known          bool      // false when the server sent nothing for it
}

// Snapshot is one response's worth of usage state for one account.
type Snapshot struct {
	FiveHour      Window
	SevenDay      Window
	Overall       string    // Unified-Status: "allowed" | "rejected"
	BindingWindow string    // Representative-Claim: which window is binding
	Reset         time.Time // top-level Unified-Reset; the per-window fallback
	OverageStatus string    // whether paid overage is available…
	OverageReason string    // …and why not. NOT a limit signal.
	RetryAfter    time.Duration
	At            time.Time
	Known         bool // false when the response carried no unified headers
}

// Verdict is the answer to "is this account out of quota?".
type Verdict struct {
	Limited bool
	// Until is when the binding window clears. ZERO MEANS UNKNOWN, NOT
	// EXPIRED: callers must not compare it against the clock to decide the
	// limit has lifted, or a missing reset header un-limits the account
	// instantly and sends the next request back to the one that refused it.
	Until  time.Time
	Window string // which window said no
	Reason string // why it was or was not classified; sanitised, never body text
	// Refusal reports whether the response was a refusal (status >= 400),
	// whether or not it was classified as a limit — NOT the same set as
	// Limited. It is what Reason's consumer (status.Account.Reason, R33)
	// gates on: an operator needs to read WHY chottag declined to classify
	// a refusal, which is three of Classify's four refusal branches, not
	// just the one that reached Limited=true.
	Refusal bool
}

// Parse reads the unified rate-limit headers. A response without them yields
// Known=false rather than a zero-valued "everything is fine" snapshot.
func Parse(h http.Header, now time.Time) Snapshot {
	s := Snapshot{
		Overall:       h.Get(prefix + "Status"),
		BindingWindow: h.Get(prefix + "Representative-Claim"),
		OverageStatus: h.Get(prefix + "Overage-Status"),
		OverageReason: h.Get(prefix + "Overage-Disabled-Reason"),
		At:            now,
	}
	s.FiveHour = parseWindow(h, "5h")
	s.SevenDay = parseWindow(h, "7d")
	s.Reset = parseEpoch(h.Get(prefix + "Reset"))
	s.RetryAfter = parseRetryAfter(h.Get("Retry-After"), now)
	s.Known = s.Overall != "" || s.FiveHour.Known || s.SevenDay.Known
	return s
}

func parseWindow(h http.Header, tag string) Window {
	w := Window{Status: h.Get(prefix + tag + "-Status")}
	if f, ok := parseUtilization(h.Get(prefix + tag + "-Utilization")); ok {
		w.Utilization, w.HasUtilization = f, true
	}
	w.ResetsAt = parseEpoch(h.Get(prefix + tag + "-Reset"))
	w.Known = w.Status != "" || w.HasUtilization || !w.ResetsAt.IsZero()
	return w
}

// parseUtilization accepts only a finite fraction in [0, 1]. A NaN would
// render as "NaN%" and poison every threshold comparison; anything outside
// the range is not a fraction the server meant.
func parseUtilization(v string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
		return 0, false
	}
	return f, true
}

// minPlausibleEpoch rejects a reset time that cannot be real. A "-1" parses
// happily to 1969, and a past reset is worse than no reset: it reads as
// "already cleared".
const minPlausibleEpoch = 1735689600 // 2025-01-01T00:00:00Z

func parseEpoch(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < minPlausibleEpoch {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// parseRetryAfter accepts both RFC 7231 forms: delta-seconds and HTTP-date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// Classify decides whether a response means the account is out of quota.
//
// The rule (spec §6.2): the account is limited when the unified
// status is "rejected" AND the window named by Representative-Claim also
// reports "rejected". Any other shape is not classified — including a
// rejected overall status with no window owning it, which is where a false
// "limited" would come from.
//
// It deliberately gates on status >= 400 rather than on 429: the headers
// state the verdict outright, so a 500 carrying them is still a limit.
func Classify(status int, h http.Header, now time.Time) Verdict {
	if status < 400 {
		return Verdict{Reason: "not a refusal"}
	}
	s := Parse(h, now)
	if !s.Known {
		return Verdict{Refusal: true, Reason: "refusal carried no unified rate-limit headers; not treated as a limit"}
	}
	if !strings.EqualFold(s.Overall, "rejected") {
		return Verdict{Refusal: true, Reason: "unified status is " + sanitize(s.Overall) + "; not a quota refusal"}
	}
	w, name := s.window(s.BindingWindow)
	if !w.Known || !strings.EqualFold(w.Status, "rejected") {
		// The overall status says rejected but no window owns it. Do not
		// guess which one: report it so the rule can be improved.
		return Verdict{Refusal: true, Reason: "unified status rejected but binding window " + sanitize(s.BindingWindow) + " is not rejected"}
	}
	return Verdict{
		Limited: true,
		Until:   s.until(w, now),
		Window:  name,
		Reason:  "window " + name + " rejected",
		Refusal: true,
	}
}

// until resolves the clearing time: the binding window's own reset, else the
// top-level reset, else Retry-After from now. Zero when none is plausible —
// which means unknown, never expired.
func (s Snapshot) until(w Window, now time.Time) time.Time {
	if !w.ResetsAt.IsZero() {
		return w.ResetsAt
	}
	if !s.Reset.IsZero() {
		return s.Reset
	}
	if s.RetryAfter > 0 {
		return now.Add(s.RetryAfter)
	}
	return time.Time{}
}

// window resolves Representative-Claim to the window it names.
func (s Snapshot) window(claim string) (Window, string) {
	switch strings.ToLower(claim) {
	case "five_hour", "5h":
		return s.FiveHour, "five_hour"
	case "seven_day", "7d":
		return s.SevenDay, "seven_day"
	}
	return Window{}, sanitize(claim)
}

// sanitize makes an upstream-controlled header value safe to print to a
// terminal and write to a log file. Reason strings reach both, and the far
// end of a TLS-intercepting proxy should never get to emit an escape
// sequence or forge a log line.
func sanitize(s string) string {
	const max = 32
	var b strings.Builder
	for i, r := range s {
		if i >= max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	if b.Len() == 0 {
		return "<unrecognised>"
	}
	return b.String()
}
