// Package agents loads agent definitions from YAML into domain objects.
package agents

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/santillana/ai-squad/internal/core"
	"github.com/santillana/ai-squad/internal/yamlx"
)

// spec is the on-disk YAML shape of an agent. It is deliberately separate from
// core.AgentDefinition so that YAML quirks (such as shell being either a bool
// or a list) never leak into the domain model.
type spec struct {
	Name         string          `yaml:"name"`
	Description  string          `yaml:"description"`
	Runtime      string          `yaml:"runtime"`
	Provider     string          `yaml:"provider"`
	Model        string          `yaml:"model"`
	SystemPrompt string          `yaml:"system_prompt"`
	Tools        []string        `yaml:"tools"`
	Permissions  permissionsSpec `yaml:"permissions"`
	Limits       limitsSpec      `yaml:"limits"`
	Timeout      *yamlx.Duration `yaml:"timeout"`
	Retry        retrySpec       `yaml:"retry"`
}

type permissionsSpec struct {
	Filesystem string    `yaml:"filesystem"`
	Shell      shellSpec `yaml:"shell"`
	Network    bool      `yaml:"network"`
	GitWrite   bool      `yaml:"git_write"`
}

type limitsSpec struct {
	MaxAttempts int             `yaml:"max_attempts"`
	Timeout     *yamlx.Duration `yaml:"timeout"`
	MaxCostUSD  float64         `yaml:"max_cost_usd"`
}

type retrySpec struct {
	MaxAttempts int             `yaml:"max_attempts"`
	Backoff     *yamlx.Duration `yaml:"backoff"`
	Multiplier  float64         `yaml:"multiplier"`
	MaxBackoff  *yamlx.Duration `yaml:"max_backoff"`
}

// shellSpec decodes the three accepted YAML forms of a shell permission:
//
//	shell: false          -> denied
//	shell: true           -> any command
//	shell: [go, git]      -> only these executables
type shellSpec struct {
	Enabled  bool
	Commands []string
}

// UnmarshalYAML implements the bool-or-list decoding.
func (s *shellSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var b bool
		if err := node.Decode(&b); err != nil {
			return fmt.Errorf("line %d: shell must be true, false, or a list of commands", node.Line)
		}
		s.Enabled = b
		s.Commands = nil
		return nil
	case yaml.SequenceNode:
		var cmds []string
		if err := node.Decode(&cmds); err != nil {
			return fmt.Errorf("line %d: shell list must contain strings: %w", node.Line, err)
		}
		for _, c := range cmds {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("line %d: shell command entries must not be empty", node.Line)
			}
		}
		s.Enabled = true
		s.Commands = cmds
		return nil
	default:
		return fmt.Errorf("line %d: shell must be true, false, or a list of commands", node.Line)
	}
}

// toDomain converts a decoded spec into a validated domain agent.
func (s *spec) toDomain(defaultName string) (*core.AgentDefinition, error) {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = defaultName
	}

	// "provider" is accepted as a deprecated alias for "runtime" so that
	// configurations written against the earlier vocabulary keep loading.
	runtime := strings.TrimSpace(s.Runtime)
	if runtime == "" {
		runtime = strings.TrimSpace(s.Provider)
	}

	fs := core.FilesystemAccess(strings.ToLower(strings.TrimSpace(s.Permissions.Filesystem)))
	if fs == "" {
		fs = core.FSNone
	}

	timeout := s.Limits.Timeout
	if timeout == nil {
		timeout = s.Timeout
	}

	def := &core.AgentDefinition{
		Name:         name,
		Description:  s.Description,
		Runtime:      runtime,
		Model:        s.Model,
		SystemPrompt: strings.TrimSpace(s.SystemPrompt),
		Tools:        s.Tools,
		Permissions: core.Permissions{
			Filesystem: fs,
			Shell: core.ShellPolicy{
				Enabled:  s.Permissions.Shell.Enabled,
				Commands: s.Permissions.Shell.Commands,
			},
			Network:  s.Permissions.Network,
			GitWrite: s.Permissions.GitWrite,
		},
		Limits: core.Limits{
			MaxAttempts: s.Limits.MaxAttempts,
			MaxCostUSD:  s.Limits.MaxCostUSD,
		},
		Retry: core.RetryPolicy{
			MaxAttempts: s.Retry.MaxAttempts,
			Multiplier:  s.Retry.Multiplier,
		},
	}
	if timeout != nil {
		def.Limits.Timeout = timeout.Duration()
	}
	if s.Retry.Backoff != nil {
		def.Retry.Backoff = s.Retry.Backoff.Duration()
	}
	if s.Retry.MaxBackoff != nil {
		def.Retry.MaxBackoff = s.Retry.MaxBackoff.Duration()
	}

	if err := def.Validate(); err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	return def, nil
}
