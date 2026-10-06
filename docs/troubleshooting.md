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
2026-10-03 09:15:02 chottag: refresh C: claude exited 0 but the token is still expired; retry in 15m
2026-10-03 09:31:02 chottag: refresh C: still expired after 6 tries over 16m; it needs a login (run: chottag login C)
```

and you get a notice (the numbers are the tries so far and the minutes they
span, at least 3 and 10). Run `chottag login <name>`. Until you do, the daemon
still tries a refresh once every 15 minutes, whatever the account's role, and
if one renews the token the account is back without a login (`refresh C:
renewed, ...`).

Since 0.9.3 the daemon also records `needs-login` where it sees it: a usage
poll or a refresh that finds the slot holds no login (the daemon log says
`usage poll B: no usable login` or `refresh B: failed: no usable login`) marks
the account and posts one notice, and auto-switch never switches onto it. The
daemon stops polling it until you log it in again.

## An account shows an old reading, `45% (2h ago)`

`chottag status` shows a reading older than 10 minutes with its age. The
daemon polls an idle account about every 30 minutes, so a reading hours old
means the polls are failing: look for `usage poll NAME idle 429` lines in the
daemon log (it then waits the server's `Retry-After`, else 60 and 120
minutes), or for a `needs-login` state, which stops polling until you run
`chottag login NAME`. A rotation-off account is polled every 2 hours.

## An account's token stays stale

`token: stale` on `chottag status` means the slot's access token has expired.
Since 0.8.6 it clears by itself when the token renews, or when a request
goes out as that account again. The daemon also refreshes each pool's serving
and remote account before its token runs out, and again about 10 seconds
after your Mac wakes (prompts sent in those 10 seconds plus the refresh still
go out on Home's own login, and only those two accounts are covered, not
every rotating member). `chottag daemon logs` says what each refresh did, one
line per outcome (the same line at most every 10 minutes, a different one
always). Since 0.9.1 every daemon log line starts with the local time,
`2006-01-02 15:04:05` (the lines below are shown without it):

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

## Remote Control disconnected: signed-in account changed

Claude Code stops Remote Control with "signed-in claude.ai account or
organization changed on this machine" when its own validate call
(`POST /api/oauth/validate`) is answered by a different account from the one
it pinned when Remote Control started. Before 0.9.2 that call was served by
whichever account was serving at that moment, so a pool move or a daemon
restart (which restarts the usage-driven choice) could drop some sessions and
leave others, and `/rc` reconnected them. From 0.9.2 chottag answers a
session's validate with the same account for the session's life, also across
a daemon restart. It only moves to the serving account when the first one
cannot answer (removed, logged in again as another email, left the pool, has
rotation off, needs login, or its login cannot be refreshed), and then
`daemon.log` has a `validate for session ... moved from C to D: C <reason>`
line; that session may still disconnect once, and `/rc` brings it back. A
token that is only stale is refreshed first, so it is not a reason. A session
that started before the update has no recorded account: its first validate
after the update records whichever account is serving then, which may not be
the one it pinned under the old daemon, so it can drop once more; run `/rc`
once and it stays.

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

## Every account reads as needing a login after a logout or a crash

On macOS the daemon keeps the login session it was started in. If that
session ends (a logout, or a WindowServer crash) while the daemon lives on,
its `security` commands can no longer reach your keychains. Since 0.9.4 that is
not read as "no login": the accounts show `stale`, nothing is marked
`needs-login`, and no notice is sent. A minute or two after the first failure,
if a fresh check still cannot reach the keychains (or two or more accounts that
read fine earlier all stop reading together, which counts even when that check
passes), the daemon logs `the login
session this daemon started in has ended` and exits, and the next `claude`
starts a fresh one in your live session. A daemon run by a supervisor
(launchd) is left running, and logs that once instead. A login that
is really gone (the slot's keychain item deleted) is still marked
`needs-login`: at once for an account this daemon has not read yet, and after a
minute of misses for one it read earlier. The first time a slot reads as
needing a login, `daemon.log` has a line with the underlying `exit status N`.

## `passthrough: token needs-login` stays after I logged in again

Since 0.9.4 `chottag login` (and a token renewal, or a usage poll that works)
clears that text within a few seconds, and a daemon start drops the previous
daemon's marks. An older daemon kept it until the account's first request on its
own login, which for a rotation-off account could be a long time:
`chottag daemon restart` clears it.

## An account's login was refused (401)

A desktop notice `chottag: C's login was refused (401)` (or 403) means a
request that chottag sent as account C on a conversation route
(`/v1/messages`, or `POST /api/oauth/validate`) was refused by Anthropic, even after
chottag refreshed C's token and tried again, so the request went out once on
your own (Home) login instead. It is not route drift. Since 0.9.1 the notice
comes at most once an hour per account, and the daemon log has a line for each
one: `2026-10-03 17:28:23 chottag: C's login was refused (401) on POST
/v1/messages; sent on Home's own login` (with more than one pool, the line
ends `; not resent`, and so does the notice's text: nothing goes out on Home's
login). If it was a one-off (for example C's token was renewed at that moment),
nothing is needed: since 0.9.1 a request refused while C's refresh is running
waits for it and retries with the new token. If it repeats, run `chottag login
C`.

## Route drift after a Claude Code update

`chottag doctor`'s `route-drift` row and the desktop notice
`chottag: route drift` mean Claude Code's traffic no longer matches the route
table chottag learned. Since 0.9.1 these count: a request about a claude.ai
object (Remote Control, a connector, an artifact, a routine), a request to a
route chottag's table does not list (what a new Claude Code route looks
like), and a 404 other than on `POST /v1/messages` (since 0.9.3 that one is
Claude Code's own thread-continue answer and is passed through, marked
`passed404` in `proxy.jsonl`). A 401 or 403 on `/v1/messages` or `POST /api/oauth/validate` is the notice above
instead. Run
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
