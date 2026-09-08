package core

import (
	"errors"
	"testing"
	"time"
)

func TestPermissionsFailClosed(t *testing.T) {
	t.Parallel()

	// The zero value must be rejected rather than silently granting nothing
	// under a name that looks configured.
	var p Permissions
	if err := p.Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero permissions must be invalid, got %v", err)
	}

	// A shell with no filesystem is contradictory.
	p = Permissions{Filesystem: FSNone, Shell: ShellPolicy{Enabled: true}}
	if err := p.Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("shell without filesystem must be rejected, got %v", err)
	}

	// Committing requires write access to the worktree.
	p = Permissions{Filesystem: FSRead, GitWrite: true}
	if err := p.Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("git_write with read-only filesystem must be rejected, got %v", err)
	}

	p = Permissions{Filesystem: FSWorkspace, GitWrite: true, Shell: ShellPolicy{Enabled: true}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid permissions rejected: %v", err)
	}
}

func TestShellPolicyAllows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  ShellPolicy
		command string
		want    bool
	}{
		{"disabled denies everything", ShellPolicy{}, "git", false},
		{"disabled denies even listed", ShellPolicy{Commands: []string{"git"}}, "git", false},
		{"enabled with no list allows any", ShellPolicy{Enabled: true}, "rm", true},
		{"allowlist permits listed", ShellPolicy{Enabled: true, Commands: []string{"go", "git"}}, "go", true},
		{"allowlist denies unlisted", ShellPolicy{Enabled: true, Commands: []string{"go", "git"}}, "curl", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Allows(tc.command); got != tc.want {
				t.Errorf("Allows(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

func TestAgentDefinitionValidate(t *testing.T) {
	t.Parallel()

	valid := func() *AgentDefinition {
		return &AgentDefinition{
			Name:         "developer",
			Runtime:      "claude",
			SystemPrompt: "You are an engineer.",
			Permissions:  Permissions{Filesystem: FSWorkspace},
		}
	}

	if err := valid().Validate(); err != nil {
		t.Fatalf("valid agent rejected: %v", err)
	}

	tests := map[string]func(*AgentDefinition){
		"missing name":          func(a *AgentDefinition) { a.Name = "" },
		"missing runtime":       func(a *AgentDefinition) { a.Runtime = "" },
		"missing system prompt": func(a *AgentDefinition) { a.SystemPrompt = "  " },
		"negative timeout":      func(a *AgentDefinition) { a.Limits.Timeout = -time.Second },
		"negative cost":         func(a *AgentDefinition) { a.Limits.MaxCostUSD = -1 },
		"negative attempts":     func(a *AgentDefinition) { a.Limits.MaxAttempts = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			a := valid()
			mutate(a)
			if err := a.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestRequiredCapabilities(t *testing.T) {
	t.Parallel()

	t.Run("read-only reviewer needs only read", func(t *testing.T) {
		a := &AgentDefinition{Permissions: Permissions{Filesystem: FSRead}}
		caps := a.RequiredCapabilities()
		if !caps.Has(CapabilityFilesystemRead) {
			t.Error("expected filesystem_read")
		}
		if caps.Has(CapabilityFilesystemWrite) || caps.Has(CapabilityShell) {
			t.Errorf("reviewer must not require write or shell, got %v", caps.List())
		}
	})

	t.Run("developer needs write, shell and tools", func(t *testing.T) {
		a := &AgentDefinition{Permissions: Permissions{
			Filesystem: FSWorkspace,
			Shell:      ShellPolicy{Enabled: true, Commands: []string{"go"}},
			GitWrite:   true,
		}}
		caps := a.RequiredCapabilities()
		for _, want := range []Capability{
			CapabilityFilesystemRead, CapabilityFilesystemWrite,
			CapabilityShell, CapabilityTools,
		} {
			if !caps.Has(want) {
				t.Errorf("expected %s in %v", want, caps.List())
			}
		}
	})

	t.Run("cost limit requires cost reporting", func(t *testing.T) {
		a := &AgentDefinition{
			Permissions: Permissions{Filesystem: FSNone},
			Limits:      Limits{MaxCostUSD: 5},
		}
		if !a.RequiredCapabilities().Has(CapabilityCostReporting) {
			t.Error("a per-step cost limit is unenforceable without cost reporting")
		}
	})
}

func TestCapabilitySetMissing(t *testing.T) {
	t.Parallel()

	// This is the check that rejects binding a shell-using agent to a bare
	// completion API before any task runs.
	runtime := NewCapabilitySet(CapabilityStreaming, CapabilityFilesystemRead)
	agent := NewCapabilitySet(CapabilityFilesystemRead, CapabilityShell, CapabilityFilesystemWrite)

	missing := runtime.Missing(agent)
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing capabilities, got %v", missing)
	}
	if missing[0] != CapabilityFilesystemWrite || missing[1] != CapabilityShell {
		t.Errorf("Missing must be sorted and accurate, got %v", missing)
	}

	if got := runtime.Missing(NewCapabilitySet(CapabilityStreaming)); len(got) != 0 {
		t.Errorf("expected nothing missing, got %v", got)
	}
}

func TestRetryPolicyBackoffFor(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{Backoff: time.Second, Multiplier: 2, MaxBackoff: 5 * time.Second}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 0},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 5 * time.Second}, // capped
		{9, 5 * time.Second}, // stays capped
	}
	for _, tc := range tests {
		if got := p.BackoffFor(tc.attempt); got != tc.want {
			t.Errorf("BackoffFor(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}

	t.Run("zero backoff disables waiting", func(t *testing.T) {
		if got := (RetryPolicy{}).BackoffFor(3); got != 0 {
			t.Errorf("expected 0, got %s", got)
		}
	})
}
