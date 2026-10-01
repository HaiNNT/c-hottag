package proxy

import (
	"context"
	"net/http"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// nativeIDHeader is Claude Code's own session id header.
const nativeIDHeader = "X-Claude-Code-Session-Id"

// Identity is who a tunnelled request is (M6): the caller its CONNECT (or
// absolute-form request) authenticated as, plus what the request itself says.
type Identity struct {
	Caller    proxyauth.Caller
	NativeID  string // X-Claude-Code-Session-Id, "" if absent
	Inference bool   // POST to api.anthropic.com /v1/messages (path exactly, query ignored)
}

type identityKey struct{}

// IdentityFrom is the request's Identity; false when its caller is not
// identified (legacy credential, or caller auth off).
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// ContextWithIdentityForTest returns ctx carrying id, as the proxy does for an
// identified request. It exists so a Chooser implementation's tests can
// present an identity without driving a real tunnel.
func ContextWithIdentityForTest(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// withIdentity attaches an Identity to r's context, only for an identified
// caller. host is the request's host without a port; tls is true only for a
// request that arrived through the intercepted tunnel (Inference needs it).
func withIdentity(r *http.Request, c proxyauth.Caller, host string, tls bool) *http.Request {
	if !c.Identified() {
		return r
	}
	id := Identity{
		Caller:    c,
		NativeID:  r.Header.Get(nativeIDHeader),
		Inference: tls && r.Method == http.MethodPost && host == router.APIHost && r.URL.Path == "/v1/messages",
	}
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
}

// shortSID is the only part of a session id a trace record may carry.
func shortSID(c proxyauth.Caller) string {
	if len(c.SID) < 8 {
		return ""
	}
	return c.SID[:8]
}

// sidOf is the short sid of the request's identified caller, "" otherwise.
func sidOf(ctx context.Context) string {
	if id, ok := IdentityFrom(ctx); ok {
		return shortSID(id.Caller)
	}
	return ""
}
