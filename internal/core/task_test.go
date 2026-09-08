package core

import (
	"errors"
	"strings"
	"testing"
)

func validTask() *Task {
	return &Task{
		ID:          "TASK-1",
		Title:       "Add Google authentication",
		Status:      StatusPending,
		Priority:    PriorityNormal,
		MaxAttempts: 3,
	}
}

func TestTaskValidate(t *testing.T) {
	t.Parallel()

	if err := validTask().Validate(); err != nil {
		t.Fatalf("valid task rejected: %v", err)
	}

	tests := map[string]func(*Task){
		"empty title":        func(t *Task) { t.Title = "   " },
		"overlong title":     func(t *Task) { t.Title = strings.Repeat("x", 501) },
		"unknown status":     func(t *Task) { t.Status = "NOPE" },
		"zero max attempts":  func(t *Task) { t.MaxAttempts = 0 },
		"negative attempts":  func(t *Task) { t.Attempts = -1 },
		"negative priority":  func(t *Task) { t.Priority = -1 },
		"empty metadata key": func(t *Task) { t.Metadata = map[string]string{"": "v"} },
		"self parent": func(t *Task) {
			id := t.ID
			t.ParentTaskID = &id
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			task := validTask()
			mutate(task)
			if err := task.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestTaskAttemptsExhausted(t *testing.T) {
	t.Parallel()

	task := validTask()
	task.Attempts, task.MaxAttempts = 2, 3
	if task.AttemptsExhausted() {
		t.Error("2 of 3 attempts is not exhausted")
	}
	task.Attempts = 3
	if !task.AttemptsExhausted() {
		t.Error("3 of 3 attempts is exhausted")
	}
}

func TestParsePriority(t *testing.T) {
	t.Parallel()

	tests := map[string]Priority{
		"low":      PriorityLow,
		"normal":   PriorityNormal,
		"":         PriorityNormal,
		"HIGH":     PriorityHigh,
		"critical": PriorityCritical,
		"42":       Priority(42),
	}
	for input, want := range tests {
		got, err := ParsePriority(input)
		if err != nil {
			t.Errorf("ParsePriority(%q) errored: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePriority(%q) = %d, want %d", input, got, want)
		}
	}

	for _, bad := range []string{"urgentish", "-5", "99999"} {
		if _, err := ParsePriority(bad); !errors.Is(err, ErrValidation) {
			t.Errorf("ParsePriority(%q) should fail validation, got %v", bad, err)
		}
	}
}

func TestValidationErrorWrapping(t *testing.T) {
	t.Parallel()

	err := Invalid("field", "reason")
	if !errors.Is(err, ErrValidation) {
		t.Fatal("Invalid must wrap ErrValidation")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "field" {
		t.Fatalf("expected a *ValidationError carrying the field, got %v", err)
	}
	if got := Invalidf("f", "expected %d", 3).Error(); got != "f: expected 3" {
		t.Errorf("Invalidf formatting wrong: %q", got)
	}
}
