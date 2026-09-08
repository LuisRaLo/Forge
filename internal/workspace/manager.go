// Package workspace manages the isolated git worktree assigned to each task.
//
// A workspace is purely a filesystem/git concern: Manager returns a
// Workspace value but never persists it. Recording Workspace.Path and
// Workspace.Branch onto a core.Task is the caller's job — the scheduler in
// Phase 4 — via the existing core.TaskRepository.Transition mutator. Keeping
// persistence out of this package is what lets it be tested without a
// database.
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/git"
)

// DefaultBranchPrefix namespaces branches ai-squad creates for a task, so
// they never collide with a human's own branches.
const DefaultBranchPrefix = "ai-squad/"

// Options configures a Manager.
type Options struct {
	// Root is the directory holding every task's worktree, typically
	// <data_dir>/worktrees. Each task gets Root/<task-id>.
	Root string
	// BranchPrefix namespaces auto-generated branch names. Defaults to
	// DefaultBranchPrefix.
	BranchPrefix string
}

// Workspace is the git worktree assigned to one task.
type Workspace struct {
	TaskID  string
	RepoDir string
	Path    string
	Branch  string
	// BaseCommit is RepoDir's HEAD at the moment this workspace's branch was
	// created — the point the task's work forked from. It lets a caller
	// distinguish "nothing happened in this step" from "the agent made and
	// committed real changes" by counting commits since this point, rather
	// than trusting the agent's own report of what it did.
	BaseCommit string
}

// Manager creates, reuses and removes per-task git worktrees.
//
// Exclusivity between two DIFFERENT tasks is inherited from the task state
// machine: only the worker that won a task's Transition to RUNNING/PLANNING
// calls Acquire for it (proven by the concurrency tests in
// internal/storage), and each task's path and branch are derived from its
// own unique ID, so two tasks can never collide on either. Manager's own
// per-task mutex exists only to make a single task's Acquire/Release safe
// against being called twice concurrently within one process, which the
// state machine does not by itself rule out (e.g. a caller bug retrying a
// call still in flight).
type Manager struct {
	root   string
	prefix string
	git    *git.Client

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	repoMu    sync.Mutex
	repoLocks map[string]*sync.Mutex
}

// NewManager builds a workspace manager. A nil client uses git.New().
func NewManager(opts Options, client *git.Client) (*Manager, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, core.Invalid("root", "must be set")
	}
	if client == nil {
		client = git.New()
	}
	prefix := opts.BranchPrefix
	if prefix == "" {
		prefix = DefaultBranchPrefix
	}
	return &Manager{
		root: opts.Root, prefix: prefix, git: client,
		locks: map[string]*sync.Mutex{}, repoLocks: map[string]*sync.Mutex{},
	}, nil
}

func (m *Manager) lockFor(taskID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[taskID]
	if !ok {
		l = &sync.Mutex{}
		m.locks[taskID] = l
	}
	return l
}

// repoLockFor serializes git worktree mutations against one repository.
//
// git's own `.git/worktrees/<name>/` administrative bookkeeping is not safe
// under many concurrent `worktree add`/`remove` invocations against the same
// repository — reproduced directly under this project's own full test suite
// (many packages spawning git subprocesses at once triggered "failed to read
// .git/worktrees/.../commondir" in `go test -race` on an otherwise correct,
// per-task-locked call). The per-task lock above only prevents one task's
// own Acquire/Release from racing itself; it does nothing for two DIFFERENT
// tasks mutating the same repository's worktree metadata concurrently. This
// second lock is keyed by repository path, so unrelated repositories are
// unaffected and still proceed in parallel.
func (m *Manager) repoLockFor(repoDir string) *sync.Mutex {
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	l, ok := m.repoLocks[repoDir]
	if !ok {
		l = &sync.Mutex{}
		m.repoLocks[repoDir] = l
	}
	return l
}

