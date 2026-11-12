// Package runner supervises agent subprocesses: it starts them in their worktree,
// captures output, records exit status and fans log lines out to live subscribers.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentbox-dev/agentbox/internal/store"
	"github.com/agentbox-dev/agentbox/internal/worktree"
)

// AgentSpec describes how to invoke an agent CLI non-interactively.
type AgentSpec struct {
	Bin  string
	Args func(prompt string) []string
}

// Agents is the built-in registry. Unknown names are run as "<name> <prompt>"
// so any CLI can be used, or use the "cmd:" prefix for a custom shell command
// where $PROMPT is available in the environment.
var Agents = map[string]AgentSpec{
	"claude":   {"claude", func(p string) []string { return []string{"-p", p, "--dangerously-skip-permissions"} }},
	"codex":    {"codex", func(p string) []string { return []string{"exec", "--full-auto", p} }},
	"aider":    {"aider", func(p string) []string { return []string{"--yes-always", "--message", p} }},
	"opencode": {"opencode", func(p string) []string { return []string{"run", p} }},
	"gemini":   {"gemini", func(p string) []string { return []string{"-p", p, "--yolo"} }},
}

// Command builds the exec.Cmd for an agent name + prompt.
func Command(ctx context.Context, agent, prompt string) *exec.Cmd {
	if cmdline, ok := strings.CutPrefix(agent, "cmd:"); ok {
		if isWindows() {
			return exec.CommandContext(ctx, "cmd", "/C", cmdline)
		}
		return exec.CommandContext(ctx, "sh", "-c", cmdline)
	}
	if spec, ok := Agents[agent]; ok {
		return exec.CommandContext(ctx, spec.Bin, spec.Args(prompt)...)
	}
	return exec.CommandContext(ctx, agent, prompt)
}

func isWindows() bool { return os.PathSeparator == '\\' }

// Event is broadcast to subscribers for live UIs.
type Event struct {
	TaskID int64
	Kind   string // "log" | "status"
	Log    *store.LogLine
	Status string
}

// Supervisor owns running processes.
type Supervisor struct {
	Store *store.Store
	WT    *worktree.Manager
	// TestCmd, if set, is run in the worktree after the agent exits 0; non-zero => failed.
	TestCmd string
	// Parallelism bounds concurrently running agents (0 = unlimited).
	Parallelism int

	mu      sync.Mutex
	running map[int64]context.CancelFunc
	subs    map[chan Event]struct{}
	sem     chan struct{}
	wg      sync.WaitGroup
}

func NewSupervisor(s *store.Store, wt *worktree.Manager, testCmd string, parallelism int) *Supervisor {
	sv := &Supervisor{Store: s, WT: wt, TestCmd: testCmd, Parallelism: parallelism,
		running: map[int64]context.CancelFunc{}, subs: map[chan Event]struct{}{}}
	if parallelism > 0 {
		sv.sem = make(chan struct{}, parallelism)
	}
	return sv
}

// Subscribe returns a channel of events and an unsubscribe func. Slow consumers drop events.
func (sv *Supervisor) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	sv.mu.Lock()
	sv.subs[ch] = struct{}{}
	sv.mu.Unlock()
	return ch, func() {
		sv.mu.Lock()
		delete(sv.subs, ch)
		sv.mu.Unlock()
	}
}

func (sv *Supervisor) publish(e Event) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for ch := range sv.subs {
		select {
		case ch <- e:
		default: // drop for slow subscriber; it can refetch from the store
		}
	}
}

// IsRunning reports whether a task has a live process.
func (sv *Supervisor) IsRunning(id int64) bool {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	_, ok := sv.running[id]
	return ok
}

// Start launches the task asynchronously. It returns immediately.
func (sv *Supervisor) Start(task store.Task, baseRef string) {
	ctx, cancel := context.WithCancel(context.Background())
	sv.mu.Lock()
	sv.running[task.ID] = cancel
	sv.mu.Unlock()
	sv.wg.Add(1)
	go func() {
		defer sv.wg.Done()
		defer func() {
			cancel()
			sv.mu.Lock()
			delete(sv.running, task.ID)
			sv.mu.Unlock()
		}()
		if sv.sem != nil {
			select {
			case sv.sem <- struct{}{}:
				defer func() { <-sv.sem }()
			case <-ctx.Done():
				sv.finish(task, baseRef, store.StatusKilled, -1)
				return
			}
		}
		sv.run(ctx, task, baseRef)
	}()
}

