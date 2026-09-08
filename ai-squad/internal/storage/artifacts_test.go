package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/santillana/ai-squad/internal/core"
)

func TestArtifactSaveAndListByTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	artifacts := NewArtifactRepo(db, core.SystemClock)

	task := seedTask(t, tasks)

	saved, err := artifacts.Save(ctx, &core.Artifact{
		TaskID:  task.ID,
		StepID:  core.StepID("feature", 0, "architect", 1),
		Name:    "plan",
		Agent:   "architect",
		Content: json.RawMessage(`{"steps":["do x"]}`),
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.ID == 0 {
		t.Fatal("expected an assigned id")
	}

	list, err := artifacts.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "plan" {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestArtifactSaveIsIdempotentPerStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	artifacts := NewArtifactRepo(db, core.SystemClock)

	task := seedTask(t, tasks)
	stepID := core.StepID("bugfix", 1, "qa", 1)

	if _, err := artifacts.Save(ctx, &core.Artifact{
		TaskID: task.ID, StepID: stepID, Name: "qa-report", Agent: "qa",
		Content: json.RawMessage(`{"passed":false}`),
	}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := artifacts.Save(ctx, &core.Artifact{
		TaskID: task.ID, StepID: stepID, Name: "qa-report", Agent: "qa",
		Content: json.RawMessage(`{"passed":true}`),
	}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	list, err := artifacts.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected the row replaced, got %d rows", len(list))
	}
	if string(list[0].Content) != `{"passed":true}` {
		t.Errorf("expected the replacement's content, got %s", list[0].Content)
	}
}

func TestArtifactLatest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	artifacts := NewArtifactRepo(db, core.SystemClock)

	task := seedTask(t, tasks)

	if _, err := artifacts.Save(ctx, &core.Artifact{
		TaskID: task.ID, StepID: core.StepID("bugfix", 1, "qa", 1),
		Name: "qa-report", Agent: "qa", Content: json.RawMessage(`{"passed":false}`),
	}); err != nil {
		t.Fatalf("save attempt 1: %v", err)
	}
	if _, err := artifacts.Save(ctx, &core.Artifact{
		TaskID: task.ID, StepID: core.StepID("bugfix", 1, "qa", 2),
		Name: "qa-report", Agent: "qa", Content: json.RawMessage(`{"passed":true}`),
	}); err != nil {
		t.Fatalf("save attempt 2: %v", err)
	}

	got, err := artifacts.Latest(ctx, task.ID, "qa-report")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if string(got.Content) != `{"passed":true}` {
		t.Errorf("expected the most recent attempt's content, got %s", got.Content)
	}

	if _, err := artifacts.Latest(ctx, task.ID, "ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestArtifactSaveRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	artifacts := NewArtifactRepo(newTestDB(t), core.SystemClock)

	_, err := artifacts.Save(ctx, &core.Artifact{
		TaskID: "TASK-1", StepID: "s", Name: "n", Content: json.RawMessage(`not json`),
	})
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestArtifactCascadesWithTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTestDB(t)
	tasks := NewTaskRepo(db, core.SystemClock)
	artifacts := NewArtifactRepo(db, core.SystemClock)

	task := seedTask(t, tasks)
	if _, err := artifacts.Save(ctx, &core.Artifact{
		TaskID: task.ID, StepID: "s1", Name: "plan", Content: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	list, err := artifacts.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected artifacts to cascade-delete with the task, got %d", len(list))
	}
}
