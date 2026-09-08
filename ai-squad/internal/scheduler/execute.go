package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/workspace"
)

// execute runs one task's current step to completion: resolve the agent and
// runtime, acquire a workspace, run, record the outcome, and decide the
// task's next state. Every exit path leaves the task in a legal state — it
// is never left claimed with nowhere to go.
func (s *Scheduler) execute(ctx context.Context, t *core.Task) {
	def, err := s.deps.Agents.Get(t.Agent)
	if err != nil {
		s.failTask(ctx, t, fmt.Errorf("resolve agent %s: %w", t.Agent, err))
		return
	}

	runtimeName := s.deps.RuntimeFor(t.Agent)
	rt, err := s.deps.Runtimes.Runtime(runtimeName)
	if err != nil {
		s.failTask(ctx, t, fmt.Errorf("resolve runtime %s for agent %s: %w", runtimeName, t.Agent, err))
		return
	}

	ws, err := s.deps.Workspaces.Acquire(ctx, t)
	if err != nil {
		s.failTask(ctx, t, fmt.Errorf("acquire workspace: %w", err))
		return
	}

	stepID := core.StepID(t.Workflow, t.Step, t.Agent, t.Attempts+1)

	runCtx := ctx
	if timeout := s.effectiveTimeout(t); timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req := core.RunRequest{
		TaskID:       t.ID,
		StepID:       stepID,
		Agent:        def,
		SystemPrompt: def.SystemPrompt,
		Prompt:       s.buildPrompt(ctx, t, def),
		WorkspaceDir: ws.Path,
		Permissions:  def.Permissions,
		Limits:       def.Limits,
	}
	if def.Gate.Enabled {
		req.OutputSchema = gateOutputSchema
	}

	start := time.Now()
	result, runErr := rt.Execute(runCtx, req, s.eventSink(t))
	duration := time.Since(start)

	s.recordRun(ctx, t, def, runtimeName, stepID, result, runErr, duration)

	if runErr != nil {
		s.handleRunError(ctx, t, def, ws, runErr)
		return
	}

	s.saveArtifact(ctx, t, def, stepID, result)
	s.advance(ctx, t, def, ws, result)
}

// buildPrompt assembles the user-turn prompt from the task and every
// artifact produced so far. Agents communicate only through this and task
// state, never directly with one another.
func (s *Scheduler) buildPrompt(ctx context.Context, t *core.Task, def *core.AgentDefinition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task\n%s\n\n%s\n", t.Title, t.Description)

	artifacts, err := s.deps.Artifacts.ListByTask(ctx, t.ID)
	if err != nil {
		s.log.Error("list artifacts for prompt", "task", t.ID, "error", err)
	}
	if len(artifacts) > 0 {
		b.WriteString("\n# Artifacts from prior steps\n")
		for _, a := range artifacts {
			fmt.Fprintf(&b, "\n## %s (by %s)\n%s\n", a.Name, a.Agent, string(a.Content))
		}
	}

	fmt.Fprintf(&b, "\n# Your job\nYou are the %s step of this task's workflow.\n", def.Name)
	if def.Description != "" {
		b.WriteString(def.Description + "\n")
	}
	return b.String()
}

func (s *Scheduler) eventSink(t *core.Task) core.EventSink {
	return func(_ context.Context, ev core.Event) {
		s.log.Debug("agent event", "task", t.ID, "type", ev.Type, "text", ev.Text)
	}
}

func (s *Scheduler) recordRun(
	ctx context.Context, t *core.Task, def *core.AgentDefinition, runtimeName, stepID string,
	result *core.RunResult, runErr error, duration time.Duration,
) {
	run := &core.AgentRun{
		TaskID: t.ID, StepID: stepID, Agent: def.Name, Runtime: runtimeName,
		Duration: duration, FinishedAt: s.deps.Clock(),
	}
	if runErr != nil {
		run.Status = core.RunFailed
		run.Error = runErr.Error()
	} else {
		run.Status = core.RunSucceeded
		run.SessionID = result.SessionID
		run.StopReason = result.StopReason
		run.Usage = result.Usage
		run.PermissionDenials = result.PermissionDenials
	}
	if _, err := s.deps.Runs.Record(ctx, run); err != nil {
		s.log.Error("record agent run", "task", t.ID, "error", err)
	}
	if len(run.PermissionDenials) > 0 {
		s.log.Warn("agent hit a permission denial", "task", t.ID, "agent", def.Name, "denials", run.PermissionDenials)
	}
}

