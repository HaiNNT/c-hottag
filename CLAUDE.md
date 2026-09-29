# c-hottag (`chottag`)

A Go single-binary Claude Code account switcher: per-account
`CLAUDE_CONFIG_DIR` slots, plus a local HTTPS intercepting proxy that picks
each request's bearer. Module `github.com/HaiNNT/c-hottag`; the CLI is
`chottag`. Not affiliated with or endorsed by Anthropic.

A maintainer may keep extra, private instructions in an untracked
`CLAUDE.local.md`. A contributor needs nothing from it.

Issues are welcome; pull requests are by invitation only. `CONTRIBUTING.md`
says how to file a good issue and how an invited change lands.

## Layout

- `cmd/chottag/`: the binary's entry point.
- `internal/cli/`: every command, the daemon, `setup`, `update`, and the
  `doctor` and `status` wiring. Its `usage` text is the command list the
  docs tests check.
- `internal/proxy/`, `internal/proxyauth/`, `internal/ca/`: the loopback
  proxy, its caller secret and health proof, and the local CA.
- `internal/router/`, `internal/selector/`, `internal/owners/`: which account
  a request carries, and which account owns each claude.ai object.
- `internal/shim/`, `internal/session/`: the `claude` shim and the sessions
  it launched.
- `internal/store/`, `internal/status/`, `internal/creds/`,
  `internal/tokens/`, `internal/refresh/`: `state.json`, `status.json`, slot
  logins and tokens.
- `internal/autoswitch/`, `internal/usage/`, `internal/usagepoll/`,
  `internal/notify/`: auto-switch, usage and notifications.
- `internal/tracelog/`, `internal/tracesum/`, `internal/redact/`: redacted
  traces and logs.
- `internal/doctor/`, `internal/daemonlock/`, `internal/fsutil/`,
  `internal/rotate/`, `internal/exit/`: the rest.
- `internal/doccheck/`: Markdown and JSON-shape helpers for the tests that
  check the docs. Test support only: no product package imports it.
- `docs/`: the user manual, from `docs/index.md`. `docs/commands.md` is
  checked against the code.
- `plugin/`, `.claude-plugin/`: the Claude Code plugin and its marketplace.
  Their machine ids (`chottag@c-hottag`: plugin `chottag`, marketplace
  `c-hottag`, skill `chottag`) stay as they are: renaming them breaks
  existing installs.
- `test/`: repo-level tests.
  - `consistency`: README, SKILL.md, the `docs/` pages, install.sh,
    `.goreleaser.yaml` and the code agree on names, flags and defaults.
  - `release`: CI and the release config match the gate below.
  - `plugin` and `guardhome`.
  - `installsh`: install.sh against a fake `gh`.
  - `leakscan` and `export`: the leak scan and the snapshot export.
  - `bunsmoke`: skips without Bun, unless `CHOTTAG_REQUIRE_BUN=1` (CI sets it).
- `install.sh`, `.goreleaser.yaml`, `.github/workflows/`: install and
  release. `docs/release-notes/README.md` is the release procedure.
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SUPPORT.md`, `ROADMAP.md`,
  `CHANGELOG.md`, `NOTICE` and the rest of `.github/` (issue forms, the pull
  request template and `pr-policy.yml`, CODEOWNERS, Dependabot): the
  community files. `test/consistency` and `test/release` check them.
- `scripts/`: `leak-scan` and `manifest-select` (CI runs them over the paths
  `public-manifest.txt` selects), `export-public`, and `dev-env` (the dev
  sandbox; see "Dev and prod").
- `routes/`: recorded route tables per Claude Code version, with ids
  collapsed.
- `.claude/hooks/guard-home.py`: a PreToolUse hook that blocks installer
  runs and writes aimed at the real HOME.

## Rules

- The primary branch is `main` (never `master`). Branch features off `main`.
- Product code uses the Go standard library only: no new dependencies.
- The gate, before every commit. CI (`.github/workflows/ci.yml`) runs exactly
  these lines, and `test/release` fails if the two differ:
  ```sh
  test -z "$(gofmt -l .)"
  go vet ./...
  go test ./... -race -count=1 -shuffle=on
  go test -tags chottag_fakeusage -race -count=1 -shuffle=on ./internal/cli/
  ```
  A shuffled failure prints `-test.shuffle N`; replay it with `-shuffle=N`.
- Never write to Home (`~/.claude`, `~/.claude.json`). Never log, print or
  persist tokens or request/response bodies. Never copy a token out of a
  slot. Don't probe credentials outside the product flow.
- Test only in a sandbox:
  - `HOME` and `CHOTTAG_HOME` come from `t.TempDir()`;
  - never run `install.sh`, `chottag setup` or `uninstall`, or a built
    `chottag`, against the real HOME (the guard-home hook enforces part of
    this for agents);
  - tests never bind port 47821, run the real `gh`, `claude` or
    `osascript`, or use the network or the Keychain.
- New external effects go behind package-level func vars, with panicking
  defaults in `TestMain` (see `internal/cli/invariants_test.go`). A
  production timeout is never a test's pass/fail bound: tests wait on
  events.
- No personal data anywhere in the tree. Fixtures use `alice@example.com`,
  `bob@example.com`, `Acme` and `/Users/alice`. `scripts/leak-scan` runs in
  the gate and in CI, and fails on home paths, real-looking emails and
  session links.

## Dev and prod

- A maintainer's daily chottag (`~/.chottag`, port 47821, the `~/.zshrc`
  PATH block) is prod. A session in this repo never changes it: the guard
  denies a non-read-only chottag invocation unless it targets a non-prod
  `CHOTTAG_HOME`, or runs through `scripts/dev-env` or `scripts/release`.
- Manual runs use `scripts/dev-env` instead: its own home, its own port
  (47850 by default), its own logins -- prod slots are never copied in.
- Manage prod itself from a Claude Code session outside this repo, with the
  `chottag` plugin skill or a terminal.
- Test the plugin with `claude --plugin-dir ./plugin` instead of installing it.

## Keeping docs true

- A command, flag, JSON field or error code that changes updates
  `internal/cli/cli.go`'s usage text, `README.md`,
  `plugin/skills/chottag/SKILL.md` and `docs/commands.md` in the same
  change. `test/consistency`, `test/plugin` and `internal/cli`'s docs test
  fail on a name that doesn't exist or isn't documented.
- A user-visible change gets a line in the next
  `docs/release-notes/v<version>.md`, and in that version's `CHANGELOG.md`
  entry.
- A change that alters a design decision updates the docs that describe it
  in the same change, so no doc describes code that no longer exists.
- A new tracked file is listed in `public-manifest.txt`, as public or as
  `!` private; `test/leakscan` fails otherwise.
