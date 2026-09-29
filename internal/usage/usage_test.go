package usage_test

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usage"
)

// limitedHeaders is the real header set captured from a weekly-limited
// account on 2026-09-20. Do not "tidy" these values.
func limitedHeaders() http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.0")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1789881600")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1.0")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1790179200")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "out_of_credits")
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1790179200")
	h.Set("Retry-After", "315223")
	h.Set("X-Should-Retry", "true")
	return h
}

// healthyHeaders is the real header set captured from a 200 on an account
// with quota.
func healthyHeaders() http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.06")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1789876200")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.23")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1790420400")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "org_level_disabled")
	return h
}

const now0 = 1789870000 // a moment before every reset in the fixtures

func now() time.Time { return time.Unix(now0, 0) }

func TestParseHealthyResponse(t *testing.T) {
	s := usage.Parse(healthyHeaders(), now())
	if !s.Known {
		t.Fatal("snapshot not Known; want parsed")
	}
	if s.FiveHour.Utilization != 0.06 || s.SevenDay.Utilization != 0.23 {
		t.Errorf("utilization = %v/%v, want 0.06/0.23", s.FiveHour.Utilization, s.SevenDay.Utilization)
	}
	if !s.FiveHour.HasUtilization || !s.SevenDay.HasUtilization {
		t.Error("HasUtilization false for windows that reported one")
	}
	if s.Overall != "allowed" || s.BindingWindow != "five_hour" {
		t.Errorf("overall/binding = %q/%q, want allowed/five_hour", s.Overall, s.BindingWindow)
	}
	if got := s.FiveHour.ResetsAt.Unix(); got != 1789876200 {
		t.Errorf("5h reset = %d, want 1789876200", got)
	}
}

func TestParseReadsTheTopLevelResetAndRetryAfter(t *testing.T) {
	s := usage.Parse(limitedHeaders(), now())
	if got := s.Reset.Unix(); got != 1790179200 {
		t.Errorf("top-level Reset = %d, want 1790179200", got)
	}
	if s.RetryAfter != 315223*time.Second {
		t.Errorf("RetryAfter = %v, want 315223s", s.RetryAfter)
	}
}

// RFC 7231 allows Retry-After as an HTTP-date. It becomes the Until fallback
// below, so a date form silently parsing to 0 would be a wrong reset time.
func TestParseAcceptsAnHTTPDateRetryAfter(t *testing.T) {
	h := healthyHeaders()
	h.Set("Retry-After", time.Unix(now0+600, 0).UTC().Format(http.TimeFormat))
	if got := usage.Parse(h, now()).RetryAfter; got != 600*time.Second {
		t.Errorf("RetryAfter = %v, want 600s from an HTTP-date", got)
	}
}

func TestParseIgnoresAnAbsentHeaderSetEntirely(t *testing.T) {
	if usage.Parse(http.Header{}, now()).Known {
		t.Fatal("Known = true for headerless response; want false so callers report unknown")
	}
}

// A window that reports only its status has NO utilization. Reporting 0 would
// render as "0% of 5h used" to a user about to hit a wall.
func TestParseDoesNotInventAUtilizationForAStatusOnlyWindow(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	s := usage.Parse(h, now())
	if !s.FiveHour.Known {
		t.Fatal("window Known = false though the server reported its status")
	}
	if s.FiveHour.HasUtilization {
		t.Error("HasUtilization = true for a window that reported no utilization")
	}
}

func TestParseRejectsNonFiniteUtilization(t *testing.T) {
	for _, v := range []string{"NaN", "1e308", "-1", "2"} {
		h := healthyHeaders()
		h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", v)
		s := usage.Parse(h, now())
		if s.FiveHour.HasUtilization {
			t.Errorf("utilization %q accepted; want rejected (it renders to the user and feeds thresholds)", v)
		}
		if math.IsNaN(s.FiveHour.Utilization) {
			t.Errorf("utilization %q stored NaN", v)
		}
	}
}

func TestClassifyLimitedUsesTheBindingWindow(t *testing.T) {
	v := usage.Classify(429, limitedHeaders(), now())
	if !v.Limited {
		t.Fatal("Limited = false; want true for a rejected unified status")
	}
	if v.Window != "seven_day" {
		t.Errorf("Window = %q, want seven_day (Representative-Claim)", v.Window)
	}
	if got := v.Until.Unix(); got != 1790179200 {
		t.Errorf("Until = %d, want 1790179200 (the 7d reset, not the 5h one)", got)
	}
}

// The unified status says rejected but the named window does not. This is the
// branch a false "limited" would come from, and a false "limited" silently
// moves the user off an account that still works.
func TestClassifyDoesNotLimitWhenTheBindingWindowIsAllowed(t *testing.T) {
	h := limitedHeaders()
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour") // 5h is allowed
	v := usage.Classify(429, h, now())
	if v.Limited {
		t.Fatalf("Limited = true though the named window (five_hour) is allowed; verdict %+v", v)
	}
}

func TestClassifyDoesNotLimitWhenTheClaimNamesAnUnknownWindow(t *testing.T) {
	h := limitedHeaders()
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "one_hour")
	if usage.Classify(429, h, now()).Limited {
		t.Fatal("Limited = true for a claim naming a window the server never sent")
	}
}

func TestClassifyDoesNotLimitWhenTheClaimIsMissing(t *testing.T) {
	h := limitedHeaders()
	h.Del("Anthropic-Ratelimit-Unified-Representative-Claim")
	if usage.Classify(429, h, now()).Limited {
		t.Fatal("Limited = true with no Representative-Claim; nothing says which window owns the refusal")
	}
}

