// Package router decides which account a request to Anthropic should carry.
// The table mirrors spec §4.4 as confirmed by M0 (routes/2.1.277.txt).
package router

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

type Class string

const (
	Serving   Class = "serving"   // whose quota / whose data is shown
	Remote    Class = "remote"    // creates or operates on a claude.ai-owned object
	Untouched Class = "untouched" // own credential or must stay on Home's login
)

// Kind names a claude.ai-owned object type tracked by the owner map.
type Kind string

const (
	KindSession     Kind = "session"     // Remote Control session
	KindEnvironment Kind = "environment" // `claude remote-control` environment
	KindArtifact    Kind = "artifact"
	KindConnector   Kind = "connector" // claude.ai connector (MCP server)
)

const (
	APIHost      = "api.anthropic.com"
	MCPProxyHost = "mcp-proxy.anthropic.com"
)

// Hosts are the intercepted hosts; every other host is a blind tunnel.
var Hosts = []string{APIHost, MCPProxyHost}

// Intercepted reports whether host (with or without port) is in Hosts.
func Intercepted(host string) bool {
	h := HostOnly(host)
	for _, x := range Hosts {
		if h == x {
			return true
		}
	}
	return false
}

// Decision says how to route one request.
type Decision struct {
	// Class applies when the request names no object, or its owner is unknown.
	Class Class
	// Object and ObjectID name the existing object the request operates on;
	// if the owner map knows who created it, that account is used instead.
	Object   Kind
	ObjectID string
	// Records and RecordField say that ids found at RecordField in the JSON
	// response are objects of kind Records owned by the account the request
	// went out as. Field syntax: dot-separated keys, "[]" iterates an array.
	Records     Kind
	RecordField string
	// RequestField is a JSON field in the *request* body holding the id of
	// the object this request operates on, for routes that do not put it in
	// the path (an artifact republish posts the slug in the body).
	RequestField string
}

// Request is what the router needs to know about one request. AbsoluteForm
// is true when the client sent an absolute-form request (the `claude
// remote-control` server) rather than tunnelling through CONNECT: some
// routes are used by both clients and must be classified differently.
type Request struct {
	Method, Host, Path, RawQuery string
	AbsoluteForm                 bool
}

type rule struct {
	method       string // "" = any
	re           *regexp.Regexp
	d            Decision
	betaSkip     bool // cswap-pin: "?beta=true" variants of these must never be swapped
	absoluteOnly bool // only when the client sent an absolute-form request
}

func r(method, pattern string, d Decision) rule {
	return rule{method: method, re: regexp.MustCompile(pattern), d: d}
}

func rb(method, pattern string, d Decision) rule {
	x := r(method, pattern, d)
	x.betaSkip = true
	return x
}

func ra(method, pattern string, d Decision) rule {
	x := r(method, pattern, d)
	x.absoluteOnly = true
	return x
}

