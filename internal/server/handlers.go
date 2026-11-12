package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/jeeqe/agentbox/internal/store"
	"github.com/jeeqe/agentbox/internal/ui"
	"github.com/jeeqe/agentbox/internal/worktree"
)

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	runs, err := s.app.Store.ListRuns(r.Context(), s.app.WT.Repo)
	if err != nil {
		serverError(w, err)
		return
	}

	views := make([]ui.RunView, 0, len(runs))
	var history []ui.HistoryView
	for _, run := range runs {
		tasks, err := s.app.Store.TasksForRun(r.Context(), run.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		// Lanes are for work that still needs a decision. Finished runs
		// become one quiet line in the history list.
		var live []store.Task
		for _, t := range tasks {
			if t.Status != store.StatusMerged && t.Status != store.StatusDiscarded {
				live = append(live, t)
			}
		}
		if len(live) > 0 {
			views = append(views, s.runView(run, live, ""))
			continue
		}
		if len(history) < 8 {
			history = append(history, historyView(run, tasks))
		}
	}
	render(w, r, ui.Dashboard(views, history))
}

// run renders just one run's task grid so it can be swapped into the
// dashboard without reloading the document shell.
func (s *server) run(w http.ResponseWriter, r *http.Request) {
	run, tasks, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	render(w, r, ui.RunGrid(s.runView(*run, tasks, "")))
}

func (s *server) diff(w http.ResponseWriter, r *http.Request) {
	task, _, ok := s.lookupTask(w, r)
	if !ok {
		return
	}
	patch, err := s.app.Store.GetDiff(r.Context(), task.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, ui.DiffPanel(s.taskView(*task, ""), patch))
}

func (s *server) merge(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, "merge")
}

func (s *server) kill(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, "kill")
}

func (s *server) restart(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, "restart")
}

func (s *server) discard(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, "discard")
}

func (s *server) rebase(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, "rebase")
}

// taskAction deliberately returns an updated card for both success and
// actionable failures. HTMX swaps successful 2xx responses by default, which
// lets a conflict show its CONFLICT state and explanation in place.
func (s *server) taskAction(w http.ResponseWriter, r *http.Request, action string) {
	task, _, ok := s.lookupTask(w, r)
	if !ok {
		return
	}

	var err error
	switch action {
	case "merge":
		err = s.app.MergeAndCleanup(r.Context(), task.ID)
	case "kill":
		s.app.Kill(task.ID)
	case "restart":
		err = s.app.Restart(r.Context(), task.ID)
	case "discard":
		err = s.app.Discard(r.Context(), task.ID)
	case "rebase":
		err = s.app.Rebase(r.Context(), task.ID)
	default:
		serverError(w, fmt.Errorf("unknown task action %q", action))
		return
	}

	// MergeAndCleanup/Rebase persist StatusConflict before returning
	// ErrConflict. Read the task back so the fresh card is authoritative.
	updated, getErr := s.app.Store.GetTask(r.Context(), task.ID)
	if getErr != nil {
		serverError(w, getErr)
		return
	}
	message := ""
	if err != nil {
		if errors.Is(err, worktree.ErrConflict) {
			message = "Conflict detected. Rebase this branch, resolve it, then try again."
		} else {
			message = err.Error()
		}
	}
	if err == nil && (action == "merge" || action == "discard") {
		s.removeCards(w, r, task, action)
	}
	render(w, r, ui.TaskCard(s.taskView(*updated, message)))
}

// stream replays durable log history before subscribing to new supervisor
// events. Subscribing first closes the otherwise unavoidable race between the
// replay query and a freshly-written log line; IDs prevent duplicate output.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	task, _, ok := s.lookupTask(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	events, unsubscribe := s.app.Sup.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	var lastID int64
	for {
		// Store caps a single Logs call at 5,000 rows. Page through it so a
		// reconnect faithfully restores a long-running agent's full history.
		logs, err := s.app.Store.Logs(r.Context(), task.ID, lastID, 5000)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, log := range logs {
			if err := s.writeLogEvent(w, r.Context(), log); err != nil {
				return
			}
			lastID = log.ID
		}
		if len(logs) < 5000 {
			break
		}
	}
	// The task may have finished while a large history was replayed. Refresh it
	// before the initial frame so a dropped (slow-client) supervisor event
	// cannot leave this connection displaying a stale RUNNING state forever.
	current, err := s.app.Store.GetTask(r.Context(), task.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	task = current
	// Send an initial status frame as well as transition frames. A browser that
	// reconnects after a status transition still receives the current pill and
	// diff counters without needing to reload the task card.
	if err := s.writeStatusEvent(w, r.Context(), *task); err != nil {
		return
	}
	flusher.Flush()
	if terminal(task.Status) {
		closeStream(w, flusher)
		return
	}

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event := <-events:
			if event.TaskID != task.ID {
				continue
			}
			switch event.Kind {
			case "log":
				if event.Log == nil || event.Log.ID <= lastID {
					continue
				}
				if err := s.writeLogEvent(w, r.Context(), *event.Log); err != nil {
					return
				}
				lastID = event.Log.ID
			case "status":
				updated, err := s.app.Store.GetTask(r.Context(), task.ID)
				if err != nil {
					return
				}
				task = updated
				if err := s.writeStatusEvent(w, r.Context(), *task); err != nil {
					return
				}
			}
			flusher.Flush()
			if terminal(task.Status) {
				closeStream(w, flusher)
				return
			}
		}
	}
}

