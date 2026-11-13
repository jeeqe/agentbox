// Package app is the service layer shared by the CLI and the web server.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jeeqe/agentbox/internal/runner"
	"github.com/jeeqe/agentbox/internal/store"
	"github.com/jeeqe/agentbox/internal/worktree"
)

// App wires the store, worktree manager and supervisor together for one repository.
type App struct {
	Store *store.Store
	WT    *worktree.Manager
	Sup   *runner.Supervisor
	// Base is the branch new worktrees are cut from and merged back into. Empty
	// means whatever branch the repository has checked out when a run starts.
	Base string
}

// DataDir returns ~/.agentbox (or $AGENTBOX_HOME), creating it if needed.
func DataDir() (string, error) {
	dir := os.Getenv("AGENTBOX_HOME")
	if dir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(h, ".agentbox")
	}
	return dir, os.MkdirAll(dir, 0o755)
}

// Open builds an App for the repository containing repoDir.
func Open(repoDir, testCmd string, parallelism int) (*App, error) {
	wt, err := worktree.New(repoDir)
	if err != nil {
		return nil, err
	}
	dir, err := DataDir()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(dir, "agentbox.db"))
	if err != nil {
		return nil, err
	}
	a := &App{Store: st, WT: wt, Sup: runner.NewSupervisor(st, wt, testCmd, parallelism)}
	a.ReconcileOrphans(context.Background())
	return a, nil
}

// ReconcileOrphans marks tasks as stopped when the process that was running
// them is gone (a crash, kill -9 or reboot). Without this they would show as
// running forever, with nothing able to stop them.
func (a *App) ReconcileOrphans(ctx context.Context) {
	active, err := a.Store.ActiveTasks(ctx)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, t := range active {
		if t.Status != store.StatusRunning && t.Status != store.StatusQueued {
			continue
		}
		if t.OwnerPID == self || processAlive(t.OwnerPID) {
			continue
		}
		_ = a.Store.MarkFinished(ctx, t.ID, store.StatusKilled, -1)
	}
}

func (a *App) Close() error { return a.Store.Close() }

// Spec is one agent+prompt pair to launch.
type Spec struct{ Agent, Prompt string }

// FanOut creates one run with one task per prompt, all using agent.
func (a *App) FanOut(ctx context.Context, agent string, prompts []string) (*store.Run, []store.Task, error) {
	specs := make([]Spec, len(prompts))
	for i, p := range prompts {
		specs[i] = Spec{agent, p}
	}
	return a.launch(ctx, "fan-out", "", specs)
}

// Race creates one run with the same prompt across several agents.
func (a *App) Race(ctx context.Context, prompt string, agents []string) (*store.Run, []store.Task, error) {
	if len(agents) < 2 {
		return nil, nil, errors.New("race needs at least two agents")
	}
	specs := make([]Spec, len(agents))
	for i, ag := range agents {
		specs[i] = Spec{ag, prompt}
	}
	return a.launch(ctx, "race", prompt, specs)
}

func (a *App) launch(ctx context.Context, kind, prompt string, specs []Spec) (*store.Run, []store.Task, error) {
	base := a.Base
	if base == "" {
		var err error
		if base, err = a.WT.CurrentBranch(ctx); err != nil {
			return nil, nil, err
		}
	} else if !a.WT.BranchExists(ctx, base) {
		return nil, nil, fmt.Errorf("--base %q is not a local branch", base)
	}
	if base == "HEAD" {
		return nil, nil, errors.New("the repository is on a detached HEAD; check out a branch first so results have somewhere to merge")
	}
	run := &store.Run{Kind: kind, Prompt: prompt, RepoPath: a.WT.Repo, BaseRef: base}
	if err := a.Store.CreateRun(ctx, run); err != nil {
		return nil, nil, err
	}
	used := map[int]bool{}
	if active, err := a.Store.ActiveTasks(ctx); err == nil {
		for _, t := range active {
			used[t.Port] = true
		}
	}
	var tasks []store.Task
	for i, sp := range specs {
		label := sp.Prompt
		if kind == "race" {
			label = sp.Agent
		}
		info, err := a.WT.Create(ctx, run.ID, i+1, label, base, used)
		if err != nil {
			for _, t := range tasks { // roll back partial run
				_ = a.WT.Remove(ctx, t.WorktreePath, t.Branch)
				_ = a.Store.SetStatus(ctx, t.ID, store.StatusDiscarded)
			}
			return nil, nil, fmt.Errorf("create worktree %d: %w", i+1, err)
		}
		used[info.Port] = true
		t := store.Task{RunID: run.ID, Agent: sp.Agent, Prompt: sp.Prompt, Branch: info.Branch, WorktreePath: info.Path, Port: info.Port}
		if err := a.Store.CreateTask(ctx, &t); err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, t)
	}
	for _, t := range tasks {
		a.Sup.Start(t, base)
	}
	return run, tasks, nil
}

