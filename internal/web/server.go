// Package web serves the local dashboard: a REST API over the same
// core.TaskRepository/AgentRegistry every CLI command uses, a WebSocket feed
// of real-time task events (internal/events.Bus), and the static frontend
// itself. It is a thin edge layer — no orchestration logic lives here, only
// translation between HTTP/JSON and the existing service layer
// (internal/tasks.Service) and ports (internal/core).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/LuisRaLo/ai-squad/internal/agents"
	"github.com/LuisRaLo/ai-squad/internal/core"
	"github.com/LuisRaLo/ai-squad/internal/events"
	"github.com/LuisRaLo/ai-squad/internal/tasks"
)

//go:embed static/index.html
var staticFS embed.FS

// Deps are the server's dependencies — ports and the existing task service,
// never a vendor-specific package.
type Deps struct {
	Tasks     *tasks.Service
	Repo      core.TaskRepository
	Runs      core.RunRepository
	Artifacts core.ArtifactRepository
	Agents    *agents.Registry
	Runtimes  core.RuntimeResolver
	Bus       *events.Bus
	// EffectiveRuntime resolves the runtime an agent will execute on,
	// honouring a configuration override — purely informational for the UI.
	EffectiveRuntime func(*core.AgentDefinition) string
	// CheckRuntimeOverride validates a candidate per-task runtime override
	// (see createTaskRequest.Runtime) against whichever step source the
	// request used — a clean rejection here means a bad choice never
	// reaches Tasks.Create. Required when Runtimes is set.
	CheckRuntimeOverride func(runtimeName, workflow, agent string, steps []string) error
	Log                  *slog.Logger
}

// Server serves the dashboard.
type Server struct {
	deps Deps
	log  *slog.Logger
	mux  *http.ServeMux
}

