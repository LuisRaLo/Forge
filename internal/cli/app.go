// Package cli implements the ai-squad command line interface.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/LuisRaLo/ai-squad/internal/agents"
	"github.com/LuisRaLo/ai-squad/internal/config"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/events"
	"github.com/LuisRaLo/ai-squad/internal/git"
	"github.com/LuisRaLo/ai-squad/internal/runtimes"
	"github.com/LuisRaLo/ai-squad/internal/scheduler"
	"github.com/LuisRaLo/ai-squad/internal/storage"
	"github.com/LuisRaLo/ai-squad/internal/tasks"
	"github.com/LuisRaLo/ai-squad/internal/workspace"
)

// App holds the dependencies a command needs. It is constructed per command
// invocation rather than kept in package state, so nothing global is shared.
type App struct {
	Cfg        *config.Config
	DB         *storage.DB
	Repo       core.TaskRepository
	Runs       core.RunRepository
	Artifacts  core.ArtifactRepository
	Agents     *agents.Registry
	Runtimes   *runtimes.Registry
	Workspaces *workspace.Manager
	Tasks      *tasks.Service
	Scheduler  *scheduler.Scheduler
	Events     *events.Bus
	Log        *slog.Logger

	logFile *os.File
}

// CheckRuntimeOverride validates a candidate per-task runtime override
// (core.RuntimeMetadataKey) against every agent in steps: the override must
// exist, and its declared capabilities must cover what each step's agent
// requires (core.AgentDefinition.RequiredCapabilities) — the same
// capability-negotiation check applied to static config bindings at
// startup (internal/runtimes.Negotiate), applied here to a dynamic,
// per-task choice instead. Called once, at task-creation time; the
// scheduler trusts the answer afterward.
//
// workflow/agent/steps mirror tasks.CreateParams' own "exactly one of"
// fields, resolved the same way tasks.Service.Create resolves them, so a
// caller can validate before ever calling Create — an incompatible choice
// is then a clean 400 at task-creation time, not a step that claims,
// starts to execute, and fails.
func (a *App) CheckRuntimeOverride(runtimeName, workflow, agent string, steps []string) error {
	rt, err := a.Runtimes.Runtime(runtimeName)
	if err != nil {
		return err
	}

	resolved := steps
	if workflow != "" {
		wf, err := a.Cfg.Workflow(workflow)
		if err != nil {
			return err
		}
		resolved = wf.Steps
	} else if agent != "" {
		resolved = []string{agent}
	}

	for _, agentName := range resolved {
		def, err := a.Agents.Get(agentName)
		if err != nil {
			return err
		}
		missing := rt.Capabilities().Missing(def.RequiredCapabilities())
		if len(missing) > 0 {
			return fmt.Errorf(
				"runtime %q cannot run step %q: missing %v: %w",
				runtimeName, agentName, missing, core.ErrValidation)
		}
	}
	return nil
}

// workflows adapts the configuration to the tasks.Workflows interface, so the
// task service never imports the config package.
type workflows struct{ cfg *config.Config }

func (w workflows) Steps(name string) ([]string, error) {
	wf, err := w.cfg.Workflow(name)
	if err != nil {
		return nil, err
	}
	return wf.Steps, nil
}

