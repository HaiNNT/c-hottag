# Command reference

Every `chottag` command, its flags and its `--json` output. `chottag help`
prints the command list; `chottag <command> --help` prints one command's
own lines (a command group like `daemon` or `trace` prints every one of its
verbs). An account is named by its display name, its email, or any prefix
of the name that matches exactly one account. Every path chottag reads or
writes is under `~/.chottag`, or under `$CHOTTAG_HOME` when that is set.
See [configuration](configuration.md) for the files under that tree, and
[auto-switch](auto-switch.md) for `chottag auto`'s settings.

## Global flags and help

| Flag | Meaning |
|---|---|
| `--json` | print one JSON document on stdout instead of the human text; accepted by every command except the ones below |
| `-h`, `--help` | print the command's usage and exit 0, without running it |

`chottag help` (also bare `chottag`, `chottag -h` and `chottag --help`)
prints the full command list.

These commands refuse `--json`, because they stream or run forever: `daemon
run`, `daemon logs`, `proxy run`, `trace run`, `trace env`, `trace mark`,
`trace summarize`, and `help` itself. Each fails with exit 2 and the error
code `json_unsupported` (`cli.go`'s `refuseJSON`).

## JSON output

Under `--json`, chottag writes exactly one JSON document to stdout and
nothing else there; every other line goes to stderr. Branch on `ok`, then
on `error.code` — never parse `error.message`, which is for humans and can
change. `warnings` is always an array, present even when empty, of
`{code, message}` objects: something worth knowing beyond the result. A
failing command's `error` object carries `code`, `message` and `exit`
(chottag's own process exit code), plus command-specific details, such as
`error.skipped` for `no_candidate` or `error.checks` and `error.problems`
for `doctor_problems`.

A success document:

```json
{
  "version": 1,
  "ok": true,
  "warnings": [
    {"code": "limited", "message": "B is limited until Sep 28 14:05"}
  ]
}
```

An error document:

```json
{
  "version": 1,
  "ok": false,
  "warnings": [],
  "error": {"code": "unknown_account", "message": "no account matches \"Z\"", "exit": 1}
}
```

## Exit codes

| Exit | Meaning |
|---|---|
| `0` | success |
| `1` | the command failed |
| `2` | a usage error: fix the command line |
| `3` | nothing failed, but you must decide something (confirm, pick an account, fix a doctor problem) |

## Install and update

### `chottag setup`

```sh
chottag setup [--claude PATH] [--label NAME] [--name DIR=NAME]...
```

Installs the shim: the `~/.chottag` tree, the local CA, the `bin/chottag`
and `bin/claude` symlinks, and a PATH line in your shell's rc file. It then
runs `chottag adopt` with the same arguments, so any slot that already
holds a login is registered without a fresh `claude auth login`.

Before `setup` changes a shell rc file that already exists, it copies it to
`~/.chottag/backups/` (named without the dot, e.g. `zshrc.20261002T150405Z`;
for a symlinked rc, the file it points to) and reports the path as
`rcBackup`. A new rc file, or one that needs no change, makes no backup, and
a failed backup stops `setup` before the rc is touched. The newest 5 backups
of each name are kept.

| Flag | Meaning |
|---|---|
| `--claude PATH` | path to the real `claude` binary, passed through to `adopt` (default: the real `claude` on PATH, never chottag's own shim) |
| `--label NAME` | label this install (1-16 characters of `a-z`, `0-9` and `-`; `""` clears it). It is stored as `label` in `state.json`, shows in `chottag status`, and turns notification titles into `chottag · NAME: ...`. A bare `setup` keeps the label. `scripts/dev-env` sets `dev` |
| `--name DIR=NAME` | register the slot dir `DIR` under the account name `NAME` (repeatable), passed through to `adopt` |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "installed": "/Users/alice/.chottag",
  "rcUpdated": true,
  "rcPath": "/Users/alice/.zshrc",
  "rcBackup": "/Users/alice/.chottag/backups/zshrc.20261002T150405Z",
  "adopt": {
    "adopted": [{"dir": "a", "name": "work"}],
    "updated": [{"dir": "b", "name": "personal"}],
    "skipped": [{"dir": "c", "reason": "no login"}]
  }
}
```

`adopt` is present, with empty arrays when there was nothing to adopt, and
absent when the inner adopt failed for another reason — that shows up
instead as an `adopt_failed` warning.

### `chottag adopt`

```sh
chottag adopt [--claude PATH] [--name DIR=NAME]...
```

Registers the account slot directories that already hold a login, so slots
created before chottag had a CLI are usable without logging in again.
chottag cannot import another tool's login into a slot (macOS ties a login
to its config directory path); this command only ever notices logins
already sitting in a slot chottag manages.

| Flag | Meaning |
|---|---|
| `--claude PATH` | path to the real `claude` binary, used to read each slot's login (default: the real `claude` on PATH, never chottag's own shim) |
| `--name DIR=NAME` | register the slot dir `DIR` under the account name `NAME` (repeatable) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "adopted": [{"dir": "a", "name": "work"}],
  "updated": [{"dir": "b", "name": "personal"}],
  "skipped": [{"dir": "c", "reason": "no login"}]
}
```

`adopt` refuses one shape of identity change, the one a slot damaged before
0.8.4 shows: the slot's email would change to the email of an account that is
serving (in any pool), and the stored organisation does not change (a blank
stored organisation, or a slot that reports none, counts as unchanged). Then the stored email and
organisation stay as they are, the account is not stamped as logged in, the
entry goes under `skipped` with the reason `identity_suspect`, and an
`identity_suspect` warning says to run `chottag login NAME` to repair it.
Everything else is adopted as usual: an unchanged identity that shares an email
(one login in several organisations), an email that moves together with the
organisation, an email that moves to one no serving account has (so an account
heals back to its true email even when a non-serving account shares it), a
first fill of a blank stored email, and a new slot. If the wrong email was
already recorded, `adopt` sees no change and does not warn: look at
`chottag status` and doctor's `identities` row (see
[troubleshooting](troubleshooting.md#an-account-shows-another-accounts-email)).

### `chottag update`

```sh
chottag update [--check] [--version V] [--repo OWNER/NAME] [--restart | --no-restart]
chottag update [--auto-check on|off] [--auto-install on|off] [--auto-restart on|off]
```

Installs the latest release from the repo chottag was installed from.
`--check` only reports whether a newer release exists; it installs
nothing, but, like the daemon's check, it posts the one desktop notice for a
newer version it is the first to find (see [Updating](updating.md#the-update-check)). It asks GitHub's API directly (one request, no `gh`, no token) and
also records the answer in `status.json`'s `update`, the same cache the
daemon's update check fills, only for the install's own repo and not for a
`--repo` that names another (see [Updating](updating.md#the-update-check)).

The whole install, from the download through `setup`, holds the update lock,
`run/update.lock`. A second `chottag update` at the same time exits `1` with
`update_in_progress` ("another chottag update is running"); nothing is
downloaded.

`--auto-check`, `--auto-install` and `--auto-restart` set a switch in
`state.json` and exit: they check and install nothing, and cannot be
combined with `--check`, `--version`, `--repo`, `--restart` or `--no-restart`
(exit `2`). They print `update check: on; auto-install: off; auto-restart:
on`, and with `--json`
`{"updates": {"check": true, "auto": false, "restart": true}}`. `--auto-install on` also turns
the check on; `--auto-check off` also turns auto-install off, since
auto-install needs the check, so `--auto-check off --auto-install on` is a
usage error (exit `2`). `--auto-restart on|off` is independent of the other
two: it lets the daemon restart itself, once idle, onto a newer chottag that
is installed but not yet running (on by default; see
[Updating](updating.md#restarting-onto-an-installed-update)).

Before anything is extracted or the new binary's `setup` runs (and after the
download is verified), `update` copies `state.json` to
`backups/state.json.<UTC stamp>` under `$CHOTTAG_HOME` (directory mode 0700,
file 0600; the newest 5 are kept) and prints a `backed up state.json to ...`
line. If that copy fails, nothing is installed and `update` fails with
`update_failed`; if only removing an old backup fails, it warns
`prune_failed`. `--check` and an already-up-to-date run make no backup.
`backups` in `--json` lists the `state.json` backup only; the rc backup that
the child `setup` makes is reported on stderr. This starts with updates run
by 0.6.0 or later: before the first update to 0.6.0, copy `state.json` by
hand (`cp ~/.chottag/state.json ~/.chottag/state.json.pre-v0.6.0`).

To undo an update: stop the daemon (`chottag daemon stop`), run `chottag
update --version vX.Y.Z` to put the older binary back, copy back the
`state.json` backup that the update being undone printed (it is also listed
in its `backups`; not the newest file, which the rollback itself just made),
then start the daemon (`chottag daemon start`).

| Flag | Meaning |
|---|---|
| `--check` | report whether a newer release exists; install nothing |
| `--version V` | install this release tag instead of the latest, e.g. to roll back. Below 0.8.0 it is refused (`pools_block_rollback`) while pools beyond `default` exist |
| `--repo OWNER/NAME` | use this repo instead of `install.json`'s or the default |
| `--restart` | restart the daemon even if a claude session is running |
| `--no-restart` | install, but leave the daemon running (`daemon` is `not-restarted`); the daemon uses the new version after `chottag daemon restart`, or restarts itself when idle (unless `--auto-restart off`; daemons from 0.6.0 on). Not with `--restart` |
| `--auto-check on\|off` | turn the daemon's update check on or off; sets the switch and exits |
| `--auto-install on\|off` | let the daemon install a new release by itself (opt-in, see [Updating](updating.md#automatic-install)); sets the switch and exits |
| `--auto-restart on\|off` | let the daemon restart itself, when idle, onto an installed newer version (on by default, see [Updating](updating.md#restarting-onto-an-installed-update)); sets the switch and exits |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "repo": "HaiNNT/c-hottag",
  "current": "v0.3.0",
  "latest": "v0.4.0",
  "updateAvailable": true,
  "installed": true,
  "pruned": ["v0.2.0"],
  "backups": ["/Users/alice/.chottag/backups/state.json.20261002T150405Z"],
  "daemon": "deferred",
  "liveSessions": 2,
  "selfRestart": false
}
```

`daemon` says what happened to the daemon after an install: `restarted`,
`restart-failed`, `deferred` (a session is running: the daemon restarts
itself once idle, unless `--auto-restart off`, or run `chottag daemon restart`
yourself), `not-running`, `not-probed` (the daemon could not be
asked), or `not-restarted` (`--no-restart`). It is left out when nothing was
installed. With `deferred`, `selfRestart` says whether the running daemon
restarts itself onto the new version when idle; `false` (a daemon from before
0.6.0, auto-restart off, or a daemon newer than what was installed) means the
update finishes only with `chottag daemon restart`, and the text says so:
`the running daemon (0.5.0) can't restart itself: run chottag daemon restart
once when convenient`.

### `chottag uninstall`

```sh
chottag uninstall [--purge]
```

Removes the shim: the fenced PATH block in your shell's rc file, the
`bin/chottag` and `bin/claude` symlinks, and `ca/bundle.pem`. It leaves the
CA itself and every account's login in place, so a plain `uninstall` alone
can never destroy a login. `backups/` (copies of `state.json` and your rc
file, which can hold secrets you export in it) stays too, unless you use
`--purge`.

`--purge` additionally deletes the whole `$CHOTTAG_HOME` tree — every
login included — but only after you type the word `purge` at a prompt;
anything else aborts with no change. Because that prompt cannot be
scripted, `--purge` refuses `--json`: it fails with
`confirmation_required`, exit 3.

| Flag | Meaning |
|---|---|
| `--purge` | also delete every account slot's login (irreversible; requires typed confirmation; refuses `--json`) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "removed": ["bin/chottag", "bin/claude"],
  "purged": false
}
```

### `chottag version`

```sh
chottag version
```

Prints chottag's own version. It takes no flags.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "chottag": "v0.4.0"
}
```

## Accounts

### `chottag login`

```sh
chottag login NAME [--pool POOL] [--claude PATH]
```

Logs a slot in through the browser (`claude auth login`) and registers it
under `NAME`. Running it again for an existing account re-logs in that
same slot.

A new account joins `POOL` only, or `default` without `--pool`; never every
pool. An account that already exists keeps its pools: if `--pool` names a pool it
is not in, `login` warns `pool_not_changed` (`<name> is already registered;
--pool applies only to a new account. To add it to <pool>, run: chottag pool
join <name> <pool>`). An unknown pool is exit 2, `no_pool`, before the
browser opens. When the email is already registered under another name,
`login` warns `email_registered`. If there is a pool that account is not in
(`--pool`, else the first extra pool), it says `<email> is already account b;
to use it in another pool, run: chottag pool join b <pool>`, which shares the
one login instead of logging it in twice; otherwise it keeps its older text. See [`chottag pool`](#chottag-pool).

| Flag | Meaning |
|---|---|
| `--pool POOL` | the pool a new account joins (default: `default`) |
| `--claude PATH` | path to the real `claude` binary (default: the real `claude` on PATH, never chottag's own shim) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "account": "work",
  "email": "alice@example.com",
  "org": "Acme"
}
```

### `chottag logout`

```sh
chottag logout NAME [--claude PATH] [--force] [--yes]
```

Revokes a slot's login and removes the account. `--force` is required when
the account is currently serving or remote in any pool; it moves each such
role to another member of that pool first (or to none), then continues.
Its memberships in every pool go with it.

| Flag | Meaning |
|---|---|
| `--claude PATH` | path to the real `claude` binary (default: the real `claude` on PATH, never chottag's own shim) |
| `--force` | log out even if the account is serving or remote (moves the role first) |
| `--yes` | do not ask for confirmation |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "account": "work",
  "removed": true,
  "dir": "a",
  "movedServing": "personal",
  "movedRemote": "personal",
  "movedInPools": [
    {"pool": "work", "serving": "team", "remote": "team"}
  ]
}
```

`movedInPools` lists the roles handed off in pools other than `default`, and
is absent when there were none.

### `chottag rename`

```sh
chottag rename OLD NEW
```

Renames an account (its display name only): the slot directory and its
Keychain login stay exactly as they are. `rename` takes no flags; running
it again with the same arguments resumes an interrupted rename.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "from": "work",
  "to": "acme-work",
  "roles": ["serving"],
  "ownerEntries": 3,
  "resumed": false,
  "changed": true
}
```

### `chottag plan`

```sh
chottag plan NAME TIER [--units N]
```

Sets `NAME`'s plan tier for auto-switch: `pro`, `max5x`, `max20x` or
`team`. `--units` overrides the tier's own capacity units per 1%; without
it the tier's default applies (pro 1, max5x 5, max20x 20, team 5), so
re-running `plan` without `--units` also clears an earlier override.

| Flag | Meaning |
|---|---|
| `--units N` | capacity units per 1%, 1-1000 (default: the tier's own) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "account": "work",
  "plan": "max5x",
  "units": 5
}
```

### `chottag rotate`

```sh
chottag rotate NAME [on|off]
```

Includes or excludes `NAME` from `chottag next` and auto-switch's
rotation. With no verb it only prints the current setting. It takes no
flags: a stray flag is exit 2, never treated as `NAME` or `on`/`off`.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "account": "work",
  "rotate": true,
  "inRotation": 2
}
```

## Switching

### `chottag tag`

```sh
chottag tag [NAME] [--force] [--pool POOL]
chottag tag --unpin [--pool POOL]
```

Sets the serving account: the one whose login the proxy hands to Claude
Code. With no `NAME` it moves to the next account in rotation, the same
choice `chottag next` makes. `--force` switches even past a limit, a
switch point, or a needs-login state — but never past rotation (a
rotated-out account is never picked).

Under [`chottag policy spread`](#chottag-policy), `tag NAME` also pins new
sessions to `NAME` and prints a line saying so (`pin` in `--json`). If
`NAME` has rotation off the pin is stored but not used while its rotation is
off, and the line says that. A bare `tag` is refused under spread, as
`next` is (`spread_next`). `tag --unpin` clears the pin; it takes no `NAME`
and no `--force`, and under `serial` it is a usage error, because a pin has
no effect there. Under `serial`, `tag` never touches the pin.

With [pools](#chottag-pool), `tag NAME` acts in `NAME`'s pool when it is in
exactly one. When `NAME` is in several, `--pool POOL` is required
(`pool_ambiguous` without it), and `POOL` must be one of its pools
(`not_in_pool`). A bare `tag` and `tag --unpin` act in `--pool` (default:
`default`), and spread is judged per pool. An unknown pool is `no_pool`. A
result outside `default` says `serving: NAME (pool POOL)`.

| Flag | Meaning |
|---|---|
| `--force` | switch even past a limit, a switch point or a needs-login state (never past rotation) |
| `--unpin` | under spread, clear the pin (no `NAME`) |
| `--pool POOL` | the pool to act in (default: `NAME`'s pool, or `default`) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "serving": "work",
  "previous": "personal",
  "pin": "work",
  "pool": "work"
}
```

`pin` appears only under spread; `tag --unpin` answers with `serving` only.
`pool` is present only outside `default`.

### `chottag next`

```sh
chottag next [--force] [--pool POOL]
```

Moves the serving account to the next one in rotation, within the pool
(`--pool`, default `default`; unknown is `no_pool`). Under
[`chottag policy spread`](#chottag-policy) it is refused with exit 2 and code
`spread_next`: sessions sit on different accounts, so a machine-wide "next"
has no meaning (in that pool: another pool on `serial` still moves). Use
`chottag tag NAME` to pin new sessions, or `chottag policy serial` to go back
to one serving account.

| Flag | Meaning |
|---|---|
| `--force` | switch even past a limit, a switch point or a needs-login state (never past rotation) |
| `--pool POOL` | the pool whose serving account moves (default: `default`) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "serving": "work",
  "previous": "personal",
  "skipped": [
    {"name": "team", "reason": "limited", "until": "2026-09-28T14:05:00Z"}
  ],
  "fallback": false,
  "pool": "work"
}
```

`pool` is present only outside `default`.

### `chottag remote`

```sh
chottag remote [NAME] [--pool POOL]
```

Sets, or with no `NAME` shows, the account that owns claude.ai objects
(Remote Control sessions, environments, artifacts and connectors created
from here on) for a pool. Also `chottag rc` and `chottag remote-control`.
`remote NAME` acts in `NAME`'s pool when it is in exactly one; in several it
needs `--pool` (`pool_ambiguous`), and the pool must be one of its
(`not_in_pool`). A bare `remote` shows `--pool`'s remote (default: `default`;
unknown is `no_pool`). Its only flag is `--pool`; any other is exit 2.

| Flag | Meaning |
|---|---|
| `--pool POOL` | the pool to act in (default: `NAME`'s pool, or `default`) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "remote": "work",
  "changed": true,
  "pool": "work"
}
```

`pool` is present only outside `default`.

### `chottag own`

```sh
chottag own KIND ID [ACCOUNT]
```

Re-attributes one already-created claude.ai object to `ACCOUNT`, or with no
`ACCOUNT` prints its current owner. `KIND` is one of `session`,
`environment`, `artifact` or `connector`. It takes no flags.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "kind": "session",
  "id": "s_123",
  "account": "work",
  "reassigned": true
}
```

## Status and health

### `chottag status`

```sh
chottag status
```

Prints each account's usage and limit state: which is serving, which is
remote, each account's 5-hour and 7-day utilization, and whether the
daemon is currently passing requests through on Home's login instead of
the account you chose. Also `chottag ls`. It takes no flags. With more than
one pool it adds a `POOLS` column and a line per pool (see below).

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "accounts": [
    {
      "name": "work",
      "email": "alice@example.com",
      "org": "Acme",
      "usage": {
        "fiveHourPct": 42.5,
        "sevenDayPct": 10,
        "fiveHourResetsAt": "2026-09-28T19:00:00Z",
        "sevenDayResetsAt": "2026-10-03T00:00:00Z",
        "updatedAt": "2026-09-28T14:00:00Z",
        "source": "observed"
      },
      "limited": false,
      "limitedUntil": "2026-09-28T19:00:00Z",
      "window": "5h",
      "reason": "",
      "token": "ok",
      "tokenAt": "2026-09-01T00:00:00Z",
      "rotate": true,
      "passthrough": "",
      "plan": "max5x",
      "pools": ["default", "personal"],
      "shared": true,
      "stale": false
    }
  ],
  "serving": "work",
  "remote": "work",
  "label": "dev",
  "limits": {
    "allLimited": false,
    "nextReset": "2026-09-28T19:00:00Z",
    "nextResetAccount": "work"
  },
  "daemon": {
    "running": true,
    "port": 47821,
    "heartbeat": "2026-09-28T14:00:30Z",
    "routeDrift": 0,
    "zeroIdExtractions": 0,
    "ownerWriteDrops": 0,
    "notifyErrors": 0,
    "liveSessions": 1,
    "restartPending": "0.6.0",
    "version": "v0.4.0",
    "versionMismatch": false,
    "identity": "verified"
  },
  "trace": {
    "lastTraced": {"claudeVersion": "2.1.282", "at": "2026-09-20T00:00:00Z"}
  },
  "sessions": [
    {
      "pid": 4242,
      "started": "2026-09-28T13:50:00Z",
      "sid": "3f9c1a2b",
      "pool": "default",
      "account": "work",
      "lastSeen": "2026-09-28T14:00:10Z",
      "requests": 17,
      "conversations": 1
    }
  ],
  "auto": {
    "mode": "balanced",
    "decision": "holding work (5h 42%, resets in 5h)",
    "lastSwitch": {
      "from": "personal",
      "to": "work",
      "trigger": "threshold",
      "window": "5h",
      "pct": 90,
      "at": "2026-09-28T09:00:00Z",
      "retried": true
    },
    "userChosen": false,
    "burnRate": 12.5,
    "pools": {
      "personal": {
        "decision": "holding work (5h 42%, resets in 5h)",
        "lastSwitch": {
          "from": "dev",
          "to": "work",
          "trigger": "threshold",
          "window": "5h",
          "pct": 90,
          "at": "2026-09-28T10:00:00Z",
          "retried": false
        },
        "userChosen": false
      }
    }
  },
  "update": {
    "latest": "0.5.0",
    "publishedAt": "2026-09-30T09:00:00Z",
    "checkedAt": "2026-10-01T09:00:00Z",
    "available": true,
    "notified": "0.5.0",
    "error": "",
    "auto": {
      "version": "0.5.0",
      "at": "2026-10-01T09:00:00Z",
      "ok": false,
      "error": "refused"
    }
  },
  "updates": {
    "check": true,
    "auto": false,
    "restart": true
  },
  "policy": "spread",
  "pin": "work",
  "pools": [
    {
      "name": "default",
      "serving": "work",
      "remote": "work",
      "policy": "spread",
      "pin": "work",
      "accounts": ["work"],
      "liveSessions": 1,
      "decision": "holding work (5h 42%, resets in 5h)",
      "lastSwitch": {
        "from": "personal",
        "to": "work",
        "trigger": "threshold",
        "window": "5h",
        "pct": 90,
        "at": "2026-09-28T09:00:00Z",
        "retried": true
      }
    },
    {
      "name": "personal",
      "serving": "work",
      "remote": "dev",
      "policy": "spread",
      "pin": "dev",
      "accounts": ["dev", "work"],
      "liveSessions": 0,
      "decision": "holding work (5h 42%, resets in 5h)",
      "lastSwitch": {
        "from": "dev",
        "to": "work",
        "trigger": "threshold",
        "window": "5h",
        "pct": 90,
        "at": "2026-09-28T10:00:00Z",
        "retried": false
      }
    }
  ]
}
```

**Pools.** `pools`, an account's `pools` and `shared`, and `auto.pools` are
present only while the install has more than one pool (see [`chottag
pool`](#chottag-pool)); with only `default` the document is exactly what it
was. The top-level `serving`, `remote`, `policy`, `pin` and `auto` (its
`decision` and `lastSwitch`) are the `default` pool's, in place. `pools[]` has
one entry per pool, `default` first and the rest sorted: its `serving`,
`remote`, `policy`, `pin` (under spread), member `accounts`, `liveSessions`
(sessions placed in the pool; an unidentified session counts for `default`),
and the daemon's `decision` and `lastSwitch` for it. `auto.pools` is the same
two fields, and `userChosen`, for each pool other than `default`. An account's `pools`
lists the pools it is in, and `shared` is `true` when that is more than one:
they then share its usage limit. A session's `pool` (in `sessions`) says which
pool it runs in.

The text form adds a `POOLS` column after `NAME` (`personal,work`), and
after the table one line per pool:

```text
pool work · serving A · remote A · serial · 3 live sessions · holding A (5h 40%, resets in 2h) · last B→A 09:12 (limit) (B shared with personal)
```

The decision and the last switch follow the `auto:` line's rules (the decision
only while the daemon runs and auto-switch is on). A spread pool shows `spread`
and its pin. An account in more than one pool is flagged at the end of each of
its pools' lines: `(B shared with personal)`. The header's `serving` and
`remote` are the `default` pool's, and say `none` when it has no
members.

`policy` and `pin` show how new sessions are placed (see [`chottag
policy`](#chottag-policy)). `policy` is `"spread"` and is left out under
`serial`, so a serial document is exactly what it was before; `pin` is the
account `chottag tag NAME` pinned, left out when none is set or the policy is
`serial`. The text form adds a line after `auto:` only under spread:
`policy: spread`, or `policy: spread · pin: work`, and `(not a candidate now)`
after the pin when that account cannot take a session at this moment (rotation
off, needs a login, limited, or at a switch point). Under `serial` the text
form has no such line.

`update` is the daemon's release-check cache. It is left out until a check
has run (the daemon's, or `chottag update --check`). `latest` is the newest
release seen, `available` is whether it is newer than the version that ran the check (the daemon, or `update --check`), `notified`
is the version a notification was last sent for, `error` is the last check's
failure, and `auto` is the last auto-install attempt. `updates` is the two
switches from `state.json`, always present: `check` (the daemon's update check),
`auto` (its automatic install) and `restart` (its restart when idle); see [`chottag update`](#chottag-update).

The text form adds one line, `update: 0.6.0 available (run: chottag update)`,
only when the cache says a release newer than this build is available.

`daemon.restartPending` is the installed version the running daemon is not
yet using: the version `bin/chottag` resolves to under `versions/`, or, if it
does not, `install.json`'s `version` (the daemon then shows it but never
restarts itself onto it, since the restart would run `bin/chottag`), when it
is newer than the daemon's own. The daemon stamps it every minute; it is left out when nothing
is pending or the daemon is not running. The text form then prints `daemon:
running 0.6.0, installed 0.6.1 (restarts when idle, or run: chottag daemon
restart)`, or, with `--auto-restart off`, `(run: chottag daemon restart)`. The
daemon restarts itself only from 0.6.0 on: an older daemon never stamps
`restartPending`, so after an update from 0.5.x it needs one manual
`chottag daemon restart`.

`label` is the install's label (`chottag setup --label`); it is omitted when
none is set, and the text form shows it as `(NAME)` on the first line.

`accounts[].dir` (the account's slot directory) is never shown: `status`
clears it from every account before reporting.

`daemon.liveSessions` counts the `claude` sessions chottag launched that are
still alive, from the same session registry `daemon stop` reads. The text
form prints it as `live sessions: N` next to the daemon lines, with the
count per account when there are any: `live sessions: 3 (A 2, B 1)`. Accounts
come in name order; a session with no account yet, and an old shim's
unidentified one, count under `–`. It is `0` when none is alive, and is
present whenever a daemon object is reported or a session is alive.

`sessions` lists the same live sessions, one per `claude` chottag launched
that is still alive, ordered by `started`. It is left out when none is alive.

- `pid` and `started` are always there.
- `sid` is the first 8 characters of the session's id, enough to tell
  sessions apart and no more; `pool` is `default`. Both are left out for a
  session an older shim started, which chottag cannot identify.
- `account` is the account the session's last inference request used. It is
  left out until the session has made one.
- `lastSeen`, `requests` and `conversations` come from the daemon's activity
  record for the session: when it last made any request, how many requests
  chottag routed for it (each account choice counts, so a 429 retry counts
  twice), and how many times its conversation changed. They are left out
  until the daemon has seen the session.

### `chottag statusline`

```sh
chottag statusline [--json] [--cmux]
```

Prints one line for Claude Code's status line: whether the Claude Code
session that runs it goes through chottag, which account this session uses,
its usage, its next reset and the pool's health. It never reads stdin, and
always exits `0` (a bad argument is still exit `2`). It prints nothing
secret: no token, email, path or proxy secret.

| Flag | Meaning |
|---|---|
| `--cmux` | also set the cmux sidebar pill (see below); off by default |

`--json` (global) prints the fields instead of the line.

It is meant to be **consumed by your own status line**, not to replace it:
add its line to what your status-line script already prints, or read its
fields from `--json`. It needs nothing from stdin, so a script can call it
as it is:

```sh
#!/bin/sh
# Your status line, with chottag's segment added at the end.
input=$(cat)                                        # Claude Code's JSON for this session
line=$(printf '%s' "$input" | YOUR_EXISTING_COMMAND)
seg=$(~/.chottag/bin/chottag statusline 2>/dev/null)
printf '%s  %s\n' "$line" "$seg"
```

Point `statusLine` at `chottag statusline` itself only when you have no
status line of your own. Prefer it over `chottag status --json` in a status
line: it knows whether the session running it goes through chottag, which
`status --json` (the global view) does not, and it is built to run on every
redraw.

A session counts as routed when either holds: one of its first 8 ancestor
processes is a live `claude` session chottag launched, or `HTTPS_PROXY` is
`http://chottag...@127.0.0.1:<port>` for this home's port (a user of
`chottag` or `chottag.<pool>.<sid>`). Then the daemon is asked, for at most
300 ms, whether it answers.

The session is found first by `HTTPS_PROXY`: its user name, `chottag.<pool>.<sid>`
(`chottag.default.<sid>` outside a pool), gives the session id and pool. Only the user name is read; the password is never
read into anything it prints. Without one, the ancestor walk's registry
entry names the session. The account shown is that session's own, once the
daemon has seen it make an inference request. Before that, for a session
chottag cannot identify, and for the legacy `chottag:<secret>` credential, it
is the serving account. It never changes anything.

| Line | Meaning |
|---|---|
| `c» work · 5h 42% · 7d 18% · ↻ 19:00 · 2/3 ok` | routed, the daemon answers; `work` is the account this session uses |
| `c» C [work] · 5h 42% · …` | the same for a session in the pool `work` (`CHOTTAG_POOL=work`): `[work]` follows the account, only outside `default` |
| `c» up` | routed, the daemon answers, but no account is serving |
| `c» down` | routed, but the daemon does not answer |
| `c» off` | not routed, chottag is not set up, or anything went wrong |

With an install label (`chottag setup --label dev`), the label follows the
mark in every state: `c» dev off`, `c» dev · work · 5h 3% · …`.

- `5h NN%` and `7d NN%` are this session's account's usage, rounded; an unknown
  or stale value prints `–`.
- `↻ HH:MM` is its next reset: the 5 h reset when the 5 h use is at or above
  80 %, otherwise the earlier of the two known resets. Local time, or
  `Mon 18:00` when more than 24 hours away. Left out when unknown.
- `n/m ok` is the accounts in rotation that are not limited, out of all
  accounts in rotation, counted in this session's pool. Left out when none is
  in rotation.
- `↑0.6.0` is last, only when the update check found a newer release
  (`chottag update` installs it). It takes no colour, and the `off`, `down`
  and `up` lines never show it. The cmux pill carries it too.
- The figures come from the status cache the daemon keeps; the statusline
  asks the daemon nothing beyond the health probe.
- Colour: the mark and the label are bold blue (amber with a label); a
  limited serving account turns `5h` and `7d` red. Colour is on although
  stdout is not a terminal, because Claude Code renders it; `NO_COLOR`
  turns it off.

The cmux sidebar pill is only set with `--cmux`; a bare `chottag statusline`
has no side effect: it only prints, never calls cmux, and never touches
`run/`. With `--cmux`, and only when `CMUX_WORKSPACE_ID` is set, it also
sets a pill named `chottag` for that workspace, holding the same line
without colour, when the line changed since the last one sent (remembered in
`run/pill-<workspace>.txt`). The cmux call is bounded to 300 ms and its
failure is ignored. `--cmux` combines with `--json`.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "session": "routed",
  "daemon": "up",
  "serving": "work",
  "pool": "work",
  "account": "work",
  "label": "dev",
  "fiveHourPct": 41.6,
  "sevenDayPct": 18.2,
  "resetsAt": "2026-10-01T19:00:00+07:00",
  "okAccounts": 2,
  "rotationAccounts": 3,
  "updateAvailable": "0.6.0",
  "restartPending": "0.6.0"
}
```

`session` is `routed` or `home`. `daemon` is `up`, `down` or `unknown`
(`unknown` when the session is not routed: the daemon is not asked).
`serving` is empty unless the session is routed and the daemon is up.
`account` is the account this session uses (see above), present when
`serving` is; `serving` is the serving account of the session's pool.
`pool` is that pool, present only outside `default` (and only for an
identified session whose pool still exists).
`label`, `fiveHourPct`, `sevenDayPct`, `resetsAt` (RFC 3339), `okAccounts`
and `rotationAccounts` are left out when unknown. `updateAvailable` is the
release the update check found (newer than this build), and is left out
otherwise. `restartPending` is the installed version the daemon is not yet
running (`daemon.restartPending` in `status`); the text form shows it as
` · ⟳0.6.1` after the update suffix. Both are shown only while the session is
routed and the daemon is up.

### `chottag doctor`

```sh
chottag doctor [--fix]
```

Checks the install and, under `--fix`, repairs what it can safely repair.
Prints one line per check, then an indented `→ hint` line for a problem it
did not fix. Exits `0` when nothing is a problem, `3` when one remains
(`doctor_problems`, still under `--json`), and `1` for an internal error
(an unreadable `state.json`, or a check that panicked).

| Flag | Meaning |
|---|---|
| `--fix` | repair what doctor can repair safely |

Its checks, in the order they run:

| Check | What it checks | What fixes it |
|---|---|---|
| `setup` | chottag is set up in this home at all (gates every later check) | `chottag setup` |
| `tree` | every directory the tree needs exists | `--fix` creates the missing ones |
| `ca` | the local CA (`ca/ca.pem`, `ca/ca.key`) exists, loads and is safe | `--fix` creates a missing pair, or fixes an owned key's permissions, while no daemon runs |
| `proxy-secret` | `ca/proxy.secret` exists, is 0600 and well formed | `--fix` regenerates a missing or unusable one, while no daemon runs |
| `bin` | `bin/chottag` and `bin/claude` point at this binary | `--fix` relinks them, unless one already points at another working chottag |
| `rc-block` | your shell's rc file has the chottag PATH block | `--fix` writes it, copying the existing file to `backups/` first and printing the path on stderr |
| `path` | this process's PATH already finds `claude` in `bin/` | open a new shell |
| `roles` | the serving and remote roles name a registered account, in every pool: a pool's roles name one of its own members, and a pool with members has both | `--fix` gives a dangling role to the first registered account, and a dangling serving role to the first account in rotation (the row names the targets). In a pool other than `default` it uses that pool's members only, and leaves serving empty when none is in rotation |
| `identities` | accounts that share an email are listed as `info` (expected when one login is in several orgs; before 0.8.4 a background refresh could also write the serving account's email into another account's slot). Never a problem | `chottag login NAME` for an account whose email is wrong |
| `real-claude` | the cached real `claude` path is still what PATH resolves | `--fix` re-resolves and re-caches it |
| `port` | the daemon's port answers, or is free | `--fix` moves state.json to a free port (only with no daemon and no live sessions) |
| `daemon` | a daemon that holds `daemon.lock` also answers on its port | `chottag daemon restart` |
| `daemon-version` | a running daemon's health version matches this chottag binary | `chottag daemon restart` |
| `daemon-identity` | a running daemon proved it holds this install's proxy secret | `chottag daemon restart` |
| `token:NAME` | the daemon's last recorded token state for the account | `chottag login NAME` |
| `owners` | every `owners.json` entry names a registered account | `chottag own <kind> <id> <account>`, or re-run `chottag rename` |
| `route-drift` | the running daemon resent a swapped request unchanged | `chottag trace on` |
| `limits` | whether every account is currently limited | wait for a reset |
| `version-drift` | the installed Claude Code version has been traced | `chottag trace on` |
| `plan-unknown` | every account has a known plan tier | `chottag plan NAME TIER` |

With more than one pool, the `roles` row names each pool (`default: serving
A, remote A; work: serving B, remote B`) and flags a problem as `work serving
"X"`. A pool whose members are all out of rotation is a warning, not a
problem: the row is `info`, says `warning: pool work has no account in
rotation, so its sessions get a 503`, and hints `chottag rotate NAME on`.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "checks": [
    {
      "id": "version-drift",
      "status": "ok",
      "detail": "Claude Code 2.1.282 was traced on Sep 20 00:00",
      "hint": "",
      "installed": "2.1.282",
      "lastTraced": {"claudeVersion": "2.1.282", "at": "2026-09-20T00:00:00Z"}
    }
  ],
  "problems": 0
}
```

With one or more problems left, `doctor` fails instead: `ok: false`, exit
`3`, `error.code` is `doctor_problems`, and the same `checks` (every row,
including the ok ones) and `problems` (the problem count) travel under
`error`:

```json
{
  "version": 1,
  "ok": false,
  "warnings": [],
  "error": {
    "code": "doctor_problems",
    "message": "1 problem(s)",
    "exit": 3,
    "checks": [
      {
        "id": "bin",
        "status": "problem",
        "detail": "bin/chottag does not point at /Users/alice/.chottag/bin/chottag",
        "hint": "chottag doctor --fix",
        "installed": "",
        "lastTraced": {"claudeVersion": "2.1.282", "at": "2026-09-20T00:00:00Z"}
      }
    ],
    "problems": 1
  }
}
```

### `chottag notify`

```sh
chottag notify [on|off]
```

Shows or sets the desktop-notification switch (macOS; on by default). With
no verb it only prints the current setting. It takes no flags.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "notify": true
}
```

### `chottag policy`

```sh
chottag policy [serial|spread] [--pool POOL]
```

Shows or sets how new sessions are placed. `serial`, the default, is one
serving account for every session. `spread` places each new session on the
account with most headroom and keeps it there. With no value it prints the
policy, and the pin under `spread` (`pin: NAME`). Its only flag is
`--pool`: each pool has its own policy (default: `default`; unknown is
`no_pool`), and a result outside `default` says `policy: spread (pool work)`.
Any other value is exit 2, code `bad_policy`. Switching policy changes nothing
about rotation, and keeps a stored pin (which has no effect under `serial`).
See [`chottag tag`](#chottag-tag) for pinning.

| Flag | Meaning |
|---|---|
| `--pool POOL` | the pool whose policy to show or set (default: `default`) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "policy": "spread",
  "pin": "work",
  "pool": "work"
}
```

`pin` is always present, `""` when unset or when the policy is `serial`.
`pool` is present only outside `default`.

Setting `spread` on a pool that has an account shared with another pool adds
a `shared_account` warning for each such account (see
[`chottag pool`](#chottag-pool)).

When the running daemon is older than this `chottag` (or its version cannot
be ordered), turning spread on adds the warning `daemon_predates_spread`:
a daemon from before 0.7.0 ignores the policy and, the next time it writes
`state.json`, drops it. Run `chottag daemon restart` first, then the policy
takes effect. The warning does not refuse the change, and `tag NAME` under
spread gives it too. No daemon, or one at the same or a newer version, gives
none.

## Pools

### `chottag pool`

```sh
chottag pool
chottag pool add NAME
chottag pool join ACCOUNT POOL
chottag pool leave ACCOUNT POOL
chottag pool rm NAME
```

A pool is a named set of accounts with its own serving account, remote
account, policy and pin. The top-level fields of `state.json` are the
`default` pool, which always exists. An account can be in several pools. A
session picks its pool with `CHOTTAG_POOL=NAME claude`; unset means `default`.

`chottag pool` lists the pools, `default` first and the rest sorted: name,
serving, remote, policy, the pin (under spread) and the member accounts; text output says `none` for an empty role or pool.

| Form | What it does |
|---|---|
| `pool add NAME` | create an empty pool; `NAME` is 1-16 of `a-z`, `0-9` and `-` (`bad_pool`, `pool_exists`) |
| `pool join ACCOUNT POOL` | add `ACCOUNT` to `POOL`; it stays in its other pools. In `POOL` it becomes serving if `POOL` has none and it rotates, and remote if `POOL` has none. Already a member: nothing changes |
| `pool leave ACCOUNT POOL` | remove `ACCOUNT` from `POOL`. It must stay in at least one pool (`last_pool`). A serving or remote role it held in `POOL` goes to the next eligible member, or to none; a pin on it is cleared |
| `pool rm NAME` | remove an empty pool other than `default` (`pool_not_empty`, `pool_default`). Removing the last extra pool writes `state.json` back as version 1 |

An unknown pool is `no_pool`. Every pool error exits 2. `rotate`, `plan`,
`rename` and `logout` act on the account in every pool it is in.

**Sharing an account warns.** When `pool join` leaves an account in more than
one pool, it succeeds and warns `shared_account`: the usage limit is shared,
so the pools affect each other. With a `spread` pool among them it says that
their sessions compete for the account and that heavy use in one pool moves
the other's sessions off it (their prompt caches go cold); with every pool on
`serial` it says that if the account serves both, they use up its 5-hour
window together and switch away from it at the same time. `chottag policy
spread --pool POOL` repeats it for each shared account in `POOL`.

**An older daemon.** A daemon older than 0.8.0 (or one reporting no usable
version) cannot read the version-2 `state.json` pools write: its requests would
go out on Home's own login, and it cannot restart itself onto 0.8.0 once it
cannot read the file. So `pool add` refuses while one runs (`daemon_predates_pools`,
exit 2: run `chottag daemon restart` first), and `pool join`, when the pool
already exists, warns with the same code. The shim backstops it: once an extra
pool exists, against such a daemon every `claude` session, `default` included,
exits 2 without starting Claude Code (`chottag: the running daemon (0.7.1)
predates pools, so a "work" session would not stay in its pool; run: chottag
daemon restart`). With no extra pool, nothing changes. None of this applies when
no daemon runs or it is 0.8.0 or later.

**Notices name the pool.** With more than one pool, a desktop notice says
which pool it is about: `chottag: work: switched to C`, `chottag: work: no
account to switch to`. A spread move names the account's pools that spread
(`chottag: work, personal: moved 2 sessions from B to C`). Each pool keeps its
own "no account to switch to" episode, so one pool's cannot hide another's.
With only `default` the texts are unchanged.

`update --version` below 0.8.0 is refused while pools beyond `default` exist,
exit 2 with `pools_block_rollback`, before anything is downloaded: remove the
extra pools with `chottag pool rm` first.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "pools": [
    {
      "name": "default",
      "serving": "personal",
      "remote": "personal",
      "policy": "serial",
      "pin": "",
      "accounts": ["personal"]
    }
  ]
}
```

`pin` is `""` unless the pool's policy is `spread`. `accounts` is an array,
empty for an empty pool.

### `chottag pool add`

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "pool": "work"
}
```

### `chottag pool join`

```json
{
  "version": 1,
  "ok": true,
  "warnings": [
    {"code": "shared_account", "message": "personal is now in default and work and shares one usage limit between them: if personal serves both, they use up its 5-hour window together and switch away from it at the same time."}
  ],
  "account": "personal",
  "pool": "work",
  "pools": ["default", "work"],
  "changed": true
}
```

`pools` is every pool the account is in afterwards. `changed` is false when it
was already a member.

### `chottag pool leave`

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "account": "personal",
  "pool": "work",
  "pools": ["default"],
  "changed": true
}
```

`pools` is what the account is in afterwards. Leaving a pool the account is not
in is `not_in_pool`, so `changed` is always true here.

### `chottag pool rm`

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "pool": "work"
}
```

`pool` names the removed pool.

## Auto-switch

```sh
chottag auto [VERB]
```

Shows, or with a verb changes, auto-switch settings: `on` and `off` toggle
it (on by default); `mode M` picks the planner mode; `set KEY VALUE` and
`reset` override or clear one setting. See
[auto-switch.md#switch-points](auto-switch.md#switch-points) and
[auto-switch.md#settings](auto-switch.md#settings) for every key `set`
accepts and what each mode does. `auto` takes no flags.

### `chottag auto`

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "enabled": true,
  "mode": "balanced",
  "switchPoints": {
    "5h.pro": 88,
    "5h.max5x": 90,
    "5h.team": 93,
    "5h.max20x": 98,
    "7d.pro": 93,
    "7d.max5x": 98,
    "7d.team": 98,
    "7d.max20x": 99
  },
  "hold5h": "10m",
  "hold7d": "1h",
  "cooldown": "5m",
  "overrides": ["5h.max5x", "hold5h", "hold7d", "cooldown"],
  "accounts": [
    {
      "name": "work",
      "plan": "max5x",
      "tier": "max5x",
      "units": 5,
      "fiveHourPct": 42.5,
      "fiveHourResetsAt": "2026-09-28T19:00:00Z",
      "sevenDayPct": 10,
      "sevenDayResetsAt": "2026-10-03T00:00:00Z",
      "stale": false
    }
  ],
  "decision": "holding work (5h 42%, resets in 5h)",
  "lastSwitch": {
    "from": "personal",
    "to": "work",
    "trigger": "threshold",
    "window": "5h",
    "pct": 90,
    "at": "2026-09-28T09:00:00Z",
    "retried": true
  }
}
```

Under `chottag policy spread` the `decision` is `spread: new sessions go to
NAME` (the account a new session would get now, which is also `serving`), or
`spread: no account can take a new session`. Auto-switch does not switch
`serving` for a switch point then; see
[how it works](how-it-works.md#spreading-sessions-spread).

## Daemon

### `chottag daemon start`

```sh
chottag daemon start
```

Starts the daemon in the background unless it is already running. It takes
no flags. (The shim already does this on demand, the first time `claude`
runs.)

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "pid": 4242,
  "port": 47821,
  "started": true
}
```

### `chottag daemon stop`

```sh
chottag daemon stop [--force]
```

Stops the daemon; the next `claude` launch starts it again. It refuses
while a supervisor would relaunch it, or while a `claude` session is live,
unless `--force`.

| Flag | Meaning |
|---|---|
| `--force` | stop even under a supervisor or with live claude sessions |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "pid": 4242,
  "stopped": true,
  "alreadyExited": false,
  "temporary": false
}
```

### `chottag daemon restart`

```sh
chottag daemon restart [--force]
```

Stops the daemon, then starts it again. Unlike `stop`, a live session does
not refuse it — it only sees a brief interruption.

| Flag | Meaning |
|---|---|
| `--force` | restart even under a supervisor |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "oldPid": 4242,
  "pid": 4300,
  "port": 47821,
  "relaunchedBy": "chottag",
  "upstreamChanged": {"from": "", "to": "http://127.0.0.1:3128"},
  "liveSessions": 1
}
```

### `chottag daemon logs`

```sh
chottag daemon logs [-n N] [-f]
```

Prints the last `N` lines of `daemon.log`. `-f` keeps printing lines as
they are written. does not support `--json`.

| Flag | Meaning |
|---|---|
| `-n N` | number of lines to print |
| `-f` | keep printing lines as they are written |

### `chottag daemon run`

```sh
chottag daemon run [--claude PATH] [--log PATH] [--upstream-proxy URL]
```

Runs the daemon in the foreground: the routing proxy, credential refresh
and auto-switch. The shim starts this on demand; running it yourself is
for development or a supervisor unit. does not support `--json`.

| Flag | Meaning |
|---|---|
| `--claude PATH` | path to the real `claude` binary (default: the cached real `claude`, else the one on PATH, looked up again on every refresh; never chottag's own shim) |
| `--log PATH` | request log path; empty disables request logging |
| `--upstream-proxy URL` | route chottag's own upstream traffic through this proxy (e.g. `http://127.0.0.1:3128`) |

## Tracing and developer tools

### `chottag trace`

```sh
chottag trace
chottag trace off
```

With no verb, shows whether the daemon's trace mode is on and when it
expires. `trace off` stops it early. Neither takes a flag.

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "tracing": true,
  "until": "2026-09-28T15:00:00Z",
  "lastTraced": {"claudeVersion": "2.1.282", "at": "2026-09-20T00:00:00Z"}
}
```

### `chottag trace on`

```sh
chottag trace on [--for DUR]
```

Switches the daemon's trace mode on for `DUR` (a Go duration; default 1h,
at most 24h).

| Flag | Meaning |
|---|---|
| `--for DUR` | how long to trace (a Go duration, at most 24h; default 1h) |

```json
{
  "version": 1,
  "ok": true,
  "warnings": [],
  "tracing": true,
  "until": "2026-09-28T15:00:00Z",
  "lastTraced": {"claudeVersion": "2.1.282", "at": "2026-09-20T00:00:00Z"}
}
```

### `chottag trace mark`

```sh
chottag trace mark TEXT...
```

Adds a marker line to the trace log. does not support `--json`.

| Flag | Meaning |
|---|---|
| `--log PATH` | trace log path |

### `chottag trace summarize`

```sh
chottag trace summarize [--all]
```

Prints the route table recorded since the last `trace on` (`--all`: the
whole log). does not support `--json`.

| Flag | Meaning |
|---|---|
| `--log PATH` | trace log path |
| `--all` | summarize the whole log, not only since the last trace on |

### `chottag trace run`

```sh
chottag trace run [flags]
```

A developer tool: a separate, observe-only proxy that never carries real
traffic. does not support `--json`.

| Flag | Meaning |
|---|---|
| `--listen ADDR` | proxy address (loopback) |
| `--log PATH` | trace log path |
| `--shapes` | record JSON shapes (types only) of bodies |
| `--limit-fingerprint` | record a redacted usage-limit classifier fingerprint (header names, an allowlist of rate-limit header values, `error.type`, and an extracted reset timestamp) for responses with status ≥ 400 — never the body |
| `--intercept SUFFIXES` | comma-separated host suffixes to intercept |
| `--keychain-service NAME` | override the macOS Keychain service name for `--swap` slots |
| `--swap CLASS=DIR` | send `CLASS` (`serving`\|`remote`) requests with the login in account dir `DIR` (repeatable, experimental) |

### `chottag trace env`

```sh
chottag trace env [--listen ADDR]
```

A developer tool: prints the shell export lines for a `trace run` session.
does not support `--json`.

| Flag | Meaning |
|---|---|
| `--listen ADDR` | proxy address |

### `chottag proxy run`

```sh
chottag proxy run [flags]
```

Runs the routing proxy in the foreground, without the daemon around it.
does not support `--json`.

| Flag | Meaning |
|---|---|
| `--listen ADDR` | proxy address (loopback) |
| `--log PATH` | request log |
| `--claude PATH` | path to the real claude binary used to refresh a slot login (default: the cached real `claude`, else the one on PATH, looked up again on every refresh; never chottag's own shim) |
| `--upstream-proxy URL` | route chottag's own upstream traffic through this proxy (e.g. `http://127.0.0.1:3128`) |

## Error codes

| Code | Exit | What happened, and what to do |
|---|---|---|
| `usage` | 2 | the command line was wrong; fix it and retry |
| `json_unsupported` | 2 | this command refuses `--json`; drop the flag |
| `internal` | 1 | an unexpected internal failure (an unreadable state file, a failed write); see the message |
| `unknown_account` | 1 | no account matches the name, email or prefix given |
| `ambiguous_account` | 1 | the prefix given matches more than one account; use a longer one |
| `no_candidate` | 3 | no account can take the serving role right now; `error.skipped` lists why each was passed over |
| `not_found` | 1 | the object `chottag own` was asked about is not in `owners.json` |
| `login_failed` | 1 | `claude auth login` did not complete |
| `role_held` | 3 | the account holds the serving or remote role; retry with `--force` |
| `out_of_tree` | 3 | a path given resolves outside `$CHOTTAG_HOME` |
| `confirmation_required` | 3 | the action needs a typed confirmation that was not given |
| `aborted` | 1 | the user declined a confirmation prompt |
| `revoke_failed` | 1 | `claude auth logout` (the revoke) did not complete |
| `credential_delete_failed` | 1 | the slot's stored credential could not be deleted |
| `no_slots` | 1 | no account slot directories were found to adopt |
| `no_accounts` | 1 | no account is registered yet |
| `not_chottag_home` | 1 | `$CHOTTAG_HOME` (or `~/.chottag`) does not look like a chottag install |
| `purge_failed` | 1 | `uninstall --purge` could not delete the tree |
| `foreign_daemon` | 1 | a daemon on the expected port belongs to another install |
| `unhealthy` | 1 | the daemon did not answer a health check |
| `unreadable_record` | 1 | `daemon.lock`'s record could not be read |
| `start_failed` | 1 | the daemon did not start |
| `supervised` | 3 | a supervisor would relaunch the daemon; retry with `--force` |
| `live_sessions` | 3 | a live claude session is using the daemon; retry with `--force` |
| `signal_failed` | 1 | sending a signal to the daemon process failed |
| `stop_timeout` | 1 | the daemon did not exit within the timeout |
| `stop_failed` | 1 | the daemon could not be stopped |
| `supervisor_no_relaunch` | 1 | a supervisor holds the daemon, but would not relaunch it after a stop |
| `session_registry_unreadable` | 1 | the live-session registry could not be read |
| `name_taken` | 2 | the account name given is already in use |
| `doctor_problems` | 3 | `doctor` found one or more problems; `error.checks` and `error.problems` carry every row and the count |
| `update_failed` | 1 | `chottag update` could not install the release (or back up `state.json` first), or `--check` could not reach GitHub |
| `update_in_progress` | 1 | another `chottag update` holds the update lock (`run/update.lock`) |
| `bad_policy` | 2 | `chottag policy` takes `serial` or `spread`, nothing else |
| `no_pool` | 2 | the pool named does not exist (`chottag pool` lists them) |
| `bad_pool` | 2 | a pool name must be 1-16 of `a-z`, `0-9` and `-` |
| `pool_exists` | 2 | `pool add` named a pool that exists (`default` always does) |
| `daemon_predates_pools` | 2 | `pool add` while the running daemon is older than 0.8.0 or reports no usable version: it cannot read the version-2 `state.json`; run `chottag daemon restart` first (`pool join` only warns) |
| `pool_not_empty` | 2 | `pool rm` of a pool that still has accounts; `pool leave` them first |
| `pool_default` | 2 | `pool rm default`: the default pool cannot be removed |
| `last_pool` | 2 | `pool leave` would leave the account in no pool |
| `not_in_pool` | 2 | the account is not in the pool named (`--pool`, `pool leave`) |
| `pool_ambiguous` | 2 | `tag NAME` or `remote NAME` for an account in several pools, without `--pool` |
| `pools_block_rollback` | 2 | `update --version` below 0.8.0 while pools beyond `default` exist: `chottag pool rm` them first |
| `spread_next` | 2 | `next` (or a bare `tag`) under `chottag policy spread`: chottag places sessions itself; pin with `chottag tag NAME`, or `chottag policy serial` |

## Warning codes

| Code | Meaning |
|---|---|
| `limited` | the account named is currently limited |
| `out_of_rotation` | an account was skipped because it is excluded from rotation |
| `no_rotation_left` | no account is left in rotation to switch to (or, naming a pool, none left in that pool) |
| `pool_not_changed` | `login NAME --pool P` for an account that already exists and is not in `P`: `--pool` applies only to a new account; `chottag pool join` adds it |
| `owners_recovered` | `owners.json` was corrupt and has been moved aside |
| `email_registered` | the email logged in belongs to an account already registered |
| `identity_suspect` | `adopt` (or `setup`, `update`) read a slot whose email changed to a serving account's with the organisation unchanged, and left the stored identity alone; run `chottag login NAME` to repair it |
| `revoke_failed` | the revoke step failed, but the command continued |
| `name_no_slot` | a name in `--name` matches no slot directory |
| `left_in_place` | something that could have been removed was left in place |
| `session_registry_unreadable` | the live-session registry could not be read, but the command continued |
| `live_sessions` | a live claude session exists; it will pick up the change on its next request |
| `upstream_changed` | the daemon's upstream proxy setting changed on this restart |
| `supervisor_relaunch` | a supervisor relaunched the daemon |
| `new_daemon` | a new daemon instance started where an old one was expected |
| `rc_not_written` | the shell rc file was not updated |
| `adopt_failed` | the inner `adopt` step (run by `setup`) failed |
| `update_deferred` | an available update was not installed |
| `restart_failed` | the daemon did not restart after an update |
| `prune_failed` | an old release or backup could not be pruned |
| `install_record` | the install record could not be read or written |
| `attestation_skipped` | release attestation verification was skipped |
| `pre_attestation` | the release predates attestation and was installed without verifying one |
| `update_cache` | `update --check` could not record its result in `status.json` |
| `shared_account` | `pool join` left an account in more than one pool (or `policy spread` found one in the pool): they share one usage limit, so the pools affect each other |
| `daemon_predates_pools` | `pool join` found the running daemon older than 0.8.0 (or reporting no version): it cannot read a version-2 `state.json`, so its requests fall back to Home's own login; run `chottag daemon restart` (`pool add` refuses instead: see the error code) |
| `daemon_predates_spread` | `chottag policy spread` (or `tag NAME` under spread) found the running daemon older than this binary: it ignores the policy and may reset it to serial; run `chottag daemon restart` first |
