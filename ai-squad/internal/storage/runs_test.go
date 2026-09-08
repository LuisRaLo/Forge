package storage

import (
	"context"
	"testing"
	"time"

	"github.com/santillana/ai-squad/internal/core"
)

func seedTask(t *testing.T, repo *TaskRepo) *core.Task {
	t.Helper()
	task, err := repo.Create(context.Background(), &core.Task{Title: "runnable"})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task
}

func cost(v float64) *float64 { return &v }

func TestRecordAndListRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	runs := NewRunRepo(db)

	task := seedTask(t, tasks)

	in := &core.AgentRun{
		TaskID:     task.ID,
		StepID:     core.StepID("feature", 0, "architect", 1),
		Agent:      "architect",
		Runtime:    "claude",
		Status:     core.RunSucceeded,
		SessionID:  "sess-1",
		StopReason: "end_turn",
		Usage: core.Usage{
			Model:        "claude-sonnet-5",
			InputTokens:  120,
			OutputTokens: 340,
			CostUSD:      cost(0.0421),
		},
		Duration:   2500 * time.Millisecond,
		StartedAt:  time.Now().UTC().Add(-3 * time.Second),
		FinishedAt: time.Now().UTC(),
	}
	saved, err := runs.Record(ctx, in)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if saved.ID == 0 {
		t.Fatal("expected an assigned id")
	}

	list, err := runs.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 run, got %d", len(list))
	}
	got := list[0]
	if got.Agent != "architect" || got.Runtime != "claude" || got.Status != core.RunSucceeded {
		t.Errorf("unexpected run: %+v", got)
	}
	if got.Usage.CostUSD == nil || *got.Usage.CostUSD != 0.0421 {
		t.Errorf("cost not preserved: %v", got.Usage.CostUSD)
	}
	if got.Usage.InputTokens != 120 || got.Usage.OutputTokens != 340 {
		t.Errorf("token usage not preserved: %+v", got.Usage)
	}
}

func TestRecordIsIdempotentPerStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	runs := NewRunRepo(db)

	task := seedTask(t, tasks)
	stepID := core.StepID("feature", 0, "developer", 1)

	first := &core.AgentRun{
		TaskID: task.ID, StepID: stepID, Agent: "developer", Runtime: "claude",
		Status: core.RunFailed, Error: "timeout",
	}
	if _, err := runs.Record(ctx, first); err != nil {
		t.Fatalf("first record: %v", err)
	}

	// A retry of the SAME attempt (same step id) must replace, not
	// duplicate — otherwise cost would double-count on a retried write.
	second := &core.AgentRun{
		TaskID: task.ID, StepID: stepID, Agent: "developer", Runtime: "claude",
		Status: core.RunSucceeded, Usage: core.Usage{CostUSD: cost(0.10)},
	}
	if _, err := runs.Record(ctx, second); err != nil {
		t.Fatalf("second record: %v", err)
	}

	list, err := runs.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected the row to be replaced, got %d rows", len(list))
	}
	if list[0].Status != core.RunSucceeded {
		t.Errorf("expected the replacement's status, got %s", list[0].Status)
	}
}

func TestRecordDistinctAttemptsAreDistinctRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	runs := NewRunRepo(db)

	task := seedTask(t, tasks)

	for attempt := 1; attempt <= 3; attempt++ {
		run := &core.AgentRun{
			TaskID: task.ID,
			StepID: core.StepID("feature", 0, "developer", attempt),
			Agent:  "developer", Runtime: "claude", Status: core.RunFailed,
		}
		if _, err := runs.Record(ctx, run); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}

	list, err := runs.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 distinct attempts, got %d", len(list))
	}
}

func TestCostSinceDistinguishesUnknownFromZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	runs := NewRunRepo(db)

	task := seedTask(t, tasks)

	known := &core.AgentRun{
		TaskID: task.ID, StepID: "s1", Agent: "a", Runtime: "claude",
		Status: core.RunSucceeded, Usage: core.Usage{CostUSD: cost(1.50)},
	}
	unknown := &core.AgentRun{
		TaskID: task.ID, StepID: "s2", Agent: "a", Runtime: "ollama",
		Status: core.RunSucceeded, // no CostUSD: a local model reports none
	}
	if _, err := runs.Record(ctx, known); err != nil {
		t.Fatalf("record known: %v", err)
	}
	if _, err := runs.Record(ctx, unknown); err != nil {
		t.Fatalf("record unknown: %v", err)
	}

	total, unknownCount, err := runs.CostSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("cost since: %v", err)
	}
	if total != 1.50 {
		t.Errorf("expected total 1.50, got %v", total)
	}
	if unknownCount != 1 {
		t.Errorf("expected 1 run with unknown cost, got %d", unknownCount)
	}

	// A window that excludes both runs must report zero, not carry them in.
	total2, _, err := runs.CostSince(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("cost since (future): %v", err)
	}
	if total2 != 0 {
		t.Errorf("expected 0 for a future window, got %v", total2)
	}
}

func TestRecordRejectsInvalidRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := NewRunRepo(newTestDB(t))

	tests := []*core.AgentRun{
		nil,
		{TaskID: "", StepID: "s", Status: core.RunSucceeded},
		{TaskID: "TASK-1", StepID: "", Status: core.RunSucceeded},
		{TaskID: "TASK-1", StepID: "s", Status: "bogus"},
	}
	for _, r := range tests {
		if _, err := runs.Record(ctx, r); err == nil {
			t.Errorf("expected an error for %+v", r)
		}
	}
}

func TestRunCascadesWithTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	runs := NewRunRepo(db)

	task := seedTask(t, tasks)
	if _, err := runs.Record(ctx, &core.AgentRun{
		TaskID: task.ID, StepID: "s1", Agent: "a", Runtime: "claude", Status: core.RunSucceeded,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	list, err := runs.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected runs to cascade-delete with the task, got %d", len(list))
	}
}
