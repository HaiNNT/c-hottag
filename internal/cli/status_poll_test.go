package cli

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

func sinkPollResult(five, seven float64, reset, at time.Time) usagepoll.Result {
	return usagepoll.Result{
		FiveHour: usagehdr.Window{Utilization: five, HasUtilization: true, ResetsAt: reset, Known: true},
		SevenDay: usagehdr.Window{Utilization: seven, HasUtilization: true, ResetsAt: reset, Known: true},
		At:       at,
	}
}

// limitHeaders is a real weekly-limit refusal's header set (§6.2).
func sinkLimitHeaders(reset time.Time) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1.0")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

func TestSinkPollFlushesANewLimitAndReportsIt(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.interval = time.Hour // only a flush can write inside the window
	waitFor, assertNone := newWriteWatcher(t, sink)

	now := time.Now().Round(0)
	reset := now.Add(2 * time.Hour).Truncate(time.Second)
	got := sink.poll("A", sinkPollResult(0.2, 0.3, reset, now), now)
	if !got.Written || got.Limited {
		t.Fatalf("applied = %+v, want written and unlimited", got)
	}
	waitFor() // the first write of the run is never coalesced
	sink.poll("A", sinkPollResult(0.25, 0.3, reset, now.Add(time.Second)), now.Add(time.Second))
	assertNone() // no limit change: coalesced

	got = sink.poll("A", sinkPollResult(1, 0.3, reset, now.Add(2*time.Second)), now.Add(2*time.Second))
	if !got.Written || !got.Limited || !got.Until.Equal(reset) {
		t.Fatalf("applied = %+v, want written, limited until %v", got, reset)
	}
	waitFor() // a new limit is flushed at once

	// Spec §6.4 item 7: a new OR CLEARED limit is flushed at once, not just
	// a new one. Both windows now read back under 100%.
	got = sink.poll("A", sinkPollResult(0.1, 0.2, reset, now.Add(3*time.Second)), now.Add(3*time.Second))
	if !got.Written || got.Limited {
		t.Fatalf("applied = %+v, want written and cleared", got)
	}
	waitFor() // a cleared limit is flushed at once too

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Limited || f.Accounts[0].Usage.Source != "polled" {
		t.Fatalf("on disk = %+v, want A cleared with source polled", f.Accounts)
	}
}

func TestSinkPollLosesToAFresherObservation(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sent := time.Now().Round(0)
	reset := sent.Add(time.Hour).Truncate(time.Second)
	sink.observe("A", usagehdr.Parse(sinkLimitHeaders(reset), sent.Add(time.Second)), usagehdr.Classify(429, sinkLimitHeaders(reset), sent.Add(time.Second)))

	got := sink.poll("A", sinkPollResult(0.1, 0.1, reset, sent.Add(2*time.Second)), sent)
	if got.Written {
		t.Fatal("a poll sent before the observation overwrote it")
	}
	if !got.Limited || !got.Until.Equal(reset) {
		t.Fatalf("applied = %+v, want the cache's current limit (until %v) reported back", got, reset)
	}
}

func TestSinkCachedReportsFreshnessAndLimit(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	now := time.Now().Round(0)
	if v := sink.cached("A", now); v.Fresh || v.Limited {
		t.Fatalf("unknown account: %+v, want not fresh, not limited", v)
	}
	reset := now.Add(time.Hour).Truncate(time.Second)
	sink.poll("A", sinkPollResult(1, 0.2, reset, now), now)
	if v := sink.cached("A", now); !v.Fresh || !v.Limited || !v.Until.Equal(reset) {
		t.Fatalf("after a limiting poll: %+v", v)
	}
	if v := sink.cached("a", now.Add(status.StaleAfter)); v.Fresh {
		t.Fatalf("10 minutes later (and case-folded): %+v, want stale", v)
	}
}

type observedCall struct {
	account string
	limited bool
	until   time.Time
}

