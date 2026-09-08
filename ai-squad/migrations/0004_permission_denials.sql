-- 0004_permission_denials: persist what a runtime reported refusing to do.
--
-- core.RunResult.PermissionDenials was captured from Claude Code's response
-- since Phase 2 but never stored — the data reached the scheduler and was
-- silently dropped. This is part of the audit trail: an agent hitting a
-- permission wall is exactly the kind of event a security-conscious
-- operator needs visible in `ai-squad logs`, not discarded.

ALTER TABLE agent_runs ADD COLUMN permission_denials TEXT NOT NULL DEFAULT '[]';
