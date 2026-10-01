# Updating

## Check and install

`chottag update --check` reports whether a newer release exists, without
installing it. `chottag update`, with no flags, checks and — if a newer
release exists — downloads, verifies and installs it. Only one update runs at
a time: a second `chottag update` exits with `update_in_progress`.

## The daily check

The daemon looks for a newer release by itself: 2 minutes after it starts,
then about once a day. The result is shown in `chottag status` (`update: 0.6.0
available (run: chottag update)`) and at the end of `chottag statusline` (`·
↑0.6.0`). `chottag update --check` runs the same check and refreshes the same
cache (`update` in `status.json`).

- **Privacy.** The check is one HTTPS request a day to `api.github.com`
  (`GET /repos/<repo>/releases/latest`). It sends no token and nothing
  beyond the request itself, and needs no `gh`. The daemon sends it through
  its upstream proxy, if it has one; `chottag update --check` uses your
  shell's `HTTPS_PROXY` (unless that is chottag's own).
- **Turn it off.** `chottag update --auto-check off`, or set
  `CHOTTAG_NO_UPDATE_CHECK=1` in the daemon's environment. A development
  build (a version that is not a release number, such as `dev`) never checks.
  Turning the check off also turns auto-install off.

## Automatic install

Off by default. `chottag update --auto-install on` lets the daemon install a
new release by itself (it also turns the check on); `--auto-install off` stops
it. It installs only when all of these hold:

- the release has the same major version as the one running;
- it was published at least 24 hours ago;
- that version was not already tried in the last 24 hours;
- no other `chottag update` is running.

It runs the same `chottag update` you would, with `--no-restart`, so the
checksum, the attestation and the `state.json` backup all apply, and `gh`
must be installed. The install itself never restarts the daemon; the daemon
switches to the new version by itself when it is idle (see [Restarting onto
an installed update](#restarting-onto-an-installed-update)), or after
`chottag daemon restart`. You get one notification when it installs and one
if it could not.

## Restarting onto an installed update

A Mac that only sleeps never restarts its daemon, and `chottag update` holds
the restart back while a Claude Code session is running. So a newer chottag
can sit installed for weeks while the old one keeps running. To close that
gap, the daemon looks at the installed version every minute and every time
the Mac wakes from sleep. When the installed version is newer than the one
running, `chottag status` says so (`daemon: running 0.6.0, installed 0.6.1
(restarts when idle, or run: chottag daemon restart)`, and `daemon.restartPending`
in `--json`) and `chottag statusline` ends with ` · ⟳0.6.1`. The self-restart works with daemons from 0.6.0 on; after an update from 0.5.x, restart once by hand with `chottag daemon restart`.

- **When it restarts.** Only when the proxy is idle: no request in flight (a
  streaming reply counts until it ends) and none started in the last 5
  minutes. A Claude Code session that is open but quiet does not block it; it
  reconnects, as after any `chottag daemon restart`.
- **How.** The daemon starts the installed `chottag daemon restart --force`
  in its own process, the same restart you would run by hand. Under launchd
  or systemd, `--force` signals the daemon and the supervisor starts it
  again from the same link.
- **Quiet means quiet.** A long-lived connection with a request in it, such
  as Remote Control or another WebSocket, counts as a request in flight for
  as long as it lives, so it delays the restart. A daemon that has just
  started also waits 5 quiet minutes.
- **Limits.** At most one attempt per installed version per hour, kept in
  `run/restart.json` so a new daemon does not start again at once. The
  restart's own output goes to `run/restart.log`; if it did not happen, the
  next look tells you once. You get
  one notification when a newer version is installed (`chottag 0.6.0 is
  installed. The daemon switches to it when Claude Code is idle.`), and one
  per version if the restart could not start or did not happen (it says
  "could not restart onto 0.6.0", gives the reason, and suggests `chottag
  daemon restart`).
- **Turn it off.** `chottag update --auto-restart off`; `--auto-restart on`
  turns it back on. It is on by default. With it off the daemon still reports
  the pending version, but never restarts by itself. A development build never
  restarts itself.

## Which repo

`chottag update` looks for a release in, in order: the repo `--repo
OWNER/NAME` names on the command line; failing that, the `repo` field in
`~/.chottag/install.json` (or `$CHOTTAG_HOME/install.json`); failing that,
the built-in default. Passing
`--repo` explicitly writes that repo back into `install.json` once the
update succeeds, so the next bare `chottag update` keeps using it.

## What it verifies

Every download is checked against the release's `checksums.txt`. From
v0.4.0 on, a release from the public repo also carries a build attestation,
and `chottag update` runs `gh attestation verify` against it, failing
closed if it does not verify. A private repo skips the attestation check
with a warning, since GitHub only attests public repositories.

## The daemon

If no Claude Code session is currently running, `chottag update` restarts
the daemon for you once the new version is installed. If a session is
running, the restart is deferred: the daemon restarts itself when it is idle
(see [above](#restarting-onto-an-installed-update); not if you turned
`--auto-restart` off), or run `chottag daemon restart` yourself once you're
ready. `chottag update --restart` restarts the daemon at once
regardless, and any running session sees a short interruption.
`chottag update --no-restart` installs and leaves the daemon alone, session or
not. Until the
daemon restarts, `chottag status` and `chottag doctor` warn that it is
still running an older version. After an update to v0.4.0, a session
started before the daemon restart gets a `407` on its next request and
must be restarted.

## Backups

Before anything is extracted or the new binary's `setup` runs (and after the
download is verified), `chottag update` copies `state.json` to
`backups/state.json.<UTC stamp>` in your chottag home (`~/.chottag` by
default); if that copy fails, nothing is installed. `update --json` lists it
as `backups`. Separately, `setup` copies your shell rc file to
`backups/zshrc.<stamp>` (or `bashrc`) before changing it, and prints the path
on stderr; if that fails, `setup` stops before the rc is touched, though
`bin/` is already relinked by then. Directory 0700, files 0600, the newest 5
of each kept; a failure to remove an old one only warns (`prune_failed`).

The `state.json` backup starts with updates run by 0.6.0 or later. Before
the first update to 0.6.0, copy it by hand once:
`cp ~/.chottag/state.json ~/.chottag/state.json.pre-v0.6.0`.

## Old versions

`chottag update` prunes `versions/` down to the last three, plus whichever
version `bin/chottag` is currently linked to and whichever the daemon is
currently running, so neither is ever removed out from under you.

## Roll back

`chottag update --version vX.Y.Z` installs a specific release, older or
newer, the same way a regular update does. Follow it with `chottag daemon
restart` so the daemon picks up the change.

To undo an update: stop the daemon (`chottag daemon stop`), run `chottag
update --version vX.Y.Z` to put the older binary back, copy back the
`state.json` backup that the update being undone printed (it is also listed
in its `backups`; not the newest file, which the rollback itself just made),
then start the daemon (`chottag daemon start`).

## The plugin

The plugin and skill update separately from the binary: `claude plugin
marketplace update c-hottag`, then restart the Claude Code session so it
picks up the new skill.

## Another way

Running `install.sh` again also installs the latest release, the same way
the original install did.

See [`commands.md#chottag-update`](commands.md#chottag-update) for every
flag and the `--json` fields.
