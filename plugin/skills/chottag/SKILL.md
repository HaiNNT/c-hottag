---
name: chottag
description: Switch which Claude account Claude Code uses on this machine, with chottag (c-hottag). Use when the user wants to switch Claude accounts, change the serving or remote account, add or log in an account, check usage or limits across accounts, or fix a chottag install, or mentions chottag or hottag. Runs chottag with --json and never edits ~/.claude.
argument-hint: "[chottag command and arguments, e.g. tag B]"
allowed-tools: Bash(chottag status:*), Bash(chottag doctor --json), Bash(chottag update --check --json), Bash(chottag version:*), Bash(chottag tag:*), Bash(chottag next:*), Bash(chottag remote:*), Bash(chottag rotate:*), Bash(chottag auto:*), Bash(chottag notify:*), Bash(chottag plan:*)
---

# chottag: switch Claude accounts

chottag keeps several Claude accounts logged in side by side, each in its own
slot, and routes every Claude Code request through a local proxy that picks the
account. The user's own Claude Code login ("Home") is never changed.

The full reference, every command, flag, exit code, error code and JSON
field, is https://github.com/HaiNNT/c-hottag/blob/main/docs/commands.md.
This skill covers what a Claude Code session needs day to day.

## Words

- **Home**: `~/.claude` and `~/.claude.json`, the user's normal login. chottag
  never writes there, and neither do you.
- **account**: a login chottag keeps in `~/.chottag/accounts/<name>`.
- **serving**: the account that pays for inference now — the name after
  `serving:` on `chottag status`'s first line (`serving: X   remote: Y`).
- **remote**: the account that owns new claude.ai objects — the name after
  `remote:` on that same line: remote-control sessions, artifacts,
  connectors. Existing objects stay with the account that created them.
  Routines are the exception: they always follow whichever account is
  remote.

`chottag status` then lists every account in a table with columns NAME, PLAN,
ORG, 5h, 7d and STATE, and ends with an `auto:` line: the mode, why it's
holding (if it is) and the last switch, e.g. `auto: balanced · holding A (5h
96%, resets in 9m) · last C→A 09:12 (limit)`, or `auto: off` when auto-switch
is off.

## With arguments

When invoked with arguments (`/chottag:chottag tag B`), run exactly:

```sh
chottag $ARGUMENTS --json
```

Then summarise the document for the user, as in "Reading results" below.
Without arguments, work out what the user wants and use "Common tasks".

## Rules

1. **Always pass `--json`** and decide from the document: `ok` is true on
   success; on failure branch on `error.code` (and `error.exit`), never on
   `error.message`, which is prose for humans. Relay every `warnings[]` entry.
2. **Log an account in with `chottag login <name> --json`**, run directly
   through Bash with `timeout: 600000` (the 10-minute maximum). It opens a
   browser and blocks until the user finishes there. Tell the user a browser
   window is waiting for them before you run it.
3. **Never run `/login` or `/logout` inside a Claude Code session.** They log
   Home in or out, not a chottag account.
4. **`tag` and `next` take effect from the session's next request.** They
   change the account for every Claude Code session started through chottag's
   `claude` shim (its `HTTPS_PROXY` names `127.0.0.1`), including this one if
   it was. A session started before chottag was installed is not routed.
5. **Never edit `~/.claude`, `~/.claude.json`, or anything in them**, and never
   edit the user's shell rc (`chottag setup` owns its PATH block). For a status
   line, show the snippet below and let the user apply it.
6. **Never run `chottag uninstall --purge` for the user.** It deletes every
   account's login and asks for a typed word in a terminal. Tell the user the
   command instead.
7. These refuse `--json` (they stream or run forever); don't run them for the
   user: `daemon logs`, `daemon run`, `proxy run`, `trace run|env|mark|summarize`
   (`summarize --all` prints the whole log),
   `help`. The daemon starts by itself when `claude` runs.
