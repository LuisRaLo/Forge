package claudecode

import (
	"strings"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func TestToolArgsFilesystemNone(t *testing.T) {
	t.Parallel()
	_, disallowed := toolArgs(core.Permissions{Filesystem: core.FSNone})
	for _, want := range []string{toolRead, toolEdit, toolWrite, toolGlob, toolGrep} {
		if !contains(disallowed, want) {
			t.Errorf("FSNone should disallow %s, got %v", want, disallowed)
		}
	}
}

func TestToolArgsFilesystemRead(t *testing.T) {
	t.Parallel()
	_, disallowed := toolArgs(core.Permissions{Filesystem: core.FSRead})
	if contains(disallowed, toolRead) {
		t.Error("FSRead must still allow Read")
	}
	if !contains(disallowed, toolEdit) || !contains(disallowed, toolWrite) {
		t.Errorf("FSRead must disallow Edit/Write, got %v", disallowed)
	}
}

func TestToolArgsFilesystemWorkspace(t *testing.T) {
	t.Parallel()
	_, disallowed := toolArgs(core.Permissions{Filesystem: core.FSWorkspace})
	for _, forbidden := range []string{toolRead, toolEdit, toolWrite} {
		if contains(disallowed, forbidden) {
			t.Errorf("FSWorkspace should not disallow %s, got %v", forbidden, disallowed)
		}
	}
}

// TestToolArgsWorkspaceWithScopedShellStaysWritable is a regression test for
// a real, reproduced-live bug: --allowedTools is NOT additive — once it
// carries any entry, every tool not explicitly named becomes unavailable,
// overriding whatever would otherwise be available by default. A prior
// version of toolArgs added only the scoped Bash(cmd:*) patterns to
// `allowed` and assumed Edit/Write stayed available by omission; in
// practice this made every FSWorkspace agent with a scoped shell allowlist
// (developer, qa) unable to write anything at all, confirmed by running the
// exact resulting flags against the installed CLI (denials for both Write
// and Bash, "running in don't ask mode"). The old tests here only checked
// that Edit/Write were absent from `disallowed`, never that they were
// present in `allowed` once a scoped shell forced --allowedTools into use —
// which is the exact condition that broke.
func TestToolArgsWorkspaceWithScopedShellStaysWritable(t *testing.T) {
	t.Parallel()
	allowed, disallowed := toolArgs(core.Permissions{
		Filesystem: core.FSWorkspace,
		Shell:      core.ShellPolicy{Enabled: true, Commands: []string{"git"}},
	})
	for _, want := range []string{toolRead, toolEdit, toolWrite, toolNotebookEdit, toolGlob, toolGrep} {
		if !contains(allowed, want) {
			t.Errorf("a scoped shell must not implicitly exclude %s from --allowedTools, got %v", want, allowed)
		}
	}
	if contains(disallowed, toolEdit) || contains(disallowed, toolWrite) {
		t.Errorf("FSWorkspace must not disallow Edit/Write, got %v", disallowed)
	}
}

// TestToolArgsReadWithScopedShellStaysReadable mirrors the above for
// FSRead: a read-only agent that also has a scoped shell (e.g. qa running
// `go test`) must still be able to Read once --allowedTools is in play.
func TestToolArgsReadWithScopedShellStaysReadable(t *testing.T) {
	t.Parallel()
	allowed, _ := toolArgs(core.Permissions{
		Filesystem: core.FSRead,
		Shell:      core.ShellPolicy{Enabled: true, Commands: []string{"go"}},
	})
	for _, want := range []string{toolRead, toolGlob, toolGrep} {
		if !contains(allowed, want) {
			t.Errorf("a scoped shell must not implicitly exclude %s from --allowedTools, got %v", want, allowed)
		}
	}
}

// TestToolArgsNetworkWithScopedShellStaysReachable mirrors the same
// principle for network:true — an agent like devops needs WebFetch/WebSearch
// explicitly present in --allowedTools once its shell allowlist forces that
// flag into use, not merely absent from --disallowedTools.
func TestToolArgsNetworkWithScopedShellStaysReachable(t *testing.T) {
	t.Parallel()
	allowed, _ := toolArgs(core.Permissions{
		Network: true,
		Shell:   core.ShellPolicy{Enabled: true, Commands: []string{"git"}},
	})
	if !contains(allowed, toolWebFetch) || !contains(allowed, toolWebSearch) {
		t.Errorf("network:true with a scoped shell must include web tools in --allowedTools, got %v", allowed)
	}
}

func TestToolArgsShellDenied(t *testing.T) {
	t.Parallel()
	_, disallowed := toolArgs(core.Permissions{Shell: core.ShellPolicy{Enabled: false}})
	if !contains(disallowed, toolBash) {
		t.Error("disabled shell must disallow Bash")
	}
}

func TestToolArgsShellAllowlist(t *testing.T) {
	t.Parallel()
	allowed, disallowed := toolArgs(core.Permissions{
		Shell: core.ShellPolicy{Enabled: true, Commands: []string{"go", "git"}},
	})
	if contains(disallowed, toolBash) {
		t.Error("an allowlisted shell must not blanket-disallow Bash")
	}
	if !contains(allowed, "Bash(go:*)") || !contains(allowed, "Bash(git:*)") {
		t.Errorf("expected scoped Bash patterns, got %v", allowed)
	}
}

func TestToolArgsShellUnrestricted(t *testing.T) {
	t.Parallel()
	allowed, disallowed := toolArgs(core.Permissions{Shell: core.ShellPolicy{Enabled: true}})
	if contains(disallowed, toolBash) {
		t.Error("an unrestricted shell must not disallow Bash")
	}
	if len(allowed) != 0 {
		t.Errorf("an unrestricted shell needs no scoped patterns, got %v", allowed)
	}
}

func TestToolArgsNetwork(t *testing.T) {
	t.Parallel()

	_, disallowed := toolArgs(core.Permissions{Network: false})
	if !contains(disallowed, toolWebFetch) || !contains(disallowed, toolWebSearch) {
		t.Errorf("network:false must disallow web tools, got %v", disallowed)
	}

	_, disallowed = toolArgs(core.Permissions{Network: true})
	if contains(disallowed, toolWebFetch) || contains(disallowed, toolWebSearch) {
		t.Errorf("network:true must not disallow web tools, got %v", disallowed)
	}
}

func TestToolArgsMatchesShippedAgentDefinitions(t *testing.T) {
	t.Parallel()

	// The reviewer ships as read-only, no shell, no network: it must not be
	// able to touch the filesystem or run anything.
	_, disallowed := toolArgs(core.Permissions{Filesystem: core.FSRead})
	for _, want := range []string{toolEdit, toolWrite} {
		if !contains(disallowed, want) {
			t.Errorf("reviewer-shaped permissions must disallow %s", want)
		}
	}

	// The developer ships with workspace write, a shell allowlist and no
	// network: git and go must be reachable, curl must not exist as a bare
	// Bash grant, and web tools must be off. Critically, Edit/Write must be
	// in the ALLOWED list, not merely absent from disallowed — see
	// TestToolArgsWorkspaceWithScopedShellStaysWritable for why that
	// distinction is the one that actually matters.
	allowed, disallowed := toolArgs(core.Permissions{
		Filesystem: core.FSWorkspace,
		Shell:      core.ShellPolicy{Enabled: true, Commands: []string{"go", "git", "make"}},
		Network:    false,
	})
	if !contains(allowed, toolEdit) || !contains(allowed, toolWrite) {
		t.Errorf("developer-shaped permissions must explicitly allow Edit/Write, got %v", allowed)
	}
	if !contains(allowed, "Bash(git:*)") {
		t.Error("developer-shaped permissions must scope Bash to git")
	}
	if !contains(disallowed, toolWebFetch) {
		t.Error("developer-shaped permissions must disallow WebFetch")
	}
}

func TestBuildArgsPromptImmediatelyFollowsPrintFlag(t *testing.T) {
	t.Parallel()
	// --allowedTools/--disallowedTools are variadic and greedily consume
	// trailing bare tokens; a prompt placed after them (or after any other
	// variadic flag) would be silently swallowed into the tool list. Anchor
	// it right after "-p" instead, which is safe regardless of what other
	// flags are present or how many values they take.
	args := buildArgs(core.RunRequest{
		Agent: &core.AgentDefinition{Permissions: core.Permissions{
			Shell: core.ShellPolicy{Enabled: false}, Network: false,
		}},
		Prompt: "do the thing",
	}, buildOpts{})

	idx := indexOfArg(args, "-p")
	if idx < 0 || idx+1 >= len(args) || args[idx+1] != "do the thing" {
		t.Fatalf("prompt must immediately follow -p, got %v", args)
	}
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func TestBuildArgsModelPrefersAgentOverride(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{Model: "opus"}}, buildOpts{model: "sonnet"})
	if !containsPair(args, "--model", "opus") {
		t.Errorf("agent-level model must win over the runtime default, got %v", args)
	}
}

