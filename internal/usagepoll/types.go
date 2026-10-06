// Package usagepoll is the daemon's /api/oauth/usage fallback poll (spec
// §6.4, M1c6b, R47). Usage is observed from proxied responses first (F23,
// §6.3); this package only covers an account that has sent no traffic and
// re-checks a limited account so its reset is noticed.
//
// It polls on events (daemon start, a limited account's reset, a wake from
// sleep, a roster change) and, for an account that is neither limited nor
// in need of a login, on an idle schedule: IdleEvery after its last usage
// update, so no account's usage goes stale for good (F268). One worker, one
// request in flight, at most one pending poll per account.
//
// It never logs, stores or returns a token or a response body. The only
// body-derived values that leave this package are the two windows' usage
// and reset times, which status.json already holds.
package usagepoll

import (
	"context"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/usage"
)

// DefaultURL is the endpoint Claude Code's own /usage calls (F156). Only
// the daemon's production wiring passes it. FetchConfig.URL has no default,
// so a test that forgets to set one panics instead of reaching the real
// host.
const DefaultURL = "https://api.anthropic.com/api/oauth/usage"

// Result is one successful poll: the two windows the cache tracks, in
// usage.Window's own units (Utilization is a 0-1 fraction, 1.0 means
// exhausted), and when the response arrived.
type Result struct {
	FiveHour usage.Window
	SevenDay usage.Window
	At       time.Time
}

// Outcome is what one fetch learned.
type Outcome struct {
	// OK means Result is a parsed, usable poll. Anything else is a failed
	// poll, which never writes usage and never creates or clears a limit.
	OK     bool
	Result Result
	// GaveUp means a 401 survived one ForceRefresh and one retry (or the
	// refresh itself failed): no further poll until the account's next
	// event (spec §6.4).
	GaveUp bool
	// NoToken means the poll never went out: the slot's token was stale
	// and Token started a background refresh instead (spec §6.4 detail
	// 1). A start or reset poll skipped this way is retried once, TokenRetry later.
	NoToken bool
	// NeedsLogin means the slot holds no usable login: the poll never went
	// out, and none will until the account logs in again (F269).
	NeedsLogin bool
	// RetryAfter is a 429's Retry-After, 0 when absent or unusable.
	RetryAfter time.Duration
	// Status is the daemon.log detail: an HTTP status code (e.g. "200",
	// "429"), a "401>"-prefixed retry (e.g. "401>200", "401>error"),
	// "no-token", "401 refresh-failed", "timeout", "error", "200
	// unparsed" or "200 unpinned". Never a token, never body text.
	Status string
}

// Fetcher performs one poll for the slot whose CLAUDE_CONFIG_DIR is dir.
type Fetcher func(ctx context.Context, dir string) Outcome

// TokenSource is the part of *tokens.Manager a poll uses. An interface so
// no test ever reaches the real refresh path, which runs the real claude.
type TokenSource interface {
	Token(ctx context.Context, slotDir string) (string, creds.Status, bool)
	ForceRefresh(ctx context.Context, slotDir string) (string, bool)
}

// Account is one registered slot account. Home is never one: it has no
// slot, and chottag never holds its token.
type Account struct {
	Name string
	Dir  string
	// Rotates is false for a rotation-off account (the remote): it is
	// polled every OffEvery instead of IdleEvery.
	Rotates bool
	// LoggedInAt is when `chottag login` last confirmed the account; a
	// needs-login mark older than it no longer applies.
	LoggedInAt time.Time
}

// CacheView is what the status cache says about one account right now,
// read when an account first joins the poller's roster.
type CacheView struct {
	Fresh   bool      // usage present and younger than status.StaleAfter
	Limited bool      // the cache's limited flag
	Until   time.Time // the cache's limitedUntil; zero means unknown
	// UpdatedAt is when the cache's usage was last updated (zero: never).
	UpdatedAt time.Time
	// NeedsLogin is the status row's token state, with TokenAt the time it
	// was set.
	NeedsLogin bool
	TokenAt    time.Time
}

// Applied is what the cache write did with a Result.
type Applied struct {
	// Written is false when the cache already held usage newer than the
	// poll's send time: fresher observed data wins.
	Written bool
	// Limited and Until are the account's limit state after the write.
	Limited bool
	Until   time.Time
}

// wallNow is the default clock: a wall reading with the monotonic part
// stripped. Deadlines are wall-clock times, so a reset that passed while
// the machine slept is overdue on wake (monotonic time stops in sleep).
func wallNow() time.Time { return time.Now().Round(0) }
