package runner

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeeqe/agentbox/internal/store"
	"github.com/jeeqe/agentbox/internal/worktree"
)

func TestSupervisorCapturesExitCodeAndStreamsLogs(t *testing.T) {
	ctx := context.Background()
	st, wt, base := supervisorFixture(t)
	defer st.Close()

	task := newSupervisorTask(t, ctx, st, wt, base, `cmd:printf 'stdout-line\n'; printf 'stderr-line\n' >&2; exit 7`)
	sv := NewSupervisor(st, wt, "", 0)
	events, unsubscribe := sv.Subscribe()
	defer unsubscribe()

	sv.Start(task, base)
	sv.Wait()

	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusFailed)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", got.ExitCode)
	}

	logs, err := st.Logs(ctx, task.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var combined strings.Builder
	for _, log := range logs {
		combined.WriteString(log.Text)
		combined.WriteByte('\n')
	}
	if text := combined.String(); !strings.Contains(text, "stdout-line") || !strings.Contains(text, "stderr-line") {
		t.Fatalf("logs did not contain streamed output: %q", text)
	}

	var sawLog, sawTerminal bool
	for {
		select {
		case event := <-events:
			if event.Kind == "log" && event.Log != nil && event.Log.TaskID == task.ID {
				sawLog = true
			}
			if event.Kind == "status" && event.TaskID == task.ID && event.Status == store.StatusFailed {
				sawTerminal = true
			}
		default:
			if !sawLog || !sawTerminal {
				t.Fatalf("events: sawLog=%v sawTerminal=%v", sawLog, sawTerminal)
			}
			return
		}
	}
}

func TestSupervisorFailsWhenTestCommandFails(t *testing.T) {
	ctx := context.Background()
	st, wt, base := supervisorFixture(t)
	defer st.Close()

	task := newSupervisorTask(t, ctx, st, wt, base, `cmd:printf change > changed.txt`)
	sv := NewSupervisor(st, wt, `printf 'test failed\n' >&2; exit 12`, 0)
	sv.Start(task, base)
	sv.Wait()

	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusFailed)
	}
	if got.ExitCode == nil || *got.ExitCode != 12 {
		t.Fatalf("exit code = %v, want 12", got.ExitCode)
	}
	logs, err := st.Logs(ctx, task.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, log := range logs {
		found = found || log.Text == "test failed"
	}
	if !found {
		t.Fatalf("test command output was not logged: %#v", logs)
	}
}

func TestSupervisorKillMarksTaskKilled(t *testing.T) {
	ctx := context.Background()
	st, wt, base := supervisorFixture(t)
	defer st.Close()

	task := newSupervisorTask(t, ctx, st, wt, base, `cmd:sleep 30`)
	sv := NewSupervisor(st, wt, "", 0)
	sv.Start(task, base)

	deadline := time.Now().Add(3 * time.Second)
	for !sv.IsRunning(task.ID) {
		if time.Now().After(deadline) {
			t.Fatal("task did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sv.Kill(task.ID) {
		t.Fatal("Kill reported no running task")
	}
	sv.Wait()
	if sv.IsRunning(task.ID) {
		t.Fatal("task remains running after Wait")
	}

	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusKilled {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusKilled)
	}
	if got.ExitCode == nil || *got.ExitCode != -1 {
		t.Fatalf("exit code = %v, want -1", got.ExitCode)
	}
}

func supervisorFixture(t *testing.T) (*store.Store, *worktree.Manager, string) {
	t.Helper()
	requireSupervisorLoopback(t)
	repo := newSupervisorRepo(t)
	wt, err := worktree.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "agentbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := wt.CurrentBranch(context.Background())
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st, wt, base
}

// Some restricted build sandboxes prohibit every bind(2) call. The worktree
// manager deliberately proves ports are available by binding them, so these
// integration tests cannot exercise their subject in such an environment.
func requireSupervisorLoopback(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listeners are unavailable: %v", err)
	}
	_ = l.Close()
}

func newSupervisorTask(t *testing.T, ctx context.Context, st *store.Store, wt *worktree.Manager, base, agent string) store.Task {
	t.Helper()
	run := &store.Run{Kind: "fan-out", RepoPath: wt.Repo, BaseRef: base}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	info, err := wt.Create(ctx, run.ID, 1, "supervisor test", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wt.Remove(context.Background(), info.Path, info.Branch) })
	task := store.Task{RunID: run.ID, Agent: agent, Prompt: "test prompt", Branch: info.Branch, WorktreePath: info.Path, Port: info.Port}
	if err := st.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func newSupervisorRepo(t *testing.T) string {
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
