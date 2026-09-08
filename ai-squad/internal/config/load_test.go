package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santillana/ai-squad/internal/core"
)

const minimalConfig = `
runtimes:
  claude:
    type: claude-code
    command: claude
agents:
  developer:
    runtime: claude
workflows:
  feature:
    steps: [developer]
`

func TestParseAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if cfg.Scheduler.MaxConcurrency != 3 {
		t.Errorf("expected default concurrency 3, got %d", cfg.Scheduler.MaxConcurrency)
	}
	if cfg.Scheduler.PollInterval.Duration() != 5*time.Second {
		t.Errorf("expected default poll interval 5s, got %s", cfg.Scheduler.PollInterval.Duration())
	}
	if cfg.Limits.MaxTaskAttempts != 3 {
		t.Errorf("expected default max attempts 3, got %d", cfg.Limits.MaxTaskAttempts)
	}
	if !cfg.Approval.RequireForProduction {
		t.Error("production approval must default to required")
	}
	if !filepath.IsAbs(cfg.System.DataDir) {
		t.Errorf("data dir must be absolute, got %s", cfg.System.DataDir)
	}
	if cfg.System.AgentsDir != filepath.Join(cfg.System.DataDir, "agents") {
		t.Errorf("agents dir should derive from data dir, got %s", cfg.System.AgentsDir)
	}
	if cfg.DatabasePath() != filepath.Join(cfg.System.DataDir, "ai-squad.db") {
		t.Errorf("unexpected database path %s", cfg.DatabasePath())
	}
}

