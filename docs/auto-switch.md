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
