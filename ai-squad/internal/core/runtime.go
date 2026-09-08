package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Capability describes something an AgentRuntime is able to do. The
// orchestrator uses capabilities to reject impossible agent/runtime bindings at
// configuration-load time instead of failing halfway through a task.
type Capability string

const (
	CapabilityStreaming        Capability = "streaming"
	CapabilityTools            Capability = "tools"
	CapabilityFilesystemRead   Capability = "filesystem_read"
	CapabilityFilesystemWrite  Capability = "filesystem_write"
	CapabilityShell            Capability = "shell"
	CapabilityNetwork          Capability = "network"
	CapabilityStructuredOutput Capability = "structured_output"
	CapabilitySessionResume    Capability = "session_resume"
	CapabilityCostReporting    Capability = "cost_reporting"
	CapabilityBudgetLimit      Capability = "budget_limit"
)

// CapabilitySet is an unordered set of capabilities.
type CapabilitySet map[Capability]struct{}

// NewCapabilitySet builds a set from the given capabilities.
func NewCapabilitySet(caps ...Capability) CapabilitySet {
	s := make(CapabilitySet, len(caps))
	for _, c := range caps {
		s[c] = struct{}{}
	}
	return s
}

// Has reports whether the set contains c.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

// Missing returns the members of want that are absent from s, sorted.
func (s CapabilitySet) Missing(want CapabilitySet) []Capability {
	var out []Capability
	for c := range want {
		if !s.Has(c) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// List returns the set's members in a stable order.
func (s CapabilitySet) List() []Capability {
	out := make([]Capability, 0, len(s))
	for c := range s {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AgentRuntime is the orchestrator's PRIMARY port. It executes one step of a
// workflow on behalf of an agent and returns a structured result.
//
// Implementations fall into two families:
//
//   - Delegating runtimes, which hand the whole agent loop to an external
//     process that owns its own tools and permissions (e.g. Claude Code).
//   - Model-driven runtimes, which implement the agent loop in-process and
//     depend on the secondary LLMProvider port (Ollama, DeepSeek, OpenAI).
//
// The core never imports a concrete runtime; it is always injected.
type AgentRuntime interface {
	// Name is the configured runtime name, used in logs and task records.
	Name() string

	// Capabilities reports what this runtime can do. It must be cheap and
	// must not perform I/O.
	Capabilities() CapabilitySet

	// Execute runs a single agent step to completion. It must honour ctx
	// cancellation and deadlines, and must never write secrets to sink.
	Execute(ctx context.Context, req RunRequest, sink EventSink) (*RunResult, error)
}

// RunRequest is everything a runtime needs for one agent step. It is a value
// type so it can be logged, hashed for idempotency, and replayed in tests.
type RunRequest struct {
	// TaskID and StepID identify the unit of work. StepID is stable across
	// retries of the same step so runtimes can deduplicate side effects.
	TaskID string
	StepID string

	// Agent is the resolved agent definition driving this step.
	Agent *AgentDefinition

	// SystemPrompt and Prompt are the instructions for this step. Prompt is
	// assembled by the orchestrator from task state and prior artifacts;
	// agents never talk to each other directly.
	SystemPrompt string
	Prompt       string

	// WorkspaceDir is the absolute path the runtime is allowed to work in.
	// It is the isolation boundary: runtimes must not reach outside it.
	WorkspaceDir string

	// Inputs are structured artifacts produced by earlier steps, keyed by
	// artifact name (e.g. "plan.json").
	Inputs map[string]json.RawMessage

	// OutputSchema, when non-nil, requests structured output validated
	// against this JSON Schema. Runtimes lacking CapabilityStructuredOutput
	// must return an error rather than silently degrading.
	OutputSchema json.RawMessage

	// SessionID lets a runtime resume prior context. Empty means a fresh
	// session.
	SessionID string

	// Permissions and Limits are the effective policy for this step, already
	// merged from agent definition and global configuration.
	Permissions Permissions
	Limits      Limits
}

// EventType classifies a streamed runtime event.
type EventType string

const (
	EventTypeStarted       EventType = "started"
	EventTypeThinking      EventType = "thinking"
	EventTypeAssistantText EventType = "assistant_text"
	EventTypeToolUse       EventType = "tool_use"
	EventTypeToolResult    EventType = "tool_result"
	EventTypeUsage         EventType = "usage"
	EventTypeLog           EventType = "log"
	EventTypeCompleted     EventType = "completed"
)

// Event is a single streamed occurrence during a run.
type Event struct {
	Type      EventType
	Timestamp time.Time
	// Text is human-readable and MUST already be redacted of secrets.
	Text string
	// Raw is the runtime's native payload, retained for the audit log.
	Raw json.RawMessage
}

// EventSink receives streamed events. It must be non-blocking and must never
// fail a run: a sink error is a logging problem, not an execution problem.
type EventSink func(ctx context.Context, ev Event)

// DiscardEvents is an EventSink that drops everything.
func DiscardEvents(context.Context, Event) {}

// Emit sends ev to sink, tolerating a nil sink.
func (s EventSink) Emit(ctx context.Context, ev Event) {
	if s == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	s(ctx, ev)
}

// Usage records what a run consumed. Fields a runtime cannot determine are
// left at their zero value; CostUSD is a pointer so "unknown" is
// distinguishable from "free".
type Usage struct {
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CostUSD             *float64
	// CostEstimated marks CostUSD as computed by us from a price table rather
	// than reported by the provider. Cost limits enforced against an
	// estimate must be surfaced to the user as estimates.
	CostEstimated bool
}

// RunResult is the outcome of one agent step.
type RunResult struct {
	// Text is the runtime's final free-form output.
	Text string
	// Structured is the validated structured output when OutputSchema was
	// requested, otherwise nil.
	Structured json.RawMessage
	// SessionID identifies the runtime session, for resume and audit.
	SessionID string
	// StopReason is the runtime's native termination reason.
	StopReason string
	// Usage is best-effort accounting.
	Usage Usage
	// Duration is wall-clock time spent in Execute.
	Duration time.Duration
	// PermissionDenials records policy refusals the runtime reported. A
	// non-empty value is a signal for the policy layer, not necessarily a
	// failure.
	PermissionDenials []string
}

// RuntimeErrorKind classifies a failure so the retry policy can decide without
// string-matching.
type RuntimeErrorKind string

const (
	ErrKindTimeout          RuntimeErrorKind = "timeout"
	ErrKindCancelled        RuntimeErrorKind = "cancelled"
	ErrKindAuth             RuntimeErrorKind = "auth"
	ErrKindRateLimit        RuntimeErrorKind = "rate_limit"
	ErrKindTransient        RuntimeErrorKind = "transient"
	ErrKindPermanent        RuntimeErrorKind = "permanent"
	ErrKindPermissionDenied RuntimeErrorKind = "permission_denied"
	ErrKindBudgetExceeded   RuntimeErrorKind = "budget_exceeded"
	ErrKindUnsupported      RuntimeErrorKind = "unsupported"
)

// RuntimeError is the error type every AgentRuntime returns on failure.
type RuntimeError struct {
	Runtime string
	Kind    RuntimeErrorKind
	Message string
	// ExitCode is meaningful for process-backed runtimes; -1 when unknown.
	ExitCode int
	Err      error
}

func (e *RuntimeError) Error() string {
	return fmt.Sprintf("runtime %s: %s: %s", e.Runtime, e.Kind, e.Message)
}

func (e *RuntimeError) Unwrap() error { return e.Err }

// Retryable reports whether re-running the step could plausibly succeed.
func (e *RuntimeError) Retryable() bool {
	switch e.Kind {
	case ErrKindTimeout, ErrKindRateLimit, ErrKindTransient:
		return true
	default:
		return false
	}
}

// IsRetryable reports whether err is a retryable runtime failure.
func IsRetryable(err error) bool {
	var re *RuntimeError
	if errors.As(err, &re) {
		return re.Retryable()
	}
	return false
}
