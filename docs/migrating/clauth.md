# Moving from clauth

Last verified: 2026-09-28.

See the [full comparison](../comparison.md) for other ways to run several
Claude Code accounts.

clauth is a maintained Claude-subscription switcher with auto-switch along a
fallback chain and a headless daemon — the same job chottag does. Its
default switch swaps a per-profile snapshot of Claude Code's credentials
file and settings into Claude Code's live files; chottag instead keeps each
login in its own slot and picks the bearer per request in a local proxy, so
Claude Code's live files are never rewritten.

## Concepts

| clauth | chottag |
|---|---|
| `clauth capture <name>` / `clauth login <profile>` | `chottag login <name>` (a fresh browser login; chottag never copies a token) |
| `clauth <profile>` | `chottag tag <name>` |
| a fallback chain, `clauth daemon` | the daemon's auto-switch, on by default: `chottag auto` |
| `clauth list` / `clauth which` | `chottag status` |
| `clauth start <profile>` (a per-process account) | none: the serving account applies to every chottag session, not to one launched process |
| an API-endpoint profile | none: chottag switches Claude subscription logins only |

## Before you start

Install chottag ([getting started](../getting-started.md)). clauth's
`~/.clauth/` directory is untouched by this move.

## Move your accounts

For each account, run `chottag login <name>`: a fresh browser login into a
new slot under `~/.chottag/accounts/<name>`. chottag has no command that
imports clauth's captured snapshot, because a login is tied to its config
directory: on macOS, Claude Code keys the Keychain entry to the config
directory's full path, so a snapshot copied into a new path would not carry
its login there. `chottag adopt` only registers a slot that is already
under `~/.chottag/accounts/` and already holds a login — for example, one
you logged into by hand with `CLAUDE_CONFIG_DIR=~/.chottag/accounts/<name>
claude auth login` (or `$CHOTTAG_HOME/accounts/<name>` if you set
`CHOTTAG_HOME`). `chottag setup` runs `adopt` as its last step, so a slot
set up that way is picked up automatically; it does not read `~/.clauth/`.

Once every account is logged into chottag, stop `clauth daemon` (and any
running fallback-chain auto-switch). chottag never writes `~/.claude`, but a
chottag session reads Home's settings as they stand, so if clauth's daemon
rewrites the `env` block of `~/.claude/settings.json` on its own schedule —
for example, switching to an API-endpoint profile — that changes what those
sessions run with. Then use `chottag tag <name>` to pick who serves the next
request, `chottag remote <name>` to pick who owns claude.ai objects, and
`chottag status` to see both.

## Undo

chottag leaves `~/.clauth/` untouched. To remove chottag itself, run
`chottag daemon stop && chottag uninstall` ([uninstall](../uninstall.md)).
To remove clauth (a script install), delete the binary it names, per its
wiki; `~/.clauth/` stays and is not deleted for you. `CHOTTAG_BYPASS=1 claude` runs Claude Code
untouched by chottag for one command, meanwhile.

## Sources

- clauth: [uwuclxdy/clauth](https://github.com/uwuclxdy/clauth) (accessed 2026-09-28)
- clauth install wiki: [Install](https://github.com/uwuclxdy/clauth/wiki/Install) (accessed 2026-09-28)
- Anthropic docs: [Authentication](https://code.claude.com/docs/en/authentication) (accessed 2026-09-28)
