# Uninstall

## Keep your logins

`chottag daemon stop` stops the daemon. It refuses while a Claude Code
session is using it, or while a service manager supervises it — pass
`--force` to stop it anyway.

`chottag uninstall` then removes the rc block it added to your shell
startup file, the `bin/chottag` and `bin/claude` symlinks, and
`ca/bundle.pem`. It keeps every account's login and `versions/`, so a later
`install.sh` picks up right where you left off.

If you also want the old release binaries gone, remove them yourself:

```sh
rm -rf ~/.chottag/versions
```

Everything above lives under `$CHOTTAG_HOME` instead of `~/.chottag` when
you installed with it set.

## A service unit

If you installed a `launchd` or `systemd` unit from `packaging/`, `chottag
uninstall` does not touch it: unload it yourself first, with the exact unit
names from
[`packaging/README.md`](https://github.com/HaiNNT/c-hottag/blob/main/packaging/README.md).

If you installed the macOS launchd agent:

```sh
launchctl bootout gui/$(id -u)/com.chottag.daemon
rm ~/Library/LaunchAgents/com.chottag.daemon.plist
```

`launchctl bootout` only unloads it until your next login — the plist sets
`RunAtLoad` and `KeepAlive`, which bring it straight back otherwise — so
remove the plist file too.

If you installed the Linux systemd user unit:

```sh
systemctl --user disable --now chottag.service
rm ~/.config/systemd/user/chottag.service
systemctl --user daemon-reload
```

## Remove everything

`chottag uninstall --purge` deletes the whole chottag home, every account's
login included. Run it instead of plain `uninstall`: plain `uninstall`
removes `bin/chottag` from `PATH`, so once it has run, a later `--purge`
needs the full path, `~/.chottag/versions/<version>/chottag uninstall
--purge` — run `--purge` first if you want everything gone in one step.

`--purge` does not stop the daemon itself: stop it first the same way as
above, `chottag daemon stop` (or `--force` if it refuses).

Run it in a terminal, not through `--json`: it refuses `--json` and instead
asks you to type `purge` to confirm, since it cannot be undone.

## The plugin

The plugin and its marketplace are separate from the binary:

```sh
claude plugin uninstall chottag@c-hottag
claude plugin marketplace remove c-hottag
```

## A status line

If you pointed the skill's `statusLine` entry in `~/.claude/settings.json`
at chottag, remove it or point it back at your previous command yourself;
chottag never edits that file.

## If chottag is broken

`CHOTTAG_BYPASS=1 claude` runs the real Claude Code directly, bypassing the
shim entirely. You can also call the real binary by its full path.

## Next

- [Updating](updating.md)
- [Troubleshooting](troubleshooting.md)
- [Documentation home](index.md)
