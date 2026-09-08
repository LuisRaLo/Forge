package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/agents"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/runtimes/mock"
	"github.com/LuisRaLo/ai-squad/internal/storage"
	"github.com/LuisRaLo/ai-squad/internal/workspace"
)

// fakeWorkflows is a minimal Workflows implementation for tests.
type fakeWorkflows map[string][]string

func (f fakeWorkflows) Steps(name string) ([]string, error) {
	steps, ok := f[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return steps, nil
}

// fakeWorkspaces avoids real git entirely: scheduler logic is exercised
// without needing a repository on disk. internal/workspace's own tests
// already prove the real Manager's concurrency and crash-safety properties
// against real git worktrees.
type fakeWorkspaces struct {
	mu       sync.Mutex
	acquired map[string]int
	released map[string]int
	failNext bool
}

func newFakeWorkspaces() *fakeWorkspaces {
	return &fakeWorkspaces{acquired: map[string]int{}, released: map[string]int{}}
}

func (f *fakeWorkspaces) Acquire(_ context.Context, t *core.Task) (*workspace.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return nil, core.Invalid("workspace", "forced failure")
	}
	f.acquired[t.ID]++
	return &workspace.Workspace{TaskID: t.ID, Path: "/fake/" + t.ID, Branch: "ai-squad/" + t.ID}, nil
}

func (f *fakeWorkspaces) Release(_ context.Context, ws *workspace.Workspace, _ workspace.ReleaseOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released[ws.TaskID]++
	return nil
}

type testEnv struct {
	sched      *Scheduler
	tasks      core.TaskRepository
	runs       core.RunRepository
	artifacts  core.ArtifactRepository
	agents     *agents.Registry
	runtime    *mock.Runtime
	workspaces *fakeWorkspaces
	svc        *taskCreator
}

// taskCreator mirrors the minimal subset of internal/tasks.Service used by
// these tests, avoiding an import cycle-adjacent dependency for something
// this small.
type taskCreator struct{ repo core.TaskRepository }

func (c *taskCreator) create(t *testing.T, ctx context.Context, task *core.Task) *core.Task {
	t.Helper()
	if task.Status == "" {
		task.Status = core.StatusPending
	}
	if task.MaxAttempts == 0 {
		task.MaxAttempts = 3
	}
	created, err := c.repo.Create(ctx, task)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return created
}

func newTestEnv(t *testing.T, cfg Config, defs ...*core.AgentDefinition) *testEnv {
	t.Helper()
	ctx := context.Background()

	db, err := storage.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	taskRepo := storage.NewTaskRepo(db, core.SystemClock)
	runRepo := storage.NewRunRepo(db)
	artifactRepo := storage.NewArtifactRepo(db, core.SystemClock)

	registry, err := agents.NewRegistry(defs...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	rt := mock.New("mock", nil)
	ws := newFakeWorkspaces()

	workflows := fakeWorkflows{
		"feature": {"architect", "developer", "qa"},
		"bugfix":  {"developer", "qa"},
		"solo":    {"developer"},
	}

	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 2
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Millisecond
	}
	if cfg.MaxStepIterations == 0 {
		cfg.MaxStepIterations = 2
	}
	cfg.MaxTaskAttempts = 3

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	sched, err := New(cfg, Deps{
		Tasks: taskRepo, Runs: runRepo, Artifacts: artifactRepo,
		Agents: registry, Runtimes: singleRuntime{rt}, Workspaces: ws, Workflows: workflows,
		RuntimeFor: func(string) string { return "mock" },
		Clock:      core.SystemClock, Log: logger,
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}

	return &testEnv{
		sched: sched, tasks: taskRepo, runs: runRepo, artifacts: artifactRepo,
		agents: registry, runtime: rt, workspaces: ws, svc: &taskCreator{repo: taskRepo},
	}
}

// singleRuntime is a core.RuntimeResolver with exactly one runtime, named
// "mock" regardless of what name is requested — tests don't need the full
// internal/runtimes.Registry to exercise scheduler logic.
type singleRuntime struct{ rt core.AgentRuntime }

func (r singleRuntime) Runtime(string) (core.AgentRuntime, error) { return r.rt, nil }
func (r singleRuntime) Names() []string                           { return []string{"mock"} }

func devAgent() *core.AgentDefinition {
	return &core.AgentDefinition{
		Name: "developer", Runtime: "mock", SystemPrompt: "p",
		Permissions: core.Permissions{Filesystem: core.FSWorkspace},
	}
}

func qaAgent() *core.AgentDefinition {
	return &core.AgentDefinition{
		Name: "qa", Runtime: "mock", SystemPrompt: "p", ArtifactName: "qa-report",
		Permissions: core.Permissions{Filesystem: core.FSWorkspace},
		Gate:        core.GateConfig{Enabled: true, StepsBack: 1},
	}
}

func devopsAgent() *core.AgentDefinition {
	return &core.AgentDefinition{
		Name: "devops", Runtime: "mock", SystemPrompt: "p",
		Permissions: core.Permissions{Filesystem: core.FSWorkspace, Network: true},
	}
}

func architectAgent() *core.AgentDefinition {
	return &core.AgentDefinition{
		Name: "architect", Runtime: "mock", SystemPrompt: "p", ArtifactName: "plan",
		Permissions: core.Permissions{Filesystem: core.FSRead},
	}
}

// waitFor polls until cond returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestSingleStepTaskCompletes(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task := env.svc.create(t, ctx, &core.Task{
		Title: "solo task", Repository: "/repo", Workflow: "solo", Agent: "developer",
	})

	go env.sched.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	got, err := env.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusCompleted {
		t.Fatalf("expected COMPLETED, got %s", got.Status)
	}

	// Success releases (cleans up) the workspace.
	env.workspaces.mu.Lock()
	released := env.workspaces.released[task.ID]
	env.workspaces.mu.Unlock()
	if released != 1 {
		t.Errorf("expected the workspace released once, got %d", released)
	}
}

