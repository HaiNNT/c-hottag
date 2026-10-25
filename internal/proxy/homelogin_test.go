package proxy_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

// TestOnHomeLoginReportsTheClientsOwnLogin: a final response that came on the
// client's own OAuth login (never swapped, or resent on it by the safety net)
// reports ok=false for 401 and ok=true for 2xx. A swapped response, another
// status, or a request without an OAuth access token reports nothing (R183).
func TestOnHomeLoginReportsTheClientsOwnLogin(t *testing.T) {
	home := [2]string{"Authorization", "Bearer sk-ant-oat01-home"}
	settings := "https://api.anthropic.com/api/claude_code/settings"
	messages := "https://api.anthropic.com/v1/messages"
	onHome := func(r *http.Request, onHome, otherwise int) int {
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
			return onHome
		}
		return otherwise
	}
	cases := []struct {
		name     string
		choose   bool // a chooser that swaps onto the refused account C
		auth     [2]string
		url      string
		upstream func(r *http.Request) int
		want     []bool
	}{
		{"untouched 401", false, home, settings, func(*http.Request) int { return 401 }, []bool{false}},
		{"untouched 200", false, home, settings, func(*http.Request) int { return 200 }, []bool{true}},
		{"untouched 500", false, home, settings, func(*http.Request) int { return 500 }, nil},
		{"api key is not an oauth login", false, [2]string{"X-Api-Key", "sk-ant-api03-key"}, settings,
			func(*http.Request) int { return 401 }, nil},
		{"swapped and served", true, home, messages,
			func(r *http.Request) int { return onHome(r, 401, 200) }, nil},
		{"swapped refused, resent on a good Home login", true, home, messages,
			func(r *http.Request) int { return onHome(r, 200, 401) }, []bool{true}},
		{"swapped refused, resent on an expired Home login", true, home, messages,
			func(*http.Request) int { return 401 }, []bool{false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.upstream(r))
			})
			var mu sync.Mutex
			var got []bool
			opts := proxytest.Options{
				OnServingRefusal: func(string, int, string, bool, string, string) {},
				OnHomeLogin: func(ok bool) {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, ok)
				},
			}
			if c.choose {
				opts.Choose = &refusingChooser{}
			}
			h := proxytest.Start(t, up, opts)
			req, _ := http.NewRequest("GET", c.url, strings.NewReader(`{}`))
			req.Header.Set(c.auth[0], c.auth[1])
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
				t.Fatalf("reports = %v, want %v", got, c.want)
			}
		})
	}
}
