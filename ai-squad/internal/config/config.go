// Package config loads and validates the ai-squad YAML configuration.
package config

import (
	"github.com/santillana/ai-squad/internal/yamlx"
)

// RuntimeType identifies a family of AgentRuntime implementations.
type RuntimeType string

const (
	// RuntimeTypeClaudeCode delegates the whole agent loop to the Claude
	// Code CLI, which owns its own tools, permissions and sessions.
	RuntimeTypeClaudeCode RuntimeType = "claude-code"
	// RuntimeTypeModel drives the agent loop in-process on top of a plain
	// completion API named by the runtime's Provider field.
	RuntimeTypeModel RuntimeType = "model"
	// RuntimeTypeMock is a deterministic runtime used by tests and dry runs.
	RuntimeTypeMock RuntimeType = "mock"
)

// ProviderType identifies a family of LLMProvider implementations.
type ProviderType string

const (
	ProviderTypeOllama           ProviderType = "ollama"
	ProviderTypeOpenAICompatible ProviderType = "openai-compatible"
	ProviderTypeMock             ProviderType = "mock"
)

// Config is the fully parsed configuration file.
type Config struct {
	System    SystemConfig              `yaml:"system"`
	Scheduler SchedulerConfig           `yaml:"scheduler"`
	Limits    LimitsConfig              `yaml:"limits"`
	Approval  ApprovalConfig            `yaml:"approval"`
	Runtimes  map[string]RuntimeConfig  `yaml:"runtimes"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Agents    map[string]AgentBinding   `yaml:"agents"`
	Workflows map[string]WorkflowConfig `yaml:"workflows"`

	// path records where this configuration was loaded from.
	path string
}

// Path returns the file this configuration was loaded from, if any.
func (c *Config) Path() string { return c.path }

// SystemConfig holds process-wide paths and logging settings.
type SystemConfig struct {
	// DataDir holds the SQLite database, logs and worktrees.
	DataDir string `yaml:"data_dir"`
	// AgentsDir holds the agent YAML definitions.
	AgentsDir string `yaml:"agents_dir"`
	// LogLevel is one of debug|info|warn|error.
	LogLevel string `yaml:"log_level"`
	// LogFormat is one of text|json.
	LogFormat string `yaml:"log_format"`
}

// SchedulerConfig bounds the worker pool.
type SchedulerConfig struct {
	MaxConcurrency int            `yaml:"max_concurrency"`
	PollInterval   yamlx.Duration `yaml:"poll_interval"`
	// LeaseDuration is how long a worker owns a task before the lease is
	// considered stale and the task is recoverable after a crash.
	LeaseDuration yamlx.Duration `yaml:"lease_duration"`
}

// LimitsConfig holds global safety limits.
type LimitsConfig struct {
	MaxTaskAttempts int            `yaml:"max_task_attempts"`
	MaxTaskDuration yamlx.Duration `yaml:"max_task_duration"`
	// MaxDailyCostUSD is enforced against reported cost where a runtime
	// provides it and against an estimate otherwise.
	MaxDailyCostUSD float64 `yaml:"max_daily_cost_usd"`
	// MaxStepIterations bounds a QA/CI feedback loop.
	MaxStepIterations int `yaml:"max_step_iterations"`
}

// ApprovalConfig controls where a human must intervene.
type ApprovalConfig struct {
	// RequireForProduction must stay true in V1; production deployment is
	// never automatic.
	RequireForProduction bool `yaml:"require_for_production"`
}

// RuntimeConfig configures one AgentRuntime instance.
type RuntimeConfig struct {
	Type RuntimeType `yaml:"type"`

	// Command is the executable for process-backed runtimes such as
	// claude-code. It is resolved on PATH at wiring time, never assumed.
	Command string `yaml:"command"`
	// Args are extra arguments appended to every invocation.
	Args []string `yaml:"args"`

	// Provider names an entry in the providers block. Required when Type is
	// "model", forbidden otherwise.
	Provider string `yaml:"provider"`

	// Model overrides the default model for this runtime.
	Model string `yaml:"model"`

	Timeout yamlx.Duration `yaml:"timeout"`
}

// ProviderConfig configures one LLMProvider instance.
type ProviderConfig struct {
	Type    ProviderType   `yaml:"type"`
	BaseURL string         `yaml:"base_url"`
	Model   string         `yaml:"model"`
	Timeout yamlx.Duration `yaml:"timeout"`

	// APIKeyEnv names the environment variable holding the credential. The
	// credential itself is read at call time and never persisted.
	APIKeyEnv string `yaml:"api_key_env"`

	// APIKey exists only so that loading fails loudly when someone pastes a
	// secret into the configuration file. It is never used.
	APIKey string `yaml:"api_key"`
}

// AgentBinding binds an agent definition to a configured runtime.
type AgentBinding struct {
	Runtime string `yaml:"runtime"`
	// Provider is a deprecated alias for Runtime.
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// RuntimeName returns the effective runtime name for the binding.
func (b AgentBinding) RuntimeName() string {
	if b.Runtime != "" {
		return b.Runtime
	}
	return b.Provider
}

// WorkflowConfig is an ordered sequence of agent steps.
type WorkflowConfig struct {
	Name  string   `yaml:"name"`
	Steps []string `yaml:"steps"`
}
