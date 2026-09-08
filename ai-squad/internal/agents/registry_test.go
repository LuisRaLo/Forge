package agents

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

const developerYAML = `
name: developer
description: Software engineer
runtime: claude
system_prompt: |
  You are a senior software engineer.
permissions:
  filesystem: workspace
  shell:
    - go
    - git
  network: false
  git_write: true
limits:
  max_attempts: 3
  timeout: 30m
retry:
  max_attempts: 3
  backoff: 15s
  multiplier: 2
  max_backoff: 5m
`

const reviewerYAML = `
name: reviewer
runtime: claude
system_prompt: You review code.
permissions:
  filesystem: read
  shell: false
`

func TestLoadFS(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"developer.yaml": {Data: []byte(developerYAML)},
		"reviewer.yml":   {Data: []byte(reviewerYAML)},
		"README.md":      {Data: []byte("not an agent")},
	}

	reg, err := LoadFS(fsys, ".")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(list))
	}
	if list[0].Name != "developer" || list[1].Name != "reviewer" {
		t.Errorf("List must be sorted by name, got %s, %s", list[0].Name, list[1].Name)
	}

	dev, err := reg.Get("developer")
	if err != nil {
		t.Fatalf("get developer: %v", err)
	}
	if dev.Runtime != "claude" {
		t.Errorf("expected runtime claude, got %q", dev.Runtime)
	}
	if !dev.Permissions.Shell.Enabled {
		t.Error("a shell list must enable the shell")
	}
	if !dev.Permissions.Shell.Allows("go") || dev.Permissions.Shell.Allows("curl") {
		t.Errorf("shell allowlist not applied: %v", dev.Permissions.Shell.Commands)
	}
	if dev.Limits.Timeout.String() != "30m0s" {
		t.Errorf("expected 30m timeout, got %s", dev.Limits.Timeout)
	}
	if dev.Retry.MaxBackoff.String() != "5m0s" {
		t.Errorf("expected 5m max backoff, got %s", dev.Retry.MaxBackoff)
	}
	if !strings.HasPrefix(dev.SystemPrompt, "You are a senior") {
		t.Errorf("system prompt not loaded: %q", dev.SystemPrompt)
	}
}

func TestShellPolicyAcceptsThreeForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		shell       string
		wantEnabled bool
		wantCount   int
	}{
		{"false denies", "shell: false", false, 0},
		{"true allows any", "shell: true", true, 0},
		{"list allowlists", "shell: [go, git]", true, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := "name: a\nruntime: r\nsystem_prompt: p\npermissions:\n  filesystem: workspace\n  " + tc.shell + "\n"
			def, err := parse([]byte(body), "a")
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if def.Permissions.Shell.Enabled != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", def.Permissions.Shell.Enabled, tc.wantEnabled)
			}
			if len(def.Permissions.Shell.Commands) != tc.wantCount {
				t.Errorf("commands = %v, want %d entries", def.Permissions.Shell.Commands, tc.wantCount)
			}
		})
	}
}

func TestShellRejectsNonsense(t *testing.T) {
	t.Parallel()

	body := "name: a\nruntime: r\nsystem_prompt: p\npermissions:\n  filesystem: workspace\n  shell: {a: 1}\n"
	if _, err := parse([]byte(body), "a"); err == nil {
		t.Fatal("a mapping is not a valid shell policy")
	}
}

func TestParseAcceptsProviderAlias(t *testing.T) {
	t.Parallel()

	body := "name: a\nprovider: claude\nsystem_prompt: p\npermissions:\n  filesystem: read\n"
	def, err := parse([]byte(body), "a")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if def.Runtime != "claude" {
		t.Errorf("provider should alias runtime, got %q", def.Runtime)
	}
}

func TestParseNameDefaultsToFilename(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"qa.yaml": {Data: []byte("runtime: claude\nsystem_prompt: p\npermissions:\n  filesystem: read\n")},
	}
	reg, err := LoadFS(fsys, ".")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if _, err := reg.Get("qa"); err != nil {
		t.Errorf("agent should be named after its file: %v", err)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	body := "name: a\nruntime: r\nsystem_prompt: p\nsytem_prompt: typo\npermissions:\n  filesystem: read\n"
	err := func() error {
		_, err := parse([]byte(body), "a")
		return err
	}()
	if err == nil {
		t.Fatal("a misspelled field must fail rather than be ignored")
	}
}

func TestParseRejectsInvalidPermissions(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unknown filesystem level": "name: a\nruntime: r\nsystem_prompt: p\npermissions:\n  filesystem: everything\n",
		"git write without write":  "name: a\nruntime: r\nsystem_prompt: p\npermissions:\n  filesystem: read\n  git_write: true\n",
		"missing runtime":          "name: a\nsystem_prompt: p\npermissions:\n  filesystem: read\n",
		"missing system prompt":    "name: a\nruntime: r\npermissions:\n  filesystem: read\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parse([]byte(body), "a"); !errors.Is(err, core.ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestPermissionsDefaultToClosed(t *testing.T) {
	t.Parallel()

	// Omitting the permissions block entirely must yield no access, not
	// unrestricted access.
	body := "name: a\nruntime: r\nsystem_prompt: p\n"
	def, err := parse([]byte(body), "a")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if def.Permissions.Filesystem != core.FSNone {
		t.Errorf("filesystem should default to none, got %s", def.Permissions.Filesystem)
	}
	if def.Permissions.Shell.Enabled || def.Permissions.Network || def.Permissions.GitWrite {
		t.Errorf("all permissions should default to denied, got %+v", def.Permissions)
	}
}

func TestRegistryGetUnknownListsKnownAgents(t *testing.T) {
	t.Parallel()

	reg, err := LoadFS(fstest.MapFS{"developer.yaml": {Data: []byte(developerYAML)}}, ".")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	_, err = reg.Get("ghost")
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "developer") {
		t.Errorf("error should list known agents to be actionable, got %q", err)
	}
}

func TestNewRegistryRejectsDuplicates(t *testing.T) {
	t.Parallel()

	a := &core.AgentDefinition{Name: "dup", Runtime: "r", SystemPrompt: "p"}
	if _, err := NewRegistry(a, a); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestLoadFSRejectsEmptyDirectory(t *testing.T) {
	t.Parallel()

	if _, err := LoadFS(fstest.MapFS{}, "."); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLoadDirRejectsMissingDirectory(t *testing.T) {
	t.Parallel()

	if _, err := LoadDir(t.TempDir() + "/absent"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
