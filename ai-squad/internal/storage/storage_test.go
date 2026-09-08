package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/santillana/ai-squad/internal/core"
)

// newTestDB opens a migrated in-memory database.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	db, err := OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open memory database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// newTestRepo returns a repository backed by an in-memory database.
func newTestRepo(t *testing.T) *TaskRepo {
	t.Helper()
	return NewTaskRepo(newTestDB(t), core.SystemClock)
}

func newTask(title string) *core.Task {
	return &core.Task{Title: title, Agent: "developer", Workflow: "feature"}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db := newTestDB(t)
	// Running migrations again must be a no-op, which is what lets `init`
	// and the daemon both call it safely.
	for i := 0; i < 3; i++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("re-running migrations failed on pass %d: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one recorded migration")
	}
}

func TestMigrateDetectsChangedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	original := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER);")}}
	if err := MigrateFS(ctx, db, original); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// Editing an applied migration must be caught, not silently ignored.
	edited := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id TEXT);")}}
	err = MigrateFS(ctx, db, edited)
	if err == nil {
		t.Fatal("expected an error when an applied migration changes")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error must explain the problem")
	}
}

func TestCreateAssignsSequentialIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	for i := 1; i <= 3; i++ {
		got, err := repo.Create(ctx, newTask(fmt.Sprintf("task %d", i)))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		want := fmt.Sprintf("TASK-%d", i)
		if got.ID != want {
			t.Errorf("expected %s, got %s", want, got.ID)
		}
	}
}

func TestCreateAppliesDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	got, err := repo.Create(ctx, newTask("defaults"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Status != core.StatusPending {
		t.Errorf("expected PENDING, got %s", got.Status)
	}
	if got.Priority != core.PriorityNormal {
		t.Errorf("expected normal priority, got %d", got.Priority)
	}
	if got.MaxAttempts != 3 {
		t.Errorf("expected 3 max attempts, got %d", got.MaxAttempts)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps must be set on create")
	}
}

func TestCreateDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	input := newTask("original")
	if _, err := repo.Create(ctx, input); err != nil {
		t.Fatalf("create: %v", err)
	}
	if input.ID != "" {
		t.Errorf("Create must not mutate its argument, got ID %q", input.ID)
	}
}

func TestCreateRejectsInvalidTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	if _, err := repo.Create(ctx, &core.Task{Title: "  "}); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if _, err := repo.Create(ctx, nil); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation for nil, got %v", err)
	}
}

func TestGetUnknownTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	if _, err := repo.Get(ctx, "TASK-999"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRoundTripPreservesEveryField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	parent, err := repo.Create(ctx, newTask("parent"))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	in := &core.Task{
		Title:          "child",
		Description:    "a long description",
		Repository:     "/tmp/repo",
		Branch:         "feature/x",
		Workflow:       "feature",
		Step:           2,
		Agent:          "qa",
		Status:         core.StatusPending,
		Priority:       core.PriorityHigh,
		MaxAttempts:    5,
		ParentTaskID:   &parent.ID,
		WorkspacePath:  "/tmp/wt",
		IdempotencyKey: "key-1",
		Metadata:       map[string]string{"origin": "cli", "ticket": "ABC-1"},
	}
	created, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Description != in.Description || got.Repository != in.Repository ||
		got.Branch != in.Branch || got.Step != in.Step || got.Agent != in.Agent ||
		got.Priority != in.Priority || got.MaxAttempts != in.MaxAttempts ||
		got.WorkspacePath != in.WorkspacePath || got.IdempotencyKey != in.IdempotencyKey {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, in)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != parent.ID {
		t.Errorf("parent lost: %v", got.ParentTaskID)
	}
	if len(got.Metadata) != 2 || got.Metadata["ticket"] != "ABC-1" {
		t.Errorf("metadata lost: %v", got.Metadata)
	}
}

func TestIdempotencyKeyIsUnique(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	first := newTask("first")
	first.IdempotencyKey = "same"
	if _, err := repo.Create(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}

	second := newTask("second")
	second.IdempotencyKey = "same"
	if _, err := repo.Create(ctx, second); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestManyTasksWithoutIdempotencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	// An empty key maps to SQL NULL, so the UNIQUE index must tolerate many
	// rows without one.
	for i := 0; i < 5; i++ {
		if _, err := repo.Create(ctx, newTask("no key")); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
}

func TestGetByIdempotencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	in := newTask("keyed")
	in.IdempotencyKey = "abc"
	created, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByIdempotencyKey(ctx, "abc")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("expected %s, got %s", created.ID, got.ID)
	}

	if _, err := repo.GetByIdempotencyKey(ctx, "nope"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := repo.GetByIdempotencyKey(ctx, ""); !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation for an empty key, got %v", err)
	}
}

func TestTransitionEnforcesStateMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("guarded"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// PENDING -> RUNNING is not an edge: work must be scheduled.
	_, err = repo.Transition(ctx, task.ID, core.StatusRunning, "illegal", nil)
	if !errors.Is(err, core.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}

	// The rejected transition must not have been persisted.
	after, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != core.StatusPending {
		t.Errorf("status changed despite a rejected transition: %s", after.Status)
	}
}

func TestTransitionStampsStartAndCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("timestamps"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := repo.Transition(ctx, task.ID, core.StatusReady, "queued", nil); err != nil {
		t.Fatalf("to ready: %v", err)
	}
	running, err := repo.Transition(ctx, task.ID, core.StatusRunning, "started", nil)
	if err != nil {
		t.Fatalf("to running: %v", err)
	}
	if running.StartedAt == nil {
		t.Fatal("StartedAt must be stamped when work begins")
	}
	if running.CompletedAt != nil {
		t.Fatal("CompletedAt must stay unset while running")
	}

	done, err := repo.Transition(ctx, task.ID, core.StatusCompleted, "done", nil)
	if err != nil {
		t.Fatalf("to completed: %v", err)
	}
	if done.CompletedAt == nil {
		t.Fatal("CompletedAt must be stamped on a terminal state")
	}
	if !done.StartedAt.Equal(*running.StartedAt) {
		t.Error("StartedAt must not be rewritten by later transitions")
	}
}

func TestTransitionAppliesMutator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("mutate"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Transition(ctx, task.ID, core.StatusReady, "queued", func(t *core.Task) {
		t.Attempts = 2
		t.LastError = "previous failure"
		t.WorkspacePath = "/tmp/wt-1"
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got.Attempts != 2 || got.LastError != "previous failure" || got.WorkspacePath != "/tmp/wt-1" {
		t.Errorf("mutator not applied: %+v", got)
	}

	reloaded, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reloaded.Attempts != 2 || reloaded.WorkspacePath != "/tmp/wt-1" {
		t.Errorf("mutation not persisted: %+v", reloaded)
	}
}

func TestTransitionMutatorCannotForgeStatusOrID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("forge"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Transition(ctx, task.ID, core.StatusReady, "queued", func(t *core.Task) {
		t.Status = core.StatusCompleted // must be ignored
		t.ID = "TASK-HACKED"            // must be ignored
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got.Status != core.StatusReady {
		t.Errorf("mutator must not override the target status, got %s", got.Status)
	}
	if got.ID != task.ID {
		t.Errorf("mutator must not change identity, got %s", got.ID)
	}
}

func TestTransitionUnknownTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	if _, err := repo.Transition(ctx, "TASK-404", core.StatusReady, "", nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestEventsFormAnAuditTrail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("audited"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusReady, "queued by scheduler", nil); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusRunning, "claimed by worker-1", nil); err != nil {
		t.Fatalf("transition: %v", err)
	}

	events, err := repo.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected creation plus two transitions, got %d", len(events))
	}
	if events[0].ToStatus != core.StatusPending || events[0].FromStatus != "" {
		t.Errorf("first event should record creation, got %+v", events[0])
	}
	if events[2].FromStatus != core.StatusReady || events[2].ToStatus != core.StatusRunning {
		t.Errorf("unexpected third event: %+v", events[2])
	}
	if events[2].Reason != "claimed by worker-1" {
		t.Errorf("reason lost: %q", events[2].Reason)
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].ID >= events[i].ID {
			t.Error("events must be returned oldest first")
		}
	}
}

func TestRejectedTransitionLeavesNoEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("clean"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = repo.Transition(ctx, task.ID, core.StatusCompleted, "illegal", nil)

	events, err := repo.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("a rolled-back transition must leave no trace, got %d events", len(events))
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	low := newTask("low priority")
	low.Priority = core.PriorityLow
	low.Repository = "/repo/a"
	if _, err := repo.Create(ctx, low); err != nil {
		t.Fatalf("create: %v", err)
	}

	high := newTask("high priority")
	high.Priority = core.PriorityCritical
	high.Repository = "/repo/b"
	created, err := repo.Create(ctx, high)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	all, err := repo.List(ctx, core.TaskFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(all))
	}
	if all[0].ID != created.ID {
		t.Error("higher priority must sort first")
	}

	byRepo, err := repo.List(ctx, core.TaskFilter{Repository: "/repo/a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(byRepo) != 1 || byRepo[0].Repository != "/repo/a" {
		t.Errorf("repository filter failed: %v", byRepo)
	}

	byStatus, err := repo.List(ctx, core.TaskFilter{Statuses: []core.TaskStatus{core.StatusRunning}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(byStatus) != 0 {
		t.Errorf("expected no running tasks, got %d", len(byStatus))
	}

	limited, err := repo.List(ctx, core.TaskFilter{Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit not applied, got %d", len(limited))
	}
}

func TestSaveDoesNotChangeStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("save"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Save must ignore a forged status: the state machine is the only route.
	task.Status = core.StatusCompleted
	task.Title = "renamed"
	if err := repo.Save(ctx, task); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusPending {
		t.Errorf("Save must not write status, got %s", got.Status)
	}
	if got.Title != "renamed" {
		t.Errorf("Save must persist mutable fields, got %q", got.Title)
	}
}

func TestSaveUnknownTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	err := repo.Save(ctx, &core.Task{
		ID: "TASK-404", Title: "ghost", Status: core.StatusPending, MaxAttempts: 1,
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestForeignKeyRejectsUnknownParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	ghost := "TASK-404"
	task := newTask("orphan")
	task.ParentTaskID = &ghost
	if _, err := repo.Create(ctx, task); err == nil {
		t.Fatal("expected the foreign key constraint to reject an unknown parent")
	}
}
