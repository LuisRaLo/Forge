package events

import (
	"context"
	"testing"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/storage"
)

func newTestRepo(t *testing.T) core.TaskRepository {
	t.Helper()
	db, err := storage.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return storage.NewTaskRepo(db, core.SystemClock)
}

func TestCreatePublishesEvent(t *testing.T) {
	t.Parallel()
	var bus Bus
	repo := NewPublishingTaskRepository(newTestRepo(t), &bus)
	ch, unsubscribe := bus.Subscribe(4)
	defer unsubscribe()

	created, err := repo.Create(context.Background(), &core.Task{Title: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != TypeTaskCreated || ev.TaskID != created.ID {
			t.Errorf("unexpected event: %+v", ev)
		}
		if ev.Task == nil {
			t.Error("expected the created task attached to the event")
		}
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}
}

func TestTransitionPublishesFromAndTo(t *testing.T) {
	t.Parallel()
	var bus Bus
	repo := NewPublishingTaskRepository(newTestRepo(t), &bus)

	task, err := repo.Create(context.Background(), &core.Task{Title: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ch, unsubscribe := bus.Subscribe(4)
	defer unsubscribe()

	if _, err := repo.Transition(context.Background(), task.ID, core.StatusReady, "queued", nil); err != nil {
		t.Fatalf("transition: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != TypeTaskTransition || ev.From != "PENDING" || ev.To != "READY" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}
}

func TestRejectedTransitionPublishesNothing(t *testing.T) {
	t.Parallel()
	var bus Bus
	repo := NewPublishingTaskRepository(newTestRepo(t), &bus)

	task, err := repo.Create(context.Background(), &core.Task{Title: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ch, unsubscribe := bus.Subscribe(4)
	defer unsubscribe()

	// PENDING -> COMPLETED is not a legal edge; nothing happened, so nothing
	// should be published.
	if _, err := repo.Transition(context.Background(), task.ID, core.StatusCompleted, "illegal", nil); err == nil {
		t.Fatal("expected the transition to be rejected")
	}

	select {
	case ev := <-ch:
		t.Fatalf("expected no event for a rejected transition, got %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReadsPassThroughUnwrapped(t *testing.T) {
	t.Parallel()
	var bus Bus
	inner := newTestRepo(t)
	repo := NewPublishingTaskRepository(inner, &bus)

	created, err := inner.Create(context.Background(), &core.Task{Title: "direct"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get through wrapper: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("expected the same task, got %s", got.ID)
	}
}
