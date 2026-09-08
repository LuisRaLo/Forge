// Package mock implements a deterministic core.AgentRuntime for tests, dry
// runs, and exercising the scheduler without any real provider installed.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Response is a scripted result for one request. Match narrows which request
// it applies to; the zero Match matches everything, so a single Response can
// serve as a catch-all default.
type Response struct {
	// Match narrows applicability. Empty fields are wildcards.
	Match struct {
		Agent  string
		TaskID string
	}

	Result *core.RunResult
	Err    error

	// Delay simulates latency; it is honoured through ctx, so it participates
	// correctly in timeout and cancellation tests.
	Delay time.Duration

	// Events are streamed through the sink before Result/Err is returned.
	Events []core.Event
}

// Runtime is a scriptable core.AgentRuntime. Zero value is ready to use: with
// no scripted responses it echoes the prompt back as a successful run, which
// is enough for wiring and CLI smoke tests.
type Runtime struct {
	name string
	caps core.CapabilitySet

	mu        sync.Mutex
	responses []Response
	calls     []core.RunRequest
}

// New builds a mock runtime. caps defaults to a generous set covering every
// capability an agent can require, so it binds to any agent definition unless
// the test narrows it explicitly.
func New(name string, caps core.CapabilitySet) *Runtime {
	if caps == nil {
		caps = core.NewCapabilitySet(
			core.CapabilityStreaming, core.CapabilityTools,
			core.CapabilityFilesystemRead, core.CapabilityFilesystemWrite,
			core.CapabilityShell, core.CapabilityNetwork,
			core.CapabilityStructuredOutput, core.CapabilitySessionResume,
			core.CapabilityCostReporting, core.CapabilityBudgetLimit,
		)
	}
	return &Runtime{name: name, caps: caps}
}

var _ core.AgentRuntime = (*Runtime)(nil)

func (r *Runtime) Name() string                     { return r.name }
func (r *Runtime) Capabilities() core.CapabilitySet { return r.caps }

// Script queues a response. Responses are consumed in the order added by
// requests that match them; unmatched requests fall through to the default
// echo behaviour.
func (r *Runtime) Script(resp Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses = append(r.responses, resp)
}

// Calls returns every request received so far, for assertions.
func (r *Runtime) Calls() []core.RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.RunRequest(nil), r.calls...)
}

// Execute implements core.AgentRuntime.
func (r *Runtime) Execute(ctx context.Context, req core.RunRequest, sink core.EventSink) (*core.RunResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	resp, idx := r.match(req)
	if idx >= 0 {
		r.responses = append(r.responses[:idx], r.responses[idx+1:]...)
	}
	r.mu.Unlock()

	sink.Emit(ctx, core.Event{Type: core.EventTypeStarted, Text: "mock run started"})

	if resp != nil && resp.Delay > 0 {
		select {
		case <-time.After(resp.Delay):
		case <-ctx.Done():
			return nil, &core.RuntimeError{
				Runtime: r.name, Kind: core.ErrKindCancelled,
				Message: "cancelled while waiting", Err: ctx.Err(),
			}
		}
	}

	for _, ev := range eventsOf(resp) {
		sink.Emit(ctx, ev)
	}

	if resp != nil && resp.Err != nil {
		return nil, resp.Err
	}
	if resp != nil && resp.Result != nil {
		out := *resp.Result
		sink.Emit(ctx, core.Event{Type: core.EventTypeCompleted, Text: out.StopReason})
		return &out, nil
	}

	// Default: a deterministic, successful echo. Good enough to exercise the
	// scheduler end to end without a script.
	result := &core.RunResult{
		Text:       fmt.Sprintf("mock(%s): handled %q", r.name, req.Prompt),
		SessionID:  req.SessionID,
		StopReason: "end_turn",
		Usage:      core.Usage{Model: "mock-model", InputTokens: 1, OutputTokens: 1},
	}
	if req.OutputSchema != nil {
		result.Structured = json.RawMessage(`{}`)
	}
	sink.Emit(ctx, core.Event{Type: core.EventTypeCompleted, Text: result.StopReason})
	return result, nil
}

func (r *Runtime) match(req core.RunRequest) (*Response, int) {
	for i, resp := range r.responses {
		agentName := ""
		if req.Agent != nil {
			agentName = req.Agent.Name
		}
		if resp.Match.Agent != "" && resp.Match.Agent != agentName {
			continue
		}
		if resp.Match.TaskID != "" && resp.Match.TaskID != req.TaskID {
			continue
		}
		out := resp
		return &out, i
	}
	return nil, -1
}

func eventsOf(resp *Response) []core.Event {
	if resp == nil {
		return nil
	}
	return resp.Events
}
