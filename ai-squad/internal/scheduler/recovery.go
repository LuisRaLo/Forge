package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/santillana/ai-squad/internal/core"
)

// recoverInterrupted reclaims every Active (PLANNING/RUNNING) task on
// startup. A freshly started process has zero live worker goroutines by
// definition, so any task still Active in the database was left behind by a
// prior process instance that died — via crash, kill, or otherwise — and is
// safe to reclaim immediately rather than waiting for a lease to expire.
// This is what makes `ai-squad daemon` restart -> recover -> continue.
func (s *Scheduler) recoverInterrupted(ctx context.Context) error {
	active, err := s.deps.Tasks.List(ctx, core.TaskFilter{
		Statuses: []core.TaskStatus{core.StatusPlanning, core.StatusRunning},
	})
	if err != nil {
		return fmt.Errorf("list active tasks: %w", err)
	}
	for _, t := range active {
		s.reclaim(ctx, t, "recovered after process restart")
	}
	return nil
}

// reapStale reclaims Active tasks whose worker appears to have died without
// the process itself crashing (a hung or panicked goroutine that recover()
// in the tick loop did not catch, or one that lost its own ctx wiring). It is
// defense in depth beyond recoverInterrupted, not the primary recovery path.
//
// A task is presumed abandoned once it has sat Active for longer than its
// own execution timeout plus ReapGrace: reusing the step's own timeout means
// a legitimately long-running step is never reaped early, and ReapGrace
// exists so a slow-but-alive worker finishing right at its deadline is not
// racing the reaper.
func (s *Scheduler) reapStale(ctx context.Context) error {
	if s.cfg.ReapGrace <= 0 {
		return nil
	}
	active, err := s.deps.Tasks.List(ctx, core.TaskFilter{
		Statuses: []core.TaskStatus{core.StatusPlanning, core.StatusRunning},
	})
	if err != nil {
		return fmt.Errorf("list active tasks: %w", err)
	}

	now := s.deps.Clock()
	for _, t := range active {
		timeout := s.effectiveTimeout(t)
		if timeout <= 0 {
			continue // no bound configured anywhere: nothing to reap against
		}
		deadline := t.UpdatedAt.Add(timeout).Add(s.cfg.ReapGrace)
		if now.Before(deadline) {
			continue
		}
		s.reclaim(ctx, t, fmt.Sprintf("reaped: no progress since %s (timeout %s + grace %s exceeded)",
			t.UpdatedAt.Format("15:04:05"), timeout, s.cfg.ReapGrace))
	}
	return nil
}

// reclaim requeues an abandoned task, or blocks it if its attempt budget is
// exhausted. Failures reclaiming one task are logged, not returned, so one
// bad row cannot stop the sweep from reaching the rest.
func (s *Scheduler) reclaim(ctx context.Context, t *core.Task, reason string) {
	mut := func(task *core.Task) {
		task.Attempts++
		task.LastError = reason
	}

	var err error
	target := core.StatusReady
	if t.AttemptsExhausted() {
		target = core.StatusBlocked
		// BLOCKED is a direct edge from both RUNNING and PLANNING; no hop
		// needed.
		_, err = s.deps.Tasks.Transition(ctx, t.ID, target, reason, mut)
	} else {
		// requeue takes care of RUNNING's missing direct edge to READY.
		_, err = s.requeue(ctx, t, reason, mut)
	}
	if err != nil {
		s.log.Error("failed to reclaim task", "task", t.ID, "error", err)
		return
	}
	s.log.Warn("reclaimed task", "task", t.ID, "target", target, "reason", reason)
}

// effectiveTimeout is the bound applied to one step's execution: the agent's
// own Limits.Timeout if set, capped by the global MaxTaskDuration, or just
// the global bound if the agent declares none.
func (s *Scheduler) effectiveTimeout(t *core.Task) time.Duration {
	var agentTimeout time.Duration
	if def, err := s.deps.Agents.Get(t.Agent); err == nil {
		agentTimeout = def.Limits.Timeout
	}
	switch {
	case agentTimeout > 0 && s.cfg.MaxTaskDuration > 0:
		if agentTimeout < s.cfg.MaxTaskDuration {
			return agentTimeout
		}
		return s.cfg.MaxTaskDuration
	case agentTimeout > 0:
		return agentTimeout
	default:
		return s.cfg.MaxTaskDuration
	}
}
