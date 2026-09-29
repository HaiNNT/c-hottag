// Package redact renders values that may carry a credential for a log,
// error message, or wire response without the credential itself. It exists
// so the technique — and the non-obvious case that defeats the obvious
// approach — lives in exactly one place: callers in internal/cli,
// internal/proxy and internal/shim all need it for the identical value
// shape (an upstream proxy URL), and a duplicated security helper drifts
// (F103, F114).
package redact

import "net/url"

// UpstreamProxy renders raw — an --upstream-proxy flag value, a HTTPS_PROXY
// environment value, or any string of that shape — for a message with any
// userinfo password masked, never the value itself: this project's hardest
// rule is that no secret reaches a log or disk, and stderr becoming one is
// only a matter of time (M1c3). An empty raw means "no upstream configured"
// and is reported as such, not as an unparsable value. raw may otherwise
// fail to parse at all (e.g. the reason a caller rejected it); there is
// then no URL to redact selectively, so this reports that rather than
// falling back to printing raw unredacted.
//
// A scheme-less value like "bob:hunter2@proxy:8080" (the shape http_proxy
// env vars and `curl -x` accept, not an exotic one) is where
// url.URL.Redacted() alone fails silently: url.Parse reads "bob" as the
// SCHEME, not a userinfo username, and stuffs "hunter2@proxy:8080" into
// u.Opaque instead of u.User — Redacted() only ever masks u.User, so it
// passes u.Opaque through untouched and the password survives verbatim
// (measured: Redacted() on that input returns the input unchanged). Any URL
// with a non-empty Opaque, or with no Host at all, therefore reports a
// fixed placeholder instead of trusting Redacted() to have found (and
// masked) whatever credential it contains.
//
// A username-only credential — "https://sk-TOKEN@proxy:8080", a real shape
// for a bearer token, not merely a hypothetical one (F139) — is where
// Redacted() alone gets the *parsed* case wrong too: it masks only
// u.User's password, and a username with no password has none to mask, so
// the token passes through verbatim. Any URL with a non-nil u.User
// therefore has its whole userinfo replaced outright before Redacted() (or
// direct rendering) ever sees it, rather than trusting Redacted() to have
// found the one piece it knows how to mask.
func UpstreamProxy(raw string) string {
	if raw == "" {
		return "(none)"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable>"
	}
	if u.Opaque != "" || u.Host == "" {
		return "<redacted>"
	}
	if u.User != nil {
		u.User = url.User("xxxxx")
	}
	return u.Redacted()
}

// WithoutUserinfo reduces raw — an upstream proxy URL, valid or not — to
// scheme://host, with any userinfo (and therefore any password) removed
// entirely rather than masked. It exists for a different need than
// UpstreamProxy above: UpstreamProxy renders a value for a human to read in
// a message, so it keeps the host and masks the userinfo (username and
// password alike, F139) rather than dropping it; this
// function is for comparing WHICH proxy two sides are configured with, so
// it drops the password outright rather than substituting a fixed mask for
// it — masking would make two upstreams that differ only in password
// compare equal, a silent behaviour change hidden inside a redactor (fix
// round 3, D1). Serving u.Redacted() from a health endpoint was rejected
// for the same reason: "xxxxx" is still on the wire, so the endpoint would
// still be publishing that a password exists and inviting exactly the
// comparison this function makes safe instead. We compare which proxy, not
// whose password.
//
// An empty, unparsable, or path-only raw (net/url's "opaque" case — see
// UpstreamProxy's doc comment for why that shape defeats a naive
// Redacted() call) reduces to "" rather than echoing raw back: there is no
// scheme/host pair to compare on, and falling back to raw would put
// user:pass right back into a value this function's entire purpose is to
// strip it from.
func WithoutUserinfo(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
