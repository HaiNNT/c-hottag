# c-hottag compared

Last verified: 2026-09-29.

Other projects change quickly: releases ship, stars move, a README's wording
changes. Every claim below is dated and cites the source it came from, so
verify anything that matters to you before you rely on it.

## What chottag is

chottag switches between several of your own Claude subscription logins for
Claude Code on one machine, without logging out. Each account keeps its own
slot; a local proxy picks the bearer per request, so a running session can
change which account serves it mid-conversation. claude.ai objects (Remote
Control sessions, artifacts, connectors) stay with whichever account owns
them, not with whichever account is currently serving; routines are the
exception, always following whichever account is currently remote. It is
local and single-user: no third-party relay; requests go only to Anthropic's
own endpoints.

## At a glance

| Tool | Category | How it switches | Where logins live | Restart needed | Source |
|---|---|---|---|---|---|
| chottag | — | picks the bearer per request, in a local proxy | `~/.chottag/accounts/<name>` (or `$CHOTTAG_HOME`), each logged in by Claude Code itself |  no |  |
| claude-swap | switcher | copies the login into its own store, then writes it back into Claude Code's live spot on switch | the macOS Keychain plus `~/.claude-swap-backup/`, or an XDG data dir on Linux | usually not (about 30s on macOS) | [GitHub](https://github.com/realiti4/claude-swap) |
| swapdex | switcher | a permanent `CLAUDE_CONFIG_DIR` slot per account; a local proxy picks the account per request | swapdex's data dir, or a directory adopted in place | no | [GitHub](https://github.com/youdie006/swapdex) |
| clauth | switcher | swaps a per-profile snapshot of the credentials file and the settings env block into Claude Code's live files | `~/.clauth/` | not stated for the default switch | [GitHub](https://github.com/uwuclxdy/clauth) |
| claude-account | switcher | a separate config and credential-storage directory per profile, plus a shim | its own profiles directory (`~/Library/Application Support/claude-account/…` or `~/.local/share/claude-account/…`) | yes, new processes only | [GitHub](https://github.com/hamzarehmandeveloper/claude-account) |
| CCSwitcher | switcher, macOS menu bar | backs up the token and the `oauthAccount` block, writes them into the Keychain and `~/.claude.json` on switch | not stated | no, the next API call picks it up | [GitHub](https://github.com/XueshiQiao/CCSwitcher) |
| Claude Account Switcher | switcher, macOS menu bar | backs up each account in the Keychain, restores the chosen one into Claude Code's active slot | the macOS Keychain | not stated | [GitHub](https://github.com/Symbioose/claude-account-switcher) |
| aisw | switcher | restores the saved credentials into Claude Code's native locations | `~/.aisw/`, or the OS keyring where available | yes, start a fresh process | [GitHub](https://github.com/burakdede/aisw) |
| caam | switcher | backs up and restores Claude Code's own auth files in place, one account active per tool | Claude Code's own files, plus caam's backups | no; re-runs the wrapped command on a rate limit | [GitHub](https://github.com/Dicklesworthstone/coding_agent_account_manager) |
| Claude Code Router | proxy/gateway | a local model gateway that routes to configured providers and models by API key | its own provider config | not documented | [GitHub](https://github.com/musistudio/claude-code-router) |
| CC Switch | proxy/gateway | rewrites Claude Code's provider config; an optional local router converts API formats | `~/.cc-switch` | no for provider data; yes, plus its login flow, to return to the official provider | [GitHub](https://github.com/farion1231/cc-switch) |
| claude-code-proxy | proxy/gateway | translates Anthropic API requests to OpenAI or Gemini | an OpenAI or Gemini API key | n/a | [GitHub](https://github.com/1rgs/claude-code-proxy) |
| LiteLLM proxy | proxy/gateway | Claude Code points at the proxy; the proxy holds provider API keys in its own config | the proxy's `config.yaml` | n/a | [docs](https://docs.litellm.ai/docs/tutorials/claude_responses_api) |
| OpenRouter | proxy/gateway, hosted | Claude Code points at OpenRouter's endpoint with an OpenRouter API key | an OpenRouter account | n/a | [docs](https://openrouter.ai/docs/guides/guides/claude-code-integration) |
| Portkey | proxy/gateway, hosted | Claude Code points at Portkey's endpoint with a Portkey API key | a Portkey account | n/a | [docs](https://portkey.ai/docs/integrations/libraries/claude-code) |
| Manual `CLAUDE_CONFIG_DIR` | config-dir script | a shell alias sets the variable per account; Claude Code logs each dir in itself | the dir itself, e.g. `~/.claude-work` | yes, another terminal or process | [docs](https://code.claude.com/docs/en/env-vars) |
| Claude Switch | config-dir script | creates a `~/.claude-<name>/` directory per account, launches Claude Code with `CLAUDE_CONFIG_DIR` set | the profile directory | yes, one account per process | [GitHub](https://github.com/SaschaHeyer/claude-switch) |

## Switchers

Most switchers for Claude Code work by copying or overwriting Claude Code's
own live login when you switch: [claude-swap](https://github.com/realiti4/claude-swap),
[clauth](https://github.com/uwuclxdy/clauth),
[CCSwitcher](https://github.com/XueshiQiao/CCSwitcher) and
[Claude Account Switcher](https://github.com/Symbioose/claude-account-switcher)
(both macOS menu-bar apps), [aisw](https://github.com/burakdede/aisw) and
[caam](https://github.com/Dicklesworthstone/coding_agent_account_manager).
[swapdex](https://github.com/youdie006/swapdex) and
[claude-account](https://github.com/hamzarehmandeveloper/claude-account)
instead give each account its own directory that Claude Code logs into
itself: swapdex's slot mode does this without copying a credential, behind a
local proxy that also picks the account per request (the closest design to
chottag's) — its older snapshot mode instead copies a login into its own
data dir and writes it back into Home; claude-account never copies a
credential, with a shim that starts a fresh Claude Code process per account. [cswap-pin](https://github.com/codeslake/cswap-pin), a companion to
claude-swap, adds the same idea as chottag's `remote` account: it keeps
claude.ai objects on one account while a switch moves who serves inference.

## Auto-switch and the pin

The README's short table (as of 2026-09-29) rests on this.

**Auto-switch at a usage limit.** claude-swap: `cswap auto` lets it "watch your usage and switch for
you" before a threshold (default 90%) is hit ([README](https://github.com/realiti4/claude-swap)).
clauth: a fallback chain "switches to the next member with headroom the moment the active one
crosses its threshold" ([README](https://github.com/uwuclxdy/clauth)). caam: `caam run` "wraps your
AI CLI execution and automatically handles rate limits", reactively, once a limit is hit
([README](https://github.com/Dicklesworthstone/coding_agent_account_manager)); switching outside
that wrapper is manual, and the README itself warns that switching while a CLI is running "may
cause auth errors in the running session" — best done "before starting a new session, not during."
CCSwitcher's README lists every feature in detail and names none that switches automatically at a
limit: not stated ([README](https://github.com/XueshiQiao/CCSwitcher)).

**Pinning Remote Control, connectors and artifacts to one account.** claude-swap, clauth, caam and
CCSwitcher each switch by swapping Claude Code's whole login (see "How it switches" in the table
above, from each tool's own README), so every claude.ai request, Remote Control sessions, connectors
and artifacts included, moves to the new account with it: none of the four pins them. None of their
READMEs claims otherwise, and a search of each tool's source (2026-09-29) found no code that routes
Remote Control, connector or artifact requests to a different account than inference. clauth's
gateway is a managed API-key gateway, not a per-request account router.
[cswap-pin](https://github.com/codeslake/cswap-pin), an add-on to claude-swap, adds a pin
explicitly: it keeps "Claude Code's Remote Control and Artifacts on one account while inference
keeps following cswap's account swap" — the same idea as chottag's `remote` account, which chottag
has built in. claude-swap itself does not ship it: [PR #210](https://github.com/realiti4/claude-swap/pull/210),
which would add it as an optional extra, is still open (checked 2026-09-29).

## Proxies and gateways

These point Claude Code at a different API endpoint entirely — a model
router, a translation layer, or a hosted gateway billed by its own key —
rather than switching between your own Claude subscription accounts:
[Claude Code Router](https://github.com/musistudio/claude-code-router),
[CC Switch](https://github.com/farion1231/cc-switch),
[claude-code-proxy](https://github.com/1rgs/claude-code-proxy), the
[LiteLLM proxy](https://docs.litellm.ai/docs/tutorials/claude_responses_api),
[OpenRouter](https://openrouter.ai/docs/guides/guides/claude-code-integration)
and [Portkey](https://portkey.ai/docs/integrations/libraries/claude-code).
Anthropic's docs note that pointing `ANTHROPIC_BASE_URL` away from
`api.anthropic.com` turns off Remote Control, so combine one of these with
chottag carefully.

## Config-dir scripts

Anthropic documents `CLAUDE_CONFIG_DIR` itself, with the pattern of a shell
alias per account ([docs](https://code.claude.com/docs/en/env-vars));
chottag's account slots use the same mechanism, plus a proxy on top.
[Claude Switch](https://github.com/SaschaHeyer/claude-switch) wraps the same
pattern in a small CLI. Both start a fresh Claude Code process per account,
with no mid-session switch and no auto-switch. [ccusage](https://github.com/ccusage/ccusage)
and the [Claude Code Usage Monitor](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor)
read local usage logs instead of switching accounts, and can run alongside
chottag.

## Not this

chottag only switches between Claude subscription accounts you own and have
logged into yourself, on your own machine; it is not a pooling or reselling
relay that shares access to accounts across people.

## Moving to chottag

- [From a manual `CLAUDE_CONFIG_DIR` alias](migrating/claude-config-dir.md)
- [From claude-swap](migrating/claude-swap.md)
- [From swapdex](migrating/swapdex.md)
- [From clauth](migrating/clauth.md)

## Sources

- claude-swap: [realiti4/claude-swap](https://github.com/realiti4/claude-swap) (accessed 2026-09-29)
- cswap-pin: [codeslake/cswap-pin](https://github.com/codeslake/cswap-pin) (accessed 2026-09-29)
- claude-swap PR #210, open: [realiti4/claude-swap#210](https://github.com/realiti4/claude-swap/pull/210) (accessed 2026-09-29)
- swapdex: [youdie006/swapdex](https://github.com/youdie006/swapdex) (accessed 2026-09-28)
- clauth: [uwuclxdy/clauth](https://github.com/uwuclxdy/clauth) (accessed 2026-09-29)
- claude-account: [hamzarehmandeveloper/claude-account](https://github.com/hamzarehmandeveloper/claude-account) (accessed 2026-09-28)
- CCSwitcher: [XueshiQiao/CCSwitcher](https://github.com/XueshiQiao/CCSwitcher) (accessed 2026-09-29)
- Claude Account Switcher: [Symbioose/claude-account-switcher](https://github.com/Symbioose/claude-account-switcher) (accessed 2026-09-28)
- aisw: [burakdede/aisw](https://github.com/burakdede/aisw) (accessed 2026-09-28)
- caam: [Dicklesworthstone/coding_agent_account_manager](https://github.com/Dicklesworthstone/coding_agent_account_manager) (accessed 2026-09-29)
- Claude Code Router: [musistudio/claude-code-router](https://github.com/musistudio/claude-code-router) (accessed 2026-09-28)
- CC Switch: [farion1231/cc-switch](https://github.com/farion1231/cc-switch) (accessed 2026-09-28)
- claude-code-proxy: [1rgs/claude-code-proxy](https://github.com/1rgs/claude-code-proxy) (accessed 2026-09-28)
- LiteLLM proxy: [Claude Code tutorial](https://docs.litellm.ai/docs/tutorials/claude_responses_api) (accessed 2026-09-28)
- OpenRouter: [Claude Code integration](https://openrouter.ai/docs/guides/guides/claude-code-integration) (accessed 2026-09-28)
- Portkey: [Claude Code integration](https://portkey.ai/docs/integrations/libraries/claude-code) (accessed 2026-09-28)
- Claude Switch: [SaschaHeyer/claude-switch](https://github.com/SaschaHeyer/claude-switch) (accessed 2026-09-28)
- ccusage: [ccusage/ccusage](https://github.com/ccusage/ccusage) (accessed 2026-09-28)
- Claude Code Usage Monitor: [Maciek-roboblog/Claude-Code-Usage-Monitor](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor) (accessed 2026-09-28)
- Anthropic docs: [Environment variables](https://code.claude.com/docs/en/env-vars) (accessed 2026-09-28)
