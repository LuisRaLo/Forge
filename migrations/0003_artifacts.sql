-- 0003_artifacts: structured communication between workflow steps.
--
-- Agents never talk to each other directly (per design). A step's structured
-- output — plan.json, qa-report.json, review.json — lands here, keyed by
-- task and by the agent that produced it, so any later step (or a human
-- inspecting the task) can read exactly what a prior step decided.

CREATE TABLE artifacts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT    NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    step_id    TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    agent      TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    created_at TEXT    NOT NULL,

    UNIQUE (task_id, step_id)
);

CREATE INDEX idx_artifacts_task ON artifacts (task_id, id);
