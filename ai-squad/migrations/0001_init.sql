-- 0001_init: task state, audit trail and identifier counters.
--
-- Timestamps are stored as RFC3339 UTC strings: they sort lexicographically,
-- survive schema dumps, and are readable without a decoder.

CREATE TABLE tasks (
    id             TEXT    PRIMARY KEY,
    title          TEXT    NOT NULL,
    description    TEXT    NOT NULL DEFAULT '',
    repository     TEXT    NOT NULL DEFAULT '',
    branch         TEXT    NOT NULL DEFAULT '',
    workflow       TEXT    NOT NULL DEFAULT '',
    step           INTEGER NOT NULL DEFAULT 0,
    agent          TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL,
    priority       INTEGER NOT NULL DEFAULT 50,
    attempts       INTEGER NOT NULL DEFAULT 0,
    max_attempts   INTEGER NOT NULL DEFAULT 3,
    parent_task_id TEXT    REFERENCES tasks(id) ON DELETE SET NULL,
    workspace_path TEXT    NOT NULL DEFAULT '',
    last_error     TEXT    NOT NULL DEFAULT '',
    metadata       TEXT    NOT NULL DEFAULT '{}',
    -- Optional caller-supplied key making task creation idempotent.
    idempotency_key TEXT   UNIQUE,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    started_at     TEXT,
    completed_at   TEXT,

    CHECK (status IN (
        'PENDING', 'PLANNING', 'READY', 'RUNNING', 'WAITING', 'REVIEW',
        'WAITING_APPROVAL', 'FAILED', 'BLOCKED', 'COMPLETED', 'CANCELLED'
    )),
    CHECK (attempts >= 0),
    CHECK (max_attempts >= 1),
    CHECK (id <> parent_task_id)
);

-- The scheduler's hot query: claim the highest-priority runnable task.
CREATE INDEX idx_tasks_claim ON tasks (status, priority DESC, created_at);
CREATE INDEX idx_tasks_parent ON tasks (parent_task_id);
CREATE INDEX idx_tasks_repository ON tasks (repository);

-- Append-only audit trail. Every status change lands here, which is what makes
-- a task's history reconstructible after a crash.
CREATE TABLE task_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT    NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    from_status TEXT    NOT NULL DEFAULT '',
    to_status   TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL
);

CREATE INDEX idx_task_events_task ON task_events (task_id, id);

-- Monotonic counters backing human-readable identifiers such as TASK-123.
CREATE TABLE counters (
    name  TEXT    PRIMARY KEY,
    value INTEGER NOT NULL
);
