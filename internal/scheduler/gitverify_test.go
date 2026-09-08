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

// gatedPassingRuntime commits a file, like commitingRuntime, but also
// returns a passing gate verdict — simulating a QA agent that actually ran
// the test suite and reported it green.
type gatedPassingRuntime struct{}

func (r gatedPassingRuntime) Name() string { return "gate" }
func (r gatedPassingRuntime) Capabilities() core.CapabilitySet {
	return core.NewCapabilitySet(core.CapabilityFilesystemWrite, core.CapabilityStructuredOutput)
}
func (r gatedPassingRuntime) Execute(ctx context.Context, req core.RunRequest, _ core.EventSink) (*core.RunResult, error) {
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
	return &core.RunResult{
		Text: "done", StopReason: "end_turn",
		Structured: []byte(`{"passed": true, "summary": "tests pass"}`),
	}, nil
}

// TestGatePassPushesBranchToOrigin proves the pipeline's other missing
// piece: once a gate step (standing in for QA actually running the test
// suite) verifies real work and reports pass, the branch is pushed to the
// configured remote automatically — no separate manual step required.
func TestGatePassPushesBranchToOrigin(t *testing.T) {
	t.Parallel()
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
		Name: "qa", Runtime: "mock", SystemPrompt: "p",
		Permissions: core.Permissions{Filesystem: core.FSWorkspace, GitWrite: true},
		Gate:        core.GateConfig{Enabled: true, StepsBack: 1},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	repoDir := newRealTestRepo(t)

	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "remote", "add", "origin", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}

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
		Agents: registry, Runtimes: singleRuntime{gatedPassingRuntime{}}, Workspaces: wsManager, Git: gitClient,
		Workflows: fakeWorkflows{}, RuntimeFor: func(*core.Task) string { return "mock" },
		Clock: core.SystemClock, Log: logger,
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}

	task, err := taskRepo.Create(ctx, &core.Task{Title: "verified work", Repository: repoDir, Agent: "qa"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := taskRepo.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	got, err := taskRepo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Branch == "" {
		t.Fatal("expected the task to have a recorded branch")
	}

	waitFor(t, 2*time.Second, func() bool {
		out, err := exec.Command("git", "-C", bareDir, "branch", "--list", got.Branch).Output()
		return err == nil && len(out) > 0
	})
}

// TestSpecFileDoesNotMaskAStepThatDidNothing proves the interaction between
// writeSpecIfMissing and hasRealChanges is safe: ai-squad's own "docs: record
// spec" commit must never be mistaken for the agent's own work. Without the
// core.SpecCommitMetadataKey override in execute(), this would regress
// TestGitWriteStepWithNoChangesFails into a false COMPLETED.
func TestSpecFileDoesNotMaskAStepThatDidNothing(t *testing.T) {
	t.Parallel()
	sched, taskRepo, repoDir := newGitVerifyEnv(t, commitingRuntime{shouldCommit: false})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := taskRepo.Create(ctx, &core.Task{
		Title: "does nothing", Description: "should never complete", Repository: repoDir, Agent: "developer",
	})
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
		t.Fatalf("expected FAILED even with a spec commit present, got %s", got.Status)
	}
	specCommit, ok := got.Metadata[core.SpecCommitMetadataKey]
	if !ok || specCommit == "" {
		t.Fatal("expected the spec commit to have been recorded on the task despite the step failing")
	}

	out, err := exec.Command("git", "-C", got.WorkspacePath, "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if !containsSub(string(out), "spec/features/") {
		t.Errorf("expected a spec file under spec/features/, got tree:\n%s", out)
	}
}

// TestSpecFileIsRecordedAlongsideRealWork proves the spec file lands in the
// workspace's history alongside a step that actually does the work.
func TestSpecFileIsRecordedAlongsideRealWork(t *testing.T) {
	t.Parallel()
	sched, taskRepo, repoDir := newGitVerifyEnv(t, commitingRuntime{shouldCommit: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := taskRepo.Create(ctx, &core.Task{
		Title: "hello world in java", Description: "print hello world", Repository: repoDir, Agent: "developer",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	go sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := taskRepo.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	got, err := taskRepo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := got.Metadata[core.SpecCommitMetadataKey]; !ok {
		t.Error("expected the spec commit to have been recorded on the task")
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