// New builds a Server. Call Handler to get the http.Handler to serve.
func New(deps Deps) (*Server, error) {
	if deps.Tasks == nil || deps.Repo == nil || deps.Runs == nil || deps.Artifacts == nil || deps.Agents == nil || deps.Bus == nil {
		return nil, core.Invalid("deps", "Tasks, Repo, Runs, Artifacts, Agents and Bus must all be set")
	}
	if deps.EffectiveRuntime == nil {
		deps.EffectiveRuntime = func(def *core.AgentDefinition) string { return def.Runtime }
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}

	s := &Server{deps: deps, log: deps.Log, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

// Handler returns the http.Handler to pass to an http.Server.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded FS is compiled in; this cannot fail at runtime
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))

	s.mux.HandleFunc("GET /api/agents", s.handleListAgents)
	s.mux.HandleFunc("GET /api/runtimes", s.handleListRuntimes)
	s.mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	s.mux.HandleFunc("GET /api/tasks/{id}", s.handleShowTask)
	s.mux.HandleFunc("POST /api/tasks/{id}/cancel", s.handleCancelTask)
	s.mux.HandleFunc("POST /api/tasks/{id}/retry", s.handleRetryTask)
	s.mux.HandleFunc("GET /ws", s.handleWebSocket)
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Agents.List()
	out := make([]agentView, len(list))
	for i, def := range list {
		out[i] = newAgentView(def, s.deps.EffectiveRuntime(def))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListRuntimes reports the configured runtime names, so the UI's
// "who resolves my spec" selector reflects whatever is actually wired up
// (Claude Code, a local Ollama model, ...) rather than a hardcoded list.
func (s *Server) handleListRuntimes(w http.ResponseWriter, r *http.Request) {
	var names []string
	if s.deps.Runtimes != nil {
		names = s.deps.Runtimes.Names()
	}
	writeJSON(w, http.StatusOK, names)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Tasks.List(r.Context(), core.TaskFilter{Limit: 200})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// createTaskRequest is the JSON body for POST /api/tasks. Exactly one of
// Workflow, Agent or Steps must be set, matching tasks.CreateParams.
type createTaskRequest struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Repository  string   `json:"repository"`
	Branch      string   `json:"branch"`
	Workflow    string   `json:"workflow"`
	Agent       string   `json:"agent"`
	Steps       []string `json:"steps"`
	Priority    string   `json:"priority"`
	// Runtime, when set, is a per-task override of who resolves this task's
	// pipeline — "who resolves my spec" — in place of each agent's own
	// configured runtime binding. Validated against every step's agent
	// before the task is ever created.
	Runtime string `json:"runtime"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	priority := core.PriorityNormal
	if req.Priority != "" {
		p, err := core.ParsePriority(req.Priority)
		if err != nil {
			writeError(w, err)
			return
		}
		priority = p
	}

	if req.Runtime != "" && s.deps.CheckRuntimeOverride != nil {
		if err := s.deps.CheckRuntimeOverride(req.Runtime, req.Workflow, req.Agent, req.Steps); err != nil {
			writeError(w, err)
			return
		}
	}

	task, err := s.deps.Tasks.Create(r.Context(), tasks.CreateParams{
		Title: req.Title, Description: req.Description, Repository: req.Repository,
		Branch: req.Branch, Workflow: req.Workflow, Agent: req.Agent, Steps: req.Steps,
		Runtime: req.Runtime, Priority: priority,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// taskDetail bundles a task with its history for the detail view, so the
// frontend needs one request rather than three.
type taskDetail struct {
	Task      *core.Task       `json:"task"`
	Events    []core.TaskEvent `json:"events"`
	Runs      []*core.AgentRun `json:"runs"`
	Artifacts []*core.Artifact `json:"artifacts"`
}

func (s *Server) handleShowTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.deps.Tasks.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	taskEvents, err := s.deps.Tasks.Events(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	runs, err := s.deps.Runs.ListByTask(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	artifacts, err := s.deps.Artifacts.ListByTask(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskDetail{Task: task, Events: taskEvents, Runs: runs, Artifacts: artifacts})
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.deps.Tasks.Cancel(r.Context(), r.PathValue("id"), "cancelled from the dashboard")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.deps.Tasks.Retry(r.Context(), r.PathValue("id"), "retried from the dashboard")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleWebSocket upgrades to a WebSocket and streams events.Event as JSON,
// one per message, for as long as the client stays connected. The very
// first message is a synthetic "snapshot" event carrying every current
// task, so a freshly opened dashboard has something to render before the
// next real change happens.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Error("websocket accept", "error", err)
		return
	}
	defer conn.CloseNow()

	ctx := conn.CloseRead(context.Background()) // this connection is send-only; discard/react to client closes

	snapshot, err := s.deps.Tasks.List(ctx, core.TaskFilter{Limit: 200})
	if err != nil {
		s.log.Error("websocket snapshot", "error", err)
		return
	}
	if err := wsjson.Write(ctx, conn, snapshotMessage{Type: "snapshot", Tasks: snapshot}); err != nil {
		return
	}

	ch, unsubscribe := s.deps.Bus.Subscribe(32)
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := wsjson.Write(ctx, conn, ev); err != nil {
				return
			}
		}
	}
}

type snapshotMessage struct {
	Type  string       `json:"type"`
	Tasks []*core.Task `json:"tasks"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeError maps a domain error to an HTTP status: validation failures are
// client errors, "not found" is 404, everything else is a server error. No
// error message here can contain a secret — every error reaching this layer
// already passed through the same redaction applied before it was ever
// persisted or logged (see docs/architecture.md's Phase 8 notes).
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, core.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, core.ErrValidation), errors.Is(err, core.ErrInvalidTransition):
		status = http.StatusBadRequest
	case errors.Is(err, core.ErrAlreadyExists):
		status = http.StatusConflict
	case errors.Is(err, core.ErrTerminal):
		status = http.StatusConflict
	}
	writeJSONError(w, status, err.Error())
}

type agentView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Runtime     string `json:"runtime"`
	Gate        bool   `json:"gate"`
}

func newAgentView(def *core.AgentDefinition, runtime string) agentView {
	return agentView{Name: def.Name, Description: def.Description, Runtime: runtime, Gate: def.Gate.Enabled}
}

// ListenAndServe is a small convenience wrapper so the CLI's `serve` command
// does not need to construct an http.Server by hand.
func ListenAndServe(ctx context.Context, addr string, handler http.Handler, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return Serve(ctx, ln, handler, log)
}

// Serve runs handler on an already-bound listener until ctx is cancelled,
// then shuts down gracefully. Taking a net.Listener rather than an address
// string is what lets a caller bind to a random free port (":0", used by
// the desktop app so it never collides with anything already running) and
// still learn which port was actually chosen, via ln.Addr(), before
// calling this.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if log != nil {
			log.Info("web server stopped")
		}
		return nil
	}
}
