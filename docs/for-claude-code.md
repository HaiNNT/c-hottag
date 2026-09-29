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
- Run `gh auth status`. It must show a login with access to
  `HaiNNT/c-hottag`. If the repository is private and the user is not yet
  a collaborator, stop and tell them so.
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
against its checksums (and, from v0.4.0, its build attestation), installs
the binary under `~/.chottag/versions/` (or `$CHOTTAG_HOME`), and runs
`chottag setup` at the end. While the repository is private its releases
carry no attestation: `install.sh` prints a note that it verified by
checksum only, which is expected, not a failure. Exit `2` means a bad argument; exit `1` means
the install itself failed, and the last line printed says why. Tell the
user what it said if it fails.

## 2. Install the plugin

`claude plugin marketplace add` clones this repo with the user's git
credentials, which a bare `GITHUB_TOKEN` does not supply. Check first:

```sh
git ls-remote https://github.com/HaiNNT/c-hottag
```

If that fails, run `gh auth setup-git`, then retry. Once it works:

```sh
claude plugin marketplace add HaiNNT/c-hottag
claude plugin install chottag@c-hottag
```

(Inside a Claude Code session, the equivalent is `/plugin marketplace add
HaiNNT/c-hottag` then `/plugin install chottag@c-hottag`.) The skill this
installs then runs as `/chottag:chottag`.

## 3. Until the user opens a new shell

The install added a PATH block to the user's shell rc, but it only takes
effect in a shell started after this step. Until then, run chottag by its
full path: `~/.chottag/bin/chottag` (or `$CHOTTAG_HOME/bin/chottag`, if
the user set that variable before installing).

## 4. Log the user's accounts in

An **account** is a Claude login chottag keeps in its own slot,
`~/.chottag/accounts/<name>` (or under `$CHOTTAG_HOME`); the user's normal
Claude Code login ("Home", `~/.claude` and `~/.claude.json`) is never
touched by any of this.

Before logging an account in, check whether `chottag setup` already
registered it: look at the install's own output for a line like `adopted
NAME (email)`, or run `chottag status --json` and see whether the account
already appears in `accounts`. `chottag setup` runs `adopt` at the end,
which registers any slot that already holds a login (from a previous
install) without a fresh browser round trip; on a first-ever install there
is nothing to adopt yet, and that is expected, not an error.

For every account the user still needs to add, ask for the name they want
to give it, then run this through Bash with `timeout: 600000` (the ten
minute maximum — it opens a browser and blocks until the user finishes
there):

```sh
chottag login <name> --json
```

Tell the user a browser window is about to open before you run it.

## 5. Verify

```sh
chottag status --json
chottag doctor --json
```

`status` must come back with `"ok": true` and every account the user
expects under `accounts`. `doctor` exits `0` when nothing is wrong; a
remaining problem is `"ok": false`, exit `3`, `error.code`
`doctor_problems`, with the failing checks under `error.checks` — each row
names its own fix.

## 6. Tell the user to start fresh

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
