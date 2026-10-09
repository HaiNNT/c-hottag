# Install and manage chottag for a user (for AI agents)

This page is written to an AI coding agent (for example, a Claude Code
session) asked to install or manage chottag (c-hottag) for the person
running it. Follow the steps in order; every command is copy-pasteable.
[For Claude Code](https://github.com/HaiNNT/c-hottag/blob/main/README.md#for-claude-code)
in the README points here.

## 0. Before anything

- Confirm the user is on macOS or Linux, amd64 or arm64. `install.sh`
  detects this itself and refuses a machine it does not support; macOS is
  the platform this project actually runs on day to day, Linux builds and
  passes CI but has not seen real use.
- Confirm Claude Code is installed (`claude --version`).
- Run `gh auth status`. It must show a login: `install.sh` downloads the
  release with `gh` and verifies its build attestation with `gh
  attestation verify` (gh 2.49 or later). The repository is public, so
  any GitHub login works.
- Show the user the README's
  [Terms of use and risk](https://github.com/HaiNNT/c-hottag/blob/main/README.md#terms-of-use-and-risk)
  and the FAQ's [Is this allowed?](faq.md#is-this-allowed) answer. Proceed
  only once the user has read them and tells you to go ahead.

## 1. Install, without a clone

Run this from any directory that is not a clone of this repo:

```sh
gh api -H 'Accept: application/vnd.github.raw' repos/HaiNNT/c-hottag/contents/install.sh | sh
```

Add `-s -- --repo OWNER/NAME` only to install from a fork:

```sh
gh api -H 'Accept: application/vnd.github.raw' repos/OWNER/NAME/contents/install.sh | sh -s -- --repo OWNER/NAME
```

`install.sh` never prompts and never uses `sudo`. It checks the release
against its checksums and its build attestation, installs the binary under
`~/.chottag/versions/` (or `$CHOTTAG_HOME`), and runs `chottag setup` at
the end. Install the latest release: don't pass `--version`. Releases of
`HaiNNT/c-hottag` before v0.4.7 were built while the repository was
private, carry no attestation, and `install.sh` refuses them. Exit `2`
means a bad argument; exit `1` means the install itself failed, and the
last line printed says why. Tell the user what it said if it fails.

## 2. Install the plugin

```sh
claude plugin marketplace add HaiNNT/c-hottag
claude plugin install chottag@c-hottag
```

(Inside a Claude Code session, the equivalent is `/plugin marketplace add
HaiNNT/c-hottag` then `/plugin install chottag@c-hottag`.) The skill this
installs then runs as `/chottag:chottag`. If `marketplace add` fails to
clone, check `git ls-remote https://github.com/HaiNNT/c-hottag` works in
the user's terminal.

## 3. Until the user opens a new shell

The install added a PATH block to the user's shell rc, but it only takes
effect in a shell started after this step. Until then, run chottag by its
full path: `~/.chottag/bin/chottag` (or `$CHOTTAG_HOME/bin/chottag`, if
the user set that variable before installing). The commands below write
`chottag` for short.

## 4. Check it started

```sh
chottag status --json
chottag doctor --json
```

`status` must come back with `"ok": true` and `daemon.running` true. On a
first install, `doctor` still reports two checks, and both are expected:
`roles` ("no accounts are registered", fixed in step 6) and `path` (fixed
by the new shell in step 8). Any other failing check is a real problem:
stop, tell the user which check failed and the fix its row names.

An **account** is a Claude login chottag keeps in its own slot,
`~/.chottag/accounts/<name>` (or under `$CHOTTAG_HOME`); the user's normal
Claude Code login ("Home", `~/.claude` and `~/.claude.json`) is never
touched by any of this. If the user had chottag installed before, `chottag
setup` may already have re-registered old accounts (an `adopted NAME
(email)` line in the install output, or entries under `accounts` in
`status`): don't log those in again.

## 5. Agree the setup with the user

Ask these in one message, each with its default, then start. Don't ask
about anything else: every other setting has a working default the user
can change later through the skill.

1. **Which Claude accounts, and a short name for each** (for example
   `work` and `personal`)? Is any of them a company Team or Enterprise
   account? If so, it may be added only if that organization allows
   chottag.
2. **Which plan is each one?** `pro`, `max5x`, `max20x` or `team`.
   Auto-switch and `spread` size each account's capacity by its plan. A
   login tells chottag only "Max", not which Max, so a Max account shows
   as `max?` until it's set. Default: the plan the login reports; ask only
   about Max accounts.
3. **Which account is pinned?** The pinned account (chottag calls it
   `remote`) owns the user's Remote Control sessions, connectors and
   artifacts, so they stay put when chottag switches the account that pays
   for prompts. Default: the account the user already uses those features
   with, or else the first one.
4. **Should the pinned account also pay for prompts?** Default: yes. Say
   no to keep it for claude.ai features only (for example a work account
   whose usage the user wants to leave alone).
5. **Do they run several Claude Code sessions at once?** Ask only when at
   least two accounts will pay for prompts. Default: no, which keeps
   `serial`: every session uses one serving account, and auto-switch moves
   them all together. Yes turns on `spread`: each new session goes to the
   account with the most room left and stays there, so its prompt cache
   stays warm, and only that session moves when its account nears a limit.
6. **Should chottag install its own updates?** Default: no. chottag checks
   for a new release about four times a day either way and says so in `chottag status`,
   on the status line and in one notification. Yes lets the daemon install
   a release by itself (same major version, at least 24 hours old).
7. **Do you keep work and personal accounts apart?** Ask only when there are
   at least two accounts. Default: no, which keeps one pool (`default`) with
   every account in it. Yes creates a pool for each side (for example `work`
   and `personal`) and puts each account the user names in its pool: a
   session in a pool is served only by that pool's accounts, and each pool has
   its own remote account, serving account and policy. Ask which account goes
   in which pool, and which one is each pool's remote account (the one that
   owns its claude.ai objects).
   Before you put an account in a **second** pool (`chottag pool join`), tell
   the user the down-side, because the account has one usage limit: if either
   pool is on `spread`, the pools' sessions compete for that account, and
   heavy use in one pool moves the other's sessions off it (their prompt
   caches go cold); if both are `serial`, and the account serves both, they
   use up its 5-hour window together and switch away from it at the same time.
   Join it only if the user still wants that. Then give the user the aliases
   to add to their shell's rc file themselves, for example
   `alias cwork='CHOTTAG_POOL=work claude'` and
   `alias cpersonal='CHOTTAG_POOL=personal claude'`, and say that plain `claude`
   starts a session in the `default` pool. `default` keeps every account
   unless the user removes one (`chottag pool leave ACCOUNT default`), so an
   unset `CHOTTAG_POOL` still has the old pool and may use any of them; ask
   whether to take the pool accounts out of it, and warn that an empty
   `default` answers plain `claude` with a 503. Never edit the rc file for them.

