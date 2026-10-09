# Configuration and files

Everything chottag keeps lives under `~/.chottag`, or `$CHOTTAG_HOME` when
that is set (`internal/cli/cli.go`'s `home()`; a relative `CHOTTAG_HOME` is
made absolute, so every command resolves it the same way regardless of its
current directory). The one file outside that tree is the fenced block
`setup` adds to your shell rc — `# >>> chottag >>>` … `# <<< chottag <<<`,
in `~/.zshrc` or `~/.bashrc` depending on `$SHELL` — which puts `bin/` on
`PATH` and, when `CHOTTAG_HOME` was set at setup time, also exports it.

chottag never writes `~/.claude` or `~/.claude.json`: those are Home, your
own normal Claude Code login, kept apart from the account slots inside
`accounts/` and the real, unmanaged Claude Code install a slot's
`CLAUDE_CONFIG_DIR` points at.

## Files

Every path below is relative to the chottag home (`~/.chottag` unless
`$CHOTTAG_HOME` says otherwise).

| Path | What it holds | Who writes it | Safe to delete? |
| --- | --- | --- | --- |
| `state.json` | Your accounts and settings: names, slot directories, plans, auto-switch and notification settings (`internal/store/store.go`). | Every `chottag` command that changes something; the daemon writes only `serving`, on an auto-switch. | No — this is your account roster. Back it up before touching it by hand. |
| `accounts/` | One subdirectory per account, each a full `CLAUDE_CONFIG_DIR` (Claude Code's own login, settings and session files for that account). | Claude Code itself, run through that slot's `CLAUDE_CONFIG_DIR`. | No — deleting a slot removes that account's login. Use `chottag logout` instead of `rm -rf`. |
| `cache/status.json` | A rebuildable summary of recent traffic: usage estimates, the daemon's last-seen state, auto-switch decisions (`internal/status/status.go`). | The daemon, from live traffic. | Yes — it is disposable and rebuilt from traffic; a missing or corrupt file is not an error. |
| `owners.json` | Which account owns each claude.ai object (a remote-control session, environment, artifact or connector), so a later request for that object reaches the account that created it. | The daemon, as it routes requests; `chottag rename` and `chottag own` edit it too. | No, not casually — deleting it loses the routing chottag learned; a new owner is only recorded on the next request. |
| `install.json` | Which repo and version this install came from, and whether it came from a release or a local build (`internal/cli/installrecord.go`). | `install.sh`, then `chottag update` on every version change. | Yes, but `chottag update` then falls back to the built-in default repo. |
| `daemon.log` | The daemon's own rotating log (routine startup/shutdown lines and errors; each line starts with its local time, `2006-01-02 15:04:05`), not request traffic. | `daemon run`. | Yes — it rotates on its own and carries no state. |
| `proxy.jsonl` | Request metadata only: method, path, ids, which account it went out as, and status — never tokens or request/response bodies. | `daemon run`, one line per request; bounded, and rotated like `daemon.log`. | Yes. |
| `trace.jsonl` | `chottag trace`'s redacted record of request and response *shapes* (kinds, templates, field names), never values. | The daemon, only while a trace window is open. | Yes. |
| `versions/` | One subdirectory per chottag version `chottag update` has downloaded, so a broken update can roll back. | `chottag update`; older ones are pruned automatically, keeping the newest three plus whatever `bin/chottag` currently links to. | Yes, other than the version currently in use — `chottag update` prunes the rest on its own. |
| `ca/ca.pem` | The public certificate of chottag's local certificate authority, which lets the proxy present valid HTTPS certificates for the hosts it intercepts. | `chottag setup` (or the first command that needs a CA), once. | No while any session might still be running — every account and Claude Code invocation is configured to trust this exact file (`NODE_EXTRA_CA_CERTS`); deleting it breaks TLS for them until they restart. |
| `ca/ca.key` | The CA's private key. | `chottag setup`, alongside `ca.pem`. | No — losing it (without also removing `ca.pem`) breaks the CA; chottag refuses to regenerate one half without the other. Never share this file. |
| `ca/proxy.secret` | The per-install secret. Each `claude` the shim launches gets its own `HTTPS_PROXY` credential derived from it (user `chottag.default.<sid>`, an HMAC password); the older `chottag:<secret>` form is still accepted (`internal/proxyauth`). | `chottag setup`, and regenerated if it goes missing. | No while a session is running — a caller with the old secret can no longer reach the daemon; running sessions need a restart after it changes. |
| `ca/bundle.pem` | A merged CA bundle: chottag's `ca.pem` plus whatever `NODE_EXTRA_CA_CERTS` already named, for a shell that had its own extra CA before chottag (`internal/shim/shim.go`). | The `claude` shim, only when `NODE_EXTRA_CA_CERTS` was already set. | Yes — it is rebuilt on the next `claude` invocation that needs it. |
| `ca/` | Holds the CA and the proxy secret above. | `chottag setup`. | No, for the reasons above. |
| `bin/` | Holds the `chottag` and `claude` symlinks that `setup` puts on `PATH`. | `chottag setup`. | No — removing it breaks the `claude` shim; re-run `chottag setup` to recreate it. |
| `bin/claude` | A symlink to the `chottag` binary itself: this is the shim `claude` invocations actually run, ahead of the real Claude Code on `PATH`. | `chottag setup`. | No, for the reason above. |
| `run/` | Holds the daemon's lock file, proving which process, if any, currently owns the daemon role, and the live-session records (one file per `claude` the shim launched). | `daemon run`, and the shim. | Yes when no daemon is running; `chottag daemon stop` is the normal way to release it. |
| `run/placements.json` | Under `chottag policy spread`: the account each live session is placed on (session id, account, slot directory, when it was placed and last moved), so a daemon restart keeps every session where it was. Written with mode 0600, only by the daemon, and only while it holds placements (an earlier spread period's expire after their sessions end); never holds a token. | The daemon, within a few seconds of a change and at shutdown. | Yes — the sessions are placed again at their next request, which can put them on different accounts. |
| `cache/` | Holds `cache/status.json` above. | `chottag setup`. | Yes, for the reason above. |

## state.json

`state.json` is your accounts and chottag's settings: which accounts are
registered, which one is serving requests, and daemon options like
auto-switch and notifications. A `chottag` command writes it when you run
one that changes something; the daemon writes only `serving`, and only on
an auto-switch.

```json
{
  "version": 1,
  "accounts": [
    {
      "name": "A",
      "email": "alice@example.com",
      "org": "Acme",
      "dir": "/Users/alice/.chottag/accounts/A",
      "addedAt": "2026-01-01T09:00:00Z",
      "noRotate": false,
      "plan": "max20x",
      "units": 0,
      "loggedInAt": "2026-01-01T09:05:00Z"
    }
  ],
  "serving": "A",
  "remote": "A",
  "port": 47821,
  "realClaude": "",
  "auto": {
    "enabled": true,
    "mode": "balanced",
    "switchPoints": {
      "5h.max5x": 90
    },
    "hold5h": "10m",
    "hold7d": "1h",
    "cooldown": "5m"
  },
  "notify": true,
  "names": "model",
  "policy": "spread",
  "pin": "A",
  "label": "dev",
  "updates": {
    "check": true,
    "auto": false,
    "restart": true
  },
  "trace": {
    "until": "2026-01-01T10:00:00Z"
  }
}
```

With a pool other than `default` (see [Pools](how-it-works.md#pools)), the
file is `"version": 2`, each account may carry `"pools"`, and there is a
top-level `"pools"` object:

```json
{
  "version": 2,
  "accounts": [
    {"name": "A", "dir": "/Users/alice/.chottag/accounts/A", "pools": ["work"]},
    {"name": "B", "dir": "/Users/alice/.chottag/accounts/B", "pools": ["work", "personal"]}
  ],
  "pools": {
    "work": {"serving": "A", "remote": "A"},
    "personal": {"serving": "B", "remote": "B", "policy": "spread", "pin": "B"}
  }
}
```

- `version` — the file format version; chottag refuses to read a file newer
  than it understands.
- `accounts` — the registered accounts, in registration order (this is also
  `chottag next`'s order).
  - `name` — the account's display name.
  - `email`, `org` — for display only; two accounts can share an email and
    differ only by org.
  - `dir` — the account's `CLAUDE_CONFIG_DIR`, fixed when the account was
    added; it never changes, even if the account is later renamed.
  - `addedAt` — when the account was registered.
  - `noRotate` — keeps this account out of `chottag next` and auto-switch.
  - `plan`, `units` — the account's plan tier and any capacity override, for
    auto-switch.
  - `loggedInAt` — when `chottag login` last confirmed this account
    is logged in, or `chottag adopt` last found its slot's email or org
    changed. Re-running adopt (or `update`) on an unchanged slot leaves it
    alone, so it never clears a real needs-login.
  - `pools` — the pools the account is in (see below). Absent means only
    `default`; an account is in at least one pool.
- `serving` — the account currently answering requests, in the `default`
  pool.
- `remote` — the account that owns new claude.ai objects created from here
  on: remote-control sessions, environments, artifacts and connectors, in the
  `default` pool.
- `port` — the daemon's port, when it is not the default.
- `realClaude` — an explicit path to the real Claude Code binary, when
  chottag should not resolve it from `PATH` on its own.
- `auto` — auto-switch settings: on/off, mode, per-window-and-tier switch
  points (keyed like `5h.max5x`; see [auto-switch.md#switch-points](auto-switch.md#switch-points)),
  and the `hold5h`/`hold7d`/`cooldown` durations. Absent means every
  default.
- `notify` — desktop notifications on or off. Absent means on.
- `names` — session naming: `model` or `off`; absent means `on`. Set it with
  `chottag names`.
- `policy` — how new sessions are placed: `spread` puts each on the account
  with most headroom and keeps it there; absent means `serial`, one serving
  account. Set it with `chottag policy`.
- `pin` — the account `chottag tag NAME` pins new sessions to under
  `spread`; it has no effect under `serial`. Absent means unpinned.
- `pools` — every pool other than `default`, by name (`a-z`, `0-9` and `-`,
  1 to 16 characters), each with its own `serving`, `remote`, `policy` and
  `pin`. The top-level `serving`, `remote`, `policy` and `pin` are the
  `default` pool's, so a file with no extra pool is exactly as before.
  Change them with `chottag pool` and the commands that take `--pool`.
  `version` is `2` while at least one extra pool exists and `1` otherwise: a
  chottag older than 0.8.0 refuses a version 2 file rather than serving every
  account from `default`. Remove the extra pools (`chottag pool rm`) before
  rolling back below 0.8.0; `chottag update --version` refuses until you do.
- `label` — a short name for this install (`chottag setup --label NAME`),
  shown in `chottag status` and in notification titles. Absent means none.
- `updates` — the update-check settings. `check` turns the daemon's release
  check on or off (absent means on); `auto` lets it install a new release by
  itself (absent means off, and turning it on also turns `check` on);
  `restart` lets the daemon restart itself, once idle, onto an installed
  newer version (absent means on).
- `trace` — the trace-mode window; absent means tracing is off.

Change these with `chottag` commands (`chottag plan`, `chottag auto`,
`chottag names`, `chottag notify`, `chottag policy`, `chottag trace`, and so on), not by hand: a command
validates what it writes, and a hand edit that gets the shape wrong is a
corrupt `state.json` on the next read.

## Environment variables

| Variable | Set by | What it does |
| --- | --- | --- |
| `CHOTTAG_HOME` | You, before running `chottag` or `claude`. | Overrides the chottag home from its default of `~/.chottag`. |
| `CHOTTAG_BYPASS` | You, for one invocation. | `CHOTTAG_BYPASS=1 claude` skips the shim entirely and runs the real Claude Code directly, unmanaged. Inside a chottag session, the inherited `HTTPS_PROXY` still routes it through chottag, counted as that session; unset `HTTPS_PROXY` as well to leave chottag entirely. `claude auth ...` and `claude setup-token` need no variable: they skip chottag on their own and drop chottag's own `HTTPS_PROXY` (only when `auth` or `setup-token` is the first argument). |
| `CHOTTAG_NAME_SESSIONS` | You, in the environment Claude Code runs in. | An override of the mode `chottag names` sets, for one environment. `0` makes the `chottag name-session` hook print nothing. Any other value (including the removed `model`), or unset, leaves the stored mode in charge. |
| `CLAUDE_CONFIG_DIR` | You (Claude Code's own variable). | Where Claude Code keeps its projects. `chottag sessions`, `chottag resume --pick` and `chottag name-session` read a session's title from `projects/` there; unset, it is `~/.claude`. chottag reads only the `ai-title` and `custom-title` records. |
| `CHOTTAG_POOL` | You, before running `claude`. | Picks the pool this session runs in, e.g. `CHOTTAG_POOL=work claude`; the session's proxy credential carries it (`chottag.work.<sid>`). Unset or empty means `default`. A name that is not a pool is refused with `chottag: no pool named "x" (chottag pool lists them)`, exit 2, and Claude Code is not started; it never falls back to `default`. An alias keeps the choice: `alias cwork='CHOTTAG_POOL=work claude'`. A running daemon older than 0.8.0 cannot read a version 2 `state.json` (its requests would go out on Home's own login), so once an extra pool exists the shim refuses to start any session against it, `default` included: `chottag: the running daemon (0.7.1) predates pools, so a "work" session would not stay in its pool; run: chottag daemon restart`, exit 2. With no extra pool nothing changes. |
| `HTTPS_PROXY` | The `claude` shim, on every `claude` invocation it manages. | Points Claude Code's HTTPS traffic at chottag's local proxy; the URL also carries this session's proxy credential (`chottag.default.<sid>` and a password derived from the install's secret). |
| `NODE_EXTRA_CA_CERTS` | The `claude` shim. | Points Node (and so Claude Code) at chottag's CA certificate, so the proxy's intercepted HTTPS connections validate. If you already had this set, the shim merges your CA into `ca/bundle.pem` and points there instead. |
| `CHOTTAG_UPSTREAM_PROXY` | The `claude` shim, internally, to hand the daemon its upstream. | Not for you to set directly — see [An existing HTTPS_PROXY](#an-existing-https_proxy) and `daemon run --upstream-proxy`. |
| `CHOTTAG_AUTO_UPDATE` | The daemon, in the environment of its own install child. | `CHOTTAG_AUTO_UPDATE=1` makes `chottag update` skip its "What's new" request and its plugin step: nobody reads them in an unattended install. Don't set it yourself. |
| `CHOTTAG_NO_UPDATE_CHECK` | You, in the daemon's environment. | `CHOTTAG_NO_UPDATE_CHECK=1` stops the daemon's update check for a new release (see [Updating](updating.md#the-update-check)); the same as `chottag update --auto-check off`, without changing `state.json`. |
| `HOME` | Your shell. | Where `~/.chottag` resolves when `CHOTTAG_HOME` is unset, and where `setup`/`uninstall` look for your shell rc file. |
| `PATH` | Your shell. | Where the shim finds the real `claude` binary to run, once its own `bin/` entry is skipped. |
| `SHELL` | Your shell. | Tells `setup`/`uninstall` which rc file to edit: `~/.zshrc` for zsh, `~/.bashrc` for bash, and neither for anything else. |
| `CMUX_SURFACE_ID` | cmux, inside a cmux surface. | Tells the shim it is running inside cmux, one of the conditions for the cmux hand-off below. |
| `CMUX_CLAUDE_WRAPPER_SHIM` | cmux, inside a cmux surface. | Names cmux's own claude wrapper; the shim hands off to it once (see [Known limitations](known-limitations.md#cmux)). |
| `CMUX_CLAUDE_PID` | cmux's own claude wrapper. | Lets the shim tell whether cmux's wrapper already exec'd it directly, so it hands off at most once per launch. |
| `CMUX_CLAUDE_HOOKS_DISABLED` | cmux, inside a cmux surface. | When `1`, cmux's own wrapper adds no session hooks, so the shim skips the hand-off — it would exec the wrapper for nothing. |
| `CMUX_CUSTOM_CLAUDE_PATH` | cmux's own "Claude Binary Path" setting, when you have set one. | The skip applies when it names any executable other than chottag's own `bin/claude`: cmux's wrapper then never comes back to chottag, so the shim skips the hand-off too (see [Known limitations](known-limitations.md#cmux)). |
| `CMUX_WORKSPACE_ID` | cmux, inside a cmux workspace. | When set, `chottag statusline --cmux` also sets a cmux sidebar pill for that workspace (see [`chottag statusline`](commands.md#chottag-statusline)). |
| `NO_COLOR` | You. | When non-empty, `chottag statusline` prints no colour. |
| `CHOTTAG_CMUX_HANDOFF` | The `claude` shim, internally, across its own cmux hand-off. | Not for you to set directly — marks a launch that already handed off once, so the shim does not loop; dropped before Claude Code itself starts. |

## The port

The daemon binds `127.0.0.1:47821` by default. `state.json`'s `port` field
changes it.

If the port is already taken by something else, `daemon run` fails
immediately rather than silently moving to another port — the port is
fixed and never rediscovered, since `HTTPS_PROXY` is set once, at `claude`
exec time. See
[Troubleshooting](troubleshooting.md) and
[the packaging README's "When the port stays taken"](https://github.com/HaiNNT/c-hottag/blob/main/packaging/README.md#when-the-port-stays-taken)
for how to find and free it.

## An existing HTTPS_PROXY

If your shell already has `HTTPS_PROXY` set before `claude` first starts a
chottag daemon, that value becomes the daemon's own upstream proxy — the
proxy it uses to reach the real Anthropic API, on top of the interception
it does locally. The daemon accepts an upstream only at startup, so a later
shell with a *different* `HTTPS_PROXY` gets a clear, fatal error rather
than silently having its traffic routed somewhere it did not choose.

To set the upstream explicitly instead of relying on the shell's
environment, use [`daemon run --upstream-proxy`](commands.md#chottag-daemon-run).
