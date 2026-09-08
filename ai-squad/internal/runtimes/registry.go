// Package runtimes wires concrete core.AgentRuntime implementations from
// configuration. This is the one place allowed to import a vendor-specific
// runtime package (claudecode, mock, and in later phases the model-driven
// runtimes for Ollama/DeepSeek/OpenAI-compatible providers); internal/core,
// internal/tasks and internal/scheduler never do.
package runtimes

import (
	"fmt"
	"sort"

	"github.com/santillana/ai-squad/internal/config"
	"github.com/santillana/ai-squad/internal/core"
	"github.com/santillana/ai-squad/internal/runtimes/claudecode"
	"github.com/santillana/ai-squad/internal/runtimes/mock"
)

// Registry resolves runtime names to constructed core.AgentRuntime instances.
type Registry struct {
	byName map[string]core.AgentRuntime
}

var _ core.RuntimeResolver = (*Registry)(nil)

// Build constructs every runtime named in cfg. It fails on the first runtime
// that cannot be constructed — for claude-code, that includes the executable
// not being found on PATH — so a broken installation is reported at wiring
// time rather than on the first task that happens to need it.
func Build(cfg map[string]config.RuntimeConfig) (*Registry, error) {
	reg := &Registry{byName: make(map[string]core.AgentRuntime, len(cfg))}

	for _, name := range sortedKeys(cfg) {
		rc := cfg[name]
		switch rc.Type {
		case config.RuntimeTypeClaudeCode:
			rt, err := claudecode.New(claudecode.Config{
				Name:      name,
				Command:   rc.Command,
				Model:     rc.Model,
				ExtraArgs: rc.Args,
			})
			if err != nil {
				return nil, fmt.Errorf("runtime %s: %w", name, err)
			}
			reg.byName[name] = rt

		case config.RuntimeTypeMock:
			reg.byName[name] = mock.New(name, nil)

		case config.RuntimeTypeModel:
			// Ollama, DeepSeek and OpenAI-compatible providers arrive in
			// Phase 6 behind this same runtime type. Reported clearly now
			// rather than left to fail obscurely deeper in the scheduler.
			return nil, fmt.Errorf(
				"runtime %s: model-driven runtimes are not implemented until Phase 6: %w",
				name, core.ErrValidation)

		default:
			return nil, fmt.Errorf("runtime %s: unknown type %q: %w", name, rc.Type, core.ErrValidation)
		}
	}
	return reg, nil
}

// Runtime resolves a runtime by name.
func (r *Registry) Runtime(name string) (core.AgentRuntime, error) {
	rt, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("runtime %q: %w (known runtimes: %v)", name, core.ErrNotFound, r.Names())
	}
	return rt, nil
}

// Names lists every constructed runtime, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]config.RuntimeConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
