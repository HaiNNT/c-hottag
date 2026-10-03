<p align="center"><img src="assets/logo/chottag.svg" alt="chottag logo" width="112"></p>

# c-hottag (`chottag`)

> **A Claude Code session installing this?** Follow [For Claude Code](#for-claude-code) from start to finish: it runs `install.sh` *and* installs the plugin.

Keep several Claude accounts logged in on one machine, and switch the one Claude Code uses, even mid-session, without logging out — with claude.ai objects pinned to their owner across the switch.

c-hottag is an independent project: not affiliated with or endorsed by Anthropic.

**The name**, said "See-Hot-Tag": in WWE tag-team wrestling, the *hot tag* is when a worn-out wrestler tags in a fresh partner. chottag does that for Claude Code: when one account is spent, a fresh one tags in and the session keeps going.

```text
$ chottag status
serving: A   remote: B

  NAME   PLAN    ORG    5h    7d    STATE
  A      max5x   Acme   96%   41%   ok
  B      pro            12%   35%   ok
auto: balanced · holding A (5h 96%, resets in 9m) · last B→A 09:12 (limit)
$ chottag next
serving: B
```

## What it does

- **Switching never breaks Remote Control, connectors or artifacts.** Most switchers swap your whole login, so those claude.ai features break or vanish after a switch. chottag pins them to one account (`chottag remote <name>`) and switches only the account that pays for your prompts.
- **Several logins side by side.** Each account has its own Claude Code config dir (a *slot*). Your own `~/.claude` login is never changed.
- **Switch mid-session.** `chottag tag B` moves every chottag session to B from its next request: no logout, no restart.
- **Auto-switch near a limit**, on by default, holding a warm prompt cache
  when a reset is close, and resending a request that hit a limit.
- **Usage at a glance, and stays current.** `chottag status` shows each account's 5-hour and 7-day usage and limits, and an update check four times a day (shown in `status` and `statusline`, one notice per version), opt-in auto-install, and a daemon that restarts itself onto an installed update when idle ([Updating](docs/updating.md)).
- **Local and scriptable.** A loopback proxy that needs a per-install secret; commands answer in `--json`. The plugin's mod (Claude Code 2.1.287 or newer) adds a status card above the prompt, `/ct` commands and a `/login` guard ([Claude Code mod](docs/commands.md#claude-code-mod)).

**Status:** macOS is the supported platform. Linux builds and passes CI, but has not been used for real. Installing uses a logged-in `gh` or a clone.

## Install

**Recommended: let Claude Code do it.** In any Claude Code session, ask: *"Install and set up chottag for me: https://github.com/HaiNNT/c-hottag"*. It follows [For Claude Code](#for-claude-code): installs the binary and the plugin, asks you which accounts, plans and options you want, logs each account in with you, and checks the result.

By hand: you need macOS or Linux on amd64 or arm64, and either `gh` logged in or a clone and Go.

```sh
gh repo clone HaiNNT/c-hottag && cd c-hottag && ./install.sh
# or, without a clone:
gh api -H 'Accept: application/vnd.github.raw' repos/HaiNNT/c-hottag/contents/install.sh | sh
```

`install.sh` checks the release against its checksums (and, from v0.4.0, its build attestation), puts the binary under `~/.chottag/versions/` (or `$CHOTTAG_HOME`), and runs `chottag setup`, which adds one PATH block to your shell rc. Open a new shell afterwards. Other ways to install: [Getting started](docs/getting-started.md). Later versions: `chottag update` ([Updating](docs/updating.md)), which also says what's new and how to update the Claude Code plugin.

## Quick start

```sh
chottag login A        # opens a browser: log in to the first account
chottag login B        # and the second
chottag tag A          # A serves inference
chottag remote B       # new remote-control sessions, artifacts, routines belong to B
chottag own artifact <artifact-id> B  # move one object that already exists to B
claude                 # Claude Code, through chottag (the daemon starts by itself)
chottag status         # usage and limits per account; `chottag statusline` for a status line
chottag next           # serving -> the next account that is not limited
chottag policy spread  # or: spread new sessions over accounts (`chottag tag NAME` pins)
chottag pool add work  # a separate pool of accounts: `chottag login C --pool work`, `CHOTTAG_POOL=work claude`
chottag doctor         # check the install; --fix repairs what it safely can
```

What *serving*, *remote* and a *slot* are: [How it works](docs/how-it-works.md). Every command, flag and exit code: [Command reference](docs/commands.md).

## Auto-switch

On by default: near a limit, the daemon switches the serving account for you. `balanced` (the default) switches at a per-plan point and holds on when a reset is close; `cache-optimize` stays on one account until it is limited. More: [Auto-switch](docs/auto-switch.md).

```sh
chottag auto                       # settings, each account's usage, the last decision
chottag auto mode cache-optimize
chottag plan B max20x              # Max accounts count as max5x until you say
chottag auto off
```

## Compared with the best-known switchers

By GitHub stars (as of 2026-09-29; the last two columns checked 2026-10-01), chottag next to the best-known Claude Code account switchers (✅ yes, ⚠️ partly, ❌ no, ❔ not stated in its README):

| Tool | Pins Remote Control, connectors, artifacts to one account | Mid-session switch, no restart | Auto-switch at a limit | Best prompt-cache use with more than 3 accounts and parallel sessions | Isolated work and personal accounts | Never writes Home's login |
|---|---|---|---|---|---|---|
| <img src="assets/logo/chottag.svg" alt="" width="16" align="absmiddle"> **chottag** | ✅ **built in** | ✅ | ✅ on by default | ✅ each session stays on one account, so a limit moves only that account's share and the rest keep their warm cache | ✅ work sessions only ever use work accounts; one account can serve both | ✅ |
| [claude-swap](https://github.com/realiti4/claude-swap) | ❌ (the separate [cswap-pin](https://github.com/codeslake/cswap-pin) add-on adds it) | ⚠️ Linux/Windows: yes; macOS: ~30s or a restart | ✅ | ⚠️ you [assign each terminal](https://github.com/realiti4/claude-swap#run-multiple-accounts-at-the-same-time-session-mode) by hand | ⚠️ per directory, one account each | ❌ |
| [clauth](https://github.com/uwuclxdy/clauth) | ❌ | ❔ not stated | ✅ | ⚠️ you pick a profile per session | ❔ not stated | ❌ |
| [caam](https://github.com/Dicklesworthstone/coding_agent_account_manager) | ❌ | ❌ switch before a new session | ✅ reactive | ⚠️ suggests an account; you launch the session on it | ❔ not stated | ❌ |
| [CCSwitcher](https://github.com/XueshiQiao/CCSwitcher) | ❌ | ✅ | ❔ not stated | ❌ every session uses one login | ❔ not stated | ❌ |

The four others switch by swapping Claude Code's whole login, so every claude.ai feature moves with it. [CC Switch](https://github.com/farion1231/cc-switch) is the best-known "switch" tool for Claude Code, but it switches API providers and gateways, not your own subscription accounts. Full sourcing, more tools and migration guides: [docs/comparison.md](docs/comparison.md).

## For Claude Code

A Claude Code session asked to install or manage chottag for a user
follows [docs/for-claude-code.md](docs/for-claude-code.md) instead of
this section: it installs without a clone, gets the user's go-ahead on
the terms, adds the plugin (`claude plugin marketplace add
HaiNNT/c-hottag`, or `/plugin marketplace add HaiNNT/c-hottag` inside a
session, then `claude plugin install chottag@c-hottag`), logs accounts
in, and covers managing chottag afterwards.

Non-negotiable, every command: pass `--json` and branch on `ok` and
`error.code`, never the message. `chottag login <name> --json` runs
through Bash with `timeout: 600000` (the 10-minute maximum) and opens a
browser. Never run `/login` or `/logout` in a session (they change
Home's login, not a chottag account), never edit `~/.claude`,
`~/.claude.json` or anything in them, and never run `chottag uninstall
--purge` for the user.

## Documentation

Start at [the docs index](docs/index.md), or go straight to [Getting started](docs/getting-started.md),
[Troubleshooting](docs/troubleshooting.md), [FAQ](docs/faq.md),
[Known limitations](docs/known-limitations.md), [Coming from another tool](docs/comparison.md) or
[Uninstall](docs/uninstall.md). Security model and how to report a problem: [SECURITY.md](SECURITY.md).
Issues are welcome; pull requests are by invitation only: [CONTRIBUTING.md](CONTRIBUTING.md). Help:
[SUPPORT.md](SUPPORT.md). Plans: [ROADMAP.md](ROADMAP.md). Changes: [CHANGELOG.md](CHANGELOG.md).

## Terms of use and risk

Use c-hottag only with Claude accounts that are yours, and never to share or resell access.
Anthropic's [Consumer Terms](https://www.anthropic.com/legal/consumer-terms) (Commercial Terms for a
Team or Enterprise seat), [Usage Policy](https://www.anthropic.com/legal/aup) and [Claude Code legal
page](https://code.claude.com/docs/en/legal-and-compliance) govern your accounts. That page says
plan limits assume "ordinary, individual usage", and that "developers may not collect, store, or
intermediate Claude.ai credentials or session tokens" — chottag's proxy intermediates them: it swaps
in each account's token as it forwards the request. Read the pages and decide for yourself: Anthropic may
restrict or suspend accounts at its discretion. A Team or Enterprise seat is also bound by its
organization's policy: use c-hottag with one only if that organization allows it. No warranty
(Apache-2.0, sections 7 and 8): use c-hottag at your own risk. Auto-switch is on by default;
`chottag auto off` turns it off.

## Uninstall

```sh
chottag daemon stop && chottag uninstall   # removes the PATH block and bin links; keeps logins
rm -rf ~/.chottag/versions                 # then, optionally, the installed binaries
chottag daemon stop && chottag uninstall --purge   # or instead: delete ~/.chottag and every login
```
The plugin, a status line and a service unit: [Uninstall](docs/uninstall.md).

## Credits

The route knowledge (which Claude Code requests belong to which account)
builds on cswap-pin (MIT). Licensed under Apache-2.0: `LICENSE` and `NOTICE`.
