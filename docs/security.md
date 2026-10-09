# Security

This page is a summary for users. The full policy, threat model and
recovery procedures live in
[SECURITY.md](https://github.com/HaiNNT/c-hottag/blob/main/SECURITY.md);
this page never contradicts it.

## What chottag can see

To swap a request's bearer, chottag holds each configured account's OAuth
access token in memory. Besides swapping tokens on your traffic, the
daemon also makes its own occasional request, using an account's own
token, to `/api/oauth/usage` — for an idle or already-limited account that
has sent no traffic of its own to observe usage from instead
(`internal/usagepoll`). It is a local man-in-the-middle only for the two
hosts it intercepts (`api.anthropic.com` and `mcp-proxy.anthropic.com`): a
`CONNECT` to any other host is a blind tunnel it never reads. An
absolute-form request is swapped only for those same two intercepted
hosts; to any other host it is parsed only enough to log its metadata,
then forwarded untouched. It never writes a token to disk: tokens live
only in Claude Code's own stores.

## The local proxy

The daemon listens on `127.0.0.1` only. Each install has its own secret in
`~/.chottag/ca/proxy.secret` (or `$CHOTTAG_HOME/ca/proxy.secret`; mode
0600); the `claude` shim never hands it over itself. It gives each `claude` its
own credential in the userinfo of `HTTPS_PROXY`:
`http://chottag.default.<sid>:<password>@127.0.0.1:47821`. The user names
the session (`<sid>` is 32 random hex characters, new for every `claude`)
and the password is `HMAC-SHA256(secret, "chottag-session-v1\n" + user)`,
so it proves the secret without revealing it and works for that user
only. The older `http://chottag:<secret>@...` form is still accepted, as an
unidentified caller.
Most HTTP clients that inherit `HTTPS_PROXY` send the credential back
automatically, but not all: Node's own built-in `fetch`, for example,
ignores `HTTPS_PROXY` entirely. Before handing a credential to `claude`, the
shim also challenges the daemon over its health endpoint and checks a
fresh `HMAC(secret, port, nonce)` proof, so a process that merely squats
the port is never trusted. A daemon from before v0.6.0 proves the secret but
accepts only the legacy credential; it does not say it understands session
credentials in that document, so the shim gives its sessions the legacy
`chottag:<secret>` credential instead.

Without a matching secret, the proxy answers
`407 Proxy Authentication Required` for:

- a `CONNECT` that would MITM one of the two intercepted hosts
  (`api.anthropic.com`, `mcp-proxy.anthropic.com`);
- an absolute-form request (`https://…` sent straight over the proxy
  connection), whatever host it names;
- a blind tunnel, only when the daemon itself is chained through an
  upstream proxy that carries its own credentials.

A `CONNECT` to any other host is relayed as a blind tunnel with no secret
check at all, because it can never carry a swapped token: another local
user, or a container sharing your loopback interface, can use chottag as
a generic relay to the open internet this way, but never to spend your
Claude accounts. Inside an intercepted tunnel, a request whose `Host`
differs from the host that tunnel's `CONNECT` actually targeted gets
`421`, instead of ever being forwarded with a swapped token under the
wrong name.

A child process `claude` spawns (a hook, an MCP server, a Bash-tool
command) inherits `HTTPS_PROXY`, secret included, and `NODE_EXTRA_CA_CERTS`.

## The local CA

`chottag setup` creates the CA under `~/.chottag/ca/`: `ca.pem` (public,
0644) and `ca.key` (private, 0600). From v0.4.0 on, a newly created CA is
name-constrained to `anthropic.com`, `claude.ai` and `claude.com`, and
excludes every IP address, so even a leaked `ca.key` can only mint
certificates for those domains. Whichever CA is in use, a leaf is only
ever minted for the host a `CONNECT` actually targeted. `chottag doctor --fix` repairs `ca.key`'s file mode if it has drifted from 0600.

## What is logged

`proxy.jsonl` (and `trace.jsonl`) record request metadata only — account
name, host, a templated path, status and timing — never a token, a
bearer, or a request or response body. `daemon.log` holds operational
lines only, never the proxy secret or a token.

## Session titles

`chottag name-session` and the `TITLE` column of `chottag sessions` read a
session's transcript, but only its `ai-title` and `custom-title` records (the
session's name). Nothing else in a transcript is read. A title is never
logged or stored by chottag; per session it keeps only a sha256 of the name it
set, in `sessions/names/`. `chottag names off` turns the hook off. With `chottag names model`, the title helper runs Claude Code with no tools and no MCP servers, so it can only reply with text; the prompt goes to it alone and is never logged or stored.

## Limits

- A process running as you, or root on this machine, is out of scope: it
  can already read your slot credentials and `proxy.secret` directly, and
  does not need chottag to spend your accounts.
- The open relay above is real: another local user (or a container
  sharing your loopback interface) can tunnel arbitrary traffic through
  chottag, though never to your Claude accounts.
- The CA is never installed into the system trust store. Only `claude`,
  and everything it spawns, trusts it, and only through
  `NODE_EXTRA_CA_CERTS`.
- Hooks, MCP servers and Bash-tool commands a session runs inherit
  `HTTPS_PROXY` with the secret, so they can spend your accounts through
  the proxy exactly as `claude` itself can.
- A CA created before v0.4.0 has no name constraint, and stays that way
  until you rotate it; `chottag doctor` reports an unconstrained CA as an
  informational finding, not a failure.

## Verifying a release

Every release ships a `checksums.txt`. From v0.4.0 on, releases on the
public repo also carry a build provenance attestation, checked with:

```sh
gh attestation verify chottag_<ver>_<os>_<arch>.tar.gz --repo HaiNNT/c-hottag
```

`chottag update` and `install.sh` both run this automatically and fail
closed if it doesn't check out.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting (this repo's
**Security** tab, **"Report a vulnerability"**) rather than a public
issue. See
[SECURITY.md](https://github.com/HaiNNT/c-hottag/blob/main/SECURITY.md)
for the full threat model and response timeline.

## Next

- [How it works](how-it-works.md)
- [Known limitations](known-limitations.md)
- [Troubleshooting](troubleshooting.md)
- [Documentation home](index.md)