func TestBuildArgsFallsBackToRuntimeModel(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{}}, buildOpts{model: "sonnet"})
	if !containsPair(args, "--model", "sonnet") {
		t.Errorf("expected the runtime default model, got %v", args)
	}
}

func TestBuildArgsOmitsMissingModel(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{}}, buildOpts{})
	if contains(args, "--model") {
		t.Errorf("no model configured anywhere: --model must be omitted, got %v", args)
	}
}

func TestBuildArgsAlwaysNonInteractiveAndStreamed(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{}}, buildOpts{})
	if !contains(args, "-p") {
		t.Error("must always run non-interactively")
	}
	if !containsPair(args, "--output-format", "stream-json") {
		t.Error("must always stream so the event sink receives events")
	}
	if !contains(args, "--permission-mode") {
		t.Error("must always set a permission mode explicitly")
	}
}

func TestBuildArgsBudgetLimit(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{
		Agent:  &core.AgentDefinition{},
		Limits: core.Limits{MaxCostUSD: 2.5},
	}, buildOpts{})
	if !containsPair(args, "--max-budget-usd", "2.5000") {
		t.Errorf("expected the cost limit passed through, got %v", args)
	}
}

func TestBuildArgsOmitsBudgetWhenUnset(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{}}, buildOpts{})
	if contains(args, "--max-budget-usd") {
		t.Errorf("no budget configured: --max-budget-usd must be omitted, got %v", args)
	}
}

