// Package tasks holds the application logic that governs a task's lifecycle.
// It is the only component allowed to decide which state transition a command
// implies; the repository enforces that the transition is legal.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Workflows resolves a workflow name to its ordered agent steps. It is a
// one-method interface so the task service does not depend on the
// configuration package.
type Workflows interface {
	Steps(name string) ([]string, error)
}

// Options tunes service behaviour.
type Options struct {
	// DefaultMaxAttempts caps step executions when a task does not set its
	// own limit.
	DefaultMaxAttempts int
	// SkipRepositoryCheck disables filesystem validation of the repository
	// path. Tests set it; production does not.
	SkipRepositoryCheck bool
}

// Service exposes the task operations the CLI and scheduler share.
type Service struct {
	repo      core.TaskRepository
	agents    core.AgentRegistry
	workflows Workflows
	opts      Options
}

// NewService wires a task service. agents and workflows may be nil, in which
// case the corresponding validation is skipped.
func NewService(repo core.TaskRepository, agents core.AgentRegistry, workflows Workflows, opts Options) (*Service, error) {
	if repo == nil {
		return nil, core.Invalid("repo", "must not be nil")
	}
	if opts.DefaultMaxAttempts < 1 {
		opts.DefaultMaxAttempts = 3
	}
	return &Service{repo: repo, agents: agents, workflows: workflows, opts: opts}, nil
}

// CreateParams describes a new task.
type CreateParams struct {
	Title       string
	Description string
	Repository  string
	Branch      string
	Workflow    string
	Agent       string
	Priority    core.Priority
	MaxAttempts int
	ParentTask  string
	Metadata    map[string]string

	// IdempotencyKey makes creation replay-safe: creating twice with the
	// same key returns the original task instead of a duplicate.
	IdempotencyKey string
}

// Create validates the request and inserts a PENDING task.
//
// Exactly one of Workflow or Agent must be given: a task either follows a
// declared step sequence or targets a single agent directly.
func (s *Service) Create(ctx context.Context, p CreateParams) (*core.Task, error) {
	if p.IdempotencyKey != "" {
		existing, err := s.repo.GetByIdempotencyKey(ctx, p.IdempotencyKey)
		switch {
		case err == nil:
			return existing, nil
		case errors.Is(err, core.ErrNotFound):
			// fall through to create
		default:
			return nil, err
		}
	}

	if strings.TrimSpace(p.Title) == "" {
		return nil, core.Invalid("title", "must not be empty")
	}

	hasWorkflow := strings.TrimSpace(p.Workflow) != ""
	hasAgent := strings.TrimSpace(p.Agent) != ""
	switch {
	case hasWorkflow && hasAgent:
		return nil, core.Invalid("workflow",
			"specify either a workflow or a single agent, not both")
	case !hasWorkflow && !hasAgent:
		return nil, core.Invalid("workflow",
			"specify a workflow (--workflow) or a single agent (--agent)")
	}

	firstAgent := p.Agent
	if hasWorkflow {
		steps, err := s.resolveWorkflow(p.Workflow)
		if err != nil {
			return nil, err
		}
		firstAgent = steps[0]
	}
	if err := s.checkAgentExists(firstAgent); err != nil {
		return nil, err
	}

	repoPath, err := s.resolveRepository(p.Repository)
	if err != nil {
		return nil, err
	}

	maxAttempts := p.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = s.opts.DefaultMaxAttempts
	}
	priority := p.Priority
	if priority == 0 {
		priority = core.PriorityNormal
	}

	var parent *string
	if strings.TrimSpace(p.ParentTask) != "" {
		if _, err := s.repo.Get(ctx, p.ParentTask); err != nil {
			return nil, fmt.Errorf("parent task: %w", err)
		}
		id := p.ParentTask
		parent = &id
	}

	t := &core.Task{
		Title:          strings.TrimSpace(p.Title),
		Description:    p.Description,
		Repository:     repoPath,
		Branch:         p.Branch,
		Workflow:       p.Workflow,
		Step:           0,
		Agent:          firstAgent,
		Status:         core.StatusPending,
		Priority:       priority,
		MaxAttempts:    maxAttempts,
		ParentTaskID:   parent,
		IdempotencyKey: p.IdempotencyKey,
		Metadata:       p.Metadata,
	}
	return s.repo.Create(ctx, t)
}

// Get returns a task by identifier.
func (s *Service) Get(ctx context.Context, id string) (*core.Task, error) {
	return s.repo.Get(ctx, id)
}

