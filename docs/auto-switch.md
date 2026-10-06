# Auto-switch

## What it does

Auto-switch is on by default. The daemon moves the serving account near a
limit, before a request gets refused. `chottag auto` shows the current
settings, each account's usage and the last decision it made.
`chottag auto off` turns it off; `chottag auto on` turns it back on. The
`auto:` line of `chottag status` shows the mode, whether it's holding, and
the last switch — or `auto: off` when it's off.

## Modes

- `balanced` (the default): switches at a per-plan switch point, picks
  whichever candidate account keeps you working the longest, and holds on
  to the current account when its window is about to reset rather than
  give up a warm prompt cache.
- `cache-optimize`: every switch point is 100, so it stays on one account
  until that account is limited, then takes the next account in
  registration order. Fewer switches, so fewer cache misses.

`balanced` is the default because most sessions benefit more from not
running out mid-task than from squeezing out the last few cache hits on
one account — not because of any usage pattern measured from a real
account.

## Switch points

A switch point is the usage percentage, for one window and plan tier, at
which auto-switch leaves this account for another candidate. 100 means it
switches only at the wall: a 100% reading or a refusal, never a threshold
below it. These are `balanced` mode's defaults, one row per window and
tier:

| Key | Switch point |
| --- | --- |
| `5h.pro` | 88 |
| `5h.max5x` | 93 |
| `5h.team` | 93 |
| `5h.max20x` | 98 |
| `7d.pro` | 93 |
| `7d.max5x` | 98 |
| `7d.team` | 98 |
| `7d.max20x` | 99 |

Set an account's tier with `chottag plan <name> <tier>` (`pro`, `max5x`,
`team` or `max20x`). A Max account counts as `max5x` until you say
otherwise.

## Settings

| Key | Default | Meaning |
| --- | --- | --- |
| `hold5h` | 30m | how long to hold the serving account when its 5-hour window is about to reset, rather than switch away right before a fresh window arrives |
| `hold7d` | 3h | the same hold, for the 7-day window |
| `cooldown` | 15m | how long to wait after a switch before switching again, so a borderline account doesn't flap back and forth |

`chottag auto set KEY VALUE` changes one setting: a switch point takes a
whole number from 50 to 100, and a hold or the cooldown takes a duration in
Go syntax (for example `45m`) from 0 to 24h. `chottag auto reset` clears
every override, back to the mode's defaults.

## Your own choice

A `chottag tag` onto an account that is already past its switch point is
held as your choice: auto-switch leaves it alone until its usage drops
below every switch point, and only then starts moving off it as usual.
That choice is not remembered across a daemon restart.

## When a request hits a limit

When a request is refused for hitting a limit, the daemon switches the
serving account and resends that request once on the new account, so you
see nothing happen. If the switch went through but the resend itself
couldn't finish (the limit arrived mid-reply), the notice says "Resend
your last message" — only that one message needs resending; everything
after it goes to the new account automatically. If no account is free to
switch to, you get a no-candidate or all-limited notice instead.
`chottag notify` controls whether these show as desktop notifications
(macOS only).

Under [`chottag policy spread`](how-it-works.md#spreading-sessions-spread)
this works per session, not on `serving`: the refused request's session is
moved to another account and resent once, whether or not auto-switch is on,
and the notices are the spread ones (`chottag: moved N sessions from A to B,
C`, or `N sessions on A have no account to move to`, plus the usual switched
notice on a retry), also controlled by `chottag notify`.

## Before it switches

Before moving serving onto an account, the daemon checks that account:

- **No login.** An account that needs a login is never switched to. The daemon
  records `needs-login` for it (one notice, `chottag login <name>` to fix) and
  picks the next candidate. A poll or a refresh that finds no login records it
  too, without waiting for a request to fail on it.
- **A stale token.** The daemon refreshes it first. A threshold switch waits
  for the next check; a switch forced by a limit (a request that was refused)
  waits for the refresh, then takes the next candidate if the token did not
  renew.
- **Old usage.** A target whose reading is older than 45 minutes ranks after
  the others, and is polled before the switch. A threshold switch waits until
  the poll lands. A poll never overrides a 429 backoff, so an account the
  server told to wait is not polled sooner.
- **A limit seen without a refused request** (the daemon's own check, not a
  request waiting on it) never waits: it starts the refresh or poll and takes
  the next candidate whose token and reading are fine right now, or keeps the
  switch deferred when there is none. A poll skipped because the account is in
  a 429 backoff says so in the daemon log.
- **Bounds.** The waits happen only for a switch forced by a limit, never on
  the path that delivers a response, and together take at most 25 seconds
  (15 for tokens, 10 for polls) however many candidates fail. A switch forced
  by a limit whose poll failed or timed out goes ahead on the old reading,
  rather than leave you on a limited account.

[`chottag policy spread`](how-it-works.md#spreading-sessions-spread) has no
such check of its own: it relies on the `needs-login` state recorded by the
daemon's warm pass, usage polls and refreshes, which keeps that account out of
new placements.

## Pools

With [pools](how-it-works.md#pools), each `serial` pool is evaluated on its
own: the planner looks at the pool's members, switches the pool's serving
account, and a wall retry moves the session to another member of its own pool,
never to an account outside it. Two pools may have the same serving account.
Usage and limits belong to the account, so a shared account looks the same
from every pool, and its usage drives the decision in each pool it serves.
The settings (`chottag auto`, `chottag notify`) are global. `chottag status`
shows each pool's last decision and switch, and a notice says which pool it
is about (`chottag: work: switched to C`). Under `spread` a pool places its
sessions among its own members.

## Plan sizes

The planner needs to know each account's plan tier to pick the right
switch points and compare accounts of different sizes fairly:

```sh
chottag plan <name> <tier> [--units N]
```

See [`chottag plan`](commands.md#chottag-plan) for the tiers, the
`--units` override and the JSON fields.

## Next

- [How it works](how-it-works.md)
- [Commands](commands.md)
- [Troubleshooting](troubleshooting.md)
- [Documentation home](index.md)