// open loads configuration, opens the database, applies migrations and wires
// the services. Callers must call Close.
func open(ctx context.Context, configPath string) (*App, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		if os.IsNotExist(underlying(err)) {
			return nil, fmt.Errorf(
				"no configuration at %s; run `ai-squad init` first", configPath)
		}
		return nil, err
	}

	db, err := storage.Open(ctx, cfg.DatabasePath())
	if err != nil {
		return nil, err
	}
	if err := storage.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	registry, err := agents.LoadDir(cfg.System.AgentsDir)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load agents from %s: %w", cfg.System.AgentsDir, err)
	}

	if err := checkBindings(cfg, registry); err != nil {
		_ = db.Close()
		return nil, err
	}

	runtimeRegistry, err := runtimes.Build(cfg.Runtimes, cfg.Providers)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	runtimeFor := func(agentName string) string {
		if def, err := registry.Get(agentName); err == nil {
			return effectiveRuntime(cfg, def)
		}
		return ""
	}
	if err := runtimes.Negotiate(registry.List(), runtimeFor, runtimeRegistry); err != nil {
		_ = db.Close()
		return nil, err
	}

	// taskRuntimeFor is what the scheduler actually calls per step: a
	// per-task runtime override (chosen at creation time — "who resolves my
	// spec") wins over the agent's own statically configured binding.
	taskRuntimeFor := func(t *core.Task) string {
		if override := t.Metadata[core.RuntimeMetadataKey]; override != "" {
			return override
		}
		return runtimeFor(t.Agent)
	}

	workspaces, err := workspace.NewManager(workspace.Options{
		Root: filepath.Join(cfg.System.DataDir, "worktrees"),
	}, git.New())
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	logger, logFile, err := newLogger(cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	// repo is always wrapped to publish task events, even for commands that
	// never look at them: publishing to a bus with no subscribers is just an
	// empty loop, so there is no reason to special-case `serve` here. This
	// is what lets the web dashboard get real-time updates by simply
	// subscribing, without the scheduler or storage packages ever knowing a
	// bus exists.
	bus := &events.Bus{}
	repo := events.NewPublishingTaskRepository(storage.NewTaskRepo(db, core.SystemClock), bus)
	runRepo := storage.NewRunRepo(db)
	artifactRepo := storage.NewArtifactRepo(db, core.SystemClock)

	svc, err := tasks.NewService(repo, registry, workflows{cfg: cfg}, tasks.Options{
		DefaultMaxAttempts: cfg.Limits.MaxTaskAttempts,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	sched, err := scheduler.New(scheduler.Config{
		MaxConcurrency:    cfg.Scheduler.MaxConcurrency,
		PollInterval:      cfg.Scheduler.PollInterval.Duration(),
		ReapGrace:         cfg.Scheduler.LeaseDuration.Duration(),
		MaxTaskAttempts:   cfg.Limits.MaxTaskAttempts,
		MaxTaskDuration:   cfg.Limits.MaxTaskDuration.Duration(),
		MaxStepIterations: cfg.Limits.MaxStepIterations,
		MaxDailyCostUSD:   cfg.Limits.MaxDailyCostUSD,
	}, scheduler.Deps{
		Tasks: repo, Runs: runRepo, Artifacts: artifactRepo,
		Agents: registry, Runtimes: runtimeRegistry, Workspaces: workspaces, Git: git.New(),
		Workflows: workflows{cfg: cfg}, RuntimeFor: taskRuntimeFor, Log: logger,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &App{
		Cfg: cfg, DB: db, Repo: repo, Runs: runRepo, Artifacts: artifactRepo,
		Agents: registry, Runtimes: runtimeRegistry, Workspaces: workspaces,
		Tasks: svc, Scheduler: sched, Events: bus, Log: logger, logFile: logFile,
	}, nil
}

// Close releases the database handle and log file.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	if a.logFile != nil {
		_ = a.logFile.Close()
	}
	if a.DB == nil {
		return nil
	}
	return a.DB.Close()
}

// newLogger builds the process-level structured logger from
// system.log_level/log_format, writing to <data_dir>/logs/ai-squad.log so
// that interactive command output (task list, status, ...) on stdout stays
// clean and human-readable. Every field logged here is either an
// identifier (task ID, agent name) or an error already redacted before it
// reached this layer (see internal/runtimes/claudecode/redact.go and
// internal/scheduler's own redactForTask) — this function does not
// perform its own redaction, so a new log call site must not introduce one
// that logs raw runtime output.
func newLogger(cfg *config.Config) (*slog.Logger, *os.File, error) {
	logDir := filepath.Join(cfg.System.DataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	path := filepath.Join(logDir, "ai-squad.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", path, err)
	}

	var level slog.Level
	switch cfg.System.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.System.LogFormat == "json" {
		handler = slog.NewJSONHandler(f, opts)
	} else {
		handler = slog.NewTextHandler(f, opts)
	}
	return slog.New(handler), f, nil
}

// checkBindings verifies that every agent names a runtime that exists in the
// configuration, and that every workflow step names an agent that exists.
//
// This is the load-time half of capability negotiation. The other half, which
// compares an agent's required capabilities against the runtime's declared
// ones, becomes possible once runtimes are constructed.
func checkBindings(cfg *config.Config, registry *agents.Registry) error {
	for _, def := range registry.List() {
		runtime := def.Runtime
		if binding, ok := cfg.Agents[def.Name]; ok && binding.RuntimeName() != "" {
			runtime = binding.RuntimeName()
		}
		if runtime == "" {
			return core.Invalid("agents."+def.Name+".runtime",
				"agent names no runtime, and the configuration does not bind one")
		}
		if _, ok := cfg.Runtimes[runtime]; !ok {
			return core.Invalid("agents."+def.Name+".runtime",
				fmt.Sprintf("agent %s is bound to unknown runtime %q", def.Name, runtime))
		}
	}

	for name, wf := range cfg.Workflows {
		for i, step := range wf.Steps {
			if _, err := registry.Get(step); err != nil {
				return fmt.Errorf("workflows.%s.steps[%d]: %w", name, i, err)
			}
		}
	}
	return nil
}

// EffectiveRuntime returns the runtime an agent will execute on, honouring a
// configuration override of the agent's own declaration.
func (a *App) EffectiveRuntime(def *core.AgentDefinition) string {
	return effectiveRuntime(a.Cfg, def)
}

// effectiveRuntime resolves the runtime an agent executes on: a configuration
// binding wins over the agent's own declared runtime.
func effectiveRuntime(cfg *config.Config, def *core.AgentDefinition) string {
	if binding, ok := cfg.Agents[def.Name]; ok && binding.RuntimeName() != "" {
		return binding.RuntimeName()
	}
	return def.Runtime
}

// underlying unwraps to the innermost error, for os.IsNotExist checks.
func underlying(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		next := u.Unwrap()
		if next == nil {
			return err
		}
		err = next
	}
}

// defaultConfigPath resolves the configuration path, honouring AI_SQUAD_CONFIG.
func defaultConfigPath() string {
	if p := os.Getenv("AI_SQUAD_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("AI_SQUAD_DATA_DIR"); d != "" {
		return filepath.Join(d, "config.yaml")
	}
	return config.DefaultPath
}