// Kill terminates a running task. It reports whether a process was running.
func (sv *Supervisor) Kill(id int64) bool {
	sv.mu.Lock()
	cancel, ok := sv.running[id]
	sv.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Wait blocks until all started tasks have finished.
func (sv *Supervisor) Wait() { sv.wg.Wait() }

func (sv *Supervisor) emitLog(taskID int64, stream, text string) {
	l := &store.LogLine{TaskID: taskID, Stream: stream, Text: text}
	if err := sv.Store.AppendLog(context.Background(), l); err != nil {
		return
	}
	sv.publish(Event{TaskID: taskID, Kind: "log", Log: l})
}

func (sv *Supervisor) setStatus(taskID int64, status string) {
	_ = sv.Store.SetStatus(context.Background(), taskID, status)
	sv.publish(Event{TaskID: taskID, Kind: "status", Status: status})
}

// runCmd runs a command to completion in dir, streaming output into the task log.
func (sv *Supervisor) runCmd(ctx context.Context, cmd *exec.Cmd, dir string, env []string, taskID int64) (int, error) {
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	var wg sync.WaitGroup
	pump := func(r io.Reader, stream string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			sv.emitLog(taskID, stream, sc.Text())
		}
	}
	wg.Add(2)
	go pump(stdout, "stdout")
	go pump(stderr, "stderr")
	wg.Wait()
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, werr
	}
	return 0, nil
}

func (sv *Supervisor) run(ctx context.Context, t store.Task, baseRef string) {
	bg := context.Background()
	_ = sv.Store.ClearLogs(bg, t.ID)
	_ = sv.Store.MarkStarted(bg, t.ID)
	sv.publish(Event{TaskID: t.ID, Kind: "status", Status: store.StatusRunning})
	sv.emitLog(t.ID, "stdout", fmt.Sprintf("$ %s  (port %d, branch %s)", t.Agent, t.Port, t.Branch))

	env := []string{"PORT=" + strconv.Itoa(t.Port), "AGENTBOX_TASK_ID=" + strconv.FormatInt(t.ID, 10), "PROMPT=" + t.Prompt}
	cmd := Command(ctx, t.Agent, t.Prompt)
	code, err := sv.runCmd(ctx, cmd, t.WorktreePath, env, t.ID)
	if ctx.Err() != nil {
		sv.finish(t, baseRef, store.StatusKilled, -1)
		return
	}
	if err != nil {
		sv.emitLog(t.ID, "stderr", "failed to start agent: "+err.Error())
		sv.finish(t, baseRef, store.StatusFailed, -1)
		return
	}
	status := store.StatusPassed
	if code != 0 {
		status = store.StatusFailed
	} else if sv.TestCmd != "" {
		sv.emitLog(t.ID, "stdout", "$ "+sv.TestCmd)
		tc := exec.CommandContext(ctx, shell(), shellFlag(), sv.TestCmd)
		tcode, terr := sv.runCmd(ctx, tc, t.WorktreePath, env, t.ID)
		if ctx.Err() != nil {
			sv.finish(t, baseRef, store.StatusKilled, -1)
			return
		}
		if terr != nil || tcode != 0 {
			status, code = store.StatusFailed, tcode
		}
	}
	sv.finish(t, baseRef, status, code)
}

func shell() string {
	if isWindows() {
		return "cmd"
	}
	return "sh"
}
func shellFlag() string {
	if isWindows() {
		return "/C"
	}
	return "-c"
}

// finish snapshots the diff and records the terminal state.
func (sv *Supervisor) finish(t store.Task, baseRef, status string, code int) {
	bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if patch, st, err := sv.WT.Snapshot(bg, t.WorktreePath, baseRef); err == nil {
		_ = sv.Store.SaveDiff(bg, t.ID, patch)
		_ = sv.Store.SetDiffStats(bg, t.ID, st.Additions, st.Deletions)
	} else {
		sv.emitLog(t.ID, "stderr", "diff snapshot failed: "+err.Error())
	}
	_ = sv.Store.MarkFinished(bg, t.ID, status, code)
	sv.publish(Event{TaskID: t.ID, Kind: "status", Status: status})
}
