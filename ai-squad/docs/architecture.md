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

## Phase 3: workspace isolation

`internal/workspace.Manager` sits on top of `internal/git`, a thin CLI
wrapper (worktree add/remove/list, extended in Phase 7 with
commit/push/PR-adjacent operations rather than built out speculatively now).

Two different tasks are kept from ever touching the same worktree by
construction, not by a lock this package invents: each task's path and
branch derive from its own unique task ID (`<data_dir>/worktrees/task-N`,
branch `ai-squad/task-n`), and only the worker that won that task's
`core.TaskRepository.Transition` to RUNNING/PLANNING — proven single-winner
by the Phase 1 concurrency tests — ever calls `Acquire` for it. `Manager`'s
own per-task mutex exists only to make repeated `Acquire`/`Release` calls for
the *same* task safe against being invoked twice concurrently within one
process, which the state machine does not by itself rule out.
`TestConcurrentAcquireDifferentTasks` and `TestConcurrentAcquireSameTask`
assert both properties against real, on-disk git repositories rather than a
mock.

`Acquire` is idempotent: if a task's `WorkspacePath` already names a
worktree git still has registered, it is returned as-is rather than
recreated. This is what lets a task recovered after a crash resume in the
same workspace instead of losing uncommitted work sitting on disk —
`TestAcquireIsIdempotent` seeds an in-progress file and asserts it survives
a second `Acquire`. Deletion is conservative in the same spirit: if a path is
occupied by something that is *not* a registered git worktree, `Acquire`
refuses to touch it rather than silently overwriting what might be a
crash artifact or manual tampering — consistent with this project's own rule
against surprise destructive defaults.

`Manager` never touches `core.Task` or the database; it returns a
`Workspace` value and lets the caller — the scheduler in Phase 4 — decide
when to persist `WorkspacePath`/`Branch` via the existing
`Transition` mutator. That is what makes the package testable with real git
repositories and zero database setup.

## Phase 4+5: the scheduler and workflow engine, built together

Phase 4 ("Scheduler, worker pool, concurrency, recovery") and Phase 5
("QA, reviewer, workflows, feedback loops") are the user's own phase split,
but implementing Phase 4 as single-step-only and then bolting multi-step
chaining onto it in Phase 5 would mean rewriting the claim/advance logic
twice. They were built as one engine (`internal/scheduler`), covering both
phases' scope, so nothing here was thrown away.

### The state machine forces REVIEW, not READY, after RUNNING

