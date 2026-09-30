# Roadmap

Where c-hottag is going. It has no dates and makes no promises: the project
is maintained by one person in their spare time. To suggest something, open a
**Feature idea** from [the issue chooser](https://github.com/HaiNNT/c-hottag/issues/new/choose).

## Now

- A status line for Claude Code that shows which account a session uses and
  how close that account is to its limits, and a status pill in the
  cmux sidebar.

## Next

- **Spread mode.** With many accounts and many sessions, place each new
  session on the account with the most headroom and keep it there, so no
  single account's 5-hour window burns out and prompt caches stay warm.
  Today every session uses the one serving account, and they switch
  together.
- **Pools.** Separate groups of accounts for separate purposes, such as work
  and personal, each with its own remote account. A session picks its pool
  when it starts.
- Linux used for real, not only built and tested in CI.
- Run the route check on each new Claude Code version as a routine release
  step, and in CI where possible.

## Later

- A daemon that restarts itself after an update, at a quiet moment with no
  request in flight, so `chottag update` never needs a manual
  `chottag daemon restart`.
- A UI for people: menu bar and launcher recipes first, then a local web
  page to manage accounts and pools.
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
