package router_test

import (
	"testing"

	"github.com/HaiNNT/c-hottag/internal/router"
)

const api, mcp = "api.anthropic.com", "mcp-proxy.anthropic.com"

// fb is the Serving decision of a route the table does not list.
func fb() router.Decision              { return router.Decision{Class: router.Serving, Fallback: true} }
func d(c router.Class) router.Decision { return router.Decision{Class: c} }
func obj(c router.Class, k router.Kind, id string) router.Decision {
	return router.Decision{Class: c, Object: k, ObjectID: id}
}
func rec(k router.Kind, field string) router.Decision {
	return router.Decision{Class: router.Remote, Records: k, RecordField: field}
}

// One case per route in routes/2.1.277.txt, plus edge cases.
func TestRoute(t *testing.T) {
	S, R, U := router.Serving, router.Remote, router.Untouched
	cases := []struct {
		method, host, path, query string
		want                      router.Decision
	}{
		// serving (M0 confirmed)
		{"GET", api, "/api/claude_cli/bootstrap", "entrypoint=cli", fb()},
		{"GET", api, "/api/claude_code/notification/preferences", "", fb()},
		{"GET", api, "/api/claude_code_grove", "", fb()},
		{"GET", api, "/api/claude_code_penguin_mode", "", fb()},
		{"POST", api, "/api/eval/sdk-zAZezfDKGoZuXXKe", "", fb()},
		{"POST", api, "/api/event_logging/v2/batch", "", fb()},
		{"GET", api, "/api/oauth/account/settings", "", fb()},
		{"GET", api, "/api/oauth/organizations/org-1/marketplaces", "", d(U)}, // F218
		{"GET", api, "/api/oauth/organizations/org-1/plugins/list-plugins", "limit=100", fb()},
		{"GET", api, "/api/oauth/organizations/org-1/plugins/pl-1/download", "", fb()},
		{"GET", api, "/api/oauth/organizations/org-1/skills/list-skills", "entrypoint=cli", fb()},
		{"GET", api, "/api/oauth/organizations/org-1/skills/sk-1/download", "", fb()},
		{"GET", api, "/api/oauth/organizations/org-1/sync/github/auth", "", fb()},
		{"GET", api, "/api/oauth/usage", "", fb()},
		{"GET", api, "/api/organizations/org-1/model_selector/cc", "", d(U)}, // F218
		{"POST", api, "/v1/messages", "beta=true", d(S)},
		{"GET", api, "/v1/ultrareview/quota", "", fb()},
		{"GET", api, "/some/new/route", "", fb()},
		{"POST", "API.Anthropic.com:443", "/v1/messages", "", d(S)},
		// RC sessions
		{"POST", api, "/v1/code/sessions", "", rec(router.KindSession, "session.id")},
		{"GET", api, "/v1/code/sessions", "", d(R)},
		{"POST", api, "/v1/code/sessions/cse_123/bridge", "", obj(R, router.KindSession, "cse_123")},
		{"GET", api, "/v1/code/sessions/cse_123", "", obj(R, router.KindSession, "cse_123")},
		{"GET", api, "/v1/sessions/session_123", "", obj(S, router.KindSession, "session_123")},
		{"PATCH", api, "/v1/sessions/session_123", "", obj(S, router.KindSession, "session_123")},
		{"POST", api, "/v1/sessions/session_123/archive", "", obj(S, router.KindSession, "session_123")},
		{"GET", api, "/v1/sessions/sess_1/events", "", obj(S, router.KindSession, "sess_1")},
		// RC after creation: untouched
		{"GET", api, "/v1/code/sessions/cse_1/worker", "include_hearth", d(U)},
		{"PUT", api, "/v1/code/sessions/cse_1/worker", "", d(U)},
		{"POST", api, "/v1/code/sessions/cse_1/worker/events", "", d(U)},
		{"POST", api, "/v1/code/sessions/cse_1/worker/events/delivery", "", d(U)},
		{"GET", api, "/v1/code/sessions/cse_1/worker/events/stream", "", d(U)},
		{"POST", api, "/v1/code/sessions/cse_1/worker/heartbeat", "", d(U)},
		{"GET", api, "/v1/code/sessions/cse_1/worker/internal-events", "limit=1000", d(U)},
		{"POST", api, "/v1/code/sessions/cse_1/worker/register", "", d(U)},
		{"POST", api, "/v1/code/sessions/cse_1/client/presence", "", d(U)},
		{"GET", api, "/v1/environments/env_1/work/poll", "", d(U)},
		{"POST", api, "/v1/environments/env_1/work/w_1/ack", "", d(U)},
		{"POST", api, "/v1/environments/env_1/work/w_1/heartbeat", "", d(U)},
		{"POST", api, "/v1/environments/env_1/work/w_1/stop", "", d(U)},
		{"POST", api, "/v1/oauth/token", "", d(U)},
		{"GET", api, "/mcp-registry/v0/servers", "limit=100", d(U)},
		// Org-bound to the login Claude Code started with: every swap is refused (F203).
		{"GET", api, "/api/claude_code/settings", "", d(U)},
		{"GET", api, "/api/claude_code/policy_limits", "", fb()},
		// `claude remote-control` server
		{"POST", api, "/api/oauth/validate", "", router.Decision{Class: S, StickySession: true}}, // MITM form: a REPL validating its own login
		{"POST", api, "/v1/environments/bridge", "", rec(router.KindEnvironment, "environment_id")},
		{"POST", api, "/v1/environments/bridge", "beta=true", d(U)},
		{"DELETE", api, "/v1/environments/bridge/env_1", "", obj(R, router.KindEnvironment, "env_1")},
		// F163: the rc-server shutdown call, observed live in 2.1.281.
		{"POST", api, "/v1/environments/bridge/env_1/offline", "", obj(R, router.KindEnvironment, "env_1")},
		{"POST", api, "/v1/environments/bridge/env_1/offline", "beta=true", d(U)},
		{"POST", api, "/v1/environments/env_1/bridge/reconnect", "", obj(R, router.KindEnvironment, "env_1")},
		{"POST", api, "/v1/environments/env_1/bridge/reconnect", "beta=true", d(U)},
		{"GET", api, "/v1/environments", "", d(R)},
		{"GET", api, "/v1/environments/env_1", "", obj(R, router.KindEnvironment, "env_1")},
		// artifacts
		{"POST", api, "/api/frame/deploy/prepare", "", rec(router.KindArtifact, "slug")},
		{"POST", api, "/api/frame/deploy/direct", "attempt_id=x", router.Decision{Class: router.Remote, Object: router.KindArtifact, RequestField: "slug", Records: router.KindArtifact, RecordField: "slug"}},
		// F39: track names its artifact in the body, so it must resolve
		// through the owner map rather than follow the pin (see
		// TestFrameTrackResolvesThroughTheOwnerMap below).
		{"POST", api, "/api/frame/track", "", router.Decision{Class: R, Object: router.KindArtifact, RequestField: "slug"}},
		{"GET", api, "/api/frame/types", "scopes=x", d(R)},
		{"GET", api, "/api/frame/types/t_1/instances", "limit=200", d(R)},
		{"GET", api, "/api/frame/sync", "slug=abc", d(R)},
		{"GET", api, "/api/frame/read/abc123", "", obj(R, router.KindArtifact, "abc123")},
		{"GET", api, "/api/frame/abc123", "bk=1", obj(R, router.KindArtifact, "abc123")},
		{"PUT", api, "/api/frame/abc/publish", "", obj(R, router.KindArtifact, "abc")},
		// routines
		{"GET", api, "/v1/code/triggers", "", d(R)},
		{"POST", api, "/v1/code/triggers/trg_1/run", "", d(R)},
		{"GET", api, "/v1/environment_providers", "", d(R)},
		// connectors
		{"GET", api, "/v1/mcp_servers", "limit=1000", rec(router.KindConnector, "data[].id")},
		{"POST", api, "/v1/mcp_servers/srv_1", "", d(R)},
		{"POST", mcp, "/v1/mcp/srv_1", "", obj(R, router.KindConnector, "srv_1")},
		{"GET", mcp + ":443", "/v1/mcp/srv_1", "", obj(R, router.KindConnector, "srv_1")},
		{"GET", mcp, "/health", "", d(R)},
		// other hosts
		{"POST", "claude.ai", "/api/anything", "", d(U)},
		{"GET", "downloads.claude.ai", "/claude-code-releases/latest", "", d(U)},
	}
	for _, c := range cases {
		if got := router.Route(router.Request{Method: c.method, Host: c.host, Path: c.path, RawQuery: c.query}); got != c.want {
			t.Errorf("Route(%s %s %s ?%s) = %+v, want %+v", c.method, c.host, c.path, c.query, got, c.want)
		}
	}
}

