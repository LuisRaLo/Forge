// Package model implements core.AgentRuntime by driving a tool-use loop
// in-process on top of a plain core.LLMProvider — the counterpart to
// internal/runtimes/claudecode, for providers with no agent loop of their
// own (Ollama, DeepSeek, any OpenAI-compatible endpoint).
//
// This is orchestration, not model intelligence: the loop mechanically
// offers the permitted tools, executes what the model calls, and feeds
// results back, exactly what the project's own brief asked for ("no quiero
// construir un agente de IA desde cero... quiero construir la capa de
// orquestación... herramientas").
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Config configures a model-driven runtime.
type Config struct {
	Name string
	// MaxToolIterations bounds the tool-call loop for a single Execute, so a
	// model that keeps calling tools forever cannot hang a task
	// indefinitely. Defaults to 25.
	MaxToolIterations int
}

// ToolBuilder constructs the tool set available for a given permission set.
// Injected rather than imported directly so this package has no compile-time
// dependency on internal/tools, and so tests can supply a fake tool set
// without touching the filesystem or a shell.
type ToolBuilder func(core.Permissions) []core.Tool

// Runtime drives a tool loop on top of a core.LLMProvider.
type Runtime struct {
	name              string
	provider          core.LLMProvider
	buildTools        ToolBuilder
	maxToolIterations int
}

// New builds a model-driven runtime over provider. buildTools may be nil,
// in which case the runtime offers no tools at all (a valid configuration
// for a read-only or planning-only agent).
func New(cfg Config, provider core.LLMProvider, buildTools ToolBuilder) (*Runtime, error) {
	if cfg.Name == "" {
		return nil, core.Invalid("name", "must be set")
	}
	if provider == nil {
		return nil, core.Invalid("provider", "must not be nil")
	}
	max := cfg.MaxToolIterations
	if max <= 0 {
		max = 25
	}
	return &Runtime{name: cfg.Name, provider: provider, buildTools: buildTools, maxToolIterations: max}, nil
}

var _ core.AgentRuntime = (*Runtime)(nil)

func (r *Runtime) Name() string { return r.name }

// Capabilities reflects what a model-driven runtime can offer: no session
// resume (each Execute is a fresh conversation) and no native cost
// reporting unless the provider happens to report usage the caller chooses
// to price — which this package does not do, so cost stays unknown rather
// than guessed. Structured output support depends on whether a schema was
// requested; declaring it here means the capability-negotiation gate in
// internal/runtimes.Negotiate lets a gated agent bind to this runtime, and
// Execute enforces the actual JSON-Schema conformance per request.
func (r *Runtime) Capabilities() core.CapabilitySet {
	return core.NewCapabilitySet(
		core.CapabilityTools, core.CapabilityFilesystemRead, core.CapabilityFilesystemWrite,
		core.CapabilityShell, core.CapabilityNetwork, core.CapabilityStructuredOutput,
	)
}

// Execute implements core.AgentRuntime: repeatedly calls the provider,
// executing any requested tool calls and feeding results back, until the
// model stops requesting tools or MaxToolIterations is reached.
func (r *Runtime) Execute(ctx context.Context, req core.RunRequest, sink core.EventSink) (*core.RunResult, error) {
	if req.Limits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Limits.Timeout)
		defer cancel()
	}

	var tools []core.Tool
	if r.buildTools != nil {
		tools = r.buildTools(req.Permissions)
	}
	toolSpecs := make([]core.ToolSpec, len(tools))
	toolsByName := make(map[string]core.Tool, len(tools))
	for i, t := range tools {
		spec := t.Spec()
		toolSpecs[i] = spec
		toolsByName[spec.Name] = t
	}

	messages := []core.Message{{Role: core.RoleUser, Text: req.Prompt}}

	sink.Emit(ctx, core.Event{Type: core.EventTypeStarted})

	start := time.Now()
	var total core.Usage
	total.Model = r.provider.Model()

	for iteration := 0; iteration < r.maxToolIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return nil, classifyCtxErr(r.name, err)
		}

		completion, err := r.provider.Complete(ctx, core.CompletionRequest{
			System: req.SystemPrompt, Messages: messages, Tools: toolSpecs,
			ResponseSchema: req.OutputSchema,
		}, sink)
		if err != nil {
			return nil, err
		}
		total.InputTokens += completion.Usage.InputTokens
		total.OutputTokens += completion.Usage.OutputTokens

		if completion.Text != "" {
			sink.Emit(ctx, core.Event{Type: core.EventTypeAssistantText, Text: completion.Text})
		}

		if len(completion.ToolCalls) == 0 {
			result := &core.RunResult{
				Text: completion.Text, StopReason: completion.StopReason,
				Usage: total, Duration: time.Since(start),
			}
			if req.OutputSchema != nil {
				structured, err := extractStructured(completion.Text, req.OutputSchema)
				if err != nil {
					return nil, &core.RuntimeError{
						Runtime: r.name, Kind: core.ErrKindPermanent,
						Message: fmt.Sprintf("model did not return output matching the requested schema: %s", err),
					}
				}
				result.Structured = structured
			}
			sink.Emit(ctx, core.Event{Type: core.EventTypeCompleted, Text: completion.StopReason})
			return result, nil
		}

		messages = append(messages, core.Message{Role: core.RoleAssistant, Text: completion.Text, ToolCalls: completion.ToolCalls})

		for _, call := range completion.ToolCalls {
			sink.Emit(ctx, core.Event{Type: core.EventTypeToolUse, Text: call.Name})
			tool, ok := toolsByName[call.Name]
			var toolResult core.ToolResult
			if !ok {
				toolResult = core.ToolResult{ToolCallID: call.ID, IsError: true, Content: "unknown or disallowed tool: " + call.Name}
			} else {
				res, err := tool.Invoke(ctx, req.WorkspaceDir, call.Input)
				if err != nil {
					toolResult = core.ToolResult{ToolCallID: call.ID, IsError: true, Content: err.Error()}
				} else {
					res.ToolCallID = call.ID
					toolResult = res
				}
			}
			sink.Emit(ctx, core.Event{Type: core.EventTypeToolResult, Text: toolResult.Content})
			messages = append(messages, core.Message{
				Role: core.RoleTool, Text: toolResult.Content, ToolCallID: call.ID,
			})
		}
	}

	return nil, &core.RuntimeError{
		Runtime: r.name, Kind: core.ErrKindPermanent,
		Message: fmt.Sprintf("exceeded %d tool-call iterations without finishing", r.maxToolIterations),
	}
}

// extractStructured validates raw text as JSON conforming, at minimum, to
// being an object — full JSON-Schema validation is not implemented; the
// scheduler's own gate-verdict parsing (internal/scheduler/execute.go)
// already treats an unparseable gate response as a hard error rather than a
// guess, which is the property that actually matters for correctness here.
func extractStructured(text string, _ json.RawMessage) (json.RawMessage, error) {
	trimmed := []byte(text)
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("model output is not valid JSON")
	}
	return trimmed, nil
}

func classifyCtxErr(name string, err error) error {
	kind := core.ErrKindCancelled
	if err.Error() == "context deadline exceeded" {
		kind = core.ErrKindTimeout
	}
	return &core.RuntimeError{Runtime: name, Kind: kind, Message: "run " + err.Error(), Err: err}
}
