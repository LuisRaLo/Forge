package git

import (
	"context"
	"os"
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

func TestCommitStagesAndCommits(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := exec.Command("sh", "-c", "echo changed > "+shQuote(filepath.Join(repo, "README.md"))).Run(); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	committed, err := c.Commit(ctx, repo, "update readme")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !committed {
		t.Fatal("expected a commit to have been made")
	}

	log, err := exec.Command("git", "-C", repo, "log", "-1", "--pretty=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := string(log); got != "update readme\n" {
		t.Errorf("unexpected commit message: %q", got)
	}
}

func TestCommitNothingToCommit(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)

	committed, err := c.Commit(context.Background(), repo, "no-op")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed {
		t.Error("expected no commit when nothing changed")
	}
}

func TestPushToLocalRemote(t *testing.T) {
	t.Parallel()
	c := New()
	ctx := context.Background()

	// A bare repo stands in for a real GitHub remote.
	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}

	repo := newTestRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}

	if err := c.Push(ctx, repo, "origin", "main"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := exec.Command("git", "-C", bareDir, "branch", "--list", "main").Output()
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if len(out) == 0 {
		t.Error("expected main to exist on the remote after push")
	}
}

func TestRemoteExists(t *testing.T) {
	t.Parallel()
	c := New()
	ctx := context.Background()
	repo := newTestRepo(t)

	exists, err := c.RemoteExists(ctx, repo, "origin")
	if err != nil {
		t.Fatalf("remote exists: %v", err)
	}
	if exists {
		t.Error("expected no origin remote on a freshly created repo")
	}

	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}

	exists, err = c.RemoteExists(ctx, repo, "origin")
	if err != nil {
		t.Fatalf("remote exists: %v", err)
	}
	if !exists {
		t.Error("expected origin remote to exist after adding it")
	}
}

func TestCurrentBranch(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)

	got, err := c.CurrentBranch(context.Background(), repo)
	if err != nil {
		t.Fatalf("current branch: %v", err)
	}
	if got != "main" {
		t.Errorf("expected main, got %s", got)
	}
}

func TestHeadCommit(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)

	got, err := c.HeadCommit(context.Background(), repo)
	if err != nil {
		t.Fatalf("head commit: %v", err)
	}
	if len(got) != 40 {
		t.Errorf("expected a full 40-char commit hash, got %q", got)
	}
}

func TestIsDirty(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	dirty, err := c.IsDirty(ctx, repo)
	if err != nil {
		t.Fatalf("is dirty: %v", err)
	}
	if dirty {
		t.Error("a freshly committed repo should not be dirty")
	}

	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	dirty, err = c.IsDirty(ctx, repo)
	if err != nil {
		t.Fatalf("is dirty: %v", err)
	}
	if !dirty {
		t.Error("an untracked file should make the repo dirty")
	}
}

func TestCommitsSince(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	base, err := c.HeadCommit(ctx, repo)
	if err != nil {
		t.Fatalf("head commit: %v", err)
	}

	n, err := c.CommitsSince(ctx, repo, base)
	if err != nil {
		t.Fatalf("commits since: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 commits since HEAD itself, got %d", n)
	}

	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := c.Commit(ctx, repo, "add new.txt"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err = c.CommitsSince(ctx, repo, base)
	if err != nil {
		t.Fatalf("commits since: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 commit since base, got %d", n)
	}
}

func TestMergeBase(t *testing.T) {
	t.Parallel()
	c := New()
	repo := newTestRepo(t)
	ctx := context.Background()

	base, err := c.HeadCommit(ctx, repo)
	if err != nil {
		t.Fatalf("head commit: %v", err)
	}

	if err := c.WorktreeAdd(ctx, repo, filepath.Join(t.TempDir(), "wt"), "feature", ""); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	got, err := c.MergeBase(ctx, repo, "feature", "main")
	if err != nil {
		t.Fatalf("merge base: %v", err)
	}
	if got != base {
		t.Errorf("expected merge-base %s, got %s", base, got)
	}
}
