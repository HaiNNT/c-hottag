# Contributing to c-hottag

Thank you for your interest. c-hottag is maintained by one person in their
spare time, so this page says plainly what helps and what doesn't.

- **Issues are welcome.** Bug reports, route drift and feature ideas are the
  main way to take part.
- **Forks are welcome.** The code is Apache-2.0: change it as you like in your
  own fork.
- **Pull requests are by invitation only.** There is no time to review
  unsolicited ones. A pull request from anyone without write access to this
  repository is closed automatically, with a link to this page.

Everyone who takes part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Filing a good issue

Open an issue from [the issue chooser](https://github.com/HaiNNT/c-hottag/issues/new/choose)
and pick the form that fits: **Bug report**, **Route drift** (a Claude Code
update changed which account a request should use) or **Feature idea**.
A good report has:

- the versions: `chottag version` and `claude --version`, and your OS and
  CPU (macOS or Linux, arm64 or amd64);
- what you ran, what you expected, and what happened instead;
- the output of `chottag doctor`, and for route drift the output of
  `chottag trace summarize` after `chottag trace on` (see
  [Troubleshooting](docs/troubleshooting.md#route-drift-after-a-claude-code-update)).

Every form has a required checkbox: the issue holds **no personal data or
tokens**. Before you paste anything, remove emails, account and org names,
home paths, session links, and every token, `proxy.secret` or `ca.key`.
Call your accounts A and B.

Not an issue:

- a security problem: report it privately, as [SECURITY.md](SECURITY.md)
  describes;
- your Claude account, plan, billing or usage limits: ask Anthropic support
  at https://support.claude.com. c-hottag can't see or change them.

## Pull requests, by invitation

If you have a fix or a feature in mind, open an issue that describes it
first. If the maintainer wants the change, they will invite you: they add
you as a collaborator, or reopen your closed pull request.

This public repository receives curated snapshots of the project, so an
invited change is applied to the development tree as a patch, keeps you as
its author there, and ships in the next snapshot. The release notes credit
you by name.

By sending a change you agree that it is licensed under Apache-2.0, like
the rest of the project (section 5 of [LICENSE](LICENSE)).

## Building and testing

You need Go at the version in `go.mod`, on macOS or Linux.

```sh
go build ./cmd/chottag
```

Before every commit, run the gate. CI runs exactly these four lines:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test ./... -race -count=1 -shuffle=on
go test -tags chottag_fakeusage -race -count=1 -shuffle=on ./internal/cli/
```

The tests run in a random order. A failure prints `-test.shuffle N`;
replay that order with `-shuffle=N`.

To try a build by hand, use `scripts/dev-env` (`scripts/dev-env build`,
then `scripts/dev-env shell`). It keeps its own home, its own port and its
own logins, so it never touches the chottag you use every day. Try the
plugin with `claude --plugin-dir ./plugin` instead of installing it.

[CLAUDE.md](CLAUDE.md) is the full list of rules for anyone changing the
code, person or AI agent. The ones that matter most:

- Product code uses the Go standard library only: no new dependencies.
- Never write to `~/.claude` or `~/.claude.json`, and never log, print or
  store a token or a request or response body.
- A new command, flag, JSON field or error code updates the usage text,
  `README.md`, the plugin skill and `docs/commands.md` in the same change;
  a user-visible change gets a line in the next release notes under
  `docs/release-notes/`.
- A new tracked file gets a line in `public-manifest.txt`.

## Test safety rules

chottag manages real logins, so its tests run in a sandbox, always:

- `HOME` and `CHOTTAG_HOME` come from `t.TempDir()`.
- Never run `install.sh`, `chottag setup`, `chottag uninstall` or a built
  `chottag` against your real home directory.
- Tests never bind port 47821, run the real `gh`, `claude` or `osascript`,
  or use the network or the Keychain.
- A new external effect goes behind a package-level function variable,
  with a default that panics under `TestMain`, so a test that forgets to
  replace it fails loudly.
- A test waits on events, never on a production timeout.
- Fixtures use `alice@example.com`, `bob@example.com`, `Acme` and
  `/Users/alice`. `scripts/leak-scan` runs in the gate and in CI, and fails
  on home paths, real-looking emails and session links.
