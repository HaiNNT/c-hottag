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
- Not installed: follow the "For Claude Code" section of the README at
  github.com/HaiNNT/c-hottag (clone, `./install.sh`, then this plugin).

## Common tasks

| the user wants | run |
|---|---|
| which account is serving, usage, limits | `chottag status --json` |
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

- `status`: `serving` and `remote` are account NAMES that index `accounts[]`.
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

chottag must not edit `~/.claude/settings.json`. To show the serving account
in the user's status line, give them this script to save OUTSIDE
`~/.chottag` (for example as `~/.local/bin/chottag-statusline.sh`, not
anywhere under `~/.chottag`, since `chottag uninstall --purge` deletes that
whole tree and would take the script with it), `chmod +x`, with their
existing status line command in place of `YOUR_EXISTING_COMMAND`, and let
them point `statusLine` at it themselves:

```sh
#!/bin/sh
# Wraps an existing Claude Code status line and appends chottag's serving account.
input=$(cat)
existing=$(printf '%s' "$input" | YOUR_EXISTING_COMMAND)
serving=$(chottag status --json 2>/dev/null | jq -r '.serving // empty' 2>/dev/null)
if [ -n "$serving" ]; then
  printf '%s  ★ %s\n' "$existing" "$serving"
else
  printf '%s\n' "$existing"
fi
```

The settings entry they add (in `~/.claude/settings.json`, by hand):

```json
"statusLine": { "type": "command", "command": "~/.local/bin/chottag-statusline.sh" }
```

`chottag status --json` reads a local cache only, so it is safe on every
refresh. The script needs `jq`. Before `chottag uninstall --purge`, tell the
user to remove this `statusLine` entry (or point it back at
`YOUR_EXISTING_COMMAND`) first: `--purge` does not touch `settings.json`, and
`chottag status` will be gone, so the ★ indicator would otherwise just
disappear silently with no sign anything is wrong.
