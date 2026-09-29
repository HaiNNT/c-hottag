# Security policy

## Supported versions

Only the latest release is supported. Fixes ship as a new release; there are
no backports to older versions.

## Reporting a vulnerability

Please report security issues through GitHub's private vulnerability
reporting: open this repo's **Security** tab and use **"Report a
vulnerability"**. Do not open a public issue for a security report.

Do not include a real token, a `proxy.secret` or a `ca.key` in a report —
describe the class of value instead.

This is a small project with no support promise, but on a best-effort basis:

- acknowledgement within 7 days;
- a fix or a mitigation plan within 30 days.

Disclosure is coordinated: please give us a chance to ship a fix before any
public write-up. Reporters are credited on request.

## What chottag is, in security terms

chottag runs a local TLS-intercepting proxy on `127.0.0.1`, port 47821 by
default. It
terminates TLS for `api.anthropic.com` and `mcp-proxy.anthropic.com` (and
only those hosts), holds every configured account's OAuth access token in
memory, and replaces each request's bearer with the account it decides
should pay for that request. It never writes a token anywhere: tokens live
only in Claude Code's own stores (the Keychain, or a slot's
`.credentials.json`).

## Threat model

**Trusted:** your OS user account, the OS itself, the Keychain (or
equivalent credential store), GitHub, and this repo's release workflow.

**Caller authentication.** Each install has its own 32-byte secret in
`~/.chottag/ca/proxy.secret` (mode 0600). The `claude` shim puts it in the
userinfo of `HTTPS_PROXY`
(`http://chottag:<secret>@127.0.0.1:47821`), and Claude Code sends it back
as `Proxy-Authorization`. Most HTTP clients a session spawns that inherit
`HTTPS_PROXY` do too — curl, Python, Go, git (after a `407`) — but not
every one: Node's built-in `fetch`, for example, ignores `HTTPS_PROXY`
entirely and never reaches chottag at all. The proxy requires a
matching secret before it will:

- MITM a `CONNECT` to a host it intercepts (`api.anthropic.com`,
  `mcp-proxy.anthropic.com`);
- forward an absolute-form request (`https://...` sent straight over the
  proxy connection), whatever its host;
- open a blind tunnel, when the daemon itself is chained through an
  upstream proxy that carries its own credentials — otherwise that upstream
  identity would be usable by any local caller without ever seeing it.

A caller without the secret gets `407 Proxy Authentication Required` on all
of those. Before handing the secret to `claude`, the shim challenges the
daemon over the health endpoint with a fresh nonce and checks its
`HMAC(secret, port, nonce)` answer — bound to the port the shim actually
probed, from the connection's own local address, not to anything the
request or response names — so a process that merely squats the port never
receives it, and a same-secret listener on a DIFFERENT port (a stopped
daemon's old port, a foreground `proxy run`, a leftover `trace run`) can't
relay the nonce and hand back its own proof as if it were the daemon on the
probed port.

**Known limitation: other blind tunnels stay open.** A `CONNECT` to a host
chottag doesn't intercept (anything other than the two Anthropic hosts
above) is relayed without authentication, because it can never swap a
token — unless the daemon itself is chained through a credentialed upstream
proxy, the case above. That means any other local user, or a container that
can reach your loopback interface, can use chottag as a generic TCP relay to
the open internet. It cannot use it to spend your Claude accounts.

**Multi-user hosts.** Another OS user (or a container sharing your loopback,
e.g. some Docker Desktop or `--network=host` setups) can no longer make
chottag swap in your accounts' tokens: doing that needs the contents of
`~/.chottag/ca/proxy.secret`, which is 0600 and owned by you, so they'd need
to already be able to read your files — at which point they can read your
Claude Code credentials directly (the macOS Keychain, or each slot's own
`.credentials.json`) and don't need chottag at all. They can
still use the loopback listener as an open relay (above). Do not run chottag
on a host you don't trust every local user of.

**Browsers can't drive it.** A web page cannot make your browser send a
`CONNECT` or an absolute-form request to a non-proxy origin, so it can never
reach the swap path. The health endpoint (`/__chottag/health`) answers only
loopback `Host` values and refuses any request carrying an `Origin` header,
which closes the one thing a browser (including through DNS rebinding)
could otherwise reach.

**Still out of scope:**

- malware, or any other process, running as you: it can already read your
  slot credentials and the proxy secret directly, and use chottag exactly as
  you can;
- root on this machine;
- a container or VM that shares your user's files (not just your loopback
  interface).

## The local CA

chottag's CA lives in `~/.chottag/ca/` (directory mode 0700): `ca.pem`
(0644, safe to read — it's a public certificate) and `ca.key` (0600, the
private key). It is never installed into the system trust store; it is
trusted only by processes chottag starts, through `NODE_EXTRA_CA_CERTS` —
**and every process those processes start** (hooks, MCP servers, Bash-tool
commands). The CA's lifetime is 10 years; there's no automatic rotation.

A CA created by chottag v0.4.0 or later is name-constrained to
`anthropic.com`, `claude.ai` and `claude.com` (and excludes every IP
address), so even a leaked `ca.key` can only mint certificates for those
domains. A CA created by an earlier version has no constraints; `chottag
doctor` reports that as an informational row, not a failure. Whichever CA is
in use, a leaf certificate is only ever minted for the host a `CONNECT`
actually targeted — never for an arbitrary SNI a client presents inside the
tunnel.

**What a leaked `ca.key` allows.** Whoever holds it can mint a certificate
that every `claude`-launched process (and its children) will trust for the
domains the CA permits — every Anthropic host, for an unconstrained CA — for
as long as the CA's 10-year lifetime lasts, unless you rotate it.

