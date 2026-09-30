# Install and manage chottag for a user (for AI agents)

This page is written to an AI coding agent (for example, a Claude Code
session) asked to install or manage chottag (c-hottag) for the person
running it. Follow the steps in order; every command is copy-pasteable.
[For Claude Code](https://github.com/HaiNNT/c-hottag/blob/main/README.md#for-claude-code)
in the README points here.

## 0. Before anything

- Confirm the user is on macOS or Linux, amd64 or arm64. `install.sh`
  detects this itself and refuses a machine it does not support; macOS is
  the platform this project actually runs on day to day, Linux builds and
  passes CI but has not seen real use.
- Confirm Claude Code is installed (`claude --version`).
- Run `gh auth status`. It must show a login: `install.sh` downloads the
  release with `gh` and verifies its build attestation with `gh
  attestation verify` (gh 2.49 or later). The repository is public, so
  any GitHub login works.
- Show the user the README's
  [Terms of use and risk](https://github.com/HaiNNT/c-hottag/blob/main/README.md#terms-of-use-and-risk)
  and the FAQ's [Is this allowed?](faq.md#is-this-allowed) answer. Proceed
  only once the user has read them and tells you to go ahead.

## 1. Install, without a clone

Run this from any directory that is not a clone of this repo:

```sh
gh api -H 'Accept: application/vnd.github.raw' repos/HaiNNT/c-hottag/contents/install.sh | sh
```

Add `-s -- --repo OWNER/NAME` only to install from a fork:

```sh
gh api -H 'Accept: application/vnd.github.raw' repos/OWNER/NAME/contents/install.sh | sh -s -- --repo OWNER/NAME
```

`install.sh` never prompts and never uses `sudo`. It checks the release
against its checksums and its build attestation, installs the binary under
`~/.chottag/versions/` (or `$CHOTTAG_HOME`), and runs `chottag setup` at
the end. Install the latest release: don't pass `--version`. Releases of
`HaiNNT/c-hottag` before v0.4.7 were built while the repository was
private, carry no attestation, and `install.sh` refuses them. Exit `2`
means a bad argument; exit `1` means the install itself failed, and the
last line printed says why. Tell the user what it said if it fails.

## 2. Install the plugin

```sh
claude plugin marketplace add HaiNNT/c-hottag
claude plugin install chottag@c-hottag
```

(Inside a Claude Code session, the equivalent is `/plugin marketplace add
HaiNNT/c-hottag` then `/plugin install chottag@c-hottag`.) The skill this
installs then runs as `/chottag:chottag`. If `marketplace add` fails to
clone, check `git ls-remote https://github.com/HaiNNT/c-hottag` works in
the user's terminal.

## 3. Until the user opens a new shell

The install added a PATH block to the user's shell rc, but it only takes
effect in a shell started after this step. Until then, run chottag by its
full path: `~/.chottag/bin/chottag` (or `$CHOTTAG_HOME/bin/chottag`, if
the user set that variable before installing). The commands below write
`chottag` for short.

## 4. Check it started

```sh
chottag status --json
chottag doctor --json
```

`status` must come back with `"ok": true` and `daemon.running` true. On a
first install, `doctor` still reports two checks, and both are expected:
`roles` ("no accounts are registered", fixed in step 6) and `path` (fixed
by the new shell in step 8). Any other failing check is a real problem:
stop, tell the user which check failed and the fix its row names.

An **account** is a Claude login chottag keeps in its own slot,
`~/.chottag/accounts/<name>` (or under `$CHOTTAG_HOME`); the user's normal
Claude Code login ("Home", `~/.claude` and `~/.claude.json`) is never
touched by any of this. If the user had chottag installed before, `chottag
setup` may already have re-registered old accounts (an `adopted NAME
(email)` line in the install output, or entries under `accounts` in
`status`): don't log those in again.

## 5. Agree the setup with the user

Ask these in one message, each with its default, then start. Don't ask
about anything else: every other setting has a working default the user
can change later through the skill.

1. **Which Claude accounts, and a short name for each** (for example
   `work` and `personal`)? Is any of them a company Team or Enterprise
   account? If so, it may be added only if that organization allows
   chottag.
2. **Which account is pinned?** The pinned account (chottag calls it
   `remote`) owns the user's Remote Control sessions, connectors and
   artifacts, so they stay put when chottag switches the account that pays
   for prompts. Default: the account the user already uses those features
   with, or else the first one.
3. **Should the pinned account also pay for prompts?** Default: yes. Say
   no to keep it for claude.ai features only (for example a work account
   whose usage the user wants to leave alone).

Tell the user, without asking, that auto-switch is on by default: when the
serving account nears its limit, chottag moves to the next one.

## 6. Log the accounts in

Log the pinned account in first, then the rest. For each one, tell the
user a browser window is about to open, then run this through Bash with
`timeout: 600000` (the ten minute maximum: it blocks until the user
finishes in the browser):

```sh
chottag login <name> --json
```

Then set the pinned account explicitly, and take it out of rotation if the
user answered no to question 3:

```sh
chottag remote <pinned> --json
chottag rotate <pinned> off --json     # only if it must not pay for prompts
```

With the pinned account out of rotation, log in at least one other account
and make it the serving one: `chottag tag <name> --json`.

## 7. Verify

```sh
chottag status --json
chottag doctor --json
```

`status` must list every account the user named under `accounts`, with
`remote` the pinned one. `doctor` exits `0` when nothing is wrong; `path`
may still fail until step 8. Any other problem is `"ok": false`, exit
`3`, `error.code` `doctor_problems`, with the failing checks under
`error.checks`, each row naming its own fix.

## 8. Tell the user to start fresh

Only a Claude Code session started after chottag was installed is routed
through it: the shell's PATH did not carry the chottag `claude` shim
before that. Tell the user to open a new terminal and start a new Claude
Code session there, then check with `chottag status` (or ask the new
session to run the chottag skill) that it says `serving:` and `remote:`
as expected.

## Managing chottag afterwards

Once installed, drive chottag through its plugin skill
(`/chottag:chottag`, or model-invoked when the user asks about accounts,
limits or switching) rather than repeating the steps above. Always pass
`--json` and branch on `ok`, then on `error.code` — never on
`error.message`, which is prose for humans. Relay every `warnings[]`
entry.

The commands used day to day:

```sh
chottag status --json                    # who is serving, who is remote, usage and limits
chottag tag <name> --json                # switch to a named account
chottag next --json                      # switch to the next account that is not limited
chottag remote <name> --json             # set who owns new remote-control sessions, artifacts, routines
chottag auto --json                      # auto-switch settings and last decision
chottag doctor --json                    # check the install
chottag update --check --json            # is a newer release available
chottag update --json                    # install it, only once the user agrees
```

Every command, flag, exit code and JSON field: [Command reference](commands.md).

## Rules for the agent

- Never run `/login` or `/logout` inside a Claude Code session: they log
  Home in or out, not a chottag account.
- Never edit `~/.claude`, `~/.claude.json`, or anything under them.
- Never run `chottag uninstall --purge` for the user: it deletes every
  account's login and needs a typed confirmation in a terminal chottag
  controls. Tell the user the command instead.
- Ask the user before `chottag doctor --fix`, `chottag update`, `chottag
  logout`, `chottag rename` or `chottag uninstall`.
- Do not add a company Team or Enterprise account unless that
  organization allows chottag; ask first if you are unsure.
- Never switch to a different account to get around a hold, a suspension
  or a ban on another one.

## If you are working inside a clone of this repo

This repo's own `CLAUDE.md` and `.claude/` are for developing chottag
itself, not for installing it for a user: its hook blocks `install.sh`
and most `chottag` commands from running against a real home from inside
a clone. To install or manage chottag for a user, leave the clone and
follow this page from the user's normal working directory instead.