// Restart re-runs a finished task in its existing worktree.
func (a *App) Restart(ctx context.Context, id int64) error {
	t, run, err := a.taskAndRun(ctx, id)
	if err != nil {
		return err
	}
	if a.Sup.IsRunning(id) {
		return errors.New("task is already running")
	}
	a.Sup.Start(*t, run.BaseRef)
	return nil
}

// Kill stops a running task.
func (a *App) Kill(id int64) bool { return a.Sup.Kill(id) }

// MergeAndCleanup merges the task branch into the run's base branch, removes its
// worktree and branch, and (for races) discards the losing siblings.
// On conflict the task is marked "conflict" and ErrConflict is returned.
func (a *App) MergeAndCleanup(ctx context.Context, id int64) error {
	t, run, err := a.taskAndRun(ctx, id)
	if err != nil {
		return err
	}
	if a.Sup.IsRunning(id) {
		return errors.New("task is still running")
	}
	if patch, st, err := a.WT.Snapshot(ctx, t.WorktreePath, run.BaseRef); err == nil {
		_ = a.Store.SaveDiff(ctx, id, patch)
		_ = a.Store.SetDiffStats(ctx, id, st.Additions, st.Deletions)
	}
	if err := a.WT.Merge(ctx, t.Branch, run.BaseRef); err != nil {
		if errors.Is(err, worktree.ErrConflict) {
			_ = a.Store.SetStatus(ctx, id, store.StatusConflict)
		}
		return err
	}
	_ = a.WT.Remove(ctx, t.WorktreePath, t.Branch)
	_ = a.Store.ClearLogs(ctx, id)
	_ = a.Store.SetStatus(ctx, id, store.StatusMerged)
	if run.Kind == "race" {
		sibs, _ := a.Store.TasksForRun(ctx, run.ID)
		for _, s := range sibs {
			if s.ID != id && s.Status != store.StatusMerged && s.Status != store.StatusDiscarded {
				_ = a.Discard(ctx, s.ID)
			}
		}
	}
	return nil
}

// Discard kills the task if running and removes its worktree and branch.
func (a *App) Discard(ctx context.Context, id int64) error {
	t, err := a.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	a.Sup.Kill(id)
	_ = a.WT.Remove(ctx, t.WorktreePath, t.Branch)
	_ = a.Store.ClearLogs(ctx, id)
	return a.Store.SetStatus(ctx, id, store.StatusDiscarded)
}

// Clean discards every active task for this repo and prunes git metadata. Returns the count removed.
func (a *App) Clean(ctx context.Context) (int, error) {
	active, err := a.Store.ActiveTasks(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range active {
		run, err := a.Store.GetRun(ctx, t.RunID)
		if err != nil || run.RepoPath != a.WT.Repo {
			continue
		}
		if err := a.Discard(ctx, t.ID); err == nil {
			n++
		}
	}
	return n, a.WT.Prune(ctx)
}

// Rebase rebases a task's branch on its base branch (to resolve "behind" conflicts before merging).
func (a *App) Rebase(ctx context.Context, id int64) error {
	t, run, err := a.taskAndRun(ctx, id)
	if err != nil {
		return err
	}
	if err := a.WT.Rebase(ctx, t.WorktreePath, run.BaseRef); err != nil {
		if errors.Is(err, worktree.ErrConflict) {
			_ = a.Store.SetStatus(ctx, id, store.StatusConflict)
		}
		return err
	}
	return nil
}

func (a *App) taskAndRun(ctx context.Context, id int64) (*store.Task, *store.Run, error) {
	t, err := a.Store.GetTask(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	r, err := a.Store.GetRun(ctx, t.RunID)
	if err != nil {
		return nil, nil, err
	}
	return t, r, nil
}
