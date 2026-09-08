package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/yamlx"
)

const (
	// DefaultDataDir is the installation directory used when no
	// configuration file location implies one.
	DefaultDataDir = "~/.ai-squad"

	// DefaultPath is where ai-squad looks for its configuration.
	DefaultPath = DefaultDataDir + "/config.yaml"
)

// Default returns a configuration with every optional field populated. Load
// starts from this value, so an empty file yields a usable system.
func Default() *Config {
	return &Config{
		System: SystemConfig{
			// Empty means "derive from the configuration file's own
			// directory", so that --config fully isolates an installation.
			DataDir:   "",
			AgentsDir: "",
			LogLevel:  "info",
			LogFormat: "text",
		},
		Scheduler: SchedulerConfig{
			MaxConcurrency: 3,
			PollInterval:   yamlx.Duration(5 * time.Second),
			LeaseDuration:  yamlx.Duration(15 * time.Minute),
		},
		Limits: LimitsConfig{
			MaxTaskAttempts:   3,
			MaxTaskDuration:   yamlx.Duration(30 * time.Minute),
			MaxDailyCostUSD:   20,
			MaxStepIterations: 3,
		},
		Approval:  ApprovalConfig{RequireForProduction: true},
		Runtimes:  map[string]RuntimeConfig{},
		Providers: map[string]ProviderConfig{},
		Agents:    map[string]AgentBinding{},
		Workflows: map[string]WorkflowConfig{},
	}
}

// Load reads, decodes and validates the configuration at path. A "~" prefix is
// expanded. Unknown fields are rejected so that typos surface immediately.
func Load(path string) (*Config, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", expanded, err)
	}

	cfg, err := parseWithBase(data, filepath.Dir(expanded))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", expanded, err)
	}
	cfg.path = expanded
	return cfg, nil
}

// Parse decodes and validates configuration bytes with no file of their own.
// An unset data directory then falls back to DefaultDataDir.
func Parse(data []byte) (*Config, error) {
	return parseWithBase(data, "")
}

// parseWithBase decodes configuration bytes, resolving an unset data directory
// against baseDir.
func parseWithBase(data []byte, baseDir string) (*Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		// An empty document decodes to io.EOF, which is not an error here.
		if err.Error() != "EOF" {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	if err := cfg.normalise(baseDir); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// normalise expands paths and fills in derived defaults.
//
// An unset data_dir resolves to the directory holding the configuration file.
// That keeps an installation self-contained: pointing --config at a directory
// puts the database, agents and worktrees beside it rather than silently
// reaching into the home directory.
func (c *Config) normalise(baseDir string) error {
	if strings.TrimSpace(c.System.DataDir) == "" {
		if env := strings.TrimSpace(os.Getenv("AI_SQUAD_DATA_DIR")); env != "" {
			c.System.DataDir = env
		} else if baseDir != "" {
			c.System.DataDir = baseDir
		} else {
			c.System.DataDir = DefaultDataDir
		}
	}

	dataDir, err := ExpandPath(c.System.DataDir)
	if err != nil {
		return err
	}
	c.System.DataDir = dataDir

	if c.System.AgentsDir == "" {
		c.System.AgentsDir = filepath.Join(dataDir, "agents")
	} else {
		agentsDir, err := ExpandPath(c.System.AgentsDir)
		if err != nil {
			return err
		}
		c.System.AgentsDir = agentsDir
	}

	for name, wf := range c.Workflows {
		if wf.Name == "" {
			wf.Name = name
			c.Workflows[name] = wf
		}
	}
	return nil
}

// Validate enforces every configuration invariant. It returns the first
// problem found, wrapped so errors.Is(err, core.ErrValidation) holds.
func (c *Config) Validate() error {
	if c.Scheduler.MaxConcurrency < 1 {
		return core.Invalid("scheduler.max_concurrency", "must be at least 1")
	}
	if c.Scheduler.MaxConcurrency > 64 {
		return core.Invalid("scheduler.max_concurrency", "must be at most 64")
	}
	if c.Scheduler.PollInterval.Duration() <= 0 {
		return core.Invalid("scheduler.poll_interval", "must be greater than zero")
	}
	if c.Scheduler.LeaseDuration.Duration() <= 0 {
		return core.Invalid("scheduler.lease_duration", "must be greater than zero")
	}
	if c.Limits.MaxTaskAttempts < 1 {
		return core.Invalid("limits.max_task_attempts", "must be at least 1")
	}
	if c.Limits.MaxTaskDuration.Duration() <= 0 {
		return core.Invalid("limits.max_task_duration", "must be greater than zero")
	}
	if c.Limits.MaxStepIterations < 1 {
		return core.Invalid("limits.max_step_iterations",
			"must be at least 1, otherwise a QA feedback loop can never make progress")
	}
	if c.Limits.MaxDailyCostUSD < 0 {
		return core.Invalid("limits.max_daily_cost_usd", "must not be negative")
	}
	if !c.Approval.RequireForProduction {
		return core.Invalid("approval.require_for_production",
			"must be true: V1 never deploys to production without human approval")
	}
	switch c.System.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return core.Invalid("system.log_level", "must be one of debug|info|warn|error")
	}
	switch c.System.LogFormat {
	case "text", "json":
	default:
		return core.Invalid("system.log_format", "must be one of text|json")
	}

	if err := c.validateProviders(); err != nil {
		return err
	}
	if err := c.validateRuntimes(); err != nil {
		return err
	}
	if err := c.validateAgents(); err != nil {
		return err
	}
	return c.validateWorkflows()
}

func (c *Config) validateProviders() error {
	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		field := "providers." + name

		// Refuse to load a configuration containing an inline credential.
		// Secrets belong in the environment, never in a file we read, log,
		// or copy into the data directory.
		if strings.TrimSpace(p.APIKey) != "" {
			return core.Invalid(field+".api_key",
				"inline API keys are not allowed; use api_key_env to name an environment variable")
		}

		switch p.Type {
		case ProviderTypeOllama, ProviderTypeOpenAICompatible, ProviderTypeMock:
		case "":
			return core.Invalid(field+".type", "must be set")
		default:
			return core.Invalid(field+".type", "unknown provider type "+string(p.Type))
		}

		if p.Type != ProviderTypeMock {
			if strings.TrimSpace(p.BaseURL) == "" {
				return core.Invalid(field+".base_url", "must be set")
			}
			if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
				return core.Invalid(field+".base_url", "must start with http:// or https://")
			}
			if strings.TrimSpace(p.Model) == "" {
				return core.Invalid(field+".model", "must be set")
			}
		}
		if p.Type == ProviderTypeOpenAICompatible && strings.TrimSpace(p.APIKeyEnv) == "" {
			return core.Invalid(field+".api_key_env",
				"must name the environment variable holding the credential")
		}
		if p.Timeout.Duration() < 0 {
			return core.Invalid(field+".timeout", "must not be negative")
		}
	}
	return nil
}

