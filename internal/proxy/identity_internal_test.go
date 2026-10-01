package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// Inference is only for a request that came through the TLS tunnel: a
// plain-http absolute-form POST naming the API host is not inference.
func TestInferenceNeedsTheTLSPath(t *testing.T) {
	c := proxyauth.Caller{Pool: "default", SID: "0123456789abcdef0123456789abcdef"}
	for _, tc := range []struct{ tls, want bool }{{true, true}, {false, false}} {
		r := httptest.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages", nil)
		id, ok := IdentityFrom(withIdentity(r, c, "api.anthropic.com", tc.tls).Context())
		if !ok || id.Inference != tc.want {
			t.Errorf("tls=%v: ok=%v Inference=%v, want %v", tc.tls, ok, id.Inference, tc.want)
		}
	}
}
