# Moving from claude-swap

Last verified: 2026-09-28.

See the [full comparison](../comparison.md) for other ways to run several
Claude Code accounts.

claude-swap (`cswap`) is a Claude-subscription switcher for Claude Code,
with auto-switch near a plan's limit — the same job chottag does; its
latest release, v0.26.0, shipped 2026-09-02. It backs up the login Claude
Code currently holds into its own store,
then writes the chosen account's login back into Claude Code's live
credential location on each switch, holding Claude Code's own credential
locks while it writes. chottag instead keeps each login in its own slot and
picks the bearer per request in a local proxy, so Claude Code's live
credential is never rewritten.

## Concepts

| claude-swap | chottag |
|---|---|
| `cswap add` (captures the current login) | `chottag login <name>` (a fresh browser login per account; chottag never copies a token) |
| `cswap switch N`, or bare `cswap switch` to rotate | `chottag tag <name>`, or bare `chottag tag` (or `chottag next`) to rotate |
| `cswap auto --threshold N --strategy best\|consume-first` | the daemon's auto-switch, on by default: `chottag auto`, `chottag auto mode balanced`, `chottag auto mode cache-optimize`, `chottag auto set KEY VALUE` |
| `cswap list`, `cswap status` | `chottag status` (its first line is `serving: X   remote: Y`) |
| `cswap disable N` / `cswap enable N` | `chottag rotate <name> off` / `chottag rotate <name> on` |
| `cswap alias N dev` | a name is chosen at `chottag login`; rename later with `chottag rename <old> <new>` |
| `cswap remove N` | `chottag logout <name>` |
| `cswap run N` (an account for this one terminal only) | no per-terminal account: the serving account applies to every chottag session |
| `cswap pin N` (the cswap-pin extra) | `chottag remote <name>`; to move one existing object, `chottag own KIND ID <name>` |

### cswap-pin

[cswap-pin](https://github.com/codeslake/cswap-pin) is a companion to
claude-swap: a local MITM proxy that keeps Remote Control sessions and
artifacts on one pinned account while `cswap switch` moves who serves
inference. That is the same idea as chottag's built-in owner account
(`chottag remote`), which needs no separate extra or proxy of its own.

## Before you start

Install chottag ([getting started](../getting-started.md)). Nothing below
touches claude-swap's data until you choose, at the end, to remove
claude-swap.

## Move your accounts

Log every account into chottag first, before removing claude-swap: run
`chottag login <name>` once per account, which opens a fresh browser login
into its own slot under `~/.chottag/accounts/<name>`. chottag has no command
that imports claude-swap's backed-up login, because a login is tied to its
config directory: on macOS, Claude Code keys the Keychain entry to the
config directory's full path, so a login copied into a new path would not
work there. `chottag adopt` only registers a slot that is already under
`~/.chottag/accounts/` and already holds a login — for example, one you
logged into by hand with `CLAUDE_CONFIG_DIR=~/.chottag/accounts/<name>
claude auth login` (or `$CHOTTAG_HOME/accounts/<name>` if you set
`CHOTTAG_HOME`). `chottag setup` runs `adopt` as its last step, so a slot set
up that way is picked up automatically; it does not read claude-swap's
store.

claude-swap's own README warns against running `/logout` before adding an
account with `cswap add`, since Claude Code can revoke the refresh token for
the account you leave. The same risk applies while both tools are
installed: don't run `/logout` in a claude-swap-managed session until every
account you still want is logged into chottag too.

Once every account is logged into chottag, use `chottag tag <name>` to pick
who serves the next request, `chottag remote <name>` to pick who owns
claude.ai objects, and `chottag status` to see both.

Removing claude-swap is a last, optional step, once you are sure you no
longer need it: `cswap purge` is destructive — it deletes all of
claude-swap's data, including its saved logins (its README calls this
"Remove all data") — followed by `uv tool uninstall claude-swap` or
`pipx uninstall claude-swap`.

## Undo

To undo, run `chottag daemon stop && chottag uninstall`
([uninstall](../uninstall.md)) first, so plain `claude` is Claude Code's
own binary again, before you log any account back into Claude Code
natively. If you have not run `cswap purge`,
claude-swap's backed-up logins are still there, and `cswap switch` goes back
to using them as before. If you did run it, claude-swap's saved logins are
gone too; undo means reinstalling claude-swap, logging Claude Code into
each account again, and running `cswap add` per account to recreate its
backups. `CHOTTAG_BYPASS=1 claude` runs Claude Code untouched by chottag for
one command, if you would rather not uninstall chottag yet.

## Sources

- claude-swap: [realiti4/claude-swap](https://github.com/realiti4/claude-swap) (accessed 2026-09-28)
- cswap-pin: [codeslake/cswap-pin](https://github.com/codeslake/cswap-pin) (accessed 2026-09-28)
- Anthropic docs: [Authentication](https://code.claude.com/docs/en/authentication) (accessed 2026-09-28)