func (c *Config) validateRuntimes() error {
	for _, name := range sortedKeys(c.Runtimes) {
		r := c.Runtimes[name]
		field := "runtimes." + name

		switch r.Type {
		case RuntimeTypeClaudeCode:
			if strings.TrimSpace(r.Command) == "" {
				return core.Invalid(field+".command",
					"must name the executable to run, for example \"claude\"")
			}
			if r.Provider != "" {
				return core.Invalid(field+".provider",
					"claude-code runtimes own their model; remove provider")
			}
		case RuntimeTypeModel:
			if strings.TrimSpace(r.Provider) == "" {
				return core.Invalid(field+".provider",
					"model runtimes must name a provider from the providers block")
			}
			if _, ok := c.Providers[r.Provider]; !ok {
				return core.Invalid(field+".provider",
					"unknown provider "+r.Provider)
			}
		case RuntimeTypeMock:
		case "":
			return core.Invalid(field+".type", "must be set")
		default:
			return core.Invalid(field+".type", "unknown runtime type "+string(r.Type))
		}

		if r.Timeout.Duration() < 0 {
			return core.Invalid(field+".timeout", "must not be negative")
		}
	}
	return nil
}

func (c *Config) validateAgents() error {
	for _, name := range sortedKeys(c.Agents) {
		b := c.Agents[name]
		runtime := b.RuntimeName()
		if strings.TrimSpace(runtime) == "" {
			return core.Invalid("agents."+name+".runtime", "must be set")
		}
		if _, ok := c.Runtimes[runtime]; !ok {
			return core.Invalid("agents."+name+".runtime", "unknown runtime "+runtime)
		}
	}
	return nil
}

func (c *Config) validateWorkflows() error {
	for _, name := range sortedKeys(c.Workflows) {
		wf := c.Workflows[name]
		if len(wf.Steps) == 0 {
			return core.Invalid("workflows."+name+".steps", "must contain at least one step")
		}
		for i, step := range wf.Steps {
			if strings.TrimSpace(step) == "" {
				return core.Invalid(
					fmt.Sprintf("workflows.%s.steps[%d]", name, i), "must not be empty")
			}
		}
	}
	return nil
}

// Workflow returns a workflow by name.
func (c *Config) Workflow(name string) (WorkflowConfig, error) {
	wf, ok := c.Workflows[name]
	if !ok {
		return WorkflowConfig{}, fmt.Errorf("workflow %q: %w (known workflows: %s)",
			name, core.ErrNotFound, strings.Join(sortedKeys(c.Workflows), ", "))
	}
	return wf, nil
}

// DatabasePath returns the SQLite file path derived from the data directory.
func (c *Config) DatabasePath() string {
	return filepath.Join(c.System.DataDir, "ai-squad.db")
}

// ExpandPath expands a leading "~" and returns an absolute path.
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", p, err)
	}
	return abs, nil
}

func homeDir() (string, error) {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return u.HomeDir, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