`RUNNING`'s allowed targets are `{REVIEW, WAITING, WAITING_APPROVAL,
COMPLETED, FAILED, BLOCKED, CANCELLED}` — `READY` is not among them, by the
design proven in Phase 1's own state-machine tests (`FAILED -> RUNNING is
rejected: a retry re-queues, so the scheduler stays the only component that
starts work` applies symmetrically to `RUNNING -> READY`). The first
scheduler implementation tried `RUNNING -> READY` directly for retries, the
QA-fail rewind, and workflow advancement, and the test suite rejected every
one of them with `cannot transition RUNNING -> READY` — the exact kind of
guard rail the state machine exists to provide.

The fix uses the machine as designed rather than fighting it: a successful
`RUNNING` step always advances into `REVIEW` (which `REVIEW -> RUNNING`
then claims for the *next* step — any next step, not only a QA-gated one;
"REVIEW" is the state machine's name for "a RUNNING step's output is queued
for the next step's claim," used universally, not literally implying human
review every time). A retry or a QA-fail rewind — which need to return an
executing task to `READY` — go through the `RUNNING -> WAITING -> READY`
hop, since `WAITING` borders both. `PLANNING` (used only for a multi-step
workflow's first step) has a direct edge to `READY` and needs no hop.

### The QA feedback loop

An agent with `gate: {enabled: true, steps_back: N}` in its YAML (only `qa`
ships with one) must return structured output shaped `{"passed": bool,
"summary": string}`. The scheduler requests this via
`RunRequest.OutputSchema`, and on a failed gate sends the task back `N`
workflow steps (position-based, not by agent name, so the mechanism is not
hardcoded to any particular role) instead of forward. A per-gate iteration
counter lives in `task.Metadata["loop:<agent>"]`, checked against
`limits.max_step_iterations`; exhausting it produces `BLOCKED` rather than
looping forever. `TestQAFailureLoopsBackToDeveloperThenSucceeds` and
`TestQAFailureExhaustsIterationsAndBlocks` assert both the loop and its
bound directly, and an end-to-end CLI smoke test against the built binary
(worker start, real state transitions, real audit log) confirmed the same
behaviour outside the unit-test harness.

Because a gate step's parsed verdict is what drives control flow, an
unparseable structured-output response is a **hard error** for that step
(`FAILED`, not a silent pass/fail guess) — matching Phase 2's own
unresolved finding that Claude Code's `--json-schema` support was not fully
verified live. A non-gated step's raw text is instead wrapped in a minimal
JSON envelope (`{"text": "..."}`) for artifact storage, which is always
valid regardless of what the agent said, rather than assuming every step
returns clean structured JSON.

### Recovery has two layers

1. **Immediate, on every start** (`recoverInterrupted`): every `PLANNING`/
   `RUNNING` task is reclaimed unconditionally, because a freshly started
   process has zero live worker goroutines by definition — anything Active
   in the database was left behind by a process that is no longer running.
   This alone satisfies "`ai-squad daemon` restart → recover → continue."
2. **A runtime reaper** (`reapStale`), defense in depth for a single
   long-running daemon: an Active task whose `UpdatedAt` is older than its
   own step timeout plus `scheduler.lease_duration` (the grace period) is
   presumed abandoned by a hung goroutine that never reached its own
   timeout — a bug the panic-recovering worker loop did not catch — and is
   reclaimed without needing the whole process to have crashed.

### A real concurrency bug the test suite caught, in git itself

Running the full suite repeatedly surfaced a flaky failure in
`internal/workspace`'s existing 12-way concurrent-worktree test: `git
worktree add`'s own administrative bookkeeping
(`.git/worktrees/<name>/commondir`) is not safe under many concurrent
`worktree add` invocations against the *same* repository once real system
load (the rest of the suite's own git subprocesses) was added to the mix —
it passed 5/5 in isolation and failed intermittently only under full-suite
pressure. This is a race in git's own state, not in this project's Go code,
but from the orchestrator's perspective a task's workspace creation must
never be corrupted by a concurrent task's, so `workspace.Manager` now holds
a second, per-repository mutex serializing `git worktree add`/`remove`
calls against one repository (a separate lock from the existing per-task
one, which only protects one task's own `Acquire`/`Release` against being
invoked twice). Two different repositories are unaffected and still proceed
in parallel. Confirmed by re-running the full suite repeatedly afterward.

## Phase 6: model-driven runtimes

`internal/runtimes/model.Runtime` is the counterpart to
`internal/runtimes/claudecode.Runtime`: it drives a tool-call loop
in-process on top of a plain `core.LLMProvider`, which is what Ollama,
DeepSeek and any OpenAI-compatible endpoint need, since none of them own an
agent loop of their own (see the architecture split at the top of this
document). The loop itself lives in `internal/runtimes/model`; the tools it
offers live in `internal/tools` (`read_file`, `write_file`, `run_shell`),
gated by `core.Permissions` exactly like the Claude Code adapter's tool
allow/disallow mapping — a denied capability simply is not offered, rather
than being offered and refused at call time. `internal/tools` is injected
into `model.Runtime` as a `ToolBuilder` function rather than imported and
called from a package-level variable, keeping the runtime testable with a
fake tool set and avoiding the kind of mutable global state this project's
own principles rule out.

Two providers exist behind the same `core.LLMProvider` port:

- `internal/providers/openaicompat` speaks the OpenAI chat-completions
  contract (`POST /chat/completions`), which is what `type:
  openai-compatible` resolves to in configuration — this is also what
  DeepSeek uses, since its API is an OpenAI-compatible superset.
- `internal/providers/ollama` speaks Ollama's own contract (`POST
  /api/chat`), distinct enough from the OpenAI shape to need its own
  provider rather than reusing openaicompat.

**Verification status, stated plainly**: `openaicompat` was tested against
an `httptest` server standing in for the real API, which is sufficient to
prove the request/response mapping and error classification are correct
against the documented contract. Neither DeepSeek nor a real
OpenAI-compatible endpoint was exercised live in this session — no
`DEEPSEEK_API_KEY` was available. `ollama` was built and tested the same
way; Ollama itself is not installed on this machine (`which ollama` found
nothing, checked directly in Phase 1's environment inspection), so unlike
the Claude Code adapter, its wire format is implemented against Ollama's
published API documentation and not independently confirmed against a
running server. Both are marked as such in their package doc comments, the
same honesty standard the Claude Code adapter was held to when its
structured-output support turned out to be unverifiable in Phase 2.

## Phase 7: GitHub integration, kept deliberately narrow

`internal/git` gained `Commit`/`Push`; `internal/github` wraps `gh pr
create`/`checks`/`view --json comments`, with every flag verified against
the installed CLI's own `--help` output rather than guessed. `gh` was not
authenticated on this machine (`gh auth status`: not logged in), so — same
honesty standard as Ollama in Phase 6 — the flag *shapes* are confirmed, an
actual PR round-trip against real GitHub is not; tests use a scripted fake
`gh` process (the same `TestMain` re-exec pattern used for Claude Code).

**A deliberate scope decision**: the fully automatic "CI FAILED -> Developer
-> QA -> CI" loop described in the brief is not wired into the scheduler as
a zero-touch pipeline stage. `devops.yaml` already grants its agent shell
access to both `git` and `gh` (Phase 1), so a Claude-Code-backed devops step
can already drive PR/CI operations itself through its own shell tool.
Building a SEPARATE, scheduler-native CI-polling gate (a workflow step that
polls external CI state, potentially for many minutes, without invoking a
model at all) is real additional machinery — async polling, `WAITING`
transitions, its own iteration accounting — that was not exercised end to
end without live GitHub access to validate against, so it was not built out
speculatively. What Phase 7 does deliver: `ai-squad pr create/status/comments`,
usable both by an operator and by any shell-capable agent, plus `ai-squad
approve` (Phase 4's CLI wiring) as the one path to `WAITING_APPROVAL ->
COMPLETED`. Neither this package nor `approve` can merge a PR or deploy —
there is no method that does either, not just a policy check declining to
call one.

## Phase 8: security hardening, audit trail, cost visibility

Three real gaps were found by re-reading what had already been built, not
by speculative addition:

1. **`RunResult.PermissionDenials` was captured but discarded.** The Claude
   Code adapter parsed it from the CLI's own JSON since Phase 2
   (`permission_denials` in the probed `result` line), the scheduler never
   persisted it. Migration `0004` adds the column; `recordRun` now populates
   it and logs a warning when non-empty, so a runtime refusing an action is
   a visible, queryable event (`ai-squad logs`) instead of silently lost.
2. **`system.log_level`/`system.log_format` were validated but never
   consumed.** Every command opened without ever constructing a logger from
   them — the scheduler defaulted to `slog.Default()`, and the config
   fields did nothing. `internal/cli.newLogger` now builds the real
   `*slog.Logger` (text or JSON, at the configured level) writing to
   `<data_dir>/logs/ai-squad.log`, kept off stdout so interactive command
   output stays clean and scriptable.
3. **No guard against a task repository pointing at `$HOME` or a
   credential directory.** A task's workspace is a git worktree of whatever
   `--repo` names; if that were `$HOME` on a machine where dotfiles are
   tracked in git, an agent would gain read/write access to `.ssh`, `.aws`,
   `.kube` and similar. `tasks.Service.resolveRepository` now rejects the
   home directory itself and its conventional credential subdirectories at
   task-creation time — checked once, centrally, rather than trusted to
   every runtime's own sandboxing.

**What was already in place from earlier phases**, listed here because
"security hardening" is a phase heading, not a first appearance: secrets
only via environment variables, never SQLite or YAML (Phase 1, enforced at
config load); the Claude Code adapter's `redact()` and filtered child
environment (Phase 2); per-task workspace isolation via git worktrees
(Phase 3); mandatory timeouts and full `context.Context` cancellation
throughout (Phases 2, 4, 6); the daily cost limit gate in the scheduler's
claim loop (Phase 4); path-escape protection and shell allowlisting in the
model-driven tool set (Phase 6); production deployment requiring
`ai-squad approve` and nothing in this codebase capable of merging a PR or
deploying on its own (Phase 7).

`ai-squad status` now surfaces cost against the daily limit directly,
including a count of runs whose cost could not be determined — the literal
requirement being "si no podemos conocer el coste real de un provider,
registra al menos el usage disponible y deja claro que el límite de coste
es estimado": the total shown is a floor, not an exact figure, and the
unknown-run count is what makes that visible rather than asserted in a
comment only.

## Deferred deliberately

- **Worktree management** (Phase 3). One workspace per task, never shared.
- **Worker leases** (Phase 4). Recovering a task abandoned by a crashed worker
  needs a lease column and a reaper. The schema will gain them in a migration
  rather than carrying unused columns now.
- **Artifacts** (Phase 5). Agents communicate through `plan.json`,
  `qa-report.json` and `review.json` written to the task, never by talking to
  each other. They will get their own table rather than being crammed into
  `metadata`, which is `map[string]string` on purpose.
