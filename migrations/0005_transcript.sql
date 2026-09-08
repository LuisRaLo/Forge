-- 0005_transcript: persist the agent's actual step-by-step activity.
--
-- Until now, a run only recorded its final outcome (status, cost, error) —
-- the events streamed during execution (assistant text, tool calls, tool
-- results) were logged to the daemon's own log file and discarded, so
-- diagnosing "it says success/failed but why" required manually reading the
-- runtime's own session transcript on disk. This makes that visibility part
-- of the run record itself, so `ai-squad logs` and the dashboard can show
-- what the agent actually did.

ALTER TABLE agent_runs ADD COLUMN transcript TEXT NOT NULL DEFAULT '[]';
