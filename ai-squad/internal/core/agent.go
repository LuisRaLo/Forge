package core

import (
	"strings"
	"time"
)

// FilesystemAccess is the coarse filesystem policy for an agent.
type FilesystemAccess string

const (
	// FSNone denies all filesystem access.
	FSNone FilesystemAccess = "none"
	// FSRead permits reading inside the workspace only.
	FSRead FilesystemAccess = "read"
	// FSWorkspace permits reading and writing inside the workspace only.
	FSWorkspace FilesystemAccess = "workspace"
)

// Valid reports whether f is a known access level.
func (f FilesystemAccess) Valid() bool {
	switch f {
	case FSNone, FSRead, FSWorkspace:
		return true
	}
	return false
}

// ShellPolicy controls shell execution. A nil/absent policy denies everything.
// When Enabled is true and Commands is empty, any command is allowed; when
// Commands is non-empty, only those executables may be invoked.
type ShellPolicy struct {
	Enabled  bool
	Commands []string
}

// Allows reports whether the named executable may be run.
func (p ShellPolicy) Allows(command string) bool {
	if !p.Enabled {
		return false
	}
	if len(p.Commands) == 0 {
		return true
	}
	for _, c := range p.Commands {
		if c == command {
			return true
		}
	}
	return false
}

// Permissions is the effective policy for an agent. The zero value denies
// everything, so forgetting to configure permissions fails closed.
type Permissions struct {
	Filesystem FilesystemAccess
	Shell      ShellPolicy
	Network    bool
	GitWrite   bool
}

// Validate checks the permission block.
func (p Permissions) Validate() error {
	if p.Filesystem == "" {
		return Invalid("permissions.filesystem", "must be one of none|read|workspace")
	}
	if !p.Filesystem.Valid() {
		return Invalid("permissions.filesystem", "unknown value "+string(p.Filesystem))
	}
	if p.GitWrite && p.Filesystem != FSWorkspace {
		return Invalid("permissions.git_write",
			"requires filesystem: workspace, since committing writes to the worktree")
	}
	if p.Shell.Enabled && p.Filesystem == FSNone {
		return Invalid("permissions.shell",
			"requires filesystem access; a shell with no filesystem is contradictory")
	}
	return nil
}

// Limits bounds a single agent step.
type Limits struct {
	// MaxAttempts caps executions of one step before it is BLOCKED.
	MaxAttempts int
	// Timeout bounds a single execution.
	Timeout time.Duration
	// MaxCostUSD caps spend for one step. Zero means "inherit the global
	// limit"; enforcement is best-effort and may be an estimate.
	MaxCostUSD float64
}

// RetryPolicy describes how a failed step is retried.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
	Multiplier  float64
	MaxBackoff  time.Duration
}

// BackoffFor returns the delay before the given attempt number (1-based).
func (r RetryPolicy) BackoffFor(attempt int) time.Duration {
	if attempt < 1 || r.Backoff <= 0 {
		return 0
	}
	d := r.Backoff
	mult := r.Multiplier
	if mult < 1 {
		mult = 1
	}
	for i := 1; i < attempt; i++ {
		d = time.Duration(float64(d) * mult)
		if r.MaxBackoff > 0 && d >= r.MaxBackoff {
			return r.MaxBackoff
		}
	}
	if r.MaxBackoff > 0 && d > r.MaxBackoff {
		return r.MaxBackoff
	}
	return d
}

// AgentDefinition is an agent described independently of any runtime. The same
// definition can be bound to Claude Code, a local model, or a hosted API.
type AgentDefinition struct {
	Name        string
	Description string

	// Runtime names an entry in the runtimes configuration block. It is a
	// name, not a type: the binding is resolved at wiring time.
	Runtime string
	// Model optionally overrides the runtime's default model.
	Model string

	SystemPrompt string

	// Tools restricts which tools the agent may use. Empty means "the
	// runtime's default set", subject to Permissions.
	Tools []string

	Permissions Permissions
	Limits      Limits
	Retry       RetryPolicy
}

// Validate checks an agent definition in isolation.
func (a *AgentDefinition) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return Invalid("name", "must not be empty")
	}
	if strings.TrimSpace(a.Runtime) == "" {
		return Invalid("runtime", "agent "+a.Name+" must name a runtime")
	}
	if strings.TrimSpace(a.SystemPrompt) == "" {
		return Invalid("system_prompt", "agent "+a.Name+" must define a system prompt")
	}
	if err := a.Permissions.Validate(); err != nil {
		return err
	}
	if a.Limits.MaxAttempts < 0 {
		return Invalid("limits.max_attempts", "must not be negative")
	}
	if a.Limits.Timeout < 0 {
		return Invalid("limits.timeout", "must not be negative")
	}
	if a.Limits.MaxCostUSD < 0 {
		return Invalid("limits.max_cost_usd", "must not be negative")
	}
	if a.Retry.MaxAttempts < 0 {
		return Invalid("retry.max_attempts", "must not be negative")
	}
	return nil
}

// RequiredCapabilities derives the capabilities a runtime must provide in order
// to host this agent. The orchestrator checks this against
// AgentRuntime.Capabilities at load time, so an impossible binding such as a
// shell-using agent on a bare completion API is rejected before any task runs.
func (a *AgentDefinition) RequiredCapabilities() CapabilitySet {
	caps := NewCapabilitySet()
	switch a.Permissions.Filesystem {
	case FSRead:
		caps[CapabilityFilesystemRead] = struct{}{}
	case FSWorkspace:
		caps[CapabilityFilesystemRead] = struct{}{}
		caps[CapabilityFilesystemWrite] = struct{}{}
	}
	if a.Permissions.Shell.Enabled {
		caps[CapabilityShell] = struct{}{}
		caps[CapabilityTools] = struct{}{}
	}
	if a.Permissions.Network {
		caps[CapabilityNetwork] = struct{}{}
	}
	if a.Permissions.GitWrite {
		caps[CapabilityShell] = struct{}{}
		caps[CapabilityFilesystemWrite] = struct{}{}
	}
	if len(a.Tools) > 0 {
		caps[CapabilityTools] = struct{}{}
	}
	if a.Limits.MaxCostUSD > 0 {
		caps[CapabilityCostReporting] = struct{}{}
	}
	return caps
}
