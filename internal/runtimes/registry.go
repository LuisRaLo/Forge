// Package runtimes wires concrete core.AgentRuntime implementations from
// configuration. This is the one place allowed to import a vendor-specific
// runtime package (claudecode, mock, and in later phases the model-driven
// runtimes for Ollama/DeepSeek/OpenAI-compatible providers); internal/core,
// internal/tasks and internal/scheduler never do.
package runtimes

import (
	"fmt"
	"sort"

	"github.com/LuisRaLo/ai-squad/internal/config"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/providers/ollama"
	"github.com/LuisRaLo/ai-squad/internal/providers/openaicompat"
	"github.com/LuisRaLo/ai-squad/internal/runtimes/claudecode"
	"github.com/LuisRaLo/ai-squad/internal/runtimes/mock"
	"github.com/LuisRaLo/ai-squad/internal/runtimes/model"
	"github.com/LuisRaLo/ai-squad/internal/tools"
)

// Registry resolves runtime names to constructed core.AgentRuntime instances.
type Registry struct {
	byName map[string]core.AgentRuntime
}

var _ core.RuntimeResolver = (*Registry)(nil)

// Build constructs every runtime named in runtimeCfgs. It fails on the first
// runtime that cannot be constructed — for claude-code, that includes the
// executable not being found on PATH — so a broken installation is reported
// at wiring time rather than on the first task that happens to need it.
//
// providerCfgs is only consulted for runtimes of type "model", which drive a
// tool loop in-process on top of a plain completion API (see
// internal/runtimes/model); "claude-code" and "mock" runtimes own their own
// execution and never reference it.
func Build(runtimeCfgs map[string]config.RuntimeConfig, providerCfgs map[string]config.ProviderConfig) (*Registry, error) {
	reg := &Registry{byName: make(map[string]core.AgentRuntime, len(runtimeCfgs))}

	for _, name := range sortedKeys(runtimeCfgs) {
		rc := runtimeCfgs[name]
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
			pc, ok := providerCfgs[rc.Provider]
			if !ok {
				return nil, fmt.Errorf("runtime %s: unknown provider %q: %w", name, rc.Provider, core.ErrValidation)
			}
			rt, err := buildModelRuntime(name, rc, pc)
			if err != nil {
				return nil, fmt.Errorf("runtime %s: %w", name, err)
			}
			reg.byName[name] = rt

		default:
			return nil, fmt.Errorf("runtime %s: unknown type %q: %w", name, rc.Type, core.ErrValidation)
		}
	}
	return reg, nil
}

// buildModelRuntime constructs the LLMProvider named by a "model" runtime's
// configuration and wraps it in a tool-driving Runtime.
func buildModelRuntime(runtimeName string, rc config.RuntimeConfig, pc config.ProviderConfig) (core.AgentRuntime, error) {
	modelName := rc.Model
	if modelName == "" {
		modelName = pc.Model
	}

	var provider core.LLMProvider
	switch pc.Type {
	case config.ProviderTypeOllama:
		p, err := ollama.New(ollama.Config{
			Name: rc.Provider, BaseURL: pc.BaseURL, Model: modelName, Timeout: pc.Timeout.Duration(),
		})
		if err != nil {
			return nil, err
		}
		provider = p

	case config.ProviderTypeOpenAICompatible:
		p, err := openaicompat.New(openaicompat.Config{
			Name: rc.Provider, BaseURL: pc.BaseURL, Model: modelName,
			APIKeyEnv: pc.APIKeyEnv, Timeout: pc.Timeout.Duration(),
		})
		if err != nil {
			return nil, err
		}
		provider = p

	case config.ProviderTypeMock:
		return nil, fmt.Errorf("provider %s: type mock has no LLMProvider; use runtime type \"mock\" instead: %w",
			rc.Provider, core.ErrValidation)

	default:
		return nil, fmt.Errorf("provider %s: unknown type %q: %w", rc.Provider, pc.Type, core.ErrValidation)
	}

	return model.New(model.Config{Name: runtimeName}, provider, tools.Set)
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
