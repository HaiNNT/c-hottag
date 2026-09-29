---
name: upstream-check
description: Check that chottag still works after a Claude Code update or a new model release. Use when Claude Code updated, a new Claude Code version or new model is released, or the user asks to verify chottag still works, check for route drift, or check for version drift. Never touches the maintainer's real ~/.chottag or ~/.claude*; everything runs inside scripts/dev-env's own sandbox.
---

# upstream-check: verify chottag against a new Claude Code or model

This is the repeatable form of the maintainer's manual release check's
route-trace step: a route trace, `trace summarize` into
`routes/<version>.txt`, and a `routes4` diff. It runs entirely inside
`scripts/dev-env`'s sandbox (`$HOME/chottag-dev`, its own port, its own
logins) so it never touches the owner's daily chottag install (R91).

## Steps

1. **Detect.** Run `scripts/detect`. It prints the installed Claude Code
   version, the highest routes table this repo already has, whether that
   table covers the installed version, the prod chottag's own
   version-drift verdict (read-only, if one is on PATH), and whether the
   dev sandbox is built and has any logins.

2. **Precondition: a dev login.** If `detect` shows `dev_env: not built`,
   run `scripts/dev-env build`. Then check for a dev login (`dev_env: built,
   N account(s)`, or `scripts/dev-env run status --json`). If there are
   none, **STOP** and ask the owner to run `scripts/dev-env run login NAME`
   themselves, in a browser, with a personal account — **never** the
   company Team account. You may launch that login command for them only
   when they ask you to; never supply the credentials yourself.

3. **Trace.** Once a dev login exists, run
   `scripts/trace --models M1,M2,...` with the model ids worth checking
   (e.g. the ones the new release shipped). It runs one non-interactive
   step per model (plus a plain session-start step) through the dev shim,
   summarizes the routes it saw into `routes/<version>.txt`, and prints a
   result line per step plus a privacy check.

4. **Diff.** Run `scripts/diff-routes routes/<version>.txt` (OLD defaults
   to the highest routes table below the new version) or
   `scripts/diff-routes OLD.txt NEW.txt` explicitly. It prints a table —
   `STATUS KIND +/- METHOD HOST PATH CLASS` — and exits 1 if any line is
   `REVIEW`.

5. **Explain each flagged line.** A `REVIEW` line for a new or changed
   object route (`/v1/code/sessions`, `/v1/sessions`, `/v1/environments`,
   `/api/frame`, `/v1/code/triggers`, `/v1/mcp_servers`, or
   `mcp-proxy.anthropic.com`), or a route whose CLASS changed, becomes a
   roadmap Findings row and a router task — it needs a rule before this
   check can pass clean. An `OK` or `INFO` line needs no action.

6. **Commit** the new `routes/<version>.txt` (or `.txt.new`, if one already
   existed and `--force` wasn't given).

7. **Report.** Write a dated report file, named `<YYYY-MM-DD>-cc-<version>.md`,
   under this checkout's own private checks log if it has one (ask the
   maintainer where, otherwise), with the `detect` output, the diff table
   with each line explained, the model step results, and the privacy
   check's result (must be 0 for both files).

8. **Optional interactive follow-ups**, from the manual release check, need
   an interactive session and the owner: `/usage`, `/remote-control` with
   the phone, an artifact publish, `/schedule`, a connector call. Offer
   them; don't run them without the owner.

## Safety

- Never the prod chottag, except `doctor --json` and `status --json`
  (read-only). Never `scripts/dev-env build|shell|run <mutating
  command>|destroy` from here except through `scripts/trace` itself.
  Never touch `~/.chottag`, `~/.claude*` or `~/.zshrc`. Never bind 47821.
- Never Home Claude: every traced session runs with `CLAUDE_CONFIG_DIR` set
  to a dev slot directory, so nothing is written under `~/.claude`.
- Never print a token, a request/response body, or `HTTPS_PROXY` (it
  carries the proxy's caller secret).
- The privacy check (`grep -cE 'sk-ant-|Bearer '` on the routes file and the
  dev trace log) must print 0 for both. `scripts/trace` dies loudly
  otherwise — treat that as a stop-everything signal, not something to
  work around.
