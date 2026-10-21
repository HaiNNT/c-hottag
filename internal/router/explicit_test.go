package router_test

import (
	"testing"

	"github.com/HaiNNT/c-hottag/internal/router"
)

// Every rule's own example reports true.
func TestExplicitRuleExamples(t *testing.T) {
	cases := []router.Request{
		{Method: "GET", Host: api, Path: "/worker"},
		{Method: "GET", Host: api, Path: "/v1/oauth/token"},
		{Method: "GET", Host: api, Path: "/api/claude_code/settings"},
		{Method: "POST", Host: api, Path: "/v1/code/sessions"},
		{Method: "GET", Host: api, Path: "/v1/sessions/s1/events"},
		{Method: "POST", Host: api, Path: "/v1/environments/bridge"},
		{Method: "POST", Host: api, Path: "/v1/environments/bridge", RawQuery: "beta=true"}, // untouched, still a rule
		{Method: "POST", Host: api, Path: "/api/oauth/validate"},
		{Method: "POST", Host: api, Path: "/api/oauth/validate", AbsoluteForm: true},
		{Method: "POST", Host: api, Path: "/api/frame/deploy/prepare"},
		{Method: "POST", Host: api, Path: "/v1/messages"},
		{Method: "GET", Host: api, Path: "/v1/mcp_servers"},
		{Method: "POST", Host: "API.anthropic.com:443", Path: "/v1/mcp_servers/x"},
		{Method: "POST", Host: mcp, Path: "/v1/mcp/srv-1/tools"},
	}
	for _, req := range cases {
		if !router.Explicit(req) {
			t.Errorf("Explicit(%+v) = false, want true", req)
		}
	}
}

// Everything Route sends to the default reports false.
func TestExplicitDefaultsAreNotExplicit(t *testing.T) {
	cases := []router.Request{
		{Method: "GET", Host: api, Path: "/api/hello"},
		{Method: "GET", Host: api, Path: "/api/claude_cli/bootstrap"},
		{Method: "GET", Host: api, Path: "/api/oauth/organizations/o1/plugins/list-plugins"},
		// the POST-only rule does not name a GET
		{Method: "GET", Host: api, Path: "/api/oauth/validate", AbsoluteForm: true},
		{Method: "GET", Host: mcp, Path: "/other"},
		{Method: "POST", Host: mcp, Path: "/"},
		{Method: "GET", Host: "example.com", Path: "/v1/messages"},
	}
	for _, req := range cases {
		got := router.Explicit(req)
		dec := router.Route(req)
		if got && dec.Fallback {
			t.Errorf("Explicit and Route disagree for %+v", req)
		}
		if got {
			t.Errorf("Explicit(%+v) = true, want false", req)
		}
	}
}

// Route and Explicit never disagree: for the API host a request is default
// exactly when Route reports Fallback.
func TestExplicitAgreesWithRouteFallback(t *testing.T) {
	paths := []string{"/api/hello", "/v1/messages", "/api/frame/x", "/worker", "/v1/other", "/api/oauth/validate"}
	for _, p := range paths {
		for _, m := range []string{"GET", "POST"} {
			req := router.Request{Method: m, Host: api, Path: p}
			if router.Explicit(req) == router.Route(req).Fallback {
				t.Errorf("%s %s: Explicit=%v Fallback=%v", m, p, router.Explicit(req), router.Route(req).Fallback)
			}
		}
	}
}
