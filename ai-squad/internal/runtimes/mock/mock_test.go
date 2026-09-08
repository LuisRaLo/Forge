package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/santillana/ai-squad/internal/core"
)

func TestDefaultRuntimeEchoesSuccessfully(t *testing.T) {
	t.Parallel()

	rt := New("mock", nil)
	res, err := rt.Execute(context.Background(), core.RunRequest{
		TaskID: "TASK-1", Prompt: "hello",
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("expected end_turn, got %s", res.StopReason)
	}
	if len(rt.Calls()) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(rt.Calls()))
	}
}

func TestScriptedResponseByAgent(t *testing.T) {
	t.Parallel()

	rt := New("mock", nil)
	rt.Script(Response{
		Result: &core.RunResult{Text: "architected", StopReason: "end_turn"},
	})
	rt.Script(Response{
		Result: &core.RunResult{Text: "implemented", StopReason: "end_turn"},
	})

	first, err := rt.Execute(context.Background(),
		core.RunRequest{Agent: &core.AgentDefinition{Name: "architect"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.Text != "architected" {
		t.Errorf("expected first script consumed in order, got %q", first.Text)
	}

	second, err := rt.Execute(context.Background(),
		core.RunRequest{Agent: &core.AgentDefinition{Name: "developer"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.Text != "implemented" {
		t.Errorf("expected second script, got %q", second.Text)
	}
}

func TestScriptedResponseIsConsumedOnce(t *testing.T) {
	t.Parallel()

	rt := New("mock", nil)
	rt.Script(Response{Result: &core.RunResult{Text: "once", StopReason: "end_turn"}})

	first, err := rt.Execute(context.Background(), core.RunRequest{}, nil)
	if err != nil || first.Text != "once" {
		t.Fatalf("unexpected first result: %+v %v", first, err)
	}

	// The script is spent; the second call falls through to the default echo.
	second, err := rt.Execute(context.Background(), core.RunRequest{Prompt: "fallback"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.Text == "once" {
		t.Error("a scripted response must not be replayed")
	}
}

func TestScriptedError(t *testing.T) {
	t.Parallel()

	rt := New("mock", nil)
	wantErr := &core.RuntimeError{Runtime: "mock", Kind: core.ErrKindTransient, Message: "boom"}
	rt.Script(Response{Err: wantErr})

	_, err := rt.Execute(context.Background(), core.RunRequest{}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the scripted error, got %v", err)
	}
	if !core.IsRetryable(err) {
		t.Error("a transient error should be retryable")
	}
}

func TestExecuteRespectsCancellation(t *testing.T) {
	t.Parallel()

	rt := New("mock", nil)
	rt.Script(Response{Delay: time.Hour, Result: &core.RunResult{}})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := rt.Execute(ctx, core.RunRequest{}, nil)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Execute should return promptly on cancellation, took %s", elapsed)
	}
	var re *core.RuntimeError
	if !errors.As(err, &re) || re.Kind != core.ErrKindCancelled {
		t.Fatalf("expected ErrKindCancelled, got %v", err)
	}
	if core.IsRetryable(err) {
		t.Error("a cancellation must not be retryable")
	}
}

func TestEventsAreStreamedInOrder(t *testing.T) {
	t.Parallel()

	rt := New("mock", nil)
	rt.Script(Response{
		Events: []core.Event{
			{Type: core.EventTypeThinking, Text: "considering"},
			{Type: core.EventTypeToolUse, Text: "reading file"},
		},
		Result: &core.RunResult{StopReason: "end_turn"},
	})

	var got []core.EventType
	sink := core.EventSink(func(_ context.Context, ev core.Event) { got = append(got, ev.Type) })

	if _, err := rt.Execute(context.Background(), core.RunRequest{}, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []core.EventType{
		core.EventTypeStarted, core.EventTypeThinking, core.EventTypeToolUse, core.EventTypeCompleted,
	}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: expected %s, got %s", i, want[i], got[i])
		}
	}
}

func TestCustomCapabilitiesAreExposed(t *testing.T) {
	t.Parallel()

	limited := core.NewCapabilitySet(core.CapabilityFilesystemRead)
	rt := New("readonly", limited)

	missing := rt.Capabilities().Missing(core.NewCapabilitySet(core.CapabilityShell))
	if len(missing) != 1 || missing[0] != core.CapabilityShell {
		t.Errorf("expected shell to be reported missing, got %v", missing)
	}
}
