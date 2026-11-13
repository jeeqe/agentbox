// Command agentbox runs coding agents in isolated git worktrees.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeeqe/agentbox/internal/app"
	"github.com/jeeqe/agentbox/internal/server"
	"github.com/jeeqe/agentbox/internal/store"
)

// These are replaced by the release build. Keep defaults useful for local builds.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const defaultPort = 8484

type launchOptions struct {
	testCmd     string
	parallelism int
	noUI        bool
	noOpen      bool
	port        int
	base        string
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "agentbox:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentbox",
		Short:         "Run AI coding agents in parallel git worktrees",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       versionString(),
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(newRunCmd(), newRaceCmd(), newUICmd(), newLsCmd(), newCleanCmd(), newVersionCmd())
	return root
}

func newRunCmd() *cobra.Command {
	var agent string
	var opts launchOptions
	cmd := &cobra.Command{
		Use:   "run [prompts...]",
		Short: "Run one agent for each prompt",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, prompts []string) error {
			if err := validateLaunchOptions(opts); err != nil {
				return err
			}
			if strings.TrimSpace(agent) == "" {
				return errors.New("--agent must not be empty")
			}
			return launch(cmd, opts, func(a *app.App) (*store.Run, []store.Task, error) {
				return a.FanOut(cmd.Context(), agent, prompts)
			})
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "agent command or registered agent name")
	bindLaunchFlags(cmd, &opts)
	return cmd
}

func newRaceCmd() *cobra.Command {
	var agentsCSV string
	var opts launchOptions
	cmd := &cobra.Command{
		Use:   "race prompt",
		Short: "Run several agents against the same prompt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLaunchOptions(opts); err != nil {
				return err
			}
			agents := splitCSV(agentsCSV)
			if len(agents) < 2 {
				return errors.New("--agents needs at least two agents")
			}
			return launch(cmd, opts, func(a *app.App) (*store.Run, []store.Task, error) {
				return a.Race(cmd.Context(), args[0], agents)
			})
		},
	}
	cmd.Flags().StringVar(&agentsCSV, "agents", "claude,codex,opencode", "comma-separated agent names or commands")
	bindLaunchFlags(cmd, &opts)
	return cmd
}

func bindLaunchFlags(cmd *cobra.Command, opts *launchOptions) {
	cmd.Flags().StringVar(&opts.testCmd, "test", "", "test command to run after an agent succeeds")
	cmd.Flags().IntVar(&opts.parallelism, "parallel", 0, "maximum concurrent agents (0 is unlimited)")
	cmd.Flags().BoolVar(&opts.noUI, "no-ui", false, "do not start the web dashboard")
	cmd.Flags().BoolVar(&opts.noOpen, "no-open", false, "do not open the dashboard in a browser")
	cmd.Flags().IntVar(&opts.port, "port", defaultPort, "dashboard port")
	cmd.Flags().StringVar(&opts.base, "base", "", "branch to start from and merge back into (default: the current branch)")
}

func validateLaunchOptions(opts launchOptions) error {
	if opts.parallelism < 0 {
		return errors.New("--parallel must be zero or greater")
	}
	if opts.port < 0 || opts.port > 65535 {
		return errors.New("--port must be between 0 and 65535")
	}
	return nil
}

func launch(cmd *cobra.Command, opts launchOptions, create func(*app.App) (*store.Run, []store.Task, error)) error {
	a, err := openApp(opts.testCmd, opts.parallelism)
	if err != nil {
		return err
	}
	defer a.Close()
	a.Base = opts.base

	run, tasks, err := create(a)
	if err != nil {
		return err
	}
	printLaunch(cmd.OutOrStdout(), run, tasks)

	var dashboard *http.Server
	var serverErr <-chan error
	if !opts.noUI {
		dashboard, _, serverErr, err = startDashboard(a, opts.port, opts.noOpen, cmd.OutOrStdout())
		if err != nil {
			stopTasks(a, tasks)
			a.Sup.Wait()
			return err
		}
	}

	if opts.noUI {
		return waitForTasks(cmd.OutOrStdout(), a, run.ID, tasks)
	}
	return serveRunUntilInterrupted(cmd.OutOrStdout(), a, run.ID, tasks, dashboard, serverErr)
}

func newUICmd() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Start the AgentBox dashboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if port < 0 || port > 65535 {
				return errors.New("--port must be between 0 and 65535")
			}
			a, err := openApp("", 0)
			if err != nil {
				return err
			}
			defer a.Close()
			return serveUntilInterrupted(cmd.OutOrStdout(), a, port, noOpen)
		},
	}
	cmd.Flags().IntVar(&port, "port", defaultPort, "dashboard port")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open the dashboard in a browser")
	return cmd
}

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List tasks for this repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := openApp("", 0)
			if err != nil {
				return err
			}
			defer a.Close()

			tasks, err := tasksForRepo(cmd.Context(), a)
			if err != nil {
				return err
			}
			printTaskTable(cmd.OutOrStdout(), tasks)
			return nil
		},
	}
}

func newCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove active AgentBox worktrees for this repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := openApp("", 0)
			if err != nil {
				return err
			}
			defer a.Close()
			n, err := a.Clean(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %d worktree(s).\n", n)
			return nil
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), versionString())
		},
	}
}

