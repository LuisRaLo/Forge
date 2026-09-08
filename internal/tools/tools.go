// Package tools implements the small, permission-gated tool set a
// model-driven runtime (internal/runtimes/model) offers a plain completion
// API that has no tool loop of its own. This is orchestration of existing
// capability, not an attempt to build model intelligence: the tools
// themselves are mechanical, and every one is checked against
// core.Permissions before it can touch anything.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Set builds the tools a model-driven runtime exposes for the given
// permissions. A denied capability simply does not appear in the set,
// rather than appearing and refusing at call time — the model is never
// offered a tool it cannot use.
func Set(perms core.Permissions) []core.Tool {
	var out []core.Tool
	switch perms.Filesystem {
	case core.FSRead:
		out = append(out, readTool{})
	case core.FSWorkspace:
		out = append(out, readTool{}, writeTool{})
	}
	if perms.Shell.Enabled {
		out = append(out, bashTool{policy: perms.Shell})
	}
	return out
}

// resolveInWorkspace joins path under workspaceDir and refuses to escape it,
// so a tool call can never reach outside the task's isolated worktree — the
// same boundary rule the security policy applies everywhere else.
func resolveInWorkspace(workspaceDir, path string) (string, error) {
	if workspaceDir == "" {
		return "", fmt.Errorf("no workspace directory configured")
	}
	joined := filepath.Join(workspaceDir, path)
	rel, err := filepath.Rel(workspaceDir, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", path)
	}
	return joined, nil
}

type readInput struct {
	Path string `json:"path"`
}

type readTool struct{}

func (readTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "read_file",
		Description: "Read a UTF-8 text file inside the workspace.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
	}
}

func (readTool) Invoke(_ context.Context, workspaceDir string, raw json.RawMessage) (core.ToolResult, error) {
	var in readInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return core.ToolResult{IsError: true, Content: "invalid input: " + err.Error()}, nil
	}
	path, err := resolveInWorkspace(workspaceDir, in.Path)
	if err != nil {
		return core.ToolResult{IsError: true, Content: err.Error()}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return core.ToolResult{IsError: true, Content: err.Error()}, nil
	}
	return core.ToolResult{Content: string(data)}, nil
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeTool struct{}

func (writeTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "write_file",
		Description: "Write (creating or overwriting) a UTF-8 text file inside the workspace.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
	}
}

func (writeTool) Invoke(_ context.Context, workspaceDir string, raw json.RawMessage) (core.ToolResult, error) {
	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return core.ToolResult{IsError: true, Content: "invalid input: " + err.Error()}, nil
	}
	path, err := resolveInWorkspace(workspaceDir, in.Path)
	if err != nil {
		return core.ToolResult{IsError: true, Content: err.Error()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return core.ToolResult{IsError: true, Content: err.Error()}, nil
	}
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return core.ToolResult{IsError: true, Content: err.Error()}, nil
	}
	return core.ToolResult{Content: "wrote " + in.Path}, nil
}

type bashInput struct {
	Command string `json:"command"`
}

// bashTool runs a shell command via `sh -c`, gated by the agent's shell
// allowlist. Argument-level parsing of the command string to check the
// leading executable name against the allowlist mirrors the same
// allowlisting concept used for the Claude Code adapter's --allowedTools
// mapping (internal/runtimes/claudecode/args.go), applied here at the tool
// layer since a model-driven runtime has no equivalent CLI-level gate.
type bashTool struct{ policy core.ShellPolicy }

func (bashTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "run_shell",
		Description: "Run a shell command inside the workspace and return its combined output.",
		InputSchema: json.RawMessage(`{"type":"object","required":["command"],"properties":{"command":{"type":"string"}}}`),
	}
}

func (t bashTool) Invoke(ctx context.Context, workspaceDir string, raw json.RawMessage) (core.ToolResult, error) {
	var in bashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return core.ToolResult{IsError: true, Content: "invalid input: " + err.Error()}, nil
	}
	fields := strings.Fields(in.Command)
	if len(fields) == 0 {
		return core.ToolResult{IsError: true, Content: "empty command"}, nil
	}
	if !t.policy.Allows(fields[0]) {
		return core.ToolResult{IsError: true, Content: fmt.Sprintf("command %q is not permitted", fields[0])}, nil
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
	cmd.Dir = workspaceDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return core.ToolResult{IsError: true, Content: string(out) + "\n" + err.Error()}, nil
	}
	return core.ToolResult{Content: string(out)}, nil
}
