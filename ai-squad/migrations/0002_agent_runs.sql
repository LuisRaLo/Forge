-- 0002_agent_runs: one row per agent execution.
--
-- This is the provider-execution record the observability and cost-control
-- requirements are built on: duration, status, retry count, token usage, and
-- cost where the runtime reports it.
--
-- cost_usd is nullable on purpose. A NULL means "this runtime did not tell us",
-- which must stay distinguishable from a genuine zero, or a daily spend limit
-- would silently treat unknown spend as free.

CREATE TABLE agent_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id      TEXT    NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,

    -- step_id identifies one attempt of one workflow step. It embeds the
    -- attempt number, so the UNIQUE constraint below makes recording a run
    -- idempotent without preventing legitimate retries.
    step_id      TEXT    NOT NULL,

    agent        TEXT    NOT NULL,
    runtime      TEXT    NOT NULL,
    status       TEXT    NOT NULL,
    session_id   TEXT    NOT NULL DEFAULT '',
    stop_reason  TEXT    NOT NULL DEFAULT '',
    error        TEXT    NOT NULL DEFAULT '',

    model                 TEXT    NOT NULL DEFAULT '',
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,

    cost_usd       REAL,
    cost_estimated INTEGER NOT NULL DEFAULT 0,

    duration_ms  INTEGER NOT NULL DEFAULT 0,
    started_at   TEXT    NOT NULL,
    finished_at  TEXT    NOT NULL,

    CHECK (status IN ('succeeded', 'failed', 'cancelled')),
    UNIQUE (task_id, step_id)
);

CREATE INDEX idx_agent_runs_task ON agent_runs (task_id, id);
-- Supports the daily spend query without scanning the table.
CREATE INDEX idx_agent_runs_started ON agent_runs (started_at);
