package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TaskStatus is a node in the task state machine.
type TaskStatus string

const (
	StatusPending         TaskStatus = "PENDING"
	StatusPlanning        TaskStatus = "PLANNING"
	StatusReady           TaskStatus = "READY"
	StatusRunning         TaskStatus = "RUNNING"
	StatusWaiting         TaskStatus = "WAITING"
	StatusReview          TaskStatus = "REVIEW"
	StatusWaitingApproval TaskStatus = "WAITING_APPROVAL"
	StatusFailed          TaskStatus = "FAILED"
	StatusBlocked         TaskStatus = "BLOCKED"
	StatusCompleted       TaskStatus = "COMPLETED"
	StatusCancelled       TaskStatus = "CANCELLED"
)

// AllStatuses lists every valid status in rough lifecycle order.
func AllStatuses() []TaskStatus {
	return []TaskStatus{
		StatusPending, StatusPlanning, StatusReady, StatusRunning,
		StatusWaiting, StatusReview, StatusWaitingApproval,
		StatusFailed, StatusBlocked, StatusCompleted, StatusCancelled,
	}
}

// ParseStatus validates and normalises a status string.
func ParseStatus(s string) (TaskStatus, error) {
	candidate := TaskStatus(strings.ToUpper(strings.TrimSpace(s)))
	if !candidate.Valid() {
		return "", Invalid("status", "unknown status "+s)
	}
	return candidate, nil
}

// Valid reports whether s is a known status.
func (s TaskStatus) Valid() bool {
	for _, known := range AllStatuses() {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition is possible.
func (s TaskStatus) Terminal() bool {
	return s == StatusCompleted || s == StatusCancelled
}

// Active reports whether a worker currently owns the task.
func (s TaskStatus) Active() bool {
	return s == StatusPlanning || s == StatusRunning
}

func (s TaskStatus) String() string { return string(s) }

// Priority orders scheduling. Higher values are scheduled first.
type Priority int

const (
	PriorityLow      Priority = 10
	PriorityNormal   Priority = 50
	PriorityHigh     Priority = 80
	PriorityCritical Priority = 100
)

// ParsePriority accepts a name ("high") or a numeric string ("80").
func ParsePriority(s string) (Priority, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return PriorityLow, nil
	case "", "normal", "medium":
		return PriorityNormal, nil
	case "high":
		return PriorityHigh, nil
	case "critical", "urgent":
		return PriorityCritical, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, Invalid("priority", "expected low|normal|high|critical or an integer, got "+s)
	}
	if n < 0 || n > 1000 {
		return 0, Invalid("priority", "must be between 0 and 1000")
	}
	return Priority(n), nil
}

// Task is the unit of work the orchestrator schedules and persists.
type Task struct {
	ID          string
	Title       string
	Description string

	// Repository is the absolute path of the git repository to work in.
	Repository string
	// Branch is the target branch for the work.
	Branch string

	// Workflow names the step sequence to execute (e.g. "feature").
	Workflow string
	// Step is the index of the current workflow step.
	Step int
	// Agent is the agent responsible for the current step.
	Agent string

	Status   TaskStatus
	Priority Priority

	// Attempts counts executions of the current step; MaxAttempts caps them
	// so a failing QA loop terminates in BLOCKED instead of spinning.
	Attempts    int
	MaxAttempts int

	// ParentTaskID links subtasks to their parent. Nil for root tasks.
	ParentTaskID *string

	// WorkspacePath is the git worktree assigned to this task, if any.
	WorkspacePath string

	// LastError holds the most recent failure message. It must be redacted
	// before it is written here.
	LastError string

	// IdempotencyKey, when set, is unique across all tasks. It lets a
	// caller retry a create without risking a duplicate task.
	IdempotencyKey string

	// Metadata is free-form, string-valued so it can never smuggle
	// unserialisable state into the database.
	Metadata map[string]string

	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// Validate checks the invariants required to persist a task.
func (t *Task) Validate() error {
	if strings.TrimSpace(t.Title) == "" {
		return Invalid("title", "must not be empty")
	}
	if len(t.Title) > 500 {
		return Invalid("title", "must be at most 500 characters")
	}
	if !t.Status.Valid() {
		return Invalid("status", "unknown status "+string(t.Status))
	}
	if t.MaxAttempts < 1 {
		return Invalid("max_attempts", "must be at least 1")
	}
	if t.Attempts < 0 {
		return Invalid("attempts", "must not be negative")
	}
	if t.Priority < 0 {
		return Invalid("priority", "must not be negative")
	}
	if t.ParentTaskID != nil && *t.ParentTaskID == t.ID {
		return Invalid("parent_task", "task cannot be its own parent")
	}
	for k := range t.Metadata {
		if strings.TrimSpace(k) == "" {
			return Invalid("metadata", "keys must not be empty")
		}
	}
	return nil
}

// AttemptsExhausted reports whether the current step may not be retried again.
func (t *Task) AttemptsExhausted() bool {
	return t.Attempts >= t.MaxAttempts
}

// StepsMetadataKey is the Task.Metadata key holding an ad hoc, per-task
// ordered step (agent name) list, as an alternative to a named workflow
// declared in configuration. It lets a caller (the web UI's step
// checkboxes, for instance) compose a one-off pipeline — "developer" alone,
// or "developer,qa", or "developer,qa,devops" — without requiring an
// operator to pre-declare every combination as a named workflow.
const StepsMetadataKey = "steps"

// EncodeSteps JSON-encodes an ad hoc step list for storage in
// Task.Metadata[StepsMetadataKey].
func EncodeSteps(steps []string) (string, error) {
	b, err := json.Marshal(steps)
	if err != nil {
		return "", fmt.Errorf("encode steps: %w", err)
	}
	return string(b), nil
}

// DecodeSteps reads an ad hoc step list from a task's metadata. ok is false
// when the task carries no ad hoc steps (it uses a named workflow or a
// single direct agent instead), which is not an error.
func DecodeSteps(metadata map[string]string) (steps []string, ok bool, err error) {
	raw, present := metadata[StepsMetadataKey]
	if !present || raw == "" {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil, false, fmt.Errorf("decode steps: %w", err)
	}
	return steps, true, nil
}