// requeue moves an executing task (RUNNING or PLANNING) back to READY,
// applying mut. PLANNING -> READY is a direct edge; RUNNING has no direct
// edge to READY in the state machine (see docs/architecture.md), so it is
// routed through WAITING, which both RUNNING and READY border. mut is
// applied on the first transition so it lands exactly once regardless of
// which path is taken.
func (s *Scheduler) requeue(ctx context.Context, t *core.Task, reason string, mut func(*core.Task)) (*core.Task, error) {
	if t.Status == core.StatusRunning {
		if _, err := s.deps.Tasks.Transition(ctx, t.ID, core.StatusWaiting, reason, mut); err != nil {
			return nil, err
		}
		return s.deps.Tasks.Transition(ctx, t.ID, core.StatusReady, reason, nil)
	}
	return s.deps.Tasks.Transition(ctx, t.ID, core.StatusReady, reason, mut)
}

// handleRunError decides what a failed execution means for the task: a
// retryable error within budget goes back to READY (after honouring the
// agent's backoff), otherwise the task is failed for manual attention.
// The workspace is deliberately left in place either way, per this
// project's rule that a failed task's workspace is preserved for
// postmortem.
func (s *Scheduler) handleRunError(ctx context.Context, t *core.Task, def *core.AgentDefinition, ws *workspace.Workspace, runErr error) {
	_ = ws // preserved intentionally; not released on failure

	attempts := t.Attempts + 1
	retryable := core.IsRetryable(runErr) && attempts < t.MaxAttempts

	if retryable {
		if backoff := def.Retry.BackoffFor(attempts); backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
		}
		_, err := s.requeue(ctx, t,
			fmt.Sprintf("retrying after error: %s", redactForTask(runErr.Error())), func(task *core.Task) {
				task.Attempts = attempts
				task.LastError = redactForTask(runErr.Error())
			})
		if err != nil {
			s.log.Error("requeue after retryable error", "task", t.ID, "error", err)
		}
		return
	}

	s.failTaskWithAttempts(ctx, t, runErr, attempts)
}

// failTask fails a task without having executed anything (e.g. the agent or
// runtime could not be resolved), so the attempt counter is not advanced.
func (s *Scheduler) failTask(ctx context.Context, t *core.Task, err error) {
	s.failTaskWithAttempts(ctx, t, err, t.Attempts)
}

func (s *Scheduler) failTaskWithAttempts(ctx context.Context, t *core.Task, err error, attempts int) {
	msg := redactForTask(err.Error())
	_, txErr := s.deps.Tasks.Transition(ctx, t.ID, core.StatusFailed, msg, func(task *core.Task) {
		task.Attempts = attempts
		task.LastError = msg
	})
	if txErr != nil {
		s.log.Error("fail task", "task", t.ID, "error", txErr)
	}
}

