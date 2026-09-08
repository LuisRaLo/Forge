package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/santillana/ai-squad/internal/core"
)

const (
	helperEnvVar  = "AI_SQUAD_TEST_HELPER"
	fixtureEnvVar = "AI_SQUAD_TEST_FIXTURE"
)

// TestMain intercepts before the real test suite runs when this binary has
// been re-exec'd to stand in for the claude CLI. This is the standard Go
// pattern for testing process-management code (used by os/exec's own tests)
// without needing a real external binary.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

// helperMain fakes the transport-level behaviour this adapter depends on:
// NDJSON on stdout, exit codes, stderr, and an artificial delay so
// cancellation/timeout are exercised deterministically. It does not simulate
// Claude Code's actual reasoning — that boundary was verified empirically in
// Phase 1 (see docs/architecture.md) and is out of scope for a unit test of
// process mechanics.
func helperMain() {
	switch os.Getenv(fixtureEnvVar) {
	case "success":
		println_(`{"type":"system","subtype":"init"}`)
		println_(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}`)
		println_(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"sess-1","stop_reason":"end_turn","total_cost_usd":0.0123,"usage":{"input_tokens":10,"output_tokens":20}}`)
		os.Exit(0)
	case "tool_use":
		println_(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"go test ./..."}}]}}`)
		println_(`{"type":"result","subtype":"success","is_error":false,"result":"ran tests","stop_reason":"end_turn","usage":{}}`)
		os.Exit(0)
	case "api_error":
		println_(`{"type":"result","subtype":"error","is_error":true,"result":"rate limited","api_error_status":429}`)
		os.Exit(0)
	case "nonzero_exit":
		os.Stderr.WriteString("fatal: something broke, api_key=sk-shouldnotleak1234567890\n")
		os.Exit(1)
	case "slow":
		time.Sleep(5 * time.Second)
		println_(`{"type":"result","subtype":"success","is_error":false,"result":"too slow","stop_reason":"end_turn","usage":{}}`)
		os.Exit(0)
	case "no_result_line":
		println_(`{"type":"system","subtype":"init"}`)
		os.Exit(0)
	case "env_probe":
		if v := os.Getenv("SHOULD_NOT_LEAK"); v != "" {
			println_(`{"type":"result","subtype":"success","is_error":false,"result":"LEAKED:` + v + `","stop_reason":"end_turn","usage":{}}`)
		} else {
			println_(`{"type":"result","subtype":"success","is_error":false,"result":"clean","stop_reason":"end_turn","usage":{}}`)
		}
		os.Exit(0)
	default:
		os.Stderr.WriteString("unknown fixture\n")
		os.Exit(2)
	}
}

func println_(s string) { os.Stdout.WriteString(s + "\n") }

// newTestRuntime builds a Runtime that re-execs this test binary as the fake
// claude process, selecting a canned fixture through the environment.
func newTestRuntime(t *testing.T, fixture string) *Runtime {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}

	return &Runtime{
		name:    "claude-test",
		command: self,
		newCommand: func(ctx context.Context, name string, _ []string) *exec.Cmd {
			// The fake process ignores claude-style args entirely; the
			// fixture is selected purely by environment variable.
			return exec.CommandContext(ctx, name)
		},
		extraEnv: []string{
			helperEnvVar + "=1",
			fixtureEnvVar + "=" + fixture,
		},
	}
}

func TestExecuteSuccess(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "success")

	var events []core.Event
	sink := core.EventSink(func(_ context.Context, ev core.Event) { events = append(events, ev) })

	res, err := rt.Execute(context.Background(), core.RunRequest{
		Agent:  &core.AgentDefinition{Name: "developer"},
		Prompt: "implement the thing",
	}, sink)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "done" || res.SessionID != "sess-1" || res.StopReason != "end_turn" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 20 {
		t.Errorf("usage not parsed: %+v", res.Usage)
	}
	if res.Usage.CostUSD == nil || *res.Usage.CostUSD != 0.0123 {
		t.Errorf("cost not parsed: %v", res.Usage.CostUSD)
	}

	var sawStarted, sawText, sawCompleted bool
	for _, ev := range events {
		switch {
		case ev.Type == core.EventTypeStarted:
			sawStarted = true
		case ev.Type == core.EventTypeAssistantText && ev.Text == "working on it":
			sawText = true
		case ev.Type == core.EventTypeCompleted:
			sawCompleted = true
		}
	}
	if !sawStarted || !sawText || !sawCompleted {
		t.Errorf("expected started+text+completed events, got %+v", events)
	}
}

func TestExecuteToolUseEvent(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "tool_use")

	var events []core.Event
	sink := core.EventSink(func(_ context.Context, ev core.Event) { events = append(events, ev) })

	if _, err := rt.Execute(context.Background(), core.RunRequest{Agent: &core.AgentDefinition{}}, sink); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == core.EventTypeToolUse && strings.Contains(ev.Text, "Bash") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a tool_use event, got %+v", events)
	}
}

func TestExecuteAPIErrorClassification(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "api_error")

	_, err := rt.Execute(context.Background(), core.RunRequest{Agent: &core.AgentDefinition{}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var re *core.RuntimeError
	if !errors.As(err, &re) {
		t.Fatalf("expected *core.RuntimeError, got %T", err)
	}
	if re.Kind != core.ErrKindRateLimit {
		t.Errorf("expected rate limit classification for HTTP 429, got %s", re.Kind)
	}
	if !core.IsRetryable(err) {
		t.Error("a rate limit must be retryable")
	}
}

func TestExecuteNonZeroExitRedactsStderr(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "nonzero_exit")

	_, err := rt.Execute(context.Background(), core.RunRequest{Agent: &core.AgentDefinition{}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-shouldnotleak1234567890") {
		t.Fatalf("secret leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("expected the redaction marker, got %v", err)
	}
	if core.IsRetryable(err) {
		t.Error("a bare non-zero exit must not be assumed retryable")
	}
}

func TestExecuteTimeout(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "slow")

	start := time.Now()
	_, err := rt.Execute(context.Background(), core.RunRequest{
		Agent:  &core.AgentDefinition{},
		Limits: core.Limits{Timeout: 100 * time.Millisecond},
	}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Execute should return promptly on timeout, took %s", elapsed)
	}
	var re *core.RuntimeError
	if !errors.As(err, &re) || re.Kind != core.ErrKindTimeout {
		t.Fatalf("expected ErrKindTimeout, got %v", err)
	}
	if !core.IsRetryable(err) {
		t.Error("a timeout should be retryable")
	}
}

func TestExecuteCancellation(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "slow")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := rt.Execute(ctx, core.RunRequest{Agent: &core.AgentDefinition{}}, nil)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
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

func TestExecuteMissingResultLineIsAnError(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(t, "no_result_line")

	_, err := rt.Execute(context.Background(), core.RunRequest{Agent: &core.AgentDefinition{}}, nil)
	if err == nil {
		t.Fatal("a process that exits 0 without a result event must be reported as an error")
	}
}

func TestChildEnvironmentDoesNotLeakUnrelatedSecrets(t *testing.T) {
	t.Setenv("SHOULD_NOT_LEAK", "super-secret-value")
	rt := newTestRuntime(t, "env_probe")

	res, err := rt.Execute(context.Background(), core.RunRequest{Agent: &core.AgentDefinition{}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Text, "super-secret-value") {
		t.Fatal("an unrelated environment secret leaked into the child process")
	}
	if res.Text != "clean" {
		t.Errorf("expected the probe to report a clean environment, got %q", res.Text)
	}
}

func TestNewRejectsMissingExecutable(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Name: "x", Command: "definitely-not-a-real-binary-xyz"}); err == nil {
		t.Fatal("expected an error for a missing executable")
	}
}

func TestNewRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Command: "claude"}); err == nil {
		t.Fatal("expected an error for a missing name")
	}
	if _, err := New(Config{Name: "x"}); err == nil {
		t.Fatal("expected an error for a missing command")
	}
}

func TestCapabilitiesCoverEveryPermissionCombination(t *testing.T) {
	t.Parallel()

	rt := &Runtime{name: "x"}
	full := (&core.AgentDefinition{
		Permissions: core.Permissions{
			Filesystem: core.FSWorkspace,
			Shell:      core.ShellPolicy{Enabled: true},
			Network:    true,
			GitWrite:   true,
		},
		Limits: core.Limits{MaxCostUSD: 1},
	}).RequiredCapabilities()

	if missing := rt.Capabilities().Missing(full); len(missing) != 0 {
		t.Errorf("claude-code runtime should support every permission combination, missing %v", missing)
	}
}
