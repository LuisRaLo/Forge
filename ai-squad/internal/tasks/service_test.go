package tasks

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/agents"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/storage"
)

// fakeWorkflows is a minimal Workflows implementation for tests.
type fakeWorkflows map[string][]string

func (f fakeWorkflows) Steps(name string) ([]string, error) {
	steps, ok := f[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return steps, nil
}

func newTestService(t *testing.T) (*Service, core.TaskRepository) {
	t.Helper()
	ctx := context.Background()

	db, err := storage.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := storage.NewTaskRepo(db, core.SystemClock)

	registry, err := agents.NewRegistry(
		&core.AgentDefinition{Name: "architect", Runtime: "claude", SystemPrompt: "p"},
		&core.AgentDefinition{Name: "developer", Runtime: "claude", SystemPrompt: "p"},
		&core.AgentDefinition{Name: "qa", Runtime: "claude", SystemPrompt: "p"},
	)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	svc, err := NewService(repo, registry, fakeWorkflows{
		"feature": {"architect", "developer", "qa"},
		"bugfix":  {"developer", "qa"},
		"broken":  {"ghost"},
	}, Options{DefaultMaxAttempts: 3, SkipRepositoryCheck: true})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, repo
}

func TestCreateFromWorkflowPicksFirstStep(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	task, err := svc.Create(context.Background(), CreateParams{
		Title:    "Add Google authentication",
		Workflow: "feature",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.Agent != "architect" {
		t.Errorf("a feature workflow must start at the architect, got %q", task.Agent)
	}
	if task.Status != core.StatusPending {
		t.Errorf("expected PENDING, got %s", task.Status)
	}
	if task.Step != 0 {
		t.Errorf("expected step 0, got %d", task.Step)
	}
	if task.MaxAttempts != 3 {
		t.Errorf("expected the configured default of 3 attempts, got %d", task.MaxAttempts)
	}
}

func TestCreateRequiresExactlyOneOfWorkflowOrAgent(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, CreateParams{Title: "t"}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("neither given: expected ErrValidation, got %v", err)
	}
	_, err := svc.Create(ctx, CreateParams{Title: "t", Workflow: "feature", Agent: "developer"})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("both given: expected ErrValidation, got %v", err)
	}
}

func TestCreateRejectsUnknownWorkflowAndAgent(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, CreateParams{Title: "t", Workflow: "ghost"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{Title: "t", Agent: "ghost"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// A workflow whose first step names a missing agent must fail at
	// creation, not when the scheduler tries to run it.
	if _, err := svc.Create(ctx, CreateParams{Title: "t", Workflow: "broken"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCreateRejectsEmptyTitle(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Create(context.Background(), CreateParams{Title: "  ", Workflow: "feature"})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestCreateIsIdempotentUnderAKey(t *testing.T) {
	t.Parallel()
	svc, repo := newTestService(t)
	ctx := context.Background()

	params := CreateParams{Title: "once", Workflow: "bugfix", IdempotencyKey: "req-1"}

	first, err := svc.Create(ctx, params)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := svc.Create(ctx, params)
	if err != nil {
		t.Fatalf("replayed create must succeed: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay created a duplicate: %s then %s", first.ID, second.ID)
	}

	all, err := repo.List(ctx, core.TaskFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 task, got %d", len(all))
	}
}

func TestCreateWithoutKeyAllowsDuplicateTitles(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	a, err := svc.Create(ctx, CreateParams{Title: "same", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := svc.Create(ctx, CreateParams{Title: "same", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("without an idempotency key, two creates are two tasks")
	}
}

func TestCreateRejectsUnknownParent(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Create(context.Background(), CreateParams{
		Title: "child", Workflow: "bugfix", ParentTask: "TASK-404",
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	task, err := svc.Create(ctx, CreateParams{Title: "cancel me", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	first, err := svc.Cancel(ctx, task.ID, "operator changed their mind")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if first.Status != core.StatusCancelled {
		t.Fatalf("expected CANCELLED, got %s", first.Status)
	}

	second, err := svc.Cancel(ctx, task.ID, "again")
	if err != nil {
		t.Fatalf("cancelling twice must succeed: %v", err)
	}
	if second.Status != core.StatusCancelled {
		t.Errorf("expected CANCELLED, got %s", second.Status)
	}

	// The repeat must not add a second cancellation to the audit trail.
	events, err := svc.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	cancellations := 0
	for _, ev := range events {
		if ev.ToStatus == core.StatusCancelled {
			cancellations++
		}
	}
	if cancellations != 1 {
		t.Errorf("expected 1 cancellation event, got %d", cancellations)
	}
}

func TestCancelRefusesCompletedTask(t *testing.T) {
	t.Parallel()
	svc, repo := newTestService(t)
	ctx := context.Background()

	task, err := svc.Create(ctx, CreateParams{Title: "done", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, to := range []core.TaskStatus{core.StatusReady, core.StatusRunning, core.StatusCompleted} {
		if _, err := repo.Transition(ctx, task.ID, to, "", nil); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}

	if _, err := svc.Cancel(ctx, task.ID, ""); !errors.Is(err, core.ErrTerminal) {
		t.Fatalf("expected ErrTerminal, got %v", err)
	}
}

func TestRetryRequeuesFailedTask(t *testing.T) {
	t.Parallel()
	svc, repo := newTestService(t)
	ctx := context.Background()

	task, err := svc.Create(ctx, CreateParams{Title: "retry me", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, to := range []core.TaskStatus{core.StatusReady, core.StatusRunning} {
		if _, err := repo.Transition(ctx, task.ID, to, "", nil); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusFailed, "boom", func(t *core.Task) {
		t.Attempts = 3
		t.LastError = "compilation failed"
	}); err != nil {
		t.Fatalf("to failed: %v", err)
	}

	got, err := svc.Retry(ctx, task.ID, "")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.Status != core.StatusReady {
		t.Errorf("expected READY, got %s", got.Status)
	}
	if got.Attempts != 0 {
		t.Errorf("a manual retry must restore the attempt budget, got %d", got.Attempts)
	}
	if got.LastError != "" {
		t.Errorf("stale error should be cleared, got %q", got.LastError)
	}
}

func TestRetryOnQueuedTaskIsANoOp(t *testing.T) {
	t.Parallel()
	svc, repo := newTestService(t)
	ctx := context.Background()

	task, err := svc.Create(ctx, CreateParams{Title: "queued", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusReady, "", nil); err != nil {
		t.Fatalf("to ready: %v", err)
	}

	before, err := svc.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	got, err := svc.Retry(ctx, task.ID, "")
	if err != nil {
		t.Fatalf("retrying a queued task must not error: %v", err)
	}
	if got.Status != core.StatusReady {
		t.Errorf("expected READY, got %s", got.Status)
	}

	after, err := svc.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a no-op retry must not write history: %d -> %d events", len(before), len(after))
	}
}

func TestRetryRefusesRunningTask(t *testing.T) {
	t.Parallel()
	svc, repo := newTestService(t)
	ctx := context.Background()

	task, err := svc.Create(ctx, CreateParams{Title: "busy", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, to := range []core.TaskStatus{core.StatusReady, core.StatusRunning} {
		if _, err := repo.Transition(ctx, task.ID, to, "", nil); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}

	// Re-queueing work that a worker currently owns would let two agents run
	// the same task.
	if _, err := svc.Retry(ctx, task.ID, ""); !errors.Is(err, core.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
}

func TestRetryUnblocksBlockedTask(t *testing.T) {
	t.Parallel()
	svc, repo := newTestService(t)
	ctx := context.Background()

	task, err := svc.Create(ctx, CreateParams{Title: "blocked", Workflow: "bugfix"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusBlocked, "loop limit reached", nil); err != nil {
		t.Fatalf("to blocked: %v", err)
	}

	got, err := svc.Retry(ctx, task.ID, "operator intervened")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.Status != core.StatusReady {
		t.Errorf("expected READY, got %s", got.Status)
	}
}

func TestNewServiceRequiresRepository(t *testing.T) {
	t.Parallel()

	if _, err := NewService(nil, nil, nil, Options{}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestResolveRepositoryRejectsMissingPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := storage.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := storage.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Repository checking on: a bad path must be caught at creation time.
	svc, err := NewService(storage.NewTaskRepo(db, core.SystemClock), nil, fakeWorkflows{"bugfix": {"dev"}}, Options{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	_, err = svc.Create(ctx, CreateParams{
		Title: "t", Workflow: "bugfix", Repository: t.TempDir() + "/does-not-exist",
	})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}

	// An existing directory is accepted and stored as an absolute path.
	task, err := svc.Create(ctx, CreateParams{Title: "t", Workflow: "bugfix", Repository: t.TempDir()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.Repository == "" {
		t.Error("repository path should be recorded")
	}
}

func TestCreateRejectsHomeDirectoryAsRepository(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available in this environment")
	}

	db, err := storage.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := storage.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Repository checking ON: the guard must fire even without a real
	// git repository being required first.
	svc, err := NewService(storage.NewTaskRepo(db, core.SystemClock), nil,
		fakeWorkflows{"bugfix": {"dev"}}, Options{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	_, err = svc.Create(context.Background(), CreateParams{
		Title: "t", Workflow: "bugfix", Repository: home,
	})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected the home directory to be rejected, got %v", err)
	}
}

func TestRejectSensitiveRepositoryDirectly(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available in this environment")
	}

	if err := rejectSensitiveRepository(home); !errors.Is(err, core.ErrValidation) {
		t.Errorf("expected home directory rejected, got %v", err)
	}
	for _, dir := range []string{".ssh", ".aws", ".kube"} {
		path := home + "/" + dir
		if err := rejectSensitiveRepository(path); !errors.Is(err, core.ErrValidation) {
			t.Errorf("expected %s rejected, got %v", dir, err)
		}
	}
	if err := rejectSensitiveRepository(home + "/Projects/my-api"); err != nil {
		t.Errorf("an ordinary project directory must not be rejected: %v", err)
	}
}

func TestCreateWithAdHocSteps(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	task, err := svc.Create(context.Background(), CreateParams{
		Title: "ad hoc", Steps: []string{"developer", "qa"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.Agent != "developer" {
		t.Errorf("expected the first ad hoc step as the agent, got %q", task.Agent)
	}
	steps, ok, err := core.DecodeSteps(task.Metadata)
	if err != nil {
		t.Fatalf("decode steps: %v", err)
	}
	if !ok {
		t.Fatal("expected ad hoc steps to be recorded in metadata")
	}
	if len(steps) != 2 || steps[0] != "developer" || steps[1] != "qa" {
		t.Errorf("unexpected steps: %v", steps)
	}
}

func TestCreateRejectsMultipleStepSources(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateParams{Title: "t", Workflow: "feature", Steps: []string{"developer"}})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	_, err = svc.Create(ctx, CreateParams{Title: "t", Agent: "developer", Steps: []string{"developer"}})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestCreateRejectsUnknownAdHocStep(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Create(context.Background(), CreateParams{
		Title: "t", Steps: []string{"developer", "ghost"},
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCreateRejectsEmptyStepName(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Create(context.Background(), CreateParams{
		Title: "t", Steps: []string{"developer", "  "},
	})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestCreateAdHocStepsDoesNotMutateCallerMetadata(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	original := map[string]string{"source": "ui"}
	task, err := svc.Create(context.Background(), CreateParams{
		Title: "t", Steps: []string{"developer"}, Metadata: original,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, present := original[core.StepsMetadataKey]; present {
		t.Fatal("Create must not mutate the caller's metadata map")
	}
	if task.Metadata["source"] != "ui" {
		t.Error("caller-supplied metadata must survive alongside the encoded steps")
	}
}