Tell the user, without asking:

- Auto-switch is on by default (`balanced`): when the serving account nears
  its limit, chottag moves to the next one, holding a warm prompt cache when
  a reset is close. `chottag auto mode cache-optimize` stays on one
  account until it is actually limited.
- Desktop notifications are on: a switch, a limit, an available update.
  `chottag notify off` turns them off.
- After an update, the daemon restarts itself onto the new version once
  Claude Code is idle (no request for 5 minutes).

## 6. Log the accounts in

Log the pinned account in first, then the rest. For each one, tell the
user a browser window is about to open, then run this through Bash with
`timeout: 600000` (the ten minute maximum: it blocks until the user
finishes in the browser):

```sh
chottag login <name> --json
```

Then apply the answers, in this order:

```sh
chottag pool add <pool> --json         # question 7, only on yes: each pool, before any login that names it
chottag login <name> --pool <pool> --json  # question 7: instead of a plain login, an account in that pool only
chottag pool join <name> <pool> --json # question 7: an account that is also in a second pool (tell the user the down-side first)
chottag plan <name> max20x --json      # question 2: each account whose plan is not right yet
chottag remote <pinned> --json         # question 3 (add --pool <pool> for an account that is in several pools)
chottag rotate <pinned> off --json     # question 4, only if it must not pay for prompts
chottag tag <name> --json              # only if the pinned account is out of rotation: serve from another one
chottag policy spread --json           # question 5, only on yes
chottag update --auto-install on --json  # question 6, only on yes
```

With the pinned account out of rotation, log in at least one other account
before `tag`. Run `tag` before `policy spread`: under `spread`, `tag` pins
new sessions to that account instead of setting the serving one.

