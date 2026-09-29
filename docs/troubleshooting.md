# Troubleshooting

The emergency exit works no matter what else is wrong: `CHOTTAG_BYPASS=1
claude` skips chottag entirely for that one launch and runs Claude Code
straight against your normal Home login. For most other symptoms, start
with `chottag doctor`, and `chottag doctor --fix` to repair what it can
safely fix on its own.

## Claude won't start

Check `chottag daemon logs` for the daemon's own error, or run `chottag
daemon run` in the foreground to watch the startup error as it happens. A
common cause is a CA or `ca.key` permission refusal: the key must stay
private, and `chottag doctor --fix` repairs its mode.

## The port is taken

The daemon binds `47821` on `127.0.0.1` by default; `state.json`'s `port`
field changes it. See packaging's [When the port stays
taken](https://github.com/HaiNNT/c-hottag/blob/main/packaging/README.md#when-the-port-stays-taken)
for what to do next.

## An account says needs-login

A `needs-login` state on `chottag status` means the slot's login has
expired or was revoked. Run `chottag login <name>` to log it back in.

## STATE says passthrough:

`passthrough: ` in the STATE column means requests meant for that account
went out on Home's own login instead, because chottag could not use the
slot's token. Fix the underlying account (usually a re-login with `chottag
login <name>`) and the passthrough clears on the next successful request.

## A session gets 407

A running session that suddenly gets `407` was started before the v0.4.0
update, or before `ca/proxy.secret` changed some other way; either way, it
is still holding a secret the daemon no longer recognizes. Restart the
session.

## HTTPS_PROXY conflicts with the daemon

If the shell's own `HTTPS_PROXY` differs from the daemon's upstream, the
`claude` shim refuses to start Claude Code from that shell, with an error
naming both proxies, rather than route traffic somewhere you did not
choose. Either unset `HTTPS_PROXY` in that shell, or run `chottag daemon
restart` from the shell you actually want the daemon to read its
environment from.

## The daemon runs an older version

`claude` starts the daemon on demand but does not upgrade an already
running one. Run `chottag daemon restart` after `chottag update` to pick
up the new binary.

## Route drift after a Claude Code update

`chottag doctor`'s `route-drift` row and the desktop notice mean Claude
Code's traffic no longer matches the route table chottag learned. Run
`chottag trace on`, reproduce the drifted request, then `chottag trace
summarize`, and open an issue with that output — it holds route shapes,
never tokens. (A claude.ai connector's own 401/403/404 from the account
chottag recorded as its owner is passed through and not counted.)

## A session isn't switching

A session that started before chottag was installed, or was launched
outside the `claude` shim, never goes through the proxy, so `tag` and
`next` cannot affect it. Inside such a session, `/status` also keeps
showing Home's account rather than the one chottag is serving from — a
known limitation, not a bug.

## Getting out

`chottag daemon stop` stops the daemon; the next `claude` launch through
the shim starts it again. To remove chottag entirely, see
[uninstall.md](uninstall.md).
