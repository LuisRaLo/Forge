package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/santillana/ai-squad/internal/core"
)

// claimNext finds the highest-priority runnable task and claims it,
// returning ok=false when there is nothing to run right now.
func (s *Scheduler) claimNext(ctx context.Context) (*core.Task, bool, error) {
	candidates, err := s.deps.Tasks.List(ctx, core.TaskFilter{
		Statuses: []core.TaskStatus{core.StatusPending, core.StatusReady},
		Limit:    16, // more than one worker slot's worth, so losing a race still leaves options this tick
	})
	if err != nil {
		return nil, false, fmt.Errorf("list runnable tasks: %w", err)
	}

	for _, t := range candidates {
		claimed, err := s.claim(ctx, t)
		if err != nil {
			if errors.Is(err, core.ErrInvalidTransition) || errors.Is(err, core.ErrNotFound) {
				continue // another worker won the race for this one, or it vanished; try the next candidate
			}
			return nil, false, err
		}
		return claimed, true, nil
	}
	return nil, false, nil
}

// claim moves a task from PENDING/READY into an executing state
// (PLANNING for a multi-step workflow's first step, RUNNING otherwise). A
// fresh PENDING single-step task passes through READY first, since the
// state machine forbids PENDING -> RUNNING directly (work is always
// scheduled, never started straight from creation).
func (s *Scheduler) claim(ctx context.Context, t *core.Task) (*core.Task, error) {
	if t.Status == core.StatusPending {
		multiStep, err := s.isFirstOfMultiStepWorkflow(t)
		if err != nil {
			return nil, err
		}
		if multiStep {
			return s.deps.Tasks.Transition(ctx, t.ID, core.StatusPlanning, "claimed: planning first workflow step", nil)
		}
		queued, err := s.deps.Tasks.Transition(ctx, t.ID, core.StatusReady, "queued", nil)
		if err != nil {
			return nil, err
		}
		t = queued
	}
	return s.deps.Tasks.Transition(ctx, t.ID, core.StatusRunning, "claimed by worker", nil)
}

// isFirstOfMultiStepWorkflow reports whether t is sitting at step 0 of a
// workflow with more than one step, which is the only case that claims into
// PLANNING rather than RUNNING.
func (s *Scheduler) isFirstOfMultiStepWorkflow(t *core.Task) (bool, error) {
	if t.Workflow == "" || t.Step != 0 {
		return false, nil
	}
	steps, err := s.deps.Workflows.Steps(t.Workflow)
	if err != nil {
		return false, fmt.Errorf("resolve workflow %s for task %s: %w", t.Workflow, t.ID, err)
	}
	return len(steps) > 1, nil
}