func TestMultiStepWorkflowAdvancesThroughEveryStep(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, architectAgent(), devAgent(), qaAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// QA passes on the first try: every step should run exactly once, in
	// order, and the task should end COMPLETED.
	env.runtime.Script(mock.Response{
		Match:  struct{ Agent, TaskID string }{Agent: "qa"},
		Result: &core.RunResult{StopReason: "end_turn", Structured: json.RawMessage(`{"passed":true,"summary":"looks good"}`)},
	})

	task := env.svc.create(t, ctx, &core.Task{
		Title: "feature task", Repository: "/repo", Workflow: "feature", Agent: "architect",
	})

	go env.sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	calls := env.runtime.Calls()
	var order []string
	for _, c := range calls {
		if c.TaskID == task.ID {
			order = append(order, c.Agent.Name)
		}
	}
	want := []string{"architect", "developer", "qa"}
	if len(order) != len(want) {
		t.Fatalf("expected %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("expected order %v, got %v", want, order)
		}
	}

	artifacts, err := env.artifacts.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	names := map[string]bool{}
	for _, a := range artifacts {
		names[a.Name] = true
	}
	for _, want := range []string{"plan", "text", "qa-report"} {
		// developer has no ArtifactName override, so its artifact key
		// defaults to its own name "developer"; only check plan/qa-report
		// which are explicit and load-bearing for the pipeline's naming.
		_ = want
	}
	if !names["plan"] || !names["qa-report"] {
		t.Errorf("expected plan and qa-report artifacts, got %v", names)
	}
}

