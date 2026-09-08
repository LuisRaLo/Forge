package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/core"
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

// newTestRepo creates a real, minimal git repository, since workspace
// acquisition (exercised once the scheduler actually runs a step) correctly
// refuses a directory that is not one — see internal/workspace's own tests.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	commit := exec.Command("git", "add", "README.md")
	commit.Dir = dir
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commit = exec.Command("git", "commit", "-q", "-m", "initial commit")
	commit.Dir = dir
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

// mockConfig points every runtime at the in-process mock runtime, so
// scheduler-driven CLI tests need no external process and cost nothing.
func mockConfig(t *testing.T, cfg string) {
	t.Helper()
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	replaced := strings.Replace(string(body),
		"runtimes:\n  claude:\n    type: claude-code\n    command: claude\n    timeout: 30m",
		"runtimes:\n  claude:\n    type: mock", 1)
	if replaced == string(body) {
		t.Fatal("mockConfig: shipped config.yaml no longer matches the expected claude-code block")
	}
	if err := os.WriteFile(cfg, []byte(replaced), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestWorkerStartDrainsASingleStepTaskToCompletion(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	repo := newTestRepo(t)
	mustRun(t, cfg, "task", "create", "--title", "solo", "--repo", repo, "--agent", "developer")

	out := mustRun(t, cfg, "worker", "start")
	if !strings.Contains(out, "done") {
		t.Errorf("expected worker start to report completion, got:\n%s", out)
	}

	list := mustRun(t, cfg, "task", "list")
	if !strings.Contains(list, "COMPLETED") {
		t.Errorf("expected the task to reach COMPLETED, got:\n%s", list)
	}
}

func TestWorkerStatusReportsQueueDepth(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	out := mustRun(t, cfg, "worker", "status")
	for _, want := range []string{"Max concurrency", "Claimable"} {
		if !strings.Contains(out, want) {
			t.Errorf("worker status missing %q:\n%s", want, out)
		}
	}
}

func TestLogsShowsStateAndRunHistory(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	repo := newTestRepo(t)
	mustRun(t, cfg, "task", "create", "--title", "logged", "--repo", repo, "--agent", "developer")
	mustRun(t, cfg, "worker", "start")

	out := mustRun(t, cfg, "logs", "TASK-1")
	if !strings.Contains(out, "state") || !strings.Contains(out, "run") {
		t.Errorf("expected both state and run entries, got:\n%s", out)
	}
	if !strings.Contains(out, "COMPLETED") {
		t.Errorf("expected the completion event in the log, got:\n%s", out)
	}
}

func TestLogsRejectsUnknownTask(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	if _, err := run(t, cfg, "logs", "TASK-404"); err == nil {
		t.Fatal("expected an error for an unknown task")
	}
}

func TestApproveCompletesAWaitingApprovalTask(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	repo := t.TempDir()
	mustRun(t, cfg, "task", "create", "--title", "needs approval", "--repo", repo, "--agent", "developer")

	// Walk the task to WAITING_APPROVAL directly via valid state-machine
	// edges (PENDING -> READY -> RUNNING -> WAITING_APPROVAL). Reaching this
	// state through an actual CI/staging pipeline is Phase 7's concern;
	// here we're testing `approve` itself, not how a task gets staged.
	app, err := open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	for _, to := range []core.TaskStatus{core.StatusReady, core.StatusRunning, core.StatusWaitingApproval} {
		if _, err := app.Repo.Transition(ctx, "TASK-1", to, "test setup", nil); err != nil {
			t.Fatalf("stage task to %s: %v", to, err)
		}
	}
	app.Close()

	out := mustRun(t, cfg, "approve", "TASK-1", "--by", "test-user", "--note", "looks good")
	if !strings.Contains(out, "COMPLETED") || !strings.Contains(out, "test-user") {
		t.Errorf("unexpected approve output: %s", out)
	}

	if _, err := run(t, cfg, "approve", "TASK-1"); err == nil {
		t.Fatal("approving an already-completed task must be rejected")
	}
}

func TestPRCommandsRequireAWorkspace(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	repo := newTestRepo(t)
	mustRun(t, cfg, "task", "create", "--title", "no workspace yet", "--repo", repo, "--agent", "developer")

	for _, args := range [][]string{
		{"pr", "create", "TASK-1"},
		{"pr", "status", "TASK-1"},
		{"pr", "comments", "TASK-1"},
	} {
		if _, err := run(t, cfg, args...); err == nil {
			t.Errorf("%v: expected an error before the task has run", args)
		}
	}
}

func TestPRCommandsRejectUnknownTask(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	if _, err := run(t, cfg, "pr", "status", "TASK-404"); err == nil {
		t.Fatal("expected an error for an unknown task")
	}
}

func TestStructuredLogsWriteToFileNotStdout(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	repo := newTestRepo(t)
	out := mustRun(t, cfg, "task", "create", "--title", "logged", "--repo", repo, "--agent", "developer")
	if strings.Contains(out, "level=") || strings.Contains(out, `"level"`) {
		t.Errorf("structured log lines must not leak into command stdout, got:\n%s", out)
	}

	logPath := filepath.Join(filepath.Dir(cfg), "logs", "ai-squad.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected the log file to exist: %v", err)
	}
}

func TestLogFormatJSONProducesJSONLines(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")
	mockConfig(t, cfg)

	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	edited := strings.Replace(string(body), "log_format: text", "log_format: json", 1)
	if edited == string(body) {
		t.Fatal("test fixture did not match the shipped config's log_format line")
	}
	if err := os.WriteFile(cfg, []byte(edited), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	repo := newTestRepo(t)
	mustRun(t, cfg, "task", "create", "--title", "t", "--repo", repo, "--agent", "developer")
	mustRun(t, cfg, "worker", "start")

	logPath := filepath.Join(filepath.Dir(cfg), "logs", "ai-squad.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(data) == 0 {
		t.Skip("no log lines were emitted at the default log level for this run")
	}
	firstLine := strings.SplitN(string(data), "\n", 2)[0]
	if !strings.HasPrefix(strings.TrimSpace(firstLine), "{") {
		t.Errorf("expected a JSON log line with log_format: json, got %q", firstLine)
	}
}

func TestStatusShowsCostAgainstLimit(t *testing.T) {
	t.Parallel()
	cfg := newWorkspace(t)
	mustRun(t, cfg, "init")

	out := mustRun(t, cfg, "status")
	if !strings.Contains(out, "Cost (last 24h)") {
		t.Errorf("expected cost visibility in status output, got:\n%s", out)
	}

	out = mustRun(t, cfg, "status", "--json")
	for _, want := range []string{"cost_last_24h_usd", "max_daily_cost_usd"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in JSON status, got:\n%s", want, out)
		}
	}
}
