package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// fakeProvider scripts a sequence of completions, one per call, so a tool
// loop can be driven deterministically without a real LLM.
type fakeProvider struct {
	turns []core.CompletionResult
	calls []core.CompletionRequest
	i     int
}

func (f *fakeProvider) Name() string  { return "fake" }
func (f *fakeProvider) Model() string { return "fake-model" }
func (f *fakeProvider) Complete(_ context.Context, req core.CompletionRequest, sink core.EventSink) (*core.CompletionResult, error) {
	f.calls = append(f.calls, req)
	if f.i >= len(f.turns) {
		return nil, errors.New("fakeProvider: no more scripted turns")
	}
	out := f.turns[f.i]
	f.i++
	return &out, nil
}

// fakeTool is a scriptable core.Tool for loop tests.
type fakeTool struct {
	name   string
	result core.ToolResult
	err    error
	calls  int
}

func (t *fakeTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (t *fakeTool) Invoke(_ context.Context, _ string, _ json.RawMessage) (core.ToolResult, error) {
	t.calls++
	return t.result, t.err
}

func fixedTools(tools ...core.Tool) ToolBuilder {
	return func(core.Permissions) []core.Tool { return tools }
}

func TestExecuteNoToolsSingleTurn(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: []core.CompletionResult{
		{Text: "done", StopReason: "stop", Usage: core.Usage{InputTokens: 3, OutputTokens: 2}},
	}}
	rt, err := New(Config{Name: "m"}, provider, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	res, err := rt.Execute(context.Background(), core.RunRequest{Prompt: "hi"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Text != "done" || res.StopReason != "stop" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.Usage.InputTokens != 3 || res.Usage.OutputTokens != 2 {
		t.Errorf("usage not aggregated: %+v", res.Usage)
	}
}

func TestExecuteDrivesToolCallLoop(t *testing.T) {
	t.Parallel()
	tool := &fakeTool{name: "read_file", result: core.ToolResult{Content: "file contents"}}

	provider := &fakeProvider{turns: []core.CompletionResult{
		{ToolCalls: []core.ToolCall{{ID: "1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)}}, StopReason: "tool_calls"},
		{Text: "the file says: file contents", StopReason: "stop"},
	}}
	rt, err := New(Config{Name: "m"}, provider, fixedTools(tool))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	res, err := rt.Execute(context.Background(), core.RunRequest{Prompt: "read a.go"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tool.calls != 1 {
		t.Fatalf("expected the tool invoked once, got %d", tool.calls)
	}
	if res.Text != "the file says: file contents" {
		t.Errorf("unexpected final text: %q", res.Text)
	}

	// The second provider call must have received the tool's result as
	// context, proving the loop actually fed it back.
	if len(provider.calls) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(provider.calls))
	}
	found := false
	for _, m := range provider.calls[1].Messages {
		if m.Role == core.RoleTool && m.Text == "file contents" {
			found = true
		}
	}
	if !found {
		t.Error("tool result was not fed back into the conversation")
	}
}

func TestExecuteUnknownToolReportsErrorWithoutCrashing(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: []core.CompletionResult{
		{ToolCalls: []core.ToolCall{{ID: "1", Name: "delete_everything"}}, StopReason: "tool_calls"},
		{Text: "ok, i could not do that", StopReason: "stop"},
	}}
	rt, err := New(Config{Name: "m"}, provider, fixedTools()) // no tools offered
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	res, err := rt.Execute(context.Background(), core.RunRequest{Prompt: "delete everything"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Text != "ok, i could not do that" {
		t.Errorf("unexpected text: %q", res.Text)
	}
}

func TestExecuteBoundsToolIterations(t *testing.T) {
	t.Parallel()
	tool := &fakeTool{name: "loop_tool", result: core.ToolResult{Content: "again"}}

	// Script far more tool-call turns than the configured max, proving the
	// loop terminates rather than spinning forever on a misbehaving model.
	var turns []core.CompletionResult
	for i := 0; i < 10; i++ {
		turns = append(turns, core.CompletionResult{
			ToolCalls: []core.ToolCall{{ID: "1", Name: "loop_tool"}}, StopReason: "tool_calls",
		})
	}
	provider := &fakeProvider{turns: turns}
	rt, err := New(Config{Name: "m", MaxToolIterations: 3}, provider, fixedTools(tool))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, err = rt.Execute(context.Background(), core.RunRequest{Prompt: "loop"}, nil)
	if err == nil {
		t.Fatal("expected an error when the iteration bound is exceeded")
	}
	if tool.calls > 3 {
		t.Errorf("expected at most 3 tool invocations, got %d", tool.calls)
	}
}

func TestExecuteStructuredOutputParsesValidJSON(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: []core.CompletionResult{
		{Text: `{"passed":true,"summary":"looks good"}`, StopReason: "stop"},
	}}
	rt, err := New(Config{Name: "m"}, provider, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	res, err := rt.Execute(context.Background(), core.RunRequest{
		Prompt: "verify", OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !json.Valid(res.Structured) {
		t.Fatalf("expected valid structured output, got %s", res.Structured)
	}
}

func TestExecuteStructuredOutputRejectsNonJSON(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: []core.CompletionResult{
		{Text: "sure, it passed I guess", StopReason: "stop"},
	}}
	rt, err := New(Config{Name: "m"}, provider, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, err = rt.Execute(context.Background(), core.RunRequest{
		Prompt: "verify", OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, nil)
	if err == nil {
		t.Fatal("expected an error: the model did not return the requested structured output")
	}
}

func TestExecuteHonoursTimeout(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{} // never called: context should already be done
	rt, err := New(Config{Name: "m"}, provider, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = rt.Execute(ctx, core.RunRequest{Prompt: "x"}, nil)
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}, &fakeProvider{}, nil); err == nil {
		t.Error("expected an error for a missing name")
	}
	if _, err := New(Config{Name: "m"}, nil, nil); err == nil {
		t.Error("expected an error for a nil provider")
	}
}

func TestCapabilitiesDeclareToolAndStructuredSupport(t *testing.T) {
	t.Parallel()
	rt, err := New(Config{Name: "m"}, &fakeProvider{}, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	caps := rt.Capabilities()
	for _, want := range []core.Capability{core.CapabilityTools, core.CapabilityStructuredOutput, core.CapabilityShell} {
		if !caps.Has(want) {
			t.Errorf("expected capability %s", want)
		}
	}
	if caps.Has(core.CapabilitySessionResume) {
		t.Error("a model-driven runtime has no session resume")
	}
}
