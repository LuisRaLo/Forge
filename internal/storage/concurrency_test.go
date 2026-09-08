package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// TestConcurrentCreateAssignsUniqueIDs proves the identifier counter is safe
// under parallel creation: no duplicates, no gaps that lose a task.
func TestConcurrentCreateAssignsUniqueIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	const workers = 32

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = make(map[string]struct{}, workers)
		errs []error
	)

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			task, err := repo.Create(ctx, newTask("concurrent"))

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[task.ID] = struct{}{}
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d creations failed, first: %v", len(errs), errs[0])
	}
	if len(ids) != workers {
		t.Fatalf("expected %d distinct ids, got %d", workers, len(ids))
	}

	stored, err := repo.List(ctx, core.TaskFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != workers {
		t.Errorf("expected %d persisted tasks, got %d", workers, len(stored))
	}
}

// TestConcurrentTransitionHasExactlyOneWinner is the core scheduling safety
// property: when several workers race to claim the same task, exactly one may
// succeed. Without it, two agents could work the same workspace at once.
func TestConcurrentTransitionHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	task, err := repo.Create(ctx, newTask("contended"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusReady, "queued", nil); err != nil {
		t.Fatalf("to ready: %v", err)
	}

	const workers = 16

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		rejected  int
		other     []error
	)

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, err := repo.Transition(ctx, task.ID, core.StatusRunning, "claimed", nil)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, core.ErrInvalidTransition):
				rejected++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected error: %v", other[0])
	}
	if succeeded != 1 {
		t.Fatalf("exactly one worker must claim the task, got %d winners", succeeded)
	}
	if rejected != workers-1 {
		t.Errorf("expected %d losers, got %d", workers-1, rejected)
	}

	got, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusRunning {
		t.Errorf("expected RUNNING, got %s", got.Status)
	}

	// Exactly one claim must be recorded in the audit trail.
	events, err := repo.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	claims := 0
	for _, ev := range events {
		if ev.ToStatus == core.StatusRunning {
			claims++
		}
	}
	if claims != 1 {
		t.Errorf("expected 1 recorded claim, got %d", claims)
	}
}

// TestConcurrentReadsAndWrites exercises mixed traffic for the race detector.
func TestConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	seed, err := repo.Create(ctx, newTask("seed"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := repo.Create(ctx, newTask("writer")); err != nil {
				t.Errorf("create: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := repo.List(ctx, core.TaskFilter{Limit: 5}); err != nil {
				t.Errorf("list: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := repo.Events(ctx, seed.ID); err != nil {
				t.Errorf("events: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestStateSurvivesProcessRestart simulates a crash: the database is closed
// without ceremony and reopened, and the scheduler's view must be intact.
func TestStateSurvivesProcessRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ai-squad.db")

	// First "process": create a task and leave it mid-flight.
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewTaskRepo(db, core.SystemClock)

	task, err := repo.Create(ctx, newTask("survivor"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Transition(ctx, task.ID, core.StatusReady, "queued", nil); err != nil {
		t.Fatalf("to ready: %v", err)
	}
	running, err := repo.Transition(ctx, task.ID, core.StatusRunning, "claimed", func(t *core.Task) {
		t.Attempts = 1
		t.WorkspacePath = "/tmp/worktrees/task-1"
	})
	if err != nil {
		t.Fatalf("to running: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Second "process": everything must still be there.
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	// Migrations run again on start and must be a no-op.
	if err := Migrate(ctx, db2); err != nil {
		t.Fatalf("re-migrate after restart: %v", err)
	}
	repo2 := NewTaskRepo(db2, core.SystemClock)

	got, err := repo2.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if got.Status != core.StatusRunning {
		t.Errorf("expected RUNNING after restart, got %s", got.Status)
	}
	if got.Attempts != 1 || got.WorkspacePath != "/tmp/worktrees/task-1" {
		t.Errorf("task state lost across restart: %+v", got)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(*running.StartedAt) {
		t.Error("StartedAt did not survive the restart")
	}

	events, err := repo2.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events after restart: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("audit trail lost: expected 3 events, got %d", len(events))
	}

	// A recovered in-flight task must still be steerable.
	if _, err := repo2.Transition(ctx, task.ID, core.StatusFailed, "recovered after restart", nil); err != nil {
		t.Fatalf("transition after restart: %v", err)
	}

	// And the identifier sequence must continue rather than restart at 1.
	next, err := repo2.Create(ctx, newTask("after restart"))
	if err != nil {
		t.Fatalf("create after restart: %v", err)
	}
	if next.ID == task.ID {
		t.Fatalf("identifier counter restarted, reissued %s", next.ID)
	}
}

// TestInterruptedWorkIsDiscoverable proves the scheduler can find tasks that a
// crashed worker left behind, which is the basis of Phase 4 recovery.
func TestInterruptedWorkIsDiscoverable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTestRepo(t)

	for _, status := range []core.TaskStatus{core.StatusRunning, core.StatusPlanning} {
		task, err := repo.Create(ctx, newTask("interrupted"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if status == core.StatusRunning {
			if _, err := repo.Transition(ctx, task.ID, core.StatusReady, "", nil); err != nil {
				t.Fatalf("to ready: %v", err)
			}
		}
		if _, err := repo.Transition(ctx, task.ID, status, "claimed", nil); err != nil {
			t.Fatalf("to %s: %v", status, err)
		}
	}

	stuck, err := repo.List(ctx, core.TaskFilter{
		Statuses: []core.TaskStatus{core.StatusRunning, core.StatusPlanning},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stuck) != 2 {
		t.Fatalf("expected 2 interrupted tasks, got %d", len(stuck))
	}
	for _, task := range stuck {
		if !task.Status.Active() {
			t.Errorf("%s should be an active state", task.Status)
		}
	}
}