func TestParseRejectsInlineAPIKey(t *testing.T) {
	t.Parallel()

	// Secrets must never live in a file the orchestrator reads, logs or
	// copies. This is a hard failure, not a warning.
	const withSecret = `
runtimes:
  cloud:
    type: model
    provider: deepseek
providers:
  deepseek:
    type: openai-compatible
    base_url: https://api.deepseek.com/v1
    model: deepseek-chat
    api_key: sk-do-not-do-this
`
	_, err := Parse([]byte(withSecret))
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("error should point at the fix, got %q", err)
	}
	if strings.Contains(err.Error(), "sk-do-not-do-this") {
		t.Fatal("the error message must not echo the secret")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	// A typo must fail loudly rather than being silently ignored.
	_, err := Parse([]byte("scheduler:\n  max_concurency: 4\n"))
	if err == nil {
		t.Fatal("expected an error for a misspelled field")
	}
	if !strings.Contains(err.Error(), "max_concurency") {
		t.Errorf("error should name the offending field, got %q", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"zero concurrency":            "scheduler:\n  max_concurrency: 0\n",
		"negative poll":               "scheduler:\n  poll_interval: -1s\n",
		"zero attempts":               "limits:\n  max_task_attempts: 0\n",
		"zero iterations":             "limits:\n  max_step_iterations: 0\n",
		"negative cost":               "limits:\n  max_daily_cost_usd: -1\n",
		"approval off":                "approval:\n  require_for_production: false\n",
		"bad log level":               "system:\n  log_level: chatty\n",
		"bad log format":              "system:\n  log_format: xml\n",
		"unknown runtime type":        "runtimes:\n  x:\n    type: telepathy\n",
		"claude without command":      "runtimes:\n  claude:\n    type: claude-code\n",
		"model without provider":      "runtimes:\n  m:\n    type: model\n",
		"model with unknown provider": "runtimes:\n  m:\n    type: model\n    provider: ghost\n",
		"agent on unknown runtime":    "agents:\n  dev:\n    runtime: ghost\n",
		"empty workflow":              "workflows:\n  feature:\n    steps: []\n",
		"provider without base url":   "providers:\n  o:\n    type: ollama\n    model: m\n",
		"openai provider without key env": "providers:\n  o:\n    type: openai-compatible\n" +
			"    base_url: https://api.example.com/v1\n    model: m\n",
		"non http base url": "providers:\n  o:\n    type: ollama\n" +
			"    base_url: ftp://localhost\n    model: m\n",
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); !errors.Is(err, core.ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestClaudeCodeRuntimeRejectsProvider(t *testing.T) {
	t.Parallel()

	// A full agent runtime owns its own model; pointing it at a completion
	// provider is a category error and is reported as one.
	body := "runtimes:\n  claude:\n    type: claude-code\n    command: claude\n    provider: ollama\n"
	_, err := Parse([]byte(body))
	if !errors.Is(err, core.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestProviderAliasAndRuntimeName(t *testing.T) {
	t.Parallel()

	// "provider:" is accepted as a deprecated alias for "runtime:".
	body := minimalConfig + "\nagents:\n  qa:\n    provider: claude\n"
	cfg, err := Parse([]byte(strings.Replace(body, "agents:\n  developer:\n    runtime: claude\n", "", 1)))
	if err != nil {
		t.Fatalf("alias should still load: %v", err)
	}
	if got := cfg.Agents["qa"].RuntimeName(); got != "claude" {
		t.Errorf("expected runtime claude via alias, got %q", got)
	}
}

func TestWorkflowLookup(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	wf, err := cfg.Workflow("feature")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Name != "feature" {
		t.Errorf("workflow name should default to its key, got %q", wf.Name)
	}
	if len(wf.Steps) != 1 || wf.Steps[0] != "developer" {
		t.Errorf("unexpected steps %v", wf.Steps)
	}

	if _, err := cfg.Workflow("ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestParseEmptyDocumentYieldsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("an empty configuration should be usable: %v", err)
	}
	if cfg.Scheduler.MaxConcurrency != 3 {
		t.Errorf("expected defaults, got %+v", cfg.Scheduler)
	}
}

func TestExpandPath(t *testing.T) {
	t.Parallel()

	got, err := ExpandPath("~/x/y")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.HasPrefix(got, "~") || !filepath.IsAbs(got) {
		t.Errorf("~ should expand to an absolute path, got %s", got)
	}

	if empty, err := ExpandPath("  "); err != nil || empty != "" {
		t.Errorf("blank input should stay blank, got %q %v", empty, err)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected an error for a missing configuration file")
	}
}

func TestDurationAcceptsStringAndSeconds(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte("scheduler:\n  poll_interval: 90\n  lease_duration: 2m\n"))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.Scheduler.PollInterval.Duration() != 90*time.Second {
		t.Errorf("bare integers should mean seconds, got %s", cfg.Scheduler.PollInterval.Duration())
	}
	if cfg.Scheduler.LeaseDuration.Duration() != 2*time.Minute {
		t.Errorf("expected 2m, got %s", cfg.Scheduler.LeaseDuration.Duration())
	}
}

func TestDataDirDefaultsToConfigDirectory(t *testing.T) {
	t.Parallel()

	// An installation must be self-contained: pointing --config at a
	// directory puts the database and agents beside it, never in the home
	// directory. This is a regression guard, not a preference.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	resolvedData, err := filepath.EvalSymlinks(cfg.System.DataDir)
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	if resolvedData != resolvedDir {
		t.Fatalf("data dir should be %s, got %s", resolvedDir, resolvedData)
	}

	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(cfg.DatabasePath(), filepath.Join(home, ".ai-squad")) {
		t.Fatalf("an isolated configuration must not write to the home directory: %s",
			cfg.DatabasePath())
	}
}

func TestExplicitDataDirWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	elsewhere := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "system:\n  data_dir: " + elsewhere + "\n" + minimalConfig
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.System.DataDir != elsewhere {
		t.Errorf("an explicit data_dir must win, got %s", cfg.System.DataDir)
	}
}

func TestParseWithoutFileFallsBackToDefaultDataDir(t *testing.T) {
	// No t.Parallel: t.Setenv mutates process state.
	t.Setenv("AI_SQUAD_DATA_DIR", "")

	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expected, err := ExpandPath(DefaultDataDir)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if cfg.System.DataDir != expected {
		t.Errorf("expected %s, got %s", expected, cfg.System.DataDir)
	}
}

func TestDataDirEnvironmentOverride(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("AI_SQUAD_DATA_DIR", custom)

	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.System.DataDir != custom {
		t.Errorf("AI_SQUAD_DATA_DIR should win over the built-in default, got %s", cfg.System.DataDir)
	}
}