// redactForTask keeps a raw error out of persisted task state when it looks
// like it might carry something sensitive; runtimes are already expected to
// redact their own errors (see internal/runtimes/claudecode/redact.go), this
// is a second, generic layer since the scheduler cannot know what every
// current and future runtime might leak.
func redactForTask(s string) string {
	const max = 2000
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// saveArtifact stores a step's structured (or best-effort wrapped) output.
// Non-gated steps are not required to produce schema-conformant JSON — see
// docs/architecture.md on the unverified state of Claude Code's structured
// output — so their raw text is wrapped in a minimal JSON envelope, which is
// always valid regardless of what the agent said.
func (s *Scheduler) saveArtifact(ctx context.Context, t *core.Task, def *core.AgentDefinition, stepID string, result *core.RunResult) {
	content := result.Structured
	if len(content) == 0 {
		wrapped, err := json.Marshal(map[string]string{"text": result.Text})
		if err != nil {
			s.log.Error("wrap artifact text", "task", t.ID, "error", err)
			return
		}
		content = wrapped
	}

	_, err := s.deps.Artifacts.Save(ctx, &core.Artifact{
		TaskID: t.ID, StepID: stepID, Name: def.EffectiveArtifactName(), Agent: def.Name, Content: content,
	})
	if err != nil {
		s.log.Error("save artifact", "task", t.ID, "error", err)
	}
}

// gateVerdict is the fixed shape a gated step's structured output must
// parse as.
type gateVerdict struct {
	Passed  bool   `json:"passed"`
	Summary string `json:"summary"`
}

// advance decides the task's next state after a successful execution:
// forward to the next workflow step, backward on a failed gate, or
// COMPLETED if this was the last step. The workspace is released only once
// the task reaches a state that will never resume in it.
func (s *Scheduler) advance(ctx context.Context, t *core.Task, def *core.AgentDefinition, ws *workspace.Workspace, result *core.RunResult) {
	if def.Gate.Enabled {
		verdict, err := parseGateVerdict(result.Structured)
		if err != nil {
			// An unparseable verdict from a gate step is a hard error, not a
			// silent guess: this is the honest fallback for a runtime whose
			// structured-output support is unverified (see
			// docs/architecture.md), rather than a false pass/fail heuristic.
			s.failTask(ctx, t, fmt.Errorf(
				"gate step %s did not return a valid {\"passed\": bool} verdict: %w", def.Name, err))
			return
		}
		if !verdict.Passed {
			s.rewind(ctx, t, def, verdict)
			return
		}
	}

	steps, last, err := s.stepInfo(t)
	if err != nil {
		s.failTask(ctx, t, err)
		return
	}

	if last {
		if _, err := s.deps.Tasks.Transition(ctx, t.ID, core.StatusCompleted, "workflow complete", nil); err != nil {
			s.log.Error("complete task", "task", t.ID, "error", err)
			return
		}
		s.log.Info("task completed", "task", t.ID, "agent", def.Name)
		if err := s.deps.Workspaces.Release(ctx, ws, workspace.ReleaseOptions{Cleanup: true}); err != nil {
			s.log.Error("release workspace on completion", "task", t.ID, "error", err)
		}
		return
	}

	nextStep := t.Step + 1
	nextAgent := steps[nextStep]

	// The state machine has no RUNNING -> READY edge (see
	// docs/architecture.md): a RUNNING step always advances into REVIEW,
	// which is what the next step's claim (REVIEW -> RUNNING) picks up —
	// for any next step, not only a gated one. PLANNING is the one
	// execution state that DOES have a direct edge to READY, used only for
	// a multi-step workflow's very first step.
	nextStatus := core.StatusReview
	if t.Status == core.StatusPlanning {
		nextStatus = core.StatusReady
	}

	_, err = s.deps.Tasks.Transition(ctx, t.ID, nextStatus,
		fmt.Sprintf("step %d (%s) succeeded, advancing to %s", t.Step, def.Name, nextAgent),
		func(task *core.Task) {
			task.Step = nextStep
			task.Agent = nextAgent
			task.Attempts = 0
			task.LastError = ""
		})
	if err != nil {
		s.log.Error("advance task", "task", t.ID, "error", err)
		return
	}
	s.log.Info("task advanced", "task", t.ID, "from_agent", def.Name, "to_agent", nextAgent)
}

// rewind sends a task back on a failed gate, bounded by MaxStepIterations so
// the QA feedback loop can never spin forever.
func (s *Scheduler) rewind(ctx context.Context, t *core.Task, def *core.AgentDefinition, verdict *gateVerdict) {
	loopKey := "loop:" + def.Name
	iterations := 0
	if v, ok := t.Metadata[loopKey]; ok {
		iterations, _ = strconv.Atoi(v)
	}

	if iterations >= s.cfg.MaxStepIterations {
		_, err := s.deps.Tasks.Transition(ctx, t.ID, core.StatusBlocked,
			fmt.Sprintf("gate %s failed %d times (limit %d): %s", def.Name, iterations, s.cfg.MaxStepIterations, verdict.Summary),
			nil)
		if err != nil {
			s.log.Error("block task on exhausted gate loop", "task", t.ID, "error", err)
		}
		return
	}

	steps, _, err := s.stepInfo(t)
	if err != nil {
		s.failTask(ctx, t, err)
		return
	}
	targetStep := t.Step - def.EffectiveGateStepsBack()
	if targetStep < 0 {
		targetStep = 0
	}
	targetAgent := steps[targetStep]

	_, err = s.requeue(ctx, t,
		fmt.Sprintf("gate %s failed, sending back to %s: %s", def.Name, targetAgent, verdict.Summary),
		func(task *core.Task) {
			if task.Metadata == nil {
				task.Metadata = map[string]string{}
			}
			task.Metadata[loopKey] = strconv.Itoa(iterations + 1)
			task.Step = targetStep
			task.Agent = targetAgent
			task.Attempts = 0
			task.LastError = verdict.Summary
		})
	if err != nil {
		s.log.Error("rewind task", "task", t.ID, "error", err)
	}
}

// stepInfo resolves a task's workflow steps and whether it is on the last
// one. A direct (non-workflow) task is treated as a single-step workflow.
func (s *Scheduler) stepInfo(t *core.Task) (steps []string, last bool, err error) {
	if t.Workflow == "" {
		return []string{t.Agent}, true, nil
	}
	steps, err = s.deps.Workflows.Steps(t.Workflow)
	if err != nil {
		return nil, false, fmt.Errorf("resolve workflow %s: %w", t.Workflow, err)
	}
	return steps, t.Step >= len(steps)-1, nil
}

func parseGateVerdict(structured json.RawMessage) (*gateVerdict, error) {
	if len(structured) == 0 {
		return nil, fmt.Errorf("no structured output was returned")
	}
	var v gateVerdict
	if err := json.Unmarshal(structured, &v); err != nil {
		return nil, fmt.Errorf("parse verdict: %w", err)
	}
	return &v, nil
}
