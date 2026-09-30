package shim

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// healthTimeout bounds a single probe. It is short because the shim calls
// ProbeHealth in a 50ms poll loop bounded overall at 2s (§4.3 step 3): a
// slow probe must not itself eat the whole budget.
const healthTimeout = 500 * time.Millisecond

// healthClient sets Proxy: nil explicitly rather than relying on
// http.DefaultTransport's http.ProxyFromEnvironment default. Go's
// ProxyFromEnvironment already exempts loopback addresses regardless of
// HTTP_PROXY/HTTPS_PROXY (net/http/httpproxy), so this changes nothing for
// the 127.0.0.1 URL probeHealth builds below — it is belt-and-braces: it
// states the invariant locally rather than depending on that exemption
// holding, and it keeps the probe off http.DefaultTransport's shared
// connection pool.
var healthClient = &http.Client{
	Timeout:   healthTimeout,
	Transport: &http.Transport{Proxy: nil},
}

// ProbeHealth reports whether the daemon listening on 127.0.0.1:port is
// chottag's own daemon, and its health document when it is. Exported for
// `chottag daemon start|restart` (internal/cli): spec §5 forbids a second
// way to decide the daemon is up (F103).
//
// A transport error, a non-200, a body that doesn't decode as JSON, and a
// document with Chottag: false are ALL treated identically as "not
// confirmed" — the shim is about to hand `claude` an HTTPS_PROXY, and only a
// document that unmarshals with Chottag true is proof, not merely "something
// answered" (that is the token-leak case HealthPath exists to rule out).
//
// ProbeHealth proves LIVENESS only: it asks no nonce, so a listener that
// merely echoes Chottag: true — including a legitimate chottag daemon that
// simply hasn't been asked for proof yet — still confirms here. It stays
// exactly this shape for `daemon start|stop|restart` and `update`, whose
// identity comes from daemon.lock, not from the health endpoint (Ruling 8).
// VerifyHealth below is the one call that establishes IDENTITY (F221, L4):
// only it decides whether claude may be handed the secret.
func ProbeHealth(port int) (bool, proxy.Health) { return probe(port, "") }

// Identity is what VerifyHealth decided about the daemon on the far end of a
// health probe (F221, Ruling 9): whether it proved this install's secret,
// predates proxy authentication entirely (no Proof field at all, from a
// build OLDER than the one asking), or proved a DIFFERENT secret. shim.Run
// is the one caller that turns this into a launch decision.
type Identity string

const (
	IdentityNone     Identity = "none"     // nothing chottag answered
	IdentityVerified Identity = "verified" // proved it holds this install's secret
	IdentityLegacy   Identity = "legacy"   // chottag, but no proof: a daemon from before part 1
	IdentityMismatch Identity = "mismatch" // a proof that does not verify
)

// VerifyHealth asks the daemon on port to prove it holds secret: a fresh
// nonce out, HMAC(secret, port, nonce) back (F221, security review L4,
// Ruling 32). The proof is checked against port — the port THIS call
// actually probed — never a port the response names, so a same-secret
// listener on another port cannot relay a valid-looking proof for it (final
// review M2). Only a verified daemon is handed the secret; the caller
// decides what legacy and mismatch mean (shim.Run trusts legacy only while
// this home's own daemon.lock is held, and refuses a mismatch outright).
//
// A proof-less answer (h.Proof == "") is Legacy ONLY when the daemon's
// Version differs from ownVersion, the caller's own running build. The
// SAME build always turns proxy auth on, so a same-version daemon with no
// proof at all cannot legitimately predate authentication — something
// failed to enable it, which is exactly what Mismatch already means to the
// caller (controller ruling, fix round 1 item 5): refused, never silently
// trusted as pre-part-1.
func VerifyHealth(port int, secret proxyauth.Secret, ownVersion string) (Identity, proxy.Health) {
	nonce := proxyauth.NewNonce()
	ok, h := probe(port, nonce)
	switch {
	case !ok:
		return IdentityNone, proxy.Health{}
	case h.Proof == "" && h.Version != ownVersion:
		return IdentityLegacy, h
	case h.Proof != "" && secret.VerifyProof(port, nonce, h.Proof):
		return IdentityVerified, h
	}
	return IdentityMismatch, h
}

// probe is ProbeHealth and VerifyHealth's shared GET against HealthPath: an
// empty nonce asks no proof at all (liveness only); a non-empty one asks the
// daemon to prove it holds the secret for exactly that nonce. The response
// body is capped at 64KiB — a local loopback health document is never
// legitimately larger — so neither caller can be made to buffer an
// unbounded body.
func probe(port int, nonce string) (bool, proxy.Health) {
	return probeWith(healthClient, port, nonce)
}

// ProbeHealthWithin is ProbeHealth with its own overall timeout, for a
// caller that runs on a display's clock (`chottag statusline`) and cannot
// wait healthTimeout. ProbeHealth itself is unchanged for every other caller.
func ProbeHealthWithin(port int, timeout time.Duration) bool {
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}
	ok, _ := probeWith(c, port, "")
	return ok
}

func probeWith(client *http.Client, port int, nonce string) (bool, proxy.Health) {
	u := "http://127.0.0.1:" + strconv.Itoa(port) + proxy.HealthPath
	if nonce != "" {
		u += "?" + proxyauth.NonceParam + "=" + nonce
	}
	resp, err := client.Get(u)
	if err != nil {
		return false, proxy.Health{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, proxy.Health{}
	}
	var h proxy.Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h); err != nil || !h.Chottag {
		return false, proxy.Health{}
	}
	return true, h
}
