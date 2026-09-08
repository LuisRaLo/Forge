package claudecode

import (
	"fmt"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Built-in Claude Code tool names relevant to permission mapping, verified
// against `claude --help` on the version probed for this adapter (2.1.236).
// Tools that touch neither filesystem, shell nor network (Task, TodoWrite,
// ...) are left alone: they carry no permission concern of their own.
const (
	toolRead         = "Read"
	toolEdit         = "Edit"
	toolWrite        = "Write"
	toolNotebookEdit = "NotebookEdit"
	toolGlob         = "Glob"
	toolGrep         = "Grep"
	toolBash         = "Bash"
	toolWebFetch     = "WebFetch"
	toolWebSearch    = "WebSearch"
)

// defaultPermissionMode is used unless a runtime config overrides it.
//
// "dontAsk" was chosen over "bypassPermissions" deliberately: the CLI's own
// help text recommends bypassPermissions "only for sandboxes with no internet
// access", because it skips every permission check outright. "dontAsk" avoids
// blocking on an interactive prompt — required for an unattended
// orchestrator — while still leaving --allowedTools/--disallowedTools as the
// enforcement point. This was exercised successfully against the installed
// CLI while probing its output format; its precise enforcement semantics
// beyond that were not independently re-verified here and should be
// confirmed against Claude Code's own docs before this mapping is trusted as
// a hard security boundary. See the package doc comment.
const defaultPermissionMode = "dontAsk"

// buildOpts carries the runtime-level defaults that fill in what a request
// does not specify.
type buildOpts struct {
	model          string
	permissionMode string
	extraArgs      []string
}

// buildArgs assembles the claude CLI argument list for one request.
//
// The prompt goes immediately after "-p", not at the end. --allowedTools and
// --disallowedTools are variadic (`<tools...>` per --help) and greedily
// consume every following bare token until the next flag; a prompt placed
// after them is silently swallowed into the tool list and the CLI then
// reports "Input must be provided either through stdin or as a prompt
// argument" — reproduced against the real installed CLI while wiring this
// adapter, not a hypothetical.
func buildArgs(req core.RunRequest, opts buildOpts) []string {
	args := []string{
		"-p", req.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", firstNonEmpty(opts.permissionMode, defaultPermissionMode),
		// Without this, the child process picks up whatever CLAUDE.md,
		// plugins, hooks and custom agents happen to be configured for the
		// invoking user/workspace — reproduced directly while wiring this
		// adapter: a run granted only "Bash(echo:*)" saw no Bash tool at
		// all, and instead had an unrelated custom tool (ToolSearch)
		// available, because the workspace's own local configuration leaked
		// in. --safe-mode disables that surface while explicitly leaving
		// auth, model selection, built-in tools and permissions untouched
		// (per --help), so OAuth/keychain login keeps working — --bare would
		// additionally force ANTHROPIC_API_KEY and break that.
		"--safe-mode",
	}

	model := effectiveModel(req, opts)
	if model != "" {
		args = append(args, "--model", model)
	}

	if req.SystemPrompt != "" {
		args = append(args, "--system-prompt", req.SystemPrompt)
	}
	if req.SessionID != "" {
		args = append(args, "--session-id", req.SessionID)
	}
	if req.OutputSchema != nil {
		args = append(args, "--json-schema", string(req.OutputSchema))
	}
	if req.Limits.MaxCostUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.4f", req.Limits.MaxCostUSD))
	}

	allowed, disallowed := toolArgs(req.Permissions)
	if len(allowed) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, allowed...)
	}
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, disallowed...)
	}

	return append(args, opts.extraArgs...)
}

// toolArgs derives allowed/disallowed Claude Code tool names from an agent's
// permissions. This is a policy layer on top of the CLI's own gate, not a
// hard sandbox — see the package doc comment in runtime.go.
// toolArgs derives allowed/disallowed Claude Code tool names from an agent's
// permissions.
//
// --allowedTools is NOT additive: reproduced directly against the installed
// CLI while debugging a real failure — a developer-shaped agent (workspace
// filesystem, a scoped shell allowlist) had Write and Bash both denied with
// "Claude Code is running in don't ask mode," even though neither was ever
// named in --disallowedTools, because --allowedTools had been given ONLY the
// scoped Bash(cmd:*) patterns. Once --allowedTools carries any entry at all,
// every built-in tool not explicitly listed becomes unavailable, overriding
// whatever would otherwise be available by default. The old version of this
// function only ever added Bash(cmd:*) patterns to allowed and assumed
// Read/Edit/Write stayed available by omission — that assumption was wrong,
// and it broke every agent with both FSWorkspace and a scoped shell list
// (developer, qa — both shipped agents that need to write files).
//
// The fix: whenever a scoped shell allowlist forces --allowedTools to be
// used at all, every OTHER tool this permission set grants is added to it
// explicitly too, so nothing is implicitly excluded. The flip side, and a
// deliberate one: a built-in tool this permission model has no opinion
// about (something outside filesystem/shell/network) is then also
// unavailable — fail closed, consistent with this project's own security
// stance, rather than silently falling through to whatever Claude Code
// considers "default."
func toolArgs(p core.Permissions) (allowed, disallowed []string) {
	usesAllowlist := len(p.Shell.Commands) > 0

	switch p.Filesystem {
	case core.FSNone, "":
		disallowed = append(disallowed, toolRead, toolEdit, toolWrite, toolNotebookEdit, toolGlob, toolGrep)
	case core.FSRead:
		if usesAllowlist {
			allowed = append(allowed, toolRead, toolGlob, toolGrep)
		}
		disallowed = append(disallowed, toolEdit, toolWrite, toolNotebookEdit)
	case core.FSWorkspace:
		if usesAllowlist {
			allowed = append(allowed, toolRead, toolEdit, toolWrite, toolNotebookEdit, toolGlob, toolGrep)
		}
	}

	switch {
	case !p.Shell.Enabled:
		disallowed = append(disallowed, toolBash)
	case len(p.Shell.Commands) > 0:
		for _, c := range p.Shell.Commands {
			allowed = append(allowed, fmt.Sprintf("Bash(%s:*)", c))
		}
	}

	if !p.Network {
		disallowed = append(disallowed, toolWebFetch, toolWebSearch)
	} else if usesAllowlist {
		allowed = append(allowed, toolWebFetch, toolWebSearch)
	}

	return allowed, disallowed
}

// effectiveModel resolves the model an agent-level override or the runtime
// default implies. An agent's own Model wins; falling entirely through
// means the CLI uses its own configured default.
func effectiveModel(req core.RunRequest, opts buildOpts) string {
	if req.Agent != nil && req.Agent.Model != "" {
		return req.Agent.Model
	}
	return opts.model
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