With pools, `remote`, `tag`, `rotate`, `policy` and questions 3 to 5 apply
per pool: an account in one pool acts there, one in several needs `--pool
<pool>` (`pool_ambiguous` otherwise), and `policy` and a bare `tag --unpin`
take `--pool <pool>` (default `default`). `chottag login <name> --pool
<pool>` puts the new account in that pool only, so log each account in once
and `pool join` it to any other pool; if `login` says the email is already an
account, use `pool join` rather than a second login. Relay the `shared_account`
warning to the user. `pool add` fails with `daemon_predates_pools` (exit 2)
when the running daemon is older than 0.8.0: run `chottag daemon restart`, then
repeat it. `pool join` warns with the same code. Until the daemon is restarted
no session starts once a pool exists.

## 7. Verify

```sh
chottag status --json
chottag doctor --json
```

`status` must list every account the user named under `accounts`, with
`remote` the pinned one, no `max?` plan left, and `policy` set to `spread`
if the user chose it (it is left out under `serial`). If the user chose
pools, `pools[]` must list each pool with the accounts the user put in it, a
`remote` and a `serving` for each, and `shared: true` only on an account the
user agreed to share; `doctor`'s `roles` row names each pool's roles, and an
`info` row warning that a pool has no account in rotation means its sessions
get a 503 until one is back (`chottag rotate <name> on`). `doctor` exits `0` when nothing is wrong; `path`
may still fail until step 8. Any other problem is `"ok": false`, exit
`3`, `error.code` `doctor_problems`, with the failing checks under
`error.checks`, each row naming its own fix.

## 8. Tell the user which sessions use chottag

Tell the user, in these words or close to them:

- Only Claude Code sessions started after the install use chottag.
  Sessions already running, this one included, stay on their normal login
  (Home) until they end.
- To move a session over, exit it, open a **new terminal** (the old one
  has no chottag PATH entry), and resume it with `claude --continue` or
  `claude --resume`.

