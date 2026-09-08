package runtimes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/config"
	"github.com/LuisRaLo/ai-squad/internal/core"
)

func TestBuildConstructsMockRuntime(t *testing.T) {
	t.Parallel()

	reg, err := Build(map[string]config.RuntimeConfig{
		"m": {Type: config.RuntimeTypeMock},
	}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rt, err := reg.Runtime("m")
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if rt.Name() != "m" {
		t.Errorf("expected name m, got %s", rt.Name())
	}
	if got := reg.Names(); len(got) != 1 || got[0] != "m" {
		t.Errorf("unexpected names: %v", got)
	}
}

func TestBuildRejectsMissingClaudeExecutable(t *testing.T) {
	t.Parallel()

	_, err := Build(map[string]config.RuntimeConfig{
		"claude": {Type: config.RuntimeTypeClaudeCode, Command: "definitely-not-a-real-binary-xyz"},
	}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing executable")
	}
}

func TestBuildRejectsModelRuntimeWithUnknownProvider(t *testing.T) {
	t.Parallel()

	_, err := Build(map[string]config.RuntimeConfig{
		"local": {Type: config.RuntimeTypeModel, Provider: "ollama"},
	}, nil)
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestBuildConstructsModelRuntimeOverOpenAICompatibleProvider(t *testing.T) {
	t.Parallel()

	reg, err := Build(
		map[string]config.RuntimeConfig{"cloud": {Type: config.RuntimeTypeModel, Provider: "deepseek"}},
		map[string]config.ProviderConfig{"deepseek": {
			Type: config.ProviderTypeOpenAICompatible, BaseURL: "https://api.deepseek.com/v1",
			Model: "deepseek-chat", APIKeyEnv: "DEEPSEEK_API_KEY",
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rt, err := reg.Runtime("cloud")
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if !rt.Capabilities().Has(core.CapabilityTools) {
		t.Error("a model runtime should declare tool support")
	}
}

func TestBuildConstructsModelRuntimeOverOllamaProvider(t *testing.T) {
	t.Parallel()

	reg, err := Build(
		map[string]config.RuntimeConfig{"local": {Type: config.RuntimeTypeModel, Provider: "ollama"}},
		map[string]config.ProviderConfig{"ollama": {
			Type: config.ProviderTypeOllama, BaseURL: "http://localhost:11434", Model: "qwen2.5-coder",
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := reg.Runtime("local"); err != nil {
		t.Fatalf("runtime: %v", err)
	}
}

func TestBuildRejectsModelRuntimeOverMockProvider(t *testing.T) {
	t.Parallel()

	_, err := Build(
		map[string]config.RuntimeConfig{"m": {Type: config.RuntimeTypeModel, Provider: "p"}},
		map[string]config.ProviderConfig{"p": {Type: config.ProviderTypeMock}},
	)
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestBuildRejectsUnknownType(t *testing.T) {
	t.Parallel()

	_, err := Build(map[string]config.RuntimeConfig{"x": {Type: "telepathy"}}, nil)
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestRuntimeUnknownNameListsKnownOnes(t *testing.T) {
	t.Parallel()

	reg, err := Build(map[string]config.RuntimeConfig{"m": {Type: config.RuntimeTypeMock}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := reg.Runtime("ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestNegotiateAcceptsCompatibleBinding(t *testing.T) {
	t.Parallel()

	reg, err := Build(map[string]config.RuntimeConfig{"m": {Type: config.RuntimeTypeMock}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	developer := &core.AgentDefinition{
		Name: "developer",
		Permissions: core.Permissions{
			Filesystem: core.FSWorkspace,
			Shell:      core.ShellPolicy{Enabled: true, Commands: []string{"go"}},
			GitWrite:   true,
		},
	}
	err = Negotiate([]*core.AgentDefinition{developer}, func(string) string { return "m" }, reg)
	if err != nil {
		t.Fatalf("a mock runtime declares every capability; expected no error, got %v", err)
	}
}

func TestNegotiateRejectsIncompatibleBinding(t *testing.T) {
	t.Parallel()

	// This is the scenario the design exists for: a shell-using agent bound
	// to a runtime that cannot support shell access must fail here, at load
	// time, with the missing capability named — not thirty minutes into a
	// task.
	reg := &Registry{byName: map[string]core.AgentRuntime{
		"bare": limitedRuntime{name: "bare", caps: core.NewCapabilitySet(core.CapabilityFilesystemRead)},
	}}

	developer := &core.AgentDefinition{
		Name: "developer",
		Permissions: core.Permissions{
			Filesystem: core.FSWorkspace,
			Shell:      core.ShellPolicy{Enabled: true},
			GitWrite:   true,
		},
	}

	err := Negotiate([]*core.AgentDefinition{developer}, func(string) string { return "bare" }, reg)
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	for _, want := range []string{"developer", "bare", "shell"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err)
		}
	}
}

func TestNegotiateRejectsUnknownRuntimeBinding(t *testing.T) {
	t.Parallel()

	reg, err := Build(map[string]config.RuntimeConfig{"m": {Type: config.RuntimeTypeMock}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	agent := &core.AgentDefinition{Name: "a", Permissions: core.Permissions{Filesystem: core.FSNone}}

	err = Negotiate([]*core.AgentDefinition{agent}, func(string) string { return "ghost" }, reg)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// limitedRuntime is a minimal core.AgentRuntime stub for negotiation tests
// that need a runtime with a deliberately narrow capability set, which the
// shared mock.Runtime does not offer by default.
type limitedRuntime struct {
	name string
	caps core.CapabilitySet
}

func (r limitedRuntime) Name() string                     { return r.name }
func (r limitedRuntime) Capabilities() core.CapabilitySet { return r.caps }
func (r limitedRuntime) Execute(context.Context, core.RunRequest, core.EventSink) (*core.RunResult, error) {
	panic("not used in this test")
}
