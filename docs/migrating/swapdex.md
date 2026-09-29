# Moving from swapdex

Last verified: 2026-09-28.

See the [full comparison](../comparison.md) for other ways to run several
Claude Code accounts.

swapdex is the closest design to chottag: a permanent config-dir slot per
account, a `claude` shim, a local proxy that picks the serving account per
request, an `adopt` command, and auto-switch. It supports several CLIs
besides Claude Code; its latest release, v0.167.3, shipped 2026-09-27.

## Concepts

| swapdex | chottag |
|---|---|
| a slot | a slot |
| `swapdex run <name> --tool claude -- auth login` | `chottag login <name>` |
| `swapdex adopt <name> <dir> --tool claude` | `chottag adopt`, for a directory already under `~/.chottag/accounts/`; otherwise `chottag login <name>` |
| `swapdex shim` | `chottag setup` (installs the `claude` shim and one PATH block) |
| `swapdex serve <name>` | `chottag tag <name>` |
| `swapdex use <name>` (where sessions live) | none: chottag sessions keep the `CLAUDE_CONFIG_DIR` your shell already sets (default `~/.claude`) |
| `swapdex auto on\|off`, `strategy roomiest\|consume-first`, `threshold` | `chottag auto on`, `chottag auto off`, `chottag auto mode balanced`, `chottag auto mode cache-optimize`, `chottag auto set KEY VALUE` |
| `swapdex pause <name>` / `swapdex resume <name>` | `chottag rotate <name> off` / `chottag rotate <name> on` |
| `swapdex ls`, `swapdex quota` | `chottag status` |
| `swapdex doctor` | `chottag doctor` |
| `swapdex rm <name>` | `chottag logout <name>` |
| (not documented) | `chottag remote <name>`, `chottag own KIND ID <name>` |

## Before you start

Install chottag ([getting started](../getting-started.md)). swapdex's slots
and its data directory are untouched by this move.

Both tools install a shim named `claude`. Before running `chottag setup`,
remove swapdex's shim from `PATH` (delete or rename the `claude` file
swapdex installed, or drop that directory from `PATH`), and separately stop
its background service with `swapdex service uninstall`. Putting chottag's
shim first on `PATH` instead is not safe: chottag's shim resolves the real
`claude` by walking `PATH` itself, so with swapdex's shim still installed
further down `PATH` it would find and run swapdex's shim as if it were the
real Claude Code, chaining the two proxies together.

## Move your accounts

For each account, run `chottag login <name>`: swapdex's own quick start
signs each new slot in natively too, rather than importing an existing
login, so this matches how you already added accounts to swapdex. `chottag
adopt` only registers a slot that is already under `~/.chottag/accounts/`
and already holds a login — for example, one you logged into by hand with
`CLAUDE_CONFIG_DIR=~/.chottag/accounts/<name> claude auth login` (or
`$CHOTTAG_HOME/accounts/<name>` if you set `CHOTTAG_HOME`). `chottag setup`
runs `adopt` as its last step, so a slot set up that way is picked up
automatically. That is the same job as `swapdex adopt` for a directory
already in place, but it does not read swapdex's own slots, and there is no
command that moves a login between the two tools' directories. On macOS,
Claude Code keys a login to its config directory's full path in the
Keychain, so a directory moved between the two tools would lose its login
regardless of which tool moves it.

Once every account is logged into chottag, use `chottag tag <name>` to pick
who serves the next request, `chottag remote <name>` to pick who owns
claude.ai objects, and `chottag status` to see both.

## Undo

chottag leaves swapdex's slots and data directory alone. To remove chottag
itself, run `chottag daemon stop && chottag uninstall`
([uninstall](../uninstall.md)), then run `swapdex shim` again to put
swapdex's shim back on `PATH`.
`CHOTTAG_BYPASS=1 claude` runs Claude Code untouched by chottag for one
command, meanwhile.

## Sources

- swapdex: [youdie006/swapdex](https://github.com/youdie006/swapdex) (accessed 2026-09-28)
- swapdex command reference: [docs/COMMANDS.md](https://github.com/youdie006/swapdex/blob/main/docs/COMMANDS.md) (accessed 2026-09-28)
- Anthropic docs: [Authentication](https://code.claude.com/docs/en/authentication) (accessed 2026-09-28)
