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
   user: `daemon logs`, `daemon run`, `proxy run`, `trace run|env|mark|summarize`,
   `help`. The daemon starts by itself when `claude` runs.
8. **Only read-only commands and everyday switches run without a permission
   prompt**: `status`, `doctor --json`, `update --check --json`, `version`,
   `tag`, `next`, `remote`, `rotate`, `auto`, `notify`, `plan`. Everything
   else (`login`, `logout`, `doctor --fix`, `update`, `rename`, `uninstall`)
   asks the user first. That is deliberate; don't work around it.

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
| show in a status line whether this session goes through chottag | `chottag statusline [--cmux]` (prints only, no side effect; `--cmux` also sets the cmux sidebar pill; one line: `c» <serving> · 5h 42% · 7d 18% · ↻ 19:00 · 2/3 ok`, `c» down` or `c» off`; never fails) |
| switch to a named account | `chottag tag <name> --json` |
| switch to the next account that is not limited | `chottag next --json` |
| add or re-login an account | `chottag login <name> --json` (timeout 600000) |
| set the account that owns new remote-control sessions, artifacts, routines | `chottag remote <name> --json` |
| keep an account out of `next` | `chottag rotate <name> off --json` |
| rename an account | `chottag rename <old> <new> --json` |
| remove an account | `chottag logout <name> --json` (ask first; see below) |
| check or repair the install | `chottag doctor --json`, then `chottag doctor --fix --json` if the user agrees |
| desktop notifications | `chottag notify on --json` / `chottag notify off --json` |
| automatic switching near a limit (on by default) | `chottag auto --json`; `chottag auto off --json` / `chottag auto on --json` |
| auto-switch mode | `chottag auto mode balanced --json` or `chottag auto mode cache-optimize --json` |
| tell chottag an account's plan size | `chottag plan <name> max20x --json` (tiers: `pro`, `max5x`, `max20x`, `team`) |
| check for, or install, a newer chottag release | `chottag update --check --json`, then `chottag update --json` if the user agrees |

Accounts are named by name, email, or a unique name prefix.

## Updating

`chottag update --check` reports whether a newer release exists; `chottag
update` installs it, following the repo the user installed from, restarting
the daemon only if no Claude session is running (`--restart` restarts it
anyway); `chottag update --version vX.Y.Z` installs a specific release,
including an older one, to roll back. It keeps the last three versions and
verifies each download against the release's checksums.

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
  `daemon`, `serving`, `label`, `fiveHourPct`, `sevenDayPct`, `resetsAt`,
  `okAccounts`, `rotationAccounts`).
- **They have none.** Offer chottag's line on its own; they add this to
  `~/.claude/settings.json` by hand:

  ```json
  {"statusLine":{"type":"command","command":"~/.chottag/bin/chottag statusline"}}
  ```

`chottag statusline` only prints: it has no side effect. Offer the cmux
sidebar pill only to a user who runs cmux, and add `--cmux` to the command
(`seg=$(~/.chottag/bin/chottag statusline --cmux 2>/dev/null)`) only with
their yes.

Use `chottag statusline`, not `chottag status --json`, in a status line: it
knows whether this session goes through chottag, and it is built for every
redraw. It prints `c» off` rather than failing when chottag is gone, but
before `chottag uninstall --purge`, tell the user to take the segment (or
the `statusLine` entry) out.
