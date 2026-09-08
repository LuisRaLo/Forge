// Package scheduler executes tasks: it claims queued work, runs the current
// workflow step's agent, advances or rewinds the workflow based on the
// result, and recovers state left behind by a crashed process.
//
// State transitions here follow the state machine in internal/core exactly.
// PENDING never jumps straight to RUNNING (work is always queued first via
// READY, or via PLANNING for a multi-step workflow's first step, which is
// itself an executing state); a task is claimed by winning a
// core.TaskRepository.Transition, whose compare-and-swap semantics were
// proven single-winner under 16-way contention in Phase 1
// (internal/storage/concurrency_test.go).
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/workspace"
)

// gateOutputSchema is the fixed structured-output shape a gated step's
// runtime is asked to conform to. A "passed" boolean is the only field the
// scheduler reads; "summary" exists so the model has somewhere to put its
// reasoning without polluting "passed".
var gateOutputSchema = json.RawMessage(
	`{"type":"object","required":["passed"],"properties":{"passed":{"type":"boolean"},"summary":{"type":"string"}}}`)

// Workflows resolves a workflow name to its ordered agent steps. Defined
// locally (identical in shape to tasks.Workflows) so this package never
// imports internal/config, matching the decoupling internal/tasks already
// established.
type Workflows interface {
	Steps(name string) ([]string, error)
}

// Workspaces is the subset of *workspace.Manager the scheduler depends on,
// so scheduler logic is testable without real git repositories.
type Workspaces interface {
	Acquire(ctx context.Context, task *core.Task) (*workspace.Workspace, error)
	Release(ctx context.Context, ws *workspace.Workspace, opts workspace.ReleaseOptions) error
}

// GitStatus is the subset of git operations the scheduler needs to (a)
// verify that a step which reports success actually changed something, (b)
// push a branch upstream once a gate step (e.g. QA running the test suite)
// has verified it, and (c) record the task's spec file into the workspace's
// history — see advance()'s and execute()'s use of it. Optional: a nil
// Deps.Git skips all of this entirely (e.g. for a runtime/agent shape where
// it wouldn't make sense).
type GitStatus interface {
	IsDirty(ctx context.Context, dir string) (bool, error)
	CommitsSince(ctx context.Context, dir, baseCommit string) (int, error)
	RemoteExists(ctx context.Context, dir, remote string) (bool, error)
	Push(ctx context.Context, dir, remote, branch string) error
	Commit(ctx context.Context, dir, message string) (bool, error)
	HeadCommit(ctx context.Context, dir string) (string, error)
}

// Config bounds scheduler behaviour.
type Config struct {
	MaxConcurrency int
	PollInterval   time.Duration
	// ReapGrace is added on top of a step's own timeout before an Active
	// task with a stale UpdatedAt is presumed abandoned by a dead worker
	// goroutine and reclaimed. It exists for defense in depth within a
	// single long-running daemon process; process-restart recovery (see
	// Start) does not depend on it and is immediate.
	ReapGrace time.Duration

	MaxTaskAttempts   int
	MaxTaskDuration   time.Duration
	MaxStepIterations int
	// MaxDailyCostUSD, when > 0, stops claiming NEW work once reported spend
	// in the trailing 24h reaches it. Already-running tasks are left to
	// finish. Enforcement is best-effort: see core.Usage.CostEstimated.
	MaxDailyCostUSD float64
}

// Deps are the scheduler's dependencies, all ports — no vendor package is
// imported here, matching the dependency rule in docs/architecture.md.
type Deps struct {
	Tasks      core.TaskRepository
	Runs       core.RunRepository
	Artifacts  core.ArtifactRepository
	Agents     core.AgentRegistry
	Runtimes   core.RuntimeResolver
	Workspaces Workspaces
	// Git is optional; see GitStatus's doc comment.
	Git       GitStatus
	Workflows Workflows
	// RuntimeFor resolves the runtime name a task's current step executes
	// on: a per-task override (core.RuntimeMetadataKey — "who resolves my
	// spec," chosen at creation time) wins over the agent's own configured
	// runtime binding. Compatibility between the override and every step's
	// agent is validated once at task-creation time by the caller
	// (internal/web, the CLI), not here — by the time the scheduler asks,
	// the answer is assumed correct.
	RuntimeFor func(t *core.Task) string
	Clock      core.Clock
	Log        *slog.Logger
}