// A burst throttle is also rate_limit_error. Classify never sees error.type at
// all — this test pins that a 429 whose windows are allowed is not a limit.
func TestClassifyDoesNotLimitAThrottleWhoseWindowsAreAllowed(t *testing.T) {
	h := healthyHeaders()
	h.Set("Retry-After", "2")
	if usage.Classify(429, h, now()).Limited {
		t.Fatal("Limited = true for a 429 whose windows are all allowed")
	}
}

func TestClassifyUnknownShapeIsNotLimited(t *testing.T) {
	v := usage.Classify(429, http.Header{}, now())
	if v.Limited {
		t.Fatal("Limited = true with no rate-limit headers; an unrecognised refusal must not be treated as a limit")
	}
	if !strings.Contains(v.Reason, "no unified rate-limit headers") {
		t.Errorf("Reason = %q; want it to name the missing headers so the daemon log is diagnosable", v.Reason)
	}
}

// Refusal is not the same set as Limited (fix round 1, item 5): it is true
// on every status>=400 branch, including the three that decline to
// classify — the case R33's reason field exists for — and false only on
// the status<400 branch, regardless of what the headers say.
func TestClassifyRefusalReflectsStatusNotClassification(t *testing.T) {
	if v := usage.Classify(200, limitedHeaders(), now()); v.Refusal {
		t.Errorf("Refusal = true for a 2xx, want false regardless of headers")
	}
	if v := usage.Classify(429, http.Header{}, now()); !v.Refusal {
		t.Error("Refusal = false for an unclassified refusal (no headers), want true")
	}
	if v := usage.Classify(429, healthyHeaders(), now()); !v.Refusal {
		t.Error("Refusal = false for a refusal whose headers say allowed, want true")
	}
	if v := usage.Classify(429, limitedHeaders(), now()); !v.Refusal {
		t.Error("Refusal = false for a refusal classified as limited, want true")
	}
}

func TestClassifySuccessIsNeverLimited(t *testing.T) {
	if usage.Classify(200, limitedHeaders(), now()).Limited {
		t.Fatal("a 2xx must never be classified as limited, whatever the headers say")
	}
}

// Overage-Status is "rejected" on a perfectly healthy account.
func TestClassifyIgnoresOverageStatus(t *testing.T) {
	if usage.Classify(429, healthyHeaders(), now()).Limited {
		t.Fatal("Overage-Status: rejected must not make an account limited")
	}
}

// The headers state the verdict outright, so a non-429 refusal carrying them
// is still a limit. Pinned because it is a deliberate choice, not an accident.
func TestClassifyHonoursTheHeadersOnANon429Refusal(t *testing.T) {
	if !usage.Classify(500, limitedHeaders(), now()).Limited {
		t.Fatal("Limited = false for a 500 whose headers say rejected")
	}
}

func TestClassifyIsCaseInsensitiveOnStatusValues(t *testing.T) {
	h := limitedHeaders()
	h.Set("Anthropic-Ratelimit-Unified-Status", "REJECTED")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "Rejected")
	if !usage.Classify(429, h, now()).Limited {
		t.Fatal("Limited = false for upper-case status values")
	}
}

// Until falls back: binding window reset -> top-level Reset -> now+Retry-After.
func TestClassifyFallsBackToTheTopLevelReset(t *testing.T) {
	h := limitedHeaders()
	h.Del("Anthropic-Ratelimit-Unified-7d-Reset")
	v := usage.Classify(429, h, now())
	if !v.Limited {
		t.Fatal("Limited = false; the window is still rejected")
	}
	if got := v.Until.Unix(); got != 1790179200 {
		t.Errorf("Until = %d, want the top-level Reset 1790179200", got)
	}
}

func TestClassifyFallsBackToRetryAfter(t *testing.T) {
	h := limitedHeaders()
	h.Del("Anthropic-Ratelimit-Unified-7d-Reset")
	h.Del("Anthropic-Ratelimit-Unified-Reset")
	h.Set("Retry-After", "3600")
	if got := usage.Classify(429, h, now()).Until.Unix(); got != now0+3600 {
		t.Errorf("Until = %d, want now+3600 = %d", got, now0+3600)
	}
}

// A reset in the past is not a reset. Zero Until means UNKNOWN, and Task 2's
// cache must treat it as unknown rather than as "already expired" — otherwise
// chottag un-limits the account instantly and rotates back onto the one that
// just refused it.
func TestClassifyLeavesUntilZeroWhenNoResetIsPlausible(t *testing.T) {
	for _, bad := range []string{"", "not-a-number", "-1", "1"} {
		h := limitedHeaders()
		h.Set("Anthropic-Ratelimit-Unified-7d-Reset", bad)
		h.Set("Anthropic-Ratelimit-Unified-Reset", bad)
		h.Del("Retry-After")
		v := usage.Classify(429, h, now())
		if !v.Limited {
			t.Fatalf("%q: Limited = false; the window is rejected regardless of its reset", bad)
		}
		if !v.Until.IsZero() {
			t.Errorf("%q: Until = %v, want zero (unknown), never a past time", bad, v.Until)
		}
	}
}

// Header values come from the far end of a TLS-intercepting proxy, and Reason
// is printed to a terminal and written to a log file.
func TestClassifyReasonCannotCarryControlCharacters(t *testing.T) {
	h := limitedHeaders()
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven\x1b[2Jday\x07\n")
	v := usage.Classify(429, h, now())
	for _, bad := range []string{"\x1b", "\x07", "\n"} {
		if strings.Contains(v.Reason, bad) {
			t.Errorf("Reason %q carries control character %q from an upstream header", v.Reason, bad)
		}
	}
}
