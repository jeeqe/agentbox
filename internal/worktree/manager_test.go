package worktree

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreateLinksEnvAllocatesPortAndSnapshots(t *testing.T) {
	t.Parallel()
	requireLoopback(t)
	ctx := context.Background()
	repo := testRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".env.local"), []byte("TOKEN=test\n"), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}

	m := testManager(t, repo)
	m.BasePort = availablePort(t)
	base := testBranch(t, repo)
	info, err := m.Create(ctx, 7, 1, "Fix Login!!!", base, map[int]bool{m.BasePort: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Remove(context.Background(), info.Path, info.Branch) })
	if info.Branch != "agentbox/7-1-fix-login" {
		t.Errorf("branch = %q, want agentbox/7-1-fix-login", info.Branch)
	}
	if info.Port <= m.BasePort {
		t.Errorf("port = %d, want a free port after reserved base port %d", info.Port, m.BasePort)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}
	envPath := filepath.Join(info.Path, ".env.local")
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("linked env not readable: %v", err)
	}
	if string(data) != "TOKEN=test\n" {
		t.Errorf("linked env = %q, want source contents", data)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Lstat(envPath)
		if err != nil {
			t.Fatalf("lstat linked env: %v", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf(".env.local mode = %v, want a symlink", fi.Mode())
		}
	}

	path := filepath.Join(info.Path, "tracked.txt")
	if err := os.WriteFile(path, []byte("first\nchanged\nadded\n"), 0o644); err != nil {
		t.Fatalf("write worktree change: %v", err)
	}
	patch, stats, err := m.Snapshot(ctx, info.Path, base)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.Contains(patch, "+added") || !strings.Contains(patch, "-second") {
		t.Errorf("Snapshot patch did not contain expected changes:\n%s", patch)
	}
	if stats.Additions != 2 || stats.Deletions != 1 {
		t.Errorf("Snapshot stats = %+v, want +2/-1", stats)
	}

	if err := m.Remove(ctx, info.Path, info.Branch); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Errorf("removed worktree stat error = %v, want not exist", err)
	}
	if _, err := gitResult(ctx, repo, "rev-parse", "--verify", info.Branch); err == nil {
		t.Error("removed worktree branch still exists")
	}
	if err := m.Remove(ctx, info.Path, info.Branch); err != nil {
		t.Fatalf("second Remove should be safe: %v", err)
	}
}

func TestAllocPortSkipsBoundAndUsedPorts(t *testing.T) {
	t.Parallel()
	requireLoopback(t)
	repo := testRepository(t)
	m := testManager(t, repo)
	m.BasePort = availablePort(t)

	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.BasePort))
	if err != nil {
		t.Fatalf("reserve base port: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	port, err := m.allocPort(map[int]bool{m.BasePort + 1: true})
	if err != nil {
		t.Fatalf("allocPort: %v", err)
	}
	if port <= m.BasePort+1 {
		t.Errorf("allocPort = %d, want a free port after bound %d and used %d", port, m.BasePort, m.BasePort+1)
	}
}

func TestMergeAndConflict(t *testing.T) {
	t.Parallel()
	requireLoopback(t)
	ctx := context.Background()

	t.Run("merge", func(t *testing.T) {
		repo := testRepository(t)
		m := testManager(t, repo)
		base := testBranch(t, repo)
		info, err := m.Create(ctx, 8, 1, "add feature", base, nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
			t.Fatalf("write feature: %v", err)
		}
		if _, _, err := m.Snapshot(ctx, info.Path, base); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := m.Merge(ctx, info.Branch, base); err != nil {
			t.Fatalf("Merge: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
		if err != nil || string(data) != "feature\n" {
			t.Fatalf("merged feature = %q, %v", data, err)
		}
		if err := m.Remove(ctx, info.Path, info.Branch); err != nil {
			t.Fatalf("Remove merged worktree: %v", err)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		repo := testRepository(t)
		m := testManager(t, repo)
		base := testBranch(t, repo)
		info, err := m.Create(ctx, 9, 1, "conflicting change", base, nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		t.Cleanup(func() { _ = m.Remove(context.Background(), info.Path, info.Branch) })
		if err := os.WriteFile(filepath.Join(info.Path, "tracked.txt"), []byte("branch change\n"), 0o644); err != nil {
			t.Fatalf("write branch change: %v", err)
		}
		if _, _, err := m.Snapshot(ctx, info.Path, base); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("main change\n"), 0o644); err != nil {
			t.Fatalf("write main change: %v", err)
		}
		gitOutput(context.Background(), repo, "add", "tracked.txt")
		gitOutput(context.Background(), repo, "commit", "-m", "main change")

		err = m.Merge(ctx, info.Branch, base)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("Merge conflict error = %v, want ErrConflict", err)
		}
		data, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
		if err != nil || string(data) != "main change\n" {
			t.Fatalf("main tree after aborted conflict = %q, %v", data, err)
		}
		status, err := gitResult(ctx, repo, "status", "--porcelain")
		if err != nil {
			t.Fatalf("status after conflict: %v", err)
		}
		if strings.TrimSpace(status) != "" {
			t.Errorf("main working tree not clean after aborted conflict: %q", status)
		}
	})
}

func testRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	ctx := context.Background()
	gitOutput(ctx, repo, "init")
	gitOutput(ctx, repo, "config", "user.email", "agentbox-test@example.invalid")
	gitOutput(ctx, repo, "config", "user.name", "AgentBox Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env*\n"), 0o644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	gitOutput(ctx, repo, "add", "tracked.txt", ".gitignore")
	gitOutput(ctx, repo, "commit", "-m", "initial")
	return repo
}

func testManager(t *testing.T, repo string) *Manager {
	t.Helper()
	m, err := New(repo)
	if err != nil {
		t.Fatalf("New manager: %v", err)
	}
	return m
}

func testBranch(t *testing.T, repo string) string {
	t.Helper()
	out := gitOutput(context.Background(), repo, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out)
}

func availablePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find available port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func requireLoopback(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets are unavailable in this environment: %v", err)
	}
	_ = l.Close()
}

func gitOutput(ctx context.Context, dir string, args ...string) string {
	out, err := gitResult(ctx, dir, args...)
	if err != nil {
		panic(fmt.Sprintf("git %s: %v\n%s", strings.Join(args, " "), err, out))
	}
	return out
}

func gitResult(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSnapshotDoesNotCommitLinkedEnvFile(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")

	m, err := New(repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := m.Create(ctx, 1, 1, "x", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, st, err := m.Snapshot(ctx, info.Path, "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(patch, ".env") || st.Additions != 1 {
		t.Fatalf("env file leaked into snapshot: adds=%d patch=%q", st.Additions, patch)
	}
}
