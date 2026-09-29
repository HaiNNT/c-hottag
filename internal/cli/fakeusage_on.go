//go:build chottag_fakeusage

// This file is compiled only into a binary built with
// -tags chottag_fakeusage (spec §10.1, R52). It exists for testing the
// "all accounts limited" UI by hand, where one account is really limited
// and the others are simulated. fakeusage_off.go is its no-op twin in
// every other build, and cmd/chottag's TestReleaseBinaryHasNoFakeLimitHook
// proves the twin is what ships.

package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const (
	fakeLimitEnv    = "CHOTTAG_FAKE_LIMIT"
	fakeLimitTTLEnv = "CHOTTAG_FAKE_LIMIT_TTL"
	fakeLimitTTL    = time.Hour
	// fakeSimulated is both the row's window and its usage source: no
	// header window is named "simulated", so allowed real traffic can
	// never clear the fake by the §6.2 window rule. The TTL only sets
	// LimitedUntil; in the cache the fake actually ends when the reset
	// poll at until+60s reads both windows under 100%, or when real
	// traffic brings a real limit that replaces it.
	fakeSimulated = "simulated"
	fakeReason    = "simulated (CHOTTAG_FAKE_LIMIT)"
)

// A tagged build names itself, so it can't be mistaken for the user's
// install: `chottag version` and the daemon's health document both carry
// the suffix. init runs after -ldflags -X has set Version.
func init() { Version += "+fakeusage" }

// applyFakeLimits marks the accounts named in CHOTTAG_FAKE_LIMIT limited in
// the status cache, once, at daemon start (spec §10.1). It fakes cached
// state only, never a response: from here on the limit is an ordinary one
// with a known reset. getenv is os.Getenv in production.
func applyFakeLimits(getenv func(string) string, state func() (store.State, error), sink *statusSink, log io.Writer, now time.Time) {
	raw := getenv(fakeLimitEnv)
	if strings.TrimSpace(raw) == "" {
		return
	}
	st, err := state()
	if err != nil {
		fmt.Fprintf(log, "chottag: fake limit: %v\n", err)
		return
	}
	ttl := fakeLimitTTL
	if v := getenv(fakeLimitTTLEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ttl = d
		} else {
			fmt.Fprintf(log, "chottag: fake limit: %s=%q is not a positive duration; using %s\n", fakeLimitTTLEnv, v, fakeLimitTTL)
		}
	}
	var names []string
	seen := map[string]bool{}
	for _, n := range strings.Split(raw, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		// Exact and case-insensitive, as the roster matches: never Find's
		// prefix or email resolution.
		a, ok := findExact(&st, n)
		if !ok {
			fmt.Fprintf(log, "chottag: fake limit: no account named %q; ignored\n", n)
			continue
		}
		if key := strings.ToLower(a.Name); !seen[key] {
			seen[key] = true
			names = append(names, a.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	until := now.Add(ttl).Truncate(time.Second)
	sink.simulateLimits(names, until, now)
	fmt.Fprintf(log, "chottag: fake limit (chottag_fakeusage build) %s until %s\n", strings.Join(names, ","), until.Format(time.RFC3339))
}

// simulateLimits writes the fake limit under the sink's mutex and flushes
// it at once, like observe's new limit (contract 4).
//
// The row's usage is replaced by a fresh, empty one (source "simulated",
// percentages unknown). Under R54/F161, knownLimit honours a known future
// LimitedUntil however old the usage is, so the fake blocks `next` until
// LimitedUntil (now + TTL) regardless. The fresh stamp is kept anyway: as
// a backstop, and so the poller's first Cached view sees Fresh, which
// seeds a reset poll rather than a start poll.
func (c *statusSink) simulateLimits(names []string, until, now time.Time) {
	c.mu.Lock()
	for _, n := range names {
		i := -1
		for j := range c.file.Accounts {
			if strings.EqualFold(c.file.Accounts[j].Name, n) {
				i = j
				break
			}
		}
		if i < 0 {
			c.file.Accounts = append(c.file.Accounts, status.Account{Name: n})
			i = len(c.file.Accounts) - 1
		}
		a := &c.file.Accounts[i]
		a.Usage = &status.Usage{UpdatedAt: now.Round(0), Source: fakeSimulated}
		a.Limited, a.LimitedUntil, a.Window, a.Reason = true, until, fakeSimulated, fakeReason
	}
	// EnsureAccounts over every existing row prunes nothing and re-rolls
	// Limits with the write path's own clockless roll-up.
	rows := make([]string, len(c.file.Accounts))
	for i, a := range c.file.Accounts {
		rows[i] = a.Name
	}
	c.file.EnsureAccounts(rows)
	c.mu.Unlock()
	c.flush()
}