// Acquire ensures a git worktree exists for task and returns it.
//
// It is idempotent: if task.WorkspacePath already names this task's
// worktree and that worktree is still registered with git, it is reused
// rather than recreated. This is what lets a task recovered after a crash
// resume in the same workspace instead of losing in-progress, uncommitted
// work sitting on disk.
func (m *Manager) Acquire(ctx context.Context, task *core.Task) (*Workspace, error) {
	if task == nil {
		return nil, core.Invalid("task", "must not be nil")
	}
	if strings.TrimSpace(task.Repository) == "" {
		return nil, core.Invalid("task.repository", "must be set to acquire a workspace")
	}
	if strings.TrimSpace(task.ID) == "" {
		return nil, core.Invalid("task.id", "must be set to acquire a workspace")
	}

	lock := m.lockFor(task.ID)
	lock.Lock()
	defer lock.Unlock()

	if !m.git.IsRepository(ctx, task.Repository) {
		return nil, fmt.Errorf("workspace for %s: %s is not a git repository", task.ID, task.Repository)
	}

	path := m.pathFor(task.ID)
	branch := task.Branch
	if branch == "" {
		branch = m.branchFor(task.ID)
	}

	repoLock := m.repoLockFor(task.Repository)
	repoLock.Lock()
	defer repoLock.Unlock()

	registered, err := m.registeredWorktree(ctx, task.Repository, path)
	if err != nil {
		return nil, err
	}
	if registered != nil {
		ws := &Workspace{TaskID: task.ID, RepoDir: task.Repository, Path: path, Branch: branch}
		if registered.Branch != "" {
			ws.Branch = registered.Branch
		}
		ws.BaseCommit = m.baseCommitFor(ctx, task.Repository, ws.Branch)
		return ws, nil
	}

	// The path is not a registered worktree. Refuse to create one on top of
	// something already there rather than silently overwriting it — it may
	// be a leftover from manual tampering or a prior crash mid-creation,
	// and deleting it without being asked is exactly the kind of surprise
	// destructive default this orchestrator's own security rules forbid.
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf(
			"workspace for %s: %s already exists on disk and is not a registered git worktree; "+
				"remove it manually before retrying", task.ID, path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat workspace path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree root: %w", err)
	}
	if err := m.git.WorktreeAdd(ctx, task.Repository, path, branch, ""); err != nil {
		return nil, fmt.Errorf("create worktree for %s: %w", task.ID, err)
	}

	ws := &Workspace{TaskID: task.ID, RepoDir: task.Repository, Path: path, Branch: branch}
	ws.BaseCommit = m.baseCommitFor(ctx, task.Repository, branch)
	return ws, nil
}

// baseCommitFor returns the commit where branch diverged from repoDir's own
// current branch — the fork point a caller can diff against to tell whether
// any real work has happened yet. Best-effort: on any error (e.g. repoDir
// happens to be in a detached-HEAD state), it returns "", and callers must
// treat that as "unknown" rather than "no changes," never fail Acquire over
// it — this is a diagnostic aid, not a correctness requirement for
// workspace creation itself.
func (m *Manager) baseCommitFor(ctx context.Context, repoDir, branch string) string {
	repoBranch, err := m.git.CurrentBranch(ctx, repoDir)
	if err != nil {
		return ""
	}
	base, err := m.git.MergeBase(ctx, repoDir, branch, repoBranch)
	if err != nil {
		return ""
	}
	return base
}

// registeredWorktree returns the git-registered entry at path, or nil if
// none exists. Paths are compared after resolving symlinks, since macOS
// reports /var as a symlink to /private/var and git's own output follows it.
func (m *Manager) registeredWorktree(ctx context.Context, repoDir, path string) (*git.WorktreeEntry, error) {
	entries, err := m.git.WorktreeList(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	for i, e := range entries {
		if samePath(e.Path, path) {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// ReleaseOptions controls what happens to a workspace once a task step ends.
type ReleaseOptions struct {
	// Cleanup removes the worktree. Callers must only set this once the task
	// finished successfully: a failed task's workspace is preserved for
	// postmortem, never removed here.
	Cleanup bool
	// Force discards uncommitted changes during cleanup. Ignored unless
	// Cleanup is set.
	Force bool
}

// Release optionally removes a workspace. It is a no-op unless
// opts.Cleanup is set, and safe to call on a workspace already absent from
// disk.
func (m *Manager) Release(ctx context.Context, ws *Workspace, opts ReleaseOptions) error {
	if ws == nil {
		return core.Invalid("workspace", "must not be nil")
	}
	if !opts.Cleanup {
		return nil
	}

	lock := m.lockFor(ws.TaskID)
	lock.Lock()
	defer lock.Unlock()

	repoLock := m.repoLockFor(ws.RepoDir)
	repoLock.Lock()
	defer repoLock.Unlock()

	registered, err := m.registeredWorktree(ctx, ws.RepoDir, ws.Path)
	if err != nil {
		return err
	}
	if registered == nil {
		// Already gone; releasing a workspace that isn't there is not an
		// error, so a retried Release stays safe.
		return nil
	}

	if err := m.git.WorktreeRemove(ctx, ws.RepoDir, ws.Path, opts.Force); err != nil {
		return fmt.Errorf("remove worktree for %s: %w", ws.TaskID, err)
	}
	return nil
}

func (m *Manager) pathFor(taskID string) string {
	return filepath.Join(m.root, sanitize(taskID))
}

func (m *Manager) branchFor(taskID string) string {
	return m.prefix + sanitize(taskID)
}

// sanitize lower-cases a task ID for conventional branch/directory naming.
// Task IDs are TASK-N, already filesystem- and ref-safe.
func sanitize(id string) string {
	return strings.ToLower(id)
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
