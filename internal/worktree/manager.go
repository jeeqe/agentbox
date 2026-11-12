// Package worktree wraps the git CLI to create, inspect, merge and remove isolated worktrees.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrConflict is returned by Merge when the branch cannot be merged cleanly.
var ErrConflict = errors.New("merge conflict")

// Manager operates on a single repository.
type Manager struct {
	Repo     string // absolute path to the main working tree
	BasePort int    // first port to try (default 3001)
	Root     string // where worktrees live (default <repo>/../.agentbox-worktrees/<repo-name>)
}

// Info describes a freshly created worktree.
type Info struct {
	Path   string
	Branch string
	Port   int
}

// New validates that repo is a git repository and returns a Manager.
func New(repo string) (*Manager, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	m := &Manager{Repo: abs, BasePort: 3001}
	top, err := m.git(context.Background(), abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", abs, err)
	}
	m.Repo = strings.TrimSpace(top)
	m.Root = filepath.Join(filepath.Dir(m.Repo), ".agentbox-worktrees", filepath.Base(m.Repo))
	return m, nil
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// CurrentBranch returns the branch checked out in the main working tree.
func (m *Manager) CurrentBranch(ctx context.Context) (string, error) {
	out, err := m.git(ctx, m.Repo, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns free text into a short branch-safe slug.
func Slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// Create adds a worktree on a new branch "agentbox/<runID>-<n>-<slug>" cut from base.
// It links env files and allocates a free port, skipping anything in usedPorts.
func (m *Manager) Create(ctx context.Context, runID int64, n int, label, base string, usedPorts map[int]bool) (*Info, error) {
	branch := fmt.Sprintf("agentbox/%d-%d-%s", runID, n, Slug(label))
	path := filepath.Join(m.Root, fmt.Sprintf("%d-%d-%s", runID, n, Slug(label)))
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return nil, err
	}
	if _, err := m.git(ctx, m.Repo, "worktree", "add", "-b", branch, path, base); err != nil {
		return nil, err
	}
	if err := m.linkEnvFiles(path); err != nil {
		_ = m.Remove(ctx, path, branch)
		return nil, err
	}
	port, err := m.allocPort(usedPorts)
	if err != nil {
		_ = m.Remove(ctx, path, branch)
		return nil, err
	}
	return &Info{Path: path, Branch: branch, Port: port}, nil
}

// linkEnvFiles symlinks untracked .env* files from the main tree into the worktree
// (falls back to copying where symlinks are unavailable, e.g. Windows).
func (m *Manager) linkEnvFiles(dst string) error {
	matches, _ := filepath.Glob(filepath.Join(m.Repo, ".env*"))
	for _, src := range matches {
		fi, err := os.Stat(src)
		if err != nil || fi.IsDir() {
			continue
		}
		target := filepath.Join(dst, filepath.Base(src))
		if _, err := os.Lstat(target); err == nil {
			continue // tracked in git (e.g. .env.example) — leave it alone
		}
		// Keep the linked file out of snapshots so secrets never land on the branch.
		if err := m.excludeLocally(filepath.Base(src)); err != nil {
			return err
		}
		if err := os.Symlink(src, target); err != nil {
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				return rerr
			}
			if werr := os.WriteFile(target, data, fi.Mode().Perm()); werr != nil {
				return werr
			}
		}
	}
	return nil
}

// excludeLocally adds name to the repo's .git/info/exclude (never committed) if missing.
// Tracked files are unaffected by excludes, so this only hides untracked env files.
func (m *Manager) excludeLocally(name string) error {
	out, err := m.git(context.Background(), m.Repo, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.Repo, p)
	}
	data, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == name {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + name + "\n")
	return err
}

func (m *Manager) allocPort(used map[int]bool) (int, error) {
	for p := m.BasePort; p < m.BasePort+500; p++ {
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l.Close()
		return p, nil
	}
	return 0, errors.New("no free port found")
}

// Stats holds line counts for a branch relative to its merge-base with base.
type Stats struct{ Additions, Deletions int }

// commitAll stages and commits everything in the worktree so the agent's work is on the branch.
// It is a no-op when the tree is clean.
func (m *Manager) commitAll(ctx context.Context, path, msg string) error {
	st, err := m.git(ctx, path, "status", "--porcelain")
	if err != nil || strings.TrimSpace(st) == "" {
		return err
	}
	if _, err := m.git(ctx, path, "add", "-A"); err != nil {
		return err
	}
	_, err = m.git(ctx, path, "-c", "user.name=agentbox", "-c", "user.email=agentbox@localhost",
		"commit", "-q", "--no-verify", "-m", msg)
	return err
}

// Snapshot commits pending changes, then returns the unified diff and line stats vs base.
func (m *Manager) Snapshot(ctx context.Context, path, base string) (string, Stats, error) {
	var s Stats
	if err := m.commitAll(ctx, path, "agentbox: snapshot"); err != nil {
		return "", s, err
	}
	patch, err := m.git(ctx, path, "diff", "--no-color", base+"...HEAD")
	if err != nil {
		return "", s, err
	}
	num, err := m.git(ctx, path, "diff", "--numstat", base+"...HEAD")
	if err != nil {
		return patch, s, err
	}
	for _, line := range strings.Split(strings.TrimSpace(num), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		a, _ := strconv.Atoi(f[0]) // "-" for binary files -> 0
		d, _ := strconv.Atoi(f[1])
		s.Additions += a
		s.Deletions += d
	}
	return patch, s, nil
}

// Merge merges branch into base in the main working tree (which must have base checked out and be clean).
// On conflict the merge is aborted and ErrConflict is returned.
func (m *Manager) Merge(ctx context.Context, branch, base string) error {
	cur, err := m.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	if cur != base {
		return fmt.Errorf("main working tree is on %q, expected %q", cur, base)
	}
	st, err := m.git(ctx, m.Repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(st) != "" {
		return errors.New("main working tree has uncommitted changes; commit or stash them first")
	}
	_, err = m.git(ctx, m.Repo, "-c", "user.name=agentbox", "-c", "user.email=agentbox@localhost",
		"merge", "--no-ff", "-m", "Merge "+branch+" (agentbox)", branch)
	if err != nil {
		_, _ = m.git(ctx, m.Repo, "merge", "--abort")
		if strings.Contains(err.Error(), "CONFLICT") || strings.Contains(err.Error(), "conflict") {
			return ErrConflict
		}
		return err
	}
	return nil
}

// Rebase rebases the worktree's branch onto base; aborts and returns ErrConflict on conflict.
func (m *Manager) Rebase(ctx context.Context, path, base string) error {
	if err := m.commitAll(ctx, path, "agentbox: snapshot"); err != nil {
		return err
	}
	if _, err := m.git(ctx, path, "-c", "user.name=agentbox", "-c", "user.email=agentbox@localhost", "rebase", base); err != nil {
		_, _ = m.git(ctx, path, "rebase", "--abort")
		return ErrConflict
	}
	return nil
}

// Remove deletes the worktree directory and its branch. Safe to call repeatedly.
func (m *Manager) Remove(ctx context.Context, path, branch string) error {
	_, werr := m.git(ctx, m.Repo, "worktree", "remove", "--force", path)
	if werr != nil {
		_ = os.RemoveAll(path)
		_, _ = m.git(ctx, m.Repo, "worktree", "prune")
	}
	if branch != "" {
		_, _ = m.git(ctx, m.Repo, "branch", "-D", branch)
	}
	return nil
}

// Prune removes stale worktree metadata.
func (m *Manager) Prune(ctx context.Context) error {
	_, err := m.git(ctx, m.Repo, "worktree", "prune")
	return err
}
