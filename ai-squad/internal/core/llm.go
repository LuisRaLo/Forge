package core

import (
	"context"
	"encoding/json"
)

// LLMProvider is a SECONDARY port. It models a plain chat-completion API with
// no agent loop of its own (Ollama, DeepSeek, any OpenAI-compatible endpoint).
//
// The orchestrator does not depend on this interface. It is consumed by
// model-driven AgentRuntime implementations, which supply the tool loop that
// these APIs lack. Keeping it secondary is deliberate: a full agent runtime
// such as Claude Code must never be squeezed through it.
type LLMProvider interface {
	// Name is the configured provider name.
	Name() string

	// Model is the model identifier this provider is configured to call.
	Model() string

	// Complete performs one request/response round trip. Streaming deltas,
	// when supported, are reported through sink; the final aggregated result
	// is still returned.
	Complete(ctx context.Context, req CompletionRequest, sink EventSink) (*CompletionResult, error)
}

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation.
type Message struct {
	Role Role
	Text string
	// ToolCalls are populated on assistant messages that request tools.
	ToolCalls []ToolCall
	// ToolCallID links a RoleTool message back to the call it answers.
	ToolCallID string
}

// ToolSpec describes a tool exposed to the model.
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema for the tool's arguments.
	InputSchema json.RawMessage
}

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is the outcome of executing a ToolCall.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// Tool is an executable capability offered to a model-driven runtime. Tools are
// gated by Permissions before they are ever exposed to a model.
type Tool interface {
	Spec() ToolSpec
	Invoke(ctx context.Context, workspaceDir string, input json.RawMessage) (ToolResult, error)
}

// CompletionRequest is one call to an LLMProvider.
type CompletionRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
	// ResponseSchema requests structured output when the provider supports it.
	ResponseSchema json.RawMessage
	MaxTokens      int
	// Temperature is a pointer so "unset" differs from "0".
	Temperature *float64
	Stop        []string
}

// CompletionResult is one response from an LLMProvider.
type CompletionResult struct {
	Text       string
	ToolCalls  []ToolCall
	Structured json.RawMessage
	StopReason string
	Usage      Usage
}