// Scheduler claims and executes tasks under a bounded worker pool.
type Scheduler struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	sem chan struct{}
	wg  sync.WaitGroup
}

// New builds a Scheduler.
func New(cfg Config, deps Deps) (*Scheduler, error) {
	if cfg.MaxConcurrency < 1 {
		return nil, core.Invalid("max_concurrency", "must be at least 1")
	}
	if cfg.PollInterval <= 0 {
		return nil, core.Invalid("poll_interval", "must be greater than zero")
	}
	if cfg.MaxStepIterations < 1 {
		return nil, core.Invalid("max_step_iterations", "must be at least 1")
	}
	for name, v := range map[string]any{
		"tasks": deps.Tasks, "runs": deps.Runs, "artifacts": deps.Artifacts,
		"agents": deps.Agents, "runtimes": deps.Runtimes, "workspaces": deps.Workspaces,
		"workflows": deps.Workflows, "runtime_for": deps.RuntimeFor,
	} {
		if v == nil {
			return nil, core.Invalidf("deps."+name, "%s must not be nil", name)
		}
	}
	if deps.Clock == nil {
		deps.Clock = core.SystemClock
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &Scheduler{cfg: cfg, deps: deps, log: deps.Log, sem: make(chan struct{}, cfg.MaxConcurrency)}, nil
}

// Run recovers interrupted work, then polls and dispatches until ctx is
// cancelled, waiting for in-flight workers to finish before returning.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.recoverInterrupted(ctx); err != nil {
		s.log.Error("recovery sweep failed", "error", err)
	}

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// RunOnce drains the currently claimable queue and returns, rather than
// looping forever. It is what backs `ai-squad worker start`, for a single
// supervised pass (e.g. from cron), as distinct from `ai-squad daemon`,
// which never returns until stopped.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if err := s.recoverInterrupted(ctx); err != nil {
		return fmt.Errorf("recovery sweep: %w", err)
	}
	// A safety valve, not an expected outcome: legitimate progress always
	// terminates via attempt limits reaching FAILED/BLOCKED/COMPLETED. This
	// guards a single supervised pass against hanging forever if it ever
	// doesn't.
	const maxPasses = 10_000
	for range maxPasses {
		if !s.hasClaimableWork(ctx) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.tick(ctx)
		s.wg.Wait()
	}
	return fmt.Errorf("worker: exceeded %d passes without draining the queue; a task may be stuck", maxPasses)
}

// hasClaimableWork reports whether any PENDING/READY/REVIEW task remains.
func (s *Scheduler) hasClaimableWork(ctx context.Context) bool {
	tasks, err := s.deps.Tasks.List(ctx, core.TaskFilter{
		Statuses: []core.TaskStatus{core.StatusPending, core.StatusReady, core.StatusReview},
		Limit:    1,
	})
	return err == nil && len(tasks) > 0
}

// tick reaps stale leases, then claims and dispatches as many tasks as there
// are free worker slots.
func (s *Scheduler) tick(ctx context.Context) {
	if err := s.reapStale(ctx); err != nil {
		s.log.Error("reaper pass failed", "error", err)
	}

	if s.dailyCostExceeded(ctx) {
		s.log.Warn("daily cost limit reached; not claiming new work")
		return
	}

	for {
		select {
		case s.sem <- struct{}{}:
		default:
			return // pool is full
		}

		task, ok, err := s.claimNext(ctx)
		if err != nil {
			s.log.Error("claim failed", "error", err)
			<-s.sem
			return
		}
		if !ok {
			<-s.sem
			return // nothing runnable
		}

		s.wg.Go(func() {
			defer func() { <-s.sem }()
			defer func() {
				// A panicking step must not take a worker slot down with it
				// or leave the task stuck claimed forever.
				if r := recover(); r != nil {
					s.log.Error("worker panic", "task", task.ID, "panic", r)
					s.failTask(context.Background(), task, fmt.Errorf("worker panic: %v", r))
				}
			}()
			s.execute(ctx, task)
		})
	}
}

func (s *Scheduler) dailyCostExceeded(ctx context.Context) bool {
	if s.cfg.MaxDailyCostUSD <= 0 {
		return false
	}
	total, _, err := s.deps.Runs.CostSince(ctx, s.deps.Clock().Add(-24*time.Hour))
	if err != nil {
		s.log.Error("cost check failed", "error", err)
		return false // fail open: a broken cost query must not halt the squad
	}
	return total >= s.cfg.MaxDailyCostUSD
}
