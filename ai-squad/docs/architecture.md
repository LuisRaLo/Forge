# Architecture

## The decision that shapes everything else

The orchestrator's primary port is `AgentRuntime`, not `LLMProvider`.

This was verified against the tools actually installed on this machine rather
than assumed. Probing `claude --help` and a live `claude -p --output-format
json` run showed that Claude Code is not a chat API behind a process boundary.
It is a complete agent that owns:

- its own tool loop and tool set (`--tools`, `--allowedTools`, `--disallowedTools`)
- a permission system (`--permission-mode`, `--add-dir`, and reported
  `permission_denials`)
- resumable sessions (`--session-id`, `--resume`)
- structured output validated against a schema (`--json-schema`)
- a spend limit (`--max-budget-usd`) and reported cost (`total_cost_usd`,
  `usage`, `modelUsage`)

Ollama, DeepSeek and any OpenAI-compatible endpoint own none of that. They are
stateless completion APIs.

Modelling both as `LLMProvider` would force one of two failures: reduce Claude
Code to text-in/text-out and discard its permissions, sessions and cost data; or
grow `LLMProvider` until it carries tool-execution semantics that a completion
API cannot honour. Splitting the two levels avoids both.

```
AgentRuntime (primary port)
├── ClaudeCodeRuntime   delegates the entire loop to an external agent process
├── ModelRuntime        implements the loop in-process
│   └── LLMProvider (secondary port)
│       ├── OllamaProvider
│       ├── OpenAICompatibleProvider   (DeepSeek and most hosted APIs)
│       └── MockProvider
└── MockRuntime         deterministic, for tests and dry runs
```

`LLMProvider` still exists and is still swappable. It is simply not the thing
the orchestrator depends on.

## Dependency rule

`internal/core` imports only the standard library. It defines the domain types
and the ports. Everything else depends inward:

```
cmd/ai-squad  ──wires──▶ concrete runtimes and providers
     │
     ▼
internal/cli ──▶ internal/tasks ──▶ internal/core ◀── internal/storage
                       │                  ▲
                       └──────────────────┘
                  internal/config, internal/agents
```

Nothing in `internal/core`, `internal/tasks` or `internal/scheduler` may import
a vendor package. Concrete implementations are constructed in `main` and passed
in as interfaces. There is no global registry and no `init()`-time registration,
because a side-effecting import is global state wearing a disguise.

`internal/core` also stays free of YAML: the on-disk shapes live in
`internal/config` and `internal/agents` as separate DTOs. That is why a YAML
quirk such as `shell:` being either a boolean or a list never reaches the
domain model.

## Capability negotiation

An agent declares permissions; a runtime declares capabilities. The orchestrator
compares them.

```go
missing := runtime.Capabilities().Missing(agent.RequiredCapabilities())
```

A `developer` agent with `shell: [go, git]` and `git_write: true` requires
`filesystem_read`, `filesystem_write`, `shell` and `tools`. Bind it to a runtime
that lacks them and the failure surfaces when configuration loads, with the
missing capabilities named — not thirty minutes into a task.

## Task state machine

`internal/core/statemachine.go` holds one transition table and no other source
of truth. `ValidateTransition` rejects anything absent from it, including
self-transitions, so a caller cannot use `RUNNING → RUNNING` to paper over a
missed state change. Idempotent no-ops are handled one layer up, in the task
service, where the intent is explicit.

Properties enforced by tests rather than by convention:

- every non-terminal state can be cancelled, so an operator can always stop work
- terminal states have no successors
- every state except `PENDING` is reachable
- `PENDING → RUNNING` is rejected: work is always scheduled, never started
  directly
- `FAILED → RUNNING` is rejected: a retry re-queues, so the scheduler stays the
  only component that starts work

## Concurrency

`TaskRepo.Transition` re-reads the task inside the transaction and validates the
edge against that fresh copy, then updates with `WHERE id = ? AND status = ?`.
When sixteen workers race to claim one task, exactly one succeeds and fifteen
receive `ErrInvalidTransition`. This is asserted directly, including that the
audit trail records exactly one claim.

The SQLite pool is capped at one connection. SQLite serialises writers in any
case, and a single connection eliminates `SQLITE_BUSY` and lost-update races
outright — worth more than read parallelism for a local orchestrator. The
consequence to respect: never hold an open `Rows` cursor across another query.
`List` and `Events` scan fully before returning for exactly this reason.

