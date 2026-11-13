# Contributing

Bug reports, ideas and pull requests are welcome.

## Setup

You need Go 1.26 or newer and git. If you change `.templ` files you also need
[templ](https://templ.guide).

```bash
git clone https://github.com/jeeqe/agentbox
cd agentbox
make build
make test
```

## Before you open a pull request

```bash
gofmt -l .                  # should print nothing
templ generate              # then check `git diff` is empty: generated files are committed
golangci-lint run ./...
go test -race ./...
```

CI runs the same four checks.

## Where things are

- `internal/worktree`: git worktrees, merging, diffs
- `internal/runner`: starts agents, captures their output
- `internal/store`: SQLite
- `internal/app`: the operations the CLI and dashboard share
- `internal/server` and `internal/ui`: the dashboard (net/http, templ, htmx)
- `cmd/agentbox`: the command line

## Style

Keep changes small and explain the why in the commit message. Tests that use a real git
repository in a temp directory and a fake agent (`--agent 'cmd:...'`) are preferred to mocks.
The dashboard has no build step and loads nothing from the network, so please keep it that way.

## Adding an agent

Built-in agents are a short table in `internal/runner/supervisor.go`: the binary name and the
arguments that make it run a prompt without asking questions. Check the flags against the real
tool before sending a pull request.
