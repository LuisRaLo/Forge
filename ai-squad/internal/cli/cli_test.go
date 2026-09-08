package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run executes the command tree with the given arguments against an isolated
// configuration, returning combined output.
func run(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()

	root := NewRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--config", configPath}, args...))

	err := root.Execute()
	return buf.String(), err
}

// newWorkspace returns a config path inside a fresh temporary directory.
func newWorkspace(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.yaml")
}

func mustRun(t *testing.T, configPath string, args ...string) string {
	t.Helper()
	out, err := run(t, configPath, args...)
	if err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
	return out
}

func TestInitCreatesAWorkingInstallation(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)

	out := mustRun(t, cfg, "init")
	if !strings.Contains(out, "migrated") {
		t.Errorf("init should report the migration, got:\n%s", out)
	}

	dir := filepath.Dir(cfg)
	for _, want := range []string{
		"config.yaml",
		"ai-squad.db",
		filepath.Join("agents", "developer.yaml"),
		filepath.Join("agents", "reviewer.yaml"),
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s to exist: %v", want, err)
		}
	}

	// The configuration must not be world readable: it describes what the
	// agents are allowed to do.
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config permissions should be 0600, got %04o", perm)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)

	mustRun(t, cfg, "init")

	// Edit the config, re-run init, and confirm the edit survived.
	const marker = "\n# operator edit\n"
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(cfg, append(body, marker...), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := mustRun(t, cfg, "init")
	if !strings.Contains(out, "already present") {
		t.Errorf("re-running init should report untouched files, got:\n%s", out)
	}

	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(after), "# operator edit") {
		t.Fatal("init overwrote an existing configuration")
	}
}

func TestDefaultConfigurationAndAgentsAreValid(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	// The shipped defaults must pass the same validation as anything an
	// operator writes by hand.
	out := mustRun(t, cfg, "config", "validate")
	if !strings.Contains(out, "is valid") {
		t.Errorf("expected a validation success, got:\n%s", out)
	}
	if !strings.Contains(out, "agents     5") {
		t.Errorf("expected the 5 default agents, got:\n%s", out)
	}
}

func TestCommandsRequireInitialisation(t *testing.T) {
	t.Parallel()

	_, err := run(t, newWorkspace(t), "task", "list")
	if err == nil {
		t.Fatal("expected an error before init")
	}
	if !strings.Contains(err.Error(), "ai-squad init") {
		t.Errorf("the error should tell the user what to do, got %q", err)
	}
}

func TestTaskLifecycle(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	repo := t.TempDir()

	out := mustRun(t, cfg, "task", "create",
		"--title", "Add Google authentication",
		"--repo", repo,
		"--workflow", "feature",
		"--priority", "high",
		"--metadata", "ticket=ABC-1")
	if !strings.Contains(out, "TASK-1") || !strings.Contains(out, "PENDING") {
		t.Fatalf("unexpected create output: %s", out)
	}

	out = mustRun(t, cfg, "task", "list")
	if !strings.Contains(out, "TASK-1") || !strings.Contains(out, "architect") {
		t.Errorf("list should show the task at its first step, got:\n%s", out)
	}

	out = mustRun(t, cfg, "task", "show", "TASK-1")
	for _, want := range []string{"TASK-1", "PENDING", "architect", "ABC-1", "History"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}

	out = mustRun(t, cfg, "task", "cancel", "TASK-1", "--reason", "no longer needed")
	if !strings.Contains(out, "CANCELLED") {
		t.Errorf("expected CANCELLED, got: %s", out)
	}

	// Cancelling twice is safe.
	mustRun(t, cfg, "task", "cancel", "TASK-1")
}

func TestTaskCreateIsIdempotentWithAKey(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	args := []string{"task", "create", "--title", "once",
		"--workflow", "bugfix", "--idempotency-key", "req-1"}

	first := mustRun(t, cfg, args...)
	second := mustRun(t, cfg, args...)
	if first != second {
		t.Fatalf("replay produced a different task:\n%s\n%s", first, second)
	}

	out := mustRun(t, cfg, "task", "list")
	if strings.Contains(out, "TASK-2") {
		t.Fatalf("replay created a duplicate task:\n%s", out)
	}
}

