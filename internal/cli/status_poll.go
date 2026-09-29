package cli

import (
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

// setOnObserved installs the usage poller's observe hook (spec §6.4). See
// the onObserved field for the lock-order contract.
func (c *statusSink) setOnObserved(fn func(account string, limited bool, until time.Time)) {
	c.mu.Lock()
	c.onObserved = fn
	c.mu.Unlock()
}

// cached is usagepoll.Config.Cached: what the cache says about one account
// right now, read when the account joins the poller's roster.
func (c *statusSink) cached(account string, now time.Time) usagepoll.CacheView {
	c.mu.Lock()
	defer c.mu.Unlock()
	limited, until := c.limitStateLocked(account)
	return usagepoll.CacheView{Fresh: c.file.Fresh(account, now), Limited: limited, Until: until}
}

// poll is usagepoll.Config.Apply: folds one poll result into the cache
// under the same mutex as observe, through the same single writer (spec
// §6.4). A limit that starts or clears is flushed at once, like observe's
// new limit (contract 4); anything else rides the coalescing window. After
// Close the fold still happens in memory but queueLocked drops the write.
func (c *statusSink) poll(account string, r usagepoll.Result, sent time.Time) usagepoll.Applied {
	c.mu.Lock()
	wasLimited := c.limitedLocked(account)
	written := c.file.Poll(account, r.FiveHour, r.SevenDay, sent, r.At)
	limited, until := c.limitStateLocked(account)
	c.mu.Unlock()

	if written {
		if wasLimited != limited {
			c.flush()
		} else {
			c.maybeSave()
		}
	}
	return usagepoll.Applied{Written: written, Limited: limited, Until: until}
}

// limitStateLocked reads one account's limit and clearing time. Caller
// holds c.mu.
func (c *statusSink) limitStateLocked(account string) (bool, time.Time) {
	for _, a := range c.file.Accounts {
		if strings.EqualFold(a.Name, account) {
			return a.Limited, a.LimitedUntil
		}
	}
	return false, time.Time{}
}