## Idempotency

- Task creation accepts an `IdempotencyKey`, stored under a UNIQUE index, with
  the empty string mapped to SQL `NULL` so unkeyed tasks are unconstrained.
  Replaying a create returns the original task.
- `init` writes no file that already exists unless `--force` is given.
- Migrations are recorded with a checksum. Re-running them is a no-op; editing
  an applied migration is a loud failure rather than silent schema drift.
- `cancel` on an already-cancelled task succeeds and writes no second event.
- `retry` on an already-queued task returns it untouched, so repetition never
  multiplies work or resets the attempt counter twice.

## Installation layout

`data_dir` defaults to the directory holding the configuration file. An
installation is therefore self-contained, and `--config` genuinely isolates one.
The alternative — a hardcoded `~/.ai-squad` — meant that pointing `--config` at
a temporary directory still wrote the database into the home directory. That
was caught by the test suite and fixed; a regression test now asserts it.

Precedence: explicit `system.data_dir` → `AI_SQUAD_DATA_DIR` → the config file's
directory → `~/.ai-squad`.

## Phase 2: what live testing against Claude Code actually found

Four requests were sent to the real, installed `claude` CLI while wiring
`internal/runtimes/claudecode`, on top of the three sent in Phase 1 to capture
its JSON contract. Two of the four surfaced real bugs; a third surfaced an
environment constraint worth recording plainly rather than hiding.

**Bug 1 — the prompt gets silently swallowed.** `--allowedTools` and
`--disallowedTools` are variadic (`<tools...>` per `--help`): they consume
every following bare token until the next flag. The initial implementation
appended the prompt last, after these lists, and the CLI reported *"Input must
be provided either through stdin or as a prompt argument"* — the prompt had
been eaten into the tool list. Fixed by anchoring the prompt immediately after
`-p`, matching the exact form verified in the Phase 1 probes, and covered by a
regression test (`TestBuildArgsPromptImmediatelyFollowsPrintFlag`).

**Bug 2 — the child process leaked local configuration.** A run permitted
only `Bash(echo:*)` produced a session where the model reported no Bash tool
at all, and instead had `ToolSearch` available — a tool from the *invoking*
Claude Code session's own harness, not a generic Claude Code built-in. The
child process was inheriting CLAUDE.md, plugins, hooks and custom agents from
the ambient environment rather than getting the clean built-in tool set the
permission mapping in `args.go` assumes. Fixed by always passing
`--safe-mode`, which disables exactly that surface while explicitly leaving
"auth, model selection, built-in tools, and permissions" working per
`--help` — `--bare` was rejected for the same fix because its own help text
says it forces `ANTHROPIC_API_KEY` and stops honouring OAuth/keychain login,
which is how this machine is actually authenticated (`apiKeySource: "none"`
in the Phase 1 probe).

**Finding, not a bug — an account-level tool policy.** Even with
`--safe-mode`, the scoped Bash grant still did not produce a session with a
`Bash` tool. The available tools instead matched this outer harness's own
deferred-tool set (`ToolSearch`, `TaskCreate`, `DesignSync`, ...). `--help`
notes that `--safe-mode` leaves "admin-managed (policy) settings" in force;
that layer appears to replace the classic built-in toolset on this
account/machine with a curated, account-specific one that this adapter has no
way to see around. This is not something more request-tuning would fix, so
further paid live calls were stopped rather than spent chasing it. Net
effect: `toolArgs` in `args.go` is verified correct in its *construction*
(confirmed unit-tested against every permission combination) and against the
documented, unmanaged CLI contract, but **not independently confirmed against
a policy-managed account** — that remains open, flagged in code, rather than
asserted.

## Deferred deliberately

- **Worktree management** (Phase 3). One workspace per task, never shared.
- **Worker leases** (Phase 4). Recovering a task abandoned by a crashed worker
  needs a lease column and a reaper. The schema will gain them in a migration
  rather than carrying unused columns now.
- **Artifacts** (Phase 5). Agents communicate through `plan.json`,
  `qa-report.json` and `review.json` written to the task, never by talking to
  each other. They will get their own table rather than being crammed into
  `metadata`, which is `map[string]string` on purpose.