8. **Only read-only commands and everyday switches run without a permission
   prompt**: `status`, `doctor --json`, `update --check --json`, `version`,
   `tag`, `next`, `remote`, `rotate`, `auto`, `notify`, `plan`. Everything
   else (`login`, `logout`, `doctor --fix`, `update`, `rename`, `uninstall`,
   `policy`, `pool add`, `pool join`, `pool leave`, `pool rm`)
   asks the user first. That is deliberate; don't work around it.

## Spreading sessions over accounts

By default every session uses the one serving account. `chottag policy spread`
places each new session on the account with the most headroom and keeps it
there, moving it only near a switch point, on a limit, or when its prompt
cache is already cold and another account is clearly better (`policy: spread`, and
`pin: <name>` if one is pinned, show on `chottag status`). Offer it to a user
with several accounts who runs many sessions at once. Ask before running
`chottag policy spread`: it changes how every new session is placed. After
`chottag update`, run `chottag daemon restart` before turning spread on: an
older daemon ignores the policy and may reset it, and `chottag policy spread`
warns `daemon_predates_spread` when it finds one. Under it
`chottag next` is refused (`spread_next`); `chottag policy serial` goes back.
`pin: <name> (not a candidate now)` means the pinned account can't take new
sessions at the moment (limited, at a switch point, rotation off or needing a
login), so they are placed normally. `bad_policy` means a value other than
`serial` or `spread`.

## Pools

A pool is a named set of accounts with its own serving account, remote
account, policy and pin; the top-level fields are the `default` pool, and a
session picks its pool with `CHOTTAG_POOL=<name> claude`. `chottag pool --json`
lists them. `pool add`, `pool join`, `pool leave` and `pool rm` ask the user
first (not pre-approved). `chottag login <name> --pool <pool>` puts a new
account in that pool only (for an existing account it warns `pool_not_changed`; use
`pool join`). `tag <name>` and `remote <name>` act in the
account's pool; for an account in several, pass `--pool` (`pool_ambiguous`
otherwise). `next`, `tag --unpin`, `policy` and a bare `remote` take
`--pool` (default `default`). `rotate`, `plan`, `rename` and `logout` apply
in every pool.

**Before `pool join` that shares an account between pools, tell the user the
down-side** (the `shared_account` warning): the usage limit is shared, so the
pools affect each other. With a `spread` pool among them, their sessions
compete for the account, and heavy use in one pool moves the other's sessions
off it (their prompt caches go cold). With every pool on `serial`, if the
account serves both they use up its 5-hour window together and switch away
from it at the same time. Warn, then join only if the user still wants it.
When `login` says the email is already an account, `pool join` is the way to
use it in another pool. `update --version` below 0.8.0 is refused
(`pools_block_rollback`) while extra pools exist: `pool rm` them first (ask).
To remove a pool, take its members out (`pool leave`; an account in no other
pool needs `pool join <account> default` first), then `pool rm`.