func openApp(testCmd string, parallelism int) (*app.App, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return app.Open(dir, testCmd, parallelism)
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func printLaunch(w io.Writer, run *store.Run, tasks []store.Task) {
	fmt.Fprintf(w, "Started run %d (%s).\n", run.ID, run.Kind)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tAGENT\tPORT\tWORKTREE\tBRANCH")
	for _, task := range tasks {
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\n", task.ID, task.Agent, task.Port, task.WorktreePath, task.Branch)
	}
	_ = tw.Flush()
}

func printTaskTable(w io.Writer, tasks []store.Task) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tRUN\tAGENT\tSTATUS\t+/-\tBRANCH")
	for _, task := range tasks {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t+%d/-%d\t%s\n",
			task.ID, task.RunID, task.Agent, task.Status, task.Additions, task.Deletions, task.Branch)
	}
	_ = tw.Flush()
}

func tasksForRepo(ctx context.Context, a *app.App) ([]store.Task, error) {
	all, err := a.Store.AllTasks(ctx)
	if err != nil {
		return nil, err
	}
	runs := make(map[int64]*store.Run)
	var tasks []store.Task
	for _, task := range all {
		run, ok := runs[task.RunID]
		if !ok {
			run, err = a.Store.GetRun(ctx, task.RunID)
			if err != nil {
				return nil, err
			}
			runs[task.RunID] = run
		}
		if run.RepoPath == a.WT.Repo {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func startDashboard(a *app.App, wantedPort int, noOpen bool, out io.Writer) (*http.Server, int, <-chan error, error) {
	listener, port, err := listenLocal(wantedPort)
	if err != nil {
		return nil, 0, nil, err
	}

	srv := &http.Server{
		Handler:           server.New(a),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	url := "http://127.0.0.1:" + strconv.Itoa(port)
	fmt.Fprintf(out, "Dashboard: %s\n", url)
	if !noOpen {
		openBrowser(url)
	}
	return srv, port, errCh, nil
}

func listenLocal(wantedPort int) (net.Listener, int, error) {
	if wantedPort == 0 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, 0, err
		}
		return listener, listener.Addr().(*net.TCPAddr).Port, nil
	}

	var lastErr error
	for port := wantedPort; port <= 65535 && port < wantedPort+100; port++ {
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			return listener, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("listen on 127.0.0.1 starting at %d: %w", wantedPort, lastErr)
}

func waitForTasks(out io.Writer, a *app.App, runID int64, started []store.Task) error {
	done := make(chan struct{})
	go func() {
		a.Sup.Wait()
		close(done)
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	interrupted := false
	select {
	case <-done:
	case <-interrupt:
		fmt.Fprintln(out, "Interrupt received; stopping active agents.")
		stopTasks(a, started)
		<-done
		interrupted = true
	}

	tasks, err := printFinalStatus(out, a, runID)
	if err != nil {
		return err
	}
	if interrupted {
		return errors.New("interrupted")
	}
	if hasFailedTask(tasks) {
		return errors.New("one or more tasks failed")
	}
	return nil
}

// serveRunUntilInterrupted keeps the dashboard available after tasks complete:
// finished worktrees can then be inspected, merged, or discarded from the UI.
func serveRunUntilInterrupted(out io.Writer, a *app.App, runID int64, started []store.Task, dashboard *http.Server, serverErr <-chan error) error {
	done := make(chan struct{})
	go func() {
		a.Sup.Wait()
		close(done)
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	select {
	case <-done:
		if _, err := printFinalStatus(out, a, runID); err != nil {
			shutdownDashboard(dashboard)
			return err
		}
		fmt.Fprintln(out, "Tasks have finished; the dashboard is still running. Press Ctrl-C to stop it.")
		select {
		case <-interrupt:
			shutdownDashboard(dashboard)
			return nil
		case err := <-serverErr:
			return fmt.Errorf("dashboard server: %w", err)
		}
	case <-interrupt:
		fmt.Fprintln(out, "Interrupt received; stopping active agents.")
		stopTasks(a, started)
		<-done
		shutdownDashboard(dashboard)
		return errors.New("interrupted")
	case err := <-serverErr:
		stopTasks(a, started)
		<-done
		shutdownDashboard(dashboard)
		return fmt.Errorf("dashboard server: %w", err)
	}
}

func printFinalStatus(out io.Writer, a *app.App, runID int64) ([]store.Task, error) {
	tasks, err := a.Store.TasksForRun(context.Background(), runID)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "Final task status:")
	printTaskTable(out, tasks)
	return tasks, nil
}

func stopTasks(a *app.App, tasks []store.Task) {
	for _, task := range tasks {
		a.Kill(task.ID)
	}
}

func hasFailedTask(tasks []store.Task) bool {
	for _, task := range tasks {
		switch task.Status {
		case store.StatusFailed, store.StatusKilled, store.StatusConflict:
			return true
		}
	}
	return false
}

func serveUntilInterrupted(out io.Writer, a *app.App, port int, noOpen bool) error {
	srv, _, serverErr, err := startDashboard(a, port, noOpen, out)
	if err != nil {
		return err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	select {
	case <-interrupt:
		shutdownDashboard(srv)
		return nil
	case err := <-serverErr:
		return fmt.Errorf("dashboard server: %w", err)
	}
}

func shutdownDashboard(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func openBrowser(url string) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func versionString() string {
	return fmt.Sprintf("agentbox %s (commit %s, built %s)", version, commit, date)
}
