# Roadmap

Where c-hottag is going. It has no dates and makes no promises: the project
is maintained by one person in their spare time. To suggest something, open a
**Feature idea** from [the issue chooser](https://github.com/HaiNNT/c-hottag/issues/new/choose).

## Now

- The first public release: the source, attested releases, the user manual
  under `docs/`, and the community files.

## Next

- Linux used for real, not only built and tested in CI.
- Run the route check on each new Claude Code version as a routine release
  step, and in CI where possible.

## Later

- A daemon that restarts itself after an update, at a quiet moment with no
  request in flight, so `chottag update` never needs a manual
  `chottag daemon restart`.
- A Homebrew tap. The formula is written; there is no tap repository yet.
- The manual under `docs/` published as a website.
- Telling a `max5x` account from a `max20x` one on its own, if Claude Code
  ever reports the plan size.

## Not planned

- Windows.
- Routing Claude Code's IDE extensions or the Desktop app.
- Sharing accounts between people, or a machine that several people use.
- Community pull requests without an invitation (see
  [CONTRIBUTING.md](CONTRIBUTING.md)).
