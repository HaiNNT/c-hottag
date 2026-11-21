package proxy_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

const (
	lrFallback = "https://api.anthropic.com/some/new/route"
	lrBoot     = "https://api.anthropic.com/api/claude_cli/bootstrap"
	lrMessages = "https://api.anthropic.com/v1/messages"
)

type lrReports struct {
	mu       sync.Mutex
	serving  []string // "account status method path"
	unclass  int
	loginFor func(string) bool
}

func (r *lrReports) servingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.serving)
}

func (r *lrReports) unclassifiedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unclass
}

// lrStart starts a harness whose upstream answers by path: status[path], 200
// for the Home token and for a path without an entry on a non-refused path.
func lrStart(t *testing.T, status map[string]int, marked func(string) bool, now func() time.Time) (*proxytest.Harness, *lrReports) {
	t.Helper()
	rep := &lrReports{}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if st, ok := status[r.URL.Path]; ok {
			w.WriteHeader(st)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := proxytest.Start(t, up, proxytest.Options{
		Choose:       &refusingChooser{},
		LoginRefused: marked,
		Now:          now,
		OnServingRefusal: func(account string, st int, _ string, _ bool, method, path string) {
			rep.mu.Lock()
			defer rep.mu.Unlock()
			rep.serving = append(rep.serving, account+" "+http.StatusText(st)+" "+method+" "+path)
		},
		OnUnclassifiedRefusal: func(string, int, string, string) {
			rep.mu.Lock()
			defer rep.mu.Unlock()
			rep.unclass++
		},
	})
	return h, rep
}

func lrDo(t *testing.T, h *proxytest.Harness, method, url string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func lrCounts(t *testing.T, h *proxytest.Harness, rep *lrReports, serving, unclass int, drift uint64) {
	t.Helper()
	if got := rep.servingCount(); got != serving {
		t.Fatalf("serving reports = %d (%v), want %d", got, rep.serving, serving)
	}
	if got := rep.unclassifiedCount(); got != unclass {
		t.Fatalf("unclassified reports = %d, want %d", got, unclass)
	}
	if got := h.Server.RouteDrift(); got != drift {
		t.Fatalf("RouteDrift = %d, want %d", got, drift)
	}
}

func lrAlways(string) bool { return true }
func lrNever(string) bool  { return false }

func TestMarkedAccountFallbackRefusalIsALoginRefusal(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, lrAlways, nil)
	lrDo(t, h, "GET", lrFallback)
	lrCounts(t, h, rep, 1, 0, 0)
	if !strings.HasPrefix(rep.serving[0], "C ") || !strings.HasSuffix(rep.serving[0], "/some/new/route") {
		t.Fatalf("report = %q", rep.serving[0])
	}
}

func TestParkedFallbackRefusalSettlesAsLoginOnServingRefusal(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/api/claude_cli/bootstrap": 403, "/v1/messages": 403}, lrNever, nil)
	lrDo(t, h, "GET", lrBoot)
	lrCounts(t, h, rep, 0, 0, 0)
	lrDo(t, h, "POST", lrMessages)
	lrCounts(t, h, rep, 2, 0, 0)
}

func TestParkedFallbackRefusalSettlesAsRouteOnServingSuccess(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, lrNever, nil)
	lrDo(t, h, "GET", lrFallback)
	lrCounts(t, h, rep, 0, 0, 0)
	lrDo(t, h, "POST", lrMessages)
	lrCounts(t, h, rep, 0, 1, 1)
}

func TestParkedFallbackRefusalNotSettledByAnotherFallbackSuccess(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, lrNever, nil)
	lrDo(t, h, "GET", lrFallback)
	lrDo(t, h, "GET", "https://api.anthropic.com/another/new/route")
	lrCounts(t, h, rep, 0, 0, 0)
}

func TestFallback404IsDriftAtOnce(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 404}, lrNever, nil)
	lrDo(t, h, "GET", lrFallback)
	lrCounts(t, h, rep, 0, 0, 1)
}

func TestRemoteSuccessSettlesNothing(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, lrNever, nil)
	lrDo(t, h, "GET", lrFallback)
	lrDo(t, h, "POST", "https://api.anthropic.com/api/frame/deploy/prepare")
	lrCounts(t, h, rep, 0, 0, 0)
}

func TestParkedRefusalExpiry(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	set := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); clock = clock.Add(d) }

	t.Run("unmarked at expiry is route", func(t *testing.T) {
		h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, lrNever, now)
		lrDo(t, h, "GET", lrFallback)
		set(9 * time.Minute)
		h.Server.SettleRefusals(now())
		lrCounts(t, h, rep, 0, 0, 0)
		set(time.Minute)
		h.Server.SettleRefusals(now())
		lrCounts(t, h, rep, 0, 1, 1)
	})
	t.Run("marked at expiry is login", func(t *testing.T) {
		var m sync.Mutex
		on := false
		marked := func(string) bool { m.Lock(); defer m.Unlock(); return on }
		h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, marked, now)
		lrDo(t, h, "GET", lrFallback)
		m.Lock()
		on = true
		m.Unlock()
		set(10 * time.Minute)
		h.Server.SettleRefusals(now())
		lrCounts(t, h, rep, 1, 0, 0)
	})
}

func TestParkedOverflowSettlesAsRouteAtOnce(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, lrNever, nil)
	for i := 0; i < 32; i++ {
		lrDo(t, h, "GET", lrFallback)
	}
	lrCounts(t, h, rep, 0, 0, 0)
	lrDo(t, h, "GET", lrFallback)
	lrCounts(t, h, rep, 0, 1, 1)
}

func TestLoginRefusedNilKeepsTodaysBehaviour(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403}, nil, nil)
	lrDo(t, h, "GET", lrFallback)
	lrCounts(t, h, rep, 0, 1, 1)
}

func TestParkedRefusalsConcurrent(t *testing.T) {
	h, rep := lrStart(t, map[string]int{"/some/new/route": 403, "/v1/messages": 403}, lrNever, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); lrDo(t, h, "GET", lrFallback) }()
		go func() { defer wg.Done(); lrDo(t, h, "POST", lrMessages) }()
	}
	wg.Wait()
	// A fallback refusal parked after the last serving refusal stays parked
	// (this test never marks the account); settling past the wait reports it
	// as route drift. Then every request is reported exactly once, as login
	// (serving) or route (drift); none is lost or doubled.
	h.Server.SettleRefusals(time.Now().Add(time.Hour))
	total := rep.servingCount() + int(h.Server.RouteDrift())
	if total != 16 {
		t.Fatalf("serving %d + drift %d, want 16 in all", rep.servingCount(), h.Server.RouteDrift())
	}
}
