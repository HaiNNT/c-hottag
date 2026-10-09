# Changelog

Every notable change to c-hottag, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each version's
heading links to its full release notes in `docs/release-notes/`. A version
reads "Unreleased" until it is dated in the commit that bumps plugin.json
to it.

## [0.10.2] - 2026-10-09

### Changed

- The chottag skill and the guide for Claude Code explain the unknown-owner and route-drift notices, `lostSessions` and session names; the skill runs the read-only `chottag sessions` and `chottag names` without asking.

### Removed

- `chottag names model` (and `/ct names model`) is removed: it exits 2 and changes nothing. Each topic was a separate `claude -p` call, too slow to finish in time and possibly charged as extra usage. A saved `model` setting, or `CHOTTAG_NAME_SESSIONS=model`, now means `on`. chottag no longer runs a model or `claude -p`.

### Fixed

- The `proxy.jsonl` record of a refused or failed request gains `refusedType` and `errType`, the upstream error type (a fixed list of Anthropic's, else `other`; never the message), and the `login was refused` log line names it: `(401 authentication_error)`.
- A refused login on `/v1/messages` or `POST /api/oauth/validate` no longer sets `drift: true` on its record when the resend on your own login is answered with another status; it was never counted by `route-drift`.
- Hardening: the retry after a 401 never reuses the token that was just refused, waits for a login renewal in flight, and tries once more if a newer token appeared meanwhile.

  See [release notes](docs/release-notes/v0.10.2.md).

## [0.10.1] - 2026-10-09

### Security

- Built with Go 1.27.2, which fixes the `net/http` advisories GO-2026-6611, GO-2026-6612, GO-2026-6613 and GO-2026-6617 in v0.10.0's binaries.

  See [release notes](docs/release-notes/v0.10.1.md).

## [0.10.0] - 2026-10-09

### Added

- `chottag sessions` lists the Claude Code sessions lost together when cmux quit or the Mac crashed, and `chottag resume` relaunches them, each in its own directory, pool and cmux tab (`--pick` chooses, `--print` only prints). The shim now journals each session it launches, for 7 days, with no tokens.
- `chottag name-session`, the plugin's `UserPromptSubmit` hook, names each session from its first prompt after its branch (or `<folder> HH:MM`), then adds Claude Code's generated title; `chottag sessions` and `resume --pick` gain a `TITLE` column and `--json` a `title`. Only `ai-title` and `custom-title` records are read, and no title is logged or stored. `chottag names [on|model|off]` shows or sets it (stored in `state.json`, shown by `chottag status`, `names` in `status --json`; `CHOTTAG_NAME_SESSIONS` overrides); `chottag names model` adds a Haiku-made topic (`<folder> · <topic>`) at the first prompt of a session with no branch, falling back to `<folder> HH:MM` on any error, and the plugin hook timeout is now 10 seconds.
- `chottag status` shows `N sessions were lost at 18:01: chottag resume` while a batch from the last 24 hours is waiting, and `lostSessions` in `--json`.
- `chottag resume` only acts on sessions lost in the last 24 hours (`sessions --all` lists older ones), never types into a busy tab, prints and marks nothing when cmux is installed but not answering, and refuses a second run at the same time (`resume_busy`). Sessions from before a reboot always count as lost.
- Owner discovery: a `GET` or `HEAD` for an object whose owner chottag does not know, refused by the `remote` account with a 403 or 404, is tried on the pool's other accounts (up to 8) and the first account that opens it is recorded as the owner.

### Changed

- `chottag status` shows a reading's age only once it is older than the account's poll interval plus 15 minutes, shows the `↻` reset as time left only under 5 hours (then `23:59`, `Thu 08:00` or `Oct 9 18:00`), and never prints `-` as STATE: it reads `ok`, `stale` or `no reading`. `--json` and the status line are unchanged.

### Fixed

- A refused request for an object whose owner chottag does not know is no longer counted as route drift. It gets its own `an artifact's owner is unknown` notice, at most once an hour per account, and the route-drift notice text no longer says the request was "resent unchanged".

  See [release notes](docs/release-notes/v0.10.0.md).

## [0.9.4] - 2026-10-06

### Changed

- `chottag status` shows when each 5h and 7d window resets, as in `28% ↻ 3h20m` or `99% (16h ago) ↻ Fri 18:00`, when the reset time is known and ahead.

### Fixed

- A daemon left in a dead macOS login session (after a logout or a WindowServer crash) no longer marks every account `needs-login`: the accounts read `stale`, with no notice, and the daemon exits a minute or two later so the next `claude` starts a fresh one. The first `needs-login` read of a slot logs its `exit status N`.
- A `passthrough: token needs-login` or `token stale` mark no longer outlives a fresh login: a re-login, a renewal or a working usage poll clears it, and a daemon start drops the previous daemon's marks.

  See [release notes](docs/release-notes/v0.9.4.md).

## [0.9.3] - 2026-10-06

### Fixed

- A 404 on `POST /v1/messages` is no longer treated as route drift. Claude Code 2.1.290's Message Threads continue a thread that only the account that created it holds, and Claude Code resends the turn as a create when it gets a 404. chottag used to refresh the account, retry, and send the turn again on Home's login (which could run it on Home's quota), add about 4 seconds, and count route drift with a notice. Now the 404 goes back unchanged, and `proxy.jsonl` marks the record `passed404`. A 401 or 403, and a 404 on any other route, are unchanged.
- Auto-switch no longer moves onto an account whose login is gone. A poll, refresh or warm pass that finds no login marks it `needs-login` (one notice), and every rotating account's token is kept warm.
- Auto-switch checks its target before a swap: it refreshes a stale token and polls an old reading, bounded and never blocking responses.
- Idle accounts' usage is polled about every 30 minutes (2 hours for rotation-off accounts), with 429 backoff, so it no longer goes stale.

### Changed

- `chottag status` shows an older reading with its age (`45% (2h ago)`), or `unknown (reset since)`, instead of `unknown`.

  See [release notes](docs/release-notes/v0.9.3.md).

## [0.9.2] - 2026-10-04

### Fixed

- Remote Control no longer drops with "signed-in claude.ai account or organization changed" after the serving account moves or the daemon restarts. A session's `POST /api/oauth/validate` is answered by the same account for the session's life, remembered in `run/validate-sessions.json` (session ids, account names and emails, plus a bounded list of recently dropped entries; never a token; at most 500 sessions, 7 days). A stale token on that account is refreshed rather than given up on, and the accounts of sessions used in the last day are kept fresh. When the account is removed, logged in again as another email, out of the pool, rotation-off (outside default), needs login or cannot be refreshed, the serving account answers and `daemon.log` says `validate for session <sid8> moved from C to D: C <reason>`. A session started before the update can drop once more on its first validate.

  See [release notes](docs/release-notes/v0.9.2.md).

## [0.9.1] - 2026-10-03

### Added

- `chottag update` and `update --check` end with a "What's new" block: the lead paragraph of each release newer than the one you ran (at most 5, newest first, about 400 characters each, with its link). `--json` has `whatsNew` (`version`, `summary`, `url`). A failed fetch only drops the block.
- `chottag update` (after an install) and `update --check` (when a newer release exists) print the plugin step: `claude plugin marketplace update c-hottag`, `claude plugin update chottag@c-hottag`, then `/reload-plugins`. `--json` has `plugin` (`commands`, `note`). chottag does not run `claude`; the skill and the Claude Code guide have Claude Code ask you, run the commands on a yes, and relay the summary.
- The mod knows its own version. When chottag is newer than the plugin it toasts once per version and the card says `plugin X · chottag Y: update the plugin`.

### Fixed

- The status card has one empty line above it, instead of sitting right under the output above.
- A request sent as account C to `/v1/messages` or `POST /api/oauth/validate` and refused with a 401 or 403 is no longer counted as route drift: it gets its own notice (`chottag: C's login was refused (401)`), at most hourly per account, a daemon log line, and a `refused` field in `proxy.jsonl`. Route drift is now for object and remote routes, unlisted routes and 404s.
- A request refused while its account's token refresh runs, or within 30 seconds of a renewal, retries with the new token instead of going out on Home's login.
- Every `daemon.log` line starts with the local time (`2006-01-02 15:04:05`).

  See [release notes](docs/release-notes/v0.9.1.md).

## [0.9.0] - 2026-10-03

### Added

- The plugin's mod (Claude Code 2.1.287 or newer): a status card above the prompt (serving and remote accounts, both usage windows with their resets in local time), notices for a switch, an update or a remote account that needs a login, the `/ct` commands (`status`, `next`, `tag NAME`, `pool`, `help`), and a guard that stops `/login` and `/logout` in a chottag session.
- `chottag statusline --json` has `limited`, `lastSwitch`, `remote`, `remoteToken`, `fiveHourResetsAt`, `sevenDayResetsAt` and `nearWindow`.
- The daemon log says what every token refresh did (`refresh C: renewed, expires in 7h59m (request, 5.2s)`, `claude exited 0 but the token is still expired`, `failed: <reason>`), the same line at most every 10 minutes.
- A refresh that exits 0 at least three times in a row, over at least 10 minutes, without renewing the token makes the account `needs-login` (`chottag login C`), with a notice; the daemon still probes it every 15 minutes, and a renewal lifts it. A locked Keychain is retried every 30 seconds for 5 minutes, then with the usual backoff.
- The daemon keeps each pool's serving account's token fresh too, and runs a warm pass about 10 seconds after a wake from sleep, so fewer prompts go out on Home's own login.

### Changed

- `chottag statusline`'s `↻` shows the time left (`↻ 42m`, `↻ 3h20m`, `↻ Mon 18:00`, `↻ Oct 9 18:00`) instead of a clock time.

### Fixed

- `token: stale` on `chottag status` clears when the token renews or a request goes out as that account again, instead of staying until a passthrough replaced it.
- The token refresh runs without the daemon's own auth and session variables (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, the Bedrock and Vertex switches, `CLAUDE_CODE_SESSION_KIND`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDECODE`, `CLAUDE_CODE_CHILD_SESSION`), so they cannot override the slot's login.
  See [release notes](docs/release-notes/v0.9.0.md).

## [0.8.5] - 2026-10-02

### Fixed

- A Remote Control, connector, artifact or routine request whose remote or owner account has a stale token never goes out on Claude Code's own (Home) login: chottag waits up to 15 seconds for a refresh, then answers a 503 naming the account (`chottag login A`), with any number of pools (when a remote account is set). The safety net never resends such a request on Home's login, so an object only Home's login can see now gets the remote account's 403/404 (`chottag remote <account>`, see known limitations).

### Added

- The daemon keeps every pool's remote account's token fresh (refreshed when 6 minutes or less are left; a needs-login account is skipped and logged once).

### Changed

- The daemon log words a serving request sent on Home's own login and a refused remote request, each at most once a minute per account.
  See [release notes](docs/release-notes/v0.8.5.md).

## [0.8.4] - 2026-10-02

### Fixed

- `chottag login`, `logout`, `adopt` and the daemon's token refresh run the real `claude` (resolved from PATH, skipping chottag's own `bin/`), never chottag's shim, so an account's slot can no longer record the serving account's email.
  Reported in [#2](https://github.com/HaiNNT/c-hottag/issues/2).
- `adopt` no longer copies another account's email into an account when its email would move onto a serving account's with the org unchanged (warning `identity_suspect`), and `chottag doctor` has a new info row `identities` that lists accounts sharing an email (expected for one login in several orgs).
  See [release notes](docs/release-notes/v0.8.4.md).

## [0.8.3] - 2026-10-02

### Changed

- `claude auth ...` and `claude setup-token` skip chottag on their own, without `CHOTTAG_BYPASS=1`, and no longer carry chottag's proxy variable, so a Home login or logout cannot pick up a chottag account's token.
  See [release notes](docs/release-notes/v0.8.3.md).

## [0.8.2] - 2026-10-02

### Changed

- The desktop notice for a new release is posted by whichever check finds it first (the daemon's or `chottag update --check`), once per version, even across a daemon restart.
- The daemon checks for a new release about every 6 hours (four requests a day) instead of daily.
  See [release notes](docs/release-notes/v0.8.2.md).

## [0.8.1] - 2026-10-01

### Changed

- The README comparison's two new columns say what users get: "Best prompt-cache use with more than 3 accounts and parallel sessions" and "Isolated work and personal accounts"; `docs/comparison.md` explains the more-than-three threshold.
  See [release notes](docs/release-notes/v0.8.1.md).

## [0.8.0] - 2026-10-01

### Added

- Pools: `chottag pool` (list), `pool add`, `pool join`, `pool leave` and `pool rm`. A pool is a set of accounts with its own serving account, remote account, policy and pin; an account can be in several. A session picks its pool with `CHOTTAG_POOL=work claude` (an unknown name exits 2 and never falls back to `default`), and is served only by that pool's accounts. `login --pool`, `tag`, `next`, `remote` and `policy` take `--pool`.
- A pool that cannot serve a request fails closed: with more than one pool, chottag answers it with a 503 naming the pool instead of sending it on Home's own login.
- `shared_account` warns when an account is in more than one pool: they share its usage limit. Pools need a 0.8.0 daemon: `pool add` refuses (`daemon_predates_pools`, exit 2) while an older one runs, `pool join` warns, and once a pool exists the shim refuses every session against such a daemon until `chottag daemon restart`.
- `status` shows a `POOLS` column and a line per pool (`pools[]`, `shared` and `auto.pools` in `--json`), `statusline` shows `[work]` (`pool` in `--json`), notices name the pool, and `doctor`'s `roles` row checks every pool.
- The README comparison table gains a "Separate account pools" column and a "Spreads parallel sessions over accounts" column, sourced in `docs/comparison.md`.
- The agent install guide's setup question 7, "Do you keep work and personal accounts apart?".
  See [release notes](docs/release-notes/v0.8.0.md).

### Changed

- `state.json` is `"version": 2` while a pool other than `default` exists; `chottag update --version` below 0.8.0 is refused then (`pools_block_rollback`).
- The README recommends letting Claude Code install and set up chottag.
  See [release notes](docs/release-notes/v0.8.0.md).

## [0.7.1] - 2026-10-01

### Changed

- The agent install guide's setup questions now cover each account's plan (`max?`), `chottag policy spread` and automatic updates, and apply the answers in the right order; the plugin skill explains a `max?` plan.
  See [release notes](docs/release-notes/v0.7.1.md).

## [0.7.0] - 2026-10-01

### Added

- `chottag policy [serial|spread]`: `spread` places each new session on the account with most headroom and keeps it there. Under it, `chottag tag NAME` pins new sessions, `chottag tag --unpin` clears the pin, and `chottag next` is refused (`spread_next`); `bad_policy` for a wrong value.
- The spread engine: a session is placed on its first request and stays there; it moves at a switch point, on a limit, or when its prompt cache is cold and a move clearly helps, at most once in 10 minutes. Placements survive a daemon restart (`run/placements.json`), and a notification says when sessions moved.
- `chottag status` shows `policy: spread` and the `pin` under spread (`policy` and `pin` in `--json`, left out under `serial`).

- `state.json` keeps top-level and per-account keys this chottag does not know and writes them back unchanged. `chottag policy spread` and `tag NAME` under spread warn (`daemon_predates_spread`) when the running daemon is older: restart it first. Turning spread on keeps running sessions on their accounts; rolling back below 0.7.0 returns to serial and forgets placements.

### Fixed

- `chottag update`, `setup` and `adopt` no longer clear a real needs-login: adopt re-stamped every account's `loggedInAt`, so a broken login looked fixed. Only `chottag login`, or `adopt` finding a changed email or org in the slot, advances it now.
  See [release notes](docs/release-notes/v0.7.0.md).
- The daemon's restart-when-idle state in `run/restart.json` is no longer deleted by `chottag status` or the daemon itself, so its once-per-version notices and hourly attempt limit hold across restarts.
  See [release notes](docs/release-notes/v0.7.0.md).

### Changed

- When `chottag update` defers the daemon restart and the running daemon can't restart itself (it predates 0.6.0, auto-restart is off, or it is newer than what was installed), it now says so plainly, `the running daemon (0.5.0) can't restart itself: run chottag daemon restart once when convenient`, and `update --json` has `selfRestart`. The plugin skill tells Claude Code to run `chottag daemon restart` to finish the update.
  See [release notes](docs/release-notes/v0.7.0.md).
- The plugin skill, `docs/for-claude-code.md` and the README now cover the update check, `update_in_progress`, the restart-when-idle markers, `own` and `adopt`; a test keeps the skill and `docs/commands.md` complete against the usage text.
  See [release notes](docs/release-notes/v0.7.0.md).

## [0.6.0] - 2026-10-01

### Added

- Each `claude` session gets its own proxy credential; the older `chottag:<secret>` form still works, and is what a daemon from before 0.6.0 gets until `chottag daemon restart`.
- `chottag status` lists the live sessions and the account each one uses (`sessions` in `--json`).
- `chottag statusline` shows this session's account (`account` in `--json`).
- `chottag update` backs up state.json, and setup backs up your shell rc file before changing it, into ~/.chottag/backups/. The state.json backup starts with updates run by 0.6.0 or later.
- The chottag mark in the README and the manual.
- `chottag status` and `chottag statusline` show an available update (`update`, `updates` and `updateAvailable` in `--json`); `chottag update --check` records it too, and reads GitHub directly, with no `gh`.
- `chottag update` takes a lock (`update_in_progress`), has `--no-restart`, and `--auto-check on|off` and `--auto-install on|off` for the daemon's update check and opt-in automatic install.
- The daemon restarts itself onto an installed newer version when the proxy is idle (no request in flight, none in the last 5 minutes), checking every minute and on wake from sleep. `status` and `statusline` show a pending restart (`daemon.restartPending`, `restartPending`, ` · ⟳<v>`). On by default; `chottag update --auto-restart on|off`.
  See [release notes](docs/release-notes/v0.6.0.md).

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

[0.10.2]: docs/release-notes/v0.10.2.md
[0.10.1]: docs/release-notes/v0.10.1.md
[0.10.0]: docs/release-notes/v0.10.0.md
[0.9.4]: docs/release-notes/v0.9.4.md
[0.9.3]: docs/release-notes/v0.9.3.md
[0.9.2]: docs/release-notes/v0.9.2.md
[0.9.1]: docs/release-notes/v0.9.1.md
[0.9.0]: docs/release-notes/v0.9.0.md
[0.8.5]: docs/release-notes/v0.8.5.md
[0.8.4]: docs/release-notes/v0.8.4.md
[0.8.3]: docs/release-notes/v0.8.3.md
[0.8.2]: docs/release-notes/v0.8.2.md
[0.8.1]: docs/release-notes/v0.8.1.md
[0.8.0]: docs/release-notes/v0.8.0.md
[0.7.1]: docs/release-notes/v0.7.1.md
[0.7.0]: docs/release-notes/v0.7.0.md
[0.6.0]: docs/release-notes/v0.6.0.md
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
