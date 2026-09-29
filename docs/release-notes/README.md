# Release notes

One file per release, `v<version>.md`, used as that GitHub release's notes. `release.yml` passes it to GoReleaser with `--release-notes`.

## Cutting a release

1. **Write the notes and the changelog entry**: the notes as
   `docs/release-notes/v<version>.md` (`test/release` fails until the file
   exists for the version in `plugin.json`), and the version in
   `CHANGELOG.md`, headed `## [<version>] - Unreleased`, with its changes
   under Added, Changed, Deprecated, Removed, Fixed or Security, and a link
   line `[<version>]: docs/release-notes/v<version>.md` at the bottom.
   `test/consistency` fails until every notes file has an entry.
2. **Bump the version, in one commit**: set `plugin/.claude-plugin/plugin.json`
   to `<version>` and replace CHANGELOG.md's `## [<version>] - Unreleased`
   with the date, as `YYYY-MM-DD` (`scripts/release bump <version>` does
   both, in the same commit). Claude Code notices a plugin update by this
   version, so every release gets a new one; the CHANGELOG entry is dated in
   the commit that bumps plugin.json.
3. **Merge to `main`** and let CI pass. A red `lint` job from a new stdlib
   vulnerability advisory does not hold a tag: every other job must be
   green, and the `go` line in `go.mod` is raised in a follow-up.
4. **Tag and push** (the maintainer does this): `git tag v<version> && git push origin v<version>`, tagging the commit from step 2 (or whatever commit CI on `main` last checked).

`release.yml` then:
- runs the gate;
- checks that the tag equals `v` plus `plugin.json`'s version;
- builds darwin and linux on amd64 and arm64 with GoReleaser, using these notes.

The Homebrew tap is written but not uploaded (`brews.skip_upload`): there is
no tap repo yet for Homebrew to install from.
