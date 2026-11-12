# Recording the demo GIF

Goal: a 10 second clip for the top of the README. It needs a real screen, a real repo, and
the agent CLIs logged in, so it has to be recorded on your own machine.

## Setup

- A small repo with a test suite that finishes in a few seconds. Use a terminal font around
  18pt and a browser window about 1280x720 so text stays readable in a GIF.
- Run `agentbox clean` first so ports start at 3001.
- Dry-run the whole thing once so the agents' first-run prompts are out of the way.
- Tools: OBS, Screen Studio or CleanShot X for the capture, then `ffmpeg` to convert.

## Sequence

1. 0:00 to 0:03. In the terminal run:
   `agentbox race "implement JWT refresh rotation" --agents claude,codex,aider --test "go test ./..."`
   Three worktrees appear in the summary table.
2. 0:03 to 0:07. The browser opens on `localhost:8484`. Three panes stream output, the
   `RUNNING` pills pulse, and the `+/-` counters climb as each agent finishes.
3. 0:07 to 0:10. Click **Inspect Diff** on the fastest agent with a `TESTS PASSED` pill, close
   it, then click **Merge & Cleanup**. The cards disappear and `git log` shows the merge.

If the real agents are too slow for ten seconds, trim in editing rather than speeding up the
terminal text.

## Converting

```bash
ffmpeg -i demo.mov -vf "fps=30,scale=1280:-1:flags=lanczos,split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse" -loop 0 docs/demo.gif
```

Keep the file under about 8 MB. If it's bigger, lower `fps` to 20 or `scale` to 1000.
Then replace the comment near the top of `README.md` with `![demo](docs/demo.gif)`.
