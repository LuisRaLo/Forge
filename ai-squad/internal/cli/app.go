// Package cli implements the ai-squad command line interface.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santillana/ai-squad/internal/agents"
	"github.com/santillana/ai-squad/internal/config"
	"github.com/santillana/ai-squad/internal/core"
	"github.com/santillana/ai-squad/internal/git"
	"github.com/santillana/ai-squad/internal/runtimes"
	"github.com/santillana/ai-squad/internal/scheduler"
	"github.com/santillana/ai-squad/internal/storage"
	"github.com/santillana/ai-squad/internal/tasks"
	"github.com/santillana/ai-squad/internal/workspace"
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

	runtimeRegistry, err := runtimes.Build(cfg.Runtimes)
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

	workspaces, err := workspace.NewManager(workspace.Options{
		Root: filepath.Join(cfg.System.DataDir, "worktrees"),
	}, git.New())
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	repo := storage.NewTaskRepo(db, core.SystemClock)
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
		Agents: registry, Runtimes: runtimeRegistry, Workspaces: workspaces,
		Workflows: workflows{cfg: cfg}, RuntimeFor: runtimeFor,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &App{
		Cfg: cfg, DB: db, Repo: repo, Runs: runRepo, Artifacts: artifactRepo,
		Agents: registry, Runtimes: runtimeRegistry, Workspaces: workspaces,
		Tasks: svc, Scheduler: sched,
	}, nil
}

// Close releases the database handle.
func (a *App) Close() error {
	if a == nil || a.DB == nil {
		return nil
	}
	return a.DB.Close()
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