func TestQAFailureLoopsBackToDeveloperThenSucceeds(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{MaxStepIterations: 3}, devAgent(), qaAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First QA pass fails, second passes. This is the exact loop the spec
	// asks for: Developer -> QA -> FAIL -> Developer -> QA -> COMPLETED.
	env.runtime.Script(mock.Response{
		Match:  struct{ Agent, TaskID string }{Agent: "qa"},
		Result: &core.RunResult{StopReason: "end_turn", Structured: json.RawMessage(`{"passed":false,"summary":"tests fail"}`)},
	})
	env.runtime.Script(mock.Response{
		Match:  struct{ Agent, TaskID string }{Agent: "qa"},
		Result: &core.RunResult{StopReason: "end_turn", Structured: json.RawMessage(`{"passed":true,"summary":"fixed"}`)},
	})

	task := env.svc.create(t, ctx, &core.Task{
		Title: "loop task", Repository: "/repo", Workflow: "bugfix", Agent: "developer",
	})

	go env.sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	var devCalls, qaCalls int
	for _, c := range env.runtime.Calls() {
		if c.TaskID != task.ID {
			continue
		}
		switch c.Agent.Name {
		case "developer":
			devCalls++
		case "qa":
			qaCalls++
		}
	}
	if devCalls != 2 {
		t.Errorf("expected developer to run twice (initial + after QA fail), got %d", devCalls)
	}
	if qaCalls != 2 {
		t.Errorf("expected qa to run twice (fail then pass), got %d", qaCalls)
	}

	events, err := env.tasks.Events(ctx, task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	sawRewind := false
	for _, ev := range events {
		if ev.Reason != "" && contains(ev.Reason, "sending back to developer") {
			sawRewind = true
		}
	}
	if !sawRewind {
		t.Error("expected an audit event recording the rewind to developer")
	}
}

func TestQAFailureExhaustsIterationsAndBlocks(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{MaxStepIterations: 2}, devAgent(), qaAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fail := mock.Response{
		Match:  struct{ Agent, TaskID string }{Agent: "qa"},
		Result: &core.RunResult{StopReason: "end_turn", Structured: json.RawMessage(`{"passed":false,"summary":"still broken"}`)},
	}
	// Never let QA pass: the loop must terminate on its own via the limit,
	// not run forever.
	for i := 0; i < 10; i++ {
		env.runtime.Script(fail)
	}

	task := env.svc.create(t, ctx, &core.Task{
		Title: "never passes", Repository: "/repo", Workflow: "bugfix", Agent: "developer",
	})

	go env.sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusBlocked
	})

	got, err := env.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusBlocked {
		t.Fatalf("expected BLOCKED, got %s", got.Status)
	}

	var qaCalls int
	for _, c := range env.runtime.Calls() {
		if c.TaskID == task.ID && c.Agent.Name == "qa" {
			qaCalls++
		}
	}
	// MaxStepIterations=2 means: 2 rewinds are permitted, so QA runs at most
	// 3 times (initial + 2 retries) before BLOCKED — never unbounded.
	if qaCalls > 3 {
		t.Errorf("expected the loop to terminate at the iteration limit, qa ran %d times", qaCalls)
	}
}

func TestRetryableRuntimeErrorRequeuesWithinAttemptBudget(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env.runtime.Script(mock.Response{
		Err: &core.RuntimeError{Runtime: "mock", Kind: core.ErrKindTransient, Message: "flaky"},
	})
	// Second attempt succeeds.

	task := env.svc.create(t, ctx, &core.Task{
		Title: "flaky task", Repository: "/repo", Workflow: "solo", Agent: "developer", MaxAttempts: 3,
	})

	go env.sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	got, err := env.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attempts != 1 {
		t.Errorf("expected the attempt counter to reflect the one failure before success, got %d", got.Attempts)
	}
}

func TestNonRetryableRuntimeErrorFailsImmediately(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env.runtime.Script(mock.Response{
		Err: &core.RuntimeError{Runtime: "mock", Kind: core.ErrKindPermissionDenied, Message: "nope"},
	})

	task := env.svc.create(t, ctx, &core.Task{
		Title: "doomed task", Repository: "/repo", Workflow: "solo", Agent: "developer", MaxAttempts: 3,
	})

	go env.sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusFailed
	})

	// A single non-retryable failure must not have burned through retries.
	var runCalls int
	for _, c := range env.runtime.Calls() {
		if c.TaskID == task.ID {
			runCalls++
		}
	}
	if runCalls != 1 {
		t.Errorf("expected exactly 1 execution attempt, got %d", runCalls)
	}
}

// concurrencyTrackingRuntime counts how many Execute calls are in flight at
// once, so the worker pool's own bound can be asserted directly rather than
// inferred from timing.
type concurrencyTrackingRuntime struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	delay    time.Duration
}

func (r *concurrencyTrackingRuntime) Name() string { return "tracking" }
func (r *concurrencyTrackingRuntime) Capabilities() core.CapabilitySet {
	return core.NewCapabilitySet(
		core.CapabilityFilesystemRead, core.CapabilityFilesystemWrite,
		core.CapabilityShell, core.CapabilityStructuredOutput,
	)
}

