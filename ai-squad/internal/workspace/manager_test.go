package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/santillana/ai-squad/internal/core"
	"github.com/santillana/ai-squad/internal/git"
)

// newTestRepo creates a real git repository with one commit on "main",
// isolated from the developer's own global git config.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
	return dir
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(Options{Root: filepath.Join(t.TempDir(), "worktrees")}, git.New())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m
}

func TestAcquireCreatesWorktree(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	ws, err := m.Acquire(ctx, &core.Task{ID: "TASK-1", Repository: repo})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ws.Branch != "ai-squad/task-1" {
		t.Errorf("expected the default branch name, got %s", ws.Branch)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "README.md")); err != nil {
		t.Errorf("expected the worktree to contain the repo's files: %v", err)
	}
}

func TestAcquireHonoursExplicitBranch(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)

	ws, err := m.Acquire(context.Background(), &core.Task{
		ID: "TASK-1", Repository: repo, Branch: "feature/custom-name",
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ws.Branch != "feature/custom-name" {
		t.Errorf("expected the task's own branch name, got %s", ws.Branch)
	}
}

func TestAcquireIsIdempotent(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	task := &core.Task{ID: "TASK-1", Repository: repo}
	first, err := m.Acquire(ctx, task)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Simulate what the scheduler would record after the first Acquire, then
	// call Acquire again as if recovering after a crash.
	task.WorkspacePath = first.Path
	task.Branch = first.Branch

	if err := os.WriteFile(filepath.Join(first.Path, "in-progress.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatalf("seed in-progress file: %v", err)
	}

	second, err := m.Acquire(ctx, task)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.Path != first.Path {
		t.Fatalf("expected the same path, got %s vs %s", second.Path, first.Path)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "in-progress.txt")); err != nil {
		t.Fatal("recovering a task must not lose in-progress work in its workspace")
	}
}

func TestAcquireRejectsNonRepository(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)

	_, err := m.Acquire(context.Background(), &core.Task{ID: "TASK-1", Repository: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error for a non-git directory")
	}
}

func TestAcquireRejectsMissingFields(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)

	if _, err := m.Acquire(context.Background(), nil); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation for a nil task, got %v", err)
	}
	if _, err := m.Acquire(context.Background(), &core.Task{ID: "TASK-1"}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation for a missing repository, got %v", err)
	}
	if _, err := m.Acquire(context.Background(), &core.Task{Repository: repo}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation for a missing task id, got %v", err)
	}
}

func TestAcquireRefusesToClobberUnregisteredDirectory(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)

	path := m.pathFor("TASK-1")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("seed directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "mystery.txt"), []byte("someone's data"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := m.Acquire(context.Background(), &core.Task{ID: "TASK-1", Repository: repo})
	if err == nil {
		t.Fatal("expected acquire to refuse overwriting an unregistered directory")
	}
	if _, statErr := os.Stat(filepath.Join(path, "mystery.txt")); statErr != nil {
		t.Fatal("the pre-existing file must not have been deleted")
	}
}

func TestReleaseCleansUpOnSuccess(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	ws, err := m.Acquire(ctx, &core.Task{ID: "TASK-1", Repository: repo})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Release(ctx, ws, ReleaseOptions{Cleanup: true}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Errorf("expected the worktree directory to be gone, stat error: %v", err)
	}
}

func TestReleaseWithoutCleanupPreservesWorkspace(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	// This is the failure path: a workspace must survive so its state is
	// inspectable after a failed task.
	ws, err := m.Acquire(ctx, &core.Task{ID: "TASK-1", Repository: repo})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Release(ctx, ws, ReleaseOptions{Cleanup: false}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Errorf("expected the workspace to survive a non-cleanup release: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	ws, err := m.Acquire(ctx, &core.Task{ID: "TASK-1", Repository: repo})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := m.Release(ctx, ws, ReleaseOptions{Cleanup: true}); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := m.Release(ctx, ws, ReleaseOptions{Cleanup: true}); err != nil {
		t.Fatalf("releasing an already-removed workspace must not error: %v", err)
	}
}

func TestReleaseRejectsNilWorkspace(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	if err := m.Release(context.Background(), nil, ReleaseOptions{}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

// TestConcurrentAcquireDifferentTasks is the property the whole package
// exists for: N agents acquiring N different workspaces against the same
// repository at once must each get their own worktree, none interfering
// with another, exercising git's own on-disk worktree locking for real.
func TestConcurrentAcquireDifferentTasks(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	const n = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		seen = map[string]bool{}
	)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			taskID := fmt.Sprintf("TASK-%d", i)
			ws, err := m.Acquire(ctx, &core.Task{ID: taskID, Repository: repo})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if seen[ws.Path] {
				errs = append(errs, fmt.Errorf("path %s claimed by more than one task", ws.Path))
			}
			seen[ws.Path] = true
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d errors, first: %v", len(errs), errs[0])
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct workspaces, got %d", n, len(seen))
	}

	entries, err := git.New().WorktreeList(ctx, repo)
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	// +1 for the repository's own main worktree.
	if len(entries) != n+1 {
		t.Errorf("expected %d registered worktrees, got %d", n+1, len(entries))
	}
}

// TestConcurrentAcquireSameTask proves that racing Acquire calls for the SAME
// task never produce two worktrees or a corrupted result: every caller must
// converge on one workspace.
func TestConcurrentAcquireSameTask(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	repo := newTestRepo(t)
	ctx := context.Background()

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*Workspace
		errs    []error
	)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ws, err := m.Acquire(ctx, &core.Task{ID: "TASK-SHARED", Repository: repo})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, ws)
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d errors, first: %v", len(errs), errs[0])
	}
	if len(results) != n {
		t.Fatalf("expected %d successful acquisitions, got %d", n, len(results))
	}
	for _, r := range results[1:] {
		if r.Path != results[0].Path || r.Branch != results[0].Branch {
			t.Fatalf("all callers must converge on one workspace, got %+v and %+v", results[0], r)
		}
	}

	entries, err := git.New().WorktreeList(ctx, repo)
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	registered := 0
	for _, e := range entries {
		if e.Branch == "ai-squad/task-shared" {
			registered++
		}
	}
	if registered != 1 {
		t.Fatalf("expected exactly 1 registered worktree for the shared task, got %d", registered)
	}
}

func TestNewManagerRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	if _, err := NewManager(Options{}, nil); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestSanitizeIsCaseInsensitiveMatch(t *testing.T) {
	t.Parallel()
	if sanitize("TASK-42") != "task-42" {
		t.Errorf("unexpected sanitized id: %s", sanitize("TASK-42"))
	}
}