// List returns tasks matching the filter.
func (s *Service) List(ctx context.Context, f core.TaskFilter) ([]*core.Task, error) {
	return s.repo.List(ctx, f)
}

// Events returns a task's audit trail.
func (s *Service) Events(ctx context.Context, id string) ([]core.TaskEvent, error) {
	return s.repo.Events(ctx, id)
}

// Cancel stops a task. It is idempotent: cancelling an already-cancelled task
// succeeds and returns the task unchanged. Cancelling a COMPLETED task is an
// error, because that would misrepresent a finished result.
func (s *Service) Cancel(ctx context.Context, id, reason string) (*core.Task, error) {
	t, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Status == core.StatusCancelled {
		return t, nil
	}
	if t.Status == core.StatusCompleted {
		return nil, fmt.Errorf("task %s is already COMPLETED: %w", id, core.ErrTerminal)
	}
	if reason == "" {
		reason = "cancelled by operator"
	}
	return s.repo.Transition(ctx, id, core.StatusCancelled, reason, nil)
}

// Retry re-queues a FAILED or BLOCKED task.
//
// It is idempotent in the sense that matters operationally: a task already
// waiting to run is returned untouched rather than being reset, so repeated
// retries never multiply work or reset the attempt counter twice.
func (s *Service) Retry(ctx context.Context, id, reason string) (*core.Task, error) {
	t, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	switch t.Status {
	case core.StatusReady, core.StatusPending:
		// Already queued; nothing to do.
		return t, nil
	case core.StatusFailed, core.StatusBlocked:
		// Retryable.
	default:
		return nil, fmt.Errorf(
			"task %s is %s and cannot be retried (retry applies to FAILED or BLOCKED tasks): %w",
			id, t.Status, core.ErrInvalidTransition)
	}

	if reason == "" {
		reason = "retried by operator"
	}
	return s.repo.Transition(ctx, id, core.StatusReady, reason, func(t *core.Task) {
		// A manual retry grants a fresh budget of attempts for the current
		// step and clears the stale error, otherwise a task that exhausted
		// its attempts could never be resumed by hand.
		t.Attempts = 0
		t.LastError = ""
		t.CompletedAt = nil
	})
}

// resolveWorkflow returns the steps of a workflow, validating that it exists
// and is non-empty.
func (s *Service) resolveWorkflow(name string) ([]string, error) {
	if s.workflows == nil {
		return nil, core.Invalid("workflow", "no workflows are configured")
	}
	steps, err := s.workflows.Steps(name)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, core.Invalid("workflow", "workflow "+name+" has no steps")
	}
	return steps, nil
}

func (s *Service) checkAgentExists(name string) error {
	if s.agents == nil {
		return nil
	}
	if _, err := s.agents.Get(name); err != nil {
		return err
	}
	return nil
}

// resolveRepository validates that the repository path exists and is a
// directory. Whether it is a usable git repository is checked when a workspace
// is created, which is where that failure is actionable.
func (s *Service) resolveRepository(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if s.opts.SkipRepositoryCheck {
		return path, nil
	}

	abs, err := expandAbs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", core.Invalid("repository", "path does not exist: "+abs)
		}
		return "", fmt.Errorf("stat repository %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", core.Invalid("repository", "path is not a directory: "+abs)
	}
	if err := rejectSensitiveRepository(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// rejectSensitiveRepository refuses a repository path that is the home
// directory itself, or a conventionally sensitive directory beneath it
// (.ssh, .aws, .kube, and similar credential stores). A task's workspace is
// a git worktree of whatever this path points at: if it were $HOME on a
// machine where dotfiles happen to be tracked in git, an agent would gain
// read/write access to SSH keys, cloud credentials and Kubernetes config —
// exactly what this project's own security rules rule out ("No permitir
// acceso a .ssh, .aws, .kube, passwords o keychains"). This is checked once,
// at task creation, rather than trusted to every runtime's own sandboxing.
func rejectSensitiveRepository(abs string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil // nothing to compare against
	}
	home = filepath.Clean(home)
	abs = filepath.Clean(abs)

	if abs == home {
		return core.Invalid("repository",
			"refusing to use the home directory itself as a task repository")
	}

	for _, sensitive := range []string{".ssh", ".aws", ".kube", ".gnupg", ".docker"} {
		if abs == filepath.Join(home, sensitive) {
			return core.Invalid("repository",
				fmt.Sprintf("refusing to use %s as a task repository: it conventionally holds credentials", abs))
		}
	}
	return nil
}