var (
	untouched = Decision{Class: Untouched}
	remote    = Decision{Class: Remote}

	apiRules = []rule{
		// Per-session credentials, presence and work polling: swapping these
		// breaks Remote Control (cswap-pin, M0 Step 3).
		r("", `/worker(/|$)`, untouched),
		r("", `/client/presence$`, untouched),
		r("", `^/v1/environments/[^/]+/work(/|$)`, untouched),
		r("", `^/v1/oauth/`, untouched),
		r("", `^/mcp-registry/`, untouched),
		// Claude Code's own settings are bound to the org of the login it
		// started with: a swapped token is always refused and the safety net
		// resends it, bumping routeDrift at every session start (F203).
		r("GET", `^/api/claude_code/settings$`, untouched),
		// The same holds for these two org-scoped routes: the org id in the
		// path is the session's own login's, never the serving account's,
		// so every swap to another account was refused (F218, superseding
		// the D9 limitation). Other org routes (skills, plugins) accept a
		// swapped token and stay serving.
		r("GET", `^/api/oauth/organizations/[^/]+/marketplaces$`, untouched),
		r("GET", `^/api/organizations/[^/]+/model_selector/cc$`, untouched),

		r("POST", `^/v1/code/sessions$`, Decision{Class: Remote, Records: KindSession, RecordField: "session.id"}),
		r("", `^/v1/code/sessions$`, remote),
		r("", `^/v1/code/sessions/([^/]+)(/.*)?$`, Decision{Class: Remote, Object: KindSession}),
		// Cloud sessions use /v1/sessions too, so an unknown id stays serving.
		r("", `^/v1/sessions/([^/]+)(/.*)?$`, Decision{Class: Serving, Object: KindSession}),

		rb("POST", `^/v1/environments/bridge$`, Decision{Class: Remote, Records: KindEnvironment, RecordField: "environment_id"}),
		// F163: the trailing /offline is the rc-server's shutdown call
		// (2.1.281); it must follow the environment's owner too.
		rb("", `^/v1/environments/bridge/([^/]+)(/.*)?$`, Decision{Class: Remote, Object: KindEnvironment}),
		rb("", `^/v1/environments/([^/]+)/bridge/reconnect$`, Decision{Class: Remote, Object: KindEnvironment}),
		rb("", `^/v1/environments(/bridge)?$`, remote),
		rb("", `^/v1/environments/([^/]+)$`, Decision{Class: Remote, Object: KindEnvironment}),
		r("", `^/v1/environment_providers(/.*)?$`, remote),
		r("", `^/v1/code/triggers(/.*)?$`, remote),
		// The `claude remote-control` server (absolute form) registers the
		// remote credential with validate, but a REPL session (mitm) calls
		// the same route for its own login and feeds the returned
		// organization_uuid to serving routes (gate G2, F9).
		ra("POST", `^/api/oauth/validate$`, remote),
		r("POST", `^/api/oauth/validate$`, Decision{Class: Serving}),

		r("POST", `^/api/frame/deploy/prepare$`, Decision{Class: Remote, Records: KindArtifact, RecordField: "slug"}),
		// A republish sends only this call, with the existing slug in the
		// request body (gate G2, F8), so its owner comes from the body.
		r("POST", `^/api/frame/deploy/direct$`, Decision{Class: Remote, Object: KindArtifact, RequestField: "slug", Records: KindArtifact, RecordField: "slug"}),
		// A track call names a specific artifact in its body, so it must
		// follow that artifact's owner rather than the current remote pin.
		// Observed live (F39): with the artifact owned by A and the pin
		// moved to D, track followed the pin and 404'd.
		//
		// The body field is assumed to be "slug", matching
		// /api/frame/deploy/direct — the only other artifact route that
		// names its object in the request body. If that is wrong the
		// lookup simply yields no id and the decision falls back to the
		// pin, i.e. exactly the behaviour this rule replaces.
		r("POST", `^/api/frame/track$`, Decision{Class: Remote, Object: KindArtifact, RequestField: "slug"}),
		r("", `^/api/frame/(deploy|track|types|sync)(/.*)?$`, remote),
		r("", `^/api/frame/read/([^/]+)$`, Decision{Class: Remote, Object: KindArtifact}),
		r("", `^/api/frame/([^/]+)(/.*)?$`, Decision{Class: Remote, Object: KindArtifact}),

		r("GET", `^/v1/mcp_servers$`, Decision{Class: Remote, Records: KindConnector, RecordField: "data[].id"}),
		r("", `^/v1/mcp_servers(/.*)?$`, remote),
	}

	mcpProxyRules = []rule{
		r("", `^/v1/mcp/([^/]+)(/.*)?$`, Decision{Class: Remote, Object: KindConnector}),
		r("", `^/`, remote),
	}
)

// Route classifies one request. host may include a port.
func Route(req Request) Decision {
	var rules []rule
	switch HostOnly(req.Host) {
	case APIHost:
		rules = apiRules
	case MCPProxyHost:
		rules = mcpProxyRules
	default:
		return untouched
	}
	for _, ru := range rules {
		if ru.method != "" && ru.method != req.Method {
			continue
		}
		if ru.absoluteOnly && !req.AbsoluteForm {
			continue
		}
		m := ru.re.FindStringSubmatch(req.Path)
		if m == nil {
			continue
		}
		if ru.betaSkip {
			if q, _ := url.ParseQuery(req.RawQuery); q.Get("beta") == "true" {
				return untouched
			}
		}
		d := ru.d
		if d.Object != "" && len(m) > 1 {
			d.ObjectID = m[1]
		}
		return d
	}
	return Decision{Class: Serving}
}

// HostOnly lower-cases hostport and strips any port.
func HostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(hostport)
}
