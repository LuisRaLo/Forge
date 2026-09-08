# ai-squad

A local orchestrator for a squad of autonomous software development agents. It
runs on your machine, keeps its state in SQLite, and coordinates agents that may
execute on entirely different backends.

The orchestrator is not tied to any vendor. Claude Code is one runtime among
several, reached through an adapter; nothing in the core imports it.

## Status

**Phase 2 of 8 is complete.** What works today: configuration, agent
definitions, the task model and its state machine, persistence with crash
recovery, the CLI, the `AgentRuntime` port, a scriptable `MockRuntime`, and a
`ClaudeCodeRuntime` adapter verified end to end against the real installed
CLI — including two bugs it found and fixed (see
`docs/architecture.md#phase-2-what-live-testing-against-claude-code-actually-found`).
Agents are not yet scheduled or given a workspace to run in — that is Phases
3 and 4.

## Architecture

The central abstraction is `AgentRuntime`, not `LLMProvider`. This distinction
matters, and it is deliberate.

A full agent runtime such as Claude Code owns its own tool loop, permission
system, filesystem access, resumable sessions and cost accounting. A plain
completion API such as Ollama or DeepSeek owns none of that. Forcing both
through one interface would either strip the capable runtime down to the
weaker one's feature set, or contaminate the completion interface with
tool-execution semantics that do not belong in it.

So there are two ports at two different levels:

```
                    ORCHESTRATOR
      scheduler │ state │ policy │ memory │ workspace
                          │
                   AgentDefinition
                          │
                  ┌───────▼────────┐
                  │  AgentRuntime  │   ← primary port
                  └───────┬────────┘
                          │
         ┌────────────────┼─────────────────┐
         ▼                ▼                 ▼
  ClaudeCodeRuntime  ModelRuntime      MockRuntime
   (delegates the     (drives the       (tests, dry
    whole loop)        loop itself)      runs)
                          │
                  ┌───────▼────────┐
                  │  LLMProvider   │   ← secondary port
                  └───────┬────────┘
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
           Ollama    DeepSeek    OpenAI-compatible
```

`LLMProvider` is not the foundation; it is a detail that one family of runtimes
depends on. Concrete runtimes and providers are wired in `cmd/ai-squad`, at the
edge of the program, so the core depends only on interfaces.

### Capability negotiation

Because runtimes differ in what they can do, `AgentRuntime` declares
`Capabilities()`, and every agent derives `RequiredCapabilities()` from its
permissions. Binding a shell-using agent to a bare completion API is therefore
rejected when configuration loads, rather than failing halfway through a task.

### Configuration has two blocks, not one

They mean different things:

- `runtimes:` — how an agent step is executed.
- `providers:` — plain completion endpoints, consumed only by `type: model`
  runtimes.

## Requirements

- Go 1.27+
- Git 2.5+ (worktrees)
- Optional: Claude Code, Docker, GitHub CLI, Ollama

## Getting started

```sh
go build -o ai-squad ./cmd/ai-squad

./ai-squad init
./ai-squad config validate
./ai-squad agent list

./ai-squad task create \
  --title "Add Google authentication" \
  --repo ~/Projects/my-api \
  --workflow feature

./ai-squad task list
./ai-squad task show TASK-1
```

An installation is self-contained: the database, agent definitions and
worktrees live beside the configuration file. Point `--config` somewhere else
and you get a completely separate installation, which is what makes the test
suite safe to run.

## Task state machine

Transitions are declared in one table in `internal/core/statemachine.go`. Any
edge absent from it is rejected; there are no implicit transitions.

```
PENDING → PLANNING → READY → RUNNING → REVIEW → COMPLETED
```

QA rejection sends work back to the implementer via `REVIEW → RUNNING`, bounded
by the task's attempt limit so the loop always terminates — in `BLOCKED` if it
cannot make progress. Production deployment goes through `WAITING_APPROVAL` and
always requires a human.

## Security

- Credentials come from environment variables only. An inline `api_key` in the
  configuration is a hard load-time failure, not a warning.
- Agent permissions fail closed: an omitted permission block grants nothing.
- Contradictory permissions are rejected (`git_write` without workspace write
  access, a shell with no filesystem).
- `Save` cannot write a task's status; every status change goes through the
  guarded `Transition`, which re-reads under the transaction so two workers
  racing for the same task cannot both win.
- Production deployment is never automatic in V1, and the configuration cannot
  turn that off.

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .
```

Real providers are never required to run the suite.

## Roadmap

| Phase | Scope | Status |
|-------|-------------------------------------------------|--------|
| 1 | CLI, SQLite, task model, state machine, config  | done |
| 2 | `AgentRuntime` port, mock runtime, Claude Code adapter | done |
| 3 | Git worktrees, workspace manager | next |
| 4 | Scheduler, worker pool, concurrency, recovery | |
| 5 | QA, reviewer, workflows, feedback loops | |
| 6 | Ollama, DeepSeek, OpenAI-compatible providers | |
| 7 | GitHub, CI, pull requests, human approval | |
| 8 | Security hardening, audit logs, cost controls | |
