# Getting sessions back after a crash

When cmux quits or the Mac crashes, every open Claude Code session dies with
its terminal. This page covers what chottag keeps so you can bring them back.

## The picker already widens

Claude Code can find old sessions without chottag. Run `claude --resume`, then
press Ctrl+W to list the sessions of all worktrees, or Ctrl+A for all
projects. `claude --resume <id>` works from any directory, but it runs in the
directory you launch it from, not the one the session had.

## What chottag records

For each session its `claude` shim launches, chottag keeps:

- the directory;
- the pool;
- the cmux workspace and tab;
- the Claude Code session id it is using now. It follows `/clear` and
  `/resume`, so it is the id you would resume, not the first one.

The id `chottag resume` types is that one, except for a session started with
`claude --resume X`: it keeps writing X's conversation but sends a new id that has no
transcript, so chottag resumes X instead. A session started with
`--fork-session` writes a new conversation, so its own id is used. A session
started with `claude --continue` names no id, so the id chottag captured may
not be resumable.

The journal lives in `~/.chottag/sessions/` (or `$CHOTTAG_HOME/sessions/`), is
kept for 7 days or 500 sessions, and holds no tokens.

A session id is used only if it is a canonical UUID. Anything else is never
typed into a terminal.

## `chottag sessions` and `chottag resume`

`chottag sessions` lists the sessions that were lost together at the last
crash or cmux quit. A session is lost when claude and its shell died together.
A session you left with `/exit` is not lost. A session also counts only if chottag saw it send a request (so it has a session id), it ended within the last 24 hours and within 2 minutes of the newest lost session, and it is not already running again. `chottag sessions --all` lists
every recorded session and its state.

`chottag resume` relaunches the lost sessions, each in its own directory and
pool. It puts each one back in the first of these that works:

1. its own cmux tab, if the tab is still there and idle;
2. a new tab in the same workspace;
3. a new workspace, one for each old workspace, if the workspace is gone too;
4. nowhere: with no cmux, or no tab was recorded, it prints the command for you to run. chottag marks that session resumed too, because you were shown the command. If cmux is there but does not answer, it prints the commands and marks nothing, like `--print`.

If you run `chottag resume` from inside the tab it will restore, that tab counts as idle: cmux types the command there and your shell runs it when `chottag` exits.

It never types into a busy tab. If one session fails, the rest still go on,
and the failed one shows up again next time.

- `chottag resume --pick` lists the sessions and asks which to relaunch.
- `chottag resume --print` only prints the commands, and marks nothing, so a later `chottag resume` still offers them.
- Running `chottag resume` twice is safe: a resumed session is marked, and a
  session that is already running again is not started a second time.

`chottag status` shows a one-line hint while sessions are waiting (`2 sessions
were lost at 18:01: chottag resume`), and `lostSessions` in `--json`.

## Session names

A session Claude Code has not been told a name for shows up in its agent view
as `<folder>-3f`, which says little. With the chottag plugin installed, chottag
names each session for you through a `UserPromptSubmit` hook (`chottag
name-session`):

- at your first prompt, the branch you are on (`m12-owner-discovery`), or
  `<folder> HH:MM` on `main`, a detached HEAD or outside git (a resumed
  session with no name is named at its next prompt);
- at a later prompt, once Claude Code has generated the session's title,
  `<that> · <title>`.

A session you name yourself (`claude -n`, `/rename`, or by accepting a plan)
keeps your name: chottag never renames it, and it does nothing when it cannot see the session's current name. `chottag sessions` and `chottag
resume --pick` show a `TITLE` column, so a lost session is easy to recognise.

To read a title, chottag opens the session's transcript and looks only at its
`ai-title` and `custom-title` records. It never logs or stores a title; per
session it keeps only a hash of the name it set. Turn naming off with
`chottag names off`. Without the plugin, add the hooks to
`~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "chottag name-session", "timeout": 5 } ] }
    ]
  }
}
```

## With cmux's own restore

cmux can also resume agent sessions by itself. Turn off its
`terminal.autoResumeAgentSessions` setting (in cmux's Settings) so the two do not both resume a session. If you leave it on,
`chottag resume` usually skips a session cmux already brought back. That works only when cmux's resume goes through chottag's `claude`, because only then does chottag see the session running again.

## Limits

- Only sessions launched through chottag's `claude` are known.
- A session you moved with `/cd` comes back in the directory it was launched
  in.
- A reused process id can hide one session until that process exits. After a reboot this cannot happen: sessions from before it are always treated as lost.
- A tab you closed while claude ran counts as lost for 24 hours.
- The session id is recorded up to 15 seconds after it changes, so a `/clear` just before a crash resumes the conversation from before it.
- A session is named by branch only until Claude Code has generated its title, which is after the first prompt, so the title shows from the second prompt on. The transcript format is internal to Claude Code: if it changes, the title is empty and the name stays the branch.
- Only one `chottag resume` runs at a time; a second one stops with an error.
- Outside cmux, or for a session with no recorded tab, `chottag resume` prints the command instead of running it, and marks the session resumed. Only `--print` marks nothing.

See [`commands.md#sessions`](commands.md#sessions) for every flag and the
`--json` fields.