func TestSinkObserveTellsThePollerOnlyWhatItLearned(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var calls []observedCall
	sink.setOnObserved(func(account string, limited bool, until time.Time) {
		// Called under sink.mu: TryLock must fail. This pins the lock
		// order the poller relies on (spec §6.4).
		if sink.mu.TryLock() {
			sink.mu.Unlock()
			t.Error("onObserved ran without the sink's lock held")
		}
		calls = append(calls, observedCall{account, limited, until})
	})
	now := time.Now().Round(0)
	reset := now.Add(time.Hour).Truncate(time.Second)
	sink.observe("A", usagehdr.Parse(sinkLimitHeaders(reset), now), usagehdr.Classify(429, sinkLimitHeaders(reset), now))
	// A headerless response learned nothing: no call.
	sink.observe("A", usagehdr.Parse(http.Header{}, now), usagehdr.Classify(500, http.Header{}, now))
	if len(calls) != 1 || calls[0].account != "A" || !calls[0].limited || !calls[0].until.Equal(reset) {
		t.Fatalf("calls = %+v, want exactly one (A, limited, %v)", calls, reset)
	}
}

// TestSinkObserveAndPollRaceOnTheSameAccount runs observe and poll
// concurrently, on the same account, many times over: with -race this
// covers the sink's mutex the way TestStatusSinkObserveIsRaceSafeUnderConcurrentUse
// already does for observe alone (spec §6.4 requires poll go through the
// same mutex as observe, via the same single writer).
//
// Each call is tagged with a globally unique "at"/"sent" timestamp (a
// shared atomic sequence) and recorded, before the call, in a map keyed by
// that timestamp. Every field status.File.Poll and status.File.Observe
// write together — Source, the two window percentages and UpdatedAt — is
// set under one lock acquisition, so whichever attempt's timestamp the
// final row carries must carry that SAME attempt's source and percentages
// too. A row whose fields came from two different attempts (Source from
// one call, a percentage from another — the mutex silently narrowed or
// dropped) fails this exact-match lookup: that is what "never a torn mix"
// means here.
func TestSinkObserveAndPollRaceOnTheSameAccount(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	const n = 200
	base := time.Now().Round(0)
	var seq int64

	type attempt struct {
		source      string
		five, seven float64
	}
	var recMu sync.Mutex
	attempts := make(map[time.Time]attempt, 2*n)
	record := func(at time.Time, a attempt) {
		recMu.Lock()
		attempts[at] = a
		recMu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			off := atomic.AddInt64(&seq, 1)
			at := base.Add(time.Duration(off) * time.Millisecond)
			five := float64(i%80) / 1000
			seven := five + 0.0002
			record(at, attempt{source: "observed", five: five * 100, seven: seven * 100})
			sink.observe("A", usagehdr.Snapshot{
				Known: true, At: at, Overall: "allowed",
				FiveHour: usagehdr.Window{Known: true, HasUtilization: true, Utilization: five, Status: "allowed"},
				SevenDay: usagehdr.Window{Known: true, HasUtilization: true, Utilization: seven, Status: "allowed"},
			}, usagehdr.Verdict{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			off := atomic.AddInt64(&seq, 1)
			at := base.Add(time.Duration(off) * time.Millisecond)
			five := 0.5 + float64(i%80)/1000
			seven := five + 0.0002
			record(at, attempt{source: "polled", five: five * 100, seven: seven * 100})
			sink.poll("A", sinkPollResult(five, seven, time.Time{}, at), at)
		}
	}()
	wg.Wait()

	sink.mu.Lock()
	var got status.Account
	for _, a := range sink.file.Accounts {
		if a.Name == "A" {
			got = a
		}
	}
	sink.mu.Unlock()

	if got.Usage == nil {
		t.Fatal("no usage row after concurrent observe/poll")
	}
	recMu.Lock()
	want, ok := attempts[got.Usage.UpdatedAt]
	recMu.Unlock()
	if !ok {
		t.Fatalf("final updatedAt %v matches no attempted write: row = %+v", got.Usage.UpdatedAt, got.Usage)
	}
	if got.Usage.Source != want.source {
		t.Fatalf("torn write: source = %q, want %q (the attempt that set updatedAt %v)", got.Usage.Source, want.source, got.Usage.UpdatedAt)
	}
	if got.Usage.FiveHourPct == nil || got.Usage.SevenDayPct == nil {
		t.Fatalf("row = %+v, want both windows set", got.Usage)
	}
	if *got.Usage.FiveHourPct != want.five || *got.Usage.SevenDayPct != want.seven {
		t.Fatalf("torn write: five/seven = %v/%v, want %v/%v (the attempt that set updatedAt %v)",
			*got.Usage.FiveHourPct, *got.Usage.SevenDayPct, want.five, want.seven, got.Usage.UpdatedAt)
	}
}
