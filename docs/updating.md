# Updating

## Check and install

`chottag update --check` reports whether a newer release exists, without
installing it. `chottag update`, with no flags, checks and — if a newer
release exists — downloads, verifies and installs it.

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
running, the restart is deferred: run `chottag daemon restart` yourself
once you're ready. `chottag update --restart` restarts the daemon at once
regardless, and any running session sees a short interruption. Until the
daemon restarts, `chottag status` and `chottag doctor` warn that it is
still running an older version. After an update to v0.4.0, a session
started before the daemon restart gets a `407` on its next request and
must be restarted.

## Old versions

`chottag update` prunes `versions/` down to the last three, plus whichever
version `bin/chottag` is currently linked to and whichever the daemon is
currently running, so neither is ever removed out from under you.

## Roll back

`chottag update --version vX.Y.Z` installs a specific release, older or
newer, the same way a regular update does. Follow it with `chottag daemon
restart` so the daemon picks up the change.

## The plugin

The plugin and skill update separately from the binary: `claude plugin
marketplace update c-hottag`, then restart the Claude Code session so it
picks up the new skill.

## Another way

Running `install.sh` again also installs the latest release, the same way
the original install did.

See [`commands.md#chottag-update`](commands.md#chottag-update) for every
flag and the `--json` fields.