func (s *server) lookupRun(w http.ResponseWriter, r *http.Request) (*store.Run, []store.Task, bool) {
	id, ok := requestID(w, r)
	if !ok {
		return nil, nil, false
	}
	run, err := s.app.Store.GetRun(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.RepoPath != s.app.WT.Repo) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		serverError(w, err)
		return nil, nil, false
	}
	tasks, err := s.app.Store.TasksForRun(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return nil, nil, false
	}
	return run, tasks, true
}

func (s *server) lookupTask(w http.ResponseWriter, r *http.Request) (*store.Task, *store.Run, bool) {
	id, ok := requestID(w, r)
	if !ok {
		return nil, nil, false
	}
	task, err := s.app.Store.GetTask(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		serverError(w, err)
		return nil, nil, false
	}
	run, err := s.app.Store.GetRun(r.Context(), task.RunID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.RepoPath != s.app.WT.Repo) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		serverError(w, err)
		return nil, nil, false
	}
	return task, run, true
}

func requestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (s *server) runView(run store.Run, tasks []store.Task, message string) ui.RunView {
	view := ui.RunView{
		ID:            run.ID,
		Kind:          run.Kind,
		Prompt:        run.Prompt,
		Base:          run.BaseRef,
		StartedAt:     run.CreatedAt.Local().Format("15:04"),
		FastestTaskID: fastestPassingTask(tasks),
		Tasks:         make([]ui.TaskView, 0, len(tasks)),
	}
	for _, task := range tasks {
		view.Tasks = append(view.Tasks, s.taskView(task, message))
	}
	return view
}

func (s *server) taskView(task store.Task, message string) ui.TaskView {
	return s.taskViewWithSupervisor(task, message, true)
}

// taskViewWithSupervisor lets a restart response immediately show RUNNING,
// even in the tiny interval before the goroutine writes MarkStarted. Status
// SSE frames use false so a terminal event cannot be visually regressed while
// the supervisor is unwinding its process bookkeeping.
func (s *server) taskViewWithSupervisor(task store.Task, message string, includeSupervisor bool) ui.TaskView {
	running := task.Status == store.StatusQueued || task.Status == store.StatusRunning
	if includeSupervisor && s.app.Sup.IsRunning(task.ID) {
		running = true
	}
	status := task.Status
	if includeSupervisor && running {
		status = store.StatusRunning
	}
	var startedMs, finishedMs int64
	if task.StartedAt != nil {
		startedMs = task.StartedAt.UnixMilli()
	}
	if task.FinishedAt != nil {
		finishedMs = task.FinishedAt.UnixMilli()
	}
	return ui.TaskView{
		FinishedMs: finishedMs,
		ID:         task.ID,
		RunID:      task.RunID,
		Agent:      task.Agent,
		Branch:     task.Branch,
		Port:       task.Port,
		Status:     status,
		Additions:  task.Additions,
		Deletions:  task.Deletions,
		Clock:      clock(task, time.Now()),
		StartedMs:  startedMs,
		Detail:     taskDetail(task, status),
		Running:    running,
		Message:    message,
	}
}

func fastestPassingTask(tasks []store.Task) int64 {
	var fastestID, untimedID int64
	var earliestFinish time.Time
	for _, task := range tasks {
		if task.Status != store.StatusPassed {
			continue
		}
		// The first agent to finish is the useful winner in a race. A task
		// without a persisted finish time can be used as a fallback for legacy
		// rows, but never beats a measured passing task.
		if task.FinishedAt == nil {
			if untimedID == 0 {
				untimedID = task.ID
			}
			continue
		}
		if fastestID == 0 || task.FinishedAt.Before(earliestFinish) {
			fastestID, earliestFinish = task.ID, *task.FinishedAt
		}
	}
	if fastestID == 0 {
		return untimedID
	}
	return fastestID
}

