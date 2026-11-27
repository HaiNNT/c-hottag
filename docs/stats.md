# Usage stats

`chottag stats` shows how much you used Claude Code, what that usage would
have cost at API prices, and whether your plans fit it. It answers three
questions: what did I use, is each plan worth its price, and would a
different set of plans suit me better.

```sh
chottag stats                      # since five months ago, by month
chottag stats --by day             # the last 30 days, day by day
chottag stats --since 2026-09-01 --until 2026-09-30   # a month or less: by day
chottag stats --account B          # one account
chottag stats --html               # a report you can open in a browser
chottag stats --json               # the same figures for a script
```

Every flag: [Commands](commands.md#chottag-stats). Without `--by`, a
`--since` within 31 days of the end of the period reports by day.

## What it reads and what it keeps

chottag keeps a small ledger in `usage/` in the chottag home (`~/.chottag`
unless `$CHOTTAG_HOME` says otherwise). It has two sources:

- **Live.** While the daemon routes a response, it notes the message's id,
  model and token counts, and the account that served it.
- **History.** Claude Code writes a transcript of every session. `chottag
  stats` and the daemon (once at start, then every 24 hours) read the
  transcripts in each account slot's `projects/` folder and in Home's
  `~/.claude/projects`, and keep the same numbers from them. Home's files
  are opened read-only and never changed. A transcript folder or file that
  cannot be read is skipped and counted: `stats` warns
  (`usage_import_skipped`) and the next run tries it again. A message seen both live and in a
  transcript is counted once.

**What is stored: numbers and model names only.** Per message: a timestamp,
the message id, the model id, the speed, and the token counts (input,
output, cache read, cache write 5-minute and 1-hour, web searches), plus the
account name. Never a prompt, a reply, a tool result, a file path, a project
name or a session id. `import.json` keeps, per transcript, only a hash of its
path (not the path) and how far it was read; a transcript that is gone drops
out of it on the next import. Per account, about every ten minutes, a utilization
snapshot: the 5-hour and 7-day percentages, the plan tier and the 7-day reset
time. A test drives a streamed reply through the proxy and checks that no
word of it, and none of the request, appears in any file in the chottag home.

**To delete it all:** `rm -r ~/.chottag/usage` (or `$CHOTTAG_HOME/usage`).
Token history comes back from the transcripts the next time `stats` runs,
but only for transcripts still on disk (Claude Code keeps them 30 days by
default). Everything rolled up from older, deleted transcripts, and all the
utilization history (see "Utilization history" below), is lost for good.

Raw per-message records are kept for 90 days; older days are rolled up into
one row per day, account and model, which is kept for good. Utilization
snapshots are kept for good. The ledger is a few megabytes a year.

Day boundaries use the machine's local time zone.

## The report

```
Usage 2026-06-01 to 2026-11-26     tokens  $ (API)    share
  overall                          799.1M  $1,884.70
  claude-opus-5-5                  701.0M  $1,688.40  90%
  claude-sonnet-5-5                98.1M   $196.30    10%
  unknown account (history, Home)  240.0M  $512.00

Accounts           plan   price  $ value    value/price  avg 7d peak  $ per 1% (7d)  $ per 1% of a Pro
  B                ultra  $200   $1,000.70  0.86x        78%          $3.27          $0.16
  C                max5x  $100   $372.00    0.64x        91%          $0.82          $0.16
  A (remote only)  team   -      -          -            -            -              -
  B  weekly peak %: 71 80 82 78
  C  weekly peak %: 91 88 94 91

Verdicts
  B  Keep. Weekly peak averaged 78% over 4 windows, limited in 0; peak demand 15.6 of 20 units.
  C  Consider upgrade to ultra. ...
  overall  You pay $300/mo for 25 units; ...

By month
  2026-10  291.7M  $702.10    ▅
  2026-11  507.4M  $1,182.60  █

Thresholds: cancelAvgPeak=10, downgradeFill=0.8, headroom=1.15, ...
Note: Units are chottag's capacity model, ...
```

- **tokens** is input + output + cache read + cache write. Cache reads are
  most of it in a long session and are cheap; the dollar column is the better
  measure of size.
- **$ (API)** is what the same tokens cost at Anthropic's published API
  prices, from a table inside chottag that a release updates. It is not what you paid and not a bill. A model with no listed price shows
  its tokens and a `?` or `+` on the dollars; a model priced as its family's
  newest is marked `~`, and each such case adds a note under the report.
  The cost of a model with a long-prompt price tier (Haiku 5.5) is priced at
  its standard rate, because the ledger holds daily totals, not single
  prompts: it can be understated for prompts over 100K tokens, and a note
  says so.
- **share** is a model's part of the total dollars.
- **unknown account (history, Home)** is usage that cannot be tied to an
  account: transcripts from before chottag, or from Home's own login. It is in
  the totals but never in an account's value or in a verdict (see "Unknown
  account" below).
- **plan** and **price** come from `chottag plan`: the account's tier and its
  monthly price, the list price (pro $20, max5x $100, ultra $200) unless you
  set one with `chottag plan NAME --price N`. A team plan has no list price.
- **$ value** is the account's API dollars in the period.
- **value/price** is $ value divided by the plan price pro-rated to the
  period. The proration starts at the later of the period's first day and the
  account's first day in the ledger (its first usage or utilization snapshot),
  so an account added last week is not priced for months it did not exist; the
  table then says `(since DATE)`, and `--json` has it as `from`. The proration
  counts at least one day, so an account first seen today is not given an
  inflated ratio. Above 1x, the plan paid for itself against API prices.
- **avg 7d peak** is the average, over the last N complete 7-day windows
  (`--weeks`, default 4), of the highest 7-day utilization chottag saw in each
  window, 0-100%.
- **$ per 1% (7d)** is the account's API dollars in a window divided by that
  window's peak percent, averaged over windows. Windows with a peak under 10%
  are left out as noise. It is a check on chottag's capacity model: an Ultra
  account's 1% should cost about four times a Max 5x account's.
- **$ per 1% of a Pro** is that figure divided by the account's units, so
  tiers compare: if chottag's capacity model is right, every account lands near
  the same number (`usdPerUnitPct` in `--json`).
- **weekly peak %** lists each account's weekly peaks, oldest first, for the
  last N windows. A 7-day window is the account's reset time; resets less than
  two hours apart (the response headers and the usage poll can differ by a
  second) are one window.

`A (remote only)` is an account with rotation off (`chottag rotate A off`),
the one that owns your claude.ai objects but does not serve requests. It
appears in the table and the totals but not in verdicts or fleet math.

## Verdicts

A verdict looks at each rotating account that has a known tier and at least N
complete 7-day windows of utilization history. With fewer, it says "Not
enough history" and gives no advice. These are the defaults, printed under
every report as `Thresholds`:

| Verdict | When |
|---|---|
| upgrade | the average weekly peak is at least 90%, or the account hit its limit (99.5% or more) in at least 2 windows |
| downgrade to a smaller tier | its peak demand, in capacity units, fits a cheaper tier at 80% of that tier's capacity or less in every window |
| cancel candidate | the average weekly peak is under 10% and the other rotating accounts' spare units cover its peak demand |
| keep | none of the above |

Capacity units are chottag's model of how big a plan is: pro 1, max5x 5,
ultra 20, team 5 (or the account's `--units`). Peak demand in units is the
peak percent times the units.

The **overall** line adds up the peak demand of all rotating accounts per
calendar week, and finds the cheapest set of plans (pro, max5x, ultra at
their list prices) whose units cover the busiest week with 15% headroom. It
prices your current set and the suggested one. Sets within about 25% of the
cheapest price count as ties, and the one with fewer accounts wins, so the
advice is never four Pro plans to save a few dollars.

Treat verdicts as a prompt to look, not a decision. The units are chottag's
estimate, not Anthropic's published numbers, and the report says so on every
run.

## Unknown account

A transcript does not say which account served a message, and a slot's folder
is not proof: a session can be moved between accounts. So history imported
from a transcript is not attributed by where its file lives. A message gets an
account only when chottag also saw it live (same message id), which is true
from the day you install a version with stats. Everything else, the history
from before that and everything under Home's own login, is counted as
**unknown account**: it is in the totals and the per-model figures, so they
stay honest, but never in an account's value, in $ per 1% or in a verdict. So
per-account figures start when you install this version. That also keeps the
Team account kept only for claude.ai objects from skewing the advice.

## Utilization history

Token history can be rebuilt from transcripts at any time. Utilization (the
percentages that make up avg 7d peak and the verdicts) cannot: Anthropic only
reports the current value. chottag records it while the daemon runs, so
utilization history starts the day you install a version with stats, and a
verdict needs N complete weeks after that. A week the daemon never ran for
has no peak and is skipped.

## The HTML report

`chottag stats --html` writes one self-contained file (no network, no
scripts from elsewhere) to `usage/report.html` in the chottag home, or to
`--out PATH`, and prints the path. It has the month's totals, stacked bars by
model, each account's weekly peak against the 100% line, the $-per-1% table
and the verdicts, in a light or dark theme to match your system.

## Health

`chottag doctor` has a `usage-ledger` row: whether `usage/` is writable and
when transcripts were last imported. It is information only and never makes
doctor fail. If the daemon is overloaded, it drops usage records rather than
slow a response; it writes how many to `daemon.log` when it stops.
