package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/agents"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/git"
	"github.com/LuisRaLo/ai-squad/internal/storage"
	"github.com/LuisRaLo/ai-squad/internal/workspace"
)

// newRealTestRepo creates a real, minimal git repository, matching the
// pattern already established in internal/workspace and internal/git's own
// tests — this file specifically needs a REAL repository (not
// fakeWorkspaces) because it is proving behaviour that only exists against
// real git: whether a step actually changed anything.
func newRealTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// commitingRuntime writes a file and commits it inside the workspace it is
// given, simulating a developer agent that actually does its job — the
// counterpart to the default mock runtime, which never touches disk at all.
type commitingRuntime struct{ shouldCommit bool }

func (r commitingRuntime) Name() string { return "committer" }
func (r commitingRuntime) Capabilities() core.CapabilitySet {
	return core.NewCapabilitySet(core.CapabilityFilesystemWrite)
}
func (r commitingRuntime) Execute(ctx context.Context, req core.RunRequest, _ core.EventSink) (*core.RunResult, error) {
	if r.shouldCommit {
		path := filepath.Join(req.WorkspaceDir, "output.txt")
		if err := os.WriteFile(path, []byte("real work\n"), 0o644); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "did the task"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = req.WorkspaceDir
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git %v: %v: %s", args, err, out)
			}
		}
	}
	return &core.RunResult{Text: "done", StopReason: "end_turn"}, nil
}

func newGitVerifyEnv(t *testing.T, rt core.AgentRuntime) (*Scheduler, core.TaskRepository, string) {
	t.Helper()
	ctx := context.Background()

	db, err := storage.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	registry, err := agents.NewRegistry(&core.AgentDefinition{
		Name: "developer", Runtime: "mock", SystemPrompt: "p",
		Permissions: core.Permissions{Filesystem: core.FSWorkspace, GitWrite: true},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	repoDir := newRealTestRepo(t)
	gitClient := git.New()
	wsManager, err := workspace.NewManager(workspace.Options{Root: filepath.Join(t.TempDir(), "worktrees")}, gitClient)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	taskRepo := storage.NewTaskRepo(db, core.SystemClock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	sched, err := New(Config{
		MaxConcurrency: 1, PollInterval: 10 * time.Millisecond, MaxStepIterations: 2, MaxTaskAttempts: 3,
	}, Deps{
		Tasks: taskRepo, Runs: storage.NewRunRepo(db), Artifacts: storage.NewArtifactRepo(db, core.SystemClock),
		Agents: registry, Runtimes: singleRuntime{rt}, Workspaces: wsManager, Git: gitClient,
		Workflows: fakeWorkflows{}, RuntimeFor: func(*core.Task) string { return "mock" },
		Clock: core.SystemClock, Log: logger,
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	return sched, taskRepo, repoDir
}

func TestGitWriteStepWithNoChangesFails(t *testing.T) {
	t.Parallel()
	sched, taskRepo, repoDir := newGitVerifyEnv(t, commitingRuntime{shouldCommit: false})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := taskRepo.Create(ctx, &core.Task{Title: "does nothing", Repository: repoDir, Agent: "developer"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	go sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := taskRepo.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusFailed
	})

	got, err := taskRepo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusFailed {
		t.Fatalf("expected FAILED, got %s", got.Status)
	}
	if !containsSub(got.LastError, "no changes") && !containsSub(got.LastError, "nothing to advance") {
		t.Errorf("expected the failure reason to explain no changes were made, got %q", got.LastError)
	}
}

func TestGitWriteStepWithRealCommitCompletes(t *testing.T) {
	t.Parallel()
	sched, taskRepo, repoDir := newGitVerifyEnv(t, commitingRuntime{shouldCommit: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := taskRepo.Create(ctx, &core.Task{Title: "does real work", Repository: repoDir, Agent: "developer"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	go sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := taskRepo.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
