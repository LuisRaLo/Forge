// Package claudecode adapts the Claude Code CLI to core.AgentRuntime.
//
// Claude Code is not a chat API behind a process boundary: it is a complete
// agent that owns its own tool loop, permission gate, resumable sessions and
// cost accounting (verified live against the installed CLI — see
// docs/architecture.md). This adapter's job is therefore narrower than an
// HTTP client's: it encapsulates the OS process around that agent —
// argument construction, environment isolation, streamed stdout, exit codes,
// timeouts and cancellation — and translates its native JSON into core types.
//
// The tool allow/disallow list built from core.Permissions in args.go is a
// policy layer on top of the CLI's own gate, not a hard sandbox: Claude Code
// trusts its own process to read/write what its tools are pointed at. Where
// hard isolation is required, run this adapter's process inside a container;
// that is not implemented here.
package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Verified and unverified, as of live testing against Claude Code 2.1.236 on
// this machine (see docs/architecture.md for the full account):
//
//   - Process lifecycle, NDJSON stream parsing, cost/usage/session
//     extraction, and --safe-mode's effect on CLAUDE.md/plugin isolation:
//     confirmed end to end against the real CLI.
//   - The prompt-placement fix (a variadic --allowedTools/--disallowedTools
//     silently swallows a trailing prompt) and the --safe-mode fix (a plain
//     run without it leaked an unrelated custom tool set) were both found by
//     running real requests, not inferred.
//   - The exact tool NAMES available for shell execution (assumed here to be
//     "Bash", per generic Claude Code documentation) could not be confirmed
//     on this machine: even with --safe-mode, a scoped Bash(echo:*) grant
//     produced a session whose available tools matched this very outer
//     harness's own deferred-tool set rather than the classic built-in
//     toolset, which --help attributes to "admin-managed (policy) settings"
//     surviving --safe-mode. That is an account/environment-level
//     configuration this adapter has no way to see around, not a defect in
//     the request it sends. Treat the toolArgs mapping in args.go as correct
//     against the documented, unmanaged CLI and unverified against a
//     policy-managed account until confirmed on one.

// Config configures one Claude Code runtime instance.
type Config struct {
	// Name is the runtime's identity in logs and task records.
	Name string
	// Command is the executable to run, e.g. "claude". Resolved on PATH.
	Command string
	// Model, when set, is used unless an agent specifies its own.
	Model string
	// PermissionMode overrides defaultPermissionMode.
	PermissionMode string
	// ExtraArgs are appended to every invocation, after this adapter's own
	// flags and before the prompt.
	ExtraArgs []string
}

// Runtime executes agent steps by delegating the whole tool loop to the
// Claude Code CLI.
type Runtime struct {
	name           string
	command        string
	defaultModel   string
	permissionMode string
	extraArgs      []string

	// extraEnv is appended to the filtered child environment. Production
	// leaves this empty; tests use it to select a fake process fixture.
	extraEnv []string

	// newCommand constructs the process. Overridden in tests to exec the test
	// binary itself instead of a real claude CLI.
	newCommand func(ctx context.Context, name string, args []string) *exec.Cmd
}