func TestTaskCreateRejectsBadInput(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	tests := map[string][]string{
		"unknown workflow":  {"task", "create", "--title", "t", "--workflow", "ghost"},
		"unknown agent":     {"task", "create", "--title", "t", "--agent", "ghost"},
		"no target":         {"task", "create", "--title", "t"},
		"both targets":      {"task", "create", "--title", "t", "--workflow", "feature", "--agent", "qa"},
		"bad priority":      {"task", "create", "--title", "t", "--workflow", "feature", "--priority", "soonish"},
		"bad metadata":      {"task", "create", "--title", "t", "--workflow", "feature", "--metadata", "novalue"},
		"missing repo path": {"task", "create", "--title", "t", "--workflow", "feature", "--repo", "/nope/nowhere"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, cfg, args...); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestTaskRetryRejectsNonRetryableState(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mustRun(t, cfg, "task", "create", "--title", "t", "--workflow", "bugfix")

	// A PENDING task is already queued: retry is a no-op, not an error.
	out := mustRun(t, cfg, "task", "retry", "TASK-1")
	if !strings.Contains(out, "PENDING") {
		t.Errorf("expected the task to stay PENDING, got: %s", out)
	}

	mustRun(t, cfg, "task", "cancel", "TASK-1")
	if _, err := run(t, cfg, "task", "retry", "TASK-1"); err == nil {
		t.Fatal("a cancelled task must not be retryable")
	}
}

func TestAgentListAndShow(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	out := mustRun(t, cfg, "agent", "list")
	for _, want := range []string{"architect", "developer", "qa", "reviewer", "devops"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent list missing %q:\n%s", want, out)
		}
	}

	out = mustRun(t, cfg, "agent", "show", "developer")
	if !strings.Contains(out, "workspace") {
		t.Errorf("developer should have workspace filesystem access:\n%s", out)
	}
	if !strings.Contains(out, "filesystem_write") {
		t.Errorf("show should report required runtime capabilities:\n%s", out)
	}

	// The reviewer is read-only and must not require shell or write access.
	out = mustRun(t, cfg, "agent", "show", "reviewer")
	if strings.Contains(out, "filesystem_write") || strings.Contains(out, "capability_shell") {
		t.Errorf("reviewer must not require write or shell capabilities:\n%s", out)
	}

	if _, err := run(t, cfg, "agent", "show", "ghost"); err == nil {
		t.Fatal("expected an error for an unknown agent")
	}
}

func TestStatusSummary(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mustRun(t, cfg, "task", "create", "--title", "t", "--workflow", "bugfix")

	out := mustRun(t, cfg, "status")
	for _, want := range []string{"Database", "Agents", "Max concurrency", "PENDING"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestJSONOutputIsMachineReadable(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mustRun(t, cfg, "task", "create", "--title", "json me", "--workflow", "bugfix")

	out := mustRun(t, cfg, "task", "list", "--json")
	if !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Errorf("expected a JSON array, got:\n%s", out)
	}
	if !strings.Contains(out, `"ID": "TASK-1"`) {
		t.Errorf("expected the task in JSON, got:\n%s", out)
	}

	out = mustRun(t, cfg, "status", "--json")
	if !strings.Contains(out, `"tasks_total": 1`) {
		t.Errorf("expected a task count, got:\n%s", out)
	}
}

func TestConfigShowRedactsNothingBecauseItHoldsNoSecrets(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	out := mustRun(t, cfg, "config", "show")
	// Only the names of credential-bearing variables may appear, never a
	// credential itself. The shipped defaults contain neither.
	for _, forbidden := range []string{"sk-", "Bearer ", "token="} {
		if strings.Contains(out, forbidden) {
			t.Errorf("configuration output looks like it leaked a secret: %q", forbidden)
		}
	}
}

func TestConfigValidateRejectsBrokenBinding(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	// Point an agent at a runtime that does not exist.
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	broken := strings.Replace(string(body),
		"  developer:\n    runtime: claude", "  developer:\n    runtime: ghost", 1)
	if broken == string(body) {
		t.Fatal("test fixture did not match the shipped configuration")
	}
	if err := os.WriteFile(cfg, []byte(broken), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err = run(t, cfg, "config", "validate")
	if err == nil {
		t.Fatal("expected validation to reject an unknown runtime")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the offending runtime, got %q", err)
	}
}

func TestWorkflowStepsMustNameRealAgents(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	broken := strings.Replace(string(body),
		"steps: [developer, qa, reviewer]", "steps: [developer, ghost]", 1)
	if err := os.WriteFile(cfg, []byte(broken), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := run(t, cfg, "config", "validate"); err == nil {
		t.Fatal("a workflow referencing a missing agent must be rejected at load time")
	}
}
