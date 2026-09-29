# Getting started

## What you need

macOS (the supported platform) or Linux (builds and passes CI, but has not
been used for real), on amd64 or arm64. Claude Code installed. And either
`gh` logged in with access to this repo, or a clone and Go (the version in
`go.mod`).

## Install

From a clone:

```sh
gh repo clone HaiNNT/c-hottag
cd c-hottag
./install.sh
```

Without a clone, through `gh`:

```sh
gh api -H 'Accept: application/vnd.github.raw' repos/HaiNNT/c-hottag/contents/install.sh | sh
```

Pass `--version vX.Y.Z` to install a specific release, or `--repo OWNER/NAME`
to install from a fork; both go through `sh -s -- …` when you're piping:

```sh
./install.sh --version vX.Y.Z --repo OWNER/NAME
# piped: … | sh -s -- --version vX.Y.Z --repo OWNER/NAME
```

`install.sh` downloads the release for your machine with `gh`, checks it
against the release's `checksums.txt`, and — from v0.4.0 on the public
repo — verifies its build attestation. Without a usable release, it falls
back to building the clone it sits in with Go. Either way the binary lands
in `~/.chottag/versions/<version>/`, the source it came from is recorded in
`install.json`, and `chottag setup` runs at the end. Exit 2 means a bad
argument; exit 1 means the install itself failed. Install into another home
with `CHOTTAG_HOME` set.

### If the repository is private

An invited user needs `gh auth login` with access to the private repo first.
A private repo's raw URL needs your GitHub token; `gh api` supplies it:

```sh
gh api -H 'Accept: application/vnd.github.raw' repos/HaiNNT/c-hottag/contents/install.sh | sh -s -- --repo HaiNNT/c-hottag
```

`--repo HaiNNT/c-hottag` is already `install.sh`'s default; the flag above
makes it explicit, and is what you change to install from another private
repo. `chottag update` then follows that repo (it is recorded in
`install.json`).

The two commands in [The plugin](#the-plugin) work unchanged: Claude Code
clones the marketplace with your git credentials, so `git clone` of the
repo must work in your terminal (`gh auth login` offers to set that up, or
run `gh auth setup-git`). Background auto-update, if you turn it on, uses
that same stored credential; a `GITHUB_TOKEN` in the environment alone is
not enough.

## Install by hand

Pick your OS and CPU in the asset name:

```sh
gh release download vX.Y.Z --repo HaiNNT/c-hottag \
  --pattern 'chottag_X.Y.Z_darwin_arm64.tar.gz' --pattern checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
gh attestation verify chottag_X.Y.Z_darwin_arm64.tar.gz --repo HaiNNT/c-hottag
tar -xzf chottag_X.Y.Z_darwin_arm64.tar.gz chottag
mkdir -p ~/.chottag/versions/X.Y.Z && mv chottag ~/.chottag/versions/X.Y.Z/
~/.chottag/versions/X.Y.Z/chottag setup
```

`go install github.com/HaiNNT/c-hottag/cmd/chottag@latest` also works, once
the repo is public: it reports its version as `dev`, and you still need to
run `chottag setup` afterwards. `install.sh` is the recommended path either
way — it verifies what it downloads and keeps old versions around.

## What setup changes

`chottag setup` creates the `~/.chottag` home tree, a local CA, the
`bin/chottag` and `bin/claude` links, and one fenced PATH block in
`~/.zshrc` or `~/.bashrc`. Nothing else changes outside the home. It never
uses `sudo`, and it never touches `~/.claude`. Open a new shell afterwards,
so the PATH block takes effect.

## The plugin

For a Claude Code session to drive chottag directly:

```sh
claude plugin marketplace add HaiNNT/c-hottag
claude plugin install chottag@c-hottag
```

The skill it installs runs as `/chottag:chottag`.

## Your first accounts

```sh
chottag login A     # opens a browser: log in to the first account
chottag login B     # and the second
chottag tag A       # A serves inference
chottag remote B    # new remote-control sessions, artifacts, routines belong to B
claude              # Claude Code, through chottag (the daemon starts by itself)
chottag status      # usage and limits per account
chottag doctor      # check the install; --fix repairs what it safely can
```

`chottag status` looks like this (values shown here are examples):

```text
serving: A   remote: B

  NAME   PLAN    ORG    5h    7d    STATE
  A      max5x   Acme   42%   18%   ok
  B      pro     Acme   0%    0%    ok
auto: balanced
```

## Next

- [How it works](how-it-works.md)
- [Auto-switch](auto-switch.md)
- [Commands](commands.md)
- [Troubleshooting](troubleshooting.md)