func (r *concurrencyTrackingRuntime) Execute(ctx context.Context, _ core.RunRequest, _ core.EventSink) (*core.RunResult, error) {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.maxSeen {
		r.maxSeen = r.inFlight
	}
	r.mu.Unlock()

	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()

	return &core.RunResult{StopReason: "end_turn"}, nil
}

func TestWorkerPoolNeverExceedsMaxConcurrency(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{MaxConcurrency: 2, PollInterval: 5 * time.Millisecond}, devAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracker := &concurrencyTrackingRuntime{delay: 60 * time.Millisecond}
	env.sched.deps.Runtimes = singleRuntime{tracker}

	var tasks []*core.Task
	for i := 0; i < 6; i++ {
		tasks = append(tasks, env.svc.create(t, ctx, &core.Task{
			Title: "pooled", Repository: "/repo", Workflow: "solo", Agent: "developer",
		}))
	}

	go env.sched.Run(ctx)

	waitFor(t, 5*time.Second, func() bool {
		done := 0
		for _, tk := range tasks {
			got, err := env.tasks.Get(ctx, tk.ID)
			if err == nil && got.Status == core.StatusCompleted {
				done++
			}
		}
		return done == len(tasks)
	})

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.maxSeen > 2 {
		t.Errorf("worker pool exceeded max_concurrency=2, observed %d concurrent executions", tracker.maxSeen)
	}
	if tracker.maxSeen < 2 {
		t.Log("note: never observed full pool utilisation; timing-dependent, not a correctness failure")
	}
}

func TestRecoveryRequeuesInterruptedTasksOnStart(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx := context.Background()

	// Simulate a task a previous, now-dead process left mid-execution.
	task := env.svc.create(t, ctx, &core.Task{
		Title: "orphaned", Repository: "/repo", Workflow: "solo", Agent: "developer",
	})
	if _, err := env.tasks.Transition(ctx, task.ID, core.StatusReady, "queued", nil); err != nil {
		t.Fatalf("to ready: %v", err)
	}
	if _, err := env.tasks.Transition(ctx, task.ID, core.StatusRunning, "claimed by a process that then died", nil); err != nil {
		t.Fatalf("to running: %v", err)
	}

	if err := env.sched.recoverInterrupted(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := env.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusReady {
		t.Fatalf("expected the orphaned task reclaimed to READY, got %s", got.Status)
	}
}

func TestRecoveryBlocksTaskWithExhaustedAttempts(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx := context.Background()

	task := env.svc.create(t, ctx, &core.Task{
		Title: "exhausted", Repository: "/repo", Workflow: "solo", Agent: "developer", MaxAttempts: 1,
	})
	if _, err := env.tasks.Transition(ctx, task.ID, core.StatusReady, "", nil); err != nil {
		t.Fatalf("to ready: %v", err)
	}
	if _, err := env.tasks.Transition(ctx, task.ID, core.StatusRunning, "", func(t *core.Task) {
		t.Attempts = 1 // already at its cap when the process died
	}); err != nil {
		t.Fatalf("to running: %v", err)
	}

	if err := env.sched.recoverInterrupted(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := env.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != core.StatusBlocked {
		t.Fatalf("expected BLOCKED for a task with no attempts left, got %s", got.Status)
	}
}

func TestWorkspaceFailureFailsTaskWithoutLosingIt(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env.workspaces.mu.Lock()
	env.workspaces.failNext = true
	env.workspaces.mu.Unlock()

	task := env.svc.create(t, ctx, &core.Task{
		Title: "bad workspace", Repository: "/repo", Workflow: "solo", Agent: "developer", MaxAttempts: 1,
	})

	go env.sched.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusFailed
	})

	got, err := env.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastError == "" {
		t.Error("expected the failure reason to be recorded")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	base := Deps{
		Tasks: storage.NewTaskRepo(mustDB(t), core.SystemClock),
		Runs:  storage.NewRunRepo(mustDB(t)), Artifacts: storage.NewArtifactRepo(mustDB(t), core.SystemClock),
		Agents: mustRegistry(t), Runtimes: singleRuntime{mock.New("m", nil)},
		Workspaces: newFakeWorkspaces(), Workflows: fakeWorkflows{},
		RuntimeFor: func(string) string { return "m" },
	}

	if _, err := New(Config{MaxConcurrency: 0, PollInterval: time.Second, MaxStepIterations: 1}, base); err == nil {
		t.Error("expected an error for zero concurrency")
	}
	if _, err := New(Config{MaxConcurrency: 1, PollInterval: 0, MaxStepIterations: 1}, base); err == nil {
		t.Error("expected an error for zero poll interval")
	}
	if _, err := New(Config{MaxConcurrency: 1, PollInterval: time.Second, MaxStepIterations: 0}, base); err == nil {
		t.Error("expected an error for zero max step iterations")
	}

	missingDeps := base
	missingDeps.Tasks = nil
	if _, err := New(Config{MaxConcurrency: 1, PollInterval: time.Second, MaxStepIterations: 1}, missingDeps); err == nil {
		t.Error("expected an error for a nil dependency")
	}
}

func mustDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := storage.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustRegistry(t *testing.T) *agents.Registry {
	t.Helper()
	reg, err := agents.NewRegistry(devAgent())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOfSub(s, sub) >= 0)
}

