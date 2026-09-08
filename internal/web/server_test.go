package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/LuisRaLo/ai-squad/internal/agents"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/events"
	"github.com/LuisRaLo/ai-squad/internal/storage"
	"github.com/LuisRaLo/ai-squad/internal/tasks"
)

type fakeWorkflows map[string][]string

func (f fakeWorkflows) Steps(name string) ([]string, error) {
	steps, ok := f[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return steps, nil
}

func newTestServer(t *testing.T) (*httptest.Server, core.TaskRepository, *events.Bus) {
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

	registry, err := agents.NewRegistry(
		&core.AgentDefinition{Name: "developer", Runtime: "mock", SystemPrompt: "p",
			Permissions: core.Permissions{Filesystem: core.FSWorkspace}},
		&core.AgentDefinition{Name: "qa", Runtime: "mock", SystemPrompt: "p",
			Permissions: core.Permissions{Filesystem: core.FSWorkspace},
			Gate:        core.GateConfig{Enabled: true, StepsBack: 1}},
	)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	var bus events.Bus
	repo := events.NewPublishingTaskRepository(storage.NewTaskRepo(db, core.SystemClock), &bus)

	svc, err := tasks.NewService(repo, registry, fakeWorkflows{}, tasks.Options{
		DefaultMaxAttempts: 3, SkipRepositoryCheck: true,
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	srv, err := New(Deps{
		Tasks: svc, Repo: repo, Runs: storage.NewRunRepo(db), Artifacts: storage.NewArtifactRepo(db, core.SystemClock),
		Agents: registry, Bus: &bus,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, repo, &bus
}

func TestListAgents(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/agents")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got []agentView
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(got))
	}
	names := map[string]bool{}
	gates := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
		gates[a.Name] = a.Gate
	}
	if !names["developer"] || !names["qa"] {
		t.Errorf("unexpected agents: %+v", got)
	}
	if !gates["qa"] || gates["developer"] {
		t.Errorf("gate flag wrong: %+v", got)
	}
}

func TestCreateTaskWithSteps(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	body := `{"title":"add auth","description":"spec text","steps":["developer","qa"],"priority":"high"}`
	resp, err := http.Post(ts.URL+"/api/tasks", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var task core.Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if task.Agent != "developer" || task.Priority != core.PriorityHigh {
		t.Errorf("unexpected task: %+v", task)
	}
}

func TestCreateTaskValidationMapsTo400(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/tasks", "application/json", strings.NewReader(`{"title":""}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestListAndShowTask(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	createResp, err := http.Post(ts.URL+"/api/tasks", "application/json",
		bytes.NewReader([]byte(`{"title":"t","agent":"developer"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var created core.Task
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	listResp, err := http.Get(ts.URL + "/api/tasks")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer listResp.Body.Close()
	var list []*core.Task
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list))
	}

	showResp, err := http.Get(ts.URL + "/api/tasks/" + created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer showResp.Body.Close()
	var detail taskDetail
	if err := json.NewDecoder(showResp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Task.ID != created.ID {
		t.Errorf("unexpected detail: %+v", detail.Task)
	}
	if len(detail.Events) == 0 {
		t.Error("expected at least the creation event")
	}
}

func TestShowUnknownTaskIs404(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/tasks/TASK-404")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCancelAndRetry(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	createResp, _ := http.Post(ts.URL+"/api/tasks", "application/json",
		bytes.NewReader([]byte(`{"title":"t","agent":"developer"}`)))
	var created core.Task
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	cancelResp, err := http.Post(ts.URL+"/api/tasks/"+created.ID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer cancelResp.Body.Close()
	var cancelled core.Task
	json.NewDecoder(cancelResp.Body).Decode(&cancelled)
	if cancelled.Status != core.StatusCancelled {
		t.Errorf("expected CANCELLED, got %s", cancelled.Status)
	}

	// Retry on a CANCELLED task is not retryable (only FAILED/BLOCKED are).
	retryResp, err := http.Post(ts.URL+"/api/tasks/"+created.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-retryable task, got %d", retryResp.StatusCode)
	}
}

func TestWebSocketSnapshotThenLiveEvent(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	var snap map[string]any
	if err := wsjson.Read(ctx, conn, &snap); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snap["type"] != "snapshot" {
		t.Fatalf("expected a snapshot message first, got %+v", snap)
	}

	// Now create a task over REST and confirm the same connection sees it
	// pushed live, without polling.
	resp, err := http.Post(ts.URL+"/api/tasks", "application/json",
		bytes.NewReader([]byte(`{"title":"live task","agent":"developer"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	var ev map[string]any
	if err := wsjson.Read(ctx, conn, &ev); err != nil {
		t.Fatalf("read live event: %v", err)
	}
	if ev["type"] != "task_created" {
		t.Fatalf("expected task_created, got %+v", ev)
	}
}

func TestNewRejectsMissingDeps(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{}); err == nil {
		t.Fatal("expected an error for missing dependencies")
	}
}
