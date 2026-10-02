# Roadmap

Where c-hottag is going. It has no dates and makes no promises: the project
is maintained by one person in their spare time. To suggest something, open a
**Feature idea** from [the issue chooser](https://github.com/HaiNNT/c-hottag/issues/new/choose).

## Shipped recently

- A status line for Claude Code (`c»`), and a cmux sidebar pill (0.5).
- A daemon that restarts itself after an update, when no session is busy (0.6).
- **Spread:** each new session goes to the account with the most room and
  stays there, so prompt caches stay warm (0.7).
- **Pools:** isolated work and personal accounts, each with its own remote
  account; a session picks its pool when it starts (0.8).

## Now

- A desktop notice the first time a new version is found, and a check four
  times a day.

## Next

- Linux used for real, not only built and tested in CI.
- Run the route check on each new Claude Code version as a routine release
  step, and in CI where possible.
- **A macOS app.** Notifications carrying the chottag logo (macOS credits
  plain script notifications to Script Editor, so they can't carry it), and
  a UI for people: a menu bar view of accounts, limits and pools, then
  managing them.

## Later

- A Homebrew tap. The formula is written; there is no tap repository yet.
- The manual under `docs/` published as a website.
- Telling a `max5x` account from a `max20x` one on its own, if Claude Code
  ever reports the plan size.

## Not planned

- Balancing each request across accounts: it throws the prompt cache away.
  The unit of balance is the session.
- Windows.
- Routing Claude Code's IDE extensions or the Desktop app.
- Sharing accounts between people, or a machine that several people use.
- Community pull requests without an invitation (see
  [CONTRIBUTING.md](CONTRIBUTING.md)).
