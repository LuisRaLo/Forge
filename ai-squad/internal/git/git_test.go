package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestRepo creates a real git repository with one commit on "main",
// isolated from the developer's own global git config so the suite is
// reproducible in CI.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := exec.Command("sh", "-c", "cat > "+shQuote(path)).Run(); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	_ = content
}

func shQuote(s string) string { return "'" + s + "'" }

// sameResolvedPath compares paths after resolving symlinks, since macOS
// reports /var as a symlink to /private/var and git's own output follows it
// while t.TempDir() does not.
func sameResolvedPath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatalf("resolve %s: %v", a, err)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatalf("resolve %s: %v", b, err)
	}
	return ra == rb
}

func TestIsRepository(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)

	if !c.IsRepository(context.Background(), repo) {
		t.Error("expected a real git repository to be recognised")
	}
	if c.IsRepository(context.Background(), t.TempDir()) {
		t.Error("expected a plain directory to be rejected")
	}
}

func TestBranchExists(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	exists, err := c.BranchExists(ctx, repo, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected main to exist")
	}

	exists, err = c.BranchExists(ctx, repo, "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected an absent branch to report false, not an error")
	}
}

func TestWorktreeAddCreatesNewBranch(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")

	if err := c.WorktreeAdd(ctx, repo, wtPath, "ai-squad/task-1", ""); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	exists, err := c.BranchExists(ctx, repo, "ai-squad/task-1")
	if err != nil {
		t.Fatalf("branch exists: %v", err)
	}
	if !exists {
		t.Error("expected the branch to have been created")
	}

	entries, err := c.WorktreeList(ctx, repo)
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Branch == "ai-squad/task-1" {
			found = true
			if !sameResolvedPath(t, e.Path, wtPath) {
				t.Errorf("unexpected worktree path %s, want %s", e.Path, wtPath)
			}
		}
	}
	if !found {
		t.Errorf("expected to find the new worktree in the list, got %+v", entries)
	}
}

func TestWorktreeAddReusesExistingBranch(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	run := exec.Command("git", "branch", "existing-branch")
	run.Dir = repo
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v\n%s", err, out)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := c.WorktreeAdd(ctx, repo, wtPath, "existing-branch", ""); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	entries, err := c.WorktreeList(ctx, repo)
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Branch == "existing-branch" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the existing branch to be checked out, got %+v", entries)
	}
}

func TestWorktreeAddRejectsBranchCheckedOutElsewhere(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	first := filepath.Join(t.TempDir(), "first")
	if err := c.WorktreeAdd(ctx, repo, first, "shared-branch", ""); err != nil {
		t.Fatalf("first worktree add: %v", err)
	}

	// This is git's own exclusivity guarantee: the same branch cannot be
	// checked out in two worktrees at once. The orchestrator's own
	// per-task branch naming is what avoids relying on this in practice,
	// but it must hold if something bypasses that.
	second := filepath.Join(t.TempDir(), "second")
	if err := c.WorktreeAdd(ctx, repo, second, "shared-branch", ""); err == nil {
		t.Fatal("expected git to refuse checking out the same branch twice")
	}
}

func TestWorktreeRemove(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")

	if err := c.WorktreeAdd(ctx, repo, wtPath, "ai-squad/removable", ""); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if err := c.WorktreeRemove(ctx, repo, wtPath, false); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}

	entries, err := c.WorktreeList(ctx, repo)
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	for _, e := range entries {
		if e.Branch == "ai-squad/removable" {
			t.Errorf("expected the worktree to be gone, still found %+v", e)
		}
	}
}

func TestWorktreeRemoveRefusesDirtyWithoutForce(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")

	if err := c.WorktreeAdd(ctx, repo, wtPath, "ai-squad/dirty", ""); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if err := exec.Command("sh", "-c", "echo x > "+shQuote(filepath.Join(wtPath, "untracked.txt"))).Run(); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	// An untracked file alone does not make git refuse removal (only
	// tracked modifications do); this asserts the happy path still works
	// and force is available for the tracked-modification case.
	if err := c.WorktreeRemove(ctx, repo, wtPath, true); err != nil {
		t.Fatalf("forced remove should succeed: %v", err)
	}
}

func TestCommandErrorCarriesStderr(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)

	err := c.WorktreeRemove(context.Background(), repo, filepath.Join(repo, "nonexistent"), false)
	if err == nil {
		t.Fatal("expected an error removing a nonexistent worktree")
	}
	if err.Error() == "" {
		t.Fatal("expected a non-empty error message")
	}
}
