# Changelog

Every notable change to c-hottag, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each version's
heading links to its full release notes in `docs/release-notes/`. A version
reads "Unreleased" until it is dated in the commit that bumps plugin.json
to it.

## [0.5.1] - 2026-10-01

### Changed

- Status-line docs and the plugin skill add chottag's segment to your own status line instead of replacing it.
- `chottag statusline` no longer changes anything: the cmux sidebar pill is now opt-in with `--cmux`.
  See [release notes](docs/release-notes/v0.5.1.md).

## [0.5.0] - 2026-09-30

### Added

- The chottag mark (`c»`), blue for prod and amber for dev, in `assets/logo/`.
- `chottag statusline` v2: usage, next reset and available accounts, in colour; a cmux sidebar pill.
- `chottag setup --label NAME`: a labelled install shows its label in notifications and `status`.
  See [release notes](docs/release-notes/v0.5.0.md).

## [0.4.10] - 2026-09-30

### Changed

- Checked against Claude Code 2.1.285 (`routes/2.1.285.txt`); no routing change needed.
- `scripts/dev-env` drops an inherited chottag proxy, so a dev sandbox never routes through another chottag.
  See [release notes](docs/release-notes/v0.4.10.md).

## [0.4.9] - 2026-09-30

### Added

- `chottag statusline`, a segment for Claude Code's status line that shows whether the session
  goes through chottag, and `live sessions` in `chottag status`.
  See [release notes](docs/release-notes/v0.4.9.md).

### Changed

- The guides explain which sessions use chottag and how to move a running one over.
- CI actions: checkout v7.0.1, setup-go v7.0.0, goreleaser-action v7.2.3.

## [0.4.8] - 2026-09-30

### Changed

- docs/for-claude-code.md: install steps for the public repo, a check that chottag started, and a
  short setup talk with the user (which accounts, which one is pinned) before logging in.
  See [release notes](docs/release-notes/v0.4.8.md).

## [0.4.7] - 2026-09-30

### Changed

- The README's comparison table marks each cell ✅, ⚠️, ❌ or ❔ instead of plain yes/no.
  See [release notes](docs/release-notes/v0.4.7.md).

## [0.4.6] - 2026-09-29

### Changed

- The README says where the name comes from (the WWE hot tag) and how to say it: "See-Hot-Tag".
  See [release notes](docs/release-notes/v0.4.6.md).

## [0.4.5] - 2026-09-29

### Changed

- README: the first feature names the pain the pin solves (switching never
  breaks Remote Control, connectors or artifacts), and the comparison
  table leads with that pin. See [release notes](docs/release-notes/v0.4.5.md).

## [0.4.4] - 2026-09-29

### Changed

- README: the README names the remote pin and compares chottag with the
  best-known account switchers. See [release notes](docs/release-notes/v0.4.4.md).

## [0.4.3] - 2026-09-29

### Added

- [Install and manage chottag for a user](docs/for-claude-code.md): an
  ordered, copy-pasteable page written for a Claude Code session installing
  or managing chottag on someone's behalf, installing without a clone (a
  clone's own `CLAUDE.md` and hook block installer runs), covering the
  plugin, a private repo's git credentials, the terms go-ahead, and that
  only a session started after the install is routed.

## [0.4.2] - 2026-09-29

### Added

- Community files: `CONTRIBUTING.md` (issues and forks welcome, pull requests
  by invitation only), `CODE_OF_CONDUCT.md`, `SUPPORT.md`, `ROADMAP.md`,
  `NOTICE` (also in every release archive), this changelog, and issue forms
  for bugs, route drift and feature ideas. A pull request from anyone without
  write access is closed automatically, with a link to `CONTRIBUTING.md`.
- Getting started covers installing from a private repository (`gh auth
  login`, then a token-carrying `gh api` fetch of `install.sh`), for sharing
  chottag before it is public.

### Changed

- The README's policy note is now "Terms of use and risk": it quotes the
  Claude Code legal page's credential rule directly, says plainly what
  chottag's proxy does, and sends you to Anthropic's own terms to decide for
  yourself; no warranty. The FAQ's "Is this allowed?" adds which behaviours
  look least "ordinary", warns against switching to another account to get
  around a suspension, a ban or a hold, and points a held account at
  Anthropic's own review and appeal page.

### Fixed

- In a cmux terminal, a `claude` started through chottag skipped cmux's own
  claude wrapper, so cmux's session hooks never ran. chottag now hands the
  launch to cmux's wrapper once, with the proxy settings already in place.
- Connector calls no longer raise false route-drift notices at session
  start.

