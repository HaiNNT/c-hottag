# Troubleshooting

The emergency exit works no matter what else is wrong: `CHOTTAG_BYPASS=1
claude` skips chottag entirely for that one launch and runs Claude Code
straight against your normal Home login. For most other symptoms, start
with `chottag doctor`, and `chottag doctor --fix` to repair what it can
safely fix on its own.

`claude auth ...` and `claude setup-token` already skip chottag by themselves,
so logging Home in or out needs no variable. `/login` typed inside a session
is not seen by the shim; start that session with `CHOTTAG_BYPASS=1 claude`.

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

Since 0.8.6 an account also becomes `needs-login` when its refresh exits 0
at least three times in a row, over at least 10 minutes, yet the token stays
expired (a dead refresh token, say). The daemon log says, in two lines:

```
chottag: refresh C: claude exited 0 but the token is still expired; retry in 15m
chottag: refresh C: still expired after 6 tries over 16m; it needs a login (run: chottag login C)
```

and you get a notice (the numbers are the tries so far and the minutes they
span, at least 3 and 10). Run `chottag login <name>`. Until you do, the daemon
still tries a refresh once every 15 minutes, whatever the account's role, and
if one renews the token the account is back without a login (`refresh C:
renewed, ...`).

## An account's token stays stale

`token: stale` on `chottag status` means the slot's access token has expired.
Since 0.8.6 it clears by itself when the token renews, or when a request
goes out as that account again. The daemon also refreshes each pool's serving
and remote account before its token runs out, and again about 10 seconds
after your Mac wakes (prompts sent in those 10 seconds plus the refresh still
go out on Home's own login, and only those two accounts are covered, not
every rotating member). `chottag daemon logs` says what each refresh did, one
line per outcome (the same line at most every 10 minutes, a different one
always):

- `chottag: refresh C: renewed, expires in 7h59m (request, 5.2s)`: it worked.
  The word in parentheses is what started it: `request`, `warm`, `forced` or
  `wake`.
- `chottag: refresh C: token unchanged, not yet due (warm, ...)` is not a
  problem: Claude Code renews only close to expiry, so a refresh that ran a
  little early left the token as it was.
- `chottag: refresh C: claude exited 0 but the token is still expired; retry
  in 2m`: Claude Code ran and renewed nothing. If it keeps up for 10 minutes
  the account is `needs-login` (above).
- `chottag: refresh C: failed: <reason>`: the attempt itself failed. A locked
  Keychain is retried every 30 seconds for 5 minutes, then less often.

Before 0.8.6 none of this was logged, and a serving account with an expired
token sent its first prompts on Home's own login until a refresh finished.

## An account shows another account's email

Before 0.8.3, `chottag login` ran `claude` through chottag's own shim; before
0.8.4 the daemon's background token refresh did. Either could make Claude Code
record the serving account's email in another account's slot (its organisation
stayed right). The symptom is several accounts on `chottag status` showing one
email. Several accounts may share an email legitimately, when one login is in
several orgs; `chottag doctor`'s `identities` row (info) lists them either way.
Re-login only the accounts that are wrong. Reported in
[#2](https://github.com/HaiNNT/c-hottag/issues/2).

To repair it, upgrade (`chottag update`), run `chottag daemon restart`, then run
`chottag login <name>` for each affected account (not the ones whose shared
email is right). `chottag adopt` refuses to record a slot whose email moved onto
a serving account's with its organisation unchanged, and warns `identity_suspect`.
If the wrong email was already recorded before you upgraded, `adopt` sees no
change and does not warn: use `chottag status` and doctor's `identities` row to
find it.

## STATE says passthrough:

`passthrough: ` in the STATE column means requests meant for that account
went out on Home's own login instead (a serving request; since 0.8.5 a remote
or owner request is refused with a 503 instead), because chottag could not use the
slot's token. Fix the underlying account (usually a re-login with `chottag
login <name>`) and the passthrough clears on the next successful request.

## Remote Control or a connector acts as the wrong account after a resume

Before 0.8.5, a Remote Control, connector or artifact request whose remote
or owner account had a stale token went out on Home's own login (the log said
`chottag: passthrough A stale`), so after a resume it could act as the wrong
account. Upgrade (`chottag update`), run `chottag daemon restart`, and
reconnect Remote Control once. From 0.8.5 such a request waits up to 15
seconds for a refresh and, if the account still has no usable login, gets a
503 that says `account A (remote) has no usable login right now; run:
chottag login A` (the log says `chottag: refused a remote request: A's token
is stale; run `chottag login A` if it persists`); never Home's login. Run `chottag login
A`.

## An object returns 403 or 404 after 0.8.5

A claude.ai object that only Home's login can see (one created before
chottag, or by a session run with `CHOTTAG_BYPASS=1`) used to work by
accident: the remote account was refused and chottag resent the request on
Home's login. From 0.8.5 that resend is gone for remote and owner requests,
so Claude Code sees the 403 or 404. Either point `chottag remote <account>` at
an account that can see the object, or use the object from a
`CHOTTAG_BYPASS=1 claude` session. See also
[Known limitations](known-limitations.md).

## A session gets 407

A running session that suddenly gets `407` was started before the v0.4.0
update, or before `ca/proxy.secret` changed some other way; either way, it
is still holding a credential the daemon no longer recognizes (a per-session
one is derived from the secret, so it stops working when the secret changes). Restart the
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