Seeing them: `chottag status --json` has `pools[]` (each pool's `serving`,
`remote`, `policy`, `pin`, `accounts`, `liveSessions`, and the daemon's
`decision` and `lastSwitch`), `pools` and `shared` on each account, and a
`pool` on each session; they are there only while a pool other than
`default` exists (the top-level `serving`, `remote` and `auto` are
`default`'s). The text form adds a `POOLS` column and a line per pool, with
`(B shared with personal)` for a shared account. `chottag statusline` shows
`[work]` after the account for a session outside `default` (`pool` in
`--json`). Notices name the pool (`chottag: work: switched to C`).

**A pool that cannot serve fails closed.** With more than one pool, a request
its pool cannot serve (no serving account, every member out of rotation,
needs a login) is answered by chottag itself with a 503 naming the pool; it
never goes out on Home's own login, which could belong to another pool, and
the safety net's resend is off. An existing object whose owner needs a login
gets the 503 as well. Claude Code retries it, so a fix lands by
itself: `chottag pool` shows the members, then `chottag login`, `chottag
rotate <name> on` or `chottag pool join`. `chottag doctor` has the matching
warning: its `roles` row is `info` ("pool work has no account in rotation")
for such a pool. With only `default` an unservable request still passes
through as before.

Warnings to relay: `shared_account` (above), `pool_not_changed` (`login
--pool` for an account that already exists), and `daemon_predates_pools`
(an error for `pool add`, a warning for `pool join`: the running daemon is
older than 0.8.0 or reports no version, so it cannot read a version 2
`state.json`; run `chottag daemon restart`, telling the user first, and repeat
the `pool add`). Once a pool exists the shim also refuses every session,
`default` included (exit 2), against such a daemon until it is restarted.

## If `chottag` is not found

- Installed, but this shell predates the install: use the full path,
  `"${CHOTTAG_HOME:-$HOME/.chottag}/bin/chottag"`, and tell the user to open a
  new shell. (Those calls are outside this skill's pre-approved commands, so
  they ask for permission.) If `CHOTTAG_HOME` was set at install time,
  `chottag setup` also recorded it in the same rc block as the PATH line, so a
  new shell resolves it without it needing to be set again by hand.
- Not installed: follow
  https://github.com/HaiNNT/c-hottag/blob/main/docs/for-claude-code.md,
  which installs without a clone and then adds this plugin.

## Common tasks

| the user wants | run |
|---|---|
| which account is serving, usage, limits | `chottag status --json` |
| which account is this session on, or what each live session uses | `chottag statusline --json` (`account`: this session's own account; `serving`: its pool's; `pool`: the pool, outside `default`); `chottag status --json` lists every live session in `sessions[]` |
| show in a status line whether this session goes through chottag | `chottag statusline [--cmux]` (prints only, no side effect; `--cmux` also sets the cmux sidebar pill; one line: `c» <this session's account> · 5h 42% · 7d 18% · ↻ 19:00 · 2/3 ok`, with `[pool]` after the account outside `default`, `c» down` or `c» off`; never fails) |
| switch to a named account | `chottag tag <name> --json` |
| switch to the next account that is not limited | `chottag next --json` |
| spread sessions over accounts / pin new sessions | `chottag policy spread --json` (asks first: it changes how every new session is placed, and is not pre-approved); `chottag tag <name> --json` then pins new sessions to it (the pin is for new sessions only; on a rotation-off account it warns and is ignored); `chottag tag --unpin --json` clears the pin; `chottag policy serial --json` goes back to one serving account. Under spread `chottag next` is refused (`spread_next`) |
| pools: list, create, put an account in one, take it out, remove one | `chottag pool --json`; `chottag pool add <name> --json`, `chottag pool join <account> <pool> --json`, `chottag pool leave <account> <pool> --json`, `chottag pool rm <name> --json` (the last four ask first; explain the shared-account down-side above before a join that shares) |
| add or re-login an account | `chottag login <name> --json` (timeout 600000) |
| set the account that owns new remote-control sessions, artifacts, routines | `chottag remote <name> --json` |
| keep an account out of `next` | `chottag rotate <name> off --json` |
| rename an account | `chottag rename <old> <new> --json` |
| move one existing claude.ai object to another account, or see its owner | `chottag own <session\|environment\|artifact\|connector> <id> [account] --json` (asks first: it is not pre-approved) |
| register slots that already hold a login | `chottag adopt [--claude PATH] --json` (asks first); `chottag setup --label NAME` and `chottag login <name> --claude PATH` also take the real `claude`'s path |
| remove an account | `chottag logout <name> --json` (ask first; see below) |
| check or repair the install | `chottag doctor --json`, then `chottag doctor --fix --json` if the user agrees |
| desktop notifications | `chottag notify on --json` / `chottag notify off --json` |
| automatic switching near a limit (on by default) | `chottag auto --json`; `chottag auto off --json` / `chottag auto on --json` |
| auto-switch mode | `chottag auto mode balanced --json` or `chottag auto mode cache-optimize --json` |
| tell chottag an account's plan size (a `max?` plan on `status` means a Max account of unknown size: ask the user 5x or 20x) | `chottag plan <name> max20x --json` (tiers: `pro`, `max5x`, `max20x`, `team`; `--units N` overrides the tier's capacity per 1%, and a later `plan` without it clears the override) |
| check for, or install, a newer chottag release | `chottag update --check --json`, then `chottag update --json` if the user agrees (`--repo OWNER/NAME` names another repo; `--check` reads GitHub directly) |

