# Known limitations

## Not routed

Claude Code's IDE extensions and the Desktop app don't start Claude Code
through the `claude` shim, so they never go through chottag's proxy: they
always run on Home's own login. A terminal session that started before
chottag was installed is the same way — it isn't routed either.

## Max plan size

chottag can't tell a `max5x` account from a `max20x` account on its own,
so a Max account counts as `max5x` (the safer under-estimate for
auto-switch) until you set its real size with `chottag plan`.

## The remote-control banner

Inference always runs on the serving account, even for a remote-control
session that a different account owns. So that session's limit banner
shows the serving account's limit, not the remote account's own.

## /status and /usage

Inside Claude Code, `/status` keeps showing Home's account, not the
account chottag is actually serving from. `/usage` does show the serving
account, but Claude Code caches its numbers in `~/.claude.json`, so a
session outside chottag can keep showing those numbers until it refreshes
on its own.

## The prompt cache

A switch always costs one prompt cache rebuild: the newly serving account
has never seen the conversation, so its first response after the switch
rebuilds the cache from scratch.

## Routes can drift

chottag's route table is learned from Claude Code's own traffic, not from
a published API, so a Claude Code update can change it. `chottag doctor`'s
`route-drift` row and the desktop notice flag it when it happens; see
[troubleshooting.md](troubleshooting.md) for the fix. A claude.ai
connector's own 401/403/404 from its owner account (a stale MCP session,
an auth handshake) is passed through as is and never counted as drift, and neither is a
401 or 403 on `/v1/messages` or `POST /api/oauth/validate`: that is an account login refusal, with its own
notice (see troubleshooting). If
that recorded owner can no longer see the connector at all (removed from
the org, say), every call to it now just fails, quietly, with no drift
notice; a later `/v1/mcp_servers` listing on another account doesn't fix
it (`owners.json` is first-writer-wins) — `chottag own <kind> <id>
<account>` reassigns it.

## An object no account here can open

For an object whose owner chottag does not know, chottag tries the pool's
other accounts, but only for a `GET` or `HEAD`: a `POST` is never repeated on
another account. An object that no account of the pool can open is reported
once an hour per account (`chottag: an artifact's owner is unknown`) and is not
tried again for an hour. It is not route drift. If it is yours, log in the account that made it, or use it from
a terminal outside chottag (`CHOTTAG_BYPASS=1 claude` inside a chottag session
still goes through chottag, because it inherits `HTTPS_PROXY`).

## Remote Control can still disconnect when a session's account is lost

chottag keeps a session's validate answer on one account (see
[how it works](how-it-works.md)), but if that account is removed, leaves the
session's pool, has rotation off or needs a login (or its login cannot be
refreshed), the answer moves to the serving account and
Claude Code's Remote Control may stop with "signed-in account changed". Run
`/rc` to reconnect. The same can happen once to a session that started before
0.9.2, on its first validate after the update. chottag tells a re-login by the
account's email, so a name logged in again under the same email but into
another organization is not noticed, and the session may disconnect with no log
line. See [troubleshooting](troubleshooting.md).

## Objects only Home's login can see

A claude.ai object (artifact, Remote Control session, connector) that only
Claude Code's own (Home) login can see, such as one created before chottag or
by a `CHOTTAG_BYPASS=1` session, no longer works through chottag from 0.8.5.
A request for it goes out as the remote account, which gets a 403 or 404, and
chottag does not resend it on Home's login (that resend let an object act as
the wrong account). Run `chottag remote <account>` for an account that can see
it, or use it from a `CHOTTAG_BYPASS=1 claude` session. See
[troubleshooting.md](troubleshooting.md).

When the remote account's own login was stale (a 401), the first load
refreshes it and may still fail; the next load discovers the owner.

## Linux

Linux builds and passes CI, but has not been used for real yet.

## cmux

Inside cmux, a terminal that wraps `claude` with its own session hooks,
chottag hands the launch off to cmux's own wrapper once, so those hooks
keep working in every shell cmux runs, including one where its `claude`
shell function is absent. `CHOTTAG_BYPASS=1` still skips chottag entirely,
hand-off included.

If cmux's own "Claude Binary Path" setting points straight at the real
Claude Code (rather than at chottag or being left empty), cmux launches
that directly and chottag is skipped for that launch — chottag's hand-off
cannot change where cmux's own setting points. Point it at chottag's own
`bin/claude` (chottag's own install directory), or leave it empty.

In a shell without cmux's claude function, chottag still runs (proxied),
and cmux's hooks are what is lost.

## Resuming sessions

`chottag resume` only knows sessions launched through chottag's `claude`. A
session you moved with `/cd` comes back in the directory it was launched in. A
reused process id can hide one session until that process exits. Outside cmux
it prints the command instead of running it. A session started with
`claude --continue` names no id, so the id chottag captured may not be
resumable.

In the Claude apps, a resumed session can show under Claude Code's neutral
name, `<host>-<words>`, without its earlier conversation. Claude Code records
which account a session's Remote Control belongs to; chottag answers that
check as the serving account. On a resume, Claude Code compares the record
with the login in its own credential store (Home's). When they differ, it
starts a fresh Remote Control session with a neutral name, leaves out the
history, and marks the conversation so that every later resume does the same.
A switch of accounts during a session can mark it too. chottag cannot clear
the mark: it is in the transcript, under `~/.claude`, which chottag never
writes. `chottag resume` passes the session's title as its Remote Control
name, so the name is right, but later renames do not reach the apps. To
rename it there, use the Claude app. See [resume.md](resume.md).

## Home's login has to stay valid

Claude Code checks its own login (Home's, the one `~/.claude` holds) before it
sends anything, so chottag cannot help when that login expires and its
renewal fails: every session says `Login expired · Please run /login`, while
chottag's accounts are fine. chottag notices the refusals and says so (a
desktop notice, a line in `chottag status`, the `home-login` row in `chottag
doctor`). Run `/login` in one Claude Code session. See
[troubleshooting](troubleshooting.md).

## Session names

`chottag name-session` names a session at its second prompt, as `<title> ·
<branch>` (`<folder> HH:MM` when there is no branch). It does nothing at the
first prompt: Claude Code generates a title only for a session that has no
name yet, so a name set then would leave the session without a title for
good, which is what happened up to 0.10.3. A session whose title is still
missing at the second prompt is named by its branch alone, and then never
gets a title. While the name is chottag's, a later prompt renames the session
after the branch it moved to (a worktree it entered); the name changes at the
next prompt, not at the move. Claude Code's running-session listing shows
only a name set from a prompt hook. The transcript's format is internal to
Claude Code, so a change there leaves the title empty and the name as the
branch. A session you named yourself is never renamed. A forked session that
carries the name chottag gave its parent is treated as named by you, so it
keeps that name and is not upgraded. See [resume.md](resume.md#session-names).

## Tools that ignore HTTPS_PROXY

A child process that ignores `HTTPS_PROXY` never reaches chottag's proxy
at all, and runs on Home's login instead. Node's own built-in `fetch` is
one example.
