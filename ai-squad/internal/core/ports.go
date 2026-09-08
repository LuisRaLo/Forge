package core

import (
	"context"
	"time"
)

// Clock supplies the current time. Injecting it keeps time-dependent logic
// testable without sleeping.
type Clock func() time.Time

// SystemClock is the production Clock. It returns UTC so timestamps are
// comparable across machines and stable in the database.
func SystemClock() time.Time { return time.Now().UTC() }

// TaskFilter narrows a task listing. Zero-valued fields are ignored.
type TaskFilter struct {
	Statuses   []TaskStatus
	Repository string
	Workflow   string
	Agent      string
	ParentID   *string
	// Limit caps the result set; zero means no cap.
	Limit int
}

// TaskRepository persists tasks. It is the only way the orchestrator reaches
// durable state, which is what makes crash recovery a storage concern rather
// than a scheduler concern.
type TaskRepository interface {
	// Create assigns an ID and inserts the task.
	Create(ctx context.Context, t *Task) (*Task, error)

	// Get returns a task by ID, or an error wrapping ErrNotFound.
	Get(ctx context.Context, id string) (*Task, error)

	// GetByIdempotencyKey returns the task created under key, or an error
	// wrapping ErrNotFound. It is what makes task creation replay-safe.
	GetByIdempotencyKey(ctx context.Context, key string) (*Task, error)

	// List returns tasks matching f, ordered by priority then creation time.
	List(ctx context.Context, f TaskFilter) ([]*Task, error)

	// Save persists mutable fields of an existing task. It must not change
	// status; status changes go through Transition so the state machine can
	// never be bypassed.
	Save(ctx context.Context, t *Task) error

	// Transition atomically moves a task to the target status, applying mut
	// to the task inside the same transaction. It re-reads the task under
	// the transaction, so a concurrent worker cannot win a lost update.
	// It returns a *TransitionError when the edge is not permitted.
	Transition(ctx context.Context, id string, to TaskStatus, reason string, mut func(*Task)) (*Task, error)

	// Events returns the audit trail for a task, oldest first.
	Events(ctx context.Context, id string) ([]TaskEvent, error)
}

// TaskEvent is one recorded state change, forming the task audit log.
type TaskEvent struct {
	ID         int64
	TaskID     string
	FromStatus TaskStatus
	ToStatus   TaskStatus
	Reason     string
	CreatedAt  time.Time
}

// AgentRegistry resolves agent definitions by name.
type AgentRegistry interface {
	Get(name string) (*AgentDefinition, error)
	List() []*AgentDefinition
}

// RuntimeResolver resolves a runtime name to a live AgentRuntime. Concrete
// runtimes are registered during wiring in main; the core only ever sees this
// interface, which is what keeps Claude Code out of the core's import graph.
type RuntimeResolver interface {
	Runtime(name string) (AgentRuntime, error)
	Names() []string
}