Sessions also get names automatically from their first prompt (the branch,
then Claude Code's generated title) when the chottag plugin is installed.
`chottag names off` turns that off, and `/ct names` changes the mode from
inside Claude Code.

Then offer a way to see which login a session uses that adds nothing to
its conversation (don't use a `!` command for this: its output lands in
the chat):

1. **The status card.** The plugin's mod (Claude Code 2.1.287 or newer) already
   draws a compact card above the prompt with the account, both usage windows and
   their resets, and shows `/ct` commands, notices and a `/login` guard: tell the
   user it appears in a new session (its `/ct` commands include `status`, `next`,
   `tag`, `pool`, `names` and `help`), and offer the status line below only for an
   older Claude Code or when they also want it there (both can show). Check with
   `claude --version`.
2. **The status line.** `chottag statusline` prints
   `c» <this session's account> · 5h … · 7d …` for a session that goes through
   chottag, `c» down` when chottag is not answering, and `c» off` for a
   Home session. It never replaces the user's status line: ask whether
   they have one first.
   - **They have one:** add chottag's segment to what their script
     prints, `$(~/.chottag/bin/chottag statusline 2>/dev/null)`, or read
     fields from `chottag statusline --json`. Show them the change first.
     The plugin skill has a ready-made wrapper script for a `statusLine`
     that runs a plain command.
   - **They have none:** offer chottag's line on its own, in their
     `~/.claude/settings.json`:

     ```json
     { "statusLine": { "type": "command", "command": "~/.chottag/bin/chottag statusline" } }
     ```

   `chottag statusline` only prints; it has no side effect. Offer the cmux
   sidebar pill only to a user who runs cmux, and add `--cmux`
   (`$(~/.chottag/bin/chottag statusline --cmux 2>/dev/null)`) only with
   their yes.

   Either way, edit only with the user's yes. This is the one edit under
   `~/.claude` this guide allows; chottag itself never writes there. Use
   `chottag statusline`, not `chottag status --json`: only `statusline`
   knows whether this session goes through chottag.
3. **From another terminal.** `chottag status` shows `live sessions: N`,
   which goes up by one when a session starts through chottag.

## Lost sessions

If `chottag status` says `N sessions were lost at HH:MM`, cmux quit or the Mac
crashed. Run `chottag sessions` to list them and tell the user. Run `chottag
resume` only when they say yes: it types a `claude --resume` command into
their terminal tabs. `chottag resume --print` only prints. Each listed
session has a `title` when its transcript holds one. The plugin also runs
`chottag name-session` as a Claude Code hook, to name sessions after their
branch and generated title; it needs no action from you, and
`chottag names off` turns it off. See [resume.md](resume.md).

## Two notices

- "an artifact's (or session's, connector's, environment's) owner is
  unknown": the remote account could not open an object chottag has no owner
  for, for example one made outside chottag. Other accounts were tried for
  reads. It is not route drift; usually do nothing. If the user knows the
  owning account, `chottag own <kind> <id> <account>` records it (ask first).
- "route drift": a swapped request stayed refused after a retry, so the route
  table may not match this Claude Code version. The notice names the next
  step, `chottag trace on` (ask first).

## Managing chottag afterwards

Once installed, drive chottag through its plugin skill
(`/chottag:chottag`, or model-invoked when the user asks about accounts,
limits or switching) rather than repeating the steps above. Always pass
`--json` and branch on `ok`, then on `error.code` — never on
`error.message`, which is prose for humans. Relay every `warnings[]`
entry.

The commands used day to day:

```sh
chottag status --json                    # who is serving, who is remote, usage and limits
chottag tag <name> --json                # switch to a named account
chottag next --json                      # switch to the next account that is not limited
chottag remote <name> --json             # set who owns new remote-control sessions, artifacts, routines
chottag auto --json                      # auto-switch settings and last decision
chottag policy spread --json              # one account per session, only if the user agrees
chottag pool --json                      # the pools and who is in each (add, join, leave, rm ask the user first)
chottag doctor --json                    # check the install
chottag update --check --json            # is a newer release available
chottag update --json                    # install it, only once the user agrees
```

Every command, flag, exit code and JSON field: [Command reference](commands.md).

chottag checks for a newer release about every 6 hours and shows `update: <v> available`
in `chottag status`. When you see it, tell the user and offer `chottag
update`. If `update` exits with `update_in_progress`, another update is
running: wait and re-check with `chottag update --check --json`; don't retry
in a loop. If `update --json` returns `"daemon": "deferred"` with
`"selfRestart": false`, the running daemon can't restart itself onto the new
version (it predates 0.6.0, or auto-restart is off): tell the user and run
`chottag daemon restart` to finish the update.

After a successful `chottag update`, relay its "What's new" summary (the
`whatsNew` array in `--json`: version, summary, link) to the user, then ask
whether to update the Claude Code plugin too. The update does not touch it,
and a plugin older than chottag can miss new commands and card fields. On a
yes, run the two commands in `plugin.commands` in order (`claude plugin
marketplace update c-hottag`, then `claude plugin update chottag@c-hottag`)
and tell the user to run `/reload-plugins` in their open sessions (or restart
them). Don't run them without the yes. Never turn on `chottag update --auto-install on` without the
user's yes.

## Rules for the agent

- Never run `/login` or `/logout` inside a Claude Code session: they log
  Home in or out, not a chottag account. (`claude auth ...` and `claude
  setup-token` in a terminal skip chottag on their own; only a `/login`
  needs that session started with `CHOTTAG_BYPASS=1 claude`.)
- Never edit `~/.claude`, `~/.claude.json`, or anything under them, except
  the `statusLine` entry in step 8, and only after the user says yes.
- Never run `chottag uninstall --purge` for the user: it deletes every
  account's login and needs a typed confirmation in a terminal chottag
  controls. Tell the user the command instead.
- Ask the user before `chottag doctor --fix`, `chottag update`, `chottag
  logout`, `chottag rename`, `chottag uninstall`, `chottag pool add`,
  `pool join`, `pool leave` or `pool rm`.
- Never edit the user's shell rc file to add the `CHOTTAG_POOL` aliases:
  give them the lines and let them add them.
- Do not add a company Team or Enterprise account unless that
  organization allows chottag; ask first if you are unsure.
- Never switch to a different account to get around a hold, a suspension
  or a ban on another one.

## If you are working inside a clone of this repo

This repo's own `CLAUDE.md` and `.claude/` are for developing chottag
itself, not for installing it for a user: its hook blocks `install.sh`
and most `chottag` commands from running against a real home from inside
a clone. To install or manage chottag for a user, leave the clone and
follow this page from the user's normal working directory instead.
