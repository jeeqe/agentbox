// Package store persists runs, agent tasks and log history in a pure-Go SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

// Status values for a Task.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusPassed    = "passed"   // agent exited 0 (and tests, if configured, passed)
	StatusFailed    = "failed"   // non-zero exit
	StatusConflict  = "conflict" // merge/rebase conflict
	StatusKilled    = "killed"
	StatusMerged    = "merged"
	StatusDiscarded = "discarded"
)

// Run groups one or more tasks started by a single CLI invocation.
type Run struct {
	ID        int64
	Kind      string // "fan-out" | "race"
	Prompt    string // for race; empty for fan-out
	RepoPath  string
	BaseRef   string // branch the worktrees were cut from
	CreatedAt time.Time
}

// Task is one agent working in one worktree.
type Task struct {
	ID           int64
	RunID        int64
	Agent        string // claude, codex, aider, opencode, ...
	Prompt       string
	Branch       string
	WorktreePath string
	Port         int
	OwnerPID     int // process that started the task; used to spot tasks orphaned by a crash
	Status       string
	ExitCode     *int
	Additions    int
	Deletions    int
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// LogLine is a single chunk of captured output.
type LogLine struct {
	ID     int64
	TaskID int64
	Stream string // "stdout" | "stderr"
	Text   string
	At     time.Time
}

// Store wraps *sql.DB.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL, prompt TEXT NOT NULL DEFAULT '',
  repo_path TEXT NOT NULL, base_ref TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  agent TEXT NOT NULL, prompt TEXT NOT NULL,
  branch TEXT NOT NULL, worktree_path TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'queued', exit_code INTEGER,
  additions INTEGER NOT NULL DEFAULT 0, deletions INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER, finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS tasks_run ON tasks(run_id);
CREATE TABLE IF NOT EXISTS logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  stream TEXT NOT NULL, text TEXT NOT NULL, at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS logs_task ON logs(task_id, id);
CREATE TABLE IF NOT EXISTS diffs (
  task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  patch TEXT NOT NULL, updated_at INTEGER NOT NULL
);`

// Open opens (creating if needed) the database at path in WAL mode.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialise writes; sqlite + WAL is plenty fast for this workload
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addColumnIfMissing(db, "tasks", "owner_pid", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// addColumnIfMissing upgrades databases created by an earlier version.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl)
	return err
}

func ts(t time.Time) int64 { return t.UnixMilli() }

func tsPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}

// CreateRun inserts a run and fills in ID/CreatedAt.
func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	r.CreatedAt = time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(kind,prompt,repo_path,base_ref,created_at) VALUES(?,?,?,?,?)`,
		r.Kind, r.Prompt, r.RepoPath, r.BaseRef, ts(r.CreatedAt))
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

func (s *Store) GetRun(ctx context.Context, id int64) (*Run, error) {
	var r Run
	var c int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id,kind,prompt,repo_path,base_ref,created_at FROM runs WHERE id=?`, id).
		Scan(&r.ID, &r.Kind, &r.Prompt, &r.RepoPath, &r.BaseRef, &c)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = time.UnixMilli(c)
	return &r, nil
}

// ListRuns returns runs for a repo (all repos if repo == ""), newest first.
func (s *Store) ListRuns(ctx context.Context, repo string) ([]Run, error) {
	q, args := `SELECT id,kind,prompt,repo_path,base_ref,created_at FROM runs`, []any{}
	if repo != "" {
		q += ` WHERE repo_path=?`
		args = append(args, repo)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var c int64
		if err := rows.Scan(&r.ID, &r.Kind, &r.Prompt, &r.RepoPath, &r.BaseRef, &c); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(c)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	if t.Status == "" {
		t.Status = StatusQueued
	}
	if t.OwnerPID == 0 {
		t.OwnerPID = os.Getpid()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks(run_id,agent,prompt,branch,worktree_path,port,owner_pid,status) VALUES(?,?,?,?,?,?,?,?)`,
		t.RunID, t.Agent, t.Prompt, t.Branch, t.WorktreePath, t.Port, t.OwnerPID, t.Status)
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

const taskCols = `id,run_id,agent,prompt,branch,worktree_path,port,owner_pid,status,exit_code,additions,deletions,started_at,finished_at`

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (*Task, error) {
	var t Task
	var exit sql.NullInt64
	var st, fin sql.NullInt64
	if err := r.Scan(&t.ID, &t.RunID, &t.Agent, &t.Prompt, &t.Branch, &t.WorktreePath, &t.Port, &t.OwnerPID,
		&t.Status, &exit, &t.Additions, &t.Deletions, &st, &fin); err != nil {
		return nil, err
	}
	if exit.Valid {
		v := int(exit.Int64)
		t.ExitCode = &v
	}
	t.StartedAt, t.FinishedAt = tsPtr(st), tsPtr(fin)
	return &t, nil
}

func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
}

func (s *Store) TasksForRun(ctx context.Context, runID int64) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE run_id=? ORDER BY id`, runID)
}

// ActiveTasks returns tasks that still own a worktree (not merged/discarded).
func (s *Store) ActiveTasks(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE status NOT IN ('merged','discarded') ORDER BY id`)
}

// AllTasks returns every task, newest run first.
func (s *Store) AllTasks(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY run_id DESC, id`)
}

func (s *Store) queryTasks(ctx context.Context, q string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// MarkStarted sets status=running and started_at.
func (s *Store) MarkStarted(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='running', owner_pid=?, started_at=?, finished_at=NULL, exit_code=NULL WHERE id=?`, os.Getpid(), ts(time.Now()), id)
	return err
}

// MarkFinished records the terminal status and exit code.
func (s *Store) MarkFinished(ctx context.Context, id int64, status string, exit int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, exit_code=?, finished_at=? WHERE id=?`, status, exit, ts(time.Now()), id)
	return err
}

func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) SetDiffStats(ctx context.Context, id int64, add, del int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET additions=?, deletions=? WHERE id=?`, add, del, id)
	return err
}

func (s *Store) SaveDiff(ctx context.Context, id int64, patch string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO diffs(task_id,patch,updated_at) VALUES(?,?,?)
		 ON CONFLICT(task_id) DO UPDATE SET patch=excluded.patch, updated_at=excluded.updated_at`,
		id, patch, ts(time.Now()))
	return err
}

// GetDiff returns the stored patch ("" if none).
func (s *Store) GetDiff(ctx context.Context, id int64) (string, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT patch FROM diffs WHERE task_id=?`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

func (s *Store) AppendLog(ctx context.Context, l *LogLine) error {
	l.At = time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO logs(task_id,stream,text,at) VALUES(?,?,?,?)`, l.TaskID, l.Stream, l.Text, ts(l.At))
	if err != nil {
		return err
	}
	l.ID, err = res.LastInsertId()
	return err
}

// Logs returns log lines with id > afterID, oldest first (limit<=0 means 5000).
func (s *Store) Logs(ctx context.Context, taskID, afterID int64, limit int) ([]LogLine, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,stream,text,at FROM logs WHERE task_id=? AND id>? ORDER BY id LIMIT ?`, taskID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogLine
	for rows.Next() {
		var l LogLine
		var at int64
		if err := rows.Scan(&l.ID, &l.TaskID, &l.Stream, &l.Text, &at); err != nil {
			return nil, err
		}
		l.At = time.UnixMilli(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClearLogs removes log history for a task (used on restart).
func (s *Store) ClearLogs(ctx context.Context, taskID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE task_id=?`, taskID)
	return err
}
