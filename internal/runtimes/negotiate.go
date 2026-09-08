package runtimes

import (
	"fmt"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Negotiate checks that every agent's required capabilities are a subset of
// its bound runtime's declared capabilities. runtimeFor resolves the runtime
// name a given agent is bound to (a configuration binding, falling back to
// the agent's own declared runtime).
//
// This is the load-time half of capability negotiation described in
// docs/architecture.md: a shell-using agent bound to a runtime that cannot
// support shell access is rejected here, before any task runs, with the
// missing capabilities named.
func Negotiate(agentList []*core.AgentDefinition, runtimeFor func(agentName string) string, resolver core.RuntimeResolver) error {
	for _, agent := range agentList {
		runtimeName := runtimeFor(agent.Name)
		rt, err := resolver.Runtime(runtimeName)
		if err != nil {
			return fmt.Errorf("agent %s: %w", agent.Name, err)
		}

		missing := rt.Capabilities().Missing(agent.RequiredCapabilities())
		if len(missing) > 0 {
			return fmt.Errorf(
				"agent %s requires %v from runtime %q, which does not support %v: %w",
				agent.Name, agent.RequiredCapabilities().List(), runtimeName, missing, core.ErrValidation)
		}
	}
	return nil
}
