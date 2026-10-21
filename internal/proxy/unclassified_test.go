package proxy_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

type unclassifiedRefusal struct {
	account      string
	status       int
	method, path string
}

// TestUnclassifiedRouteRefusalIsReported: a swapped request on a route the
// table does not list (the router's Serving fallback) that is refused 403 (or
// 401 with permission_error) is counted as route drift and as unclassified,
// and reported once with the templated path. An explicit route, or any other
// status, is not reported (R186).
func TestUnclassifiedRouteRefusalIsReported(t *testing.T) {
	cases := []struct {
		name, method, url, body string
		status                  int
		wantReports             int
		wantDrift               uint64
	}{
		{"unlisted 403", "GET", "https://api.anthropic.com/some/new/route/123e4567-e89b-12d3-a456-426614174000", "", 403, 1, 1},
		{"unlisted 401 permission_error", "GET", "https://api.anthropic.com/some/new/route", `{"error":{"type":"permission_error"}}`, 401, 1, 1},
		{"unlisted 401 plain", "GET", "https://api.anthropic.com/some/new/route", "", 401, 0, 1},
		{"unlisted 404", "GET", "https://api.anthropic.com/some/new/route", "", 404, 0, 1},
		{"explicit remote 403 is today's behaviour", "POST", "https://api.anthropic.com/api/frame/deploy/prepare", "", 403, 0, 1},
		{"explicit serving 403 is a serving refusal", "POST", "https://api.anthropic.com/v1/messages", "", 403, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
					w.WriteHeader(http.StatusOK)
					return
				}
				if c.body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			})
			var mu sync.Mutex
			var got []unclassifiedRefusal
			h := proxytest.Start(t, up, proxytest.Options{
				Choose:           &refusingChooser{},
				OnServingRefusal: func(string, int, string, bool, string, string) {},
				OnUnclassifiedRefusal: func(account string, status int, method, path string) {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, unclassifiedRefusal{account, status, method, path})
				},
			})
			req, _ := http.NewRequest(c.method, c.url, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if d := h.Server.RouteDrift(); d != c.wantDrift {
				t.Fatalf("RouteDrift = %d, want %d", d, c.wantDrift)
			}
			if u := h.Server.UnclassifiedDrift(); u != uint64(c.wantReports) {
				t.Fatalf("UnclassifiedDrift = %d, want %d", u, c.wantReports)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != c.wantReports {
				t.Fatalf("reports = %v, want %d", got, c.wantReports)
			}
			if c.wantReports == 1 {
				if got[0].account != "C" || got[0].status != c.status || got[0].method != "GET" || !strings.HasPrefix(got[0].path, "/some/new/route") || strings.Contains(got[0].path, "123e4567") {
					t.Fatalf("report = %+v, want account C, the status, GET and a templated path", got[0])
				}
			}
		})
	}
}
