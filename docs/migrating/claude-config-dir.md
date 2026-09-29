# Moving from a manual `CLAUDE_CONFIG_DIR` alias

Last verified: 2026-09-28.

See the [full comparison](../comparison.md) for other ways to run several
Claude Code accounts.

Anthropic documents `CLAUDE_CONFIG_DIR` and the pattern of a shell alias per
account directly: "Useful for running multiple accounts side by side: for
example, `alias claude-work='CLAUDE_CONFIG_DIR=~/.claude-work claude'`."
chottag's account slots use this same mechanism, plus a proxy that can
switch which account serves a request mid-session, an owner account for
claude.ai objects, and auto-switch near a plan's limit.

## Concepts

| Manual `CLAUDE_CONFIG_DIR` | chottag |
|---|---|
| a dir such as `~/.claude-work` | a slot, `~/.chottag/accounts/<name>` |
| `CLAUDE_CONFIG_DIR=~/.claude-work claude auth login` | `chottag login <name>` (runs Claude Code's own login, with `CLAUDE_CONFIG_DIR` set to the slot) |
| `alias claude-work=...`, one terminal per account | plain `claude` through the shim; `chottag tag <name>` picks the serving account, even mid-session |
| restart, or another terminal, to change account | `chottag tag` or `chottag next` apply from the next request |
| (nothing) | `chottag auto` (auto-switch, on by default); `chottag remote` and `chottag own` (who owns a claude.ai object) |

## Before you start

Install chottag ([getting started](../getting-started.md)). Your existing
`~/.claude-work`-style directories and aliases are untouched; nothing about
this move deletes or edits them.

Once chottag's shim is installed, though, `claude` on `PATH` is chottag's
shim, so an old alias such as `alias claude-work='CLAUDE_CONFIG_DIR=~/.claude-work
claude'` still runs through it: the session keeps `~/.claude-work`'s own
settings and history, but chottag's proxy swaps every request's bearer to
whichever account is currently serving, which may not be the login
`~/.claude-work` itself holds. Run an alias with `CHOTTAG_BYPASS=1` in front
of it to get the old, unswapped behaviour for one command, or retire the
alias once you have moved that account into a chottag slot.

## Move your accounts

For each account, run `chottag login <name>`: a fresh browser login into a
new slot under `~/.chottag/accounts/<name>`. chottag never copies a token
out of an existing directory, and there is no command that would import one.
That is not an oversight: on macOS, Claude Code keeps a login in the
Keychain under an entry keyed to the config directory's full path, so a
directory moved to a new path loses its login regardless of which tool
moves it. Don't copy `.credentials.json` or any other credential file
between directories; log in again instead.

`chottag adopt` only registers a slot that is already under
`~/.chottag/accounts/` and already holds a login — for example, one you
logged into by hand with `CLAUDE_CONFIG_DIR=~/.chottag/accounts/<name>
claude auth login` (or `$CHOTTAG_HOME/accounts/<name>` if you set
`CHOTTAG_HOME`). `chottag setup` runs `adopt` as its last step, so a slot set
up that way is picked up automatically. It cannot adopt one of your old
`~/.claude-work` directories in place; log into a chottag slot instead.

Once every account is logged in, use `chottag tag <name>` to choose which
account serves the next request, `chottag remote <name>` to choose which
account owns claude.ai objects, and `chottag status` to see both at a
glance.

## Undo

chottag never edits your old `~/.claude-work`-style directories or aliases,
so going back needs nothing on that side. To remove chottag itself, run
`chottag daemon stop && chottag uninstall` ([uninstall](../uninstall.md));
your aliases and directories still work exactly as before.
`CHOTTAG_BYPASS=1 claude` runs Claude Code untouched by chottag for one
command, meanwhile.

## Sources

- Anthropic docs: [Environment variables](https://code.claude.com/docs/en/env-vars) (accessed 2026-09-28)
- Anthropic docs: [Authentication](https://code.claude.com/docs/en/authentication) (accessed 2026-09-28)
