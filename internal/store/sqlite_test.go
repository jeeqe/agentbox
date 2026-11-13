package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestStoreCRUDAndWAL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "agentbox.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}

	run := &Run{Kind: "fan-out", RepoPath: "/tmp/repo", BaseRef: "main"}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.ID == 0 || run.CreatedAt.IsZero() {
		t.Fatalf("CreateRun did not populate identity/timestamp: %#v", run)
	}

	gotRun, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.Kind != run.Kind || gotRun.RepoPath != run.RepoPath || gotRun.BaseRef != run.BaseRef {
		t.Fatalf("GetRun = %#v, want %#v", gotRun, run)
	}

	otherRun := &Run{Kind: "race", Prompt: "try it", RepoPath: "/elsewhere", BaseRef: "trunk"}
	if err := s.CreateRun(ctx, otherRun); err != nil {
		t.Fatalf("CreateRun(other): %v", err)
	}
	runs, err := s.ListRuns(ctx, run.RepoPath)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("ListRuns(repo) = %#v, want only run %d", runs, run.ID)
	}

	task := &Task{
		RunID: run.ID, Agent: "cmd:printf hello", Prompt: "make a change",
		Branch: "agentbox/1-1-change", WorktreePath: "/tmp/worktree", Port: 3010,
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.ID == 0 || task.Status != StatusQueued {
		t.Fatalf("CreateTask = %#v, want ID and queued status", task)
	}

	if err := s.MarkStarted(ctx, task.ID); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	if err := s.SetDiffStats(ctx, task.ID, 8, 3); err != nil {
		t.Fatalf("SetDiffStats: %v", err)
	}
	if err := s.SaveDiff(ctx, task.ID, "first patch"); err != nil {
		t.Fatalf("SaveDiff: %v", err)
	}
	if err := s.SaveDiff(ctx, task.ID, "updated patch"); err != nil {
		t.Fatalf("SaveDiff(update): %v", err)
	}
	if err := s.MarkFinished(ctx, task.ID, StatusPassed, 0); err != nil {
		t.Fatalf("MarkFinished: %v", err)
	}

	gotTask, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.Status != StatusPassed || gotTask.ExitCode == nil || *gotTask.ExitCode != 0 ||
		gotTask.StartedAt == nil || gotTask.FinishedAt == nil || gotTask.Additions != 8 || gotTask.Deletions != 3 {
		t.Fatalf("GetTask = %#v, want finished passed task with metadata", gotTask)
	}
	patch, err := s.GetDiff(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if patch != "updated patch" {
		t.Fatalf("GetDiff = %q, want updated patch", patch)
	}

	first := &LogLine{TaskID: task.ID, Stream: "stdout", Text: "one"}
	second := &LogLine{TaskID: task.ID, Stream: "stderr", Text: "two"}
	if err := s.AppendLog(ctx, first); err != nil {
		t.Fatalf("AppendLog(first): %v", err)
	}
	if err := s.AppendLog(ctx, second); err != nil {
		t.Fatalf("AppendLog(second): %v", err)
	}
	logs, err := s.Logs(ctx, task.ID, first.ID, 10)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ID != second.ID || logs[0].Text != "two" {
		t.Fatalf("Logs(after first) = %#v, want second log", logs)
	}
	if err := s.ClearLogs(ctx, task.ID); err != nil {
		t.Fatalf("ClearLogs: %v", err)
	}
	logs, err = s.Logs(ctx, task.ID, 0, 0)
	if err != nil {
		t.Fatalf("Logs(after clear): %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("Logs(after clear) = %#v, want no logs", logs)
	}

	if err := s.SetStatus(ctx, task.ID, StatusDiscarded); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	tasks, err := s.TasksForRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("TasksForRun: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != StatusDiscarded {
		t.Fatalf("TasksForRun = %#v, want discarded task", tasks)
	}
	active, err := s.ActiveTasks(ctx)
	if err != nil {
		t.Fatalf("ActiveTasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("ActiveTasks = %#v, want no active tasks", active)
	}
	all, err := s.AllTasks(ctx)
	if err != nil {
		t.Fatalf("AllTasks: %v", err)
	}
	if len(all) != 1 || all[0].ID != task.ID {
		t.Fatalf("AllTasks = %#v, want created task", all)
	}
}

func TestOpenUpgradesDatabaseWithoutOwnerPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The tasks table as released in v0.1.0, before owner_pid existed.
	_, err = old.Exec(`CREATE TABLE runs (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, prompt TEXT NOT NULL DEFAULT '', repo_path TEXT NOT NULL, base_ref TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id INTEGER NOT NULL, agent TEXT NOT NULL, prompt TEXT NOT NULL, branch TEXT NOT NULL, worktree_path TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'queued', exit_code INTEGER, additions INTEGER NOT NULL DEFAULT 0, deletions INTEGER NOT NULL DEFAULT 0, started_at INTEGER, finished_at INTEGER);
INSERT INTO runs(kind,repo_path,base_ref,created_at) VALUES('race','/r','main',1);
INSERT INTO tasks(run_id,agent,prompt,branch,worktree_path,status) VALUES(1,'claude','p','b','/w','passed');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer s.Close()
	tasks, err := s.TasksForRun(context.Background(), 1)
	if err != nil || len(tasks) != 1 || tasks[0].OwnerPID != 0 || tasks[0].Status != StatusPassed {
		t.Fatalf("tasks = %+v, err %v", tasks, err)
	}
	// Opening again must not try to add the column twice.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}

func TestTrimLogsKeepsNewestLines(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	run := &Run{Kind: "fan-out", RepoPath: "/r", BaseRef: "main"}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	a := &Task{RunID: run.ID, Agent: "x", Prompt: "p", Branch: "a", WorktreePath: "/a"}
	b := &Task{RunID: run.ID, Agent: "x", Prompt: "p", Branch: "b", WorktreePath: "/b"}
	for _, tk := range []*Task{a, b} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 10; i++ {
		for _, tk := range []*Task{a, b} {
			if err := s.AppendLog(ctx, &LogLine{TaskID: tk.ID, Stream: "stdout", Text: fmt.Sprint("line ", i)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.TrimLogs(ctx, a.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Logs(ctx, a.ID, 0, 100)
	if len(got) != 3 || got[0].Text != "line 8" || got[2].Text != "line 10" {
		t.Fatalf("task a logs after trim = %+v, want lines 8..10", got)
	}
	other, _ := s.Logs(ctx, b.ID, 0, 100)
	if len(other) != 10 {
		t.Fatalf("trimming one task removed %d lines from another", 10-len(other))
	}
	// Trimming when there is less than the limit changes nothing.
	if err := s.TrimLogs(ctx, a.ID, 50); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Logs(ctx, a.ID, 0, 100); len(got) != 3 {
		t.Fatalf("trim below the limit dropped lines: %d left", len(got))
	}
}
