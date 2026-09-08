package core

import (
	"context"
	"fmt"
	"time"
)

// RunStatus is the outcome of a single agent execution.
type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// Valid reports whether s is a known run status.
func (s RunStatus) Valid() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCancelled:
		return true
	}
	return false
}

// AgentRun records one execution of one agent step, including what it cost.
type AgentRun struct {
	ID     int64
	TaskID string

	// StepID identifies one attempt of one workflow step. It embeds the
	// attempt number so that recording is idempotent across retries.
	StepID string

	Agent      string
	Runtime    string
	Status     RunStatus
	SessionID  string
	StopReason string

	// Error is redacted before it reaches this field.
	Error string

	// PermissionDenials records policy refusals the runtime reported (see
	// RunResult.PermissionDenials). Part of the audit trail: an agent
	// hitting a permission wall is a security-relevant event, not something
	// to discard once observed.
	PermissionDenials []string

	Usage      Usage
	Duration   time.Duration
	StartedAt  time.Time
	FinishedAt time.Time
}

// StepID builds the canonical step identifier for a task attempt.
func StepID(workflow string, step int, agent string, attempt int) string {
	if workflow == "" {
		workflow = "direct"
	}
	return fmt.Sprintf("%s/%d/%s/attempt-%d", workflow, step, agent, attempt)
}

// RunRepository persists agent execution records.
type RunRepository interface {
	// Record stores a run. Recording the same task and step twice replaces
	// the earlier row rather than duplicating it, so a retried write is
	// safe.
	Record(ctx context.Context, run *AgentRun) (*AgentRun, error)

	// ListByTask returns a task's runs, oldest first.
	ListByTask(ctx context.Context, taskID string) ([]*AgentRun, error)

	// CostSince totals reported spend since a point in time. Runs whose cost
	// is unknown contribute nothing to the total and are counted separately,
	// so a limit is never enforced against a number that pretends unknown
	// spend was zero.
	CostSince(ctx context.Context, since time.Time) (total float64, unknown int, err error)
}
