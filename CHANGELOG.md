# Changelog

## v0.1.1

Fixes and hardening found in the first real races.

- Agent output no longer shows raw terminal escape codes (the `□[0m` under a lane). Codes are
  stripped before logs are stored, and progress bars that redraw a line keep only the last draw.
- A very long output line no longer blocks the agent. Lines over 64 KB are cut and marked.
- Running agents show the command that started them until their first output arrives, instead
  of a blank line. Blank output lines are skipped.
- The dashboard refuses requests with a non-local `Host` header and state-changing requests
  from other sites, so another web page can't merge or discard your branches.
- Tasks left "running" by a crash or `kill -9` are marked stopped the next time you run any
  `agentbox` command, instead of staying running forever.
- A task run by a different `agentbox` process in the same repo now streams its output and
  status live, and shows that it can't be stopped from here.
- `--base <branch>` on `run` and `race`.
- A short confirmation after merging or discarding ("Merged claude into main. Removed 2 other
  worktrees.").
- `agentbox` refuses to start on a detached HEAD, and says which branch to check out when a
  merge target isn't checked out.
- Log history is capped at the newest 20,000 lines per task, and removed when a task is merged
  or discarded.
- Favicon, run counts in the browser tab title, and the status summary is no longer re-read by
  screen readers every second.
- Homebrew installs clear the macOS quarantine flag so the first run isn't blocked.
- Databases from v0.1.0 upgrade automatically.

## v0.1.0

First release: `run` and `race`, per-agent worktrees with `.env` linking and ports, a live
dashboard with inline diffs, one-click merge and cleanup.