func TestRouteUsesTheRequestForm(t *testing.T) {
	absolute := router.Request{Method: "POST", Host: api, Path: "/api/oauth/validate", AbsoluteForm: true}
	if got := router.Route(absolute); got.Class != router.Remote || got.StickySession {
		t.Errorf("absolute-form validate = %+v, want remote (the remote-control server)", got)
	}
	mitm := router.Request{Method: "POST", Host: api, Path: "/api/oauth/validate"}
	if got := router.Route(mitm); got.Class != router.Serving || !got.StickySession {
		t.Errorf("mitm validate = %+v, want serving and sticky per session (a REPL's own login, R160)", got)
	}
	// The form never changes any other route.
	for _, path := range []string{"/v1/messages", "/v1/code/sessions", "/api/frame/deploy/prepare"} {
		a := router.Route(router.Request{Method: "POST", Host: api, Path: path, AbsoluteForm: true})
		b := router.Route(router.Request{Method: "POST", Host: api, Path: path})
		if a != b {
			t.Errorf("%s: absolute %+v != mitm %+v", path, a, b)
		}
	}
}

// F39: with the artifact owned by A and the remote pin moved to D, a live
// track call followed the pin to D and 404'd. track must resolve through
// the owner map like every other route touching a specific artifact.
func TestFrameTrackResolvesThroughTheOwnerMap(t *testing.T) {
	d := router.Route(router.Request{Method: "POST", Host: api, Path: "/api/frame/track"})
	if d.Class != router.Remote {
		t.Errorf("Class = %q, want %q", d.Class, router.Remote)
	}
	if d.Object != router.KindArtifact {
		t.Errorf("Object = %q, want %q — a track call names a specific artifact, so it must follow that artifact's owner rather than the current pin (F39)", d.Object, router.KindArtifact)
	}
	if d.RequestField != "slug" {
		t.Errorf("RequestField = %q, want %q", d.RequestField, "slug")
	}
}

// The sibling routes must keep following the pin: they name no object.
func TestFrameTypesAndSyncStillFollowThePin(t *testing.T) {
	for _, p := range []string{"/api/frame/types", "/api/frame/sync"} {
		d := router.Route(router.Request{Method: "GET", Host: api, Path: p})
		if d.Class != router.Remote {
			t.Errorf("%s: Class = %q, want %q", p, d.Class, router.Remote)
		}
		if d.Object != "" {
			t.Errorf("%s: Object = %q, want empty", p, d.Object)
		}
	}
}

func TestIntercepted(t *testing.T) {
	for host, want := range map[string]bool{
		"api.anthropic.com":           true,
		"MCP-Proxy.anthropic.com:443": true,
		"claude.ai":                   false,
		"anthropic.com":               false,
		"evil-api.anthropic.com":      false,
	} {
		if got := router.Intercepted(host); got != want {
			t.Errorf("Intercepted(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"api.anthropic.com:443": "api.anthropic.com",
		"Claude.AI":             "claude.ai",
		"[::1]:8080":            "::1",
	} {
		if got := router.HostOnly(in); got != want {
			t.Errorf("HostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
