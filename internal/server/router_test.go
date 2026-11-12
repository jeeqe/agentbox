package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jeeqe/agentbox/internal/app"
	server "github.com/jeeqe/agentbox/internal/server"
	"github.com/jeeqe/agentbox/internal/store"
)

func TestHealthz(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	server.New(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestDashboardRendersStoredRun(t *testing.T) {
	a := newTestApp(t)
	_, task := seedTask(t, a)

	rec := httptest.NewRecorder()
	server.New(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"codex", task.Branch, "Make the dashboard testable"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not include %q:\n%s", want, body)
		}
	}
	if !strings.Contains(strings.ToLower(body), "passed") {
		t.Errorf("dashboard does not include the passed task status:\n%s", body)
	}
}

func TestDiffEndpointRendersStoredPatch(t *testing.T) {
	a := newTestApp(t)
	_, task := seedTask(t, a)
	patch := "diff --git a/readme.txt b/readme.txt\nindex 0000000..1111111 100644\n--- a/readme.txt\n+++ b/readme.txt\n@@ -0,0 +1 @@\n+added <unsafe>\n"
	if err := a.Store.SaveDiff(context.Background(), task.ID, patch); err != nil {
		t.Fatalf("save diff: %v", err)
	}

	rec := httptest.NewRecorder()
	server.New(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tasks/%d/diff", task.ID), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/%d/diff status = %d, want %d; body: %s", task.ID, rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"diff --git", "+added &lt;unsafe&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("diff response does not include %q:\n%s", want, body)
		}
	}
}

func TestTaskStreamReplaysEscapedLogs(t *testing.T) {
	a := newTestApp(t)
	_, task := seedTask(t, a)
	if err := a.Store.AppendLog(context.Background(), &store.LogLine{
		TaskID: task.ID,
		Stream: "stdout",
		Text:   "hello <dashboard>",
	}); err != nil {
		t.Fatalf("append log: %v", err)
	}

	// The seeded task is terminal, so the handler emits its durable replay and
	// current status frame synchronously. ResponseRecorder implements
	// http.Flusher, making this a real SSE-handler test without a loopback bind.
	rec := httptest.NewRecorder()
	server.New(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tasks/%d/stream", task.ID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET task stream status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("stream Content-Type = %q, want text/event-stream", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: log",
		"hello &lt;dashboard&gt;",
		"event: status",
		fmt.Sprintf(`id="task-status-%d"`, task.ID),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE replay did not contain %q:\n%s", want, body)
		}
	}
}

func newTestApp(t *testing.T) *app.App {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--initial-branch=main")
	if err := os.WriteFile(repo+"/README.md", []byte("# test repository\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "-c", "user.name=AgentBox Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")

	t.Setenv("AGENTBOX_HOME", t.TempDir())
	a, err := app.Open(repo, "", 0)
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close app: %v", err)
		}
	})
	return a
}

func seedTask(t *testing.T, a *app.App) (*store.Run, *store.Task) {
	t.Helper()
	ctx := context.Background()
	base, err := a.WT.CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("current branch: %v", err)
	}
	r := &store.Run{
		Kind:     "race",
		Prompt:   "Make the dashboard testable",
		RepoPath: a.WT.Repo,
		BaseRef:  base,
	}
	if err := a.Store.CreateRun(ctx, r); err != nil {
		t.Fatalf("create run: %v", err)
	}
	task := &store.Task{
		RunID:        r.ID,
		Agent:        "codex",
		Prompt:       r.Prompt,
		Branch:       "agentbox/test-dashboard",
		WorktreePath: a.WT.Repo,
		Port:         34001,
		Status:       store.StatusPassed,
		Additions:    7,
		Deletions:    2,
	}
	if err := a.Store.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := a.Store.SetDiffStats(ctx, task.ID, 7, 2); err != nil {
		t.Fatalf("set diff stats: %v", err)
	}
	return r, task
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestMergeRaceWinnerRemovesAllCards(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	_, tasks, err := a.Race(ctx, "add a file", []string{"cmd:echo one > one.txt", "cmd:echo two > two.txt"})
	if err != nil {
		t.Fatalf("race: %v", err)
	}
	a.Sup.Wait()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/tasks/%d/merge", tasks[0].ID), nil)
	server.New(a).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge status = %d; body: %s", rec.Code, rec.Body.String())
	}
	got := rec.Header().Get("X-Agentbox-Remove")
	want := fmt.Sprintf("%d,%d", tasks[0].ID, tasks[1].ID)
	if got != want {
		t.Fatalf("X-Agentbox-Remove = %q, want %q", got, want)
	}
	loser, err := a.Store.GetTask(ctx, tasks[1].ID)
	if err != nil || loser.Status != store.StatusDiscarded {
		t.Fatalf("loser = %+v, err %v; want discarded", loser, err)
	}
}