## [0.4.1] - 2026-09-29

### Changed

- The Claude Code plugin is now `chottag@c-hottag`: the plugin `chottag`
  from the marketplace `c-hottag`, and its skill runs as `/chottag:chottag`.
  A plugin installed under its earlier id has to be removed and installed
  again; the release notes list the commands.

### Fixed

- A `claude` launch with no daemon running could start a chain of processes.
  On macOS the shim saw itself as `…/bin/claude` and started the daemon
  under that name, so the new process ran as the shim and started another.
  The shim now starts the daemon from the resolved `chottag` binary, and
  never from a binary named `claude`. v0.4.0 is affected.

## [0.4.0] - 2026-09-29

The first release. [0.3.0] was never released, so its changes ship here too.

### Added

- Accounts in their own slots: `chottag login`, `logout`, `rename`, `rotate`
  and `adopt`. Your own `~/.claude` login is never changed.
- Per-request routing through a local HTTPS proxy on 127.0.0.1. Inference goes
  to the serving account (`chottag tag`, `chottag next`); remote-control
  sessions, artifacts and connectors go to the account that owns them, and new
  ones to the remote account (`chottag remote`). Routines follow the remote
  account.
- Usage and limits per account in `chottag status`, read from response
  headers, with a light usage poll as a fallback.
- Auto-switch, on by default: per-plan switch points (`chottag plan`), the
  `balanced` and `cache-optimize` modes, and one resend of a request refused at
  a limit. `chottag auto off` turns it off.
- Desktop notices on macOS for switches, limits, logins and route drift;
  `chottag notify off` turns them off.
- `chottag daemon`, `chottag doctor` and `chottag trace`.
- `chottag update`: check for, install or roll back a release from the repo
  you installed from (`install.sh --repo` picks it). Until the daemon
  restarts on the new version, `chottag status` and `chottag doctor` warn
  that it still runs the old one.
- `--json` on every account, status and settings command.
- The Claude Code plugin, whose skill drives chottag from a session.
- A user manual under `docs/`: getting started, how it works, a command
  reference checked against the code, configuration, auto-switch, updating,
  troubleshooting, FAQ, security, known limitations, uninstall, and a
  comparison with migration guides.
- `SECURITY.md`: private vulnerability reporting through GitHub, supported
  versions, and a summary of the threat model.

### Changed

- The project is c-hottag: the Go module is `github.com/HaiNNT/c-hottag` and
  the default repository `HaiNNT/c-hottag`. `chottag update` and `install.sh`
  refuse a repository that GitHub only redirects; point them at the new name.

### Fixed

- The daemon could miss a `chottag own` reassignment on Linux when the owners
  file was rewritten at the same size within one file-clock tick. It now also
  notices that the file was replaced.

### Security

- The local proxy requires a per-install secret from its callers. A caller
  without it gets `407 Proxy Authentication Required`, so restart every
  running Claude Code session after updating.
- The `claude` shim checks that the daemon holds this install's secret, with
  a challenge bound to the daemon's own port, before handing the secret over.
- Inside an intercepted tunnel, a request whose `Host` differs from the
  tunnel's target is refused with `421 Misdirected Request`.
- The local CA is limited to `anthropic.com`, `claude.ai` and `claude.com`,
  and a certificate is only made for the host a `CONNECT` targets.
  `chottag doctor` checks `ca.key`'s permissions.
- chottag refuses to start with a `ca.key` that others can read or a `ca/`
  that others can write; `chottag doctor --fix` repairs it.
- Releases from the public repository carry a build attestation.
  `chottag update` and `install.sh` verify it alongside the checksum, and
  fail closed.
- The daemon runs from a fixed working directory, and the plugin skill
  pre-approves only read-only commands and everyday account switches.

[0.5.1]: docs/release-notes/v0.5.1.md
[0.5.0]: docs/release-notes/v0.5.0.md
[0.4.10]: docs/release-notes/v0.4.10.md
[0.4.9]: docs/release-notes/v0.4.9.md
[0.4.8]: docs/release-notes/v0.4.8.md
[0.4.7]: docs/release-notes/v0.4.7.md
[0.4.6]: docs/release-notes/v0.4.6.md
[0.4.5]: docs/release-notes/v0.4.5.md
[0.4.4]: docs/release-notes/v0.4.4.md
[0.4.3]: docs/release-notes/v0.4.3.md
[0.4.2]: docs/release-notes/v0.4.2.md
[0.4.1]: docs/release-notes/v0.4.1.md
[0.4.0]: docs/release-notes/v0.4.0.md
[0.3.0]: docs/release-notes/v0.3.0.md