func indexOfSub(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRunOnceDrainsQueueAndReturns(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{MaxConcurrency: 2, PollInterval: time.Hour}, devAgent())
	ctx := context.Background()

	var tasks []*core.Task
	for i := 0; i < 4; i++ {
		tasks = append(tasks, env.svc.create(t, ctx, &core.Task{
			Title: "batch", Repository: "/repo", Workflow: "solo", Agent: "developer",
		}))
	}

	done := make(chan error, 1)
	go func() { done <- env.sched.RunOnce(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return; PollInterval=1h proves it isn't just ticking through")
	}

	for _, tk := range tasks {
		got, err := env.tasks.Get(ctx, tk.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != core.StatusCompleted {
			t.Errorf("task %s: expected COMPLETED, got %s", tk.ID, got.Status)
		}
	}
}

func TestRunOnceReturnsImmediatelyWhenNothingToDo(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{PollInterval: time.Hour}, devAgent())

	start := time.Now()
	if err := env.sched.RunOnce(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RunOnce with no work should return promptly, took %s", elapsed)
	}
}

func TestAdHocStepsRunTheChosenPipeline(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent(), qaAgent(), devopsAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// QA passes: dev -> qa -> devops, exactly the pipeline the caller
	// composed at task-creation time — this is the "dev + qa + deploy"
	// checkbox combination, expressed as an ad hoc step list rather than a
	// pre-declared named workflow.
	env.runtime.Script(mock.Response{
		Match:  struct{ Agent, TaskID string }{Agent: "qa"},
		Result: &core.RunResult{StopReason: "end_turn", Structured: json.RawMessage(`{"passed":true}`)},
	})

	task := env.svc.create(t, ctx, &core.Task{
		Title: "ad hoc pipeline", Repository: "/repo", Agent: "developer",
		Metadata: mustEncodeSteps(t, "developer", "qa", "devops"),
	})

	go env.sched.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})

	var order []string
	for _, c := range env.runtime.Calls() {
		if c.TaskID == task.ID {
			order = append(order, c.Agent.Name)
		}
	}
	want := []string{"developer", "qa", "devops"}
	if len(order) != len(want) {
		t.Fatalf("expected %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("expected order %v, got %v", want, order)
		}
	}
}

func TestAdHocSingleStepBehavesLikeDirectAgent(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, Config{}, devAgent())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task := env.svc.create(t, ctx, &core.Task{
		Title: "solo dev", Repository: "/repo", Agent: "developer",
		Metadata: mustEncodeSteps(t, "developer"),
	})

	go env.sched.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		got, err := env.tasks.Get(ctx, task.ID)
		return err == nil && got.Status == core.StatusCompleted
	})
}

func mustEncodeSteps(t *testing.T, steps ...string) map[string]string {
	t.Helper()
	encoded, err := core.EncodeSteps(steps)
	if err != nil {
		t.Fatalf("encode steps: %v", err)
	}
	return map[string]string{core.StepsMetadataKey: encoded}
}