// clock formats a task's elapsed time as m:ss (h:mm:ss past an hour).
func clock(task store.Task, now time.Time) string {
	if task.StartedAt == nil {
		return "0:00"
	}
	end := now
	if task.FinishedAt != nil {
		end = *task.FinishedAt
	}
	d := end.Sub(*task.StartedAt)
	if d < 0 {
		d = 0
	}
	secs := int(d.Seconds())
	if secs >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", secs/3600, secs%3600/60, secs%60)
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// taskDetail is the short second line under a lane's status. Running tasks
// get their latest output line from the browser instead, so it stays empty.
func taskDetail(task store.Task, status string) string {
	switch status {
	case store.StatusFailed:
		switch {
		case task.ExitCode == nil:
			return "failed"
		case *task.ExitCode < 0:
			return "could not start"
		default:
			return fmt.Sprintf("exited with code %d", *task.ExitCode)
		}
	case store.StatusConflict:
		return "conflicts with the base branch"
	case store.StatusKilled:
		return "stopped before it finished"
	}
	return ""
}

func render(w http.ResponseWriter, r *http.Request, component templ.Component) {
	body, err := renderComponent(r.Context(), component)
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

func renderComponent(ctx context.Context, component templ.Component) (string, error) {
	var body bytes.Buffer
	if err := component.Render(ctx, &body); err != nil {
		return "", err
	}
	return body.String(), nil
}

func serverError(w http.ResponseWriter, err error) {
	http.Error(w, "agentbox: "+err.Error(), http.StatusInternalServerError)
}

func terminal(status string) bool {
	switch status {
	case store.StatusPassed, store.StatusFailed, store.StatusConflict, store.StatusKilled, store.StatusMerged, store.StatusDiscarded:
		return true
	default:
		return false
	}
}

// writeSSE frames a (possibly multi-line) HTML fragment according to the SSE
// wire format. Prefixing every physical line keeps generated templ whitespace
// and multi-line escaped logs from accidentally terminating an event.
func writeSSE(w io.Writer, event, data string) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	for _, line := range strings.Split(data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// writeLogEvent renders log text through templ rather than interpolating it
// into HTML. templ escapes text nodes, including logs restored from SQLite.
func (s *server) writeLogEvent(w io.Writer, ctx context.Context, log store.LogLine) error {
	stream := "stdout"
	if log.Stream == "stderr" {
		stream = "stderr"
	}
	fragment, err := renderComponent(ctx, ui.TaskLogLine(stream, log.Text))
	if err != nil {
		return err
	}
	return writeSSE(w, "log", fragment)
}

// writeStatusEvent supplies a full OOB task-meta fragment. It updates both the
// pill and counters without forcing an expensive terminal-pane replacement.
func (s *server) writeStatusEvent(w io.Writer, ctx context.Context, task store.Task) error {
	fragment, err := renderComponent(ctx, ui.StatusUpdate(s.taskViewWithSupervisor(task, "", false)))
	if err != nil {
		return err
	}
	return writeSSE(w, "status", fragment)
}

// removeCards asks the page, through a response header, to animate cards out:
// the acted-on task, and for a merged race also the discarded losers.
func (s *server) removeCards(w http.ResponseWriter, r *http.Request, task *store.Task, action string) {
	ids := []int64{task.ID}
	if action == "merge" {
		if siblings, err := s.app.Store.TasksForRun(r.Context(), task.RunID); err == nil {
			for _, sib := range siblings {
				if sib.ID != task.ID && sib.Status == store.StatusDiscarded {
					ids = append(ids, sib.ID)
				}
			}
		}
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	w.Header().Set("X-Agentbox-Remove", strings.Join(parts, ","))
}

// closeStream tells the page the task is finished. The sse-close attribute on
// the card closes the EventSource, so the browser does not reconnect and replay.
func closeStream(w io.Writer, flusher http.Flusher) {
	if err := writeSSE(w, "close", "done"); err == nil {
		flusher.Flush()
	}
}

// historyView summarises a finished run in one line.
func historyView(run store.Run, tasks []store.Task) ui.HistoryView {
	title := fmt.Sprintf("Fan-out %d", run.ID)
	if run.Kind == "race" {
		title = fmt.Sprintf("Race %d", run.ID)
		if run.Prompt != "" {
			title += ": " + run.Prompt
		}
	}
	outcome := "discarded"
	for _, t := range tasks {
		if t.Status == store.StatusMerged {
			outcome = "merged " + t.Agent
			break
		}
	}
	return ui.HistoryView{Title: title, Outcome: outcome, StartedAt: run.CreatedAt.Local().Format("Jan 2, 15:04")}
}