func TestBuildArgsSessionResume(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{}, SessionID: "sess-1"}, buildOpts{})
	if !containsPair(args, "--session-id", "sess-1") {
		t.Errorf("expected the session id passed through, got %v", args)
	}
}

func TestBuildArgsExtraArgsDoNotDisplacePrompt(t *testing.T) {
	t.Parallel()
	args := buildArgs(core.RunRequest{Agent: &core.AgentDefinition{}, Prompt: "p"},
		buildOpts{extraArgs: []string{"--add-dir", "/extra"}})
	if !containsPair(args, "--add-dir", "/extra") {
		t.Errorf("expected extra args included, got %v", args)
	}
	idx := indexOfArg(args, "-p")
	if idx < 0 || args[idx+1] != "p" {
		t.Errorf("prompt must still immediately follow -p, got %v", args)
	}
}

func TestRedactStripsCommonSecretShapes(t *testing.T) {
	t.Parallel()
	in := "auth failed with api_key=sk-abcdefghij1234567890 and Bearer zzzYYYxxxWWWvvv123"
	out := redact(in)
	if strings.Contains(out, "sk-abcdefghij1234567890") || strings.Contains(out, "zzzYYYxxxWWWvvv123") {
		t.Errorf("secret survived redaction: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected a redaction marker, got %q", out)
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	t.Parallel()
	in := "tests passed: 42 ok, 0 failed"
	if got := redact(in); got != in {
		t.Errorf("redact must not touch ordinary text, got %q", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