**Rotating the CA:**

```sh
mv ~/.chottag/ca/ca.pem ~/.chottag/ca/ca.pem.bak
mv ~/.chottag/ca/ca.key ~/.chottag/ca/ca.key.bak
chottag daemon restart
```

Then restart every running Claude Code session — they cached the old CA in
`NODE_EXTRA_CA_CERTS` at launch.

Exclude `~/.chottag/ca` from dotfile sync tools and backups: a synced
`ca.key` is a leaked `ca.key`.

## The proxy secret

Rotating it:

```sh
rm ~/.chottag/ca/proxy.secret
chottag daemon restart
```

If `doctor` reports the secret itself as bad (not merely missing), use
`chottag doctor --fix` instead of the `rm` above. With a daemon running —
the normal state — `doctor` itself tells you the order: `chottag daemon
stop`, then `chottag doctor --fix`, then start the daemon again yourself
(`chottag daemon start`, or just run `claude`, which starts it) — running
sessions then need a restart, same as below.

Then restart every running Claude Code session; each one holds the old
secret in its own `HTTPS_PROXY`.

**`chottag proxy run` and `chottag trace run` are dev-only paths that don't
hold `daemon.lock`.** `doctor --fix`'s busy check only sees a chottag
process through that lock, so it can't tell one of these is using the
secret and can regenerate it out from under a running `proxy run` or `trace
run`, leaving that process's own shims (and any client whose `HTTPS_PROXY`
still names the old secret) with a mismatch. Don't run `doctor --fix`
while either is up; use `chottag daemon stop`/`start` for the real daemon
instead, which the lock does cover.

`chottag trace env` prints an `export HTTPS_PROXY="http://chottag:$(cat
"...")@host:port"` line: the secret is read from its file only when a shell
evaluates that line, so it never appears in the printed line itself, in a
terminal's scrollback, or in `daemon.log`.

## Tokens and logs

chottag never writes a token to disk. Account logins live only in Claude
Code's own stores.

- **`proxy.jsonl` and `trace.jsonl`** (0600) record, per request: the
  account name, the host, a templated path (ids replaced with `{id}`, a
  4-byte hash), the allowlisted query keys it recognises, the auth kind
  (e.g. `oauth-access`, never the bearer itself), whether it was swapped,
  and status and timing. They never hold a token, a request or response
  body, a raw id, or a query value.
- **`daemon.log`** (0600) holds operational lines only: it never gets the
  proxy secret (see above) or a token.
- **`cache/status.json`** (0600) holds chottag's derived view of each
  account: its name, email and org (from Claude Code's own `/usage`
  response), plan tier, and usage percentages. No tokens.
- A `407` or `421` refusal is logged with the request's host, method (for
  an absolute-form request) and form, alongside the status — never the
  `Proxy-Authorization` value, a path or a body.

## Environment effects

The shim sets `HTTPS_PROXY` (now carrying the secret) and
`NODE_EXTRA_CA_CERTS` on the `claude` process it launches, and Claude Code
passes both on to everything it spawns: hooks, MCP servers, and commands the
Bash tool runs. `CHOTTAG_BYPASS=1 claude` skips the shim entirely and execs
the real binary with whatever environment this invocation already had — it
does not clear `HTTPS_PROXY`/`NODE_EXTRA_CA_CERTS`. From a plain shell that
never had them, that's neither variable set; run from inside a session the
shim already launched (e.g. a nested `claude` a hook or the Bash tool
starts), the inherited secret URL stays set.

## Verifying a release

Releases from v0.4.0 on, on the public repo, carry a
[build provenance attestation](https://docs.github.com/en/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds):

```sh
gh attestation verify chottag_<ver>_<os>_<arch>.tar.gz --repo HaiNNT/c-hottag
```

(the repo name changes if the project is renamed; use whatever repo you
installed from). `chottag update` and `install.sh` both do this
automatically, alongside the `checksums.txt` check every release has always
had, and fail closed: an error, an unexpected answer, a repo that turns out
to have moved, or a `gh` without the `attestation` subcommand all abort with
nothing installed. A release from a private repo is checksum-only, with a
warning, since GitHub doesn't attest private-repo builds for a personal
account.

To install a specific, pinned tag rather than whatever `install.sh` resolves
as latest:

```sh
gh api 'repos/HaiNNT/c-hottag/contents/install.sh?ref=v0.4.0' \
  -H 'Accept: application/vnd.github.raw' | sh -s -- --version v0.4.0
```

## Uninstall and cleanup

```sh
chottag uninstall            # removes the PATH block and bin/chottag, bin/claude; keeps logins
rm -rf ~/.chottag/versions   # then, optionally, the installed binaries
```

or, to remove every account's login too:

```sh
chottag uninstall --purge    # deletes ~/.chottag, versions/ included; asks you to type "purge"
```

`chottag uninstall` does not stop or remove a launchd agent or systemd user
unit you installed from `packaging/`; do that yourself first:

```sh
launchctl bootout gui/$(id -u)/com.chottag.daemon        # macOS
systemctl --user disable --now chottag.service            # Linux
```

Deleting `~/.chottag/ca` (or `--purge`, which includes it) removes the CA:
sessions that cached it in `NODE_EXTRA_CA_CERTS` will need a restart to stop
trusting it.

## Terms

chottag is for one person switching between their own Claude accounts.
Reports about sharing accounts between different people, or otherwise using
chottag against Anthropic's terms, are out of scope for security reports
here.
