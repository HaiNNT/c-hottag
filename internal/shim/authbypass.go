package shim

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// authSubcommand reports whether args invoke `claude auth ...` or
// `claude setup-token` (R144): the subcommand must be the first argument,
// exactly and case-sensitively. No flag is skipped: a flag's separate value
// (`--model auth`, `--resume auth`, `-c auth`) is indistinguishable from a
// subcommand without tracking every value flag, and a false match would
// launch a coding session with no chottag. So `claude --debug auth login`
// takes the normal path (accepted). A prompt such as "auth flow" is one
// argument containing a space, so it never matches.
func authSubcommand(args []string) bool {
	return len(args) > 0 && (args[0] == "auth" || args[0] == "setup-token")
}

// authBypassEnv is env minus chottag's own HTTPS_PROXY/https_proxy. A user's
// own proxy is kept. NODE_EXTRA_CA_CERTS is never touched: it may be a bundle
// carrying the user's own CA, and trusting chottag's CA is harmless without
// its proxy.
func authBypassEnv(env []string) []string {
	var drop []string
	for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
		if isChottagProxy(envGet(env, k)) {
			drop = append(drop, k)
		}
	}
	return dropEnv(env, drop...)
}

// isChottagProxy reports whether raw is a proxy URL chottag's shim wrote: a
// loopback host carrying a chottag credential (user "chottag" or
// "chottag.<pool>.<sid>") on any port, or a loopback URL on the default port.
// The port is not read from state.json: this path must work when it is
// broken, so a credential-less loopback URL on a non-default port is kept.
func isChottagProxy(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	if u.User != nil {
		name := u.User.Username()
		if name == proxyauth.User || strings.HasPrefix(name, proxyauth.User+".") {
			return true
		}
	}
	return u.Port() == strconv.Itoa(store.DefaultPort)
}
