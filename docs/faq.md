# FAQ

## Is this allowed?

c-hottag can't answer that for you. Anthropic's terms govern your accounts,
and the Claude Code legal page says "developers may not collect, store, or
intermediate Claude.ai credentials or session tokens" — the very thing
chottag's proxy does with each account's token. Read the README's
[Terms of use and risk](https://github.com/HaiNNT/c-hottag/blob/main/README.md#terms-of-use-and-risk)
and the pages it links, and decide for yourself. Use chottag only with
accounts that are yours, and never to share or resell access.

Auto-switch and the safety-net resend after a limit are the behaviours most
likely to fall outside "ordinary, individual usage". Anthropic may act at
its own discretion and without prior notice, and any action it takes may
reach every account linked to the same install, not only the one that
triggered it. The Usage Policy forbids getting around a ban "through the
use of a different account", so if an account is suspended, banned or on
hold, don't switch to another one to keep working through it. For a hold,
Anthropic's [error reference](https://code.claude.com/docs/en/errors)
says the account has been flagged for review: see
<https://claude.ai/restricted> for the details and the appeal, and wait
for the hold to be lifted.

## Does chottag change my ~/.claude?

No. `~/.claude` and `~/.claude.json` are Home, your normal Claude Code
login, and chottag never writes to them. It keeps every account it manages
in its own slot instead.

## Why a local CA?

chottag runs a local HTTPS proxy that swaps in the right account's token
before a request reaches Claude Code's servers. Doing that inside the TLS
tunnel needs a local CA, but it only issues certificates for the hosts a
tunnel actually targets. See [security.md](security.md) for the detail.

## What does it log?

`proxy.jsonl` records request metadata (host, path, which account, status)
for diagnosis; `chottag trace` uses `trace.jsonl` instead. Neither ever
logs tokens or request or response bodies.

## Where are its files?

Everything lives under `~/.chottag`, or under `CHOTTAG_HOME` if you set
it at install time. See [configuration.md](configuration.md) for the
layout.

## Does it work on Linux?

Linux builds and passes CI, but has not been used for real yet.

## Can I use it on several machines?

Yes. Each machine runs its own chottag install with its own logins;
nothing syncs between machines.

## Does it work with API keys or other models?

No. chottag switches between subscription logins for Claude Code only. See
[comparison.md](comparison.md) for how it differs from API-key or
multi-model tools.

## How do I turn auto-switch off?

`chottag auto off`. Switch it back on with `chottag auto on`.

## Does it work in the IDE extensions or the Desktop app?

No. See [known-limitations.md](known-limitations.md) for what is and
isn't routed.

## What happens to the prompt cache when it switches?

The new serving account has never seen the conversation before, so the
first message after a switch rebuilds its prompt cache.
