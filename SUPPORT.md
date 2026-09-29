# Getting help

c-hottag is maintained by one person in their spare time. Help is best
effort: there is no support promise and no response time.

## Something isn't working

1. Run `chottag doctor`. It checks the install and says what to run for
   each problem; `chottag doctor --fix` repairs what it safely can.
2. Read [Troubleshooting](docs/troubleshooting.md) and the
   [FAQ](docs/faq.md). If chottag itself is in your way,
   `CHOTTAG_BYPASS=1 claude` starts Claude Code without it.
3. Still stuck? Open an issue from
   [the issue chooser](https://github.com/HaiNNT/c-hottag/issues/new/choose):
   a **Bug report**, or **Route drift** if a Claude Code update changed which
   account a request uses.

Questions and ideas go through Issues too: the **Feature idea** form.

Every issue is public. Keep it to **no personal data or tokens**: remove
emails, account and org names, home paths, session links, and every token,
`proxy.secret` or `ca.key` before you paste output.

## Not here

- **A security problem:** report it privately, as [SECURITY.md](SECURITY.md)
  describes, never in an issue.
- **Your Claude account, plan, billing or usage limits:** Anthropic support,
  at https://support.claude.com. c-hottag can't see or change any of them.
