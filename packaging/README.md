# Installing the chottag daemon as a service

`chottag daemon run` is the command the service files below invoke. It does
**not** detach — the supervisor (launchd or systemd) owns the process, and a
daemon that forks would fight its own supervisor. It does hold
`~/.chottag/run/daemon.lock` for its whole life, and records there that
launchd or systemd started it. So `chottag daemon stop` and `chottag daemon
restart` refuse a supervised daemon (exit 3) and print the supervisor's own
command instead (`launchctl bootout` / `launchctl kickstart -k`,
`systemctl --user stop|restart chottag.service`), because a
`KeepAlive`/`Restart=always` unit restarts whatever they kill. `--force`
overrides that; `restart --force` on a supervised daemon does not spawn a
replacement itself; it stops the daemon and then waits (up to 15s) for the
supervisor's own relaunch, reporting that daemon's pid.

Neither launchd nor systemd is installed for you by `chottag` yet
(`packaging/` ships the unit files only — there is no installer). Both unit
files run `~/.chottag/bin/chottag`, the link `chottag setup` makes. Install
chottag first (`./install.sh`, see the top-level README); it puts the binary
in `~/.chottag/versions/<version>/` and links `bin/chottag` to it. Don't copy
a binary to `~/.chottag/bin/chottag` yourself: that path is setup's link.

## macOS (launchd)

The plist uses a `__HOME__` placeholder instead of `$HOME`, because launchd
does not expand environment variables in `ProgramArguments` or path keys.
Substitute it at install time:

```sh
mkdir -p ~/Library/LaunchAgents
sed "s|__HOME__|$HOME|g" packaging/launchd/com.chottag.daemon.plist \
  > ~/Library/LaunchAgents/com.chottag.daemon.plist
```

Load it:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.chottag.daemon.plist
```

`chottag uninstall` does not unload it: unload it yourself, e.g. before
reinstalling:

```sh
launchctl bootout gui/$(id -u)/com.chottag.daemon
```

`RunAtLoad` and `KeepAlive` are both set, so the agent starts at login and
launchd restarts it if it exits.

## Linux (systemd user unit)

```sh
mkdir -p ~/.config/systemd/user
cp packaging/systemd/chottag.service ~/.config/systemd/user/chottag.service
systemctl --user enable --now chottag.service
```

`chottag uninstall` does not disable or stop this unit either: run
`systemctl --user disable --now chottag.service` yourself.

On most headless setups a user's systemd instance (and everything in it)
stops when their last session logs out. To keep the daemon running across
logout, enable lingering for your user once:

```sh
loginctl enable-linger $USER
```

Check it:

```sh
systemctl --user status chottag
```

The unit sets `ProtectSystem=strict` with `ReadWritePaths=%h/.chottag`: the
daemon writes only under `~/.chottag` (state, cache, logs, and the per-account
`CLAUDE_CONFIG_DIR` slots it execs the real `claude` binary against for token
refresh), and everything else is read-only to the process. This has **not**
been exercised against a real systemd instance — it is untested hardening,
shipped deliberately rather than silently. If token refreshes start failing
only under the systemd unit (and not when the same binary is run by hand),
`ProtectSystem=strict` / `ReadWritePaths` is the first thing to relax: try
dropping `ProtectSystem=strict` (or widening `ReadWritePaths`) and see if the
failure clears. A refresh failure surfaces as a stale/needs-login token state
in `chottag status` even though `claude auth login` still works when run
directly.

The second thing to check: `CLAUDE_CONFIG_DIR` always resolves under
`~/.chottag/accounts`, so chottag's own writes are covered by
`ReadWritePaths`. What is **not** verified is whether the real `claude`
binary — which chottag execs as a child with that `CLAUDE_CONFIG_DIR` set,
for token refresh — writes anything *outside* it, e.g. caches or telemetry
under `~/.cache` or similar. If refreshes fail only under the systemd unit
after the first check above is ruled out, that's the next place to look.

## Logs

The daemon logs to a rotating `~/.chottag/daemon.log`; normal operation is
silent on the daemon's own stdout/stderr. On macOS, a failure to *start*
(e.g. the port cannot be resolved) is written to `~/.chottag/daemon.stderr.log`
via the plist's `StandardErrorPath`. By that point `daemon.log` is already
open and also gets the message — `StandardErrorPath` exists on top of that
by deliberate design, so the failure is visible in launchd's own log, which
launchd itself surfaces (e.g. in Console.app), rather than only inside the
rotating `daemon.log` launchd never looks at. `StandardOutPath` is
deliberately left unset — duplicating `daemon.log` into an unrotated file
would defeat the rotation. On Linux, `journalctl --user -u chottag` shows
anything written to stdout/stderr, which in normal operation should be
nothing.

## When the port stays taken

`daemon run` fails immediately (exit 1) whenever its configured port
(`state.json`'s `port`, or `47821` by default) is already taken by
something else — that failure is deliberate (see `store.State.ResolvedPort`
in `internal/store/store.go`, spec §4.8): the port is fixed and never
searched for a free alternative, because a running session's
`HTTPS_PROXY` was fixed at exec, so silently moving the port would point it
at nothing. That condition does not clear itself, so the supervisor's
automatic-restart behaviour matters:

- **launchd**: our plist sets no `ThrottleInterval`, so launchd falls back
  to its own default (~10s) throttle and retries **forever** with no
  give-up. The agent will keep failing quietly in the background until
  whatever is holding the port is stopped.
- **systemd**: `RestartSec=2` with no `StartLimitIntervalSec`/
  `StartLimitBurst` override means the unit will likely trip systemd's own
  default start-rate limit and the unit ends up in the **failed** state —
  silently down, not retrying, until someone clears it:
  ```sh
  systemctl --user reset-failed chottag
  ```
  Run that (after resolving whatever is holding the port) to let systemd
  resume restarting the unit.