Accounts are named by name, email, or a unique name prefix.

## Updating

`chottag update --check` reports whether a newer release exists; `chottag
update` installs it, following the repo the user installed from, restarting
the daemon only if no Claude session is running (`--restart` restarts it
anyway); `chottag update --version vX.Y.Z` installs a specific release,
including an older one, to roll back. Before installing, `update` backs up
`state.json` into `~/.chottag/backups/` (`backups` in `--json`), and `setup`
backs up the shell rc file the same way before changing it. `update` keeps
the last three versions and verifies each download against the release's
checksums. `--no-restart` installs and leaves the daemon running.

The daemon also checks for a new release about every 6 hours (and `update --check` posts the one notice per version if it finds it first); `status` shows
`update: <v> available`, and `status --json` has `update` and `updates`, and `statusline --json` has
`updateAvailable` (the newer version, when there is one) and `restartPending`.
**Tell the user when one is available and offer `chottag update`.** The check
is on by default and can be turned off with `chottag update --auto-check off`.
`chottag update --auto-install on` lets the daemon install releases by itself
(same major version, at least 24 hours old). **Never turn auto-install on
unless the user says yes.**

When a newer chottag is installed but the daemon still runs the old one,
`status` says `daemon: running X, installed Y (restarts when idle, ...)`
(`daemon.restartPending` in `--json`, `⟳Y` on the status line). The daemon
restarts itself onto it once the proxy is idle (no request in flight, none in
the last 5 minutes); this is on by default and `chottag update --auto-restart
off` turns it off. To switch at once, run `chottag daemon restart`.

**Finish the update.** When `chottag update --json` returns `"daemon":
"deferred"` with `"selfRestart": false` (the text says the running daemon
"can't restart itself" or "won't restart onto it"), the new version is not in
use yet and nothing will switch to it on its own. Tell the user, then run
`chottag daemon restart` to finish the update they asked for: it takes a few
seconds, and running sessions retry any request caught in the gap. With
`"selfRestart": true`, nothing more is needed.

If `chottag update` exits with `update_in_progress`, another update is
running (the user's, or the daemon's automatic install). Wait, then check
with `chottag update --check --json` or `chottag status --json`; don't retry
in a loop. Setting `CHOTTAG_NO_UPDATE_CHECK=1` in the daemon's environment
also switches the update check off.

## Reading results

- `status`: `daemon.liveSessions` counts the live sessions chottag launched.
  `serving` and `remote` are account NAMES that index `accounts[]`.
  Usage percentages are 0–100. An absent percentage, or `stale: true` on the
  account, means unknown: never show it as 0%. `limited: true` means limited
  now, until `limitedUntil` when that is present. `token` is a state
  (`ok | expiring | stale | needs-login`); for `needs-login`, offer
  `chottag login <name>`.
- `tag` / `next`: `serving` is the new account and `previous` the old one.
- `auto`: `enabled`, `mode`, `decision` (what the daemon is doing now) and
  `lastSwitch`. When a request hits a limit, chottag switches and resends it
  itself; `lastSwitch.retried` says whether that worked. Only when the notice
  says "Resend your last message" was the message refused, and it
  only needs resending: the next request goes to the new account.
