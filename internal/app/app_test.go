package app

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeeqe/agentbox/internal/store"
)

func TestFanOutAndClean(t *testing.T) {
	ctx := context.Background()
	a, repo := openTestApp(t, "")
	defer a.Close()

	run, tasks, err := a.FanOut(ctx, `cmd:printf 'agent output\n'; printf done > result.txt`, []string{"first prompt", "second prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != "fan-out" || len(tasks) != 2 {
		t.Fatalf("run/tasks = %#v / %#v", run, tasks)
	}
	a.Sup.Wait()
	for _, task := range tasks {
		got, err := a.Store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.StatusPassed {
			t.Fatalf("task %d status = %q, want %q", task.ID, got.Status, store.StatusPassed)
		}
		if _, err := os.Stat(filepath.Join(task.WorktreePath, "result.txt")); err != nil {
			t.Fatalf("task %d result file: %v", task.ID, err)
		}
	}

	removed, err := a.Clean(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != len(tasks) {
		t.Fatalf("Clean removed %d tasks, want %d", removed, len(tasks))
	}
	for _, task := range tasks {
		got, err := a.Store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.StatusDiscarded {
			t.Fatalf("task %d status = %q, want %q", task.ID, got.Status, store.StatusDiscarded)
		}
		if _, err := os.Stat(task.WorktreePath); !os.IsNotExist(err) {
			t.Fatalf("worktree %q still exists (err=%v)", task.WorktreePath, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("Clean unexpectedly merged a task (err=%v)", err)
	}
}

func TestRaceMergesWinnerAndDiscardsLosers(t *testing.T) {
	ctx := context.Background()
	a, repo := openTestApp(t, "")
	defer a.Close()

	run, tasks, err := a.Race(ctx, "write a result", []string{
		`cmd:printf winner > race-result.txt`,
		`cmd:printf loser > race-result.txt`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != "race" || len(tasks) != 2 {
		t.Fatalf("run/tasks = %#v / %#v", run, tasks)
	}
	a.Sup.Wait()
	if err := a.MergeAndCleanup(ctx, tasks[0].ID); err != nil {
		t.Fatal(err)
	}

	winner, err := a.Store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	loser, err := a.Store.GetTask(ctx, tasks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if winner.Status != store.StatusMerged {
		t.Fatalf("winner status = %q, want %q", winner.Status, store.StatusMerged)
	}
	if loser.Status != store.StatusDiscarded {
		t.Fatalf("loser status = %q, want %q", loser.Status, store.StatusDiscarded)
	}
	contents, err := os.ReadFile(filepath.Join(repo, "race-result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "winner" {
		t.Fatalf("merged result = %q, want winner", contents)
	}
	if _, err := os.Stat(loser.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("loser worktree %q still exists (err=%v)", loser.WorktreePath, err)
	}
}

func openTestApp(t *testing.T, testCmd string) (*App, string) {
	t.Helper()
	requireAppLoopback(t)
	repo := newAppRepo(t)
	t.Setenv("AGENTBOX_HOME", t.TempDir())
	a, err := Open(repo, testCmd, 0)
	if err != nil {
		t.Fatal(err)
	}
	return a, repo
}

// The worktree manager allocates ports with a loopback bind. Skip only in
// sandboxes that prohibit all listeners; ordinary CI exercises the full flow.
func requireAppLoopback(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listeners are unavailable: %v", err)
	}
	_ = l.Close()
}

func newAppRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("-c", "user.name=AgentBox Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial")
	return repo
}

func TestOrphanedTasksAreStoppedOnOpen(t *testing.T) {
	repo := newAppRepo(t)
	t.Setenv("AGENTBOX_HOME", t.TempDir())
	ctx := context.Background()

	a, err := Open(repo, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	run := &store.Run{Kind: "fan-out", RepoPath: a.WT.Repo, BaseRef: "main"}
	if err := a.Store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	// A task whose owner is a pid that cannot exist, as left by a crashed process.
	orphan := &store.Task{RunID: run.ID, Agent: "x", Prompt: "p", Branch: "b1", WorktreePath: "/nowhere", OwnerPID: 2147483646, Status: store.StatusRunning}
	// A task owned by this live process must be left alone.
	mine := &store.Task{RunID: run.ID, Agent: "x", Prompt: "p", Branch: "b2", WorktreePath: "/nowhere", Status: store.StatusRunning}
	for _, tk := range []*store.Task{orphan, mine} {
		if err := a.Store.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := Open(repo, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, _ := b.Store.GetTask(ctx, orphan.ID)
	if got.Status != store.StatusKilled {
		t.Errorf("orphaned task status = %q, want %q", got.Status, store.StatusKilled)
	}
	got, _ = b.Store.GetTask(ctx, mine.ID)
	if got.Status != store.StatusRunning {
		t.Errorf("live task status = %q, want it left running", got.Status)
	}
}

func TestLaunchRefusesDetachedHead(t *testing.T) {
	repo := newAppRepo(t)
	t.Setenv("AGENTBOX_HOME", t.TempDir())
	cmd := exec.Command("git", "checkout", "-q", "--detach")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	a, err := Open(repo, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, _, err := a.FanOut(context.Background(), "cmd:true", []string{"x"}); err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("err = %v, want a detached HEAD error", err)
	}
}