// New builds a Claude Code runtime, resolving Command on PATH immediately so
// a missing install is reported at wiring time, not on the first task.
func New(cfg Config) (*Runtime, error) {
	if cfg.Name == "" {
		return nil, core.Invalid("name", "must be set")
	}
	if cfg.Command == "" {
		return nil, core.Invalid("command", "must name the claude executable, e.g. \"claude\"")
	}
	resolved, err := exec.LookPath(cfg.Command)
	if err != nil {
		return nil, fmt.Errorf("claude-code runtime %q: executable %q not found on PATH: %w",
			cfg.Name, cfg.Command, err)
	}

	return &Runtime{
		name:           cfg.Name,
		command:        resolved,
		defaultModel:   cfg.Model,
		permissionMode: cfg.PermissionMode,
		extraArgs:      cfg.ExtraArgs,
		newCommand: func(ctx context.Context, name string, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}, nil
}

var _ core.AgentRuntime = (*Runtime)(nil)

// Name implements core.AgentRuntime.
func (r *Runtime) Name() string { return r.name }

// Capabilities reports everything Claude Code can support when its tool gate
// permits it. The actual restriction to an agent's narrower permissions
// happens per-request via the allow/disallow tool lists, not here — a
// capability check compares this against what an agent requires, and a
// full-capability agent runtime supports any agent definition by
// construction.
func (r *Runtime) Capabilities() core.CapabilitySet {
	return core.NewCapabilitySet(
		core.CapabilityStreaming, core.CapabilityTools,
		core.CapabilityFilesystemRead, core.CapabilityFilesystemWrite,
		core.CapabilityShell, core.CapabilityNetwork,
		core.CapabilityStructuredOutput, core.CapabilitySessionResume,
		core.CapabilityCostReporting, core.CapabilityBudgetLimit,
	)
}

// Execute implements core.AgentRuntime.
func (r *Runtime) Execute(ctx context.Context, req core.RunRequest, sink core.EventSink) (*core.RunResult, error) {
	if req.Agent == nil {
		return nil, core.Invalid("req.agent", "must be set")
	}

	if req.Limits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Limits.Timeout)
		defer cancel()
	}

	args := buildArgs(req, buildOpts{
		model:          r.defaultModel,
		permissionMode: r.permissionMode,
		extraArgs:      r.extraArgs,
	})

	cmd := r.newCommand(ctx, r.command, args)
	if req.WorkspaceDir != "" {
		cmd.Dir = req.WorkspaceDir
	}
	cmd.Env = childEnv(r.extraEnv...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &core.RuntimeError{Runtime: r.name, Kind: core.ErrKindPermanent,
			Message: "attach stdout pipe", Err: err}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &boundedWriter{buf: &stderr, max: 64 * 1024}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, &core.RuntimeError{Runtime: r.name, Kind: core.ErrKindPermanent,
			Message: "start process", Err: err}
	}

	sink.Emit(ctx, core.Event{Type: core.EventTypeStarted, Text: "claude code started"})

	final, streamErr := streamReader(ctx, stdout, sink)
	waitErr := cmd.Wait()
	duration := time.Since(start)

	if ctx.Err() != nil {
		kind := core.ErrKindCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = core.ErrKindTimeout
		}
		return nil, &core.RuntimeError{
			Runtime: r.name, Kind: kind, ExitCode: exitCode(cmd),
			Message: fmt.Sprintf("run %s", ctx.Err()), Err: ctx.Err(),
		}
	}

	if waitErr != nil {
		return nil, &core.RuntimeError{
			Runtime: r.name, Kind: core.ErrKindPermanent, ExitCode: exitCode(cmd),
			Message: fmt.Sprintf("process exited: %s", redact(firstLine(stderr.String()))),
			Err:     waitErr,
		}
	}
	if streamErr != nil {
		return nil, &core.RuntimeError{Runtime: r.name, Kind: core.ErrKindPermanent,
			Message: "parse output stream", Err: streamErr}
	}
	if final == nil {
		return nil, &core.RuntimeError{Runtime: r.name, Kind: core.ErrKindPermanent,
			Message: "process exited 0 without emitting a result event"}
	}
	if final.IsError {
		return nil, &core.RuntimeError{
			Runtime: r.name, Kind: classifyAPIError(final.APIErrorStatus),
			Message: redact(final.Result),
		}
	}

	result := &core.RunResult{
		Text:       final.Result,
		SessionID:  final.SessionID,
		StopReason: final.StopReason,
		Duration:   duration,
		Usage: core.Usage{
			// The result line reports usage in aggregate; it does not name a
			// single model (Claude Code can invoke an internal helper model
			// alongside the primary one — observed directly while probing
			// this adapter). This records what was requested, which may not
			// account for every token in the total.
			Model:               effectiveModel(req, buildOpts{model: r.defaultModel}),
			InputTokens:         final.Usage.InputTokens,
			OutputTokens:        final.Usage.OutputTokens,
			CacheReadTokens:     final.Usage.CacheReadInputTokens,
			CacheCreationTokens: final.Usage.CacheCreationInputTokens,
			CostUSD:             final.TotalCostUSD,
		},
	}
	for _, d := range final.PermissionDenials {
		result.PermissionDenials = append(result.PermissionDenials, string(d))
	}
	if req.OutputSchema != nil {
		// Best-effort: --json-schema was exercised in the Phase 1 probe and
		// changed the model's stop_reason to tool_use rather than visibly
		// populating a structured field in the truncated sample captured
		// there. Passing the raw result text through here is a placeholder,
		// not a verified contract — treat structured output from this
		// runtime as unproven until it is empirically confirmed against a
		// full, untruncated response in Phase 5, when plan.json/qa-report.json
		// artifacts become load-bearing.
		result.Structured = []byte(final.Result)
	}
	return result, nil
}

// boundedWriter caps how much stderr this adapter retains, so a runaway
// process cannot exhaust memory through its error output.
type boundedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() < w.max {
		remaining := w.max - w.buf.Len()
		if len(p) > remaining {
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// classifyAPIError maps the result line's api_error_status, when present, to
// a RuntimeErrorKind so the retry policy does not have to string-match. The
// mapping follows ordinary HTTP status conventions; it is not itself a field
// whose meaning the CLI documents beyond being present and null on success in
// the Phase 1 probe.
func classifyAPIError(status *int) core.RuntimeErrorKind {
	if status == nil {
		return core.ErrKindPermanent
	}
	switch {
	case *status == 401 || *status == 403:
		return core.ErrKindAuth
	case *status == 429:
		return core.ErrKindRateLimit
	case *status >= 500:
		return core.ErrKindTransient
	default:
		return core.ErrKindPermanent
	}
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