- Failure codes and what to do:
  - `no_candidate` (exit 3): every other account is limited, needs a login,
    out of rotation, or genuinely full (100%). An account merely above its
    own switch point is picked anyway as a fallback — `serving` names it and
    `fallback` is `true` — so `no_candidate` means there was nothing left
    with any capacity at all, not just nothing below its switch point.
    `error.skipped` has each skipped one's reason (`limited | needs_login |
    above_switch_point | out_of_rotation`) and reset time; tell the user.
    Only if they ask, `chottag next --force --json` switches anyway.
  - `unknown_account` / `ambiguous_account`: run `chottag status --json`, show
    the names, and ask which one.
  - `confirmation_required` (exit 3, from `logout`): ask the user; if they
    agree, run again with `--yes`.
  - `role_held` (exit 3): the account is serving or remote. Ask; `--force`
    moves the role to the next account first.
  - `login_failed`: the browser flow did not finish; offer to run it again.
  - `usage` (exit 2): the command line was wrong; fix it, don't retry blindly.
- `doctor`: each row has `id`, `status` (`ok | problem | info | fixed |
  skipped`), `detail` and `hint`. When every row is clean, the document is
  `ok: true` with the rows under `checks`. When one or more rows are a
  `problem`, the whole run is `ok: false`, exit 3, `error.code`
  `doctor_problems`, and the rows are under `error.checks` instead — look
  there, not at a top-level `checks`, to find them. Report the `problem` rows
  with their hints. `--fix` repairs only what is safe; the rest need the
  hinted command.

## Status line

chottag must not edit `~/.claude/settings.json`, and neither do you. chottag
never replaces the user's status line: `chottag statusline` gives their own
status line something to show. Ask first whether they already have one.

- **They have one (the usual case).** Add chottag's segment to what their
  script already prints. Show them the change and make it only with their
  yes. If their `statusLine` runs a command rather than a script, give them
  this script to save OUTSIDE `~/.chottag` (for example
  `~/.local/bin/chottag-statusline.sh`: `chottag uninstall --purge` deletes
  the whole `~/.chottag` tree), `chmod +x`, with their command in place of
  `YOUR_EXISTING_COMMAND`, and let them point `statusLine` at it:

  ```sh
  #!/bin/sh
  # Your status line, with chottag's segment added at the end.
  input=$(cat)                                        # Claude Code's JSON for this session
  line=$(printf '%s' "$input" | YOUR_EXISTING_COMMAND)
  seg=$(~/.chottag/bin/chottag statusline 2>/dev/null)
  printf '%s  %s\n' "$line" "$seg"
  ```

  To show only some of it, read fields instead:
  `chottag statusline --json | jq -r '.serving'` (fields: `session`,
  `daemon`, `serving`, `account`, `label`, `fiveHourPct`, `sevenDayPct`, `resetsAt`,
  `okAccounts`, `rotationAccounts`, `updateAvailable`, `restartPending`).
- **They have none.** Offer chottag's line on its own; they add this to
  `~/.claude/settings.json` by hand:

  ```json
  {"statusLine":{"type":"command","command":"~/.chottag/bin/chottag statusline"}}
  ```

The text line ends with ` · ↑<v>` when the update check found a newer
release `<v>`, and ` · ⟳<v>` when `<v>` is installed but the daemon still
runs the old one (it restarts itself when idle). The `off`, `down` and `up`
lines never show them.

`chottag statusline` only prints: it has no side effect. Offer the cmux
sidebar pill only to a user who runs cmux, and add `--cmux` to the command
(`seg=$(~/.chottag/bin/chottag statusline --cmux 2>/dev/null)`) only with
their yes.

Use `chottag statusline`, not `chottag status --json`, in a status line: it
knows whether this session goes through chottag, and it is built for every
redraw. It prints `c» off` rather than failing when chottag is gone, but
before `chottag uninstall --purge`, tell the user to take the segment (or
the `statusLine` entry) out.
